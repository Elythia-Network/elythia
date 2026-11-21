package drive_test

import (
	"errors"
	"testing"
	"time"

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
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(), storage, idGen, nil, nil, nil, nil)
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

// TestDelete_MigratedFileAlsoDeletesTheLocalCopy covers a file the
// local-to-object-storage migration copied (#4180): its local copy stays
// after the migration, and deleting the file removes it too.
func TestDelete_MigratedFileAlsoDeletesTheLocalCopy(t *testing.T) {
	f := objectStoredFile()
	at := time.Now()
	f.ObjectStorageMigratedAt = &at
	svc, _, enq, repo := svcWithEnqueuer(t, f)
	local := &countingStorage{LocalStorage: drive.NewLocalStorage(t.TempDir(), "")}
	svc.SetLocalStorage(local)

	require.NoError(t, svc.Delete(&model.User{ID: "u1"}, "f1"))

	assert.Equal(t, []string{"a1", "t1", "w1"}, enq.keys)
	assert.Equal(t, []string{"a1", "t1", "w1"}, local.deleted, "the local copy of a migrated file stayed")
	assert.Empty(t, repo.Files)
}

// TestDelete_LocalFileDuringMigrationAlsoDeletesTheCopy covers a local row
// deleted while the migration runs: the migration may have copied it after
// the delete read the row, so the delete also removes the object storage
// copy. While no migration runs, it does not (as upstream).
func TestDelete_LocalFileDuringMigrationAlsoDeletesTheCopy(t *testing.T) {
	for _, running := range []bool{true, false} {
		f := objectStoredFile()
		f.StoredInternal = true
		svc, storage, enq, _ := svcWithEnqueuer(t, f)
		svc.SetStorageMigrationRunning(func() bool { return running })

		require.NoError(t, svc.Delete(&model.User{ID: "u1"}, "f1"))

		assert.Equal(t, []string{"a1", "t1", "w1"}, storage.deleted, "running=%v", running)
		if running {
			assert.Equal(t, []string{"a1", "t1", "w1"}, enq.keys)
		} else {
			assert.Empty(t, enq.keys)
		}
	}
}

// failingDeleteStorage fails every delete.
type failingDeleteStorage struct{ *drive.LocalStorage }

func (failingDeleteStorage) Delete(string) error { return errors.New("disk error") }

// TestDelete_FailedLocalDeleteKeepsTheCopy covers a local row whose delete
// fails while the migration runs: the row stays, so the object storage copy
// the migration may make next must not be deleted by a queued job.
func TestDelete_FailedLocalDeleteKeepsTheCopy(t *testing.T) {
	f := objectStoredFile()
	f.StoredInternal = true
	repo := testutil.NewMockDriveFileRepository()
	repo.Files[f.ID] = f
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(),
		failingDeleteStorage{drive.NewLocalStorage(t.TempDir(), "")}, idGen, nil, nil, nil, nil)
	enq := &recordingObjectDeleteEnqueuer{}
	svc.SetObjectDeleteEnqueuer(enq)
	svc.SetStorageMigrationRunning(func() bool { return true })

	require.Error(t, svc.Delete(&model.User{ID: "u1"}, "f1"))
	assert.Empty(t, enq.keys)
	assert.Contains(t, repo.Files, "f1")
}

func TestHasMigratedLocalCopy(t *testing.T) {
	at := time.Now()
	for _, tt := range []struct {
		name string
		f    *model.DriveFile
		want bool
	}{
		{"nil", nil, false},
		{"migrated", &model.DriveFile{ObjectStorageMigratedAt: &at}, true},
		{"never migrated", &model.DriveFile{}, false},
		{"local", &model.DriveFile{StoredInternal: true, ObjectStorageMigratedAt: &at}, false},
		{"link", &model.DriveFile{IsLink: true, ObjectStorageMigratedAt: &at}, false},
	} {
		assert.Equal(t, tt.want, drive.HasMigratedLocalCopy(tt.f), tt.name)
	}
}

// TestDeleteAllByHost_FailedDeleteStillDeletesTheLocalCopy covers the purge
// of a host's files: it deletes the rows even when an object delete fails,
// so the local copy of a migrated file must go too, or nothing points at it.
func TestDeleteAllByHost_FailedDeleteStillDeletesTheLocalCopy(t *testing.T) {
	f := objectStoredFile()
	host := "remote.example"
	f.UserHost = &host
	at := time.Now()
	f.ObjectStorageMigratedAt = &at
	repo := testutil.NewMockDriveFileRepository()
	repo.Files[f.ID] = f
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(),
		failingDeleteStorage{drive.NewLocalStorage(t.TempDir(), "")}, idGen, nil, nil, nil, nil)
	local := &countingStorage{LocalStorage: drive.NewLocalStorage(t.TempDir(), "")}
	svc.SetLocalStorage(local)

	n, err := svc.DeleteAllByHost(host)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Equal(t, []string{"a1", "t1", "w1"}, local.deleted)
}

// DeleteFileObjects deletes the stored objects the way Delete does, and
// leaves the row for the caller (the delete-account job deletes the rows of
// a whole account at once).
func TestDeleteFileObjects_DeletesObjectsAndKeepsTheRow(t *testing.T) {
	svc, storage, enq, repo := svcWithEnqueuer(t, objectStoredFile())
	require.NoError(t, svc.DeleteFileObjects(repo.Files["f1"]))
	assert.Equal(t, []string{"a1", "t1", "w1"}, enq.keys)
	assert.Empty(t, storage.deleted)
	assert.Contains(t, repo.Files, "f1", "the row is the caller's to delete")

	local := objectStoredFile()
	local.StoredInternal = true
	svc, storage, enq, repo = svcWithEnqueuer(t, local)
	require.NoError(t, svc.DeleteFileObjects(repo.Files["f1"]))
	assert.Empty(t, enq.keys)
	assert.Equal(t, []string{"a1", "t1", "w1"}, storage.deleted)
	assert.Contains(t, repo.Files, "f1")
}

// When the primary object cannot be deleted, DeleteFileObjects still deletes
// the local copy of a migrated file: the caller deletes the row anyway, so
// nothing would point at the copy any more.
func TestDeleteFileObjects_FailedDeleteStillDeletesTheLocalCopy(t *testing.T) {
	f := objectStoredFile()
	at := time.Now()
	f.ObjectStorageMigratedAt = &at
	repo := testutil.NewMockDriveFileRepository()
	repo.Files[f.ID] = f
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(),
		failingDeleteStorage{drive.NewLocalStorage(t.TempDir(), "")}, idGen, nil, nil, nil, nil)
	local := &countingStorage{LocalStorage: drive.NewLocalStorage(t.TempDir(), "")}
	svc.SetLocalStorage(local)

	require.Error(t, svc.DeleteFileObjects(f))
	assert.Equal(t, []string{"a1", "t1", "w1"}, local.deleted)
}
