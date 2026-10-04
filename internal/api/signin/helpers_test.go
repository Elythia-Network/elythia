package signin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/core/twofactor"
	"github.com/shiroha-a/mk/internal/misc/password"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeStubWebauthnCred returns a minimal webauthn.Credential just to feed
// counter update logic; signature/PublicKey are not consulted here.
func makeStubWebauthnCred() *webauthn.Credential {
	return &webauthn.Credential{
		ID:        []byte{0xaa, 0xbb, 0xcc},
		PublicKey: []byte{0x01, 0x02},
		Authenticator: webauthn.Authenticator{
			SignCount: 17,
		},
	}
}

// encodeCredID は base64url で WebAuthn credential ID を符号化する。
// padding は付かない (RawURLEncoding 仕様)。signin handler と handler_2fa の
// 鍵格納フォーマットを揃える役割。
func TestEncodeCredID(t *testing.T) {
	assert.Equal(t, "AAEC", encodeCredID([]byte{0, 1, 2}))
	assert.Equal(t, "", encodeCredID(nil))
	// padding が付かないこと (URLEncoding ではなく RawURLEncoding)
	assert.NotContains(t, encodeCredID([]byte{0xff, 0xff}), "=")
}

// okBody は signin 成功時のレスポンス map (TS 互換 `{finished, id, i}`) を
// 構築する。Token nil のときは i が空文字列になる。
func TestOkBody_TokenSet(t *testing.T) {
	tok := "tok-abc"
	user := &model.User{ID: "u1", Token: &tok}
	body := (&Handler{}).okBody(user)
	assert.Equal(t, true, body["finished"])
	assert.Equal(t, "u1", body["id"])
	assert.Equal(t, "tok-abc", body["i"])
}

func TestOkBody_TokenNil(t *testing.T) {
	user := &model.User{ID: "u1", Token: nil}
	body := (&Handler{}).okBody(user)
	assert.Equal(t, "", body["i"])
}

// newPasskeyContext は 32 文字の hex を返す (16 byte の random)。複数回呼んで
// 衝突しないこと (= randomness が活きていること) を確認する。
func TestNewPasskeyContext_Random(t *testing.T) {
	a, err := newPasskeyContext()
	assert.NoError(t, err)
	assert.Len(t, a, 32)
	b, _ := newPasskeyContext()
	assert.NotEqual(t, a, b)
}

func TestNewPasskeyContext_RandError(t *testing.T) {
	old := readRandom
	defer func() { readRandom = old }()
	readRandom = func(_ []byte) (int, error) { return 0, assert.AnError }
	_, err := newPasskeyContext()
	assert.Error(t, err)
}

// errSecurityKeyRepo は FindByID で DB 障害を返す stub。resolvePasskeyKey が
// 障害を unknown key (36b96a7d) に丸めないことを確かめるために使う。
type errSecurityKeyRepo struct{}

func (errSecurityKeyRepo) Create(*model.UserSecurityKey) error { return nil }
func (errSecurityKeyRepo) FindByID(string) (*model.UserSecurityKey, error) {
	return nil, assert.AnError
}
func (errSecurityKeyRepo) ListByUser(string) ([]*model.UserSecurityKey, error) {
	return nil, assert.AnError
}
func (errSecurityKeyRepo) UpdateName(string, string, string) error { return nil }
func (errSecurityKeyRepo) UpdateCounter(string, int64) error       { return nil }
func (errSecurityKeyRepo) Delete(string, string) error             { return nil }
func (errSecurityKeyRepo) DeleteByUser(string) error               { return nil }
func (errSecurityKeyRepo) CountByUser(string) (int64, error)       { return 0, nil }

func newResolveHandler(t *testing.T) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1"}
	h := NewHandler(repo)
	h.SetWebAuthn(nil, &inMemorySKInternal{keys: map[string][]*model.UserSecurityKey{
		"u1": {{ID: "AAEC", UserID: "u1"}, {ID: "AwQF", UserID: "u1"}},
	}})
	return h, repo
}

// resolvePasskeyKey looks the key up by the credential id like upstream's
// `findOneBy({ id: response.id })` and hands only that key to the verifier.
func TestResolvePasskeyKey_Success(t *testing.T) {
	h, _ := newResolveHandler(t)
	u, keys, missing, err := h.resolvePasskeyKey("AwQF", credentialIDString)
	require.NoError(t, err)
	assert.False(t, missing)
	assert.Equal(t, "u1", u.ID)
	require.Len(t, keys, 1)
	assert.Equal(t, "AwQF", keys[0].ID)
}

