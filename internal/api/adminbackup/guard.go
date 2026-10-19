// Package adminbackup serves the admin endpoints for the database backups
// (admin/backup/*, #3462). Every endpoint, including the listing, is guarded
// by strongauth: administrators only, native session tokens only, 2FA or a
// passkey registered, and a fresh re-authentication on every request.
package adminbackup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Error IDs of the strong authentication. Elythia-only.
const (
	idTwoFactorRequired  = "3efbd3f8-46f0-4f87-a139-82bbec2f1868"
	idPasswordNotSet     = "944a86b1-5e3b-48c9-b9d1-6a58e5bdc901"
	idReauthRequired     = "490bb357-092b-4298-9fe5-1418bcbd55f1"
	idReauthFailed       = "82808eb2-6622-4363-83ba-af99cc470bb3"
	idPasskeyUnavailable = "dd2aa4c6-6ea1-4564-92ae-44d68e617d43"
	idAuthUnavailable    = "a2e8e922-25bf-4ca2-999d-5e060ca6ea54"
	idCodeReused         = "d632df49-9d46-4c95-a1d7-73e7a98d2d97"
	// idNotAdministrator is the ID RequireAdmin returns (upstream requireAdmin).
	idNotAdministrator = "c3d38592-54c0-429d-be96-5636b0431a61"
)

// reauthBodyLimit caps the body the guard reads. パスキーの assertion を含めても
// 数 KB に収まる。
const reauthBodyLimit = 64 << 10

// reauthFields are the re-authentication fields of every admin/backup/*
// request body.
type reauthFields struct {
	Password   string          `json:"password"`
	Token      string          `json:"token"`
	Credential json.RawMessage `json:"credential"`
}

// subject builds the strongauth subject of the request.
func subject(c echo.Context) strongauth.Subject {
	return strongauth.Subject{
		User:           middleware.GetUser(c),
		PresentedToken: middleware.GetToken(c),
		ClientIP:       c.RealIP(),
	}
}

// Guard requires every strongauth condition, including the re-authentication
// sent in the body (`password` and either `token` or `credential`).
//
// ルートの登録 (internal/server の routes_admin.go) の RequireAdmin / RequireSecure と重なるが、ここでも全条件を見る。
// route の middleware を付け忘れても閉じたままにするためと、メンテナンス中に
// バックアップ用のサービスが同じ判定を使えるようにするため (#3463)。
func Guard(v *strongauth.Verifier) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			var body []byte
			if req.Body != nil {
				b, err := io.ReadAll(io.LimitReader(req.Body, reauthBodyLimit+1))
				if err != nil || len(b) > reauthBodyLimit {
					return apierr.JSONInvalidParam(c)
				}
				body = b
				// handler が同じ body を Bind できるよう戻す。
				req.Body = io.NopCloser(bytes.NewReader(body))
			}
			var f reauthFields
			if len(bytes.TrimSpace(body)) > 0 {
				// handler の Bind は Content-Type に従って読むので、JSON 以外で
				// 送られると再認証だけ通って (TOTP を消費して) から落ちる。読み方を
				// 揃えるため、JSON だけを受ける。
				if !middleware.IsJSONContentType(req.Header.Get(echo.HeaderContentType)) {
					return apierr.JSONInvalidParam(c)
				}
				if err := json.Unmarshal(body, &f); err != nil {
					return apierr.JSONInvalidParam(c)
				}
			}
			err := v.Verify(req.Context(), subject(c), strongauth.Reauth{
				Password:   f.Password,
				Token:      f.Token,
				Credential: f.Credential,
				Request:    req,
			})
			if err != nil {
				return refusal(c, err)
			}
			return next(c)
		}
	}
}

// refusal writes the response for a strongauth refusal.
func refusal(c echo.Context, err error) error {
	res := Refusal(err, middleware.IsSuspendedRequest(c))
	if res.RetryAfter != "" {
		c.Response().Header().Set("Retry-After", res.RetryAfter)
	}
	return c.JSON(res.Status, res.Body)
}

// RefusalResponse is the HTTP response for a strongauth refusal.
type RefusalResponse struct {
	Status int
	Body   any
	// RetryAfter is the Retry-After header value, or "".
	RetryAfter string
}

