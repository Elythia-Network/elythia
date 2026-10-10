package strongauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

const (
	adminID   = "admin1"
	modID     = "mod1"
	password  = "correct horse"
	nativeTok = "native-token"
)

type fakeRoles map[string]bool

func (r fakeRoles) IsAdministrator(id string) bool { return r[id] }

type fakeProfiles struct {
	profiles  map[string]*model.UserProfile
	err       error
	removeErr error
	removed   []string
}

func (p *fakeProfiles) GetProfileErr(id string) (*model.UserProfile, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.profiles[id], nil
}

func (p *fakeProfiles) RemoveBackupCode(userID, code string) error {
	if p.removeErr != nil {
		return p.removeErr
	}
	p.removed = append(p.removed, code)
	pr := p.profiles[userID]
	var rest model.StringArray
	for _, c := range pr.TwoFactorBackupSecret {
		if c != code {
			rest = append(rest, c)
		}
	}
	pr.TwoFactorBackupSecret = rest
	return nil
}

type fakeKeys struct {
	keys     []*model.UserSecurityKey
	err      error
	counters map[string]int64
}

func (k *fakeKeys) ListByUser(string) ([]*model.UserSecurityKey, error) { return k.keys, k.err }
func (k *fakeKeys) UpdateCounter(id string, c int64) error {
	if k.counters == nil {
		k.counters = map[string]int64{}
	}
	k.counters[id] = c
	return nil
}

type fakePasskeys struct {
	beginErr  error
	finishErr error
	finished  int
}

func (p *fakePasskeys) BeginLogin(context.Context, *model.User, []*model.UserSecurityKey) (*protocol.CredentialAssertion, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return &protocol.CredentialAssertion{Response: protocol.PublicKeyCredentialRequestOptions{Challenge: []byte("chal")}}, nil
}

func (p *fakePasskeys) FinishLogin(_ context.Context, _ *model.User, _ []*model.UserSecurityKey, req *http.Request) (*webauthn.Credential, error) {
	p.finished++
	if p.finishErr != nil {
		return nil, p.finishErr
	}
	if req == nil || req.Body == nil {
		return nil, errors.New("no body")
	}
	return &webauthn.Credential{ID: []byte{1, 2, 3}, Authenticator: webauthn.Authenticator{SignCount: 7}}, nil
}

type erringGuard struct{}

func (erringGuard) Begin(context.Context, string, string) (passwordguard.Attempt, error) {
	return nil, errors.New("redis down")
}

type erringReplay struct{}

func (erringReplay) MarkUsed(context.Context, string, string) (bool, error) {
	return false, errors.New("redis down")
}

type fixture struct {
	v        *Verifier
	profiles *fakeProfiles
	keys     *fakeKeys
	passkeys *fakePasskeys
	secret   string
	deps     Deps
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	h := string(hash)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: "admin"})
	require.NoError(t, err)
	secret := key.Secret()
	profiles := &fakeProfiles{profiles: map[string]*model.UserProfile{
		adminID: {
			UserID:                adminID,
			Password:              &h,
			TwoFactorEnabled:      true,
			TwoFactorSecret:       &secret,
			TwoFactorBackupSecret: model.StringArray{"BACKUPCODE0001"},
		},
		modID: {UserID: modID, Password: &h, TwoFactorEnabled: true, TwoFactorSecret: &secret},
	}}
	f := &fixture{
		profiles: profiles,
		keys:     &fakeKeys{keys: []*model.UserSecurityKey{{ID: "AQID", UserID: adminID}}},
		passkeys: &fakePasskeys{},
		secret:   secret,
	}
	f.deps = Deps{
		Roles:    fakeRoles{adminID: true},
		Profiles: profiles,
		Keys:     f.keys,
		Passkeys: f.passkeys,
		Guard:    passwordguard.NewRedisGuard(rdb),
		Replay:   twofactor.NewRedisReplayGuard(rdb),
	}
	f.v, err = New(f.deps)
	require.NoError(t, err)
	return f
}

func (f *fixture) code(t *testing.T) string {
	t.Helper()
	c, err := totp.GenerateCode(f.secret, time.Now())
	require.NoError(t, err)
	return c
}