func TestResolvePasskeyKey_UnknownKey(t *testing.T) {
	h, _ := newResolveHandler(t)
	for name, id := range map[string]string{
		"not stored":   "ZZZZ",
		"empty string": "",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := h.resolvePasskeyKey(id, credentialIDString)
			assert.ErrorIs(t, err, twofactor.ErrWebAuthnUnknownKey)
		})
	}
	// 列に入らない値は DB に渡さない。渡すと PostgreSQL のエラーになるので、
	// FindByID が失敗する stub で「引いていない」ことを確かめる。
	for name, id := range map[string]string{
		"NUL":          "AA\x00EC",
		"invalid utf8": "AA\xffEC",
	} {
		t.Run(name, func(t *testing.T) {
			h := NewHandler(testutil.NewMockUserRepository())
			h.SetWebAuthn(nil, errSecurityKeyRepo{})
			_, _, _, err := h.resolvePasskeyKey(id, credentialIDString)
			assert.ErrorIs(t, err, twofactor.ErrWebAuthnUnknownKey)
		})
	}
	t.Run("id is not a string", func(t *testing.T) {
		_, _, _, err := h.resolvePasskeyKey("", credentialIDNotString)
		assert.ErrorIs(t, err, twofactor.ErrWebAuthnUnknownKey)
	})
	t.Run("no key repository", func(t *testing.T) {
		h := NewHandler(testutil.NewMockUserRepository())
		_, _, _, err := h.resolvePasskeyKey("AAEC", credentialIDString)
		assert.ErrorIs(t, err, twofactor.ErrWebAuthnUnknownKey)
	})
}

// A missing id makes upstream's TypeORM drop the condition and @simplewebauthn
// throw `Missing credential ID`, so it is a verification failure (b18c89a7),
// not an unknown key.
func TestResolvePasskeyKey_MissingIDIsVerificationFailure(t *testing.T) {
	h, _ := newResolveHandler(t)
	_, _, _, err := h.resolvePasskeyKey("", credentialIDMissing)
	assert.ErrorIs(t, err, twofactor.ErrWebAuthnVerificationFailed)
	assert.NotErrorIs(t, err, twofactor.ErrWebAuthnUnknownKey)
}

func TestResolvePasskeyKey_DatabaseErrorsAreNotAuthFailures(t *testing.T) {
	t.Run("key lookup", func(t *testing.T) {
		h := NewHandler(testutil.NewMockUserRepository())
		h.SetWebAuthn(nil, errSecurityKeyRepo{})
		_, _, _, err := h.resolvePasskeyKey("AAEC", credentialIDString)
		assert.ErrorIs(t, err, assert.AnError)
		assert.NotErrorIs(t, err, twofactor.ErrWebAuthnUnknownKey)
	})
	t.Run("owner lookup", func(t *testing.T) {
		h, repo := newResolveHandler(t)
		repo.FindErr = assert.AnError
		_, _, _, err := h.resolvePasskeyKey("AAEC", credentialIDString)
		assert.ErrorIs(t, err, assert.AnError)
	})
}

// The owner not being a local user is reported after verification
// (652f899f), so the resolver hands back a stand-in with the owner id.
func TestResolvePasskeyKey_OwnerMissing(t *testing.T) {
	t.Run("no user row", func(t *testing.T) {
		h, repo := newResolveHandler(t)
		delete(repo.Users, "u1")
		u, keys, missing, err := h.resolvePasskeyKey("AAEC", credentialIDString)
		require.NoError(t, err)
		assert.True(t, missing)
		assert.Equal(t, "u1", u.ID)
		assert.Len(t, keys, 1)
	})
	t.Run("remote user", func(t *testing.T) {
		h, repo := newResolveHandler(t)
		host := "remote.example"
		repo.Users["u1"].Host = &host
		u, _, missing, err := h.resolvePasskeyKey("AAEC", credentialIDString)
		require.NoError(t, err)
		assert.True(t, missing)
		assert.Nil(t, u.Host, "the stand-in must not be the remote user row")
	})
}

func TestPasskeyCredentialID(t *testing.T) {
	cases := []struct {
		cred string
		id   string
		kind credentialIDKind
	}{
		{`{"id":"AAEC"}`, "AAEC", credentialIDString},
		{`{"id":""}`, "", credentialIDString},
		{`{"id":null}`, "", credentialIDMissing},
		{`{"id":1}`, "", credentialIDNotString},
		{`{"id":{"a":1}}`, "", credentialIDNotString},
		{`{}`, "", credentialIDMissing},
		{`true`, "", credentialIDMissing},
		{`"AAEC"`, "", credentialIDMissing},
		{`[]`, "", credentialIDMissing},
	}
	for _, tc := range cases {
		t.Run(tc.cred, func(t *testing.T) {
			id, kind := passkeyCredentialID(json.RawMessage(tc.cred))
			assert.Equal(t, tc.id, id)
			assert.Equal(t, tc.kind, kind)
		})
	}
}

