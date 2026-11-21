package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/elythia-network/elythia/internal/api/adminbackup"
	"github.com/elythia-network/elythia/internal/api/apierr"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/model"
)

// DriveStorageMigrator copies locally stored drive files to object storage
// (coredrive.StorageMigrator, #4180).
type DriveStorageMigrator interface {
	Start(ctx context.Context) error
	StartVerify(ctx context.Context) error
	StartPurge(ctx context.Context) error
	StartRestore(ctx context.Context) error
	StartCleanup(ctx context.Context) error
	Status(ctx context.Context) (coredrive.StorageMigrationStatus, error)
}

// SetDriveStorageMigrator wires the local-to-object-storage migration
// (#4180).
func (h *Handler) SetDriveStorageMigrator(m DriveStorageMigrator) {
	h.driveStorageMigration = m
}

// SetDriveStorageMigrationVerifier wires the strong authentication that
// admin/drive/object-storage-migration/purge-local requires (#4184). It is
// the verifier of admin/backup/*.
func (h *Handler) SetDriveStorageMigrationVerifier(v *strongauth.Verifier) {
	h.driveStorageMigrationReauth = v
}

const (
	idStorageMigrationNotConfigured = "1b01ee55-a494-424e-8ae9-4776b15cbc30"
	idStorageMigrationNothingToDo   = "eabd8ef9-4dda-4499-a3de-fc6fffa26861"
	idStorageMigrationRunning       = "5f833225-1667-48ac-bc86-10ca1dfd6817"
	idStorageMigrationNoQueue       = "5e53fbe0-6cfb-447a-9540-a6648135f4a3"
	idStorageMigrationNoMigrated    = "81c3d7e1-144c-46fa-850d-2a5b8a9c00ef"
	idStorageMigrationNoPurgeable   = "64fc1936-b77f-43fc-b9d6-d72cf180eed7"
	idStorageMigrationNoCopies      = "dde90589-c70e-4790-9f99-68fd6e78cc09"
)

// DriveStorageMigrationStart handles POST
// /api/admin/drive/object-storage-migration/start.
//
// **Elythia 独自 endpoint** (#4180)。ローカルに置いたファイルを、オブジェクト
// ストレージへ写すジョブを積んで、すぐに 204 を返す。進み具合は status で読む。
func (h *Handler) DriveStorageMigrationStart(c *echo.Context) error {
	if h.driveStorageMigration == nil {
		return apierr.JSONInternalError(c)
	}
	return h.driveStorageMigrationStarted(c, h.driveStorageMigration.Start(c.Request().Context()),
		"admin/drive/object-storage-migration/start", moderationlog.LogMigrateDriveFilesToObjectStorage)
}

// DriveStorageMigrationVerify handles POST
// /api/admin/drive/object-storage-migration/verify.
//
// **Elythia 独自 endpoint** (#4181)。オブジェクトストレージへ写したファイルを読み、
// 大きさと md5 が記録と一致するかを確かめるジョブを積んで、すぐに 204 を返す。
// 結果は status で読む。移す処理と同時には走らない。
func (h *Handler) DriveStorageMigrationVerify(c *echo.Context) error {
	if h.driveStorageMigration == nil {
		return apierr.JSONInternalError(c)
	}
	return h.driveStorageMigrationStarted(c, h.driveStorageMigration.StartVerify(c.Request().Context()),
		"admin/drive/object-storage-migration/verify", moderationlog.LogVerifyMigratedDriveFiles)
}

