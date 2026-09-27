package twofactor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var twofaTestRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Printf("twofactor test: redis setup failed: %v", err)
		os.Exit(m.Run())
	}
	twofaTestRedis = tr
	code := m.Run()
	twofaTestRedis.Teardown(ctx)
	os.Exit(code)
}

// requireRedis skips when testcontainers Redis is unavailable.
func requireRedis(t *testing.T) {
	t.Helper()
	if twofaTestRedis == nil {
		t.Skip("redis testcontainer not available")
	}
}

// --- NewWebAuthnService ---

func TestNewWebAuthnService_EmptyURL(t *testing.T) {
	_, err := NewWebAuthnService("", "Misskey", nil)
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestNewWebAuthnService_BadURL(t *testing.T) {
	_, err := NewWebAuthnService("not a url", "Misskey", nil)
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestNewWebAuthnService_OK(t *testing.T) {
	svc, err := NewWebAuthnService("https://example.com", "Misskey", nil)
	require.NoError(t, err)
	assert.NotNil(t, svc.wa)
}

// --- session helpers (real Redis) ---

func TestPutAndTakeLoginSession_Roundtrip(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)

	sd := makeFakeSessionData()
	require.NoError(t, svc.putLoginSession(context.Background(), "alice", sd))

	got, err := svc.takeLoginSession(context.Background(), "alice")
	require.NoError(t, err)
	assert.Equal(t, sd.Challenge, got.Challenge)

	// take は single-use: 2 回目は ErrWebAuthnSessionNotFound
	_, err = svc.takeLoginSession(context.Background(), "alice")
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

func TestPutLoginSession_Overwrites(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)

	first := makeFakeSessionData()
	first.Challenge = "first"
	second := makeFakeSessionData()
	second.Challenge = "second"

	require.NoError(t, svc.putLoginSession(context.Background(), "alice", first))
	require.NoError(t, svc.putLoginSession(context.Background(), "alice", second))

	got, err := svc.takeLoginSession(context.Background(), "alice")
	require.NoError(t, err)
	assert.Equal(t, "second", got.Challenge)
}

func TestTakeLoginSession_Missing(t *testing.T) {
	requireRedis(t)
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	_, err = svc.takeLoginSession(context.Background(), "ghost")
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

func TestTakeLoginSession_NilRedis(t *testing.T) {
	svc := &WebAuthnService{wa: nil, redis: nil}
	_, err := svc.takeLoginSession(context.Background(), "alice")
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestPutLoginSession_NilRedis(t *testing.T) {
	svc := &WebAuthnService{wa: nil, redis: nil}
	err := svc.putLoginSession(context.Background(), "alice", makeFakeSessionData())
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

// --- BeginRegistration / BeginLogin (exercise wiring with real config) ---

func TestBeginRegistration(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	creation, err := svc.BeginRegistration(context.Background(), &model.User{ID: "alice", Username: "alice"}, nil)
	require.NoError(t, err)
	require.NotNil(t, creation)
	assert.NotEmpty(t, creation.Response.Challenge)
	// registration session が user 単位 1 件で保存されていること
	got, err := svc.takeRegistrationSession(context.Background(), "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, got.Challenge)
}

func TestBeginLogin(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	// 鍵が無いと go-webauthn は ErrBadRequest を返す。空でも sd 生成は試みる。
	// アサーション: 鍵 0 個でも内部で AllowedCredentials=[] になる動作確認。
	_, err = svc.BeginLogin(context.Background(), &model.User{ID: "alice", Username: "alice"}, nil)
	// 鍵 0 個だと "Found no credentials" 系のエラーになるので、ここでは
	// エラーが返ることを許容する (実装依存)。
	if err == nil {
		t.Log("BeginLogin succeeded with empty credentials (newer go-webauthn)")
	}
}

func TestBeginRegistration_NotConfigured(t *testing.T) {
	svc := &WebAuthnService{}
	_, err := svc.BeginRegistration(context.Background(), &model.User{ID: "x"}, nil)
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestBeginLogin_NotConfigured(t *testing.T) {
	svc := &WebAuthnService{}
	_, err := svc.BeginLogin(context.Background(), &model.User{ID: "x"}, nil)
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestFinishRegistration_NotConfigured(t *testing.T) {
	svc := &WebAuthnService{}
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	_, err := svc.FinishRegistration(context.Background(), &model.User{ID: "x"}, nil, req)
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestFinishLogin_NotConfigured(t *testing.T) {
	svc := &WebAuthnService{}
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	_, err := svc.FinishLogin(context.Background(), &model.User{ID: "x"}, nil, req)
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestFinishRegistration_SessionMissing(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	_, err = svc.FinishRegistration(context.Background(), &model.User{ID: "alice"}, nil, req)
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

func TestFinishLogin_SessionMissing(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	_, err = svc.FinishLogin(context.Background(), &model.User{ID: "alice"}, nil, req)
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

// session が存在するが credential body が壊れている場合、wa.FinishLogin が
// エラーを返す path。session 読み出し → wa.FinishLogin → err を全て exercise。
func TestFinishLogin_BadCredential(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	require.NoError(t, svc.putLoginSession(context.Background(), "alice", makeFakeSessionData()))
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"id":"x","rawId":"x","type":"public-key","response":{}}`))
	req.Header.Set("Content-Type", "application/json")
	_, err = svc.FinishLogin(context.Background(), &model.User{ID: "alice", Username: "alice"}, nil, req)
	assert.Error(t, err)
}

// 同じく FinishPasskeyLogin の wa.FinishDiscoverableLogin err path。
func TestFinishPasskeyLogin_BadCredential(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	require.NoError(t, svc.putPasskeySession(context.Background(), "ctx", makeFakeSessionData()))
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"id":"x","rawId":"x","type":"public-key","response":{}}`))
	req.Header.Set("Content-Type", "application/json")
	resolver := func(rawID, userHandle []byte) (*model.User, []*model.UserSecurityKey, error) {
		return &model.User{ID: "alice"}, nil, nil
	}
	_, _, err = svc.FinishPasskeyLogin(context.Background(), "ctx", req, resolver)
	assert.Error(t, err)
}

// FinishPasskeyLogin の resolver closure を発火させる path。assertion body
// は go-webauthn upstream の TestFinishLoginFailure 用 fixture を流用 (RPID =
// webauthn.io)。resolver が err を返すと FinishDiscoverableLogin が err 伝播
// するので、closure 内の `if err != nil` 分岐をカバーできる。
func TestFinishPasskeyLogin_ResolverError(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://webauthn.io", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)

	const (
		credentialID = "AI7D5q2P0LS-Fal9ZT7CHM2N5BLbUunF92T8b6iYC199bO2kagSuU05-5dZGqb1SP0A0lyTWng"
		userHandle   = "0ToAAAAAAAAAAA"
		challenge    = "E4PTcIH_HfX1pC6Sigk1SC9NAlgeztN0439vi8z_c9k"
	)

	byteUserHandle, _ := base64.RawURLEncoding.DecodeString(userHandle)
	// passkey (discoverable) session は UserID 空 が必須 (go-webauthn が
	// ValidatePasskeyLogin の先頭でチェックする)。
	sd := &webauthn.SessionData{
		Challenge: challenge,
	}
	require.NoError(t, svc.putPasskeySession(context.Background(), "ctx", sd))

	body := []byte(`{
		"id":"` + credentialID + `",
		"rawId":"` + credentialID + `",
		"type":"public-key",
		"response":{
			"authenticatorData":"dKbqkhPJnC90siSSsyDPQCYqlMGpUKA5fyklC2CEHvBFXJJiGa3OAAI1vMYKZIsLJfHwVQMANwCOw-atj9C0vhWpfWU-whzNjeQS21Lpxfdk_G-omAtffWztpGoErlNOfuXWRqm9Uj9ANJck1p6lAQIDJiABIVggKAhfsdHcBIc0KPgAcRyAIK_-Vi-nCXHkRHPNaCMBZ-4iWCBxB8fGYQSBONi9uvq0gv95dGWlhJrBwCsj_a4LJQKVHQ",
			"clientDataJSON":"eyJjaGFsbGVuZ2UiOiJFNFBUY0lIX0hmWDFwQzZTaWdrMVNDOU5BbGdlenROMDQzOXZpOHpfYzlrIiwibmV3X2tleXNfbWF5X2JlX2FkZGVkX2hlcmUiOiJkbyBub3QgY29tcGFyZSBjbGllbnREYXRhSlNPTiBhZ2FpbnN0IGEgdGVtcGxhdGUuIFNlZSBodHRwczovL2dvby5nbC95YWJQZXgiLCJvcmlnaW4iOiJodHRwczovL3dlYmF1dGhuLmlvIiwidHlwZSI6IndlYmF1dGhuLmdldCJ9",
			"signature":"MEUCIBtIVOQxzFYdyWQyxaLR0tik1TnuPhGVhXVSNgFwLmN5AiEAnxXdCq0UeAVGWxOaFcjBZ_mEZoXqNboY5IkQDdlWZYc",
			"userHandle":"` + userHandle + `"
		}
	}`)

	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resolveCalled := false
	resolver := func(rawID, userHandleArg []byte) (*model.User, []*model.UserSecurityKey, error) {
		resolveCalled = true
		assert.Equal(t, byteUserHandle, userHandleArg)
		return nil, nil, errors.New("resolver fail")
	}
	_, _, err = svc.FinishPasskeyLogin(context.Background(), "ctx", req, resolver)
	assert.Error(t, err)
	assert.True(t, resolveCalled, "resolver closure must be invoked when body parses")
}

// --- userAdapter ---

func TestUserAdapter_Methods(t *testing.T) {
	displayName := "Alice in Wonderland"
	u := &model.User{ID: "alice-id", Username: "alice", Name: &displayName}
	a := &userAdapter{user: u, keys: nil}
	assert.Equal(t, []byte("alice-id"), a.WebAuthnID())
	assert.Equal(t, "alice", a.WebAuthnName())
	assert.Equal(t, displayName, a.WebAuthnDisplayName())
	assert.Empty(t, a.WebAuthnCredentials())
}

func TestUserAdapter_DisplayNameFallback(t *testing.T) {
	u := &model.User{ID: "x", Username: "alice"}
	a := &userAdapter{user: u}
	assert.Equal(t, "alice", a.WebAuthnDisplayName())
}

func TestUserAdapter_DisplayNameEmptyName(t *testing.T) {
	empty := ""
	u := &model.User{ID: "x", Username: "alice", Name: &empty}
	a := &userAdapter{user: u}
	assert.Equal(t, "alice", a.WebAuthnDisplayName())
}

func TestUserAdapter_CredentialsRoundtrip(t *testing.T) {
	keys := []*model.UserSecurityKey{
		{
			ID:         "AAEC", // base64url("\x00\x01\x02")
			PublicKey:  "AwQF",
			Counter:    42,
			Transports: model.StringArray{"usb", "nfc"},
		},
	}
	a := &userAdapter{user: &model.User{ID: "x"}, keys: keys}
	creds := a.WebAuthnCredentials()
	require.Len(t, creds, 1)
	assert.Equal(t, []byte{0, 1, 2}, creds[0].ID)
	assert.Equal(t, []byte{3, 4, 5}, creds[0].PublicKey)
	assert.Equal(t, uint32(42), creds[0].Authenticator.SignCount)
	assert.Len(t, creds[0].Transport, 2)
}

// DB に保存された credentialDeviceType=multiDevice / credentialBackedUp=true
// (Cloud sync passkey) が webauthn.Credential.Flags に正しく復元される (#707)。
// 復元しないと go-webauthn の認証時に Backup Eligible mismatch エラーになる。
func TestUserAdapter_RestoresBackupFlags(t *testing.T) {
	device := "multiDevice"
	backedUp := true
	keys := []*model.UserSecurityKey{
		{
			ID:                   "AAEC",
			PublicKey:            "AwQF",
			Counter:              1,
			CredentialDeviceType: &device,
			CredentialBackedUp:   &backedUp,
		},
	}
	a := &userAdapter{user: &model.User{ID: "x"}, keys: keys}
	creds := a.WebAuthnCredentials()
	require.Len(t, creds, 1)
	assert.True(t, creds[0].Flags.BackupEligible)
	assert.True(t, creds[0].Flags.BackupState)
}

// device-bound (singleDevice) は BackupEligible=false で復元される。
func TestUserAdapter_RestoresSingleDeviceFlags(t *testing.T) {
	device := "singleDevice"
	backedUp := false
	keys := []*model.UserSecurityKey{
		{
			ID:                   "AAEC",
			PublicKey:            "AwQF",
			CredentialDeviceType: &device,
			CredentialBackedUp:   &backedUp,
		},
	}
	a := &userAdapter{user: &model.User{ID: "x"}, keys: keys}
	creds := a.WebAuthnCredentials()
	require.Len(t, creds, 1)
	assert.False(t, creds[0].Flags.BackupEligible)
	assert.False(t, creds[0].Flags.BackupState)
}

// 古いレコード (Flag 列が NULL、本 PR 修正前に登録された passkey) は default
// false で復元される。Cloud sync passkey の場合は再登録が必要になる。
func TestUserAdapter_NilFlagsDefaultFalse(t *testing.T) {
	keys := []*model.UserSecurityKey{
		{ID: "AAEC", PublicKey: "AwQF"},
	}
	a := &userAdapter{user: &model.User{ID: "x"}, keys: keys}
	creds := a.WebAuthnCredentials()
	require.Len(t, creds, 1)
	assert.False(t, creds[0].Flags.BackupEligible)
	assert.False(t, creds[0].Flags.BackupState)
}

func TestUserAdapter_SkipsBadEncoding(t *testing.T) {
	keys := []*model.UserSecurityKey{
		{ID: "not!base64url", PublicKey: "AwQF"},
		{ID: "AAEC", PublicKey: "not!base64url"},
		{ID: "AAEC", PublicKey: "AwQF"},
	}
	a := &userAdapter{keys: keys}
	creds := a.WebAuthnCredentials()
	assert.Len(t, creds, 1, "only the third (well-formed) credential survives")
}

// --- CredentialToModel ---

func TestCredentialToModel_NoTransports(t *testing.T) {
	cred := makeFakeCredential([]byte{1, 2, 3}, []byte{4, 5}, 7, nil)
	m := CredentialToModel(cred, "alice", "")
	assert.Equal(t, "alice", m.UserID)
	assert.Equal(t, int64(7), m.Counter)
	assert.NotEmpty(t, m.ID)
	assert.NotEmpty(t, m.PublicKey)
	assert.Empty(t, m.Transports)
}

func TestCredentialToModel_WithTransports(t *testing.T) {
	cred := makeFakeCredential([]byte{1}, []byte{2}, 0, []string{"usb", "ble"})
	m := CredentialToModel(cred, "alice", "Yubikey")
	assert.Equal(t, "Yubikey", m.Name)
	assert.Equal(t, []string{"usb", "ble"}, []string(m.Transports))
}

// BackupEligible / BackupState フラグが DB 列 (credentialDeviceType /
// credentialBackedUp) に正しく保存される (#707)。Cloud sync passkey
// (BE=true) を保存するときに multiDevice として記録される必要がある。
func TestCredentialToModel_BackupEligibleSaved(t *testing.T) {
	cred := makeFakeCredential([]byte{1}, []byte{2}, 0, nil)
	cred.Flags.BackupEligible = true
	cred.Flags.BackupState = true
	m := CredentialToModel(cred, "alice", "Phone")
	require.NotNil(t, m.CredentialDeviceType)
	assert.Equal(t, "multiDevice", *m.CredentialDeviceType)
	require.NotNil(t, m.CredentialBackedUp)
	assert.True(t, *m.CredentialBackedUp)
}

// device-bound (Yubikey 等、BE=false) は singleDevice として記録される。
func TestCredentialToModel_DeviceBoundSaved(t *testing.T) {
	cred := makeFakeCredential([]byte{1}, []byte{2}, 0, nil)
	cred.Flags.BackupEligible = false
	cred.Flags.BackupState = false
	m := CredentialToModel(cred, "alice", "Yubikey")
	require.NotNil(t, m.CredentialDeviceType)
	assert.Equal(t, "singleDevice", *m.CredentialDeviceType)
	require.NotNil(t, m.CredentialBackedUp)
	assert.False(t, *m.CredentialBackedUp)
}

// --- key helpers ---

func TestLoginSessionKey(t *testing.T) {
	assert.Equal(t, "twofa:webauthn:alice:login", loginSessionKey("alice"))
}

func TestPasskeySessionKey(t *testing.T) {
	assert.Equal(t, "twofa:webauthn:passkey:ctx1", passkeySessionKey("ctx1"))
}

// upstream の GHSA は登録 / 2FA 認証 / passkey サインインが同一の
// `webauthn:challenge:` 名前空間を共有していたこと (かつ passkey の context が
// リクエストボディ由来だったこと) が原因だった。mk-go は固定リテラル `passkey:` を
// **可変部の前**に置くので構造的に衝突しない。**キー名を文字列で pin するだけでは
// この性質を検証できない** — 3 系統が互いのキーに化けられないことを直接見る。
//
// userID は base36 なので `:` を含まない。passkey の ctxID に他系統のキー形を
// そのまま流し込んでも、生成されるキーは別物でなければならない。
func TestWebAuthnSessionKeys_CannotCollideAcrossFlows(t *testing.T) {
	const uid = "alice"
	login := loginSessionKey(uid)
	reg := registrationSessionKey(uid)

	// 3 系統が互いに異なる。
	assert.NotEqual(t, login, reg)
	assert.NotEqual(t, login, passkeySessionKey(uid))
	assert.NotEqual(t, reg, passkeySessionKey(uid))

	// passkey の context に他系統のキーの可変部を流し込んでも当たらない。
	for _, ctx := range []string{uid, uid + ":login", uid + ":registration", "alice:login"} {
		got := passkeySessionKey(ctx)
		assert.NotEqual(t, login, got, "passkey ctx %q が login キーに化けた", ctx)
		assert.NotEqual(t, reg, got, "passkey ctx %q が registration キーに化けた", ctx)
		assert.True(t, strings.HasPrefix(got, "twofa:webauthn:passkey:"),
			"passkey キーは固定リテラルで始まる (可変部を前に置くと衝突しうる)")
	}

	// 逆向き: login / registration のキーが passkey 名前空間に入らない。
	assert.False(t, strings.HasPrefix(login, "twofa:webauthn:passkey:"))
	assert.False(t, strings.HasPrefix(reg, "twofa:webauthn:passkey:"))
}

// --- BeginRegistration / BeginLogin Redis error paths ---

func TestBeginRegistration_PutSessionFails(t *testing.T) {
	requireRedis(t)
	// 専用クライアントを開いて閉じる → Set が err を返す
	c := redis.NewClient(&redis.Options{Addr: twofaTestRedis.Client.Options().Addr})
	require.NoError(t, c.Close())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", c)
	require.NoError(t, err)
	_, err = svc.BeginRegistration(context.Background(), &model.User{ID: "alice", Username: "alice"}, nil)
	assert.Error(t, err)
}

// BeginLogin が assertion を返す経路 (鍵が 1 つ以上ある状態)。
// session id は client へ返さず、サーバ側で user-keyed 保存される。
func TestBeginLogin_WithCredential(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	keys := []*model.UserSecurityKey{
		{ID: "AAEC", PublicKey: "AwQF", Counter: 1},
	}
	assertion, err := svc.BeginLogin(context.Background(), &model.User{ID: "alice", Username: "alice"}, keys)
	require.NoError(t, err)
	assert.NotNil(t, assertion)
	// SessionData が Redis に user-keyed で残っていること
	got, err := svc.takeLoginSession(context.Background(), "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, got.Challenge)
}

// BeginLogin で putSession が失敗する経路 (closed redis)
func TestBeginLogin_PutSessionFails(t *testing.T) {
	requireRedis(t)
	c := redis.NewClient(&redis.Options{Addr: twofaTestRedis.Client.Options().Addr})
	require.NoError(t, c.Close())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", c)
	require.NoError(t, err)
	keys := []*model.UserSecurityKey{{ID: "AAEC", PublicKey: "AwQF", Counter: 0}}
	_, err = svc.BeginLogin(context.Background(), &model.User{ID: "alice", Username: "alice"}, keys)
	assert.Error(t, err)
}

// takeSession が Redis 接続エラー (Nil 以外) を伝搬する経路
func TestTakeSession_RedisError(t *testing.T) {
	requireRedis(t)
	// 専用クライアント
	c := redis.NewClient(&redis.Options{Addr: twofaTestRedis.Client.Options().Addr})
	svc, err := NewWebAuthnService("https://example.com", "Misskey", c)
	require.NoError(t, err)
	// 一旦正常に PUT
	sd := makeFakeSessionData()
	require.NoError(t, svc.putLoginSession(context.Background(), "alice", sd))
	// クライアント閉じてから take → connection closed エラー
	require.NoError(t, c.Close())
	_, err = svc.takeLoginSession(context.Background(), "alice")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

// --- BeginPasskeyLogin / passkey session helpers ---

func TestBeginPasskeyLogin(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	assertion, err := svc.BeginPasskeyLogin(context.Background(), "ctx-uuid-1")
	require.NoError(t, err)
	assert.NotNil(t, assertion)
	assert.NotEmpty(t, assertion.Response.Challenge)
	// 同じ context で take すると challenge が読めて、二度目は session not found
	got, err := svc.takePasskeySession(context.Background(), "ctx-uuid-1")
	require.NoError(t, err)
	assert.NotEmpty(t, got.Challenge)
	_, err = svc.takePasskeySession(context.Background(), "ctx-uuid-1")
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

func TestBeginPasskeyLogin_NotConfigured(t *testing.T) {
	svc := &WebAuthnService{}
	_, err := svc.BeginPasskeyLogin(context.Background(), "x")
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestBeginPasskeyLogin_PutSessionFails(t *testing.T) {
	requireRedis(t)
	c := redis.NewClient(&redis.Options{Addr: twofaTestRedis.Client.Options().Addr})
	require.NoError(t, c.Close())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", c)
	require.NoError(t, err)
	_, err = svc.BeginPasskeyLogin(context.Background(), "ctx")
	assert.Error(t, err)
}

func TestFinishPasskeyLogin_NotConfigured(t *testing.T) {
	svc := &WebAuthnService{}
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	_, _, err := svc.FinishPasskeyLogin(context.Background(), "ctx", req, nil)
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestFinishPasskeyLogin_SessionMissing(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	_, _, err = svc.FinishPasskeyLogin(context.Background(), "ghost", req, nil)
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

func TestPutPasskeySession_NilRedis(t *testing.T) {
	svc := &WebAuthnService{}
	err := svc.putPasskeySession(context.Background(), "ctx", makeFakeSessionData())
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestTakePasskeySession_NilRedis(t *testing.T) {
	svc := &WebAuthnService{}
	_, err := svc.takePasskeySession(context.Background(), "ctx")
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestTakePasskeySession_RedisError(t *testing.T) {
	requireRedis(t)
	c := redis.NewClient(&redis.Options{Addr: twofaTestRedis.Client.Options().Addr})
	svc, err := NewWebAuthnService("https://example.com", "Misskey", c)
	require.NoError(t, err)
	require.NoError(t, svc.putPasskeySession(context.Background(), "ctx", makeFakeSessionData()))
	require.NoError(t, c.Close())
	_, err = svc.takePasskeySession(context.Background(), "ctx")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

// --- FinishRegistration / FinishLogin success path tests using W3C spec
//     test vectors (NoneES256). ベクタは go-webauthn 上流の test とほぼ同じ
//     fixture を再利用しているが、こちらは mk-go の WebAuthnService を経由
//     して redis セッション取得 → wa.FinishRegistration の包括動作を網羅する。

// The W3C vector's authenticator data carries flags 0x59 (UP / BE / BS / AT)
// without UV, so it exercises the rejection path. 受理側は
// TestFinishRegistration_RequiresUV が in-process の認証器で見ている。
func TestFinishRegistration_W3CVectorWithoutUVIsRejected(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())

	// W3C webauthn-3 §sctn-test-vectors-none-es256 の attestation 一式。
	// RPID は "example.org", origin は "https://example.org" 固定。
	const (
		attestationObjectHex = "a363666d74646e6f6e656761747453746d74a068617574684461746158a4bfabc37432958b063360d3ad6461c9c4735ae7f8edd46592a5e0f01452b2e4b559000000008446ccb9ab1db374750b2367ff6f3a1f0020f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4a5010203262001215820afefa16f97ca9b2d23eb86ccb64098d20db90856062eb249c33a9b672f26df61225820930a56b87a2fca66334b03458abf879717c12cc68ed73290af2e2664796b9220"
		clientDataJSONHex    = "7b2274797065223a22776562617574686e2e637265617465222c226368616c6c656e6765223a22414d4d507434557878475453746e63647134313759447742466938767049612d7077386f4f755657345441222c226f726967696e223a2268747470733a2f2f6578616d706c652e6f7267222c2263726f73734f726967696e223a66616c73652c22657874726144617461223a22636c69656e74446174614a534f4e206d617920626520657874656e6465642077697468206164646974696f6e616c206669656c647320696e20746865206675747572652c207375636820617320746869733a20426b5165446a646354427258426941774a544c453551227d"
		credentialIDHex      = "f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4"
		challengeHex         = "00c30fb78531c464d2b6771dab8d7b603c01162f2fa486bea70f283ae556e130"
	)
	body := buildAttestationResponse(t, attestationObjectHex, clientDataJSONHex, credentialIDHex)
	challenge := encodeBase64URL(decodeHex(t, challengeHex))

	svc, err := NewWebAuthnService("https://example.org", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)

	// セッションを Redis に直接書き込む (BeginRegistration を呼ばずに固定 fixture
	// の challenge を使うため)
	sd := &webauthn.SessionData{
		Challenge:  challenge,
		UserID:     []byte("test-user-id"),
		CredParams: []protocol.CredentialParameter{{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256}},
	}
	require.NoError(t, svc.putRegistrationSession(context.Background(), "test-user-id", sd))

	httpReq := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	_, err = svc.FinishRegistration(context.Background(), &model.User{ID: "test-user-id", Username: "test"}, nil, httpReq)
	requireUVRejected(t, err)
}

// --- helpers ---

func decodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func encodeBase64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func buildAttestationResponse(t *testing.T, attObjectHex, clientDataJSONHex, credIDHex string) []byte {
	t.Helper()
	credID := decodeHex(t, credIDHex)
	id := encodeBase64URL(credID)
	attObj := encodeBase64URL(decodeHex(t, attObjectHex))
	cdj := encodeBase64URL(decodeHex(t, clientDataJSONHex))
	body, err := json.Marshal(map[string]any{
		"id":    id,
		"rawId": id,
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": attObj,
			"clientDataJSON":    cdj,
		},
	})
	require.NoError(t, err)
	return body
}

// --- registration session helpers (#698) ---
//
// upstream-compat な single-in-flight-per-user 経路。session id round-trip が
// 無いので user 単位で 1 件だけ Redis に保持する。

func TestRegistrationSessionKey(t *testing.T) {
	assert.Equal(t, "twofa:webauthn:alice:registration", registrationSessionKey("alice"))
}

func TestPutAndTakeRegistrationSession_Roundtrip(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)

	sd := makeFakeSessionData()
	require.NoError(t, svc.putRegistrationSession(context.Background(), "alice", sd))

	got, err := svc.takeRegistrationSession(context.Background(), "alice")
	require.NoError(t, err)
	assert.Equal(t, sd.Challenge, got.Challenge)

	// take は single-use: 2 回目は ErrWebAuthnSessionNotFound
	_, err = svc.takeRegistrationSession(context.Background(), "alice")
	assert.ErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

func TestPutRegistrationSession_Overwrites(t *testing.T) {
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService("https://example.com", "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)

	first := makeFakeSessionData()
	first.Challenge = "first"
	second := makeFakeSessionData()
	second.Challenge = "second"

	require.NoError(t, svc.putRegistrationSession(context.Background(), "alice", first))
	require.NoError(t, svc.putRegistrationSession(context.Background(), "alice", second))

	got, err := svc.takeRegistrationSession(context.Background(), "alice")
	require.NoError(t, err)
	assert.Equal(t, "second", got.Challenge)
}

func TestPutRegistrationSession_NilRedis(t *testing.T) {
	svc := &WebAuthnService{}
	err := svc.putRegistrationSession(context.Background(), "alice", makeFakeSessionData())
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestTakeRegistrationSession_NilRedis(t *testing.T) {
	svc := &WebAuthnService{}
	_, err := svc.takeRegistrationSession(context.Background(), "alice")
	assert.ErrorIs(t, err, ErrWebAuthnNotConfigured)
}

func TestTakeRegistrationSession_RedisError(t *testing.T) {
	requireRedis(t)
	c := redis.NewClient(&redis.Options{Addr: twofaTestRedis.Client.Options().Addr})
	svc, err := NewWebAuthnService("https://example.com", "Misskey", c)
	require.NoError(t, err)
	require.NoError(t, svc.putRegistrationSession(context.Background(), "alice", makeFakeSessionData()))
	require.NoError(t, c.Close())
	_, err = svc.takeRegistrationSession(context.Background(), "alice")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrWebAuthnSessionNotFound)
}

// --- ensure unused imports stay tidy ---

var _ = redis.Nil
