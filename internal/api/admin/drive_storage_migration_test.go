package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
)

type stubDriveStorageMigrator struct {
	startErr      error
	starts        int
	verifyErr     error
	verifyStarts  int
	purgeErr      error
	purgeStarts   int
	restoreErr    error
	restoreStarts int
	cleanupErr    error
	cleanupStarts int
	status        coredrive.StorageMigrationStatus
	statusErr     error
}

func (s *stubDriveStorageMigrator) StartCleanup(context.Context) error {
	s.cleanupStarts++
	return s.cleanupErr
}

func (s *stubDriveStorageMigrator) StartRestore(context.Context) error {
	s.restoreStarts++
	return s.restoreErr
}

func (s *stubDriveStorageMigrator) StartPurge(context.Context) error {
	s.purgeStarts++
	return s.purgeErr
}

func (s *stubDriveStorageMigrator) StartVerify(context.Context) error {
	s.verifyStarts++
	return s.verifyErr
}

func (s *stubDriveStorageMigrator) Start(context.Context) error {
	s.starts++
	return s.startErr
}

func (s *stubDriveStorageMigrator) Status(context.Context) (coredrive.StorageMigrationStatus, error) {
	return s.status, s.statusErr
}

func TestDriveStorageMigrationStart(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveStorageMigrator{}
	h.SetDriveStorageMigrator(stub)
	rec := doPost(h.DriveStorageMigrationStart, `{}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, stub.starts)

	for _, tt := range []struct {
		err    error
		status int
		code   string
		id     string
	}{
		{coredrive.ErrStorageMigrationNotConfigured, http.StatusBadRequest, "OBJECT_STORAGE_NOT_CONFIGURED", "1b01ee55-a494-424e-8ae9-4776b15cbc30"},
		{coredrive.ErrStorageMigrationNothingToDo, http.StatusBadRequest, "NO_LOCAL_FILES", "eabd8ef9-4dda-4499-a3de-fc6fffa26861"},
		{coredrive.ErrStorageMigrationRunning, http.StatusConflict, "STORAGE_MIGRATION_RUNNING", "5f833225-1667-48ac-bc86-10ca1dfd6817"},
		{coredrive.ErrStorageMigrationNoQueue, http.StatusBadRequest, "JOB_QUEUE_UNAVAILABLE", "5e53fbe0-6cfb-447a-9540-a6648135f4a3"},
		{fmt.Errorf("wrapped: %w", errors.New("redis down")), http.StatusInternalServerError, "INTERNAL_ERROR", ""},
	} {
		t.Run(tt.code, func(t *testing.T) {
			stub.startErr = tt.err
			rec := doPost(h.DriveStorageMigrationStart, `{}`, adminUser)
			assert.Equal(t, tt.status, rec.Code)
			var env struct {
				Error struct {
					Code string `json:"code"`
					ID   string `json:"id"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, tt.code, env.Error.Code)
			// エラーの id はクライアントが見分けに使うので、書き換えたら落とす。
			if tt.id != "" {
				assert.Equal(t, tt.id, env.Error.ID)
			} else {
				assert.NotEmpty(t, env.Error.ID)
			}
		})
	}
}

