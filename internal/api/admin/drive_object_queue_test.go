package admin_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubObjectStorageEnqueuer records what the handlers push onto the
// objectStorage queue (#2325).
type stubObjectStorageEnqueuer struct {
	keys       []string
	sweepCalls int
	deleteErr  error
	sweepErr   error
}

func (s *stubObjectStorageEnqueuer) EnqueueDeleteObjectStorageFile(key string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.keys = append(s.keys, key)
	return nil
}

func (s *stubObjectStorageEnqueuer) EnqueueCleanRemoteFiles() error {
	s.sweepCalls++
	return s.sweepErr
}

// clean-remote-files は job を 1 本積んで即返す。行の削除は worker が行うので
// リクエスト中には消えない (upstream の createCleanRemoteFilesJob と同じ)。
func TestDriveCleanRemoteFiles_EnqueuesSweepJob(t *testing.T) {
	host := "remote.example"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "cached_remote", IsLink: false, UserHost: &host},
	)
	enq := &stubObjectStorageEnqueuer{}
	h.SetObjectStorageEnqueuer(enq)
	h.SetStorageDeleter(&stubStorageDeleter{})

	rec := doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, enq.sweepCalls)
	assert.Contains(t, repo.Files, "cached_remote", "削除は worker が行うのでこの時点では残る")
}

func TestDriveCleanRemoteFiles_EnqueueFailureReturns500(t *testing.T) {
	h, _ := setupDriveFileHandler(t)
	h.SetObjectStorageEnqueuer(&stubObjectStorageEnqueuer{sweepErr: errors.New("redis down")})

	rec := doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// cleanup は upstream 同様ファイルごとに deleteFile job を積む。
func TestDriveCleanup_EnqueuesObjectDeletes(t *testing.T) {
	ak1, tk1, ak2 := "ak1", "tk1", "ak2"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "o1", UserID: nil, AccessKey: &ak1, ThumbnailAccessKey: &tk1},
		&model.DriveFile{ID: "o2", UserID: nil, AccessKey: &ak2},
	)
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)
	enq := &stubObjectStorageEnqueuer{}
	h.SetObjectStorageEnqueuer(enq)

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.ElementsMatch(t, []string{"ak1", "tk1", "ak2"}, enq.keys)
	assert.Empty(t, sd.deleted, "object storage の実体は worker が消す")
	assert.Empty(t, repo.Files)
}

// storedInternal=true な行はローカル FS にあるので queue を経由せず同期削除する。
func TestDriveCleanup_StoredInternalStillDeletedInline(t *testing.T) {
	akLocal := "local1"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "o1", UserID: nil, AccessKey: &akLocal, StoredInternal: true},
	)
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)
	local := &stubStorageDeleter{}
	h.SetLocalStorageDeleter(local)
	enq := &stubObjectStorageEnqueuer{}
	h.SetObjectStorageEnqueuer(enq)

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, enq.keys)
	assert.Equal(t, []string{"local1"}, local.deleted)
}

// queue が死んでいても実体は消す (取りこぼすと追跡不能な孤児になる)。
func TestDriveCleanup_FallsBackToInlineWhenEnqueueFails(t *testing.T) {
	ak1 := "ak1"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "o1", UserID: nil, AccessKey: &ak1},
	)
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)
	h.SetObjectStorageEnqueuer(&stubObjectStorageEnqueuer{deleteErr: errors.New("redis down")})

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"ak1"}, sd.deleted)
}
