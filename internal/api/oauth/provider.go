package oauth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/misc"
	"github.com/shiroha-a/mk/internal/misc/permissions"
	"github.com/shiroha-a/mk/internal/model"
)

// tokenLen is the length of generated authorization codes and access tokens.
// The access_token.token column is varchar(128); 64 alphanumeric chars (~380
// bits) is ample and well within the limit.
const tokenLen = 64

// UserResolver resolves a user by their native session token. Only native
// login tokens (users.token) match — app/MiAuth access tokens live in a
// separate table — so this naturally restricts the consent step to a logged-in
// browser session.
type UserResolver interface {
	FindByToken(token string) (*model.User, error)
}

// TokenStore mints and revokes OAuth access tokens.
type TokenStore interface {
	Create(token *model.AccessToken) error
	FindByID(id string) (*model.AccessToken, error)
	DeleteByID(id string) error
}

// TokenInvalidator drops a raw token from the auth middleware cache. Without
// it, a revoked token keeps working until the 30s TTL expires.
type TokenInvalidator interface {
	InvalidateToken(token string)
}

// IDGenerator generates access-token row IDs (aidx).
type IDGenerator interface {
	Generate(t time.Time) string
}

// ConsentMeta is injected into the consent page so the frontend OAuth component
// can render the authorization prompt.
type ConsentMeta struct {
	TransactionID string
	ClientName    string
	ClientLogo    string
	Scope         string // space-separated
}

// ConsentRenderer serves the frontend shell HTML with the OAuth meta tags for
// the consent prompt. Wired from the server to frontendHTML.
type ConsentRenderer func(c echo.Context, m ConsentMeta) error

// Handler implements the OAuth2 authorization-code provider endpoints.
type Handler struct {
	store         Store
	httpClient    *http.Client
	userRepo      UserResolver
	tokenRepo     TokenStore
	idGen         IDGenerator
	issuer        string
	allowHTTP     bool
	renderConsent ConsentRenderer
	// authInvalidator は optional。未設定でも revoke 自体は DB から消えるが、
	// auth cache の TTL (30s) の間だけ古い token が通ってしまう。
	authInvalidator TokenInvalidator
	// streamRevoker は失効させた token で張られた /streaming 接続を閉じる。
	// optional。未設定だと、盗んだ code から得た token の WebSocket が
	// 失効後も開いたまま残る。
	streamRevoker StreamRevoker
}

// StreamRevoker closes the /streaming connections authenticated with an app
// access token. Implemented by stream.StreamRevokePublisher.
type StreamRevoker interface {
	RevokeAccessTokenStreams(userID, tokenID string)
}

// SetStreamRevoker wires the revoker used when a code replay revokes a token.
func (h *Handler) SetStreamRevoker(r StreamRevoker) {
	h.streamRevoker = r
}

// HasStreamRevoker reports whether the stream revoker is wired.
func (h *Handler) HasStreamRevoker() bool { return h.streamRevoker != nil }

// SetAuthInvalidator wires the auth middleware so code-replay revocation takes
// effect immediately instead of after the auth cache TTL.
func (h *Handler) SetAuthInvalidator(inv TokenInvalidator) {
	h.authInvalidator = inv
}

// HasAuthInvalidator reports whether the auth cache invalidator is wired.
//
// **これが引かれるのは攻撃を検出した瞬間**。RFC 6749 4.1.2 の
// authorization code 再利用を見つけて token を失効させる経路なので、
// 未配線だとコードを盗んで再利用した相手の access token が、サーバーが
// 失効を決めた後も auth cache の TTL (30s) のあいだ通り続ける (#2682)。
func (h *Handler) HasAuthInvalidator() bool { return h.authInvalidator != nil }

// NewHandler constructs the OAuth provider handler. httpClient must be
// SSRF-guarded (it fetches attacker-suppliable client_id URLs). allowHTTP
// permits http client_id schemes (tests only). renderConsent serves the
// consent page.
func NewHandler(store Store, httpClient *http.Client, userRepo UserResolver, tokenRepo TokenStore, idGen IDGenerator, issuer string, allowHTTP bool, renderConsent ConsentRenderer) *Handler {
	return &Handler{
		store:         store,
		httpClient:    httpClient,
		userRepo:      userRepo,
		tokenRepo:     tokenRepo,
		idGen:         idGen,
		issuer:        issuer,
		allowHTTP:     allowHTTP,
		renderConsent: renderConsent,
	}
}

