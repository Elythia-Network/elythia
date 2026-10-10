package adminbackup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/backupadmin"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Error IDs of the backup operations. Elythia-only.
const (
	idNotConfigured        = "f7da5281-5dde-4827-843a-2fae3abae34c"
	idNoSuchGeneration     = "3c0b2940-b9a6-402a-ae46-d358cb796600"
	idGenerationIncomplete = "8694147d-9a44-4f2d-97a7-d7bfdf91c814"
	idServiceNotConfigured = "f0f40daa-924e-4de2-be3d-0906e74bde81"
	idServiceBusy          = "be699693-6cda-4b02-a93a-cd199afc3a84"
	idServiceError         = "82e34f17-dc0f-43c6-bfe6-436e12812de2"
)

// ModLogger records moderation logs. *moderationlog.Service implements it.
type ModLogger interface {
	Log(ctx context.Context, moderatorID string, t moderationlog.LogType, info map[string]any)
}

// Handler serves admin/backup/*.
type Handler struct {
	svc      *backupadmin.Service
	verifier *strongauth.Verifier
	modlog   ModLogger
}

// NewHandler returns a Handler. modlog may be nil only in tests.
func NewHandler(svc *backupadmin.Service, verifier *strongauth.Verifier, modlog ModLogger) *Handler {
	return &Handler{svc: svc, verifier: verifier, modlog: modlog}
}

// Guard returns the strong authentication middleware for every
// admin/backup/* route except ReauthChallenge.
func (h *Handler) Guard() echo.MiddlewareFunc { return Guard(h.verifier) }

// log records a moderation log. 操作が成立してから呼ぶ。
func (h *Handler) log(c echo.Context, t moderationlog.LogType, info map[string]any) {
	if h.modlog == nil {
		return
	}
	u := middleware.GetUser(c)
	if u == nil {
		return
	}
	h.modlog.Log(c.Request().Context(), u.ID, t, info)
}

// serviceError writes the response for an error of backupadmin.
func serviceError(c echo.Context, op string, err error) error {
	switch {
	case errors.Is(err, backupadmin.ErrNotConfigured):
		return c.JSON(http.StatusBadRequest, apierr.Error("BACKUP_NOT_CONFIGURED",
			"The backup storage is not configured.", idNotConfigured))
	case errors.Is(err, backupadmin.ErrInvalidID):
		return apierr.JSONInvalidParamClient(c, "id", "is not a generation id")
	case errors.Is(err, backupadmin.ErrNotFound):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_GENERATION",
			"No such backup generation.", idNoSuchGeneration))
	case errors.Is(err, backupadmin.ErrIncomplete):
		return c.JSON(http.StatusBadRequest, apierr.Error("GENERATION_INCOMPLETE",
			"The backup generation is incomplete.", idGenerationIncomplete))
	case errors.Is(err, backupadmin.ErrServiceNotConfigured):
		return c.JSON(http.StatusBadRequest, apierr.Error("BACKUP_SERVICE_NOT_CONFIGURED",
			"The backup service is not configured.", idServiceNotConfigured))
	case errors.Is(err, backupadmin.ErrServiceBusy):
		return c.JSON(http.StatusConflict, apierr.Error("BACKUP_SERVICE_BUSY",
			"The backup service is busy. Please try again later.", idServiceBusy))
	case errors.Is(err, backupadmin.ErrServiceFailed):
		slog.Error("admin/backup: backup service request failed", "op", op, "err", err)
		return c.JSON(http.StatusBadGateway, apierr.ErrorWithKind("BACKUP_SERVICE_ERROR",
			"The backup service could not handle the request.", idServiceError, apierr.KindServer))
	default:
		slog.Error("admin/backup: operation failed", "op", op, "err", err)
		return apierr.JSONInternalError(c)
	}
}

// idRequest is the body of the operations on one generation.
type idRequest struct {
	ID string `json:"id"`
}

func bindID(c echo.Context) (string, bool) {
	var req idRequest
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return "", false
	}
	return req.ID, true
}

// List handles POST /api/admin/backup/list.
func (h *Handler) List(c echo.Context) error {
	ov, err := h.svc.List(c.Request().Context())
	if err != nil {
		return serviceError(c, "list", err)
	}
	// 閲覧も記録する。一覧そのものに秘密は無いが、この画面に入れたこと自体を
	// 後から追えるようにする。
	h.log(c, moderationlog.LogListBackups, map[string]any{
		"generationCount": ov.Usage.GenerationCount,
		"totalBytes":      ov.Usage.TotalBytes,
	})
	return c.JSON(http.StatusOK, ov)
}

