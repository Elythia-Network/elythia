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

// NewDirStorage returns a DirStorage rooted at root, which must already exist
// and be a directory.
//
// 根のディレクトリを作らないのが要点。NAS の mount が外れていると mount 先の
// ディレクトリ自体が無いことが多く、ここで作ると同じホストのディスクへ黙って
// 書き始める (同じホストのディスクはバックアップに数えない、#3457)。
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
	return &DirStorage{root: abs}, nil
}

// Root returns the absolute path of the storage directory.
func (d *DirStorage) Root() string { return d.root }

// path maps key to a file path, rejecting keys that are not plain relative
// slash-separated paths (no "..", no empty or temp-file segments).
func (d *DirStorage) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, `\`) || path.Clean(key) != key {
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
func (d *DirStorage) Put(ctx context.Context, key string, r io.Reader) (err error) {
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
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, ctxReader{ctx: ctx, r: r}); err != nil {
		return fmt.Errorf("backup: write %s: %w", key, err)
	}
	// rename の前に中身をディスクへ落とす。落とさずに rename すると、電源断の後に
	// 名前だけあって中身が空のファイルが残りうる。
	if err = f.Sync(); err != nil {
		return fmt.Errorf("backup: sync %s: %w", key, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("backup: close %s: %w", key, err)
	}
	if err = os.Rename(tmp, p); err != nil {
		return fmt.Errorf("backup: rename %s: %w", key, err)
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
		if !strings.HasPrefix(key, prefix) {
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
