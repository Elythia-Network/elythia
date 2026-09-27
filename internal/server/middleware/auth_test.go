package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthenticate_NoToken(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		user := GetUser(c)
		assert.Nil(t, user)
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_BearerToken(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	user := &model.User{ID: "user1", Username: "testuser"}
	nativeToken := "abcdef1234567890"
	userRepo.Tokens[nativeToken] = user

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+nativeToken)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		u := GetUser(c)
		assert.NotNil(t, u)
		assert.Equal(t, "user1", u.ID)
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
}

// #960: handler が現在 request の auth token を取得できる必要がある
// (i/update 成功後に tokenCache を invalidate する目的)。Authenticate
// middleware は raw token を context に詰める。
func TestAuthenticate_PutsTokenInContext(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	user := &model.User{ID: "user1", Username: "testuser"}
	nativeToken := "abcdef1234567890"
	userRepo.Tokens[nativeToken] = user

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+nativeToken)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		assert.Equal(t, nativeToken, GetToken(c),
			"middleware は raw auth token を context にそのまま積むべき")
		return c.String(http.StatusOK, "ok")
	})
	require.NoError(t, handler(c))
}

func TestGetToken_NotAuthenticated(t *testing.T) {
	// 未認証 request では GetToken は空文字を返す (panic しない)。
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	assert.Equal(t, "", GetToken(c))
}

// #965: admin が target user を suspend / unsuspend / delete したとき、
// target の全 token cache entry を 1 回で削除する exported method。
// AuthMiddleware は admin handler の UserTokenInvalidator interface を
// duck-typed で実装する。
func TestAuthMiddleware_InvalidateTokensForUser(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	// 直接 cache に詰めて exported method の挙動だけを isolate に test する。
	auth.tokenCache.put("a", &model.User{ID: "u1"}, nil, "", false)
	auth.tokenCache.put("b", &model.User{ID: "u1"}, nil, "", false)
	auth.tokenCache.put("c", &model.User{ID: "u2"}, nil, "", false)

	auth.InvalidateTokensForUser("u1")

	if _, _, _, _, ok := auth.tokenCache.get("a"); ok {
		t.Error("u1 token a が残っている")
	}
	if _, _, _, _, ok := auth.tokenCache.get("b"); ok {
		t.Error("u1 token b が残っている")
	}
	if _, _, _, _, ok := auth.tokenCache.get("c"); !ok {
		t.Error("u2 token c が誤って削除された")
	}
}

func TestAuthMiddleware_InvalidateTokensForUser_EmptyUserIDIsNoop(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)
	auth.tokenCache.put("a", &model.User{ID: "u1"}, nil, "", false)
	auth.InvalidateTokensForUser("")
	if _, _, _, _, ok := auth.tokenCache.get("a"); !ok {
		t.Error("userID 空で invalidate が走って巻き添えになった")
	}
}

// #962 P2: 論理削除 / 凍結された user は middleware 段階で anonymous
// request 扱いに落とす。cache miss → DB fetch path で gate が fire する
// ことを担保する。
func TestAuthenticate_SuspendedUserTreatedAsAnonymous(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	suspended := &model.User{ID: "u1", Username: "suspended", IsSuspended: true}
	token := "suspended-user-token"
	userRepo.Tokens[token] = suspended

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		assert.Nil(t, GetUser(c), "凍結 user は context に attach されない")
		assert.Equal(t, "", GetToken(c), "凍結 user は token も context に attach されない")
		// raw token は存在する (解決できなくても)。cacheSec gate がこれで cache を抑える (#2049)。
		assert.True(t, HasRawToken(c), "raw token は HasRawToken=true (#2049)")
		return c.String(http.StatusOK, "ok")
	})
	require.NoError(t, handler(c))
}

