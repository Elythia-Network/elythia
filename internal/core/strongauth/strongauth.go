// Package strongauth checks the strong authentication that guards the most
// sensitive administrator operations, such as the database backups (#3462).
//
// A request passes only when all of these hold:
//
//   - the user is an administrator (moderators are refused)
//   - the request carries the user's native session token (users.token), not
//     an app or MiAuth access token (Misskey's `secure: true`)
//   - the account has two-factor authentication (TOTP) or a passkey
//     registered
//   - the request re-authenticates with the password and a second factor (a
//     TOTP code, a backup code or a passkey assertion)
//
// Failed re-authentications are counted by passwordguard. The package has no
// echo dependency so that the backup service can run the same checks against
// the database while the main server is in maintenance mode (#3463).
//
// バックアップには利用者の秘密鍵・token・パスワードの hash が入るので、この
// 認証で守る画面は、乗っ取られたときの被害が最も大きい場所の1つになる。
// i/* の再認証 (i/regenerate-token など) より厳しくし、次の点を変えている。
//
//   - **2つ目の要素の失敗も passwordguard で数える。** i/* は password の照合
//     だけを数えるが、ここでは予約をリクエストの最初に取り、全ての要素が通った
//     ときだけ取り消す
//   - **障害時は閉じる。** i/* は Redis の障害で利用者を締め出さないよう
//     fail-open にしているが、ここはコマンド (elythia backup) という代わりの
//     手段があるので、照合できないときは拒否する
//   - **どの要素で失敗したかを返さない。** password と 2つ目の要素のどちらが
//     違ったかを分けると、片方ずつ当てられる。例外は、password が合っていて
//     TOTP のコードを使い回しただけのとき (ReasonCodeReused)。操作のたびに
//     再認証するので普通に起き、数えると本人が締め出される
package strongauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/bcrypt"
)

// Reason classifies why a request was refused.
type Reason int

// Reasons, roughly in the order they are checked.
const (
	// ReasonNoCredential: the request is not signed in.
	ReasonNoCredential Reason = iota + 1
	// ReasonNotAdmin: the user is not an administrator.
	ReasonNotAdmin
	// ReasonNotNativeToken: the token is an app / MiAuth access token.
	ReasonNotNativeToken
	// ReasonTwoFactorRequired: the account has neither TOTP nor a passkey.
	ReasonTwoFactorRequired
	// ReasonReauthRequired: the password or the second factor is missing.
	ReasonReauthRequired
	// ReasonFailed: the password or the second factor is wrong.
	ReasonFailed
	// ReasonRateLimited: too many failed re-authentications.
	ReasonRateLimited
	// ReasonPasskeyUnavailable: a passkey was asked for, but the account has
	// none or the server cannot verify passkeys.
	ReasonPasskeyUnavailable
	// ReasonUnavailable: a store needed for the check failed.
	ReasonUnavailable
	// ReasonPasswordNotSet: the account has no password to re-authenticate
	// with.
	ReasonPasswordNotSet
	// ReasonCodeReused: the password was right but the TOTP or backup code
	// was already used within the replay window. Not counted as a failure.
	ReasonCodeReused
)

// Error is returned when a request is refused.
type Error struct {
	Reason Reason
	// RetryAfter is set with ReasonRateLimited.
	RetryAfter time.Duration
	// Err is the underlying cause for ReasonUnavailable (for logging only).
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("strongauth: refused (reason %d): %v", e.Reason, e.Err)
	}
	return fmt.Sprintf("strongauth: refused (reason %d)", e.Reason)
}

func (e *Error) Unwrap() error { return e.Err }

// ReasonOf returns the Reason of err, or 0 when err is not an *Error.
func ReasonOf(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return 0
}

func refuse(r Reason) error { return &Error{Reason: r} }

func unavailable(err error) error { return &Error{Reason: ReasonUnavailable, Err: err} }

// RoleChecker reports whether a user is an administrator.
type RoleChecker interface {
	IsAdministrator(userID string) bool
}

