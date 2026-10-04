package signin_test

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/core/twofactor"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// redisClientFromTest opens a fresh redis client pointing at the same
// testcontainer used by the package, useful for "closed redis" failure paths.
func redisClientFromTest(t *testing.T) *redis.Client {
	t.Helper()
	if signinTestRedis == nil {
		t.Fatalf("redis testcontainer required")
	}
	return redis.NewClient(&redis.Options{Addr: signinTestRedis.Client.Options().Addr})
}

var signinTestRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Printf("api/signin: redis testcontainer unavailable: %v", err)
		os.Exit(m.Run())
	}
	signinTestRedis = tr
	code := m.Run()
	signinTestRedis.Teardown(ctx)
	os.Exit(code)
}

// helpers

func newTestUserWithTOTP(repo *testutil.MockUserRepository, username, password, totpSecret string, backupCodes []string) *model.User {
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	user := createTestUserWithStoredPassword(repo, username, string(hash))
	prof := repo.Profiles[user.ID]
	prof.TwoFactorEnabled = true
	prof.TwoFactorSecret = &totpSecret
	if backupCodes != nil {
		prof.TwoFactorBackupSecret = model.StringArray(backupCodes)
	}
	return user
}

// inMemorySK is a no-DB implementation of UserSecurityKeyRepository for the
// signin tests that exercise the WebAuthn assertion path. We only need
// ListByUser + UpdateCounter; everything else is no-op.
type inMemorySK struct {
	keys map[string][]*model.UserSecurityKey
}

func (r *inMemorySK) Create(_ *model.UserSecurityKey) error { return nil }
func (r *inMemorySK) FindByID(id string) (*model.UserSecurityKey, error) {
	for _, keys := range r.keys {
		for _, k := range keys {
			if k.ID == id {
				return k, nil
			}
		}
	}
	return nil, testutil.ErrNotFound
}
func (r *inMemorySK) ListByUser(userID string) ([]*model.UserSecurityKey, error) {
	return r.keys[userID], nil
}
func (r *inMemorySK) UpdateName(_, _, _ string) error       { return nil }
func (r *inMemorySK) UpdateCounter(_ string, _ int64) error { return nil }
func (r *inMemorySK) Delete(_, _ string) error              { return nil }
func (r *inMemorySK) DeleteByUser(_ string) error           { return nil }
func (r *inMemorySK) CountByUser(_ string) (int64, error)   { return 0, nil }

var _ repository.UserSecurityKeyRepository = (*inMemorySK)(nil)

// --- TOTP ---

func Test2FA_TOTP_Step3_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	// 既知の TOTP secret を使う
	secret := "JBSWY3DPEHPK3PXP" // base32 "Hello!"
	newTestUserWithTOTP(repo, "alice", "pass", secret, nil)

	// Step 2: パスワードを送ると 2FA が必要と告げられる
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var step2 map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &step2))
	assert.Equal(t, false, step2["finished"])
	assert.Equal(t, "totp", step2["next"])

	// Step 3: 有効な TOTP token を送ると finished
	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	rec2 := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, rec2.Code)
	var step3 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &step3))
	assert.Equal(t, true, step3["finished"])
}