func TestDriveStorageMigrationVerify(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveStorageMigrator{}
	h.SetDriveStorageMigrator(stub)
	rec := doPost(h.DriveStorageMigrationVerify, `{}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, stub.verifyStarts)
	assert.Zero(t, stub.starts, "verify started a migration")

	for _, tt := range []struct {
		err    error
		status int
		code   string
		id     string
	}{
		{coredrive.ErrStorageMigrationNotConfigured, http.StatusBadRequest, "OBJECT_STORAGE_NOT_CONFIGURED", "1b01ee55-a494-424e-8ae9-4776b15cbc30"},
		{coredrive.ErrStorageMigrationNothingToVerify, http.StatusBadRequest, "NO_MIGRATED_FILES", "81c3d7e1-144c-46fa-850d-2a5b8a9c00ef"},
		{coredrive.ErrStorageMigrationRunning, http.StatusConflict, "STORAGE_MIGRATION_RUNNING", "5f833225-1667-48ac-bc86-10ca1dfd6817"},
		{coredrive.ErrStorageMigrationNoQueue, http.StatusBadRequest, "JOB_QUEUE_UNAVAILABLE", "5e53fbe0-6cfb-447a-9540-a6648135f4a3"},
		{errors.New("redis down"), http.StatusInternalServerError, "INTERNAL_ERROR", ""},
	} {
		t.Run(tt.code, func(t *testing.T) {
			stub.verifyErr = tt.err
			rec := doPost(h.DriveStorageMigrationVerify, `{}`, adminUser)
			assert.Equal(t, tt.status, rec.Code)
			var env struct {
				Error struct {
					Code string `json:"code"`
					ID   string `json:"id"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, tt.code, env.Error.Code)
			if tt.id != "" {
				assert.Equal(t, tt.id, env.Error.ID)
			} else {
				assert.NotEmpty(t, env.Error.ID)
			}
		})
	}
}