func adminSubject() Subject {
	tok := nativeTok
	return Subject{User: &model.User{ID: adminID, Token: &tok}, PresentedToken: nativeTok, ClientIP: "192.0.2.1"}
}

func TestNew_RequiresDeps(t *testing.T) {
	f := newFixture(t)
	for name, mutate := range map[string]func(*Deps){
		"roles":    func(d *Deps) { d.Roles = nil },
		"profiles": func(d *Deps) { d.Profiles = nil },
		"guard":    func(d *Deps) { d.Guard = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := f.deps
			mutate(&d)
			_, err := New(d)
			assert.Error(t, err)
		})
	}
}

func TestCheckEligible(t *testing.T) {
	f := newFixture(t)
	modTok := nativeTok
	noPw := *f.profiles.profiles[adminID]
	noPw.Password = nil
	no2FA := *f.profiles.profiles[adminID]
	no2FA.TwoFactorEnabled = false
	keysOnly := no2FA
	keysOnly.SecurityKeysAvailable = true

	cases := []struct {
		name    string
		subject Subject
		profile *model.UserProfile
		loadErr error
		want    Reason
	}{
		{name: "ok", subject: adminSubject()},
		{name: "no user", subject: Subject{}, want: ReasonNoCredential},
		{name: "moderator", subject: Subject{User: &model.User{ID: modID, Token: &modTok}, PresentedToken: nativeTok}, want: ReasonNotAdmin},
		{name: "app token", subject: func() Subject { s := adminSubject(); s.PresentedToken = "app-token"; return s }(), want: ReasonNotNativeToken},
		{name: "no native token on user", subject: func() Subject { s := adminSubject(); s.User.Token = nil; return s }(), want: ReasonNotNativeToken},
		{name: "empty token", subject: func() Subject { s := adminSubject(); e := ""; s.User.Token = &e; s.PresentedToken = ""; return s }(), want: ReasonNotNativeToken},
		{name: "no 2fa", subject: adminSubject(), profile: &no2FA, want: ReasonTwoFactorRequired},
		{name: "passkey only", subject: adminSubject(), profile: &keysOnly},
		{name: "no password", subject: adminSubject(), profile: &noPw, want: ReasonPasswordNotSet},
		{name: "profile load error", subject: adminSubject(), loadErr: errors.New("db down"), want: ReasonUnavailable},
		{name: "nil profile", subject: adminSubject(), profile: &model.UserProfile{}, want: ReasonUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := f.profiles.profiles[adminID]
			t.Cleanup(func() { f.profiles.profiles[adminID] = orig; f.profiles.err = nil })
			if tc.profile != nil {
				if tc.profile.UserID == "" {
					f.profiles.profiles[adminID] = nil
				} else {
					f.profiles.profiles[adminID] = tc.profile
				}
			}
			f.profiles.err = tc.loadErr
			err := f.v.CheckEligible(tc.subject)
			if tc.want == 0 {
				assert.NoError(t, err)
				return
			}
			assert.Equal(t, tc.want, ReasonOf(err), "err=%v", err)
		})
	}
}

func TestVerify_TOTP(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: f.code(t)}))
}

func TestVerify_RejectsIneligibleBeforeReauth(t *testing.T) {
	f := newFixture(t)
	s := adminSubject()
	s.PresentedToken = "app-token"
	err := f.v.Verify(context.Background(), s, Reauth{Password: password, Token: f.code(t)})
	assert.Equal(t, ReasonNotNativeToken, ReasonOf(err))
}

func TestVerify_MissingFactors(t *testing.T) {
	f := newFixture(t)
	for name, r := range map[string]Reauth{
		"nothing":                 {},
		"no password":             {Token: "123456"},
		"no second factor":        {Password: password},
		"credential, no request":  {Password: password, Credential: json.RawMessage(`{}`)},
		"empty credential & code": {Password: password, Credential: json.RawMessage{}},
	} {
		t.Run(name, func(t *testing.T) {
			err := f.v.Verify(context.Background(), adminSubject(), r)
			assert.Equal(t, ReasonReauthRequired, ReasonOf(err))
		})
	}
}

