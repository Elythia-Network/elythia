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
	startErr  error
	starts    int
	status    coredrive.StorageMigrationStatus
	statusErr error
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

func TestDriveStorageMigrationStatus(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	at := time.Date(2026, 11, 15, 1, 2, 3, 400_000_000, time.FixedZone("JST", 9*3600))
	stub := &stubDriveStorageMigrator{status: coredrive.StorageMigrationStatus{
		ObjectStorageEnabled: true, Running: true, Local: 5, Migrated: 7, Failed: 1,
		Failures: []model.DriveObjectStorageMigrationFailure{{FileID: "f1", Operation: "migrate", Error: "boom", FailedAt: at}},
	}}
	h.SetDriveStorageMigrator(stub)
	rec := doPost(h.DriveStorageMigrationStatus, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"objectStorageEnabled":true,"running":"migrate","localFiles":5,"migratedFiles":7,"failedFiles":1,
		"failures":[{"fileId":"f1","error":"boom","failedAt":"2026-11-14T16:02:03.400Z"}]}`, rec.Body.String())

	stub.status = coredrive.StorageMigrationStatus{}
	rec = doPost(h.DriveStorageMigrationStatus, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"objectStorageEnabled":false,"running":null,"localFiles":0,"migratedFiles":0,"failedFiles":0,"failures":[]}`, rec.Body.String())

	stub.statusErr = errors.New("db down")
	rec = doPost(h.DriveStorageMigrationStatus, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestDriveStorageMigration_Unwired(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DriveStorageMigrationStart, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DriveStorageMigrationStatus, `{}`, adminUser).Code)
}