func TestDriveStorageMigrationPurgeLocal(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveStorageMigrator{}
	h.SetDriveStorageMigrator(stub)
	v, code := purgeVerifier(t)
	h.SetDriveStorageMigrationVerifier(v)
	rec := doNativePost(h.DriveStorageMigrationPurgeLocal, purgeBody(purgePassword, code()), purgeAdminID)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, stub.purgeStarts)
	assert.Zero(t, stub.starts)
	assert.Zero(t, stub.verifyStarts)

	for _, tt := range []struct {
		err    error
		status int
		code   string
		id     string
	}{
		{coredrive.ErrStorageMigrationNotConfigured, http.StatusBadRequest, "OBJECT_STORAGE_NOT_CONFIGURED", "1b01ee55-a494-424e-8ae9-4776b15cbc30"},
		{coredrive.ErrStorageMigrationNothingToPurge, http.StatusBadRequest, "NO_PURGEABLE_FILES", "64fc1936-b77f-43fc-b9d6-d72cf180eed7"},
		{coredrive.ErrStorageMigrationRunning, http.StatusConflict, "STORAGE_MIGRATION_RUNNING", "5f833225-1667-48ac-bc86-10ca1dfd6817"},
		{coredrive.ErrStorageMigrationNoQueue, http.StatusBadRequest, "JOB_QUEUE_UNAVAILABLE", "5e53fbe0-6cfb-447a-9540-a6648135f4a3"},
		{errors.New("redis down"), http.StatusInternalServerError, "INTERNAL_ERROR", ""},
	} {
		t.Run(tt.code, func(t *testing.T) {
			stub.purgeErr = tt.err
			rec := doNativePost(h.DriveStorageMigrationPurgeLocal, purgeBody(purgePassword, code()), purgeAdminID)
			assert.Equal(t, tt.status, rec.Code)
			var env struct {
				Error struct {
					Code string `json:"code"`
					ID   string `json:"id"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, tt.code, env.Error.Code)
			if tt.id != "" {
				assert.Equal(t, tt.id, env.Error.ID)
			} else {
				assert.NotEmpty(t, env.Error.ID)
			}
		})
	}
}

func TestDriveStorageMigrationRestore(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveStorageMigrator{}
	h.SetDriveStorageMigrator(stub)
	rec := doPost(h.DriveStorageMigrationRestore, `{}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, stub.restoreStarts)
	assert.Zero(t, stub.starts)
	assert.Zero(t, stub.purgeStarts)

	for _, tt := range []struct {
		err    error
		status int
		code   string
		id     string
	}{
		{coredrive.ErrStorageMigrationNotConfigured, http.StatusBadRequest, "OBJECT_STORAGE_NOT_CONFIGURED", "1b01ee55-a494-424e-8ae9-4776b15cbc30"},
		{coredrive.ErrStorageMigrationNothingToRestore, http.StatusBadRequest, "NO_MIGRATED_FILES", "81c3d7e1-144c-46fa-850d-2a5b8a9c00ef"},
		{coredrive.ErrStorageMigrationRunning, http.StatusConflict, "STORAGE_MIGRATION_RUNNING", "5f833225-1667-48ac-bc86-10ca1dfd6817"},
		{coredrive.ErrStorageMigrationNoQueue, http.StatusBadRequest, "JOB_QUEUE_UNAVAILABLE", "5e53fbe0-6cfb-447a-9540-a6648135f4a3"},
		{errors.New("redis down"), http.StatusInternalServerError, "INTERNAL_ERROR", ""},
	} {
		t.Run(tt.code, func(t *testing.T) {
			stub.restoreErr = tt.err
			rec := doPost(h.DriveStorageMigrationRestore, `{}`, adminUser)
			assert.Equal(t, tt.status, rec.Code)
			var env struct {
				Error struct {
					Code string `json:"code"`
					ID   string `json:"id"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, tt.code, env.Error.Code)
			if tt.id != "" {
				assert.Equal(t, tt.id, env.Error.ID)
			} else {
				assert.NotEmpty(t, env.Error.ID)
			}
		})
	}
}

func TestDriveStorageMigrationCleanupCopies(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDriveStorageMigrator{}
	h.SetDriveStorageMigrator(stub)
	repo := attachModLog(t, h)
	rec := doPost(h.DriveStorageMigrationCleanupCopies, `{}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, stub.cleanupStarts)
	assert.Zero(t, stub.restoreStarts)
	assert.Zero(t, stub.purgeStarts)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "cleanupRestoredDriveFileCopies", repo.Snapshot()[0].Type)

	for _, tt := range []struct {
		err    error
		status int
		code   string
		id     string
	}{
		{coredrive.ErrStorageMigrationNotConfigured, http.StatusBadRequest, "OBJECT_STORAGE_NOT_CONFIGURED", "1b01ee55-a494-424e-8ae9-4776b15cbc30"},
		{coredrive.ErrStorageMigrationNothingToCleanUp, http.StatusBadRequest, "NO_RESTORED_COPIES", "dde90589-c70e-4790-9f99-68fd6e78cc09"},
		{coredrive.ErrStorageMigrationRunning, http.StatusConflict, "STORAGE_MIGRATION_RUNNING", "5f833225-1667-48ac-bc86-10ca1dfd6817"},
		{coredrive.ErrStorageMigrationNoQueue, http.StatusBadRequest, "JOB_QUEUE_UNAVAILABLE", "5e53fbe0-6cfb-447a-9540-a6648135f4a3"},
		{errors.New("redis down"), http.StatusInternalServerError, "INTERNAL_ERROR", ""},
	} {
		t.Run(tt.code, func(t *testing.T) {
			stub.cleanupErr = tt.err
			rec := doPost(h.DriveStorageMigrationCleanupCopies, `{}`, adminUser)
			assert.Equal(t, tt.status, rec.Code)
			var env struct {
				Error struct {
					Code string `json:"code"`
					ID   string `json:"id"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, tt.code, env.Error.Code)
			if tt.id != "" {
				assert.Equal(t, tt.id, env.Error.ID)
			} else {
				assert.NotEmpty(t, env.Error.ID)
			}
		})
	}
	// 断ったときはログに残さない。
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, repo.Snapshot(), 1)
}

func TestDriveStorageMigrationStatus(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	at := time.Date(2026, 11, 15, 1, 2, 3, 400_000_000, time.FixedZone("JST", 9*3600))
	cleanupErr := "settings changed"
	stub := &stubDriveStorageMigrator{status: coredrive.StorageMigrationStatus{
		ObjectStorageEnabled: true, Running: coredrive.StorageMigrationOperationVerify, Local: 5, Migrated: 7, Failed: 1,
		Failures:     []model.DriveObjectStorageMigrationFailure{{FileID: "f1", Operation: "migrate", Error: "boom", FailedAt: at}},
		Verified:     4,
		VerifyFailed: 2,
		VerifyFailures: []model.DriveObjectStorageMigrationFailure{
			{FileID: "f2", Operation: "verify", Error: "md5", FailedAt: at},
			{FileID: "f3", Operation: "verify", Error: "gone", FailedAt: at},
		},
		Purgeable:       3,
		LocalDeleted:    6,
		PurgeFailed:     1,
		PurgeFailures:   []model.DriveObjectStorageMigrationFailure{{FileID: "f4", Operation: "purge", Error: "kept", FailedAt: at}},
		RestoreFailed:   2,
		RestoreFailures: []model.DriveObjectStorageMigrationFailure{{FileID: "f5", Operation: "restore", Error: "neither", FailedAt: at}},
		RestoredCopies:  9,
		CleanupFailed:   1,
		CleanupFailures: []model.DriveObjectStorageRestoredCopy{
			{Key: "ak-f6", FileID: "f6", Location: "bucket/ak-f6", RestoredAt: at, Error: &cleanupErr, FailedAt: &at},
			// 失敗していない記録は一覧に出さない。
			{Key: "ak-f7", FileID: "f7", Location: "bucket/ak-f7", RestoredAt: at},
		},
	}}
	h.SetDriveStorageMigrator(stub)
	rec := doPost(h.DriveStorageMigrationStatus, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"objectStorageEnabled":true,"running":"verify","localFiles":5,"migratedFiles":7,"failedFiles":1,
		"failures":[{"fileId":"f1","error":"boom","failedAt":"2026-11-14T16:02:03.400Z"}],
		"verifiedFiles":4,"verifyFailedFiles":2,
		"verifyFailures":[{"fileId":"f2","error":"md5","failedAt":"2026-11-14T16:02:03.400Z"},{"fileId":"f3","error":"gone","failedAt":"2026-11-14T16:02:03.400Z"}],
		"purgeableFiles":3,"localDeletedFiles":6,"purgeFailedFiles":1,
		"purgeFailures":[{"fileId":"f4","error":"kept","failedAt":"2026-11-14T16:02:03.400Z"}],
		"restoreFailedFiles":2,"restoreFailures":[{"fileId":"f5","error":"neither","failedAt":"2026-11-14T16:02:03.400Z"}],
		"restoredCopies":9,"cleanupFailedCopies":1,
		"cleanupFailures":[{"key":"ak-f6","fileId":"f6","error":"settings changed","failedAt":"2026-11-14T16:02:03.400Z"}]}`, rec.Body.String())

	stub.status = coredrive.StorageMigrationStatus{Running: coredrive.StorageMigrationOperationCleanup}
	rec = doPost(h.DriveStorageMigrationStatus, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"objectStorageEnabled":false,"running":"cleanup","localFiles":0,"migratedFiles":0,"failedFiles":0,"failures":[],
		"verifiedFiles":0,"verifyFailedFiles":0,"verifyFailures":[],
		"purgeableFiles":0,"localDeletedFiles":0,"purgeFailedFiles":0,"purgeFailures":[],
		"restoreFailedFiles":0,"restoreFailures":[],"restoredCopies":0,"cleanupFailedCopies":0,"cleanupFailures":[]}`, rec.Body.String())

	stub.status = coredrive.StorageMigrationStatus{}
	rec = doPost(h.DriveStorageMigrationStatus, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"objectStorageEnabled":false,"running":null,"localFiles":0,"migratedFiles":0,"failedFiles":0,"failures":[],
		"verifiedFiles":0,"verifyFailedFiles":0,"verifyFailures":[],
		"purgeableFiles":0,"localDeletedFiles":0,"purgeFailedFiles":0,"purgeFailures":[],
		"restoreFailedFiles":0,"restoreFailures":[],"restoredCopies":0,"cleanupFailedCopies":0,"cleanupFailures":[]}`, rec.Body.String())

	stub.statusErr = errors.New("db down")
	rec = doPost(h.DriveStorageMigrationStatus, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestDriveStorageMigration_Unwired(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DriveStorageMigrationStart, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DriveStorageMigrationStatus, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DriveStorageMigrationVerify, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DriveStorageMigrationPurgeLocal, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DriveStorageMigrationRestore, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DriveStorageMigrationCleanupCopies, `{}`, adminUser).Code)
}
