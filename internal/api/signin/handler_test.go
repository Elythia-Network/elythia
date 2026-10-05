package signin_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/elythia-network/elythia/internal/api/apierr"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/signin"
	"github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

type mockIPRecorder struct {
	fn func(userID, ip string)
}

func (m *mockIPRecorder) Record(userID, ip string) {
	if m.fn != nil {
		m.fn(userID, ip)
	}
}

func newTestHandler(t *testing.T) (*signin.Handler, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	return signin.NewHandler(userRepo), userRepo
}

func doPost(h func(echo.Context) error, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/signin", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h(c)
	return rec
}

// doPostCanceled は**キャンセル済みの request context** で叩く。
//
// Argon2id の検証枠を取れなかった経路 (OutcomeUnavailable) を再現するのに使う。
// セマフォを実際に占有する手段だと production 側にテスト専用の export が要り、
// しかも acquire timeout 分 (3 秒) 待つことになる。`Acquire` は ctx が死んで
// いれば即座に失敗するので、同じ分岐を待ち時間ゼロで踏める。
func doPostCanceled(h func(echo.Context) error, body string) *httptest.ResponseRecorder {
	e := echo.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/signin", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h(c)
	return rec
}

func createTestUser(repo *testutil.MockUserRepository, username, password string) *model.User {
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	return createTestUserWithStoredPassword(repo, username, string(hash))
}

func createTestUserWithStoredPassword(repo *testutil.MockUserRepository, username, stored string) *model.User {
	token := "testtoken1234567"
	user := &model.User{
		ID:            "u1",
		Username:      username,
		UsernameLower: strings.ToLower(username),
		Token:         &token,
	}
	repo.Users["u1"] = user
	repo.Profiles["u1"] = &model.UserProfile{
		UserID:   "u1",
		Password: &stored,
	}
	return user
}

func signinArgon2Fixture(plain string) string {
	salt := []byte("0123456789abcdef")
	digest := argon2.IDKey([]byte(plain), salt, 3, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest))
}

func TestSignin_Step1_NoPassword(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")

	rec := doPost(h.Signin, `{"username":"admin"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["finished"])
	assert.Equal(t, "password", resp["next"])
}

func TestSignin_Step2_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["finished"])
	assert.Equal(t, "u1", resp["id"])
	assert.Equal(t, "testtoken1234567", resp["i"])
}

func TestSignin_AcceptsCherryPickArgon2(t *testing.T) {
	h, repo := newTestHandler(t)
	stored := signinArgon2Fixture("pass123")
	createTestUserWithStoredPassword(repo, "admin", stored)

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestSignin_MigratesArgon2AfterSuccess(t *testing.T) {
	h, repo := newTestHandler(t)
	stored := signinArgon2Fixture("pass123")
	createTestUserWithStoredPassword(repo, "admin", stored)

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := *repo.Profiles["u1"].Password
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(after), []byte("pass123")))
	afterCost, err := bcrypt.Cost([]byte(after))
	require.NoError(t, err)
	assert.Equal(t, password.Cost(), afterCost)
}

func TestSignin_Argon2PasswordOver72BytesStaysArgon2(t *testing.T) {
	h, repo := newTestHandler(t)
	plain := strings.Repeat("a", 73)
	stored := signinArgon2Fixture(plain)
	createTestUserWithStoredPassword(repo, "admin", stored)

	rec := doPost(h.Signin, `{"username":"admin","password":"`+plain+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, stored, *repo.Profiles["u1"].Password)
}

func TestSignin_Argon2CASConflictDoesNotOverwriteNewPassword(t *testing.T) {
	h, repo := newTestHandler(t)
	stored := signinArgon2Fixture("pass123")
	createTestUserWithStoredPassword(repo, "admin", stored)
	newer, err := password.Hash("newer")
	require.NoError(t, err)
	repo.UpdatePasswordIfCurrentFn = func(userID, currentHash, newHash string) (bool, error) {
		assert.Equal(t, "u1", userID)
		assert.Equal(t, stored, currentHash)
		assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(newHash), []byte("pass123")))
		repo.Profiles["u1"].Password = &newer
		return false, nil
	}

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, newer, *repo.Profiles["u1"].Password)
}

