package i

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enableTwoFactorWithBackupCodes marks 2FA as enabled and registers a known
// backup code list so test callers can pass `"backup1"` as the 2FA token
// without dealing with TOTP (which depends on real time windows). For tests
// that need to exercise the TOTP code path specifically, use
// enableTwoFactorWithTOTP instead. (#698)
func enableTwoFactorWithBackupCodes(repo *testutil.MockUserRepository, uid string) {
	p := repo.Profiles[uid]
	p.TwoFactorEnabled = true
	p.TwoFactorBackupSecret = model.StringArray{"backup1", "backup2"}
}

// enableTwoFactorWithTOTP generates a fresh TOTP secret, stores it on the
// profile, and returns it so the caller can compute valid codes via
// totp.GenerateCode. backup codes are not populated, so the verify path is
// guaranteed to go through TOTP.Validate (no backup-code shortcut).
func enableTwoFactorWithTOTP(t *testing.T, repo *testutil.MockUserRepository, uid string) string {
	t.Helper()
	secret, _, err := twofactor.GenerateSecret("Misskey", uid)
	require.NoError(t, err)
	p := repo.Profiles[uid]
	p.TwoFactorEnabled = true
	p.TwoFactorSecret = &secret
	p.TwoFactorBackupSecret = nil
	return secret
}

// この test file は WebAuthn 関連 5 ハンドラの validation / wiring を網羅する。
// FinishRegistration / FinishLogin の本物の attestation 検証は
// internal/core/twofactor の test に任せる (W3C spec vector を使用)。
// ここでは:
//   - パラメータ検証 (空 body / password 欠落)
//   - 認証チェック (wrong password / no profile)
//   - WebAuthn 未注入時の 503
//   - register-key / remove-key / update-key / password-less の正常系 wiring
//
// 共通: TestMain で testcontainers の Redis を立ち上げて WebAuthnService を
// 注入できる状態にしておく。

var iTestRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Printf("api/i: redis testcontainer unavailable: %v", err)
		os.Exit(m.Run())
	}
	iTestRedis = tr
	code := m.Run()
	iTestRedis.Teardown(ctx)
	os.Exit(code)
}

// inMemorySecurityKeyRepo は最小限の repository.UserSecurityKeyRepository 実装。
// DB を立てずに handler のロジックを exercise したい用途。
type inMemorySecurityKeyRepo struct {
	keys map[string]*model.UserSecurityKey
}

func newInMemSKRepo() *inMemorySecurityKeyRepo {
	return &inMemorySecurityKeyRepo{keys: map[string]*model.UserSecurityKey{}}
}

func (r *inMemorySecurityKeyRepo) Create(k *model.UserSecurityKey) error {
	r.keys[k.ID] = k
	return nil
}

func (r *inMemorySecurityKeyRepo) FindByID(id string) (*model.UserSecurityKey, error) {
	if k, ok := r.keys[id]; ok {
		return k, nil
	}
	return nil, testutil.ErrNotFound
}

