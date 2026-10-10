package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// failingReader returns n bytes of data and then an error, like a pg_dump
// that dies partway.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func readAll(t *testing.T, st Storage, key string) []byte {
	t.Helper()
	r, err := st.Get(context.Background(), key)
	require.NoError(t, err)
	defer r.Close()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return b
}

// storageContract checks the behaviour every Storage must have.
func storageContract(t *testing.T, st Storage) {
	ctx := context.Background()

	_, err := st.Get(ctx, "generations/x/meta.json")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = st.Stat(ctx, "generations/x/meta.json")
	assert.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, st.Delete(ctx, "generations/x/meta.json"), "deleting a missing key is not an error")

	require.NoError(t, st.Put(ctx, "generations/b/two", strings.NewReader("22")))
	require.NoError(t, st.Put(ctx, "generations/a/one", strings.NewReader("1")))
	require.NoError(t, st.Put(ctx, "other", strings.NewReader("333")))
	assert.Equal(t, []byte("1"), readAll(t, st, "generations/a/one"))

	info, err := st.Stat(ctx, "generations/b/two")
	require.NoError(t, err)
	assert.Equal(t, "generations/b/two", info.Key)
	assert.Equal(t, int64(2), info.Size)
	assert.WithinDuration(t, time.Now(), info.ModTime, time.Hour)

	objs, err := st.List(ctx, "generations/")
	require.NoError(t, err)
	require.Len(t, objs, 2)
	assert.Equal(t, "generations/a/one", objs[0].Key)
	assert.Equal(t, "generations/b/two", objs[1].Key)
	assert.Equal(t, int64(2), objs[1].Size)
	all, err := st.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// 上書きは中身を入れ替える。
	require.NoError(t, st.Put(ctx, "generations/a/one", strings.NewReader("one")))
	assert.Equal(t, []byte("one"), readAll(t, st, "generations/a/one"))

	// 途中で失敗した Put は、新しい key に何も残さず、既存の key を壊さない。
	boom := errors.New("boom")
	err = st.Put(ctx, "generations/c/partial", &failingReader{data: []byte("half"), err: boom})
	require.ErrorIs(t, err, boom)
	_, err = st.Stat(ctx, "generations/c/partial")
	assert.ErrorIs(t, err, ErrNotFound)
	err = st.Put(ctx, "generations/a/one", &failingReader{data: []byte("xx"), err: boom})
	require.ErrorIs(t, err, boom)
	assert.Equal(t, []byte("one"), readAll(t, st, "generations/a/one"))
	objs, err = st.List(ctx, "generations/")
	require.NoError(t, err)
	assert.Len(t, objs, 2)

	require.NoError(t, st.Delete(ctx, "generations/a/one"))
	_, err = st.Stat(ctx, "generations/a/one")
	assert.ErrorIs(t, err, ErrNotFound)

	for _, key := range []string{"", "/abs"} {
		assert.Error(t, st.Put(ctx, key, strings.NewReader("x")), key)
		_, err := st.Get(ctx, key)
		assert.Error(t, err, key)
		_, err = st.Stat(ctx, key)
		assert.Error(t, err, key)
		assert.Error(t, st.Delete(ctx, key), key)
	}
}

func TestDirStorage_Contract(t *testing.T) {
	st, err := NewDirStorage(markedDir(t))
	require.NoError(t, err)
	storageContract(t, st)
}

func TestS3Storage_Contract(t *testing.T) {
	storageContract(t, newS3Storage(t))
}

func TestNewDirStorage_RequiresExistingDirectory(t *testing.T) {
	_, err := NewDirStorage("")
	require.Error(t, err)

	missing := filepath.Join(t.TempDir(), "nas")
	_, err = NewDirStorage(missing)
	require.Error(t, err)
	_, statErr := os.Stat(missing)
	assert.True(t, os.IsNotExist(statErr), "an unmounted path must not be created")

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, err = NewDirStorage(file)
	require.ErrorContains(t, err, "not a directory")

	// 目印が無ければ、ディレクトリがあっても書かない (mount が外れて、ホストの空の
	// ディレクトリが見えている状態)。
	unmounted := t.TempDir()
	_, err = NewDirStorage(unmounted)
	require.ErrorContains(t, err, DirMarkerFile)
	entries, err := os.ReadDir(unmounted)
	require.NoError(t, err)
	assert.Empty(t, entries, "the marker is not created")

	require.NoError(t, CreateDirMarker(unmounted))
	require.NoError(t, CreateDirMarker(unmounted), "creating it twice is fine")
	st, err := NewDirStorage(unmounted)
	require.NoError(t, err)
	assert.Equal(t, unmounted, st.Root())
	objs, err := st.List(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, objs, "the marker is not an object")

	wd, err := os.Getwd()
	require.NoError(t, err)
	t.Chdir(unmounted)
	st, err = NewDirStorage(".")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(st.Root()))
	t.Chdir(wd)

	require.Error(t, CreateDirMarker(filepath.Join(t.TempDir(), "missing")))
}