func TestSignin_Argon2MigrationErrorDoesNotFailSignin(t *testing.T) {
	h, repo := newTestHandler(t)
	stored := signinArgon2Fixture("pass123")
	createTestUserWithStoredPassword(repo, "admin", stored)
	repo.UpdatePasswordIfCurrentFn = func(_, _, _ string) (bool, error) { return false, assert.AnError }

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"finished":true,"id":"u1","i":"testtoken1234567"}`, rec.Body.String())
	assert.Equal(t, stored, *repo.Profiles["u1"].Password)
}

func TestSignin_RejectsMalformedArgon2WithExistingShape(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUserWithStoredPassword(repo, "admin", "$argon2id$v=19$m=999999999,t=3,p=4$bad$bad")

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"error":{"id":"932c904e-9460-45b7-9ce6-7ed33be7eb2c"}}`, rec.Body.String())
}

// chanLoginNotifier captures OnLogin asynchronously (#1559)。
type chanLoginNotifier struct{ ch chan string }

func (n *chanLoginNotifier) OnLogin(userID string) { n.ch <- userID }

// #1559 [LOW] signin 成功時に login 通知が発火する。
func TestSignin_FiresLoginNotifier(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")
	notifier := &chanLoginNotifier{ch: make(chan string, 1)}
	h.SetLoginNotifier(notifier)

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code)

	select {
	case uid := <-notifier.ch:
		assert.Equal(t, "u1", uid)
	case <-time.After(2 * time.Second):
		t.Fatal("login notifier was not fired")
	}
}

// #1804: RecordSuccessfulSignin は signin / signup 共有の成功時 side-effect
// entrypoint。signin 履歴 (success:true) + login 通知を発火する。
func TestRecordSuccessfulSignin_FiresHistoryAndNotifier(t *testing.T) {
	h, _ := newTestHandler(t)
	signinRepo := testutil.NewMockSigninRepository()
	idGen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(signinRepo, idGen)
	notifier := &chanLoginNotifier{ch: make(chan string, 1)}
	h.SetLoginNotifier(notifier)

	h.RecordSuccessfulSignin("u1", "1.2.3.4", http.Header{})

	select {
	case uid := <-notifier.ch:
		assert.Equal(t, "u1", uid)
	case <-time.After(2 * time.Second):
		t.Fatal("login notifier was not fired")
	}
	require.Eventually(t, func() bool { return signinRepo.Len() == 1 }, 2*time.Second, 10*time.Millisecond)
	assert.True(t, signinRepo.Signins[0].Success, "success:true で記録する")
}

