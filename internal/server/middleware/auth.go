package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"gorm.io/gorm"
)

// errOrphanedAccessToken は access_token 行は残っているが Preload した
// User が nil (= ユーザー削除済み) のケース。Authenticate はこれを「認証
// 失敗扱い」(anonymous request 継続) として扱い、後続 dereference panic
// を避ける (Devin #514 FLAG-1)。
var errOrphanedAccessToken = errors.New("auth: access_token references a deleted user")

type contextKey string

const (
	UserContextKey  contextKey = "misskeyUser"
	TokenContextKey contextKey = "misskeyToken"
	// AuthScopeContextKey carries the *AuthScope (OAuth scope view) for the
	// authenticated request. RequireScope reads it to enforce endpoint
	// meta.kind against an app token's permission array (#1552).
	AuthScopeContextKey contextKey = "misskeyAuthScope"
	// suspendedContextKey flags that the request carried a valid token whose
	// account is suspended. The user is kept out of UserContextKey (so public
	// endpoints treat it as anonymous, #962 P2) but credential-required gates
	// read this flag to return 403 YOUR_ACCOUNT_SUSPENDED instead of 401
	// CREDENTIAL_REQUIRED (upstream ApiCallService.ts、#1559)。
	suspendedContextKey contextKey = "misskeySuspended"
	// rawTokenPresentContextKey flags that the request carried a raw auth token
	// (Bearer / body `i`), regardless of whether it resolved to a valid user.
	// upstream ApiCallService が cacheSec の Cache-Control 判定を raw token (`!token`)
	// で行うため、無効/suspended/deleted token でも cache を付けないようにする (#2049)。
	rawTokenPresentContextKey contextKey = "misskeyRawTokenPresent"
	// deletedContextKey flags that the request carried a valid token whose
	// account is (logically) deleted. HTTP では匿名として扱うが、/streaming は
	// 凍結と同じくこの接続を拒否する (IsInactiveAccountRequest)。
	deletedContextKey contextKey = "misskeyDeleted"
)

// AuthScope is the OAuth scope view of an authenticated request. IsApp is
// true when the request used an app access_token (Scopes = its permission
// array); false for a native login token, which has full access.
type AuthScope struct {
	IsApp  bool
	Scopes []string
	// TokenID は app access_token の id。i/registry/* が app ごとの registry
	// 空間を分離するための domain として使う (#1717)。native token / cookie
	// session では "" (= 分離しない、ps.domain をそのまま使う)。
	TokenID string
}

// AuthMiddleware provides token-based authentication.
type AuthMiddleware struct {
	userRepo        repository.UserRepository
	accessTokenRepo repository.AccessTokenRepository

	// tokenCache は token → resolved user の短命キャッシュ (#512 / #413 #3)。
	// 認証要リクエストの DB hit を 30 秒 TTL で消す。logout/token revoke の
	// 反映遅延は許容範囲内 (TTL 以内)。
	tokenCache *tokenCache
}

// NewAuthMiddleware creates a new AuthMiddleware.
func NewAuthMiddleware(userRepo repository.UserRepository, accessTokenRepo repository.AccessTokenRepository) *AuthMiddleware {
	return &AuthMiddleware{
		userRepo:        userRepo,
		accessTokenRepo: accessTokenRepo,
		tokenCache:      newTokenCache(),
	}
}

// InvalidateToken removes a token entry from the auth cache so the next
// /api/i request with that token re-resolves through the DB (= will fail
// after `users.token` was rotated by i/regenerate-token、#884 security
// fix)。本 method は i.TokenInvalidator interface の実装。
func (a *AuthMiddleware) InvalidateToken(token string) {
	if token == "" {
		return
	}
	a.tokenCache.invalidate(token)
}

// InvalidateTokensForUser drops every cached entry that resolves to the
// given userID, across all of the user's tokens (native login + access
// tokens). Admin actions (suspend / unsuspend / delete-account) call this
// so the target user is locked out of (or restored back into) the cache
// without waiting up to 30s for natural TTL expiry (#965). #884 の
// InvalidateToken は本 request の token 単独削除なので、admin が他 user
// の全 session を即時失効したいケースには別 method が要る。
//
// noop when userID is empty.
func (a *AuthMiddleware) InvalidateTokensForUser(userID string) {
	if userID == "" {
		return
	}
	a.tokenCache.invalidateByUserID(userID)
}

