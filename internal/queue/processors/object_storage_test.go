package processors_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeObjectStorage records deletions and can be made to fail.
type fakeObjectStorage struct {
	mu      sync.Mutex
	deleted []string
	err     error
}

func (f *fakeObjectStorage) Put(string, io.Reader) (string, error) { return "", nil }
func (f *fakeObjectStorage) Get(string) (io.ReadCloser, error)     { return nil, errors.New("unused") }

func (f *fakeObjectStorage) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	return f.err
}

func (f *fakeObjectStorage) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// fakeRemoteFileCleaner serves the queued batches in order and records the IDs
// handed to DeleteByIDs.
type fakeRemoteFileCleaner struct {
	batches    [][]*model.DriveFile
	deletedIDs []string
	listErr    error
	deleteErr  error
	listCalls  int
}

func (f *fakeRemoteFileCleaner) ListRemoteCache(int) ([]*model.DriveFile, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	next := f.batches[0]
	f.batches = f.batches[1:]
	return next, nil
}

func (f *fakeRemoteFileCleaner) ExpireByIDs(ids []string) (int64, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	f.deletedIDs = append(f.deletedIDs, ids...)
	return int64(len(ids)), nil
}

func strptr(s string) *string { return &s }

func deleteFileTask(t *testing.T, key string) driver.Task {
	t.Helper()
	body, err := json.Marshal(queue.ObjectStorageDeleteFilePayload{Key: key})
	require.NoError(t, err)
	return driver.RawTask{TypeName: queue.TaskTypeObjectStorageDeleteFile, Body: body}
}

func TestObjectStorageProcessor_HandleDeleteFile(t *testing.T) {
	st := &fakeObjectStorage{}
	p := processors.NewObjectStorageProcessor(st, nil, nil)

	require.NoError(t, p.HandleDeleteFile(context.Background(), deleteFileTask(t, "abc.webp")))
	assert.Equal(t, []string{"abc.webp"}, st.keys())
}