// Authorize handles GET /oauth/authorize: validate the request, discover the
// client, store the transaction, and serve the consent page. Errors that occur
// before the redirect_uri is validated return a direct 400 (never redirect to
// an unvalidated URI); errors after redirect to the client with ?error=.
func (h *Handler) Authorize(c echo.Context) error {
	q := c.Request().URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	scopeParam := q.Get("scope")
	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")
	state := q.Get("state")

	// 0. response_type 検証。upstream OAuth2ProviderService は client_id 検証より前に
	// response_type を見て、code 以外なら 501 unsupported_response_type を直接返す
	// (redirect しない、#2079)。implicit grant 等を弾く。
	if q.Get("response_type") != "code" {
		applyOAuthNoStore(c)
		return c.JSON(http.StatusNotImplemented, map[string]any{"error": "unsupported_response_type"})
	}

	// 1. client_id 検証 (失敗時は信頼できる redirect 先が無いので直接 400)。
	cu, err := validateClientID(clientID, h.allowHTTP)
	if err != nil {
		return directError(c, "invalid client_id")
	}
	canonicalID := cu.String()

	// 2-3. client discovery (SSRF-guarded fetch + metadata parse)。
	info, err := discoverClientInformation(h.httpClient, canonicalID)
	if err != nil {
		slog.Info("oauth: client discovery failed", "clientId", canonicalID, "err", err)
		return directError(c, "failed to fetch client information")
	}

	// 4. redirect_uri が discovery list に exact 一致するか (open redirect 防止)。
	// ここを通るまで redirect_uri へ error redirect してはならない。
	if redirectURI == "" || !contains(info.RedirectURIs, redirectURI) {
		return directError(c, "invalid redirect_uri")
	}

	// 5. scope を dedupe して既知 kinds に filter。既知 scope が無ければ
	// invalid_scope (upstream OAuth2ProviderService と同じ、#1904)。未知 scope を
	// token.permission に格納しないことで RequireScope の判定対象を健全に保つ。
	scopes := filterKnownScopes(dedupeFields(scopeParam))
	if len(scopes) == 0 {
		return h.redirectError(c, redirectURI, state, "invalid_scope", "scope parameter has no known scope")
	}

	// 6. PKCE は必須 (S256 のみ)。downgrade attack 防止。
	if codeChallenge == "" {
		return h.redirectError(c, redirectURI, state, "invalid_request", "code_challenge is required")
	}
	if codeChallengeMethod != "S256" {
		return h.redirectError(c, redirectURI, state, "invalid_request", "code_challenge_method must be S256")
	}

	// 7. transaction 格納 (5min)。
	txnID := misc.SecureRandomString(tokenLen, misc.AlphanumericChars)
	txn := &transaction{
		ClientID:      canonicalID,
		ClientName:    info.Name,
		ClientLogo:    info.Logo,
		RedirectURI:   redirectURI,
		Scopes:        scopes,
		CodeChallenge: codeChallenge,
		State:         state,
	}
	if err := h.store.PutTransaction(c.Request().Context(), txnID, txn); err != nil {
		slog.Error("oauth: failed to store transaction", "err", err)
		return h.redirectError(c, redirectURI, state, "server_error", "failed to store transaction")
	}

	// 8. consent page を frontend shell + meta で返す。
	c.Response().Header().Set("Cache-Control", "no-store")
	return h.renderConsent(c, ConsentMeta{
		TransactionID: txnID,
		ClientName:    info.Name,
		ClientLogo:    info.Logo,
		Scope:         strings.Join(scopes, " "),
	})
}

// Decision handles POST /oauth/decision: the consent form posts the user's
// native login_token, the transaction_id, and an optional cancel flag. On
// allow it issues an authorization code and redirects to the client.
func (h *Handler) Decision(c echo.Context) error {
	ctx := c.Request().Context()
	transactionID := c.FormValue("transaction_id")
	loginToken := c.FormValue("login_token")
	cancel := c.FormValue("cancel")

	// upstream は decision endpoint のエラーを「Misskey 側の問題でクライアントには
	// 関係ない」と位置づけ、redirect させずに直接返す (oauth.ts のコメント
	// "Do not use indirect error here")。cancel だけが redirect になる。
	//
	// transaction_id 欠落は invalid_request (400)、transaction が無効 / 期限切れは
	// access_denied (403) と種別を分ける。
	if transactionID == "" {
		return directError(c, "Missing transaction ID")
	}
	txn, err := h.store.TakeTransaction(ctx, transactionID)
	if err != nil {
		return directAccessDenied(c, "Invalid or expired transaction ID")
	}

	if cancel != "" {
		return h.redirectError(c, txn.RedirectURI, txn.State, "access_denied", "the user denied the request")
	}

	// #2106 L21: upstream OAuth2ProviderService は lookup 前に空 login_token を弾く
	// (InvalidRequestError 'No user')。空文字で FindByToken("") を実行しないようガードする
	// (token='' 行が万一存在しても consent 完了を防ぐ defense-in-depth)。
	if loginToken == "" {
		return directError(c, "No user")
	}
	// login_token は native session token のみ一致する (app token は別テーブル)。
	user, err := h.userRepo.FindByToken(loginToken)
	if err != nil || user == nil {
		return directError(c, "No such user")
	}

	code := misc.SecureRandomString(tokenLen, misc.AlphanumericChars)
	g := &grant{
		ClientID:      txn.ClientID,
		UserID:        user.ID,
		RedirectURI:   txn.RedirectURI,
		CodeChallenge: txn.CodeChallenge,
		Scopes:        txn.Scopes,
	}
	if err := h.store.PutGrant(ctx, code, g); err != nil {
		slog.Error("oauth: failed to store grant", "err", err)
		return h.redirectError(c, txn.RedirectURI, txn.State, "server_error", "failed to issue code")
	}

	redirect := appendQuery(txn.RedirectURI, map[string]string{
		"code":  code,
		"state": txn.State,
		"iss":   h.issuer,
	})
	// **認可コードを載せる redirect にも no-store を付ける。**
	// upstream の `redirectWithQuery` も冒頭で `applyNoStore` する。
	applyOAuthNoStore(c)
	return c.Redirect(http.StatusFound, redirect)
}