// Authenticate is an Echo middleware that extracts and validates the user token.
// It does NOT reject unauthenticated requests - it just sets the user if valid.
func (a *AuthMiddleware) Authenticate() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			token := extractToken(c)
			if token == "" {
				return next(c)
			}
			// raw token が存在することを記録する (解決可否に依らず)。cacheSec の
			// Cache-Control 判定が upstream の `!token` (raw token) を見るため (#2049)。
			c.Set(string(rawTokenPresentContextKey), true)

			user, scopes, tokenID, isApp, err := a.resolveUser(token)
			if err != nil {
				// token はあるが解決できない。token 不在 (= 無効 token) は
				// upstream ApiCallService が AuthenticationError として 401
				// AUTHENTICATION_FAILED を返す経路なのでそれに揃える (#1559)。
				// WWW-Authenticate: error=invalid_token は WWWAuthenticate
				// middleware が body から判定して付ける。orphaned access_token
				// (#514) や transient な DB error は誤って全 request を 401 に
				// しないよう従来どおり anonymous に落とし、後続の認可に委ねる。
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return c.JSON(http.StatusUnauthorized, apierr.AuthenticationFailed())
				}
				return next(c)
			}

			// 論理削除された user は anonymous request 扱いに落とす (#962 P2)。
			if user.IsDeleted {
				c.Set(string(deletedContextKey), true)
				return next(c)
			}
			// 凍結された user は公開 endpoint では anonymous 扱いに落とす
			// (#962 P2) が、credential 必須 endpoint では 403
			// YOUR_ACCOUNT_SUSPENDED を返せるよう flag を積む (upstream
			// ApiCallService.ts、#1559)。upstream は signin handler で
			// isSuspended だけ check しているが、有効 token を既に持っている
			// session は post-auth pipeline では gate されず tokenCache
			// (30s TTL) 経由でも bypass できてしまうため、cache miss → DB
			// fetch 経路で確実に弾く。cache hit (凍結前 entry) は stale な
			// isSuspended=false を返すので self-mutation / admin 経路は
			// InvalidateToken(sForUser) で 30s → μs window に縮める。
			if user.IsSuspended {
				c.Set(string(suspendedContextKey), true)
				return next(c)
			}

			c.Set(string(UserContextKey), user)
			// handler 側でこの request の auth token を引きたいケースがある
			// (例: i/update が成功後に tokenCache を invalidate する目的、
			// #960)。raw token をそのまま context に積む。app/auth 経由の
			// access_token / native login token どちらも同じキーで取得可能。
			c.Set(string(TokenContextKey), token)
			// OAuth scope view を積む。RequireScope が endpoint の meta.kind を
			// app token の permission で強制する (#1552)。native token は
			// IsApp=false で full access 扱い。
			c.Set(string(AuthScopeContextKey), &AuthScope{IsApp: isApp, Scopes: scopes, TokenID: tokenID})
			return next(c)
		}
	}
}

// IsSuspendedRequest reports whether the request carried a valid token whose
// account is suspended. Handlers that do their own credential check (instead of
// RequireAuth) need this to keep the 403 YOUR_ACCOUNT_SUSPENDED behaviour that
// credentialRequiredResponse provides.
//
// upstream ApiCallService も suspended は 403 にするので、endpoint 固有の
// CREDENTIAL_REQUIRED を返す handler でも suspended だけは 403 に分ける必要がある。
func IsSuspendedRequest(c echo.Context) bool {
	v, _ := c.Get(string(suspendedContextKey)).(bool)
	return v
}

// IsInactiveAccountRequest reports whether the request carried a valid token
// whose account is suspended or deleted. Both are treated as anonymous by
// Authenticate; /streaming uses this to refuse the upgrade instead
// (upstream StreamingApiServerService rejects suspended users with 403).
func IsInactiveAccountRequest(c echo.Context) bool {
	if IsSuspendedRequest(c) {
		return true
	}
	v, _ := c.Get(string(deletedContextKey)).(bool)
	return v
}