func TestObjectStorageProcessor_HandleDeleteFile_StorageError(t *testing.T) {
	st := &fakeObjectStorage{err: errors.New("503 slow down")}
	p := processors.NewObjectStorageProcessor(st, nil, nil)

	// error を返して再試行させる (一時的な 5xx は待てば回復する)。
	err := p.HandleDeleteFile(context.Background(), deleteFileTask(t, "abc.webp"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "abc.webp")
}

func TestObjectStorageProcessor_HandleDeleteFile_NoStorage(t *testing.T) {
	p := processors.NewObjectStorageProcessor(nil, nil, nil)
	assert.NoError(t, p.HandleDeleteFile(context.Background(), deleteFileTask(t, "abc.webp")))
}

func TestObjectStorageProcessor_HandleDeleteFile_EmptyKey(t *testing.T) {
	st := &fakeObjectStorage{}
	p := processors.NewObjectStorageProcessor(st, nil, nil)

	require.NoError(t, p.HandleDeleteFile(context.Background(), deleteFileTask(t, "")))
	assert.Empty(t, st.keys())
}

func TestObjectStorageProcessor_HandleDeleteFile_MalformedPayload(t *testing.T) {
	st := &fakeObjectStorage{}
	p := processors.NewObjectStorageProcessor(st, nil, nil)

	// 壊れた payload は何度試しても直らないので retry させない (error を返さない)。
	task := driver.RawTask{TypeName: queue.TaskTypeObjectStorageDeleteFile, Body: []byte(`{not json`)}
	require.NoError(t, p.HandleDeleteFile(context.Background(), task))
	assert.Empty(t, st.keys())
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles(t *testing.T) {
	// 1 バッチ (= batchSize 未満) で打ち切られることを確認する。
	repo := &fakeRemoteFileCleaner{batches: [][]*model.DriveFile{{
		{ID: "f1", AccessKey: strptr("a1"), ThumbnailAccessKey: strptr("t1"), WebpublicAccessKey: strptr("w1")},
		{ID: "f2", AccessKey: strptr("a2")},
	}}}
	st := &fakeObjectStorage{}
	p := processors.NewObjectStorageProcessor(st, nil, repo)

	require.NoError(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
	assert.Equal(t, []string{"a1", "t1", "w1", "a2"}, st.keys())
	assert.Equal(t, []string{"f1", "f2"}, repo.deletedIDs)
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_RoutesStoredInternalToLocal(t *testing.T) {
	// object storage 有効化より前に保存された行はローカル FS に実体がある。
	repo := &fakeRemoteFileCleaner{batches: [][]*model.DriveFile{{
		{ID: "f1", AccessKey: strptr("remote"), StoredInternal: false},
		{ID: "f2", AccessKey: strptr("legacy"), StoredInternal: true},
	}}}
	object := &fakeObjectStorage{}
	local := &fakeObjectStorage{}
	p := processors.NewObjectStorageProcessor(object, local, repo)

	require.NoError(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
	assert.Equal(t, []string{"remote"}, object.keys())
	assert.Equal(t, []string{"legacy"}, local.keys())
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_MultipleBatches(t *testing.T) {
	// 満杯のバッチが続くかぎりループし、空になったら止まる。
	full := make([]*model.DriveFile, 100)
	for i := range full {
		full[i] = &model.DriveFile{ID: "full" + strconv.Itoa(i), AccessKey: strptr("k" + strconv.Itoa(i))}
	}
	repo := &fakeRemoteFileCleaner{batches: [][]*model.DriveFile{full, {{ID: "tail", AccessKey: strptr("kt")}}}}
	st := &fakeObjectStorage{}
	p := processors.NewObjectStorageProcessor(st, nil, repo)

	require.NoError(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
	assert.Len(t, repo.deletedIDs, 101)
	assert.Equal(t, 2, repo.listCalls)
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_StorageErrorDoesNotStop(t *testing.T) {
	// 実体削除に失敗しても DB 行は消す。止めると同じ行を再 list し続けてしまう。
	repo := &fakeRemoteFileCleaner{batches: [][]*model.DriveFile{{
		{ID: "f1", AccessKey: strptr("a1")},
	}}}
	st := &fakeObjectStorage{err: errors.New("gone")}
	p := processors.NewObjectStorageProcessor(st, nil, repo)

	require.NoError(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
	assert.Equal(t, []string{"f1"}, repo.deletedIDs)
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_ListError(t *testing.T) {
	repo := &fakeRemoteFileCleaner{listErr: errors.New("db down")}
	p := processors.NewObjectStorageProcessor(&fakeObjectStorage{}, nil, repo)

	assert.Error(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_DeleteError(t *testing.T) {
	repo := &fakeRemoteFileCleaner{
		batches:   [][]*model.DriveFile{{{ID: "f1", AccessKey: strptr("a1")}}},
		deleteErr: errors.New("db down"),
	}
	p := processors.NewObjectStorageProcessor(&fakeObjectStorage{}, nil, repo)

	assert.Error(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_NoRepo(t *testing.T) {
	p := processors.NewObjectStorageProcessor(&fakeObjectStorage{}, nil, nil)
	assert.NoError(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_CancelledContext(t *testing.T) {
	repo := &fakeRemoteFileCleaner{batches: [][]*model.DriveFile{{{ID: "f1", AccessKey: strptr("a1")}}}}
	p := processors.NewObjectStorageProcessor(&fakeObjectStorage{}, nil, repo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, p.HandleCleanRemoteFiles(ctx, driver.RawTask{}))
	assert.Empty(t, repo.deletedIDs)
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_SkipsEmptyKeys(t *testing.T) {
	repo := &fakeRemoteFileCleaner{batches: [][]*model.DriveFile{{
		{ID: "f1", AccessKey: strptr(""), ThumbnailAccessKey: nil},
	}}}
	st := &fakeObjectStorage{}
	p := processors.NewObjectStorageProcessor(st, nil, repo)

	require.NoError(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
	assert.Empty(t, st.keys())
	assert.Equal(t, []string{"f1"}, repo.deletedIDs)
}

func TestObjectStorageProcessor_HandleCleanRemoteFiles_NoBackend(t *testing.T) {
	// storage 未配線でも DB 行の掃除だけは進める。
	repo := &fakeRemoteFileCleaner{batches: [][]*model.DriveFile{{{ID: "f1", AccessKey: strptr("a1")}}}}
	p := processors.NewObjectStorageProcessor(nil, nil, repo)

	require.NoError(t, p.HandleCleanRemoteFiles(context.Background(), driver.RawTask{}))
	assert.Equal(t, []string{"f1"}, repo.deletedIDs)
}

// TestObjectStorageProcessor_HandleDeleteFile_LocalBackend guards the case
// where object storage got unconfigured while deletes were still queued: the
// job must fail loudly instead of silently dropping the object.
func TestObjectStorageProcessor_HandleDeleteFile_LocalBackend(t *testing.T) {
	local := coredrive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	p := processors.NewObjectStorageProcessor(local, nil, nil)

	err := p.HandleDeleteFile(context.Background(), deleteFileTask(t, "abc.webp"))
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "not configured"))
}