// Refusal returns the response for err from strongauth. suspended tells a
// suspended account from a missing credential.
//
// バックアップ用のサービス (#3463 (6c)) も同じ形で返す。本体は daemon の応答を
// そのまま利用者へ渡すので、管理画面から見て本体と daemon の区別が付かない。
func Refusal(err error, suspended bool) RefusalResponse {
	var se *strongauth.Error
	if !errors.As(err, &se) {
		slog.Error("admin/backup: unexpected auth error", "err", err)
		return RefusalResponse{Status: http.StatusInternalServerError, Body: apierr.InternalError()}
	}
	switch se.Reason {
	case strongauth.ReasonNoCredential:
		if suspended {
			return RefusalResponse{Status: http.StatusForbidden, Body: apierr.YourAccountSuspended()}
		}
		return RefusalResponse{Status: http.StatusUnauthorized, Body: apierr.CredentialRequired()}
	case strongauth.ReasonNotAdmin:
		return RefusalResponse{Status: http.StatusForbidden, Body: apierr.ErrorWithKind("ROLE_PERMISSION_DENIED",
			"You are not an administrator.", idNotAdministrator, apierr.KindPermission)}
	case strongauth.ReasonNotNativeToken:
		return RefusalResponse{Status: http.StatusForbidden, Body: apierr.ErrorWithKind("ACCESS_DENIED",
			"Access denied.", apierr.UUIDAccessDeniedSecure, apierr.KindClient)}
	case strongauth.ReasonTwoFactorRequired:
		return RefusalResponse{Status: http.StatusForbidden, Body: apierr.ErrorWithKind("TWO_FACTOR_REQUIRED",
			"Register two-factor authentication or a passkey to use this.", idTwoFactorRequired, apierr.KindPermission)}
	case strongauth.ReasonPasswordNotSet:
		return RefusalResponse{Status: http.StatusForbidden, Body: apierr.ErrorWithKind("PASSWORD_NOT_SET",
			"Set a password to use this.", idPasswordNotSet, apierr.KindPermission)}
	case strongauth.ReasonReauthRequired:
		return RefusalResponse{Status: http.StatusForbidden, Body: apierr.ErrorWithKind("REAUTHENTICATION_REQUIRED",
			"Enter your password and a two-factor code or use a passkey.", idReauthRequired, apierr.KindPermission)}
	case strongauth.ReasonFailed:
		// AUTHENTICATION_FAILED (401) は token が無効という意味で、frontend が
		// サインアウトの扱いをするので使わない。
		return RefusalResponse{Status: http.StatusForbidden, Body: apierr.ErrorWithKind("REAUTHENTICATION_FAILED",
			"The password or the two-factor authentication is incorrect.", idReauthFailed, apierr.KindPermission)}
	case strongauth.ReasonCodeReused:
		return RefusalResponse{Status: http.StatusForbidden, Body: apierr.ErrorWithKind("TWO_FACTOR_CODE_ALREADY_USED",
			"This two-factor code was already used. Wait for the next code or use a passkey.", idCodeReused, apierr.KindPermission)}
	case strongauth.ReasonRateLimited:
		retry := int64((se.RetryAfter + time.Second - 1) / time.Second)
		return RefusalResponse{Status: http.StatusTooManyRequests, Body: apierr.RateLimitExceeded(), RetryAfter: strconv.FormatInt(retry, 10)}
	case strongauth.ReasonPasskeyUnavailable:
		return RefusalResponse{Status: http.StatusBadRequest, Body: apierr.Error("PASSKEY_UNAVAILABLE",
			"No passkey is registered, or passkeys are not available on this server.", idPasskeyUnavailable)}
	default:
		slog.Error("admin/backup: authentication unavailable", "err", err)
		return RefusalResponse{Status: http.StatusServiceUnavailable, Body: apierr.ErrorWithKind("AUTHENTICATION_UNAVAILABLE",
			"Authentication is temporarily unavailable. Please try again later.", idAuthUnavailable, apierr.KindServer), RetryAfter: "10"}
	}
}
