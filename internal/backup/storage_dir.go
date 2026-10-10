package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// dirTempPrefix is the name prefix of files DirStorage.Put writes before
// renaming them into place. List never returns them.
const dirTempPrefix = ".tmp-"

// Permissions of what DirStorage creates: the owner and the group may read,
// nobody else may. The group may also delete (write on directories), but not
// change the files.
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
// be a directory and contain DirMarkerFile.
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
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("backup: storage directory: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("backup: storage directory %s is not a directory", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, DirMarkerFile)); err != nil {
		return nil, fmt.Errorf("backup: %s has no %s. Is the backup device mounted there? "+
			"Create the file on the mounted device (see docs/backup.md): %w", abs, DirMarkerFile, err)
	}
	return &DirStorage{root: abs}, nil
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

// Root returns the absolute path of the storage directory.
func (d *DirStorage) Root() string { return d.root }

// path maps key to a file path, rejecting keys that are not plain relative
// slash-separated paths (no "..", no empty or temp-file segments).
func (d *DirStorage) path(key string) (string, error) {
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
	return filepath.Join(d.root, filepath.FromSlash(key)), nil
}

// Put writes r to a temporary file next to key and renames it into place, so
// a failed Put never leaves a partial file under key.
func (d *DirStorage) Put(ctx context.Context, key string, r io.Reader) error {
	return d.put(ctx, key, r, func(tmp, p string) error {
		if err := os.Rename(tmp, p); err != nil {
			return fmt.Errorf("backup: rename %s: %w", key, err)
		}
		return nil
	})
}

// put writes r to a temporary file next to key and hands it to place.
func (d *DirStorage) put(ctx context.Context, key string, r io.Reader, place func(tmp, p string) error) (err error) {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := d.mkdirs(dir); err != nil {
		return err
	}
	// CreateTemp は 0600 で作る。書き終える前に他から読まれないよう、そのまま書き、
	// 置く前に filePerm に広げる。
	f, err := os.CreateTemp(dir, dirTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("backup: create temp file: %w", err)
	}
	tmp := f.Name()
	closed := false
	defer func() {
		if err != nil {
			if !closed {
				_ = f.Close()
			}
			_ = os.Remove(tmp)
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
	if err = f.Chmod(filePerm); err != nil {
		return fmt.Errorf("backup: chmod %s: %w", key, err)
	}
	closed = true
	if err = f.Close(); err != nil {
		return fmt.Errorf("backup: close %s: %w", key, err)
	}
	if err = place(tmp, p); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// mkdirs creates the directories from the root down to dir with dirPerm.
// Directories that already exist are left as they are.
//
// 既にあるディレクトリの mode は変えない。運営者が手で付けた setgid や、別の
// UID が作ったものを、こちらの都合で書き換えない。
func (d *DirStorage) mkdirs(dir string) error {
	rel, err := filepath.Rel(d.root, dir)
	if err != nil || rel == "." {
		return err
	}
	cur := d.root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		err := os.Mkdir(cur, dirPerm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("backup: mkdir %s: %w", cur, err)
		}
		fi, err := os.Stat(cur)
		if err != nil {
			return fmt.Errorf("backup: mkdir %s: %w", cur, err)
		}
		// 親の setgid を引き継いだ (グループを共有する) ディレクトリでは、それを
		// 残す。落とすと、その下に作るファイルのグループが自分の主グループになり、
		// 本体から読めなくなる。
		if err := os.Chmod(cur, dirPerm|fi.Mode()&fs.ModeSetgid); err != nil {
			return fmt.Errorf("backup: chmod %s: %w", cur, err)
		}
	}
	return nil
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
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	fi, err := d.statFile(p)
	if err != nil {
		return nil, err
	}
	if beforeDirOpen != nil {
		beforeDirOpen(p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, notFoundOr(err)
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
	p, err := d.path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := d.statFile(p)
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// statFile returns the FileInfo of p when p and every directory between the
// root and p are not symbolic links and p is a regular file.
func (d *DirStorage) statFile(p string) (fs.FileInfo, error) {
	rel, err := filepath.Rel(d.root, p)
	if err != nil {
		return nil, err
	}
	cur := d.root
	var fi fs.FileInfo
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		if fi, err = os.Lstat(cur); err != nil {
			return nil, notFoundOr(err)
		}
		// 途中のディレクトリが symlink でも、根の外を指しうる。
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, ErrNotFound
		}
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
// including) the root.
func (d *DirStorage) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: delete %s: %w", key, err)
	}
	// 世代を消し終えた後に空のディレクトリを残さない。中身が残っていれば
	// Remove が失敗するだけなので、そこで止める。
	for dir := filepath.Dir(p); dir != d.root && strings.HasPrefix(dir, d.root); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
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