// #2049: 無効/suspended/deleted token を付けた未認証 GET は HasRawToken=true なので
// cacheSec endpoint で public Cache-Control を付けない (upstream `!token` 判定)。一方
// token を一切付けない GET は HasRawToken=false。
func TestAuthenticate_HasRawToken(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)
	e := echo.New()

	// token 無し → HasRawToken=false。
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	require.NoError(t, auth.Authenticate()(func(c echo.Context) error {
		assert.False(t, HasRawToken(c), "token 無しは HasRawToken=false")
		return nil
	})(c))

	// query param i= の token (解決不能でも transient error 経路で anonymous) → HasRawToken=true。
	deleted := &model.User{ID: "ud", Username: "del", IsDeleted: true}
	userRepo.Tokens["dtok"] = deleted
	req2 := httptest.NewRequest(http.MethodGet, "/?i=dtok", nil)
	c2 := e.NewContext(req2, httptest.NewRecorder())
	require.NoError(t, auth.Authenticate()(func(c echo.Context) error {
		assert.True(t, HasRawToken(c), "deleted token でも raw token はあるので HasRawToken=true")
		return nil
	})(c2))
}

func TestAuthenticate_DeletedUserTreatedAsAnonymous(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	deleted := &model.User{ID: "u2", Username: "deleted", IsDeleted: true}
	token := "deleted-user-token"
	userRepo.Tokens[token] = deleted

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		assert.Nil(t, GetUser(c), "削除済 user は context に attach されない")
		assert.True(t, IsInactiveAccountRequest(c), "/streaming が拒否できるよう印を積む")
		assert.False(t, IsSuspendedRequest(c), "凍結の 403 YOUR_ACCOUNT_SUSPENDED とは区別する")
		return c.String(http.StatusOK, "ok")
	})
	require.NoError(t, handler(c))
}

// 凍結 → RequireAuth gate と組み合わせると 401 になることを担保。
// signin handler 以外の認証要 endpoint は middleware 通過時点で
// CREDENTIAL_REQUIRED で弾かれる (= 30s attack window が closed)。
func TestRequireAuth_SuspendedRejected(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	suspended := &model.User{ID: "u3", Username: "frozen", IsSuspended: true}
	token := "another-suspended-token"
	userRepo.Tokens[token] = suspended

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	// Authenticate → RequireAuth の chain は production の認証要 endpoint
	// と同じ pipeline。
	chained := auth.Authenticate()(RequireAuth()(func(c echo.Context) error {
		t.Fatal("RequireAuth は通過するべきでない")
		return nil
	}))
	require.NoError(t, chained(c))
	// 凍結 user が credential 必須 endpoint を叩くと 403 YOUR_ACCOUNT_SUSPENDED
	// が返る (upstream ApiCallService.ts、#1559)。
	assert.Equal(t, http.StatusForbidden, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "YOUR_ACCOUNT_SUSPENDED", errObj["code"])
	assert.Equal(t, "a8c724b3-6e9c-4b46-b1a8-bc3ed6258370", errObj["id"])
	assert.Equal(t, apierr.KindPermission, errObj["kind"])
}

func TestAuthenticate_QueryParam(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	user := &model.User{ID: "user2", Username: "queryuser"}
	nativeToken := "querytoken123456"
	userRepo.Tokens[nativeToken] = user

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?i="+nativeToken, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		u := GetUser(c)
		assert.NotNil(t, u)
		assert.Equal(t, "user2", u.ID)
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
}

// countingUserRepo wraps the mock to count FindByToken calls so we can
// assert that the resolved user came from the in-memory cache and not DB
// on the second hit (#512).
type countingUserRepo struct {
	*testutil.MockUserRepository
	findByTokenCalls int
}

func (c *countingUserRepo) FindByToken(token string) (*model.User, error) {
	c.findByTokenCalls++
	return c.MockUserRepository.FindByToken(token)
}

func TestAuthenticate_TokenCacheAvoidsDBOnRepeatedHit(t *testing.T) {
	repo := &countingUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	tokenRepo := testutil.NewMockAccessTokenRepository()
	user := &model.User{ID: "u_cache_1", Username: "cacheuser"}
	const tok = "cache-tok-aaaa"
	repo.Tokens[tok] = user

	auth := NewAuthMiddleware(repo, tokenRepo)
	e := echo.New()
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = auth.Authenticate()(func(c echo.Context) error {
			u := GetUser(c)
			assert.NotNil(t, u)
			assert.Equal(t, "u_cache_1", u.ID)
			return c.String(http.StatusOK, "ok")
		})(c)
		return rec
	}

	for i := 0; i < 5; i++ {
		assert.Equal(t, http.StatusOK, send().Code)
	}
	assert.Equal(t, 1, repo.findByTokenCalls,
		"FindByToken should be called only on the cache miss; subsequent hits use the in-memory cache")
}