func TestSignin_WrongPassword(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")

	rec := doPost(h.Signin, `{"username":"admin","password":"wrongpass"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSignin_UserNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Signin, `{"username":"ghost","password":"x"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSignin_EmptyUsername(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Signin, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSignin_SuspendedUser(t *testing.T) {
	h, repo := newTestHandler(t)
	user := createTestUser(repo, "banned", "pass")
	user.IsSuspended = true

	rec := doPost(h.Signin, `{"username":"banned","password":"pass"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSignin_NoProfile(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u1"] = &model.User{
		ID: "u1", Username: "noprof", UsernameLower: "noprof",
	}
	// プロフィールなし

	rec := doPost(h.Signin, `{"username":"noprof","password":"x"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSignin_NilToken(t *testing.T) {
	h, repo := newTestHandler(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	repo.Users["u1"] = &model.User{
		ID: "u1", Username: "notoken", UsernameLower: "notoken",
		Token: nil, // トークンなし
	}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &hashStr}

	rec := doPost(h.Signin, `{"username":"notoken","password":"pass"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "", resp["i"]) // token is empty string
}

// --- SigninFlow ---

func TestSigninFlow_Step1(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")

	rec := doPost(h.SigninFlow, `{"username":"admin"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["finished"])
	// 2FA 無効ユーザは TS upstream 互換で常に 'captcha' (#705)。
	assert.Equal(t, "captcha", resp["next"])
}

func TestSigninFlow_Step2_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")

	rec := doPost(h.SigninFlow, `{"username":"admin","password":"pass123"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["finished"])
	assert.Equal(t, "u1", resp["id"])
	assert.NotEmpty(t, resp["i"])
}

func TestSigninFlow_AcceptsCherryPickArgon2(t *testing.T) {
	h, repo := newTestHandler(t)
	stored := signinArgon2Fixture("pass123")
	createTestUserWithStoredPassword(repo, "admin", stored)

	rec := doPost(h.SigninFlow, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestSigninFlow_WrongPassword(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")

	rec := doPost(h.SigninFlow, `{"username":"admin","password":"wrong"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSigninFlow_UserNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.SigninFlow, `{"username":"ghost"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSigninFlow_EmptyUsername(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.SigninFlow, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSigninFlow_SuspendedUser(t *testing.T) {
	h, repo := newTestHandler(t)
	user := createTestUser(repo, "banned2", "pass")
	user.IsSuspended = true

	rec := doPost(h.SigninFlow, `{"username":"banned2"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSigninFlow_NoProfile(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u2"] = &model.User{
		ID: "u2", Username: "noprof2", UsernameLower: "noprof2",
	}

	rec := doPost(h.SigninFlow, `{"username":"noprof2","password":"x"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSigninFlow_NilToken(t *testing.T) {
	h, repo := newTestHandler(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	repo.Users["u3"] = &model.User{
		ID: "u3", Username: "notoken2", UsernameLower: "notoken2",
		Token: nil,
	}
	repo.Profiles["u3"] = &model.UserProfile{UserID: "u3", Password: &hashStr}

	rec := doPost(h.SigninFlow, `{"username":"notoken2","password":"pass"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "", resp["i"])
}

// --- CAPTCHA integration ---

func TestSigninFlow_CaptchaPassesWithCorrectToken(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "capuser", "pass")

	captchaSvc := captcha.NewService(&model.Meta{EnableTestcaptcha: true})
	h.SetCaptcha(captchaSvc)

	rec := doPost(h.SigninFlow, `{"username":"capuser","password":"pass","testcaptcha-response":"testcaptcha-passed"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["finished"])
}

func TestSigninFlow_CaptchaBlocksMissingToken(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "capuser2", "pass")

	captchaSvc := captcha.NewService(&model.Meta{EnableTestcaptcha: true})
	h.SetCaptcha(captchaSvc)

	// testcaptcha-response を送らないので CAPTCHA 検証失敗。upstream は
	// `throw new FastifyReplyError(400, err)` を投げる (SigninApiService.ts)
	// ため、mk-go も #810 で同 Fastify shape に揃える。
	rec := doPost(h.SigninFlow, `{"username":"capuser2","password":"pass"}`)
	testutil.AssertFastifyError(t, rec, http.StatusBadRequest, "CAPTCHA_FAILED")
}

func TestSigninFlow_DoesNotMigrateArgon2WhenCaptchaFails(t *testing.T) {
	h, repo := newTestHandler(t)
	stored := signinArgon2Fixture("pass123")
	createTestUserWithStoredPassword(repo, "admin", stored)
	h.SetCaptcha(captcha.NewService(&model.Meta{EnableTestcaptcha: true}))

	rec := doPost(h.SigninFlow, `{"username":"admin","password":"pass123"}`)
	testutil.AssertFastifyError(t, rec, http.StatusBadRequest, "CAPTCHA_FAILED")
	assert.Equal(t, stored, *repo.Profiles["u1"].Password)
}

func TestSigninFlow_MigratesArgon2AfterCaptchaSuccess(t *testing.T) {
	h, repo := newTestHandler(t)
	stored := signinArgon2Fixture("pass123")
	createTestUserWithStoredPassword(repo, "admin", stored)
	h.SetCaptcha(captcha.NewService(&model.Meta{EnableTestcaptcha: true}))

	rec := doPost(h.SigninFlow, `{"username":"admin","password":"pass123","testcaptcha-response":"testcaptcha-passed"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(*repo.Profiles["u1"].Password), []byte("pass123")))
}

func TestSigninFlow_CaptchaSkippedFor2FAUsers(t *testing.T) {
	h, repo := newTestHandler(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	token := "tok"
	repo.Users["u2fa"] = &model.User{
		ID: "u2fa", Username: "tfa", UsernameLower: "tfa", Token: &token,
	}
	repo.Profiles["u2fa"] = &model.UserProfile{
		UserID:           "u2fa",
		Password:         &hashStr,
		TwoFactorEnabled: true,
	}

	captchaSvc := captcha.NewService(&model.Meta{EnableTestcaptcha: true})
	h.SetCaptcha(captchaSvc)

	// 2FA 有効ユーザーは CAPTCHA をスキップして "totp" ステップへ進む。
	rec := doPost(h.SigninFlow, `{"username":"tfa","password":"pass"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "totp", resp["next"])
}

func TestSignin_RecordsSigninHistory(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "testuser", "password123")

	signinRepo := testutil.NewMockSigninRepository()
	idGen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(signinRepo, idGen)

	rec := doPost(h.Signin, `{"username":"testuser","password":"password123"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	// goroutineでsigninレコードが作成されるのを待つ
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, signinRepo.Len())
}

// #1776: 認証失敗 (パスワード不一致) でも signin 履歴を success:false で記録する。
// upstream SigninApiService.fail() は全認証失敗で signins に success:false を insert。
func TestSignin_RecordsFailedSignin(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "testuser", "password123")

	signinRepo := testutil.NewMockSigninRepository()
	idGen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(signinRepo, idGen)

	rec := doPost(h.Signin, `{"username":"testuser","password":"WRONG"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, signinRepo.Len())
	assert.False(t, signinRepo.Signins[0].Success, "失敗ログインは success:false で記録する")
}

// #1776: SigninFlow の 2FA token 失敗でも success:false を記録する。
func TestSigninFlow_RecordsFailed2FA(t *testing.T) {
	h, repo := newTestHandler(t)
	user := createTestUser(repo, "tfauser", "password123")
	repo.Profiles[user.ID].TwoFactorEnabled = true
	secret := "JBSWY3DPEHPK3PXP"
	repo.Profiles[user.ID].TwoFactorSecret = &secret

	signinRepo := testutil.NewMockSigninRepository()
	idGen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(signinRepo, idGen)

	rec := doPost(h.SigninFlow, `{"username":"tfauser","password":"password123","token":"000000"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, signinRepo.Len())
	assert.False(t, signinRepo.Signins[0].Success)
}

// #1776: 凍結アカウントの signin 試行は履歴に記録しない (upstream は suspended で
// fail() を呼ばず先に弾く)。user-not-found も同様。
func TestSignin_SuspendedAndNotFoundDoNotRecord(t *testing.T) {
	h, repo := newTestHandler(t)
	user := createTestUser(repo, "suspendeduser", "password123")
	user.IsSuspended = true

	signinRepo := testutil.NewMockSigninRepository()
	idGen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(signinRepo, idGen)

	rec := doPost(h.Signin, `{"username":"suspendeduser","password":"password123"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = doPost(h.Signin, `{"username":"ghost","password":"x"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, signinRepo.Len(), "suspended / user-not-found は失敗履歴を残さない")
}

// stubMainStreamPublisher captures PublishMainEvent calls.
type stubMainStreamPublisher struct {
	mu    sync.Mutex
	calls []mainEventCall
}

type mainEventCall struct {
	userID    string
	eventType string
	body      any
}

func (s *stubMainStreamPublisher) PublishMainEvent(userID, eventType string, body any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, mainEventCall{userID, eventType, body})
}

func TestSignin_PublishesSigninEvent(t *testing.T) {
	h, repo := newTestHandler(t)
	user := createTestUser(repo, "testuser", "password123")

	signinRepo := testutil.NewMockSigninRepository()
	idGen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(signinRepo, idGen)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	rec := doPost(h.Signin, `{"username":"testuser","password":"password123"}`)
	require.Equal(t, http.StatusOK, rec.Code)

	// 非同期goroutineのrecordSignin完了を待つ
	time.Sleep(100 * time.Millisecond)

	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Len(t, pub.calls, 1)
	assert.Equal(t, user.ID, pub.calls[0].userID)
	assert.Equal(t, "signin", pub.calls[0].eventType)
	// body は PackSignin の map。最低限 id/success が含まれる。
	body, ok := pub.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, body["id"])
	assert.Equal(t, true, body["success"])
}

func TestSignin_IPLogging(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "testuser", "password123")

	var logged atomic.Bool
	h.SetIPRecorder(&mockIPRecorder{fn: func(userID, ip string) {
		logged.Store(true)
	}})

	rec := doPost(h.Signin, `{"username":"testuser","password":"password123"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	// **記録は同期。** goroutine へ逃がすのは recorder 側の仕事になったので、
	// handler から戻った時点で呼ばれている。
	assert.True(t, logged.Load())
}

func TestSignin_SanitizesHeaders(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "testuser", "password123")

	signinRepo := testutil.NewMockSigninRepository()
	idGen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(signinRepo, idGen)

	// Authorization, Cookieヘッダー付きのリクエスト
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/signin", strings.NewReader(`{"username":"testuser","password":"password123"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Cookie", "session=abc123")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h.Signin(c)
	assert.Equal(t, http.StatusOK, rec.Code)

	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, signinRepo.Len())
	stored := string(signinRepo.Signins[0].Headers)
	assert.NotContains(t, stored, "secret-token")
	assert.NotContains(t, stored, "abc123")
}

func TestSigninFlow_RecordsSigninHistory(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "testuser", "password123")

	signinRepo := testutil.NewMockSigninRepository()
	idGen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(signinRepo, idGen)

	rec := doPost(h.SigninFlow, `{"username":"testuser","password":"password123"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, signinRepo.Len())
}

// --- Step 1 captcha branching ---

func TestSigninFlow_Step1_CaptchaNext_WhenCaptchaEnabled(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "capstep", "pass")

	captchaSvc := captcha.NewService(&model.Meta{EnableTestcaptcha: true})
	h.SetCaptcha(captchaSvc)

	// 2FA無効 + captcha有効 → Step 1で "captcha" を返す
	rec := doPost(h.SigninFlow, `{"username":"capstep"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["finished"])
	assert.Equal(t, "captcha", resp["next"])
}

func TestSigninFlow_Step1_PasswordNext_When2FAEnabled(t *testing.T) {
	h, repo := newTestHandler(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	token := "tok"
	repo.Users["u2fa2"] = &model.User{
		ID: "u2fa2", Username: "tfa2", UsernameLower: "tfa2", Token: &token,
	}
	repo.Profiles["u2fa2"] = &model.UserProfile{
		UserID:           "u2fa2",
		Password:         &hashStr,
		TwoFactorEnabled: true,
	}

	captchaSvc := captcha.NewService(&model.Meta{EnableTestcaptcha: true})
	h.SetCaptcha(captchaSvc)

	// 2FA有効 + captcha有効 → Step 1で "password" を返す
	rec := doPost(h.SigninFlow, `{"username":"tfa2"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["finished"])
	assert.Equal(t, "password", resp["next"])
}

func TestSigninFlow_Step1_CaptchaNext_WhenNoCaptchaConfigured(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "nocap", "pass")

	// captchaサービスなしでも、TS upstream は 2FA 無効ユーザに対して常に
	// 'captcha' を返す。フロント (MkSignin.vue) は instance meta で widget の
	// 表示要否を判定するため、サーバ側の captcha 設定とは独立している (#705)。
	rec := doPost(h.SigninFlow, `{"username":"nocap"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["finished"])
	assert.Equal(t, "captcha", resp["next"])
}

// #2106 H2 (CRITICAL): レガシー /api/signin は 2FA 有効ユーザーに password だけで
// token を発行してはならない。challenge を返し /signin-flow へ誘導する。
func TestSignin_LegacyTwoFactorNotBypassed(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")
	repo.Profiles["u1"].TwoFactorEnabled = true

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["finished"])
	assert.Equal(t, "totp", resp["next"])
	_, hasToken := resp["i"]
	assert.False(t, hasToken, "2FA user must NOT receive a session token via legacy /signin")
}

// ログインが通ったら、設定より弱いハッシュをその場で焼き直すこと。
//
// **これが無いと bcryptCost を上げても意味がない。** 既存の利用者のハッシュは
// パスワードを変更するまで古い強度のまま残る。ログインは平文を握っている唯一の
// 機会なので、そこで上げる。
func TestSignin_RehashesWeakPasswordOnSuccess(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123") // bcrypt.MinCost で作られる

	before := *repo.Profiles["u1"].Password
	beforeCost, err := bcrypt.Cost([]byte(before))
	require.NoError(t, err)
	require.Less(t, beforeCost, password.Cost(), "前提: 設定より弱いこと")

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := *repo.Profiles["u1"].Password
	afterCost, err := bcrypt.Cost([]byte(after))
	require.NoError(t, err)
	assert.Equal(t, password.Cost(), afterCost, "設定した cost に焼き直されていない")
	// **平文は変わっていないこと。** 焼き直しでパスワードが変わると締め出す。
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(after), []byte("pass123")))
}

// パスワードが違うときは焼き直さない。**間違った平文で上書きすると締め出す。**
func TestSignin_DoesNotRehashOnWrongPassword(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")
	before := *repo.Profiles["u1"].Password

	doPost(h.Signin, `{"username":"admin","password":"wrong"}`)

	assert.Equal(t, before, *repo.Profiles["u1"].Password, "失敗したのに書き換えている")
}

// 既に設定どおりの強度なら触らない (毎回のログインで無駄な書き込みをしない)。
func TestSignin_DoesNotRehashWhenAlreadyStrong(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123")
	strong, err := password.Hash("pass123")
	require.NoError(t, err)
	repo.Profiles["u1"].Password = &strong

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, strong, *repo.Profiles["u1"].Password, "同じ強度なのに焼き直している")
}

// 検証枠を取れなかったときは 403 ではなく 503 を返し、**そこで処理を止める** (#2849)。
//
// **body 全体を見るのが要点。** 初版は `c.JSON` が成功時に nil を返すことを
// 見落として早期 return が死んでおり、503 の後も処理が続いて JSON が 2 個
// 連結され、偽の失敗ログイン履歴まで残っていた。status だけ見るテストでは
// echo が 2 回目の WriteHeader を無視するため素通りする。
func TestSignin_VerifierBusyReturns503(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUserWithStoredPassword(repo, "admin", signinArgon2Fixture("pass123"))
	sr := newRecordingSigninRepo()
	gen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(sr, gen)

	rec := doPostCanceled(h.Signin, `{"username":"admin","password":"pass123"}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, "360", rec.Header().Get("Retry-After"))
	assertSingleJSONError(t, rec, apierr.UUIDPasswordVerificationUnavailable)
	// **password を一度も検証していないので履歴も残さない。**
	sr.assertNoSigninRecorded(t)
}

// 同じく signin-flow でも 503 になり、captcha まで進まない。
func TestSigninFlow_VerifierBusyReturns503(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUserWithStoredPassword(repo, "admin", signinArgon2Fixture("pass123"))
	sr := newRecordingSigninRepo()
	gen, _ := id.NewGenerator("aidx")
	h.SetSigninRepo(sr, gen)

	rec := doPostCanceled(h.SigninFlow, `{"username":"admin","password":"pass123"}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, "360", rec.Header().Get("Retry-After"))
	assertSingleJSONError(t, rec, apierr.UUIDPasswordVerificationUnavailable)
	sr.assertNoSigninRecorded(t)
}

// assertSingleJSONError は body が **JSON 1 個**で、その error.id が wantID で
// あることを確かめる。連結された 2 個目を見逃さないために decoder で読み切る。
func assertSingleJSONError(t *testing.T, rec *httptest.ResponseRecorder, wantID string) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	var first struct {
		Error struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"error"`
	}
	require.NoError(t, dec.Decode(&first), "body=%q", rec.Body.String())
	assert.Equal(t, wantID, first.Error.ID)
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		t.Fatalf("body に JSON が 2 個以上ある: %q", rec.Body.String())
	}
}

// recordingSigninRepo counts Create calls so tests can assert that the 503 path
// leaves no signin history.
//
// **channel で受ける。** `h.fail` は `go h.recordSignin(...)` で非同期に書くので、
// ハンドラ復帰直後にカウンタを読むと必ず 0 が返り、503 の経路で記録を足す変異が
// 素通りする (実測で 6/6 生存)。
type recordingSigninRepo struct {
	ch chan struct{}
}

func newRecordingSigninRepo() *recordingSigninRepo {
	return &recordingSigninRepo{ch: make(chan struct{}, 8)}
}

func (r *recordingSigninRepo) Create(*model.Signin) error {
	r.ch <- struct{}{}
	return nil
}

// assertNoSigninRecorded は一定時間 Create が呼ばれないことを確かめる。
func (r *recordingSigninRepo) assertNoSigninRecorded(t *testing.T) {
	t.Helper()
	select {
	case <-r.ch:
		t.Fatal("503 の経路で失敗ログイン履歴が記録された")
	case <-time.After(200 * time.Millisecond):
	}
}

func (r *recordingSigninRepo) ListByUserID(string, int, string, string) ([]*model.Signin, error) {
	return nil, nil
}

// 未対応 profile は従来どおり 403 のまま。**503 に倒さない** — データの異常で
// あって一時的な負荷ではないので、再試行しても直らない (#2849)。
func TestSignin_UnsupportedProfileStays403(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUserWithStoredPassword(repo, "admin", "$argon2id$v=19$m=4096,t=3,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY")

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Header().Get("Retry-After"))
}

// captureLogs swaps the default slog handler for the duration of the test.
// JSON で取る (属性と本文中の偶然の一致を区別するため)。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// 未対応 profile では **warn を出し、そこに salt / digest を含めない** (#2849)。
//
// **ヘルパ単体のテストでは守れない。** ProfileForLog が正しくても、handler が
// 生の hash を渡してしまえば漏れる。初版はそれを差し替える変異も、slog.Warn を
// 丸ごと消す変異も、全テストが緑のままだった。
func TestSignin_UnsupportedProfileLogsWithoutHash(t *testing.T) {
	const stored = "$argon2id$v=19$m=4096,t=3,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY"
	h, repo := newTestHandler(t)
	createTestUserWithStoredPassword(repo, "admin", stored)
	buf := captureLogs(t)

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)

	out := buf.String()
	require.Contains(t, out, "unsupported password hash", "warn が出ていない")
	require.Contains(t, out, `"userId":"u1"`, "どのアカウントか追えない")
	assert.Contains(t, out, "m=4096,t=3,p=1", "診断に要る profile が出ていない")

	parts := strings.Split(stored, "$")
	assert.NotContains(t, out, parts[4], "salt がログに出ている")
	assert.NotContains(t, out, parts[5], "digest がログに出ている")
}

// 枠を取れなかったときも warn を出す。出ないと「なぜ 503 が出たか」を
// operator が追えない (#2849)。
func TestSignin_VerifierBusyLogsWarn(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUserWithStoredPassword(repo, "admin", signinArgon2Fixture("pass123"))
	buf := captureLogs(t)

	rec := doPostCanceled(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, buf.String(), "password verification unavailable")
	assert.Contains(t, buf.String(), `"userId":"u1"`)
}

// 503 のログの scheme が**読める形**で出ること (#2849)。
// slog の JSONHandler は fmt.Stringer を使わないので、呼び出し側で明示的に
// String() を通さないと `"scheme":2` という数字になる。
func TestSignin_VerifierBusyLogsReadableScheme(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUserWithStoredPassword(repo, "admin", signinArgon2Fixture("pass123"))
	buf := captureLogs(t)

	doPostCanceled(h.Signin, `{"username":"admin","password":"pass123"}`)

	assert.Contains(t, buf.String(), `"scheme":"argon2id"`)
	assert.NotContains(t, buf.String(), `"scheme":2`)
}

// bcrypt cost の rehash も CAS で書く (#2850)。
//
// **無条件の UpdateProfile だと新しいパスワードを消す。** cost を上げた instance
// で「ログイン (旧パスワードで検証成功)」と「別タブでパスワード変更」が競合した
// とき、rehash が旧パスワードの hash を書き戻す。#2842 が Argon2 移行のために
// CAS を新設したのに、隣の同種の書き込みだけ無防備だった。
func TestSignin_RehashUsesCASAndDoesNotOverwriteNewPassword(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123") // bcrypt.MinCost = 設定より弱い
	stored := *repo.Profiles["u1"].Password

	// **競合は CAS の呼び出し中に起こす。** 先に差し替えると Verify が落ちて
	// signin 自体が 403 になり、rehash まで到達しない。
	newer, err := password.Hash("brand-new")
	require.NoError(t, err)
	var gotCurrent string
	repo.UpdatePasswordIfCurrentFn = func(userID, current, next string) (bool, error) {
		gotCurrent = current
		p := newer
		repo.Profiles["u1"].Password = &p // 別経路が先に書き換えた
		return false, nil                 // CAS 不成立
	}

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// **観測した古い hash を条件にしていること。**
	assert.Equal(t, stored, gotCurrent, "CAS の条件が観測済み hash になっていない")
	// **新しいパスワードが生きていること。**
	assert.NoError(t, bcrypt.CompareHashAndPassword(
		[]byte(*repo.Profiles["u1"].Password), []byte("brand-new")),
		"rehash が新しいパスワードを上書きした")
}

// 移行の永続化失敗は err をログに残す (#2850)。
// 無いと DB 障害と制約違反の切り分けができない。
func TestSignin_MigrationPersistenceErrorLogsErr(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUserWithStoredPassword(repo, "admin", signinArgon2Fixture("argon-pass"))
	repo.UpdatePasswordIfCurrentFn = func(string, string, string) (bool, error) {
		return false, errors.New("boom-db-failure")
	}
	buf := captureLogs(t)

	rec := doPost(h.Signin, `{"username":"admin","password":"argon-pass"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	out := buf.String()
	assert.Contains(t, out, `"category":"persistence_error"`)
	assert.Contains(t, out, "boom-db-failure", "err がログに落ちている")
}

// rehash の永続化が失敗してもログインは成立させる (#2850)。
// 認証は済んでいるので、hash の焼き直しに失敗しただけで締め出さない。
func TestSignin_RehashPersistenceErrorDoesNotFailSignin(t *testing.T) {
	h, repo := newTestHandler(t)
	createTestUser(repo, "admin", "pass123") // bcrypt.MinCost = 設定より弱い
	repo.UpdatePasswordIfCurrentFn = func(string, string, string) (bool, error) {
		return false, errors.New("boom-rehash-store")
	}
	buf := captureLogs(t)

	rec := doPost(h.Signin, `{"username":"admin","password":"pass123"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, buf.String(), "failed to store rehashed password")
	assert.Contains(t, buf.String(), "boom-rehash-store", "err がログに落ちている")
}

// 72 byte 超は**恒久保留**として Info で記録する (#2850)。
//
// hash_error と同じ Warn に混ぜると、該当利用者がログインするたびに warn が
// 出てログを埋める。直せる異常ではないので分類を分けている。
func TestSignin_PasswordTooLongLogsPermanentDeferral(t *testing.T) {
	h, repo := newTestHandler(t)
	long := strings.Repeat("a", 80) // bcrypt は 72 byte で ErrPasswordTooLong
	createTestUserWithStoredPassword(repo, "admin", signinArgon2Fixture(long))
	buf := captureLogs(t)

	rec := doPost(h.Signin, `{"username":"admin","password":"`+long+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	out := buf.String()
	assert.Contains(t, out, `"category":"password_too_long"`)
	assert.Contains(t, out, "permanently deferred")
	// **hash_error と混ざっていないこと。**
	assert.NotContains(t, out, `"category":"hash_error"`)
	// hash は Argon2id のまま。
	assert.True(t, strings.HasPrefix(*repo.Profiles["u1"].Password, "$argon2id$"))
}

// legacy `/api/signin` も captcha を検証すること。
//
// **この endpoint は `signin-flow` に統合される前の互換 shim** だが、パスワードを
// 検証してセッショントークンを発行する以上、captcha も同じように見る必要がある。
// 見ていないと、運営者が captcha を有効にしても 2FA 無効の利用者に対する
// クレデンシャルスタッフィング対策が素通りする。upstream にはこの endpoint 自体が
// 無く、`signin-flow` 相当は 5 provider すべてを検証する。
func TestSignin_VerifiesCaptcha(t *testing.T) {
	t.Run("トークンが無ければ拒否", func(t *testing.T) {
		h, repo := newTestHandler(t)
		createTestUser(repo, "legacycap", "pass")
		h.SetCaptcha(captcha.NewService(&model.Meta{EnableTestcaptcha: true}))

		rec := doPost(h.Signin, `{"username":"legacycap","password":"pass"}`)
		testutil.AssertFastifyError(t, rec, http.StatusBadRequest, "CAPTCHA_FAILED")
	})

	// **弾きすぎていないことを見る。** 常に拒否する実装でも上は緑になる。
	t.Run("正しいトークンなら通る", func(t *testing.T) {
		h, repo := newTestHandler(t)
		createTestUser(repo, "legacycap2", "pass")
		h.SetCaptcha(captcha.NewService(&model.Meta{EnableTestcaptcha: true}))

		rec := doPost(h.Signin, `{"username":"legacycap2","password":"pass","testcaptcha-response":"testcaptcha-passed"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, true, resp["finished"])
	})

	// captcha 未設定なら従来どおり通る (既存インスタンスを壊さない)。
	t.Run("captcha 未配線なら通る", func(t *testing.T) {
		h, repo := newTestHandler(t)
		createTestUser(repo, "legacycap3", "pass")

		rec := doPost(h.Signin, `{"username":"legacycap3","password":"pass"}`)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}