func TestPasskeyFailureID(t *testing.T) {
	cases := map[error]string{
		twofactor.ErrWebAuthnSessionNotFound:      "2d16e51c-007b-4edd-afd2-f7dd02c947f6",
		twofactor.ErrWebAuthnUnknownKey:           "36b96a7d-b547-412d-aeed-2d611cdc8cdc",
		twofactor.ErrWebAuthnAssertionNotVerified: "932c904e-9460-45b7-9ce6-7ed33be7eb2c",
		twofactor.ErrWebAuthnVerificationFailed:   "b18c89a7-5b5e-4cec-bb5b-0419f332d430",
		twofactor.ErrWebAuthnCounterRollback:      "b18c89a7-5b5e-4cec-bb5b-0419f332d430",
	}
	for err, want := range cases {
		id, ok := passkeyFailureID(fmt.Errorf("wrapped: %w", err))
		assert.True(t, ok, err.Error())
		assert.Equal(t, want, id, err.Error())
	}
	_, ok := passkeyFailureID(assert.AnError)
	assert.False(t, ok, "infrastructure errors are not authentication failures")
}

// inMemorySKInternal: helpers_test.go (内部 package) からも使える簡易 stub。
// handler_2fa_test.go の inMemorySK は signin_test package なので別途定義する。
type inMemorySKInternal struct {
	keys map[string][]*model.UserSecurityKey
}

func (r *inMemorySKInternal) Create(*model.UserSecurityKey) error { return nil }
func (r *inMemorySKInternal) FindByID(id string) (*model.UserSecurityKey, error) {
	for _, keys := range r.keys {
		for _, k := range keys {
			if k.ID == id {
				return k, nil
			}
		}
	}
	return nil, testutil.ErrNotFound
}
func (r *inMemorySKInternal) ListByUser(userID string) ([]*model.UserSecurityKey, error) {
	return r.keys[userID], nil
}
func (r *inMemorySKInternal) UpdateName(string, string, string) error { return nil }
func (r *inMemorySKInternal) UpdateCounter(string, int64) error       { return nil }
func (r *inMemorySKInternal) Delete(string, string) error             { return nil }
func (r *inMemorySKInternal) DeleteByUser(string) error               { return nil }
func (r *inMemorySKInternal) CountByUser(string) (int64, error)       { return 0, nil }