// orphaned access_token (User 削除済み) は anonymous request 扱いに落ちて
// dereference panic を起こさないこと (Devin #514 FLAG-1)。
func TestAuthenticate_OrphanedAccessTokenFallsBackToAnonymous(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	const tok = "orphan-tok"
	tokenRepo.Tokens[sha256Hash(tok)] = &model.AccessToken{
		ID: "at_orphan",
		// User intentionally nil to simulate the orphaned row.
	}

	auth := NewAuthMiddleware(repo, tokenRepo)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	// 旧実装ではここで panic していた。Authenticate が anonymous で next に
	// 渡すこと、ハンドラ内 GetUser が nil を返すことを確認する。
	require.NotPanics(t, func() {
		err := auth.Authenticate()(func(c echo.Context) error {
			assert.Nil(t, GetUser(c), "orphaned access_token must not produce a user")
			return c.String(http.StatusOK, "ok")
		})(c)
		assert.NoError(t, err)
	})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_TokenCacheNotFoundIsNotPoisoned(t *testing.T) {
	repo := &countingUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(repo, tokenRepo)

	send := func() {
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer ghost-tok")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = auth.Authenticate()(func(c echo.Context) error {
			assert.Nil(t, GetUser(c))
			return c.String(http.StatusOK, "ok")
		})(c)
	}

	send()
	send()
	// 不在 token は cache に積まないので 2 回とも DB を引くこと。
	assert.Equal(t, 2, repo.findByTokenCalls,
		"not-found tokens must not be cached so token revoke / DB recovery is reflected immediately")
}

func TestAuthenticate_InvalidToken(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer invalidtoken")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		t.Fatal("無効 token では handler に到達しない")
		return nil
	})

	// 無効 (= 解決不能) token は upstream ApiCallService と同じく 401
	// AUTHENTICATION_FAILED を返す (#1559)。
	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "AUTHENTICATION_FAILED", errObj["code"])
	assert.Equal(t, "b0a7f5f8-dc2f-4171-b91f-de88ad238e14", errObj["id"])
}

