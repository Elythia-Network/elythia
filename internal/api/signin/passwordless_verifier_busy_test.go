package signin_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// 枠が枯れても passkey 利用者は credential 分岐に到達できる (#2853)。
//
// **503 で終わらせない。** `OutcomeUnavailable` は「password が正しいか
// 分からない」であって「間違っている」ではない。passkey を持っているのに
// パスワード検証の輻輳でログインできないのは筋が通らない。
//
// ctx をキャンセルすると Argon2id 経路が OutcomeUnavailable になるので、
// 「503 なら手前で止まった / passkey 検証失敗の 403 なら到達した」で判別できる。
func TestSigninFlow_PasswordlessReachesCredentialWhenVerifierBusy(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis container unavailable")
	}
	h, repo := newTestHandler(t)
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{
		"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
	}})

	user := newTestUserWithTOTP(repo, "alice", "unused", "JBSWY3DPEHPK3PXP", nil)
	stored := signinArgon2Fixture("argon-pass")
	repo.Profiles[user.ID].Password = &stored
	repo.Profiles[user.ID].UsePasswordLessLogin = true

	rec := doPostCanceled(h.SigninFlow,
		`{"username":"alice","password":"argon-pass","credential":{"id":"x"}}`)

	require.Equal(t, http.StatusForbidden, rec.Code, "503 で止まっている: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "93b86c4b-72f9-40eb-9815-798928603d1e",
		"passkey 検証失敗ではない error id が返っている")
}