// helper: build an echo.Context with empty body.
func newCtx() echo.Context {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/signin-with-passkey", strings.NewReader(""))
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

// finishPasskeySignin の各分岐 (success / suspended / no profile / passwordless
// 無効) を直接テストする。webauthn signature verify success path はユニットで
// 再現できないのでこの helper 経由で網羅する (#705)。
func TestFinishPasskeySignin_NilUser(t *testing.T) {
	h := &Handler{}
	c := newCtx()
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	require.NoError(t, h.finishPasskeySignin(c, nil, nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	// #2081: user==null は upstream SigninWithPasskeyApiService:155 の 652f899f。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "652f899f-66d4-490e-993e-6606c8ec04c3", resp["error"].(map[string]any)["id"])
}

func TestFinishPasskeySignin_Suspended(t *testing.T) {
	h := &Handler{}
	c := newCtx()
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	user := &model.User{ID: "u1", IsSuspended: true}
	require.NoError(t, h.finishPasskeySignin(c, user, nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "e03a5f46-d309-4865-9b69-56282d94e1eb", errMap["id"])
}

func TestFinishPasskeySignin_NoProfile(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1"}
	// プロフィールを登録しない → FindProfileByUserID が err
	h := NewHandler(repo)
	c := newCtx()
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	user := &model.User{ID: "u1"}
	require.NoError(t, h.finishPasskeySignin(c, user, nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestFinishPasskeySignin_PasswordlessNotEnabled(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1"}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", UsePasswordLessLogin: false}
	h := NewHandler(repo)
	c := newCtx()
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	user := &model.User{ID: "u1"}
	require.NoError(t, h.finishPasskeySignin(c, user, nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "2d84773e-f7b7-4d0b-8f72-bb69b584c912", errMap["id"])
}

// パスキーの失敗経路が IP を記録しないことを固定する (#3135)。
//
// **静的ゲートだけでは塞がらない。** `internal/entitycompat` の allowlist は
// `<file>#<func>` の粒度なので、**allowlist 済みの `finishPasskeySignin` の中に
// `Record` を足す**変異も、そこから allowlist 済みの `RecordSuccessfulSignin` を
// 呼ぶ変異も、call site の一覧としては何も変わらない (実測でどちらも素通りした)。
// しかも `signin-with-passkey` は振る舞いテストを 1 つも持っていなかったので、
// 両側とも空白だった。
//
// **成功側も同じテストで見る。** 失敗側だけだと、recorder を配線し忘れた状態
// (= 何をしても記録されない) が緑で通る。
func TestFinishPasskeySignin_FailuresDoNotRecordIP(t *testing.T) {
	newHandler := func(t *testing.T, passwordless bool, withProfile bool) (*Handler, *countingIPRecorder) {
		t.Helper()
		repo := testutil.NewMockUserRepository()
		tok := "Tk-1"
		repo.Users["u1"] = &model.User{ID: "u1", Token: &tok}
		if withProfile {
			repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", UsePasswordLessLogin: passwordless}
		}
		h := NewHandler(repo)
		rec := &countingIPRecorder{}
		h.SetIPRecorder(rec)
		return h, rec
	}

	// **`nil user` と `suspended` は profile を引く前に return する**ので、fixture の
	// profile の有無は結果に効かない。効くケースと同じ表に混ぜると「この分岐は profile の
	// 状態に依存する」と誤読させる (実測: 反転しても緑) ので、分けてある。
	for _, tc := range []struct {
		name string
		user *model.User
	}{
		{name: "nil user", user: nil},
		{name: "suspended", user: &model.User{ID: "u1", IsSuspended: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ipRec := newHandler(t, true, true)
			c := newCtx()
			rec := c.Response().Writer.(*httptest.ResponseRecorder)
			require.NoError(t, h.finishPasskeySignin(c, tc.user, nil))
			require.Equal(t, http.StatusForbidden, rec.Code)
			assert.Zero(t, ipRec.n(), "失敗したパスキー認証の IP を記録している")
		})
	}

	for _, tc := range []struct {
		name         string
		withProfile  bool
		passwordless bool
	}{
		{name: "no profile", withProfile: false},
		{name: "passwordless disabled", withProfile: true, passwordless: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ipRec := newHandler(t, tc.passwordless, tc.withProfile)
			c := newCtx()
			rec := c.Response().Writer.(*httptest.ResponseRecorder)
			require.NoError(t, h.finishPasskeySignin(c, &model.User{ID: "u1"}, nil))
			require.Equal(t, http.StatusForbidden, rec.Code)
			assert.Zero(t, ipRec.n(), "失敗したパスキー認証の IP を記録している")
		})
	}

	t.Run("success records", func(t *testing.T) {
		h, ipRec := newHandler(t, true, true)
		c := newCtx()
		rec := c.Response().Writer.(*httptest.ResponseRecorder)
		require.NoError(t, h.finishPasskeySignin(c, &model.User{ID: "u1"}, nil))
		require.Equal(t, http.StatusOK, rec.Code)
		got := ipRec.seen()
		require.Len(t, got, 1, "成功したパスキー認証の IP が記録されていない")
		assert.Equal(t, "u1", got[0].userID)
		// `newCtx` の `httptest.NewRequest` 既定 RemoteAddr (`192.0.2.1:1234`) から
		// `c.RealIP()` が返す値。
		assert.Equal(t, "192.0.2.1", got[0].ip, "呼び出し元と別の IP を記録している")
	})
}

// countingIPRecorder は**引数まで残す**。争点は「誰の IP が誰に紐づくか」なので、
// 件数だけだと `Record("someone-else", "10.0.0.1")` に差し替える変異が素通りする
// (実測: パッケージ全体でも誰も見ていなかった)。
type countingIPRecorder struct {
	mu    sync.Mutex
	calls []struct{ userID, ip string }
}

func (r *countingIPRecorder) Record(userID, ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct{ userID, ip string }{userID, ip})
}

func (r *countingIPRecorder) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *countingIPRecorder) seen() []struct{ userID, ip string } {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]struct{ userID, ip string }(nil), r.calls...)
}

func TestFinishPasskeySignin_Success(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	tok := "Tk-1"
	repo.Users["u1"] = &model.User{ID: "u1", Token: &tok}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", UsePasswordLessLogin: true}
	h := NewHandler(repo)
	c := newCtx()
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	user := &model.User{ID: "u1", Token: &tok}
	require.NoError(t, h.finishPasskeySignin(c, user, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	signinResp, ok := resp["signinResponse"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, signinResp["finished"])
	assert.Equal(t, "u1", signinResp["id"])
	assert.Equal(t, "Tk-1", signinResp["i"])
}

func TestFinishPasskeySignin_DoesNotMigrateArgon2(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	tok := "Tk-1"
	stored := "$argon2id$v=19$m=65536,t=3,p=4$unchanged$unchanged"
	repo.Users["u1"] = &model.User{ID: "u1", Token: &tok}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &stored, UsePasswordLessLogin: true}
	h := NewHandler(repo)
	c := newCtx()
	c.Set(pendingPasswordMigrationKey, pendingPasswordMigration{stored: stored, plain: "argon-pass"})
	user := &model.User{ID: "u1", Token: &tok}

	require.NoError(t, h.finishPasskeySignin(c, user, nil))
	assert.Equal(t, stored, *repo.Profiles["u1"].Password)
}

func TestSetPendingPasswordMigration_RequiresVerifiedPassword(t *testing.T) {
	c := newCtx()

	setPendingPasswordMigration(c, password.SchemeArgon2id, false, "stored", "wrong")

	assert.Nil(t, c.Get(pendingPasswordMigrationKey))
}

// recordSignin は signinRepo.Create が err を返しても panic しないこと
// (slog warn して return)。mainStreamPublisher が nil なのでそのまま終わる。
type errSigninRepo struct{}

func (errSigninRepo) Create(*model.Signin) error { return assert.AnError }
func (errSigninRepo) ListByUserID(string, int, string, string) ([]*model.Signin, error) {
	return nil, nil
}

type fixedIDGen struct{}

func (fixedIDGen) Generate(_ time.Time) string           { return "test-id" }
func (fixedIDGen) ParseTime(_ string) (time.Time, error) { return time.Time{}, nil }

func TestRecordSignin_RepoError(t *testing.T) {
	h := &Handler{signinRepo: errSigninRepo{}, idGen: fixedIDGen{}}
	hdrs := http.Header{}
	hdrs.Set("X-Test", "1")
	// Create err でも return すること
	h.recordSignin("u1", "1.2.3.4", hdrs, true)
	// 失敗履歴 (success:false) 経路でも Create err を握りつぶす。
	h.recordSignin("u1", "1.2.3.4", hdrs, false)
}

// counter update / ipLogger / signinRepo の hook が全て繋がった success path。
type recCounterRepo struct{ called bool }

func (r *recCounterRepo) Create(*model.UserSecurityKey) error { return nil }
func (r *recCounterRepo) FindByID(string) (*model.UserSecurityKey, error) {
	return nil, testutil.ErrNotFound
}
func (r *recCounterRepo) ListByUser(string) ([]*model.UserSecurityKey, error) { return nil, nil }
func (r *recCounterRepo) UpdateName(string, string, string) error             { return nil }
func (r *recCounterRepo) UpdateCounter(string, int64) error                   { r.called = true; return nil }
func (r *recCounterRepo) Delete(string, string) error                         { return nil }
func (r *recCounterRepo) DeleteByUser(string) error                           { return nil }
func (r *recCounterRepo) CountByUser(string) (int64, error)                   { return 0, nil }

type stubIPRecorder struct{ logged chan struct{} }

func (s *stubIPRecorder) Record(_, _ string) {
	select {
	case s.logged <- struct{}{}:
	default:
	}
}

func TestFinishPasskeySignin_HooksFire(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	tok := "Tk-2"
	repo.Users["u1"] = &model.User{ID: "u1", Token: &tok}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", UsePasswordLessLogin: true}
	h := NewHandler(repo)
	skRepo := &recCounterRepo{}
	h.SetWebAuthn(nil, skRepo) // WebAuthn nil でも skRepo だけ注入できる

	logged := make(chan struct{}, 1)
	h.SetIPRecorder(&stubIPRecorder{logged: logged})
	signinRepo := testutil.NewMockSigninRepository()
	h.SetSigninRepo(signinRepo, fixedIDGen{})

	c := newCtx()
	rec := c.Response().Writer.(*httptest.ResponseRecorder)
	user := &model.User{ID: "u1", Token: &tok}

	// ダミー credential — go-webauthn の Credential 型を使う。
	cred := makeStubWebauthnCred()
	require.NoError(t, h.finishPasskeySignin(c, user, cred))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, skRepo.called, "counter update must be called when securityKeyRepo set")

	// 非同期 hook の完了を緩く待つ
	select {
	case <-logged:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ipLogger was not invoked")
	}
}
