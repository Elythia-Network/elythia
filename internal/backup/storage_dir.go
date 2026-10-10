package backup

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// dirTempPrefix is the name prefix of files DirStorage.Put writes before
// renaming them into place. List never returns them.
const dirTempPrefix = ".tmp-"

// Permissions of what DirStorage creates: the owner and the group may read,
// nobody else may. The group cannot change the content of a file, but it can
// create, replace and delete files in the directories (write on them).
//
// 本体 (管理画面、#3462) はバックアップ用のサービスと別の UID で動く (991 と 70)。
// 本体が一覧・ダウンロード・削除をするには、同じグループで読めて、ディレクトリに
// 書ける (消せる) 必要がある。dump には秘密鍵や token が入るので、他人 (other) には
// 一切渡さない。グループは、根に setgid を付けたディレクトリから引き継ぐ
// (docs/backup.md)。umask に左右されないよう、作った後で付け直す。
const (
	dirPerm  fs.FileMode = 0o770
	filePerm fs.FileMode = 0o640
)

// DirStorage keeps generations in a directory, meant to be a mount of another
// device (NAS and so on). Keys map to paths below the root.
type DirStorage struct {
	root string
}

// DirMarkerFile is the file that must exist at the root of a directory
// storage. DirStorage refuses a root without it.
//
// 根のディレクトリがあるだけでは、別の機器が mount されているとは言えない。compose の
// bind mount では、NAS の mount が外れてもホストの空のディレクトリがそのまま見え、
// そこへ書くと同じホストのディスクにバックアップを置くことになる (同じホストの
// ディスクはバックアップに数えない、#3457)。運営者が mount した先に作った目印が
// 見えることを、書く前の条件にする。
const DirMarkerFile = ".elythia-backup"