func TestVerify_WrongCodeIsCountedAndLimited(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < passwordguard.DefaultMaxFailures; i++ {
		err := f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: "000000"})
		require.Equal(t, ReasonFailed, ReasonOf(err), "attempt %d", i+1)
	}
	// 2つ目の要素の失敗も数えるので、正しい組でも枠が尽きている。
	err := f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: f.code(t)})
	require.Equal(t, ReasonRateLimited, ReasonOf(err))
	var se *Error
	require.ErrorAs(t, err, &se)
	assert.Positive(t, se.RetryAfter)
}

func TestVerify_WrongPasswordIsCountedAndReleasesCode(t *testing.T) {
	f := newFixture(t)
	code := f.code(t)
	err := f.v.Verify(context.Background(), adminSubject(), Reauth{Password: "wrong", Token: code})
	require.Equal(t, ReasonFailed, ReasonOf(err))
	// password の打ち間違いで TOTP のコードを焼かない。
	require.NoError(t, f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: code}))
	for i := 0; i < passwordguard.DefaultMaxFailures-1; i++ {
		require.Equal(t, ReasonFailed, ReasonOf(f.v.Verify(context.Background(), adminSubject(), Reauth{Password: "wrong", Token: "000000"})))
	}
	assert.Equal(t, ReasonRateLimited, ReasonOf(f.v.Verify(context.Background(), adminSubject(), Reauth{Password: "wrong", Token: "000000"})))
}

func TestVerify_SuccessDoesNotCount(t *testing.T) {
	f := newFixture(t)
	// 成功は失敗として残らない。枠を超える回数だけ成功させても通り続ける。
	// TOTP は 1 回しか使えないので、使ったコードの記録を毎回消す。
	for i := 0; i <= passwordguard.DefaultMaxFailures; i++ {
		code := f.code(t)
		require.NoError(t, f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: code}), "attempt %d", i+1)
		twofactor.ReleaseReservation(context.Background(), f.deps.Replay, adminID, code)
	}
}

func TestVerify_TOTPReplayIsRejected(t *testing.T) {
	f := newFixture(t)
	code := f.code(t)
	require.NoError(t, f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: code}))
	err := f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: code})
	assert.Equal(t, ReasonFailed, ReasonOf(err))
}

func TestVerify_TOTPNotEnabled(t *testing.T) {
	f := newFixture(t)
	f.profiles.profiles[adminID].TwoFactorEnabled = false
	f.profiles.profiles[adminID].SecurityKeysAvailable = true
	err := f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: f.code(t)})
	assert.Equal(t, ReasonFailed, ReasonOf(err))
}

func TestVerify_BackupCode(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: "BACKUPCODE0001"}))
	assert.Equal(t, []string{"BACKUPCODE0001"}, f.profiles.removed)
	err := f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: "BACKUPCODE0001"})
	assert.Equal(t, ReasonFailed, ReasonOf(err), "a backup code must be single-use")
}

func TestVerify_BackupCodeNotBurnedByWrongPassword(t *testing.T) {
	f := newFixture(t)
	err := f.v.Verify(context.Background(), adminSubject(), Reauth{Password: "wrong", Token: "BACKUPCODE0001"})
	require.Equal(t, ReasonFailed, ReasonOf(err))
	assert.Empty(t, f.profiles.removed)
	require.NoError(t, f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: "BACKUPCODE0001"}))
}

func TestVerify_BackupCodeCommitFailureRefuses(t *testing.T) {
	f := newFixture(t)
	f.profiles.removeErr = errors.New("db down")
	err := f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: "BACKUPCODE0001"})
	assert.Equal(t, ReasonUnavailable, ReasonOf(err))
	// 予約は取り消されているので、書き込みが戻れば同じコードを使える。
	f.profiles.removeErr = nil
	require.NoError(t, f.v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: "BACKUPCODE0001"}))
}

func TestVerify_FailsClosedWhenStoresAreDown(t *testing.T) {
	t.Run("password guard", func(t *testing.T) {
		f := newFixture(t)
		d := f.deps
		d.Guard = erringGuard{}
		v, err := New(d)
		require.NoError(t, err)
		err = v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: f.code(t)})
		assert.Equal(t, ReasonUnavailable, ReasonOf(err))
		assert.Contains(t, err.Error(), "redis down")
	})
	for name, token := range map[string]string{"totp replay": "", "backup code reservation": "BACKUPCODE0001"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			d := f.deps
			d.Replay = erringReplay{}
			v, err := New(d)
			require.NoError(t, err)
			if token == "" {
				token = f.code(t)
			}
			err = v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: token})
			assert.Equal(t, ReasonUnavailable, ReasonOf(err))
		})
	}
}