func Test2FA_TOTP_AcceptsArgon2Password(t *testing.T) {
	h, repo := newTestHandler(t)
	secret := "JBSWY3DPEHPK3PXP"
	user := newTestUserWithTOTP(repo, "alice", "unused", secret, nil)
	stored := signinArgon2Fixture("argon-pass")
	repo.Profiles[user.ID].Password = &stored
	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"argon-pass","token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func Test2FA_Argon2MigrationWaitsForTOTP(t *testing.T) {
	h, repo := newTestHandler(t)
	secret := "JBSWY3DPEHPK3PXP"
	user := newTestUserWithTOTP(repo, "alice", "unused", secret, []string{"backup1"})
	stored := signinArgon2Fixture("argon-pass")
	repo.Profiles[user.ID].Password = &stored

	step2 := doPost(h.SigninFlow, `{"username":"alice","password":"argon-pass"}`)
	require.Equal(t, http.StatusOK, step2.Code)
	assert.Equal(t, stored, *repo.Profiles[user.ID].Password)

	bad := doPost(h.SigninFlow, `{"username":"alice","password":"argon-pass","token":"000000"}`)
	require.Equal(t, http.StatusForbidden, bad.Code)
	assert.Equal(t, stored, *repo.Profiles[user.ID].Password)

	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	good := doPost(h.SigninFlow, `{"username":"alice","password":"argon-pass","token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, good.Code, good.Body.String())
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(*repo.Profiles[user.ID].Password), []byte("argon-pass")))
}

func Test2FA_Argon2MigrationAfterBackupCode(t *testing.T) {
	h, repo := newTestHandler(t)
	user := newTestUserWithTOTP(repo, "alice", "unused", "JBSWY3DPEHPK3PXP", []string{"backup1"})
	stored := signinArgon2Fixture("argon-pass")
	repo.Profiles[user.ID].Password = &stored

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"argon-pass","token":"backup1"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(*repo.Profiles[user.ID].Password), []byte("argon-pass")))
}

func Test2FA_TOTP_InvalidToken(t *testing.T) {
	h, repo := newTestHandler(t)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"000000"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// Test2FA_TOTP_ReplayRejected: 同じ TOTP コードを 2 回目以降に送ったときは
// acceptance window 内でも 403 で refuse されること (RFC 6238 §5.2)。
// Redis backed replay guard を wire した状態で検証する。
//
// test-unique な KeyPrefix を渡しているのは、共有 signinTestRedis に対して
// `-count=N` (N≥2) や他テストとの key 衝突で flaky になることを防ぐため。
// production の prefix (= "mk:2fa:totp:used") とは別空間で動かす。
func Test2FA_TOTP_ReplayRejected(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis testcontainer required")
	}
	h, repo := newTestHandler(t)
	secret := "JBSWY3DPEHPK3PXP"
	newTestUserWithTOTP(repo, "alice", "pass", secret, nil)
	h.SetTOTPReplayGuard(&twofactor.RedisReplayGuard{
		Client:    redisClientFromTest(t),
		KeyPrefix: "test:" + t.Name(),
	})

	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	// 1 回目は通る
	rec1 := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, rec1.Code)
	var step3 map[string]any
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &step3))
	assert.Equal(t, true, step3["finished"])

	// 2 回目は replay として 403
	rec2 := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"`+token+`"}`)
	assert.Equal(t, http.StatusForbidden, rec2.Code, "same TOTP code must not be reusable within acceptance window")
}

// Test2FA_TOTP_NilGuard_AllowsReplay は guard 未配線時の fail-open 挙動を guard する。
// production では必ず wire するが、unit test / dev 環境で nil でも回帰せず以前と同じ挙動を保つ。
func Test2FA_TOTP_NilGuard_AllowsReplay(t *testing.T) {
	h, repo := newTestHandler(t)
	secret := "JBSWY3DPEHPK3PXP"
	newTestUserWithTOTP(repo, "alice", "pass", secret, nil)
	// guard 未配線 (h.totpReplayGuard == nil)

	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	rec1 := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"`+token+`"}`)
	assert.Equal(t, http.StatusOK, rec1.Code)
	// nil guard では replay protection 無しなので 2 回目も通る (= 後方互換 fallback)
	rec2 := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"`+token+`"}`)
	assert.Equal(t, http.StatusOK, rec2.Code)
}

// --- Backup codes ---

func Test2FA_BackupCode_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	codes := []string{"abcd1234", "efgh5678"}
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", codes)

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"abcd1234"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["finished"])

	// 使用済みコードが消えている (single-use)
	remaining := repo.Profiles["u1"].TwoFactorBackupSecret
	assert.Equal(t, []string{"efgh5678"}, []string(remaining))
}

func Test2FA_BackupCode_AllInvalid(t *testing.T) {
	h, repo := newTestHandler(t)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", []string{"valid"})
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"wrong"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// --- WebAuthn assertion (no keys → fallback to TOTP) ---

func Test2FA_WebAuthnNoKeys_FallsBackToTOTP(t *testing.T) {
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	skRepo := &inMemorySK{keys: map[string][]*model.UserSecurityKey{}}
	h.SetWebAuthn(svc, skRepo)

	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	// パスワードのみ → TOTP step を返す (security key 無し)
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "totp", resp["next"])
}

func Test2FA_WebAuthnWithKeys_ReturnsAssertionChallenge(t *testing.T) {
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	skRepo := &inMemorySK{
		keys: map[string][]*model.UserSecurityKey{
			"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
		},
	}
	h.SetWebAuthn(svc, skRepo)

	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// TS upstream の SigninFlowResponse: `next: 'passkey'` + `authRequest` (#705)
	assert.Equal(t, "passkey", resp["next"])
	assert.Nil(t, resp["sessionId"], "sessionId must not be returned (server keeps challenge keyed by userId)")
	authRequest, ok := resp["authRequest"].(map[string]any)
	require.True(t, ok, "authRequest must be a JSON object")
	// PublicKeyCredentialRequestOptions (TS の PublicKeyCredentialRequestOptionsJSON 互換)
	// は challenge をトップレベルに持つ。`{publicKey: ...}` ラッパーがあると壊れる。
	assert.NotEmpty(t, authRequest["challenge"])
	assert.Nil(t, authRequest["publicKey"], "must not have a publicKey wrapper")
}