// NewDirStorage returns a DirStorage rooted at root, which must already exist,
// be a directory and contain DirMarkerFile. A root given through a symbolic
// link is resolved first.
//
// 根のディレクトリも目印も作らない。作ると、mount が外れたときに同じホストの
// ディスクへ黙って書き始める。
func NewDirStorage(root string) (*DirStorage, error) {
	if root == "" {
		return nil, errors.New("backup: storage.dir.path is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("backup: resolve %s: %w", root, err)
	}
	// 根が symlink (例: /backup -> /mnt/nas/elythia-backup) だと、WalkDir は根の
	// 中へ降りず、Put / Get は通るのに List だけが空になる。先に実体へ解決し、
	// 目印はその実体の中で探す。
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("backup: storage directory: %w", err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("backup: storage directory: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("backup: storage directory %s is not a directory", abs)
	}
	if _, err := os.Stat(filepath.Join(resolved, DirMarkerFile)); err != nil {
		return nil, fmt.Errorf("backup: %s has no %s. Is the backup device mounted there? "+
			"Create the file on the mounted device (see docs/backup.md): %w", abs, DirMarkerFile, err)
	}
	return &DirStorage{root: resolved}, nil
}

// CreateDirMarker writes DirMarkerFile into root, which must already exist.
// Run it once on the mounted device; tests use it to prepare a storage.
func CreateDirMarker(root string) error {
	f, err := os.OpenFile(filepath.Join(root, DirMarkerFile), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("backup: create %s: %w", DirMarkerFile, err)
	}
	return f.Close()
}

// Root returns the absolute path of the storage directory, with symbolic
// links resolved.
func (d *DirStorage) Root() string { return d.root }

// errSymlink is returned when a path inside the storage goes through a
// symbolic link.
var errSymlink = errors.New("backup: symbolic link in the storage path")

// rel maps key to a root-relative path, rejecting keys that are not plain
// relative slash-separated paths (no "..", no empty or temp-file segments).
func (d *DirStorage) rel(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, `\`) || path.Clean(key) != key {
		return "", fmt.Errorf("backup: invalid key %q", key)
	}
	if key == DirMarkerFile {
		return "", fmt.Errorf("backup: invalid key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." || strings.HasPrefix(seg, dirTempPrefix) {
			return "", fmt.Errorf("backup: invalid key %q", key)
		}
	}
	return filepath.FromSlash(key), nil
}

// openRoot opens the storage root. Every operation goes through it, so a
// symbolic link swapped in after a check still cannot lead outside the root.
//
// 根の fd は操作ごとに開く。持ち続けると、NAS を mount し直したときに古い
// mount を指したままになる。
func (d *DirStorage) openRoot() (*os.Root, error) {
	r, err := os.OpenRoot(d.root)
	if err != nil {
		return nil, fmt.Errorf("backup: open %s: %w", d.root, err)
	}
	return r, nil
}

// lstatPath checks every directory between the root and rel with Lstat and
// returns the FileInfo of rel itself. A symbolic link anywhere, rel
// included, yields errSymlink.
//
// os.Root は根の外へ出る symlink を拒むが、根の中を指す symlink は辿る。保存先に
// 書ける者 (根は 2770 なのでグループ) が世代のディレクトリやファイルを symlink に
// 置き換えると、別の世代のファイルを読み書きできてしまうので、根の中の symlink も
// 受けない。
func lstatPath(r *os.Root, rel string) (fs.FileInfo, error) {
	var fi fs.FileInfo
	cur := ""
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		var err error
		if fi, err = r.Lstat(cur); err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, errSymlink
		}
	}
	return fi, nil
}

// Put writes r to a temporary file next to key and renames it into place, so
// a failed Put never leaves a partial file under key.
func (d *DirStorage) Put(ctx context.Context, key string, r io.Reader) (err error) {
	rel, err := d.rel(key)
	if err != nil {
		return err
	}
	root, err := d.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	dir := filepath.Dir(rel)
	if err := mkdirs(root, dir); err != nil {
		return err
	}
	// 書き終える前に他から読まれないよう 0600 で作って書き、置く前に filePerm に
	// 広げる。
	tmp, f, err := createTemp(root, dir)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if err != nil {
			if !closed {
				_ = f.Close()
			}
			_ = root.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, ctxReader{ctx: ctx, r: r}); err != nil {
		return fmt.Errorf("backup: write %s: %w", key, err)
	}
	// 置く前に中身をディスクへ落とす。落とさずに rename すると、電源断の後に
	// 名前だけあって中身が空のファイルが残りうる。
	if err = f.Sync(); err != nil {
		return fmt.Errorf("backup: sync %s: %w", key, err)
	}
	if err = chmodTolerant(key, func() error { return chmodFile(f, filePerm) }); err != nil {
		return err
	}
	closed = true
	if err = f.Close(); err != nil {
		return fmt.Errorf("backup: close %s: %w", key, err)
	}
	// rename は置き先が symlink でも辿らず、symlink そのものを置き換える。
	if err = root.Rename(tmp, rel); err != nil {
		return fmt.Errorf("backup: rename %s: %w", key, err)
	}
	syncDir(filepath.Join(d.root, dir))
	return nil
}

// createTemp creates a new file named dirTempPrefix+random in dir.
func createTemp(root *os.Root, dir string) (string, *os.File, error) {
	for range 10 {
		var b [8]byte
		if _, err := cryptorand.Read(b[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(dir, dirTempPrefix+hex.EncodeToString(b[:]))
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("backup: create temp file: %w", err)
		}
		return name, f, nil
	}
	return "", nil, errors.New("backup: create temp file: too many collisions")
}

// mkdirs creates the directories from the root down to dir with dirPerm.
// Directories that already exist are left as they are; a symbolic link or a
// file on the way is an error.
//
// 既にあるディレクトリの mode は変えない。運営者が手で付けた setgid や、別の
// UID が作ったものを、こちらの都合で書き換えない。
func mkdirs(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(dir, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		err := root.Mkdir(cur, dirPerm)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("backup: mkdir %s: %w", cur, err)
		}
		fi, lerr := root.Lstat(cur)
		if lerr != nil {
			return fmt.Errorf("backup: mkdir %s: %w", cur, lerr)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("backup: mkdir %s: %w", cur, errSymlink)
		}
		if !fi.IsDir() {
			return fmt.Errorf("backup: mkdir %s: not a directory", cur)
		}
		if err != nil {
			// 既にあった。
			continue
		}
		// 親の setgid を引き継いだ (グループを共有する) ディレクトリでは、それを
		// 残す。落とすと、その下に作るファイルのグループが自分の主グループになり、
		// 本体から読めなくなる。
		mode := dirPerm | fi.Mode()&fs.ModeSetgid
		if err := chmodTolerant(cur, func() error { return chmodDir(root, cur, mode) }); err != nil {
			return err
		}
	}
	return nil
}

// chmodFile and chmodDir are replaced in tests to simulate file systems that
// refuse chmod.
var (
	chmodFile = func(f *os.File, mode fs.FileMode) error { return f.Chmod(mode) }
	chmodDir  = func(root *os.Root, name string, mode fs.FileMode) error { return root.Chmod(name, mode) }
)

// chmodTolerant runs chmod and ignores the errors of a file system that has
// no Unix permissions, logging a warning instead.
//
// unix extensions の無い CIFS (file_mode / dir_mode で mount) などでは、作った
// 本人でも chmod が EPERM / ENOTSUP / EINVAL になる。権限は mount の設定で
// 決まっていて chmod では変えられないので、ここで止めると、以前は取れていた
// 構成でバックアップそのものが取れなくなる。作ったばかりのファイルの持ち主は
// 自分なので、普通のファイルシステムでこれらが返ることは無い。それ以外の誤り
// (EIO / EROFS など) は止める。
func chmodTolerant(name string, chmod func() error) error {
	err := chmod()
	if err == nil {
		return nil
	}
	// ENOTSUP (= EOPNOTSUPP) と ENOSYS は errors.ErrUnsupported に当たる。
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) || errors.Is(err, errors.ErrUnsupported) {
		slog.Warn("backup: the storage does not accept chmod; the permissions are left to the mount options", "path", name, "err", err)
		return nil
	}
	return fmt.Errorf("backup: chmod %s: %w", name, err)
}

// syncDir makes a rename durable. Errors are ignored: some network file
// systems do not support fsync on a directory.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}

// beforeDirOpen runs between the check and the open in DirStorage.Get. Tests
// use it to swap the file in that window.
var beforeDirOpen func(p string)

// Get opens key for reading. Only a regular file is an object: a symbolic
// link, even one that stays inside the root, is not followed.
//
// 保存先に書ける者が dump の名前で symlink を置くと、辿った先 (本体のコンテナの
// 中の任意のファイル) を管理画面のダウンロードで渡してしまう。List は symlink を
// 返さないが、API は名前を直接指定できるので、ここで止める。
func (d *DirStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	rel, err := d.rel(key)
	if err != nil {
		return nil, err
	}
	root, err := d.openRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	fi, err := regularFile(root, rel)
	if err != nil {
		return nil, err
	}
	if beforeDirOpen != nil {
		beforeDirOpen(filepath.Join(d.root, rel))
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, ErrNotFound
	}
	// 確かめてから開くまでの間に symlink や別のファイルへ差し替えられていないか。
	// 開いたものが確かめたものと同じ inode でなければ渡さない。
	if ofi, err := f.Stat(); err != nil || !os.SameFile(fi, ofi) {
		_ = f.Close()
		return nil, ErrNotFound
	}
	return f, nil
}

// Stat describes key. Like Get, it does not follow symbolic links.
func (d *DirStorage) Stat(_ context.Context, key string) (ObjectInfo, error) {
	rel, err := d.rel(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	root, err := d.openRoot()
	if err != nil {
		return ObjectInfo{}, err
	}
	defer func() { _ = root.Close() }()
	fi, err := regularFile(root, rel)
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// regularFile returns the FileInfo of rel when no symbolic link is on the
// way and rel is a regular file, ErrNotFound otherwise.
func regularFile(root *os.Root, rel string) (fs.FileInfo, error) {
	fi, err := lstatPath(root, rel)
	if errors.Is(err, errSymlink) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, notFoundOr(err)
	}
	// ディレクトリは object ではない (S3 にディレクトリが無いのと揃える)。
	if !fi.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	return fi, nil
}

// List returns the regular files whose key starts with prefix, in key order.
func (d *DirStorage) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), dirTempPrefix) {
			return nil
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if key == DirMarkerFile || !strings.HasPrefix(key, prefix) {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		out = append(out, ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("backup: list %s: %w", d.root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Delete removes key and then any directories it leaves empty, up to (not
// including) the root. A symbolic link on the way is an error; a symbolic
// link at key itself is removed without touching what it points to.
func (d *DirStorage) Delete(_ context.Context, key string) error {
	rel, err := d.rel(key)
	if err != nil {
		return err
	}
	root, err := d.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// 途中のディレクトリが symlink だと、辿った先のファイルを消してしまう。
	if dir := filepath.Dir(rel); dir != "." {
		if _, err := lstatPath(root, dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("backup: delete %s: %w", key, err)
		}
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: delete %s: %w", key, err)
	}
	// 世代を消し終えた後に空のディレクトリを残さない。中身が残っていれば
	// Remove が失敗するだけなので、そこで止める。
	for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
		if root.Remove(dir) != nil {
			break
		}
	}
	return nil
}

func notFoundOr(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// ctxReader stops a copy when ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