// Token handles POST /oauth/token: exchange an authorization code (+ PKCE
// verifier) for an access token. Accepts urlencoded or JSON bodies.
func (h *Handler) Token(c echo.Context) error {
	ctx := c.Request().Context()
	// CORS (cross-origin SPA / native client) は root の CORS middleware が
	// 付与するのでここでは設定しない (二重 header 回避、#1899 review)。

	// upstream は urlencoded / JSON 両方の body を受け付ける
	// (bodyParser.urlencoded + bodyParser.json)。Content-Type が JSON の場合は
	// FormValue が拾えないので明示的に decode する。
	var grantType, code, clientID, redirectURI, codeVerifier string
	if strings.HasPrefix(c.Request().Header.Get("Content-Type"), "application/json") {
		var body struct {
			GrantType    string `json:"grant_type"`
			Code         string `json:"code"`
			ClientID     string `json:"client_id"`
			RedirectURI  string `json:"redirect_uri"`
			CodeVerifier string `json:"code_verifier"`
		}
		_ = json.NewDecoder(c.Request().Body).Decode(&body)
		grantType, code, clientID, redirectURI, codeVerifier = body.GrantType, body.Code, body.ClientID, body.RedirectURI, body.CodeVerifier
	} else {
		grantType = c.FormValue("grant_type")
		code = c.FormValue("code")
		clientID = c.FormValue("client_id")
		redirectURI = c.FormValue("redirect_uri")
		codeVerifier = c.FormValue("code_verifier")
	}

	if grantType != "authorization_code" {
		return tokenError(c, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
	}

	g, err := h.store.GetGrant(ctx, code)
	if err != nil && !errors.Is(err, ErrNotFound) {
		// store backend error (e.g. redis down) は invalid_grant ではなく
		// server_error。code 自体の無効と区別する。
		slog.Error("oauth: grant lookup failed", "err", err)
		return tokenError(c, http.StatusInternalServerError, "server_error", "internal error")
	}
	if g == nil {
		return tokenError(c, http.StatusBadRequest, "invalid_grant", "invalid authorization code")
	}

	// code を atomic に claim する。validation 成否に関わらず単回で焼き切れる。
	firstUse, err := h.store.ClaimGrant(ctx, code)
	if err != nil {
		slog.Error("oauth: grant claim failed", "err", err)
		return tokenError(c, http.StatusInternalServerError, "server_error", "internal error")
	}
	if !firstUse {
		// replay: 以前発行した token を revoke する (RFC6749 §4.1.2)。
		if tid, _ := h.store.GetGrantToken(ctx, code); tid != "" {
			// DB から消すだけでは auth cache (30s TTL) に残った entry で
			// 通り続けるため、raw token を引いてから cache も落とす。
			var raw string
			if row, ferr := h.tokenRepo.FindByID(tid); ferr == nil && row != nil {
				raw = row.Token
			}
			if derr := h.tokenRepo.DeleteByID(tid); derr != nil {
				slog.Warn("oauth: failed to revoke token on code replay", "tokenId", tid, "err", derr)
			} else {
				if raw != "" && h.authInvalidator != nil {
					h.authInvalidator.InvalidateToken(raw)
				}
				// その token で既に張られた WebSocket も閉じる (mk-go 独自)。
				if h.streamRevoker != nil {
					h.streamRevoker.RevokeAccessTokenStreams(g.UserID, tid)
				}
			}
		}
		slog.Info("oauth: authorization code replay detected", "clientId", g.ClientID, "userId", g.UserID)
		return tokenError(c, http.StatusBadRequest, "invalid_grant", "authorization code already used")
	}

	if canonicalClientID(clientID, h.allowHTTP) != g.ClientID {
		return tokenError(c, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
	}
	if redirectURI != g.RedirectURI {
		return tokenError(c, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
	}
	if !verifyS256(codeVerifier, g.CodeChallenge) {
		return tokenError(c, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
	}

	now := time.Now()
	accessToken := misc.SecureRandomString(tokenLen, misc.AlphanumericChars)
	name := g.ClientID
	row := &model.AccessToken{
		ID:         h.idGen.Generate(now),
		Token:      accessToken,
		Hash:       accessToken, // OAuth token: hash == token (no app secret)。
		UserID:     g.UserID,
		Name:       &name,
		Permission: g.Scopes,
		LastUsedAt: &now,
	}
	if err := h.tokenRepo.Create(row); err != nil {
		slog.Error("oauth: failed to create access token", "err", err)
		return tokenError(c, http.StatusInternalServerError, "server_error", "failed to issue token")
	}
	// replay 時に revoke できるよう、code に紐づく token id を記録する。
	if err := h.store.PutGrantToken(ctx, code, row.ID); err != nil {
		slog.Warn("oauth: failed to record grant token id", "err", err)
	}

	slog.Info("oauth: issued access token", "clientId", g.ClientID, "userId", g.UserID, "scopes", g.Scopes)
	// **トークン応答には no-store を付ける (RFC 6749 §5.1 は MUST)。**
	// `/oauth/*` は api グループの外なので既定ヘッダも掛からない。
	applyOAuthNoStore(c)
	return c.JSON(http.StatusOK, map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"scope":        strings.Join(g.Scopes, " "),
	})
}

// NotFound replies 404 for unknown /oauth/* paths so clients can probe endpoint
// support, mirroring upstream's catch-all.
func (h *Handler) NotFound(c echo.Context) error {
	return c.JSON(http.StatusNotFound, map[string]any{
		"error": map[string]any{
			"message": "Unknown OAuth endpoint.",
			"code":    "UNKNOWN_OAUTH_ENDPOINT",
			"id":      "aa49e620-26cb-4e28-aad6-8cbcb58db147",
			"kind":    "client",
		},
	})
}

// --- helpers ---

// canonicalClientID returns the normalized client_id (path-completed) or "" if
// invalid, so the token-exchange comparison matches the authorize-time form
// regardless of a missing trailing slash.
func canonicalClientID(raw string, allowHTTP bool) string {
	u, err := validateClientID(raw, allowHTTP)
	if err != nil {
		return ""
	}
	return u.String()
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// filterKnownScopes keeps only recognized permission kinds, preserving order,
// matching upstream's `.filter(s => kinds.includes(s))` (#1904)。
func filterKnownScopes(scopes []string) []string {
	out := scopes[:0:0]
	for _, s := range scopes {
		if permissions.IsKnown(s) {
			out = append(out, s)
		}
	}
	return out
}

// dedupeFields splits a space-separated scope string and removes duplicates,
// preserving order.
func dedupeFields(s string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, f := range strings.Fields(s) {
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

func appendQuery(base string, params map[string]string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// directError returns a plain 400 for authorization errors that occur before a
// trusted redirect_uri is established.
func directError(c echo.Context, desc string) error {
	return c.JSON(http.StatusBadRequest, map[string]any{
		"error":             "invalid_request",
		"error_description": desc,
	})
}

// directAccessDenied returns the 403 access_denied upstream's
// createForbiddenAccessDenied produces. 無効 / 期限切れの transaction ID に
// 使う (invalid_request の 400 とは別種別)。
func directAccessDenied(c echo.Context, desc string) error {
	return c.JSON(http.StatusForbidden, map[string]any{
		"error":             "access_denied",
		"error_description": desc,
	})
}

// applyOAuthNoStore sets the no-cache headers upstream's applyNoStore applies to
// authorization-endpoint responses (#2079)。
func applyOAuthNoStore(c echo.Context) {
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Pragma", "no-cache")
}

// redirectError redirects to the (validated) client redirect_uri with the OAuth
// error parameters, including the RFC9207 iss.
func (h *Handler) redirectError(c echo.Context, redirectURI, state, code, desc string) error {
	// #2106 L22: upstream は redirect error に error / state / iss のみ載せ、error_description は
	// 付与しない (RFC 上 optional、token endpoint の JSON error にのみ含める)。desc は log に残す。
	slog.Debug("oauth: redirect error", "code", code, "desc", desc)
	redirect := appendQuery(redirectURI, map[string]string{
		"error": code,
		"state": state,
		"iss":   h.issuer,
	})
	applyOAuthNoStore(c)
	return c.Redirect(http.StatusFound, redirect)
}

// tokenError returns an OAuth token-endpoint JSON error.
func tokenError(c echo.Context, status int, code, desc string) error {
	applyOAuthNoStore(c)
	return c.JSON(status, map[string]any{
		"error":             code,
		"error_description": desc,
	})
}