// ProfileStore reads and updates the credentials in user_profile.
type ProfileStore interface {
	// GetProfileErr returns the profile. A lookup failure must be returned
	// as an error, not as a nil profile.
	GetProfileErr(userID string) (*model.UserProfile, error)
	// RemoveBackupCode consumes one backup code.
	RemoveBackupCode(userID, code string) error
}

// SecurityKeyStore lists and updates the user's passkeys.
type SecurityKeyStore interface {
	ListByUser(userID string) ([]*model.UserSecurityKey, error)
	UpdateCounter(id string, counter int64) error
}

// Passkeys issues and verifies passkey assertions. *twofactor.WebAuthnService
// implements it.
type Passkeys interface {
	BeginLogin(ctx context.Context, user *model.User, existing []*model.UserSecurityKey) (*protocol.CredentialAssertion, error)
	FinishLogin(ctx context.Context, user *model.User, existing []*model.UserSecurityKey, req *http.Request) (*webauthn.Credential, error)
}

// Deps are the stores the Verifier reads. Roles, Profiles and Guard are
// required; Keys and Passkeys may be nil (passkeys are then not accepted,
// TOTP still works). Replay may be nil only in tests.
type Deps struct {
	Roles    RoleChecker
	Profiles ProfileStore
	Keys     SecurityKeyStore
	Passkeys Passkeys
	// Guard counts failed re-authentications. Required: a nil guard would
	// silently allow unlimited guessing.
	Guard passwordguard.Guard
	// Replay records used TOTP and backup codes so that one code cannot pass
	// twice.
	Replay twofactor.ReplayGuard
}

// Verifier performs the checks. It is safe for concurrent use.
type Verifier struct {
	d Deps
}

// New returns a Verifier. It fails when a required dependency is missing so
// that a wiring mistake cannot open the gate.
func New(d Deps) (*Verifier, error) {
	if d.Roles == nil || d.Profiles == nil || d.Guard == nil {
		return nil, errors.New("strongauth: Roles, Profiles and Guard are required")
	}
	return &Verifier{d: d}, nil
}

// Subject is who is asking: the signed-in user and the raw token the request
// carried.
type Subject struct {
	User *model.User
	// PresentedToken is the raw token of the request (Bearer or body `i`).
	PresentedToken string
	// ClientIP keys the per-client-range failure budget of passwordguard.
	ClientIP string
}

// Reauth is the re-authentication sent with every operation.
type Reauth struct {
	Password string
	// Token is a TOTP code or a backup code. Ignored when Credential is set.
	Token string
	// Credential is a passkey assertion (PublicKeyCredential.toJSON()) for a
	// challenge from BeginPasskey.
	Credential json.RawMessage
	// Request is the HTTP request the credential came with. The passkey
	// check reads its origin. Required with Credential.
	Request *http.Request
}

// eligible checks everything but the re-authentication and returns the
// profile.
func (v *Verifier) eligible(s Subject) (*model.UserProfile, error) {
	u := s.User
	if u == nil {
		return nil, refuse(ReasonNoCredential)
	}
	if !v.d.Roles.IsAdministrator(u.ID) {
		return nil, refuse(ReasonNotAdmin)
	}
	// RequireSecure と同じ判定。app / MiAuth の token は access_token の行で
	// 解決されるので、users.token とは一致しない。
	if u.Token == nil || *u.Token == "" || *u.Token != s.PresentedToken {
		return nil, refuse(ReasonNotNativeToken)
	}
	profile, err := v.d.Profiles.GetProfileErr(u.ID)
	if err != nil {
		return nil, unavailable(fmt.Errorf("load profile: %w", err))
	}
	if profile == nil {
		return nil, unavailable(errors.New("load profile: nil profile"))
	}
	// パスキーは TOTP を有効にしてからでないと登録できない (i/2fa/register-key の
	// TWO_FACTOR_NOT_ENABLED)。securityKeysAvailable も見るのは、TS 版から
	// 引き継いだ DB で TOTP だけが外れている行を締め出さないため。
	if !profile.TwoFactorEnabled && !profile.SecurityKeysAvailable {
		return nil, refuse(ReasonTwoFactorRequired)
	}
	// パスワードの無いアカウント (パスキーだけでログインする形はこの実装には無いが、
	// TS 版の DB では起こりうる) は再認証できない。
	if profile.Password == nil || *profile.Password == "" {
		return nil, refuse(ReasonPasswordNotSet)
	}
	return profile, nil
}