func TestDirStorage_RejectsKeysOutsideRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, CreateDirMarker(root))
	st, err := NewDirStorage(root)
	require.NoError(t, err)
	ctx := context.Background()
	for _, key := range []string{"../escape", "a/../../escape", "a//b", "a/./b", `a\b`, "a/", ".tmp-x", "a/.tmp-x", "..", DirMarkerFile} {
		assert.Error(t, st.Put(ctx, key, strings.NewReader("x")), key)
	}
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "nothing written next to the root")
}

func TestDirStorage_PutLeavesNoTempFile(t *testing.T) {
	root := markedDir(t)
	st, err := NewDirStorage(root)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("secret")))

	require.Error(t, st.Put(ctx, "generations/a/broken", &failingReader{data: []byte("x"), err: io.ErrClosedPipe}))
	entries, err := os.ReadDir(filepath.Join(root, "generations", "a"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "dump.pgc", entries[0].Name())

	// List は書きかけの一時ファイルを返さない (別のプロセスが書いている途中)。
	require.NoError(t, os.WriteFile(filepath.Join(root, "generations", "a", dirTempPrefix+"123"), []byte("x"), 0o600))
	objs, err := st.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, objs, 1)

	// ディレクトリは object ではない。
	_, err = st.Stat(ctx, "generations/a")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = st.Get(ctx, "generations/a")
	assert.ErrorIs(t, err, ErrNotFound)

	// キャンセルした ctx では書かない。
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, st.Put(cctx, "generations/b/x", strings.NewReader("x")), context.Canceled)
}

func TestDirStorage_DeleteRemovesEmptyDirectories(t *testing.T) {
	root := markedDir(t)
	st, err := NewDirStorage(root)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, st.Put(ctx, "generations/a/one", strings.NewReader("1")))
	require.NoError(t, st.Put(ctx, "generations/a/two", strings.NewReader("2")))
	require.NoError(t, st.Delete(ctx, "generations/a/one"))
	_, err = os.Stat(filepath.Join(root, "generations", "a"))
	require.NoError(t, err, "a directory that still has files stays")
	require.NoError(t, st.Delete(ctx, "generations/a/two"))
	_, err = os.Stat(filepath.Join(root, "generations"))
	assert.True(t, os.IsNotExist(err), "empty generation directories are removed")
	_, err = os.Stat(root)
	require.NoError(t, err, "the root itself stays")
}

func TestDirStorage_ListAndPutErrors(t *testing.T) {
	root := markedDir(t)
	st, err := NewDirStorage(root)
	require.NoError(t, err)
	ctx := context.Background()
	// 親が通常のファイルだと、ディレクトリを作れない。
	require.NoError(t, st.Put(ctx, "file", strings.NewReader("x")))
	require.Error(t, st.Put(ctx, "file/child", strings.NewReader("x")))

	// 根が消えると List は失敗する。
	require.NoError(t, os.RemoveAll(root))
	_, err = st.List(ctx, "")
	require.Error(t, err)
}

// TestS3Storage_Multipart sends an object larger than the part size, so it
// goes through CreateMultipartUpload / UploadPart / Complete.
func TestS3Storage_Multipart(t *testing.T) {
	st := newS3Storage(t)
	st.PartSize = 5 << 20 // S3 の part の最小 (最後以外)
	ctx := context.Background()
	for _, size := range []int{2 * st.PartSize, 2*st.PartSize + 12345} {
		data := randomBytes(t, size)
		require.NoError(t, st.Put(ctx, "big", bytes.NewReader(data)))
		assert.Equal(t, sha256Hex(data), sha256Hex(rawS3Get(t, st, "big")), size)
	}
}

