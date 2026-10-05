package middleware

import (
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/labstack/echo/v4"
)

// RolePolicyChecker reports whether a user has a given role policy enabled.
// Implemented by core/role.Service. Declared here so middleware does not
// depend on the role package (avoids the cycle role → server → middleware →
// role and matches the RoleChecker pattern used by RequireAdmin /
// RequireModerator).
type RolePolicyChecker interface {
	HasRolePolicy(userID, policyKey string) bool
}

// RequireRolePolicy returns an Echo middleware that gates the wrapped
// handler by the given role policy key. The middleware:
//
//   - returns 401 CREDENTIAL_REQUIRED when no authenticated user is in
//     context (= same shape as RequireAuth);
//   - returns 403 ROLE_PERMISSION_DENIED with upstream-compatible UUID
//     (apierr.UUIDRolePermissionDenied) when checker.HasRolePolicy reports
//     false (= upstream `ApiCallService` requiredRolePolicy violation shape);
//   - **denies (403) when checker is nil** so a route wired without a policy
//     backend does not silently lose its gate (#3037).
//
// Callers must place this middleware after Authenticate() so GetUser can
// resolve the bearer token. The standard order is `RequireAuth(),
// RequireRolePolicy(checker, key)` since the 401 path here is a fallback
// for misconfigured routes — RequireAuth gives the same response shape
// when called first.
func RequireRolePolicy(checker RolePolicyChecker, policyKey string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user := GetUser(c)
			if user == nil {
				return c.JSON(http.StatusUnauthorized, map[string]any{
					"error": map[string]any{
						"message": "Authentication is required.",
						"code":    "CREDENTIAL_REQUIRED",
						"id":      "1384574d-a912-4b81-8601-c7b1c4085df1",
						"kind":    apierr.KindClient,
					},
				})
			}
			// **checker 未配線は通さない (#3037)。** 以前は skip していたが、
			// それは「依存が欠けたら gate ごと消える」形で、
			// `RequireRolePolicy(nil, ...)` と書いた route が**静かに無防備**に
			// なる。プラグインの `pluginRequest.roles` が「未配線なら常に
			// false = 画面は出ても API は通らない」と決めているのと同じ側に倒す。
			//
			// **Error で記録する。** 配線漏れはサーバー側の不具合なので、
			// 403 だけ返して黙ると誰も気付けない。
			if checker == nil {
				slog.Error("middleware: role policy checker is not wired; denying",
					"policy", policyKey, "path", c.Request().URL.Path)
				return c.JSON(http.StatusForbidden, apierr.RolePermissionDenied())
			}
			if !checker.HasRolePolicy(user.ID, policyKey) {
				return c.JSON(http.StatusForbidden, apierr.RolePermissionDenied())
			}
			return next(c)
		}
	}
}

// RequireRolePolicyPublic gates an endpoint on a role policy while allowing
// anonymous access (upstream requireCredential:false + requiredRolePolicy、例:
// users/search)。匿名は base/default policy で評価する (HasRolePolicy に空 userID を
// 渡すと DefaultPolicies + meta.policies が引かれる)。policy 不許可は 403
// ROLE_PERMISSION_DENIED、401 は返さない (#1784)。checker 未配線時は deny (`RequireRolePolicy` と同じ理由、#3037)。
func RequireRolePolicyPublic(checker RolePolicyChecker, policyKey string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// 未配線は通さない (`RequireRolePolicy` と同じ理由)。
			if checker == nil {
				slog.Error("middleware: role policy checker is not wired; denying",
					"policy", policyKey, "path", c.Request().URL.Path)
				return c.JSON(http.StatusForbidden, apierr.RolePermissionDenied())
			}
			uid := ""
			if u := GetUser(c); u != nil {
				uid = u.ID
			}
			if !checker.HasRolePolicy(uid, policyKey) {
				return c.JSON(http.StatusForbidden, apierr.RolePermissionDenied())
			}
			return next(c)
		}
	}
}

// ChatAvailabilityChecker resolves a user's merged role policies so the
// chatAvailability entry gate can read the `chatAvailability` string value.
// core/role.Service implements GetUserPolicies.
type ChatAvailabilityChecker interface {
	GetUserPolicies(userID string) map[string]any
}

// ChatAvailabilityAllows reports whether the chatAvailability policy value
// permits the given mode ("read" | "write"), mirroring upstream
// ChatService.getChatAvailability: available=>read+write, readonly=>read only,
// unavailable=>neither. A missing / unknown value defaults to allow, matching
// DefaultPolicies (chatAvailability="available") (#1796)。
func ChatAvailabilityAllows(policy, mode string) bool {
	switch policy {
	case "readonly":
		return mode == "read"
	case "unavailable":
		return false
	default: // "available" または legacy/欠損 → default available
		return true
	}
}

// RequireChatAvailability gates a chat endpoint on the actor's chatAvailability
// role policy for the given mode, mirroring upstream
// chatService.checkChatAvailability(me.id, mode). Denied requests get 403
// ROLE_PERMISSION_DENIED. Place after RequireAuth (GetUser must be non-nil);
// a nil user skips the gate (= 401 は RequireAuth が担当、#1796)。
// **checker 未配線は deny** (`RequireRolePolicy` と同じ、#3037)。
func RequireChatAvailability(checker ChatAvailabilityChecker, mode string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// **未配線は通さない。** 同じファイルの `RequireRolePolicy` と
			// 同じ原則 — 依存が欠けたら gate ごと消える形をやめる。ここだけ
			// 残すと、次に nil を渡す経路が生えたとき chat だけ黙って開く。
			if checker == nil {
				slog.Error("middleware: chat availability checker is not wired; denying",
					"mode", mode, "path", c.Request().URL.Path)
				return c.JSON(http.StatusForbidden, apierr.RolePermissionDenied())
			}
			user := GetUser(c)
			if user == nil {
				return next(c)
			}
			policy, _ := checker.GetUserPolicies(user.ID)["chatAvailability"].(string)
			if !ChatAvailabilityAllows(policy, mode) {
				return c.JSON(http.StatusForbidden, apierr.RolePermissionDenied())
			}
			return next(c)
		}
	}
}