// credentialRequiredResponse writes the error for a credential-required gate
// when GetUser is nil: 403 YOUR_ACCOUNT_SUSPENDED when the request carried a
// valid token for a suspended account (upstream ApiCallService.ts、#1559)、else
// 401 CREDENTIAL_REQUIRED. Shared by RequireAuth / RequireSecure /
// RequireAdmin / RequireModerator so the suspended branch stays consistent.
func credentialRequiredResponse(c echo.Context) error {
	if v, _ := c.Get(string(suspendedContextKey)).(bool); v {
		return c.JSON(http.StatusForbidden, apierr.YourAccountSuspended())
	}
	return c.JSON(http.StatusUnauthorized, apierr.CredentialRequired())
}

// RequireAuth is a middleware that requires authentication.
func RequireAuth() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user := GetUser(c)
			if user == nil {
				return credentialRequiredResponse(c)
			}
			return next(c)
		}
	}
}

// RequireSecure requires the request to be authenticated via the user's native
// session token (users.token), not a third-party app / MiAuth access token.
// It mirrors Misskey's `secure: true` meta (isSecure = user != null && token ==
// null): account-security endpoints (password change / 2FA / data export &
// import / authorized-apps) must not be drivable by an OAuth app even when the
// app holds a valid access token.
//
// The native token is the only credential that equals users.token; app / MiAuth
// access tokens resolve the same user but via a separate access_tokens row, so
// their raw token never matches user.Token.
func RequireSecure() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user := GetUser(c)
			if user == nil {
				return credentialRequiredResponse(c)
			}
			if user.Token == nil || *user.Token != GetToken(c) {
				return c.JSON(http.StatusForbidden, apierr.ErrorWithKind(
					"ACCESS_DENIED",
					"Access denied.",
					apierr.UUIDAccessDeniedSecure,
					apierr.KindClient,
				))
			}
			return next(c)
		}
	}
}

// GetUser returns the authenticated user from the context.
func GetUser(c echo.Context) *model.User {
	u, ok := c.Get(string(UserContextKey)).(*model.User)
	if !ok {
		return nil
	}
	return u
}

// GetToken returns the raw auth token used for the current request, or
// empty string if the request was not authenticated. Handlers that perform
// self-mutation (e.g. i/update) use this to invalidate the auth middleware's
// tokenCache entry so subsequent requests read fresh user state from DB
// instead of returning stale cached values within the 30s TTL window
// (#960).
func GetToken(c echo.Context) string {
	t, ok := c.Get(string(TokenContextKey)).(string)
	if !ok {
		return ""
	}
	return t
}

// HasRawToken reports whether the request carried a raw auth token (Bearer /
// body `i`), regardless of whether it resolved to a valid user. cacheSec
// endpoints use this so an invalid/suspended/deleted token still suppresses the
// public Cache-Control, matching upstream ApiCallService's `!token` check (#2049).
func HasRawToken(c echo.Context) bool {
	v, _ := c.Get(string(rawTokenPresentContextKey)).(bool)
	return v
}

// GetAuthScope returns the OAuth scope view for the current request, or nil
// when the request was unauthenticated (no token). A non-nil result with
// IsApp=false denotes a native login token (full access).
func GetAuthScope(c echo.Context) *AuthScope {
	s, ok := c.Get(string(AuthScopeContextKey)).(*AuthScope)
	if !ok {
		return nil
	}
	return s
}

// RequireScope enforces the upstream endpoint meta.kind (OAuth scope) for app
// access tokens, mirroring ApiCallService.ts: a request authenticated with an
// app token must carry `kind` in its permission array, otherwise 403
// PERMISSION_DENIED. Native login tokens (and the anonymous case, which the
// companion RequireModerator/RequireAuth rejects first) are not scope-checked
// — they have full access. Place RequireScope after the role gate so role and
// scope are both enforced (#1552).
func RequireScope(kind string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			sc := GetAuthScope(c)
			// native login token / anonymous → no scope restriction.
			if sc == nil || !sc.IsApp {
				return next(c)
			}
			for _, p := range sc.Scopes {
				if p == kind {
					return next(c)
				}
			}
			return c.JSON(http.StatusForbidden, apierr.PermissionDenied())
		}
	}
}