// passwordless でない利用者は枠が枯れたら従来どおり 503 (#2853)。
//
// **これを緩めると「枠が取れなかった」が「パスワードが違う」に化ける。**
func TestSigninFlow_NonPasswordlessStill503WhenVerifierBusy(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis container unavailable")
	}
	h, repo := newTestHandler(t)
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{
		"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
	}})

	user := newTestUserWithTOTP(repo, "alice", "unused", "JBSWY3DPEHPK3PXP", nil)
	stored := signinArgon2Fixture("argon-pass")
	repo.Profiles[user.ID].Password = &stored
	repo.Profiles[user.ID].UsePasswordLessLogin = false

	rec := doPostCanceled(h.SigninFlow,
		`{"username":"alice","password":"argon-pass","credential":{"id":"x"}}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "503 でなくなっている: %s", rec.Body.String())
}

// 2FA 無効 / credential 無しでは従来どおり 503 (#2853)。
//
// **どちらも credential 分岐に到達しない。** 素通しにする先が無いので、
// 503 を返さないと「枠が取れなかった」が別の失敗に化ける。
func TestSigninFlow_TolerateNeeds2FAAndCredential(t *testing.T) {
	for _, tt := range []struct {
		name       string
		twoFactor  bool
		credential string
	}{
		{name: "2FA 無効", twoFactor: false, credential: `,"credential":{"id":"x"}`},
		{name: "credential 無し", twoFactor: true, credential: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			user := newTestUserWithTOTP(repo, "alice", "unused", "JBSWY3DPEHPK3PXP", nil)
			stored := signinArgon2Fixture("argon-pass")
			repo.Profiles[user.ID].Password = &stored
			repo.Profiles[user.ID].UsePasswordLessLogin = true
			repo.Profiles[user.ID].TwoFactorEnabled = tt.twoFactor

			rec := doPostCanceled(h.SigninFlow,
				`{"username":"alice","password":"argon-pass"`+tt.credential+`}`)

			assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
				"503 でなくなっている: %s", rec.Body.String())
		})
	}
}

// 枠があるときは移行が従来どおり発火する (#2853)。
//
// **これは token 経路を見ている。** `migratePendingPassword` は `h.ok` 内でしか
// 走らず、credential 経路は WebAuthn assertion を通せないとログインが完了
// しないので、テストからは移行そのものを観測できない。credential 経路で検証が
// 走ること (= 移行の前提) は `PasswordlessRehashesOnCredentialPath` が担保する。
// **両方揃って初めて「検証を丸ごと飛ばす」形を弾ける**ので、片方だけ消さないこと。
func TestSigninFlow_PasswordlessStillMigratesWhenVerifierAvailable(t *testing.T) {
	h, repo := newTestHandler(t)
	secret := "JBSWY3DPEHPK3PXP"
	user := newTestUserWithTOTP(repo, "alice", "unused", secret, nil)
	stored := signinArgon2Fixture("argon-pass")
	repo.Profiles[user.ID].Password = &stored
	repo.Profiles[user.ID].UsePasswordLessLogin = true
	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	rec := doPost(h.SigninFlow,
		`{"username":"alice","password":"argon-pass","token":"`+token+`"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := repo.Profiles[user.ID].Password
	require.NotNil(t, after)
	assert.NotEqual(t, stored, *after, "Argon2id から bcrypt への移行が発火していない")
}

// 枠があるときは bcrypt の再ハッシュも従来どおり発火する (#2853)。
func TestSigninFlow_PasswordlessStillRehashesWhenVerifierAvailable(t *testing.T) {
	h, repo := newTestHandler(t)
	weak, err := bcrypt.GenerateFromPassword([]byte("correct-pass"), bcrypt.MinCost)
	require.NoError(t, err)
	stored := string(weak)
	user := newTestUserWithTOTP(repo, "alice", "unused", "JBSWY3DPEHPK3PXP", nil)
	repo.Profiles[user.ID].Password = &stored
	repo.Profiles[user.ID].UsePasswordLessLogin = true

	rec := doPost(h.SigninFlow, `{"username":"alice","password":"correct-pass"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := repo.Profiles[user.ID].Password
	require.NotNil(t, after)
	assert.NotEqual(t, stored, *after, "cost が古いままで再ハッシュが発火していない")
}

// passwordless + 2FA 無効でも、正しいパスワードは従来どおり通る (#2853)。
//
// **#2849 で `TwoFactorEnabled` を落として回帰させた組み合わせ。**
// `2fa/unregister` は usePasswordLessLogin を戻さないので到達可能。
func TestSigninFlow_PasswordlessWithout2FAAcceptsPassword(t *testing.T) {
	h, repo := newTestHandler(t)
	user := createTestUser(repo, "alice", "correct-pass")
	repo.Profiles[user.ID].UsePasswordLessLogin = true
	repo.Profiles[user.ID].TwoFactorEnabled = false

	rec := doPost(h.SigninFlow,
		`{"username":"alice","password":"correct-pass","credential":{"id":"x"}}`)

	assert.Equal(t, http.StatusOK, rec.Code, "正しいパスワードが通らない: %s", rec.Body.String())
}

// 枠が枯れても「検証済み」にはしない (#2853)。
//
// **これは tolerate しない側を見ている。** token を送っているので
// `tolerateUnavailable` は成立せず (token 節)、`verifyPassword` の既定経路で
// 503 になる。`OutcomeUnavailable` を「検証済み」に倒すと、誤ったパスワード +
// 有効な TOTP でログインが成立してしまう。
//
// **tolerate 側の `ok=false` を固定しているのは
// `TestSigninFlow_DoesNotStageMigrationForUnverifiedPassword`** (internal package)。
// あちらは未検証の平文が移行対象に積まれないことを見る。
func TestSigninFlow_UnavailableIsNotTreatedAsVerified(t *testing.T) {
	h, repo := newTestHandler(t)
	secret := "JBSWY3DPEHPK3PXP"
	user := newTestUserWithTOTP(repo, "alice", "unused", secret, nil)
	stored := signinArgon2Fixture("argon-pass")
	repo.Profiles[user.ID].Password = &stored
	repo.Profiles[user.ID].UsePasswordLessLogin = true
	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	// token があるので tolerate は成立しない (credential は無関係)。
	rec := doPostCanceled(h.SigninFlow,
		`{"username":"alice","password":"WRONG","token":"`+token+`","credential":{"id":"x"}}`)

	assert.NotEqual(t, http.StatusOK, rec.Code,
		"未検証のパスワードでログインが成立している: %s", rec.Body.String())
}

// token が来ていたら枠が枯れても従来どおり 503 (#2853)。
//
// **token 分岐は credential 分岐より前にあり `passwordOK` を要求する。**
// そこへ ok=false で落とすと、正しい password でも 403 (「パスワードが違います」)
// になり、身に覚えのない失敗ログイン履歴まで残る。
func TestSigninFlow_TokenPresentStill503WhenVerifierBusy(t *testing.T) {
	h, repo := newTestHandler(t)
	secret := "JBSWY3DPEHPK3PXP"
	user := newTestUserWithTOTP(repo, "alice", "unused", secret, nil)
	stored := signinArgon2Fixture("argon-pass")
	repo.Profiles[user.ID].Password = &stored
	repo.Profiles[user.ID].UsePasswordLessLogin = true
	token, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	rec := doPostCanceled(h.SigninFlow,
		`{"username":"alice","password":"argon-pass","token":"`+token+`","credential":{"id":"x"}}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"正しいパスワードが 403 に化けている: %s", rec.Body.String())
}

// credential 経路でも枠があれば再ハッシュが発火する (#2853)。
//
// **検証ごと飛ばす実装を弾く唯一のテスト。** `maybeRehashPassword` は検証時に
// 即座に走るので、passkey の検証が失敗して 403 で終わる要求でも cost の更新は
// 起きる。これが起きなくなる形は、Argon2id 移行が止まる形と同じ
// (移行は `h.ok` 内なので credential 経路では直接観測できない)。
func TestSigninFlow_PasswordlessRehashesOnCredentialPath(t *testing.T) {
	if signinTestRedis == nil {
		t.Skip("redis container unavailable")
	}
	h, repo := newTestHandler(t)
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{
		"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
	}})

	weak, err := bcrypt.GenerateFromPassword([]byte("correct-pass"), bcrypt.MinCost)
	require.NoError(t, err)
	stored := string(weak)
	user := newTestUserWithTOTP(repo, "alice", "unused", "JBSWY3DPEHPK3PXP", nil)
	repo.Profiles[user.ID].Password = &stored
	repo.Profiles[user.ID].UsePasswordLessLogin = true

	// passkey は失敗する (403) が、その手前で検証と再ハッシュは済んでいる。
	rec := doPost(h.SigninFlow,
		`{"username":"alice","password":"correct-pass","credential":{"id":"x"}}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	after := repo.Profiles[user.ID].Password
	require.NotNil(t, after)
	assert.NotEqual(t, stored, *after, "credential 経路で再ハッシュが発火していない")
}