func TestRequireAuth_Authenticated(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	user := &model.User{ID: "user1", Username: "test"}
	c.Set(string(UserContextKey), user)

	handler := RequireAuth()(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireAuth_Unauthenticated(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := RequireAuth()(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthenticate_AccessToken(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	user := &model.User{ID: "user3", Username: "tokenuser"}
	// native tokenには登録しない→access tokenのhashで検索される
	// SHA256("access_secret") = 予め計算したhash
	accessSecret := "access_secret"
	hash := sha256Hash(accessSecret)
	tokenRepo.Tokens[hash] = &model.AccessToken{
		ID:     "at1",
		Hash:   hash,
		UserID: user.ID,
		User:   user,
	}

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+accessSecret)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		u := GetUser(c)
		assert.NotNil(t, u)
		assert.Equal(t, "user3", u.ID)
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
}

// TestAuthenticate_AppIssuedAccessToken は #910 drift fix の regression guard:
// auth/accept は hash = sha256(token + app.secret) で保存するため、
// middleware が hash 列だけで lookup すると 401 になる。token (raw) 列での
// fallback (FindByHashOrToken) で resolve されることを確認する。
func TestAuthenticate_AppIssuedAccessToken(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	user := &model.User{ID: "user_app_1", Username: "appuser"}
	rawToken := "raw_app_token_xyz"
	// app/auth flow と同じく hash は raw token そのものではなく
	// raw + secret の sha256 で別値。middleware の sha256(rawToken) は
	// 一致しないので、token 列 fallback でしか resolve できない。
	hashWithSecret := sha256Hash(rawToken + "app_secret_value")
	tokenRepo.Tokens[hashWithSecret] = &model.AccessToken{
		ID:     "at_app_1",
		Token:  rawToken,
		Hash:   hashWithSecret,
		UserID: user.ID,
		User:   user,
	}

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		u := GetUser(c)
		assert.NotNil(t, u)
		assert.Equal(t, "user_app_1", u.ID)
		return c.String(http.StatusOK, "ok")
	})

	require.NoError(t, handler(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_JSONBodyToken(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	user := &model.User{ID: "user4", Username: "bodyuser"}
	nativeToken := "bodytoken1234567"
	userRepo.Tokens[nativeToken] = user

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	body := `{"i":"` + nativeToken + `","text":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		u := GetUser(c)
		assert.NotNil(t, u)
		assert.Equal(t, "user4", u.ID)
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
}

// TestAuthenticate_JSONBodyToken_BOMPrefix は UTF-8 BOM 付き JSON body の
// token 抽出を確認する (#1609)。本家は secure-json-parse が BOM を除去して
// から body.i を読むため認証が成功する。encoding/json は BOM を受理しない
// ので、除去せず Unmarshal すると匿名扱いに落ちて本家とずれる。
func TestAuthenticate_JSONBodyToken_BOMPrefix(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	user := &model.User{ID: "user5", Username: "bomuser"}
	nativeToken := "bomtoken12345678"
	userRepo.Tokens[nativeToken] = user

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	body := "\xEF\xBB\xBF" + `{"i":"` + nativeToken + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/i", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		u := GetUser(c)
		assert.NotNil(t, u, "BOM-prefixed body token must authenticate")
		assert.Equal(t, "user5", u.ID)
		return c.String(http.StatusOK, "ok")
	})

	require.NoError(t, handler(c))
}

func TestAuthenticate_JSONBodyToken_EmptyI(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"i":""}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		assert.Nil(t, GetUser(c))
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
}

func TestAuthenticate_JSONBodyToken_NoIField(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"text":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		assert.Nil(t, GetUser(c))
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
}

func TestAuthenticate_JSONBodyToken_InvalidJSON(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		assert.Nil(t, GetUser(c))
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
}

func TestAuthenticate_BearerTakesPrecedenceOverBody(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()

	bearerUser := &model.User{ID: "bearer1", Username: "beareruser"}
	userRepo.Tokens["bearertoken12345"] = bearerUser

	bodyUser := &model.User{ID: "body1", Username: "bodyuser"}
	userRepo.Tokens["bodytoken1234567"] = bodyUser

	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	body := `{"i":"bodytoken1234567"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer bearertoken12345")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := auth.Authenticate()(func(c echo.Context) error {
		u := GetUser(c)
		assert.NotNil(t, u)
		// Bearer header が body より優先
		assert.Equal(t, "bearer1", u.ID)
		return c.String(http.StatusOK, "ok")
	})

	err := handler(c)
	assert.NoError(t, err)
}

// --- RequireAdmin / RequireModerator tests ---

type stubRoleChecker struct {
	admin     bool
	moderator bool
}

func (s *stubRoleChecker) IsAdministrator(_ string) bool { return s.admin }
func (s *stubRoleChecker) IsModerator(_ string) bool     { return s.moderator }

func TestRequireAdmin_Success(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(UserContextKey), &model.User{ID: "u1"})

	handler := RequireAdmin(&stubRoleChecker{admin: true})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireAdmin_NotAdmin(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(UserContextKey), &model.User{ID: "u1"})

	handler := RequireAdmin(&stubRoleChecker{admin: false})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	// #1775: upstream ApiCallService requireAdmin gate の UUID と一致させる。
	assert.Contains(t, rec.Body.String(), "ROLE_PERMISSION_DENIED")
	assert.Contains(t, rec.Body.String(), "c3d38592-54c0-429d-be96-5636b0431a61")
}

func TestRequireAdmin_NoUser(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := RequireAdmin(&stubRoleChecker{})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRequireModerator_Success(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(UserContextKey), &model.User{ID: "u1"})

	handler := RequireModerator(&stubRoleChecker{moderator: true})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireModerator_NotModerator(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(UserContextKey), &model.User{ID: "u1"})

	handler := RequireModerator(&stubRoleChecker{moderator: false})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	// #1775: moderator gate は admin gate (c3d38592-…) とは別の upstream UUID。
	assert.Contains(t, rec.Body.String(), "ROLE_PERMISSION_DENIED")
	assert.Contains(t, rec.Body.String(), "d33d5333-db36-423d-a8f9-1a2b9549da41")
}

func TestRequireModerator_NoUser(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := RequireModerator(&stubRoleChecker{})(func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	err := handler(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestGetUser_NoUser(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	user := GetUser(c)
	assert.Nil(t, user)
}

func TestRequireSecure_NativeTokenAllowed(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
	nat := "nativetoken1234"
	c.Set(string(UserContextKey), &model.User{ID: "u1", Token: &nat})
	c.Set(string(TokenContextKey), nat)
	err := RequireSecure()(func(c echo.Context) error { return c.String(http.StatusOK, "ok") })(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, c.Response().Status)
}

func TestRequireSecure_AppTokenDenied(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)
	nat := "nativetoken1234"
	// access token (app/MiAuth): user.Token (native) と request token が異なる
	c.Set(string(UserContextKey), &model.User{ID: "u1", Token: &nat})
	c.Set(string(TokenContextKey), "apptoken_different")
	err := RequireSecure()(func(c echo.Context) error { return c.String(http.StatusOK, "ok") })(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
}

func TestRequireSecure_Anonymous(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)
	err := RequireSecure()(func(c echo.Context) error { return c.String(http.StatusOK, "ok") })(c)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// --- RequireScope (OAuth kind enforcement, #1552) ----------------------------

// newScopeCtx builds an echo context with the given AuthScope pre-set (nil
// leaves it unset = unauthenticated/anonymous).
func newScopeCtx(scope *AuthScope) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)
	if scope != nil {
		c.Set(string(AuthScopeContextKey), scope)
	}
	return c, rec
}

func TestRequireScope_NativeTokenPasses(t *testing.T) {
	// native login token (IsApp=false) は scope 無検査で full access。
	c, rec := newScopeCtx(&AuthScope{IsApp: false})
	called := false
	h := RequireScope("write:admin:queue")(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.True(t, called, "native token は通過する")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireScope_AnonymousPasses(t *testing.T) {
	// AuthScope 未設定 (anonymous) は RequireScope では通過させ、anonymous の
	// 拒否は companion の RequireModerator/RequireAuth に委ねる。
	c, rec := newScopeCtx(nil)
	called := false
	h := RequireScope("read:admin:queue")(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireScope_AppTokenWithKindPasses(t *testing.T) {
	c, rec := newScopeCtx(&AuthScope{IsApp: true, Scopes: []string{"read:account", "write:admin:queue"}})
	called := false
	h := RequireScope("write:admin:queue")(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.True(t, called, "kind を持つ app token は通過する")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireScope_AppTokenWithoutKindDenied(t *testing.T) {
	// app token が endpoint の kind を持たない → 403 PERMISSION_DENIED。
	c, rec := newScopeCtx(&AuthScope{IsApp: true, Scopes: []string{"read:account"}})
	called := false
	h := RequireScope("write:admin:queue")(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.False(t, called, "kind を持たない app token は handler に到達しない")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "PERMISSION_DENIED", errObj["code"])
	assert.Equal(t, "1370e5b7-d4eb-4566-bb1d-7748ee6a1838", errObj["id"])
	assert.Equal(t, "permission", errObj["kind"])
}

func TestRequireScope_AppTokenEmptyScopesDenied(t *testing.T) {
	// permission 空の app token も拒否 (isApp=true なので full access ではない)。
	c, rec := newScopeCtx(&AuthScope{IsApp: true, Scopes: nil})
	h := RequireScope("read:admin:queue")(func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestAuthenticate_AppTokenScopesPropagated は Authenticate が app token の
// permission を AuthScope として context に伝播することを end-to-end で確認する。
func TestAuthenticate_AppTokenScopesPropagated(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	appUser := &model.User{ID: "u1", Username: "app"}
	rawToken := "app-token-xyz"
	tokenRepo.Tokens[rawToken] = &model.AccessToken{
		Token:      rawToken,
		Permission: model.StringArray{"read:admin:queue"},
		User:       appUser,
	}
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	c := e.NewContext(req, httptest.NewRecorder())

	handler := auth.Authenticate()(func(c echo.Context) error {
		sc := GetAuthScope(c)
		require.NotNil(t, sc)
		assert.True(t, sc.IsApp, "app token は IsApp=true")
		assert.Equal(t, []string{"read:admin:queue"}, sc.Scopes)
		return c.String(http.StatusOK, "ok")
	})
	require.NoError(t, handler(c))
}

// TestAuthenticate_NativeTokenIsNotApp は native login token が IsApp=false /
// scopes nil で伝播することを確認する (= full access)。
func TestAuthenticate_NativeTokenIsNotApp(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	nativeUser := &model.User{ID: "u2", Username: "native"}
	rawToken := "native-token-abc"
	userRepo.Tokens[rawToken] = nativeUser
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	c := e.NewContext(req, httptest.NewRecorder())

	handler := auth.Authenticate()(func(c echo.Context) error {
		sc := GetAuthScope(c)
		require.NotNil(t, sc)
		assert.False(t, sc.IsApp, "native token は IsApp=false")
		assert.Nil(t, sc.Scopes)
		return c.String(http.StatusOK, "ok")
	})
	require.NoError(t, handler(c))
}

// --- RequireNotMoved (#1562 prohibitMoved) ---

func newMovedCtx(user *model.User) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)
	if user != nil {
		c.Set(string(UserContextKey), user)
	}
	return c, rec
}

func TestRequireNotMoved_NormalUserPasses(t *testing.T) {
	c, rec := newMovedCtx(&model.User{ID: "u1"})
	called := false
	h := RequireNotMoved()(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireNotMoved_MovedUserRejected(t *testing.T) {
	moved := "https://other.example/users/xyz"
	c, rec := newMovedCtx(&model.User{ID: "u1", MovedToURI: &moved})
	called := false
	h := RequireNotMoved()(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.False(t, called, "moved account must not reach the handler")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "YOUR_ACCOUNT_MOVED")
	assert.Contains(t, rec.Body.String(), "56f20ec9-fd06-4fa5-841b-edd6d7d4fa31")
	assert.Contains(t, rec.Body.String(), `"kind":"permission"`)
}

func TestRequireNotMoved_EmptyMovedURIPasses(t *testing.T) {
	// movedToUri が空文字の行 (NULL でなく '' で入った過去データ) は moved
	// 扱いしない。
	empty := ""
	c, rec := newMovedCtx(&model.User{ID: "u1", MovedToURI: &empty})
	called := false
	h := RequireNotMoved()(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRequireNotMoved_AnonymousPasses(t *testing.T) {
	// user 未設定 (anonymous) は素通しし、認可は RequireAuth に任せる。
	c, rec := newMovedCtx(nil)
	called := false
	h := RequireNotMoved()(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	require.NoError(t, h(c))
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1559 無効 token の 401 AUTHENTICATION_FAILED 応答に WWWAuthenticate
// middleware が WWW-Authenticate: error=invalid_token を付ける (full chain)。
func TestAuthenticate_InvalidToken_WWWAuthenticateHeader(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	tokenRepo := testutil.NewMockAccessTokenRepository()
	auth := NewAuthMiddleware(userRepo, tokenRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", nil)
	req.Header.Set("Authorization", "Bearer bogustoken")
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	chained := WWWAuthenticate()(auth.Authenticate()(func(c echo.Context) error {
		t.Fatal("無効 token では handler に到達しない")
		return nil
	}))
	require.NoError(t, chained(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`)
}

// #1559 凍結 user は RequireSecure / RequireAdmin / RequireModerator でも
// 403 YOUR_ACCOUNT_SUSPENDED を返す (credential 必須 gate 共通)。
func TestSuspendedUser_403OnAllCredentialGates(t *testing.T) {
	gates := map[string]echo.MiddlewareFunc{
		"RequireAuth":      RequireAuth(),
		"RequireSecure":    RequireSecure(),
		"RequireAdmin":     RequireAdmin(&stubRoleChecker{admin: true}),
		"RequireModerator": RequireModerator(&stubRoleChecker{moderator: true}),
	}
	for name, gate := range gates {
		t.Run(name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			tokenRepo := testutil.NewMockAccessTokenRepository()
			token := "frozen-token-" + name
			userRepo.Tokens[token] = &model.User{ID: "u_s", Username: "frozen", IsSuspended: true}
			auth := NewAuthMiddleware(userRepo, tokenRepo)

			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			chained := auth.Authenticate()(gate(func(c echo.Context) error {
				t.Fatal("凍結 user は gate を通過しない")
				return nil
			}))
			require.NoError(t, chained(c))
			assert.Equal(t, http.StatusForbidden, rec.Code)
			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, "YOUR_ACCOUNT_SUSPENDED", resp["error"].(map[string]any)["code"])
		})
	}
}