// DriveStorageMigrationPurgeLocal handles POST
// /api/admin/drive/object-storage-migration/purge-local.
//
// **Elythia 独自 endpoint** (#4182)。確かめたファイルについて、消す直前にもう一度
// 確かめてから、ローカルの実体を消すジョブを積んで、すぐに 204 を返す。進み具合は
// status で読む。移す処理・確かめる処理と同時には走らない。
//
// 取り消せない操作なので、DB のバックアップ (admin/backup/*) と同じ再認証を課す
// (#4184)。再認証は route の middleware でなく、ここ (引数の検証の後) で行う。
// 前に置くと、形の誤った本文でも TOTP のコードを使い切ってから 400 になる (#3878)。
func (h *Handler) DriveStorageMigrationPurgeLocal(c *echo.Context) error {
	if h.driveStorageMigration == nil || h.driveStorageMigrationReauth == nil {
		// 再認証を配線し忘れたら、通さずに閉じる。
		return apierr.JSONInternalError(c)
	}
	if ok, err := adminbackup.Reauthenticate(h.driveStorageMigrationReauth, c); !ok {
		return err
	}
	return h.driveStorageMigrationStarted(c, h.driveStorageMigration.StartPurge(c.Request().Context()),
		"admin/drive/object-storage-migration/purge-local", moderationlog.LogPurgeLocalDriveFiles)
}

// DriveStorageMigrationRestore handles POST
// /api/admin/drive/object-storage-migration/restore.
//
// **Elythia 独自 endpoint** (#4183)。移す処理で移したファイルを、オブジェクト
// ストレージからローカルへ戻すジョブを積んで、すぐに 204 を返す。初めから
// オブジェクトストレージへアップロードしたファイルは戻さない。進み具合は status で
// 読む。他の処理と同時には走らない。
func (h *Handler) DriveStorageMigrationRestore(c *echo.Context) error {
	if h.driveStorageMigration == nil {
		return apierr.JSONInternalError(c)
	}
	return h.driveStorageMigrationStarted(c, h.driveStorageMigration.StartRestore(c.Request().Context()),
		"admin/drive/object-storage-migration/restore", moderationlog.LogRestoreMigratedDriveFiles)
}

// DriveStorageMigrationCleanupCopies handles POST
// /api/admin/drive/object-storage-migration/cleanup-copies.
//
// **Elythia 独自 endpoint** (#4197)。ローカルへ戻したファイル (#4183) がオブジェクト
// ストレージに残した写しを消すジョブを積んで、すぐに 204 を返す。消すのは戻したときに
// 記録した写しだけで、初めからオブジェクトストレージへアップロードしたファイルと、戻せ
// なかったファイルの実体には触れない。進み具合は status で読む。他の処理と同時には
// 走らない。
//
// ローカルを消す口と違い、再認証は求めない。消すのはローカルへ戻した後の写しで、
// 実体はローカルにある。
func (h *Handler) DriveStorageMigrationCleanupCopies(c *echo.Context) error {
	if h.driveStorageMigration == nil {
		return apierr.JSONInternalError(c)
	}
	return h.driveStorageMigrationStarted(c, h.driveStorageMigration.StartCleanup(c.Request().Context()),
		"admin/drive/object-storage-migration/cleanup-copies", moderationlog.LogCleanupRestoredDriveFileCopies)
}

