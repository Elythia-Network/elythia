package drive_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingObjectDeleteEnqueuer captures the keys handed to the objectStorage
// queue and can be made to fail so the inline fallback is exercised.
type recordingObjectDeleteEnqueuer struct {
	keys []string
	err  error
}

func (r *recordingObjectDeleteEnqueuer) EnqueueDeleteObjectStorageFile(key string) error {
	if r.err != nil {
		return r.err
	}
	r.keys = append(r.keys, key)
	return nil
}

// countingStorage wraps LocalStorage so tests can tell inline deletion apart
// from enqueued deletion.
type countingStorage struct {
	*drive.LocalStorage
	deleted []string
}

func (c *countingStorage) Delete(key string) error {
	c.deleted = append(c.deleted, key)
	return c.LocalStorage.Delete(key)
}

func svcWithEnqueuer(t *testing.T, f *model.DriveFile) (*drive.Service, *countingStorage, *recordingObjectDeleteEnqueuer, *testutil.MockDriveFileRepository) {
	t.Helper()
	repo := testutil.NewMockDriveFileRepository()
	repo.Files[f.ID] = f
	storage := &countingStorage{LocalStorage: drive.NewLocalStorage(t.TempDir(), "")}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(), storage, idGen)
	enq := &recordingObjectDeleteEnqueuer{}
	svc.SetObjectDeleteEnqueuer(enq)
	return svc, storage, enq, repo
}

func objectStoredFile() *model.DriveFile {
	uid := "u1"
	primary, thumb, web := "a1", "t1", "w1"
	return &model.DriveFile{
		ID: "f1", UserID: &uid,
		AccessKey: &primary, ThumbnailAccessKey: &thumb, WebpublicAccessKey: &web,
		StoredInternal: false,
	}
}

// TestDelete_EnqueuesObjectStorageDeletes は object storage 上の実体が
// 同期削除ではなく queue 経由になることを確認する (#2325)。
func TestDelete_EnqueuesObjectStorageDeletes(t *testing.T) {
	svc, storage, enq, repo := svcWithEnqueuer(t, objectStoredFile())

	require.NoError(t, svc.Delete(&model.User{ID: "u1"}, "f1"))

	assert.Equal(t, []string{"a1", "t1", "w1"}, enq.keys)
	assert.Empty(t, storage.deleted, "object storage の実体は worker が消すので同期削除しない")
	assert.Empty(t, repo.Files)
}

// TestDelete_StoredInternalDeletesInline は storedInternal=true な行
// (ローカル FS) が upstream 同様に同期削除されることを確認する。
func TestDelete_StoredInternalDeletesInline(t *testing.T) {
	f := objectStoredFile()
	f.StoredInternal = true
	svc, storage, enq, _ := svcWithEnqueuer(t, f)

	require.NoError(t, svc.Delete(&model.User{ID: "u1"}, "f1"))

	assert.Empty(t, enq.keys)
	assert.Equal(t, []string{"a1", "t1", "w1"}, storage.deleted)
}

// TestDelete_LinkRowTouchesNoStorage は実体を持たない link 行で削除も
// enqueue も行わないことを確認する。
func TestDelete_LinkRowTouchesNoStorage(t *testing.T) {
	f := objectStoredFile()
	f.IsLink = true
	svc, storage, enq, repo := svcWithEnqueuer(t, f)

	require.NoError(t, svc.Delete(&model.User{ID: "u1"}, "f1"))

	assert.Empty(t, enq.keys)
	assert.Empty(t, storage.deleted)
	assert.Empty(t, repo.Files)
}

// TestDelete_FallsBackToInlineWhenEnqueueFails は queue が死んでいても実体を
// 取りこぼさないことを確認する (追跡不能な孤児を作らないため)。
func TestDelete_FallsBackToInlineWhenEnqueueFails(t *testing.T) {
	svc, storage, enq, _ := svcWithEnqueuer(t, objectStoredFile())
	enq.err = errors.New("redis down")

	require.NoError(t, svc.Delete(&model.User{ID: "u1"}, "f1"))

	assert.Equal(t, []string{"a1", "t1", "w1"}, storage.deleted)
}

// TestDeleteAllByHost_EnqueuesObjectStorageDeletes covers the admin
// federation purge path.
func TestDeleteAllByHost_EnqueuesObjectStorageDeletes(t *testing.T) {
	host := "remote.example"
	f := objectStoredFile()
	f.UserHost = &host
	svc, storage, enq, _ := svcWithEnqueuer(t, f)

	n, err := svc.DeleteAllByHost(host)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Equal(t, []string{"a1", "t1", "w1"}, enq.keys)
	assert.Empty(t, storage.deleted)
}
