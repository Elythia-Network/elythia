package sw

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/webpush"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// Handler handles Service Worker push notification endpoints.
type Handler struct {
	repo     repository.SwSubscriptionRepository
	metaRepo repository.MetaRepository
	idGen    id.Generator
}

// NewHandler creates a new SW handler.
func NewHandler(repo repository.SwSubscriptionRepository, metaRepo repository.MetaRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, metaRepo: metaRepo, idGen: idGen}
}

// Register handles POST /api/sw/register.
func (h *Handler) Register(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Endpoint        string `json:"endpoint"`
		Auth            string `json:"auth"`
		PublicKey       string `json:"publickey"`
		SendReadMessage bool   `json:"sendReadMessage"`
	}
	if err := c.Bind(&req); err != nil || req.Endpoint == "" || req.Auth == "" || req.PublicKey == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "endpoint, auth, and publickey are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// **列に入らない値はここで断る (#3025)。** 下の重複チェックは
	// `IsNotFound` を「重複ではない」と読んで新規登録へ落ちるので、通すと
	// INSERT が SQLSTATE 22021 で落ちて 500 になる。**書き込みも必ず失敗する値**
	// なので「見つからない」に丸めてはいけない数少ない形。
	if !colfit.Storable(req.Endpoint) || !colfit.Storable(req.Auth) || !colfit.Storable(req.PublicKey) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "endpoint, auth, and publickey must not contain an invalid character.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// 配送先として使える endpoint だけを受け付ける (本家 sw/register の
	// invalidEndpoint と同じ code / id / 400)。本家と同じく既存の購読の確認より
	// 前に断るので、不正な endpoint が already-subscribed で返ることもない。
	if !webpush.IsValidEndpoint(req.Endpoint) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_ENDPOINT", "Invalid push endpoint.", "4432adbe-17c0-4f9f-b43c-9ceb2f8910fe"))
	}

	var swPublicKey *string
	if m, err := h.metaRepo.Fetch(); err == nil {
		swPublicKey = m.SwPublicKey
	}

	// 既存サブスクリプションの確認は (userId, endpoint, auth, publickey) の 4-tuple
	// で行う (upstream register.ts findOneBy)。auth / publickey が rotate した
	// 再 subscribe は match しないため、下の新規登録に落ちて最新キーで insert される。
	// 2-tuple (userId, endpoint) match だと stale キーのまま already-subscribed を
	// 返して Web Push 配信が無言で壊れていた (#1775)。
	existing, err := h.repo.FindByUserEndpointAuthKey(user.ID, req.Endpoint, req.Auth, req.PublicKey)
	// **DB 障害で重複チェックを skip しない** (#2792)。`err == nil` だけを見ると、
	// 接続断のときに下の新規登録へ落ちる。`sw_subscription` に unique index は
	// 無いので**重複行が恒久的に残り、その端末へ web push が二重配信される**。
	if err != nil && !repository.IsNotFound(err) {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err == nil && existing != nil {
		return c.JSON(http.StatusOK, map[string]any{
			"state":           "already-subscribed",
			"key":             swPublicKey,
			"userId":          user.ID,
			"endpoint":        existing.Endpoint,
			"sendReadMessage": existing.SendReadMessage,
		})
	}

	// 新規登録
	sub := &model.SwSubscription{
		ID:              h.idGen.Generate(time.Now()),
		UserID:          user.ID,
		Endpoint:        req.Endpoint,
		Auth:            req.Auth,
		PublicKey:       req.PublicKey,
		SendReadMessage: req.SendReadMessage,
	}
	if err := h.repo.Create(sub); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"state":           "subscribed",
		"key":             swPublicKey,
		"userId":          user.ID,
		"endpoint":        req.Endpoint,
		"sendReadMessage": req.SendReadMessage,
	})
}

// ShowRegistration handles POST /api/sw/show-registration.
func (h *Handler) ShowRegistration(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := c.Bind(&req); err != nil || req.Endpoint == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "endpoint is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	sub, err := h.repo.FindByUserAndEndpoint(user.ID, req.Endpoint)
	if err != nil {
		// upstream Misskey TS は handler が null を return すると Endpoint base
		// が 204 No Content に変換する。mk-go は明示的に 200 + JSON null を
		// 返していたため drop-in 互換性が崩れていた (#918)。
		// gorm.ErrRecordNotFound は 204、それ以外 (DB connection error 等) は
		// 500 + slog で観測性確保 (#917 federation/show-instance と同 pattern)。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.NoContent(http.StatusNoContent)
		}
		slog.Error("sw/show-registration: FindByUserAndEndpoint failed",
			"userId", user.ID, "endpoint", req.Endpoint, "err", err)
		return apierr.JSONInternalError(c)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"userId":          sub.UserID,
		"endpoint":        sub.Endpoint,
		"sendReadMessage": sub.SendReadMessage,
	})
}

// UpdateRegistration handles POST /api/sw/update-registration.
func (h *Handler) UpdateRegistration(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Endpoint        string `json:"endpoint"`
		SendReadMessage *bool  `json:"sendReadMessage"`
	}
	if err := c.Bind(&req); err != nil || req.Endpoint == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "endpoint is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	sub, err := h.repo.FindByUserAndEndpoint(user.ID, req.Endpoint)
	if err != nil {
		// gorm.ErrRecordNotFound は 404 (= upstream / 旧実装と一致)、
		// それ以外 (DB connection error 等) は 500 + slog で観測性確保
		// (#917 / #918 と同 pattern)。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_REGISTRATION", "No such registration.", "b09d8066-8064-5613-efb6-0e963b21d012"))
		}
		slog.Error("sw/update-registration: FindByUserAndEndpoint failed",
			"userId", user.ID, "endpoint", req.Endpoint, "err", err)
		return apierr.JSONInternalError(c)
	}

	if req.SendReadMessage != nil {
		sub.SendReadMessage = *req.SendReadMessage
	}
	if err := h.repo.Update(sub); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"userId":          sub.UserID,
		"endpoint":        sub.Endpoint,
		"sendReadMessage": sub.SendReadMessage,
	})
}

// Unregister handles POST /api/sw/unregister.
func (h *Handler) Unregister(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := c.Bind(&req); err != nil || req.Endpoint == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "endpoint is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	if user != nil {
		_ = h.repo.DeleteByUserAndEndpoint(user.ID, req.Endpoint)
	} else {
		_ = h.repo.DeleteByEndpoint(req.Endpoint)
	}

	return c.NoContent(http.StatusNoContent)
}
