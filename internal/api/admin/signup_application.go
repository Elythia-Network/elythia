package admin

import (
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// SignupApplicationReviewer is the review-side surface of the signup
// application state machine (#2555). 循環依存を避けるため interface で受け取る。
// 実装は core/signupapplication.Service。
type SignupApplicationReviewer interface {
	List(filter string, limit, offset int) ([]*model.SignupApplication, error)
	Count(filter string) (int, error)
	Approve(applicationID, moderatorID string) error
	Reject(applicationID, moderatorID string) error
	ExpireStale() (int, error)
}

// SetSignupApplicationReviewer wires the signup application service (#2555).
// 未配線なら承認制の管理 endpoint は 503 を返す。
func (h *Handler) SetSignupApplicationReviewer(r SignupApplicationReviewer) {
	h.signupApplications = r
}

const (
	defaultSignupApplicationLimit = 30
	maxSignupApplicationLimit     = 100
)

// packSignupApplication converts a row into the admin list response shape.
//
// **クレームコードは出さない。** hash しか保存していないうえ、出せてしまうと
// 管理者が申請者になりすまして登録できる。
func packSignupApplication(a *model.SignupApplication) map[string]any {
	return map[string]any{
		"id":            a.ID,
		"status":        a.Status,
		"answers":       a.Answers,
		"createdAt":     a.CreatedAt,
		"updatedAt":     a.UpdatedAt,
		"expiresAt":     a.ExpiresAt,
		"processedById": a.ProcessedByID,
		"processedAt":   a.ProcessedAt,
		"usedById":      a.UsedByID,
	}
}

// SignupApplicationList handles POST /api/admin/signup-application/list.
//
// **mk-go 独自 endpoint** (#2555)。upstream に承認制が無いため対応物は無い。
// scope は `read:admin:invite-codes` を再利用する — 承認はメール確認の経路で
// registration_ticket の発行につながるので管轄が同じで、`internal/misc/permissions`
// は upstream misskey-js と完全一致させる契約があり mk-go 固有 scope を足せない
// (admin/federation/delivery-health が `read:admin:server-info` を再利用するのと
// 同じ判断)。
func (h *Handler) SignupApplicationList(c echo.Context) error {
	if h.signupApplications == nil {
		return c.JSON(http.StatusServiceUnavailable,
			apierr.Error("UNAVAILABLE", "Approval-based signup is not configured.", "0c5b7d0e-2b7e-4d0e-9f0e-6a3f4b1f5c21"))
	}
	var req struct {
		Filter string `json:"filter"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
	}
	// body 無しでも既定値で応答する (管理画面が引数なしで叩く)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultSignupApplicationLimit
	}
	if limit > maxSignupApplicationLimit {
		limit = maxSignupApplicationLimit
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	filter := req.Filter
	if filter == "" {
		filter = repository.SignupApplicationFilterAll
	}

	// **一覧の前に掃除する。** 期限切れの反映は申請者の参照時に行われるので、
	// 誰も見に来ていない申請は pending のまま残る。掃除せずに出すと、管理者に
	// 「審査待ちに見えるが承認できない」行が並ぶ。
	if _, err := h.signupApplications.ExpireStale(); err != nil {
		// 掃除に失敗しても一覧は出す (表示が実態より古くなるだけ)。
		c.Logger().Warnf("signup application: expire stale failed: %v", err)
	}

	rows, err := h.signupApplications.List(filter, limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError,
			apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	count, err := h.signupApplications.Count(filter)
	if err != nil {
		return c.JSON(http.StatusInternalServerError,
			apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	packed := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		packed = append(packed, packSignupApplication(r))
	}
	return c.JSON(http.StatusOK, map[string]any{
		"applications": packed,
		"count":        count,
	})
}

// SignupApplicationApprove handles POST /api/admin/signup-application/approve.
//
// 承認は状態を変えるだけで、registration_ticket はここでは発行しない (#2555)。
func (h *Handler) SignupApplicationApprove(c echo.Context) error {
	return h.reviewSignupApplication(c, func(id, moderatorID string) error {
		return h.signupApplications.Approve(id, moderatorID)
	})
}

// SignupApplicationReject handles POST /api/admin/signup-application/reject.
func (h *Handler) SignupApplicationReject(c echo.Context) error {
	return h.reviewSignupApplication(c, func(id, moderatorID string) error {
		return h.signupApplications.Reject(id, moderatorID)
	})
}

// reviewSignupApplication is shared by approve / reject: the parameter shape and
// the error mapping are identical, so they are not duplicated.
func (h *Handler) reviewSignupApplication(c echo.Context, act func(id, moderatorID string) error) error {
	if h.signupApplications == nil {
		return c.JSON(http.StatusServiceUnavailable,
			apierr.Error("UNAVAILABLE", "Approval-based signup is not configured.", "0c5b7d0e-2b7e-4d0e-9f0e-6a3f4b1f5c21"))
	}
	var req struct {
		ApplicationID string `json:"applicationId"`
	}
	if err := c.Bind(&req); err != nil || req.ApplicationID == "" {
		return c.JSON(http.StatusBadRequest,
			apierr.Error("INVALID_PARAM", "applicationId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	actor := middleware.GetUser(c)
	if actor == nil {
		// RequireModerator が保証する。ここに来るなら配線ミス。
		return c.JSON(http.StatusForbidden,
			apierr.Error("ACCESS_DENIED", "Access denied.", "8a3f4b1f-5c21-4d0e-9f0e-6a3f4b1f5c22"))
	}

	switch err := act(req.ApplicationID, actor.ID); {
	case err == nil:
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	case errors.Is(err, signupapplication.ErrNotFound):
		return c.JSON(http.StatusNotFound,
			apierr.Error("NO_SUCH_APPLICATION", "No such signup application.", "1f2c0f7a-9b0e-4d5e-8a3f-4b1f5c210c5b"))
	case errors.Is(err, signupapplication.ErrExpired):
		// 期限切れは「審査待ちではない」と一緒にしない。行は pending のまま
		// 残りうるので、まとめると管理者に伝わらない (#2555)。
		return c.JSON(http.StatusBadRequest,
			apierr.Error("APPLICATION_EXPIRED", "This signup application has expired.", "2b7e4d0e-9f0e-4a3f-8b1f-5c210c5b7d0e"))
	case errors.Is(err, signupapplication.ErrNotPending):
		return c.JSON(http.StatusBadRequest,
			apierr.Error("APPLICATION_NOT_PENDING", "This signup application is not awaiting review.", "4d0e9f0e-6a3f-4b1f-9c21-0c5b7d0e2b7e"))
	default:
		return c.JSON(http.StatusInternalServerError,
			apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
}