func Test2FA_WebAuthnFinishLogin_BadCredential(t *testing.T) {
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	skRepo := &inMemorySK{
		keys: map[string][]*model.UserSecurityKey{
			"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
		},
	}
	h.SetWebAuthn(svc, skRepo)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)

	// challenge を一切作らずに credential だけ送ると session not found で 403
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","credential":{"id":"x","rawId":"x","type":"public-key","response":{}}}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// Step 1 で 2FA 無効ユーザは TS upstream と同じく常に 'captcha' を返す (#705)。
// captcha 設定の有無に関わらず、フロントが instance meta で widget の表示要否を
// 判定するため。
func TestStep1_NonTwoFactorUser_ReturnsCaptcha(t *testing.T) {
	h, repo := newTestHandler(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &hashStr}

	rec := doPost(h.SigninFlow, `{"username":"alice"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["finished"])
	assert.Equal(t, "captcha", resp["next"])
}

func TestStep1_TwoFactorUser_ReturnsPassword(t *testing.T) {
	h, repo := newTestHandler(t)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	rec := doPost(h.SigninFlow, `{"username":"alice"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "password", resp["next"])
}

// TOTP 失敗時のエラー ID は `cdf1235b-...` (TS 専用)。`932c904e-...`
// (パスワード違い) と区別される (#705)。
func Test2FA_TOTP_FailureErrorID(t *testing.T) {
	h, repo := newTestHandler(t)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":"000000"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errMap, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "cdf1235b-ac71-46d4-a3a6-84ccce48df6f", errMap["id"])
}

// security key 設定済みだが credential 検証で失敗 → upstream 互換の WebAuthn
// 専用 ID `93b86c4b-...` を返す (#705)。
func Test2FA_WebAuthnFailureErrorID(t *testing.T) {
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	skRepo := &inMemorySK{
		keys: map[string][]*model.UserSecurityKey{
			"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
		},
	}
	h.SetWebAuthn(svc, skRepo)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","credential":{"id":"x","rawId":"x","type":"public-key","response":{}}}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "93b86c4b-72f9-40eb-9815-798928603d1e", errMap["id"])
}

// credential を送ったが鍵が無いユーザは passwordless でも通常でも 403 を返す。
// (signin-flow path で webauthnSvc が nil または鍵未登録のケース)
func Test2FA_WebAuthn_NoKeysButCredentialSent(t *testing.T) {
	h, repo := newTestHandler(t)
	// webauthnSvc を注入しない → hasKeys=false で 403
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","credential":{"id":"x","rawId":"x","type":"public-key","response":{}}}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "93b86c4b-72f9-40eb-9815-798928603d1e", errMap["id"])
}

// 2FA + key 経路で BeginLogin が失敗 (Redis 接続エラー) すると TOTP に
// fallback する: `next: 'totp'` を返す。
func Test2FA_BeginLogin_Fail_FallsBackToTOTP(t *testing.T) {
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	// 専用 redis client を閉じておく
	closedClient := redisClientFromTest(t)
	require.NoError(t, closedClient.Close())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", closedClient)
	require.NoError(t, err)
	skRepo := &inMemorySK{
		keys: map[string][]*model.UserSecurityKey{
			"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
		},
	}
	h.SetWebAuthn(svc, skRepo)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "totp", resp["next"])
}

// 2FA + key + 不正パスワード + passwordless 無効 → 403。
func Test2FA_WebAuthn_BadPassword_NoPasswordless(t *testing.T) {
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	skRepo := &inMemorySK{
		keys: map[string][]*model.UserSecurityKey{
			"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
		},
	}
	h.SetWebAuthn(svc, skRepo)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"WRONG","credential":{"id":"x","rawId":"x","type":"public-key","response":{}}}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "932c904e-9460-45b7-9ce6-7ed33be7eb2c", errMap["id"])
}

// SigninWithPasskey の Step 1 で BeginPasskeyLogin が失敗する path
// (closed Redis) → 500。
func TestSigninWithPasskey_BeginFails(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	h, _ := newTestHandler(t)
	closedClient := redisClientFromTest(t)
	require.NoError(t, closedClient.Close())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", closedClient)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})

	rec := doPost(h.SigninWithPasskey, `{}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// #2106 L20: usePasswordLessLogin=true なら signin-flow で誤パスワードでも passkey
// challenge を発行する (upstream SigninApiService と同じく same || usePasswordLessLogin)。
func Test2FA_WebAuthnPasswordless_WrongPassword_StillChallenges(t *testing.T) {
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	skRepo := &inMemorySK{
		keys: map[string][]*model.UserSecurityKey{
			"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
		},
	}
	h.SetWebAuthn(svc, skRepo)

	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	repo.Profiles["u1"].UsePasswordLessLogin = true

	// 誤パスワードでも passkey challenge (next:passkey) が返る。
	rec := doPost(h.SigninFlow, `{"username":"alice","password":"wrong"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "passkey", resp["next"], "usePasswordLessLogin なら誤パスワードでも challenge を発行")
}