func (r *inMemorySecurityKeyRepo) ListByUser(userID string) ([]*model.UserSecurityKey, error) {
	var out []*model.UserSecurityKey
	for _, k := range r.keys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (r *inMemorySecurityKeyRepo) UpdateName(id, userID, name string) error {
	k, ok := r.keys[id]
	if !ok || k.UserID != userID {
		return testutil.ErrNotFound
	}
	k.Name = name
	return nil
}

func (r *inMemorySecurityKeyRepo) UpdateCounter(id string, counter int64) error {
	k, ok := r.keys[id]
	if !ok {
		return testutil.ErrNotFound
	}
	k.Counter = counter
	return nil
}

func (r *inMemorySecurityKeyRepo) DeleteByUser(userID string) error {
	for id, k := range r.keys {
		if k.UserID == userID {
			delete(r.keys, id)
		}
	}
	return nil
}

func (r *inMemorySecurityKeyRepo) Delete(id, userID string) error {
	k, ok := r.keys[id]
	if !ok || k.UserID != userID {
		return testutil.ErrNotFound
	}
	delete(r.keys, id)
	return nil
}

func (r *inMemorySecurityKeyRepo) CountByUser(userID string) (int64, error) {
	var n int64
	for _, k := range r.keys {
		if k.UserID == userID {
			n++
		}
	}
	return n, nil
}

// 静的に interface を満たすことを確認
var _ repository.UserSecurityKeyRepository = (*inMemorySecurityKeyRepo)(nil)

// newWebAuthnHandler builds an extra Handler with WebAuthn dependencies wired
// up against the testcontainer Redis instance.
func newWebAuthnHandler(t *testing.T) (*Handler, *testutil.MockUserRepository, *inMemorySecurityKeyRepo) {
	t.Helper()
	if iTestRedis == nil {
		t.Skip("redis testcontainer not available")
	}
	h, repo := newExtraHandler(t)
	skRepo := newInMemSKRepo()
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", iTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, skRepo)
	return h, repo, skRepo
}

// --- TwoFARegisterKey ---

func TestTwoFARegisterKey_NotConfigured(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFARegisterKey, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestTwoFARegisterKey_NoPassword(t *testing.T) {
	h, _, _ := newWebAuthnHandler(t)
	rec := postExtra(h.TwoFARegisterKey, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFARegisterKey_WrongPassword(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "correct")
	rec := postExtra(h.TwoFARegisterKey, `{"password":"wrong"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFARegisterKey_NoProfile(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "u1"}
	rec := postExtra(h.TwoFARegisterKey, `{"password":"pass"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFARegisterKey_TwoFactorNotEnabled(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	// 2FA 未有効化なら upstream と同じく TWO_FACTOR_NOT_ENABLED で 403
	rec := postExtra(h.TwoFARegisterKey, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TWO_FACTOR_NOT_ENABLED")
}

func TestTwoFARegisterKey_InvalidToken(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.TwoFARegisterKey, `{"password":"pass","token":"wrong"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
}

func TestTwoFARegisterKey_Success(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.TwoFARegisterKey, `{"password":"pass","token":"backup1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	// upstream 互換: PublicKeyCredentialCreationOptions そのものを返す
	// (challenge / rp / pubKeyCredParams が top-level に並ぶ)
	assert.Contains(t, rec.Body.String(), "challenge")
	assert.Contains(t, rec.Body.String(), "pubKeyCredParams")
	// sessionId / creation という旧フィールドが残っていないこと
	assert.NotContains(t, rec.Body.String(), "sessionId")
	// backup code が消費されている
	assert.Equal(t, model.StringArray{"backup2"}, repo.Profiles["u1"].TwoFactorBackupSecret)
}

func TestTwoFARegisterKey_TOTPSuccess(t *testing.T) {
	// TOTP 直接検証経路 (verify2FAToken の non-backup-code 分岐) のカバレッジを確保。
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	secret := enableTwoFactorWithTOTP(t, repo, "u1")
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	rec := postExtra(h.TwoFARegisterKey, `{"password":"pass","token":"`+code+`"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "challenge")
}

// Cross-endpoint replay regression guard: signin で消費した TOTP コードを
// TwoFARegisterKey で再利用しようとすると 403 INVALID_TOKEN で refuse される。
// router.go で signin と /api/i に同一 guard インスタンスを inject している
// ことが前提 (PR #1100)。本 test は guard を直接 MarkUsed して signin 消費
// を模擬し、shared-guard 経由の cross-endpoint protection を assert する。
func TestTwoFARegisterKey_TOTPReplayFromSignin_Blocked(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	secret := enableTwoFactorWithTOTP(t, repo, "u1")

	// test-unique KeyPrefix で他テスト / -count=N の汚染を回避。
	guard := &twofactor.RedisReplayGuard{
		Client:    iTestRedis.Client,
		KeyPrefix: "test:" + t.Name(),
	}
	h.SetTOTPReplayGuard(guard)

	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	// signin で TOTP を消費したことを模擬: guard を直接叩いて (u1, code) を
	// used にする。signin handler の ValidateWithReplay と同じ key shape。
	ok, err := guard.MarkUsed(t.Context(), "u1", code)
	require.NoError(t, err)
	require.True(t, ok, "first MarkUsed should set the slot")

	// 同じ TOTP コードを TwoFARegisterKey に送る → refuse されるべき。
	rec := postExtra(h.TwoFARegisterKey, `{"password":"pass","token":"`+code+`"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"TOTP code consumed by signin must be refused by TwoFARegisterKey (cross-endpoint replay protection)")
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
}

// --- TwoFAKeyDone ---

func TestTwoFAKeyDone_MissingFields(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFAKeyDone, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFAKeyDone_WrongPassword(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "correct")
	rec := postExtra(h.TwoFAKeyDone, `{"password":"wrong","credential":{}}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFAKeyDone_TwoFactorNotEnabled(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFAKeyDone, `{"password":"pass","credential":{"id":"x"}}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TWO_FACTOR_NOT_ENABLED")
}

func TestTwoFAKeyDone_InvalidToken(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.TwoFAKeyDone, `{"password":"pass","token":"wrong","credential":{"id":"x"}}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
}

func TestTwoFAKeyDone_FailureBranch(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	// 不正な attestation → registration session も無いので FinishRegistration が失敗 → 403
	rec := postExtra(h.TwoFAKeyDone, `{"password":"pass","token":"backup1","credential":{"id":"x","rawId":"x","type":"public-key","response":{"attestationObject":"","clientDataJSON":""}}}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestTwoFAKeyDone_NameTooLong(t *testing.T) {
	// upstream は paramDef で maxLength=30 を強制している。defense-in-depth で
	// API 直叩き経路でも backend 側が拒否することを確認。
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	longName := "this-name-is-far-longer-than-the-maximum-allowed-30-chars"
	body := `{"password":"pass","token":"backup1","name":"` + longName + `","credential":{"id":"x"}}`
	rec := postExtra(h.TwoFAKeyDone, body, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

// --- TwoFARemoveKey ---

func TestTwoFARemoveKey_MissingFields(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFARemoveKey, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1767: upstream remove-key は no-match でも no-op 成功 (204)。NO_SUCH_KEY は無い。
func TestTwoFARemoveKey_MissingIsNoOp(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFARemoveKey, `{"password":"pass","credentialId":"ghost"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// upstream の i/2fa/remove-key / update-key は `return {}` なので 200 + 空
// オブジェクト。204 ではない。
func TestTwoFARemoveKey_Success(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{
		ID: "key1", UserID: "u1", Name: "k", PublicKey: "pk",
	}))
	rec := postExtra(h.TwoFARemoveKey, `{"password":"pass","credentialId":"key1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	// 残り 0 件なので securityKeysAvailable=false にされる
	assert.False(t, repo.Profiles["u1"].SecurityKeysAvailable)
	assert.False(t, repo.Profiles["u1"].UsePasswordLessLogin)
}

// TOTP gate (upstream drop-in 互換): 2FA 有効ユーザが token 無しで
// remove-key を呼ぶと 403 INVALID_TOKEN で refuse される。passkey 削除は
// 2FA の物理 factor を抜く操作なので強い認証が必要。
func TestTwoFARemoveKey_With2FA_RequiresToken(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{
		ID: "key1", UserID: "u1", Name: "k", PublicKey: "pk",
	}))
	rec := postExtra(h.TwoFARemoveKey, `{"password":"pass","credentialId":"key1"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
}

// 2FA 有効でも valid token (backup code) を渡せば key を削除できる。
func TestTwoFARemoveKey_With2FA_AcceptsBackupCode(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{
		ID: "key1", UserID: "u1", Name: "k", PublicKey: "pk",
	}))
	rec := postExtra(h.TwoFARemoveKey, `{"password":"pass","credentialId":"key1","token":"backup1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// upstream order regression guard: TwoFARemoveKey でも wrong-password +
// wrong-token は upstream 順 (TOTP first) で INVALID_TOKEN を返す。
// requireWebAuthn helper の呼び出し順序を入れ替えた回帰防止。
func TestTwoFARemoveKey_With2FA_WrongPasswordAndToken_ReturnsTokenError(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{
		ID: "key1", UserID: "u1", Name: "k", PublicKey: "pk",
	}))
	rec := postExtra(h.TwoFARemoveKey, `{"password":"wrong","credentialId":"key1","token":"wrong"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN",
		"TOTP gate must fire before password check (upstream Misskey TS order)")
	assert.NotContains(t, rec.Body.String(), "INCORRECT_PASSWORD")
}

// --- TwoFAUpdateKey ---

func TestTwoFAUpdateKey_MissingFields(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFAUpdateKey, `{"password":"pass","credentialId":"k"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFAUpdateKey_NotFound(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFAUpdateKey, `{"password":"pass","credentialId":"ghost","name":"x"}`, user)
	// #1767: upstream noSuchKey は httpStatusCode 未指定 = 400 (code は一致)。
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTwoFAUpdateKey_Success(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{
		ID: "key1", UserID: "u1", Name: "old", PublicKey: "pk",
	}))
	rec := postExtra(h.TwoFAUpdateKey, `{"password":"pass","credentialId":"key1","name":"renamed"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "renamed", skRepo.keys["key1"].Name)
}

// #1952 upstream update-key.ts の paramDef は {name, credentialId} のみで password を
// 受け付けない。standard misskey-js client は password を送らないので、password 無しで
// 204 成功し rename されることを検証する。
func TestTwoFAUpdateKey_NoPasswordSucceeds(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{
		ID: "key1", UserID: "u1", Name: "old", PublicKey: "pk",
	}))
	rec := postExtra(h.TwoFAUpdateKey, `{"credentialId":"key1","name":"renamed"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "renamed", skRepo.keys["key1"].Name)
}

// name が maxLength:30 超は INVALID_PARAM (upstream paramDef、#1952)。
func TestTwoFAUpdateKey_NameTooLong(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{
		ID: "key1", UserID: "u1", Name: "old", PublicKey: "pk",
	}))
	long := strings.Repeat("a", 31)
	rec := postExtra(h.TwoFAUpdateKey, `{"credentialId":"key1","name":"`+long+`"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	// 名前は変更されない。
	assert.Equal(t, "old", skRepo.keys["key1"].Name)
}

// securityKeyRepo 未配線 (WebAuthn 未設定) は 503 UNAVAILABLE (#1952)。
func TestTwoFAUpdateKey_NotConfigured(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.TwoFAUpdateKey, `{"credentialId":"k","name":"x"}`, user)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// session user 不在は ACCESS_DENIED (secure session 認証前提、#1952)。
func TestTwoFAUpdateKey_NoUser(t *testing.T) {
	h, _, _ := newWebAuthnHandler(t)
	rec := postExtra(h.TwoFAUpdateKey, `{"credentialId":"k","name":"x"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
}

// #1555 他人所有の key を更新しようとすると ACCESS_DENIED (NO_SUCH_KEY ではない)。
// upstream update-key.ts は key==null を noSuchKey、key.userId!==me.id を
// accessDenied で区別する。
func TestTwoFAUpdateKey_AccessDenied(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	// key は別ユーザー (u2) 所有。
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{
		ID: "key1", UserID: "u2", Name: "old", PublicKey: "pk",
	}))
	rec := postExtra(h.TwoFAUpdateKey, `{"password":"pass","credentialId":"key1","name":"x"}`, user)
	// #1767: upstream accessDenied は httpStatusCode 未指定 = 400 (code は一致)。
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
	// 名前は変更されない。
	assert.Equal(t, "old", skRepo.keys["key1"].Name)
}

// --- TwoFAPasswordLess ---
//
// upstream Misskey TS は paramDef を `{value: boolean}` で password を
// 要求しない (#758)。本 endpoint は session で認証済み前提なので mk-go も
// 合わせている。

// value 未指定 (空 body) は default false 扱いで no-content 成功する。
// upstream の paramDef は required:['value'] だが mk-go の echo Bind は
// JSON body 欠落を default 値で埋めるので、ここで bad request にしない。
func TestTwoFAPasswordLess_EmptyBodyDefaultsFalse(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	repo.Profiles["u1"].UsePasswordLessLogin = true
	rec := postExtra(h.TwoFAPasswordLess, `{}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.False(t, repo.Profiles["u1"].UsePasswordLessLogin)
}

// value=true で security key 0 件 → noKey error + profile が
// usePasswordLessLogin=false に巻き戻される (upstream 互換)。
func TestTwoFAPasswordLess_EnableNoKeys(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	repo.Profiles["u1"].UsePasswordLessLogin = true
	rec := postExtra(h.TwoFAPasswordLess, `{"value":true}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, repo.Profiles["u1"].UsePasswordLessLogin,
		"upstream rolls back the flag before throwing noKey")
}

func TestTwoFAPasswordLess_EnableWithKey(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{ID: "k1", UserID: "u1"}))
	rec := postExtra(h.TwoFAPasswordLess, `{"value":true}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Profiles["u1"].UsePasswordLessLogin)
}

func TestTwoFAPasswordLess_Disable(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	repo.Profiles["u1"].UsePasswordLessLogin = true
	rec := postExtra(h.TwoFAPasswordLess, `{"value":false}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.False(t, repo.Profiles["u1"].UsePasswordLessLogin)
}

// publishMeUpdatedPartial 経由で frontend の $i に partial merge される
// payload が main stream に送られていることを assert する (#758)。
func TestTwoFAPasswordLess_PublishesPartialMeUpdated(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{ID: "k1", UserID: "u1"}))
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	rec := postExtra(h.TwoFAPasswordLess, `{"value":true}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "meUpdated", pub.calls[0].eventType)
	body, ok := pub.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, body["usePasswordLessLogin"])
}

// publishMeUpdatedPartial: mainStreamPublisher が wire されていない場合は
// no-op (production の fallback path 確保 + test の panic 防止)。
func TestPublishMeUpdatedPartial_NoPublisherIsNoop(t *testing.T) {
	h, _, _ := newWebAuthnHandler(t)
	// SetMainStreamPublisher を呼ばずそのまま invoke しても panic しない。
	h.publishMeUpdatedPartial("u1", map[string]any{"x": 1})
}

// publishMeUpdatedPartial: 空 fields は publish しない (no-op)。
func TestPublishMeUpdatedPartial_EmptyFieldsIsNoop(t *testing.T) {
	h, _, _ := newWebAuthnHandler(t)
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	h.publishMeUpdatedPartial("u1", nil)
	h.publishMeUpdatedPartial("u1", map[string]any{})
	assert.Empty(t, pub.calls, "no PublishMainEvent for empty fields")
}

// publishMeUpdated (full MeDetailed): publisher が wire されていれば
// userService.ShowByID 経由で User+Profile を packed publish する。
// payload は upstream Misskey TS と同じ MeDetailed shape (#968)。
// UserDetailed shape に戻る regression を catch するため、enriched map が
// MeDetailed 専用 key (avatarId) と unread field を持つことを assert する。
// MeDetailed 拡張 field 自体の transfer は entity 側の TestPackMeDetailed_*
// でカバー済み。
func TestPublishMeUpdated_FullPublishWhenWired(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	setupUserWithPassword(repo, "u1", "pass")
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	h.publishMeUpdated("u1")
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "meUpdated", pub.calls[0].eventType)
	require.NotNil(t, pub.calls[0].body)

	// #1258 fu: meUpdated は meDetailedWithUnread 経由で enrich した map を送る
	// (raw struct だと unread が default で出て $i を clobber する)。
	body, ok := pub.calls[0].body.(map[string]any)
	require.True(t, ok, "publishMeUpdated body は enriched MeDetailed map であるべき")
	// embed が壊れていないことの sanity check (UserLite 由来 field)。
	assert.Equal(t, "u1", body["id"])
	// MeDetailed shape である確認 (UserDetailed shape に戻る regression catch):
	// avatarId は MeDetailed 専用 key。
	assert.Contains(t, body, "avatarId")
	// unread enrich 経路を通っていること (clobber 回避の要)。
	assert.Contains(t, body, "unreadNotificationsCount")
	// #1240 不変条件: meUpdated は policies を **含めない** (含めると frontend の
	// updateCurrentAccountPartial が $i.policies を null/空で clobber する)。
	// meDetailedWithUnread は PackMeDetailed の omitempty を round-trip で維持する
	// が、これは繊細なので回帰ガードとして固定する。
	assert.NotContains(t, body, "policies", "meUpdated は policies を含めてはいけない (#1240)")
}

// publishMeUpdated: 存在しない userID は ShowByID で err → publish せず
// log だけ残す。
func TestPublishMeUpdated_UnknownUserIsNoop(t *testing.T) {
	h, _, _ := newWebAuthnHandler(t)
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	h.publishMeUpdated("nonexistent")
	assert.Empty(t, pub.calls, "no publish for unknown user")
}

// publishMeUpdated: publisher 未配線は no-op (production の fallback path)。
func TestPublishMeUpdated_NoPublisherIsNoop(t *testing.T) {
	h, _, _ := newWebAuthnHandler(t)
	// SetMainStreamPublisher を呼ばずに invoke しても panic しない。
	h.publishMeUpdated("u1")
}