// CheckEligible checks the conditions other than the re-authentication. The
// admin page uses it to tell the user what is missing before asking for the
// password.
func (v *Verifier) CheckEligible(s Subject) error {
	_, err := v.eligible(s)
	return err
}

// BeginPasskey starts a passkey challenge for the re-authentication. The
// subject must already be eligible.
func (v *Verifier) BeginPasskey(ctx context.Context, s Subject) (*protocol.CredentialAssertion, error) {
	if _, err := v.eligible(s); err != nil {
		return nil, err
	}
	keys, err := v.passkeys(s.User.ID)
	if err != nil {
		return nil, err
	}
	assertion, err := v.d.Passkeys.BeginLogin(ctx, s.User, keys)
	if err != nil {
		return nil, unavailable(fmt.Errorf("begin passkey: %w", err))
	}
	return assertion, nil
}

func (v *Verifier) passkeys(userID string) ([]*model.UserSecurityKey, error) {
	if v.d.Keys == nil || v.d.Passkeys == nil {
		return nil, refuse(ReasonPasskeyUnavailable)
	}
	keys, err := v.d.Keys.ListByUser(userID)
	if err != nil {
		return nil, unavailable(fmt.Errorf("list passkeys: %w", err))
	}
	if len(keys) == 0 {
		return nil, refuse(ReasonPasskeyUnavailable)
	}
	return keys, nil
}

// Verify checks every condition, including the re-authentication. A nil
// error means the operation may proceed; second factors are consumed by then.
func (v *Verifier) Verify(ctx context.Context, s Subject, r Reauth) error {
	profile, err := v.eligible(s)
	if err != nil {
		return err
	}
	// `"credential": null` は付けていないのと同じに扱う。null をパスキーの
	// 経路に回すと、token を付けていても失敗として数えられる。
	usePasskey := len(r.Credential) > 0 && !bytes.Equal(bytes.TrimSpace(r.Credential), []byte("null"))
	if r.Password == "" || (!usePasskey && r.Token == "") {
		return refuse(ReasonReauthRequired)
	}
	if usePasskey && r.Request == nil {
		return refuse(ReasonReauthRequired)
	}
	// パスキーを照合できない構成かどうかは、予約より前に確かめる。照合して
	// いないものを失敗として数えない。
	var keys []*model.UserSecurityKey
	if usePasskey {
		if keys, err = v.passkeys(s.User.ID); err != nil {
			return err
		}
	}

	// 予約は取り消しに引きずられない ctx で行う (i/* の beginPasswordCheck と同じ)。
	// 切断で予約が落ちると、数えられない照合が走る。
	bg := context.WithoutCancel(ctx)
	attempt, err := v.d.Guard.Begin(bg, s.User.ID, s.ClientIP)
	if err != nil {
		var le *passwordguard.LimitedError
		if errors.As(err, &le) {
			return &Error{Reason: ReasonRateLimited, RetryAfter: le.RetryAfter}
		}
		return unavailable(fmt.Errorf("password guard: %w", err))
	}

	var second secondFactor
	var secondErr error
	if usePasskey {
		second, secondErr = v.checkPasskey(ctx, s.User, keys, r)
	} else {
		second, secondErr = v.checkCode(bg, profile, r.Token)
	}
	if ReasonOf(secondErr) == ReasonUnavailable {
		// 照合できなかっただけなので、失敗として数えない。
		attempt.Release(bg)
		return secondErr
	}
	// **2つ目の要素の成否にかかわらず password を照合する。** 2つ目の要素が
	// 通ったときだけ bcrypt を走らせると、応答の速さでコードの正否が分かる。
	pwOK := bcrypt.CompareHashAndPassword([]byte(*profile.Password), []byte(r.Password)) == nil
	switch {
	case ReasonOf(secondErr) == ReasonCodeReused && pwOK:
		// 同じ TOTP のコードを続けて使っただけ (操作のたびに再認証するので、
		// 2分以内に2つ操作すると普通に起きる)。password が合っていれば
		// 総当たりではないので数えず、次のコードを待つよう伝える。
		attempt.Release(bg)
		return secondErr
	case secondErr != nil:
		// 失敗は予約を残したまま (= 失敗 1 回) にする。
		second.rollback()
		return refuse(ReasonFailed)
	case !pwOK:
		second.rollback()
		return refuse(ReasonFailed)
	}
	// **消費を確定できなかったら通さない。** 通すと、書き込みが落ちているあいだ
	// 同じバックアップコードで何度でも操作が成立する (i/* の verify2FAToken と同じ)。
	if err := second.commit(); err != nil {
		second.rollback()
		attempt.Release(bg)
		return unavailable(fmt.Errorf("consume second factor: %w", err))
	}
	attempt.Release(bg)
	return nil
}