// TestS3Storage_MultipartFailureLeavesNothing: a reader that fails after the
// first part must leave neither an object nor an unfinished multipart upload
// (whose parts would keep costing storage).
func TestS3Storage_MultipartFailureLeavesNothing(t *testing.T) {
	st := newS3Storage(t)
	st.PartSize = 5 << 20
	ctx := context.Background()
	boom := errors.New("pg_dump died")
	err := st.Put(ctx, "big", &failingReader{data: randomBytes(t, 2*st.PartSize+100), err: boom})
	require.ErrorIs(t, err, boom)
	_, err = st.Stat(ctx, "big")
	assert.ErrorIs(t, err, ErrNotFound)
	ups, err := st.client.(*s3.Client).ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(minioBucket),
		// MinIO は object の key そのものを prefix に渡さないと未完了の upload を返さない。
		Prefix: aws.String(st.prefix + "big"),
	})
	require.NoError(t, err)
	assert.Empty(t, ups.Uploads)
}

func TestS3Storage_PresignGet(t *testing.T) {
	st := newS3Storage(t)
	ctx := context.Background()
	require.NoError(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("dump-bytes")))
	u, err := st.PresignGet(ctx, "generations/a/dump.pgc", time.Minute)
	require.NoError(t, err)
	resp, err := http.Get(u) //nolint:gosec,noctx // テストで MinIO の署名付き URL を叩く
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "dump-bytes", string(body))
	assert.Equal(t, `attachment; filename="dump.pgc"`, resp.Header.Get("Content-Disposition"), "not a generation ID: the name only")

	// 世代のファイルは "<世代ID>-<名前>" で保存させる。
	gen := Key("20261010T040000Z", DumpFileAge)
	require.NoError(t, st.Put(ctx, gen, strings.NewReader("enc")))
	u, err = st.PresignGet(ctx, gen, time.Minute)
	require.NoError(t, err)
	resp, err = http.Get(u) //nolint:gosec,noctx // テストで MinIO の署名付き URL を叩く
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `attachment; filename="20261010T040000Z-dump.pgc.age"`, resp.Header.Get("Content-Disposition"))

	short, err := st.PresignGet(ctx, "generations/a/dump.pgc", time.Second)
	require.NoError(t, err)
	time.Sleep(2 * time.Second)
	resp, err = http.Get(short) //nolint:gosec,noctx // テストで MinIO の署名付き URL を叩く
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "expired URL")

	_, err = st.PresignGet(ctx, "", time.Minute)
	require.Error(t, err)
}

func TestS3Storage_Prefix(t *testing.T) {
	startMinIO(t)
	ctx := context.Background()
	base := "prefix-" + randomHex(t, 4)
	for _, p := range []string{base, "/" + base + "/", base + "/"} {
		st, err := NewS3Storage(minioOptions(p))
		require.NoError(t, err)
		assert.Equal(t, base+"/", st.prefix, p)
	}
	st, err := NewS3Storage(minioOptions(base))
	require.NoError(t, err)
	require.NoError(t, st.Put(ctx, "k", strings.NewReader("v")))
	root, err := NewS3Storage(minioOptions(""))
	require.NoError(t, err)
	assert.Equal(t, "", root.prefix)
	assert.Equal(t, []byte("v"), readAll(t, root, base+"/k"))
	objs, err := st.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, objs, 1)
	assert.Equal(t, "k", objs[0].Key, "keys are relative to the prefix")
}

func TestS3Storage_ListPaginates(t *testing.T) {
	st := newS3Storage(t)
	ctx := context.Background()
	// MinIO の 1 ページは最大 1000 件。
	const n = 1005
	for i := range n {
		require.NoError(t, st.Put(ctx, "k/"+string(rune('a'+i%26))+randomHex(t, 4), strings.NewReader("x")))
	}
	objs, err := st.List(ctx, "k/")
	require.NoError(t, err)
	assert.Len(t, objs, n)
}

func TestS3Storage_Errors(t *testing.T) {
	startMinIO(t)
	ctx := context.Background()
	o := minioOptions("x")
	o.Bucket = "no-such-bucket-" + randomHex(t, 4)
	st, err := NewS3Storage(o)
	require.NoError(t, err)
	st.PartSize = 5 << 20
	assert.Error(t, st.Put(ctx, "k", strings.NewReader("v")))
	assert.Error(t, st.Put(ctx, "k", bytes.NewReader(randomBytes(t, st.PartSize+1))))
	_, err = st.Get(ctx, "k")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
	_, err = st.List(ctx, "")
	assert.Error(t, err)
	assert.Error(t, st.Delete(ctx, "k"))

	ok := newS3Storage(t)
	ok.PartSize = 0 // 0 は既定の大きさ
	require.NoError(t, ok.Put(ctx, "k", strings.NewReader("v")))
	failing := &failingReader{data: nil, err: errors.New("read failed")}
	require.ErrorContains(t, ok.Put(ctx, "k2", failing), "read failed")
}

