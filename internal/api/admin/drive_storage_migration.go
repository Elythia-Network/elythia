package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
)

// DriveStorageMigrator copies locally stored drive files to object storage
// (coredrive.StorageMigrator, #4180).
type DriveStorageMigrator interface {
	Start(ctx context.Context) error
	Status(ctx context.Context) (coredrive.StorageMigrationStatus, error)
}

// SetDriveStorageMigrator wires the local-to-object-storage migration
// (#4180).
func (h *Handler) SetDriveStorageMigrator(m DriveStorageMigrator) {
	h.driveStorageMigration = m
}

const (
	idStorageMigrationNotConfigured = "1b01ee55-a494-424e-8ae9-4776b15cbc30"
	idStorageMigrationNothingToDo   = "eabd8ef9-4dda-4499-a3de-fc6fffa26861"
	idStorageMigrationRunning       = "5f833225-1667-48ac-bc86-10ca1dfd6817"
	idStorageMigrationNoQueue       = "5e53fbe0-6cfb-447a-9540-a6648135f4a3"
)

// DriveStorageMigrationStart handles POST
// /api/admin/drive/object-storage-migration/start.
//
// **Elythia 独自 endpoint** (#4180)。ローカルに置いたファイルを、オブジェクト
// ストレージへ写すジョブを積んで、すぐに 204 を返す。進み具合は status で読む。
func (h *Handler) DriveStorageMigrationStart(c echo.Context) error {
	if h.driveStorageMigration == nil {
		return apierr.JSONInternalError(c)
	}
	err := h.driveStorageMigration.Start(c.Request().Context())
	switch {
	case err == nil:
		// モデレーションのログの種類は、画面と一緒に #4184 で足す。
		return c.NoContent(http.StatusNoContent)
	case errors.Is(err, coredrive.ErrStorageMigrationNotConfigured):
		return c.JSON(http.StatusBadRequest, apierr.Error("OBJECT_STORAGE_NOT_CONFIGURED",
			"Object storage is not configured.", idStorageMigrationNotConfigured))
	case errors.Is(err, coredrive.ErrStorageMigrationNothingToDo):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_LOCAL_FILES",
			"No drive file is stored locally.", idStorageMigrationNothingToDo))
	case errors.Is(err, coredrive.ErrStorageMigrationRunning):
		return c.JSON(http.StatusConflict, apierr.Error("STORAGE_MIGRATION_RUNNING",
			"A storage migration is already running.", idStorageMigrationRunning))
	case errors.Is(err, coredrive.ErrStorageMigrationNoQueue):
		return c.JSON(http.StatusBadRequest, apierr.Error("JOB_QUEUE_UNAVAILABLE",
			"The job queue is not available.", idStorageMigrationNoQueue))
	default:
		slog.Error("admin/drive/object-storage-migration/start failed", "err", err)
		return apierr.JSONInternalError(c)
	}
}

// driveStorageMigrationFailure is a failed file in the status.
type driveStorageMigrationFailure struct {
	FileID   string `json:"fileId"`
	Error    string `json:"error"`
	FailedAt string `json:"failedAt"`
}

// DriveStorageMigrationStatus handles POST
// /api/admin/drive/object-storage-migration/status (#4180).
func (h *Handler) DriveStorageMigrationStatus(c echo.Context) error {
	if h.driveStorageMigration == nil {
		return apierr.JSONInternalError(c)
	}
	st, err := h.driveStorageMigration.Status(c.Request().Context())
	if err != nil {
		slog.Error("admin/drive/object-storage-migration/status failed", "err", err)
		return apierr.JSONInternalError(c)
	}
	failures := make([]driveStorageMigrationFailure, 0, len(st.Failures))
	for _, f := range st.Failures {
		failures = append(failures, driveStorageMigrationFailure{
			FileID: f.FileID, Error: f.Error, FailedAt: f.FailedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	running := any(nil)
	if st.Running {
		running = "migrate"
	}
	return c.JSON(http.StatusOK, map[string]any{
		"objectStorageEnabled": st.ObjectStorageEnabled,
		"running":              running,
		"localFiles":           st.Local,
		"migratedFiles":        st.Migrated,
		"failedFiles":          st.Failed,
		"failures":             failures,
	})
}