// secondFactor is a verified second factor whose consumption can still be
// committed or undone.
type secondFactor struct {
	commitFn   func() error
	rollbackFn func()
}

func (f secondFactor) commit() error {
	if f.commitFn == nil {
		return nil
	}
	return f.commitFn()
}

func (f secondFactor) rollback() {
	if f.rollbackFn != nil {
		f.rollbackFn()
	}
}

// checkCode verifies a TOTP code or a backup code and reserves it.
func (v *Verifier) checkCode(ctx context.Context, profile *model.UserProfile, code string) (secondFactor, error) {
	userID := profile.UserID
	if _, err := twofactor.ConsumeBackupCode([]string(profile.TwoFactorBackupSecret), code); err == nil {
		key := twofactor.BackupCodeGuardKey(code)
		if err := v.reserve(ctx, userID, key); err != nil {
			return secondFactor{}, err
		}
		return secondFactor{
			commitFn: func() error {
				// 読んだ配列を書き戻さず、1枚だけ消す (#2852)。
				return v.d.Profiles.RemoveBackupCode(userID, code)
			},
			rollbackFn: func() { twofactor.ReleaseReservation(ctx, v.d.Replay, userID, key) },
		}, nil
	}
	if !profile.TwoFactorEnabled || profile.TwoFactorSecret == nil || !twofactor.Validate(code, *profile.TwoFactorSecret) {
		return secondFactor{}, refuse(ReasonFailed)
	}
	if err := v.reserve(ctx, userID, code); err != nil {
		return secondFactor{}, err
	}
	return secondFactor{
		rollbackFn: func() { twofactor.ReleaseReservation(ctx, v.d.Replay, userID, code) },
	}, nil
}

// reserve records a code as used. Unlike twofactor.ReserveOnce it fails
// closed when the guard is down.
func (v *Verifier) reserve(ctx context.Context, userID, key string) error {
	if v.d.Replay == nil {
		return nil
	}
	ok, err := v.d.Replay.MarkUsed(ctx, userID, key)
	if err != nil {
		return unavailable(fmt.Errorf("replay guard: %w", err))
	}
	if !ok {
		// 同じコードを窓の中で2回使った。password が合っていれば Verify が
		// 数えずに返す。
		return refuse(ReasonCodeReused)
	}
	return nil
}

// checkPasskey verifies a passkey assertion for the challenge of BeginPasskey.
func (v *Verifier) checkPasskey(ctx context.Context, u *model.User, keys []*model.UserSecurityKey, r Reauth) (secondFactor, error) {
	cred, err := v.d.Passkeys.FinishLogin(ctx, u, keys, twofactor.CredentialRequest(r.Request, r.Credential))
	if err != nil {
		slog.Warn("strongauth: passkey verification failed", "userId", u.ID, "err", err)
		return secondFactor{}, refuse(ReasonFailed)
	}
	return secondFactor{
		commitFn: func() error {
			// counter は複製された認証器の検出に使う (signin と同じ)。書けなくても
			// 認証そのものは成立しているので、拒否はしない。
			if err := v.d.Keys.UpdateCounter(base64.RawURLEncoding.EncodeToString(cred.ID), int64(cred.Authenticator.SignCount)); err != nil {
				slog.Warn("strongauth: failed to update passkey counter", "userId", u.ID, "err", err)
			}
			return nil
		},
	}, nil
}