func TestNewS3Storage_Validation(t *testing.T) {
	_, err := NewS3Storage(config.BackupS3Options{AccessKey: "a", SecretKey: "b"})
	require.ErrorContains(t, err, "bucket")
	_, err = NewS3Storage(config.BackupS3Options{Bucket: "b", AccessKey: "a"})
	require.ErrorContains(t, err, "secretKey")
	st, err := NewS3Storage(config.BackupS3Options{Bucket: "b", AccessKey: "a", SecretKey: "s"})
	require.NoError(t, err)
	assert.Equal(t, DefaultS3PartSize, st.PartSize)
}

func TestIsS3NotFound(t *testing.T) {
	assert.False(t, isS3NotFound(errors.New("other")))
}

func TestOpenStorage(t *testing.T) {
	dir := markedDir(t)
	st, err := OpenStorage(config.BackupStorageOptions{Type: StorageTypeDir, Dir: config.BackupDirectoryOptions{Path: dir}})
	require.NoError(t, err)
	assert.IsType(t, &DirStorage{}, st)
	_, ok := st.(Presigner)
	assert.False(t, ok, "a directory cannot hand out URLs")

	st, err = OpenStorage(config.BackupStorageOptions{Type: StorageTypeS3, S3: config.BackupS3Options{Bucket: "b", AccessKey: "a", SecretKey: "s"}})
	require.NoError(t, err)
	_, ok = st.(Presigner)
	assert.True(t, ok)

	_, err = OpenStorage(config.BackupStorageOptions{})
	require.ErrorContains(t, err, "storage.type is empty")
	_, err = OpenStorage(config.BackupStorageOptions{Type: "local"})
	require.ErrorContains(t, err, `unknown storage.type "local"`)
}

