package signin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/signin"
	"github.com/shiroha-a/mk/internal/core/twofactor"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// /api/signin-with-passkey の init 段 (credential 無し) は challenge を含む
// PublicKeyCredentialRequestOptions と一回限りの context を返す (#705)。
func TestSigninWithPasskey_Init(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	h, _ := newTestHandler(t)
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})

	rec := doPost(h.SigninWithPasskey, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	option, ok := resp["option"].(map[string]any)
	require.True(t, ok, "option must be a JSON object")
	// upstream 互換: option はそのまま PublicKeyCredentialRequestOptions
	// (no `publicKey` wrapper)。フロントは parseRequestOptionsFromJSON で読む。
	assert.NotEmpty(t, option["challenge"])
	assert.Nil(t, option["publicKey"])
	context, ok := resp["context"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, context)
}

// init 2 回目: 別 context を返す (single-use)。
func TestSigninWithPasskey_InitTwice_DistinctContext(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	h, _ := newTestHandler(t)
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})

	rec1 := doPost(h.SigninWithPasskey, `{}`)
	rec2 := doPost(h.SigninWithPasskey, `{}`)
	var r1, r2 map[string]any
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &r1))
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &r2))
	assert.NotEqual(t, r1["context"], r2["context"])
}

// WebAuthnService が未注入のときは 503 を返す (frontend に「passkey 経由は
// 使えない」と伝える)。
func TestSigninWithPasskey_NotConfigured(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.SigninWithPasskey, `{}`)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// credential を送るが context が空の場合は 400 + 専用エラー ID。
func TestSigninWithPasskey_VerifyMissingContext(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	h, _ := newTestHandler(t)
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})

	body := `{"credential":{"id":"x","rawId":"x","type":"public-key","response":{}}}`
	rec := doPost(h.SigninWithPasskey, body)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errMap, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "1658cc2e-4495-461f-aee4-d403cdf073c1", errMap["id"])
}

// 不正な credential body は signature verify が走る前に 403 を返す。
func TestSigninWithPasskey_VerifyBadCredential(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	h, _ := newTestHandler(t)
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})

	body := `{"context":"00112233445566778899aabbccddeeff","credential":{"id":"x","rawId":"x","type":"public-key","response":{}}}`
	rec := doPost(h.SigninWithPasskey, body)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// JSON body が壊れている場合は、本番では handler の前で JSONBodyParse が
// Fastify の FST_ERR_CTP_INVALID_JSON_BODY を返す (本家も handler の前で
// body を parse する)。handler を直接呼んだときは 400 の HTTPError を返し、
// 本家に無い独自の id (`ed1d7571-…`) の本文は書かない (#3330)。
func TestSigninWithPasskey_BadBody(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := doPost(middleware.JSONBodyParse()(h.SigninWithPasskey), `not-json`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"FST_ERR_CTP_INVALID_JSON_BODY"`)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/signin-with-passkey", strings.NewReader(`not-json`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	direct := httptest.NewRecorder()
	err := h.SigninWithPasskey(e.NewContext(req, direct))
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusBadRequest, he.Code)
	assert.NotContains(t, direct.Body.String(), "ed1d7571")
}

// Step 1 中に entropy 枯渇 → 500。readRandom seam を差し替える。
func TestSigninWithPasskey_NewContextRandError(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	h, _ := newTestHandler(t)
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})

	old := signin.SwapReadRandomBytes(func(_ []byte) (int, error) { return 0, assert.AnError })
	// 戻り値 (差し替え前の関数) は要らない。呼ぶこと自体が復元。
	//nolint:staticcheck // SA9010: 副作用目的の呼び出しで、戻り値を呼ぶ形ではない
	defer signin.SwapReadRandomBytes(old)

	rec := doPost(h.SigninWithPasskey, `{}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// passwordless login が有効でないユーザーは認証成功しても 403 を返す
// (resolver が user を返してきても profile.usePasswordLessLogin が
// false の場合)。
//
// この path は実 webauthn 検証を通せないので、resolver 経由までは到達するが
// 必ず先に signature verify で fail する。互換性確認のための smoke のみ。
func TestSigninWithPasskey_PasswordlessNotEnabled_Fails(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	h, repo := newTestHandler(t)

	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})

	// usePasswordLessLogin=false のユーザー
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	repo.Users["uPLN"] = &model.User{ID: "uPLN", Username: "pln", UsernameLower: "pln"}
	repo.Profiles["uPLN"] = &model.UserProfile{
		UserID:               "uPLN",
		Password:             &hashStr,
		UsePasswordLessLogin: false,
	}

	body := `{"context":"00112233445566778899aabbccddeeff","credential":{"id":"x","rawId":"x","type":"public-key","response":{}}}`
	rec := doPost(h.SigninWithPasskey, body)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// **`context` は形まで見る (#3037)。**
//
// サーバーが発行した 16 バイト乱数の hex 以外を受け取る意味が無い。素通しすると
// この値が (a) Redis のキーの一部 (`twofa:webauthn:passkey:<context>`) になり、
// (b) 失敗時にそのままログへ出る。どちらも**未認証で任意長・任意バイト列**を
// 渡せる面。upstream (`SigninWithPasskeyApiService.ts:122`) も UUID 形式を
// 要求する。
func TestSigninWithPasskey_RejectsMalformedContext(t *testing.T) {
	h, _ := newTestHandler(t)
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})

	for _, tt := range []struct {
		name string
		ctx  string
	}{
		{"短い", "00112233"},
		{"長い", strings.Repeat("a", 33)},
		{"hex でない文字", "00112233445566778899aabbccddeegg"},
		{"大文字", "00112233445566778899AABBCCDDEEFF"},
		{"Redis のキー区切りを含む", "00112233445566778899aabbccdd:eef"},
		// 長大な値 (ログとキーの両方に効く)。
		{"極端に長い", strings.Repeat("0", 100000)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"context":"` + tt.ctx + `","credential":{"id":"x","rawId":"x","type":"public-key","response":{}}}`
			rec := doPost(h.SigninWithPasskey, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "形の違う context を受け取っている")
		})
	}
}

// **生成した形は通る。** これが無いと「常に 400」の実装でも上のテストが通る。
func TestValidPasskeyContext_AcceptsGenerated(t *testing.T) {
	for i := 0; i < 20; i++ {
		ctxID, err := signin.NewPasskeyContextForTest()
		require.NoError(t, err)
		assert.True(t, signin.ValidPasskeyContextForTest(ctxID), "生成した context を弾いている: %q", ctxID)
	}
}