// Take handles POST /api/admin/backup/take.
func (h *Handler) Take(c echo.Context) error {
	job, err := h.svc.Take(c.Request().Context(), c.RealIP())
	if err != nil {
		return serviceError(c, "take", err)
	}
	h.log(c, moderationlog.LogTakeBackup, map[string]any{})
	return c.JSON(http.StatusOK, map[string]any{"accepted": true, "job": job})
}

// Verify handles POST /api/admin/backup/verify.
func (h *Handler) Verify(c echo.Context) error {
	id, ok := bindID(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	job, err := h.svc.Verify(c.Request().Context(), id, c.RealIP())
	if err != nil {
		return serviceError(c, "verify", err)
	}
	h.log(c, moderationlog.LogVerifyBackup, map[string]any{"generationId": id})
	return c.JSON(http.StatusOK, map[string]any{"accepted": true, "job": job})
}

// Delete handles POST /api/admin/backup/delete.
func (h *Handler) Delete(c echo.Context) error {
	id, ok := bindID(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	freed, err := h.svc.Delete(c.Request().Context(), id)
	if err != nil {
		if freed > 0 {
			// 途中まで消えている。meta.json は先に消しているので、残りは未完成の
			// 世代として一覧に出る。記録は残す。
			h.log(c, moderationlog.LogDeleteBackup, map[string]any{"generationId": id, "freedBytes": freed, "partial": true})
		}
		return serviceError(c, "delete", err)
	}
	h.log(c, moderationlog.LogDeleteBackup, map[string]any{"generationId": id, "freedBytes": freed})
	return c.JSON(http.StatusOK, map[string]any{"freedBytes": freed})
}

// Download handles POST /api/admin/backup/download.
func (h *Handler) Download(c echo.Context) error {
	id, ok := bindID(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	d, err := h.svc.Download(c.Request().Context(), id)
	if err != nil {
		return serviceError(c, "download", err)
	}
	h.log(c, moderationlog.LogDownloadBackup, map[string]any{
		"generationId": id,
		"via":          d.Via,
		"size":         d.Size,
		"encrypted":    d.Encrypted,
	})
	return c.JSON(http.StatusOK, d)
}

// ReauthChallenge handles POST /api/admin/backup/reauth-challenge. It returns
// the PublicKeyCredentialRequestOptions for a passkey re-authentication.
//
// 再認証の前段なので Guard は掛けない。代わりに、再認証以外の条件 (管理者・
// native token・2FA の登録) をここで見る。challenge を出すだけで、何も見せず
// 何も変えない。
func (h *Handler) ReauthChallenge(c echo.Context) error {
	assertion, err := h.verifier.BeginPasskey(c.Request().Context(), subject(c))
	if err != nil {
		return refusal(c, err)
	}
	return c.JSON(http.StatusOK, assertion.Response)
}

// ServeDownload handles GET /backup-download/:token, the server-side download
// of a directory storage. The token comes from Download.
func (h *Handler) ServeDownload(c echo.Context) error {
	res := c.Response()
	res.Header().Set("Cache-Control", "no-store")
	res.Header().Set("X-Content-Type-Options", "nosniff")
	res.Header().Set("Referrer-Policy", "no-referrer")
	g, rc, info, err := h.svc.Open(c.Request().Context(), c.Param("token"))
	if err != nil {
		if errors.Is(err, backupadmin.ErrTokenInvalid) || errors.Is(err, backupadmin.ErrNotFound) || errors.Is(err, backupadmin.ErrNotConfigured) {
			return c.String(http.StatusNotFound, "Not Found")
		}
		slog.Error("admin/backup: download failed", "err", err)
		return c.String(http.StatusInternalServerError, "Internal Server Error")
	}
	defer func() { _ = rc.Close() }()
	res.Header().Set("Content-Type", "application/octet-stream")
	res.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": g.FileName}))
	// ディレクトリの保存先はファイルを返すので、Range (途切れたダウンロードの
	// 再開) に応じられる。seek できない実装なら頭から流す。
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(res, c.Request(), g.FileName, info.ModTime, rs)
		return nil
	}
	return c.Stream(http.StatusOK, "application/octet-stream", rc)
}