func TestNewS3Storage_EndpointErrorsDoNotEchoTheValue(t *testing.T) {
	for _, ep := range []string{"https://AKIA:SECRETPW@s3.example.com", "http://[::1", "s3.example.com", "ftp://s3.example.com", "https:///no-host"} {
		_, err := NewS3Storage(config.BackupS3Options{Endpoint: ep, Bucket: "b", AccessKey: "a", SecretKey: "s"})
		require.Error(t, err, ep)
		assert.NotContains(t, err.Error(), "SECRETPW", ep)
		assert.NotContains(t, err.Error(), ep, ep)
	}
	_, err := NewS3Storage(config.BackupS3Options{Endpoint: "http://minio:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	require.NoError(t, err)
}

func TestDownloadName(t *testing.T) {
	assert.Equal(t, "20261010T040000Z-meta.json", downloadName(Key("20261010T040000Z", MetaFile)))
	assert.Equal(t, "dump.pgc", downloadName("generations/latest/dump.pgc"))
	// 引用符・改行・非 ASCII はヘッダーを壊すので置き換える。
	assert.Equal(t, "a_b__c___.txt", downloadName("x/a\"b\r\nc日本語.txt"))
}

// TestDirStorage_PutPermissions: the group may read the files and delete
// them (the main server runs as another UID, #3462); others get nothing,
// whatever the umask is.
func TestDirStorage_PutPermissions(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	root := markedDir(t)
	// 運営者の手順と同じく、根に setgid を付けてグループを共有する。
	require.NoError(t, os.Chmod(root, 0o770|os.ModeSetgid))
	st, err := NewDirStorage(root)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("secret")))

	fi, err := os.Stat(filepath.Join(root, "generations", "a", "dump.pgc"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm(), "the group reads, others do not")
	rfi, err := os.Stat(root)
	require.NoError(t, err)
	rootGID := rfi.Sys().(*syscall.Stat_t).Gid
	assert.Equal(t, rootGID, fi.Sys().(*syscall.Stat_t).Gid, "the file takes the group of the root")
	for _, dir := range []string{"generations", filepath.Join("generations", "a")} {
		fi, err := os.Stat(filepath.Join(root, dir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o770), fi.Mode().Perm(), dir)
		assert.NotZero(t, fi.Mode()&os.ModeSetgid, "%s keeps the inherited setgid", dir)
		assert.Equal(t, rootGID, fi.Sys().(*syscall.Stat_t).Gid, dir)
	}

	// 既にあるディレクトリの mode は変えない。
	require.NoError(t, os.Mkdir(filepath.Join(root, "generations", "b"), 0o700))
	require.NoError(t, os.Chmod(filepath.Join(root, "generations", "b"), 0o700))
	require.NoError(t, st.Put(ctx, "generations/b/x", strings.NewReader("x")))
	fi, err = os.Stat(filepath.Join(root, "generations", "b"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
}

// TestDirStorage_DoesNotFollowSymlinks: someone who can write to the
// storage must not be able to hand out files outside it through a symlink.
func TestDirStorage_DoesNotFollowSymlinks(t *testing.T) {
	root := markedDir(t)
	st, err := NewDirStorage(root)
	require.NoError(t, err)
	ctx := context.Background()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "dump.pgc"), []byte("host secret"), 0o600))
	require.NoError(t, st.Put(ctx, "generations/a/real", strings.NewReader("in storage")))

	// 世代の中の symlink (根の外を指すものと、根の中を指すもの)。
	require.NoError(t, os.Symlink(filepath.Join(outside, "dump.pgc"), filepath.Join(root, "generations", "a", "dump.pgc")))
	require.NoError(t, os.Symlink(filepath.Join(root, "generations", "a", "real"), filepath.Join(root, "generations", "a", "inner")))
	// 途中のディレクトリが symlink。
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "generations", "b")))

	for _, key := range []string{"generations/a/dump.pgc", "generations/a/inner", "generations/b/dump.pgc"} {
		_, err := st.Stat(ctx, key)
		assert.ErrorIs(t, err, ErrNotFound, key)
		_, err = st.Get(ctx, key)
		assert.ErrorIs(t, err, ErrNotFound, key)
	}
	objs, err := st.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, objs, 1)
	assert.Equal(t, "generations/a/real", objs[0].Key)
	b := readAll(t, st, "generations/a/real")
	assert.Equal(t, "in storage", string(b))
}

// TestDirStorage_GetRefusesASwapAfterTheCheck replaces the checked file
// before it is opened: with a symlink out of the root, and with another file
// inside the root (which os.Root alone would open).
func TestDirStorage_GetRefusesASwapAfterTheCheck(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { beforeDirOpen = nil })
	t.Run("symlink out of the root", func(t *testing.T) {
		root := markedDir(t)
		st, err := NewDirStorage(root)
		require.NoError(t, err)
		require.NoError(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("in storage")))
		outside := filepath.Join(t.TempDir(), "secret")
		require.NoError(t, os.WriteFile(outside, []byte("host secret"), 0o600))
		beforeDirOpen = func(p string) {
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Symlink(outside, p))
		}
		_, err = st.Get(ctx, "generations/a/dump.pgc")
		assert.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("another file", func(t *testing.T) {
		root := markedDir(t)
		st, err := NewDirStorage(root)
		require.NoError(t, err)
		require.NoError(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("checked")))
		require.NoError(t, st.Put(ctx, "generations/b/dump.pgc", strings.NewReader("other")))
		beforeDirOpen = func(p string) {
			require.NoError(t, os.Rename(filepath.Join(root, "generations", "b", "dump.pgc"), p))
		}
		_, err = st.Get(ctx, "generations/a/dump.pgc")
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

// TestDirStorage_ChmodRefusedByTheFileSystem: a file system without Unix
// permissions (CIFS without unix extensions) refuses chmod even for the
// owner. Taking a backup must still work there; other errors still stop it.
func TestDirStorage_ChmodRefusedByTheFileSystem(t *testing.T) {
	origFile, origDir := chmodFile, chmodDir
	t.Cleanup(func() { chmodFile, chmodDir = origFile, origDir })
	ctx := context.Background()
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.ENOTSUP, syscall.EINVAL} {
		t.Run(errno.Error(), func(t *testing.T) {
			var fileCalls, dirCalls int
			chmodFile = func(f *os.File, _ fs.FileMode) error {
				fileCalls++
				return &fs.PathError{Op: "chmod", Path: f.Name(), Err: errno}
			}
			chmodDir = func(_ *os.Root, name string, _ fs.FileMode) error {
				dirCalls++
				return &fs.PathError{Op: "chmod", Path: name, Err: errno}
			}
			st, err := NewDirStorage(markedDir(t))
			require.NoError(t, err)
			require.NoError(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("x")))
			assert.Equal(t, "x", string(readAll(t, st, "generations/a/dump.pgc")))
			assert.Equal(t, 1, fileCalls)
			assert.Equal(t, 2, dirCalls, "generations and generations/a")
		})
	}
	t.Run("other errors stop the put", func(t *testing.T) {
		chmodDir = origDir
		chmodFile = func(f *os.File, _ fs.FileMode) error {
			return &fs.PathError{Op: "chmod", Path: f.Name(), Err: syscall.EIO}
		}
		root := markedDir(t)
		st, err := NewDirStorage(root)
		require.NoError(t, err)
		require.ErrorIs(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("x")), syscall.EIO)
		entries, err := os.ReadDir(filepath.Join(root, "generations", "a"))
		require.NoError(t, err)
		assert.Empty(t, entries, "no temp file is left")

		chmodFile = origFile
		chmodDir = func(_ *os.Root, name string, _ fs.FileMode) error {
			return &fs.PathError{Op: "chmod", Path: name, Err: syscall.EIO}
		}
		require.ErrorIs(t, st.Put(ctx, "generations/b/dump.pgc", strings.NewReader("x")), syscall.EIO)
	})
}

