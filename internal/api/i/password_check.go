package i

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/passwordguard"
	"golang.org/x/crypto/bcrypt"
)

// SetPasswordFailureGuard attaches the per-account budget for wrong current
// passwords. nil disables it (tests only; router.go always wires it).
func (h *Handler) SetPasswordFailureGuard(g passwordguard.Guard) {
	h.passwordGuard = g
}

// beginPasswordCheck reserves one current-password comparison for userID.
//
// It writes 429 RATE_LIMIT_EXCEEDED and returns ok=false when the account has
// used up its failure budget; the caller must return without comparing. The
// returned attempt stays counted as a failure unless the caller releases it
// (after a successful comparison, or when no comparison happened).
//
// **レート制限を route ごとの limiter に置かない理由。** limiter は権限検査
// より前に走り、成否に関係なく user bucket を消費する。token を持つだけの
// 第三者 (scope 不問。RequireSecure で 403 になるリクエストでも消費する) が
// 被害者の `i/regenerate-token` を使い切れると、token 漏洩時の唯一の対処を
// 攻撃者が止められる。ここでは**照合に失敗した回数だけ**を数える。
//
// 枠は (アカウント, 接続元の範囲) ごとと、アカウント全体の 2 段
// (passwordguard の doc)。native token を持つ攻撃者が被害者の操作を
// 止めるには、接続元の範囲を多数用意してアカウント全体の枠を使い切る必要がある。
//
// **予約と取り消しはリクエストの取り消しに引きずられない ctx で行う。**
// クライアントが切断すると ctx が取り消されて予約が失敗し、下の fail-open で
// 数えられない照合が走る。取り消しも同じで、成功直後の切断が失敗として残る。
func (h *Handler) beginPasswordCheck(c echo.Context, userID string) (passwordguard.Attempt, bool) {
	if h.passwordGuard == nil {
		return passwordguard.NoopAttempt(), true
	}
	a, err := h.passwordGuard.Begin(context.WithoutCancel(c.Request().Context()), userID, c.RealIP())
	if err == nil {
		return a, true
	}
	var le *passwordguard.LimitedError
	if errors.As(err, &le) {
		retry := int64((le.RetryAfter + time.Second - 1) / time.Second)
		c.Response().Header().Set("Retry-After", strconv.FormatInt(retry, 10))
		_ = c.JSON(http.StatusTooManyRequests, apierr.RateLimitExceeded())
		return nil, false
	}
	// store の障害で利用者のパスワード操作を止めない。limiter と同じ fail-open。
	slog.Warn("password failure guard unavailable", "userId", userID, "err", err)
	return passwordguard.NoopAttempt(), true
}

// comparePassword checks plain against the stored bcrypt hash under the
// per-account failure budget.
//
// It writes the response (429, or 400 INCORRECT_PASSWORD with incorrectID)
// and returns false when the check did not pass.
func (h *Handler) comparePassword(c echo.Context, userID, stored, plain, incorrectID string) bool {
	attempt, ok := h.beginPasswordCheck(c, userID)
	if !ok {
		return false
	}
	if err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(plain)); err != nil {
		_ = c.JSON(http.StatusBadRequest, apierr.Error("INCORRECT_PASSWORD", "Incorrect password.", incorrectID))
		return false
	}
	attempt.Release(context.WithoutCancel(c.Request().Context()))
	return true
}
