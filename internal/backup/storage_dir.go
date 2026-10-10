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

// PutNew is Put, except that it fails with ErrExists instead of replacing an
// existing key.
//
// 一時ファイルを hard link で置く。link は置き先が既にあれば失敗するので、同じ key へ
// 同時に書いても片方だけが通る。link を持たないファイルシステム (一部の SMB など) では、
// 有無を確かめてから rename する形に落ちる。その間に割り込まれる隙は残る。
func (d *DirStorage) PutNew(ctx context.Context, key string, r io.Reader) error {
	return d.put(ctx, key, r, func(tmp, p string) error {
		err := os.Link(tmp, p)
		switch {
		case err == nil:
			_ = os.Remove(tmp)
			return nil
		case errors.Is(err, fs.ErrExist):
			return ErrExists
		}
		if _, serr := os.Lstat(p); serr == nil {
			return ErrExists
		}
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backup: mkdir %s: %w", dir, err)
	}
	// CreateTemp は 0600 で作る。バックアップには秘密鍵や token が入るので、
	// 他の利用者から読めないままにする。
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

// syncDir makes a rename durable. Errors are ignored: some network file
// systems do not support fsync on a directory.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}

// Get opens key for reading.
func (d *DirStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	if _, err := d.statFile(p); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, notFoundOr(err)
	}
	return f, nil
}

// Stat describes key.
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

func (d *DirStorage) statFile(p string) (fs.FileInfo, error) {
	fi, err := os.Stat(p)
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