// TestNewDirStorage_ResolvesASymlinkedRoot: with the root given through a
// symbolic link, List must see what Put wrote.
func TestNewDirStorage_ResolvesASymlinkedRoot(t *testing.T) {
	real := markedDir(t)
	link := filepath.Join(t.TempDir(), "backup")
	require.NoError(t, os.Symlink(real, link))
	st, err := NewDirStorage(link)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)
	assert.Equal(t, want, st.Root())
	ctx := context.Background()
	require.NoError(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("x")))
	objs, err := st.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, objs, 1)
	assert.Equal(t, "generations/a/dump.pgc", objs[0].Key)

	// 目印は実体の側で探す。symlink の先に目印が無ければ拒む。
	bare := filepath.Join(t.TempDir(), "bare")
	require.NoError(t, os.Symlink(t.TempDir(), bare))
	_, err = NewDirStorage(bare)
	require.ErrorContains(t, err, DirMarkerFile)
	_, err = NewDirStorage(filepath.Join(t.TempDir(), "dangling"))
	require.Error(t, err)
}

// TestDirStorage_PutAndDeleteDoNotFollowSymlinks: someone in the storage's
// group can replace a generation directory with a symbolic link; neither
// writing nor deleting may reach through it.
func TestDirStorage_PutAndDeleteDoNotFollowSymlinks(t *testing.T) {
	root := markedDir(t)
	st, err := NewDirStorage(root)
	require.NoError(t, err)
	ctx := context.Background()
	outside := t.TempDir()
	victim := filepath.Join(outside, "dump.pgc")
	require.NoError(t, os.WriteFile(victim, []byte("host file"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "generations"), 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "generations", "b")))
	// 根の中を指すものも受けない。
	require.NoError(t, st.Put(ctx, "generations/a/x", strings.NewReader("x")))
	require.NoError(t, os.Symlink(filepath.Join(root, "generations", "a"), filepath.Join(root, "generations", "c")))

	for _, key := range []string{"generations/b/dump.pgc", "generations/b/new", "generations/c/x", "generations/c/new"} {
		assert.ErrorIs(t, st.Put(ctx, key, strings.NewReader("overwritten")), errSymlink, key)
		assert.ErrorIs(t, st.Delete(ctx, key), errSymlink, key)
	}
	b, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "host file", string(b))
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "nothing written outside")
	assert.Equal(t, "x", string(readAll(t, st, "generations/a/x")))

	// 置き先そのものが symlink なら、辿らずに symlink を置き換える / 消す。
	link := filepath.Join(root, "generations", "a", "dump.pgc")
	require.NoError(t, os.Symlink(victim, link))
	require.NoError(t, st.Put(ctx, "generations/a/dump.pgc", strings.NewReader("new dump")))
	fi, err := os.Lstat(link)
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(victim, link))
	require.NoError(t, st.Delete(ctx, "generations/a/dump.pgc"))
	b, err = os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "host file", string(b))
}