// 未認証 (token 無し) は従来どおり 401 CREDENTIAL_REQUIRED (凍結 flag 無し)。
func TestCredentialRequired_NoToken(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	handler := RequireAuth()(func(c echo.Context) error { return nil })
	require.NoError(t, handler(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "CREDENTIAL_REQUIRED", resp["error"].(map[string]any)["code"])
}

// **`kind` を宣言できない経路では app token を一律で拒否する (#3037)。**
// upstream `ApiCallService.ts:412-413` の後半 (`!ep.meta.kind &&
// requireCredential`) に対応する。
func TestRejectAppToken(t *testing.T) {
	for _, tt := range []struct {
		name     string
		scope    *AuthScope
		wantCode int
		wantNext bool
	}{
		{"app token は拒否", &AuthScope{IsApp: true, Scopes: []string{"read:account"}}, http.StatusForbidden, false},
		// **scope を全部持っていても拒否する。** upstream も permission の
		// 中身は見ずに「app token であること」だけで落とす。
		{"scope を持つ app token も拒否", &AuthScope{IsApp: true, Scopes: []string{"write:account", "read:account"}}, http.StatusForbidden, false},
		{"native token は通す", &AuthScope{IsApp: false}, http.StatusOK, true},
		{"未認証は通す", nil, http.StatusOK, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/api/plugin/x/y", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			if tt.scope != nil {
				c.Set(string(AuthScopeContextKey), tt.scope)
			}

			called := false
			h := RejectAppToken()(func(echo.Context) error {
				called = true
				return c.String(http.StatusOK, "ok")
			})

			require.NoError(t, h(c))
			assert.Equal(t, tt.wantCode, rec.Code)
			assert.Equal(t, tt.wantNext, called)
		})
	}
}