// RejectAppToken denies requests authenticated with a third-party app access
// token. Native login tokens and unauthenticated requests pass through.
//
// **`kind` を宣言できない経路のための受け皿 (#3037)。** upstream の
// `ApiCallService.ts:412-413` は「`kind` が無く、かつ資格情報を要する
// endpoint」に対し app token を**一律で拒否**する。`RequireScope` が
// 「`kind` があるとき」の半分で、こちらが残りの半分にあたる。
//
// プラグインのルートがこれを要る側になる — 本体は plugin が何を要求するか
// 知らないので `kind` を決められず、`RequireScope` を配線できない。**gate を
// 置かないと、`read:account` だけを許可した第三者アプリのトークンで
// `POST /api/plugin/<name>/...` に到達できる**。プラグインが doc どおり
// `req.IsModerator()` で守っていても scope は効かず、そこから
// `ctx.API().AsUser(req.UserID())` (= 対象利用者の native token) 経由で
// `i/change-password` のような endpoint へ抜けられる。
func RejectAppToken() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if sc := GetAuthScope(c); sc != nil && sc.IsApp {
				return c.JSON(http.StatusForbidden, apierr.PermissionDenied())
			}
			return next(c)
		}
	}
}

// RequireNotMoved rejects requests from accounts that have completed an
// account migration (movedToUri set), mirroring upstream prohibitMoved
// endpoints (ApiCallService.ts:368-377、#1562)。RequireAuth の後段に配線する
// こと (GetUser が nil の場合は素通しし、認可は RequireAuth 側に任せる)。
func RequireNotMoved() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user := GetUser(c)
			if user != nil && user.MovedToURI != nil && *user.MovedToURI != "" {
				return c.JSON(http.StatusForbidden, apierr.ErrorWithKind(
					"YOUR_ACCOUNT_MOVED",
					"You have moved your account.",
					"56f20ec9-fd06-4fa5-841b-edd6d7d4fa31",
					apierr.KindPermission,
				))
			}
			return next(c)
		}
	}
}

// RoleChecker abstracts role checking to avoid circular dependency with core/role.
type RoleChecker interface {
	IsAdministrator(userID string) bool
	IsModerator(userID string) bool
}

// RequireAdmin is a middleware that requires the user to have an administrator role.
func RequireAdmin(checker RoleChecker) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user := GetUser(c)
			if user == nil {
				return credentialRequiredResponse(c)
			}
			if !checker.IsAdministrator(user.ID) {
				return c.JSON(http.StatusForbidden, map[string]any{
					"error": map[string]any{
						"message": "You are not an administrator.",
						"code":    "ROLE_PERMISSION_DENIED",
						// upstream ApiCallService requireAdmin gate の UUID (#1775)。
						"id":   "c3d38592-54c0-429d-be96-5636b0431a61",
						"kind": apierr.KindPermission,
					},
				})
			}
			return next(c)
		}
	}
}

// RequireModerator is a middleware that requires the user to have a moderator or admin role.
func RequireModerator(checker RoleChecker) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user := GetUser(c)
			if user == nil {
				return credentialRequiredResponse(c)
			}
			if !checker.IsModerator(user.ID) {
				return c.JSON(http.StatusForbidden, map[string]any{
					"error": map[string]any{
						"message": "You are not a moderator.",
						"code":    "ROLE_PERMISSION_DENIED",
						// upstream ApiCallService requireModerator gate の UUID。admin gate
						// (c3d38592-...) とは別物 (#1775)。
						"id":   "d33d5333-db36-423d-a8f9-1a2b9549da41",
						"kind": apierr.KindPermission,
					},
				})
			}
			return next(c)
		}
	}
}

func extractToken(c echo.Context) string {
	// Bearer token from Authorization header
	auth := c.Request().Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}

	// Query parameter
	if t := c.QueryParam("i"); t != "" {
		return t
	}

	// multipart/form-data の "i" フィールド (ファイルアップロード時)
	req := c.Request()
	ct := req.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if t := c.FormValue("i"); t != "" {
			return t
		}
		return ""
	}

	// JSON body の "i" フィールド (Misskey-style)
	// フロントエンドは全 API で POST {"i":"token", ...} を送信する。
	// ボディを読んだ後にリセットし、後続ハンドラが再読み取り可能にする。
	if req.Body != nil && req.ContentLength != 0 {
		body, err := io.ReadAll(req.Body)
		if err == nil && len(body) > 0 {
			// ボディをリセット (下流のJSONBodyParse/c.Bindには原文を渡す)
			req.Body = io.NopCloser(bytes.NewReader(body))
			var parsed struct {
				I string `json:"i"`
			}
			// 本家 (secure-json-parse) はUTF-8 BOMを除去してからparseする
			// ため、BOM付きbodyのtoken抽出もここで除去して揃える (#1609)。
			// encoding/json はBOMを受理しないので除去しないと匿名扱いになる。
			if json.Unmarshal(bytes.TrimPrefix(body, utf8BOM), &parsed) == nil && parsed.I != "" {
				return parsed.I
			}
		}
	}

	return ""
}