func TestVerify_NoReplayGuard(t *testing.T) {
	f := newFixture(t)
	d := f.deps
	d.Replay = nil
	v, err := New(d)
	require.NoError(t, err)
	require.NoError(t, v.Verify(context.Background(), adminSubject(), Reauth{Password: password, Token: f.code(t)}))
}

func passkeyReauth(pw string) Reauth {
	return Reauth{
		Password:   pw,
		Credential: json.RawMessage(`{"id":"AQID"}`),
		Request:    httptest.NewRequest(http.MethodPost, "/api/admin/backup/list", strings.NewReader("{}")),
	}
}

func TestVerify_Passkey(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.v.Verify(context.Background(), adminSubject(), passkeyReauth(password)))
	assert.Equal(t, 1, f.passkeys.finished)
	assert.Equal(t, int64(7), f.keys.counters["AQID"])
}

func TestVerify_PasskeyFailures(t *testing.T) {
	t.Run("assertion rejected", func(t *testing.T) {
		f := newFixture(t)
		f.passkeys.finishErr = errors.New("bad signature")
		assert.Equal(t, ReasonFailed, ReasonOf(f.v.Verify(context.Background(), adminSubject(), passkeyReauth(password))))
	})
	t.Run("wrong password", func(t *testing.T) {
		f := newFixture(t)
		assert.Equal(t, ReasonFailed, ReasonOf(f.v.Verify(context.Background(), adminSubject(), passkeyReauth("wrong"))))
		assert.Empty(t, f.keys.counters)
	})
	t.Run("no keys", func(t *testing.T) {
		f := newFixture(t)
		f.keys.keys = nil
		assert.Equal(t, ReasonPasskeyUnavailable, ReasonOf(f.v.Verify(context.Background(), adminSubject(), passkeyReauth(password))))
	})
	t.Run("key lookup fails", func(t *testing.T) {
		f := newFixture(t)
		f.keys.err = errors.New("db down")
		assert.Equal(t, ReasonUnavailable, ReasonOf(f.v.Verify(context.Background(), adminSubject(), passkeyReauth(password))))
	})
	t.Run("passkeys not wired", func(t *testing.T) {
		f := newFixture(t)
		d := f.deps
		d.Passkeys = nil
		v, err := New(d)
		require.NoError(t, err)
		assert.Equal(t, ReasonPasskeyUnavailable, ReasonOf(v.Verify(context.Background(), adminSubject(), passkeyReauth(password))))
	})
}

func TestBeginPasskey(t *testing.T) {
	f := newFixture(t)
	a, err := f.v.BeginPasskey(context.Background(), adminSubject())
	require.NoError(t, err)
	assert.Equal(t, protocol.URLEncodedBase64("chal"), a.Response.Challenge)

	s := adminSubject()
	s.PresentedToken = "app-token"
	_, err = f.v.BeginPasskey(context.Background(), s)
	assert.Equal(t, ReasonNotNativeToken, ReasonOf(err))

	f.passkeys.beginErr = errors.New("redis down")
	_, err = f.v.BeginPasskey(context.Background(), adminSubject())
	assert.Equal(t, ReasonUnavailable, ReasonOf(err))

	f.keys.keys = nil
	_, err = f.v.BeginPasskey(context.Background(), adminSubject())
	assert.Equal(t, ReasonPasskeyUnavailable, ReasonOf(err))
}

func TestErrorHelpers(t *testing.T) {
	assert.Equal(t, Reason(0), ReasonOf(errors.New("other")))
	assert.Equal(t, Reason(0), ReasonOf(nil))
	e := &Error{Reason: ReasonFailed}
	assert.Contains(t, e.Error(), "refused")
	assert.NoError(t, e.Unwrap())
	wrapped := &Error{Reason: ReasonUnavailable, Err: errors.New("cause")}
	assert.ErrorContains(t, wrapped, "cause")
}