// driveStorageMigrationStarted writes the response of starting a run.
func (h *Handler) driveStorageMigrationStarted(c *echo.Context, err error, endpoint string, log moderationlog.LogType) error {
	switch {
	case err == nil:
		// 始めたことだけを残す (#4184)。断ったときは何も変えていないので残さない。
		h.logModeration(c, log, map[string]any{})
		return c.NoContent(http.StatusNoContent)
	case errors.Is(err, coredrive.ErrStorageMigrationNotConfigured):
		return c.JSON(http.StatusBadRequest, apierr.Error("OBJECT_STORAGE_NOT_CONFIGURED",
			"Object storage is not configured.", idStorageMigrationNotConfigured))
	case errors.Is(err, coredrive.ErrStorageMigrationNothingToDo):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_LOCAL_FILES",
			"No drive file is stored locally.", idStorageMigrationNothingToDo))
	case errors.Is(err, coredrive.ErrStorageMigrationNothingToVerify),
		errors.Is(err, coredrive.ErrStorageMigrationNothingToRestore):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_MIGRATED_FILES",
			"No drive file has been migrated to object storage.", idStorageMigrationNoMigrated))
	case errors.Is(err, coredrive.ErrStorageMigrationNothingToPurge):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_PURGEABLE_FILES",
			"No verified drive file keeps its local copy.", idStorageMigrationNoPurgeable))
	case errors.Is(err, coredrive.ErrStorageMigrationNothingToCleanUp):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_RESTORED_COPIES",
			"No copy left in object storage by moving drive files back is recorded.", idStorageMigrationNoCopies))
	case errors.Is(err, coredrive.ErrStorageMigrationRunning):
		return c.JSON(http.StatusConflict, apierr.Error("STORAGE_MIGRATION_RUNNING",
			"A storage migration is already running.", idStorageMigrationRunning))
	case errors.Is(err, coredrive.ErrStorageMigrationNoQueue):
		return c.JSON(http.StatusBadRequest, apierr.Error("JOB_QUEUE_UNAVAILABLE",
			"The job queue is not available.", idStorageMigrationNoQueue))
	default:
		slog.Error(endpoint+" failed", "err", err)
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
func (h *Handler) DriveStorageMigrationStatus(c *echo.Context) error {
	if h.driveStorageMigration == nil {
		return apierr.JSONInternalError(c)
	}
	st, err := h.driveStorageMigration.Status(c.Request().Context())
	if err != nil {
		slog.Error("admin/drive/object-storage-migration/status failed", "err", err)
		return apierr.JSONInternalError(c)
	}
	running := any(nil)
	if st.Running != "" {
		running = st.Running
	}
	return c.JSON(http.StatusOK, map[string]any{
		"objectStorageEnabled": st.ObjectStorageEnabled,
		"running":              running,
		"localFiles":           st.Local,
		"migratedFiles":        st.Migrated,
		"failedFiles":          st.Failed,
		"failures":             packStorageMigrationFailures(st.Failures),
		"verifiedFiles":        st.Verified,
		"verifyFailedFiles":    st.VerifyFailed,
		"verifyFailures":       packStorageMigrationFailures(st.VerifyFailures),
		"purgeableFiles":       st.Purgeable,
		"localDeletedFiles":    st.LocalDeleted,
		"purgeFailedFiles":     st.PurgeFailed,
		"purgeFailures":        packStorageMigrationFailures(st.PurgeFailures),
		"restoreFailedFiles":   st.RestoreFailed,
		"restoreFailures":      packStorageMigrationFailures(st.RestoreFailures),
		"restoredCopies":       st.RestoredCopies,
		"cleanupFailedCopies":  st.CleanupFailed,
		"cleanupFailures":      packRestoredCopyFailures(st.CleanupFailures),
	})
}

// driveStorageMigrationCopyFailure is a copy the cleanup could not delete in
// the status (#4197). The file of fileId may have been deleted.
type driveStorageMigrationCopyFailure struct {
	Key      string `json:"key"`
	FileID   string `json:"fileId"`
	Error    string `json:"error"`
	FailedAt string `json:"failedAt"`
}

func packRestoredCopyFailures(in []model.DriveObjectStorageRestoredCopy) []driveStorageMigrationCopyFailure {
	out := make([]driveStorageMigrationCopyFailure, 0, len(in))
	for _, c := range in {
		if c.Error == nil || c.FailedAt == nil {
			continue
		}
		out = append(out, driveStorageMigrationCopyFailure{
			Key: c.Key, FileID: c.FileID, Error: *c.Error, FailedAt: c.FailedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	return out
}

func packStorageMigrationFailures(in []model.DriveObjectStorageMigrationFailure) []driveStorageMigrationFailure {
	out := make([]driveStorageMigrationFailure, 0, len(in))
	for _, f := range in {
		out = append(out, driveStorageMigrationFailure{
			FileID: f.FileID, Error: f.Error, FailedAt: f.FailedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	return out
}