// resolveUser resolves a raw token to its user and OAuth scope view.
// isApp is true when the token is an app access_token (its permission array
// is returned as scopes); false for a native login token (full access,
// scopes nil). RequireScope (#1552) uses these to enforce endpoint meta.kind.
func (a *AuthMiddleware) resolveUser(token string) (user *model.User, scopes []string, tokenID string, isApp bool, err error) {
	// hot path: TTL 内なら DB を引かずに cached user/scope を返す (#512)。
	if u, sc, tid, app, ok := a.tokenCache.get(token); ok {
		return u, sc, tid, app, nil
	}

	// まずnative tokenで検索 (= user 自身の login token, full access)。
	user, err = a.userRepo.FindByToken(token)
	if err == nil && !nativeTokenMatches(user.Token, token) {
		// **保存値と完全一致しないものは native token として認めない。**
		// user.token は char(16) で、PostgreSQL の char 比較は末尾の空白を
		// 無視するので、"<token> " も同じ利用者に解決される。認証自体は通っても
		// 害が無いように見えるが、この文字列は tokenCache の別キーになり
		// (InvalidateToken が消すのは空白なしのキーだけ)、WebSocket の失効の鍵も
		// 「リクエストの文字列」から作られて i/regenerate-token で閉じなくなる。
		// 正当なクライアントが空白を足すことは無いので、完全一致を要求する
		// (upstream の findOneBy も char 比較なので、ここは安全側への乖離)。
		// 空白付きの文字列は access_token (varchar) とも一致しないので、下の
		// 経路で not found に落ちる。
		user, err = nil, gorm.ErrRecordNotFound
	}
	if err == nil {
		a.tokenCache.put(token, user, nil, "", false)
		return user, nil, "", false, nil
	}

	// hash 列 (miauth: sha256(token)) と token 列 (raw, app/auth) を 1 query
	// で OR 検索する。upstream Misskey TS の AuthenticateService と同 pattern
	// (#910)。両 index がある前提で BitmapOr scan に乗る。
	hash := sha256Hash(token)
	accessToken, err := a.accessTokenRepo.FindByHashOrToken(hash, token)
	if err != nil {
		// not-found 系は cache に積まない: 失効後 30 秒間 stale を返さない
		// ようにするのと、未知 token 連打への DDoS を rate limiter 側に任せる
		// 設計のため (#512 scope)。
		return nil, nil, "", false, err
	}

	// orphaned access_token (User 行が削除済み) のとき GORM の Preload は
	// User を nil にする。そのまま (nil, nil) を返すと Authenticate 側で
	// user.ID dereference panic になるので、専用 error を返して anonymous
	// request 扱いに落とす (Devin #514 FLAG-1)。cache にも積まない。
	if accessToken.User == nil {
		return nil, nil, "", false, errOrphanedAccessToken
	}
	// app token は permission 配列を scope として持つ。空 ([]) でも isApp=true
	// として扱い、RequireScope が kind 不足を弾けるようにする。tokenID は
	// registry domain 分離用 (#1717)。
	scopes = []string(accessToken.Permission)
	a.tokenCache.put(token, accessToken.User, scopes, accessToken.ID, true)
	return accessToken.User, scopes, accessToken.ID, true, nil
}

// nativeTokenMatches reports whether the raw request token is exactly the
// native token stored for the user.
//
// 保存値は char(16) なので、16 文字に満たない token は末尾を空白で埋めた形で
// 読み出される。埋め草だけを落として比べ、リクエスト側の空白は落とさない。
//
// stored が nil のときは照合できないので通す。本番の FindByToken は
// `token = ?` に一致した行しか返さないので nil にはならず、nil になるのは
// token 列を埋めない mock 経由だけ。
func nativeTokenMatches(stored *string, token string) bool {
	return stored == nil || strings.TrimRight(*stored, " ") == token
}

func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