// 応答は `RequireScope` の拒否と同じ shape にする (クライアントが
// insufficient_scope として扱えるように)。
func TestRejectAppToken_ResponseShapeMatchesRequireScope(t *testing.T) {
	body := func(mw echo.MiddlewareFunc) string {
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodPost, "/api/x", nil), rec)
		c.Set(string(AuthScopeContextKey), &AuthScope{IsApp: true, Scopes: []string{"read:account"}})
		require.NoError(t, mw(func(echo.Context) error { return nil })(c))
		return rec.Body.String()
	}

	assert.JSONEq(t, body(RequireScope("write:account")), body(RejectAppToken()))
}

// 保存値は char(16) なので 16 文字未満の token は末尾が空白で埋まって返る。
// 埋め草は落として比べ、リクエスト側の空白は落とさない。
func TestNativeTokenMatches(t *testing.T) {
	s := func(v string) *string { return &v }
	assert.True(t, nativeTokenMatches(s("abcdef1234567890"), "abcdef1234567890"))
	assert.False(t, nativeTokenMatches(s("abcdef1234567890"), "abcdef1234567890 "))
	assert.True(t, nativeTokenMatches(s("short           "), "short"), "char の埋め草は無視する")
	assert.False(t, nativeTokenMatches(s("short           "), "short "))
	assert.False(t, nativeTokenMatches(s("abcdef1234567890"), "other"))
	assert.True(t, nativeTokenMatches(nil, "x"), "token 列を持たない mock は照合できないので通す")
}

func TestIsInactiveAccountRequest(t *testing.T) {
	e := echo.New()
	newCtx := func() echo.Context {
		return e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	}
	assert.False(t, IsInactiveAccountRequest(newCtx()))
	c := newCtx()
	c.Set(string(suspendedContextKey), true)
	assert.True(t, IsInactiveAccountRequest(c))
	c = newCtx()
	c.Set(string(deletedContextKey), true)
	assert.True(t, IsInactiveAccountRequest(c))
}
