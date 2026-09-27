package twofactor

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/model"
)

// Errors returned by WebAuthnService.
var (
	// ErrWebAuthnSessionNotFound is returned when the begin/finish session
	// cannot be located in Redis (expired, never created, or wrong id).
	ErrWebAuthnSessionNotFound = errors.New("twofactor: webauthn session not found")
	// ErrWebAuthnNotConfigured is returned when WebAuthnService methods are
	// called on a zero-value or nil-Redis instance.
	ErrWebAuthnNotConfigured = errors.New("twofactor: webauthn service not configured")
	// ErrWebAuthnCounterRollback is returned when the assertion's signature
	// counter did not advance past the stored value, which signals a cloned
	// authenticator.
	ErrWebAuthnCounterRollback = errors.New("twofactor: webauthn signature counter did not increase")
)

// webAuthnSessionTTL bounds how long a registration / authentication challenge
// stays valid. 5 minutes matches the typical user interaction window for
// inserting a security key and tapping it.
const webAuthnSessionTTL = 5 * time.Minute

// WebAuthnService wraps a *webauthn.WebAuthn instance with the bits the rest
// of the codebase needs: Redis-backed challenge sessions and a model adapter
// that converts between go-webauthn and our DB shapes.
type WebAuthnService struct {
	wa    *webauthn.WebAuthn
	redis redis.Cmdable
}

// NewWebAuthnService constructs a WebAuthnService from the public URL of the
// instance and an optional Redis client. The instance URL is parsed to derive
// `RPID` (host) and `RPOrigins` (scheme + host) — these are required by the
// WebAuthn spec to bind credentials to the relying party domain.
//
// Returns ErrWebAuthnNotConfigured if instanceURL cannot be parsed.
func NewWebAuthnService(instanceURL, displayName string, redisClient redis.Cmdable) (*WebAuthnService, error) {
	if instanceURL == "" {
		return nil, ErrWebAuthnNotConfigured
	}
	u, err := url.Parse(instanceURL)
	if err != nil || u.Host == "" {
		return nil, ErrWebAuthnNotConfigured
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: displayName,
		RPOrigins:     []string{strings.TrimRight(instanceURL, "/")},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn config: %w", err)
	}
	return &WebAuthnService{wa: wa, redis: redisClient}, nil
}

// --- webauthn.User adapter ------------------------------------------------

// userAdapter implements webauthn.User for one of our model.User rows + their
// existing security keys. The same adapter is reused on register (where the
// existing keys list helps the authenticator avoid duplicate enrollments) and
// on login (where it provides the allowedCredentials list).
type userAdapter struct {
	user *model.User
	keys []*model.UserSecurityKey
}

// WebAuthnID returns the user handle as raw bytes. We use the ULID-style
// User.ID directly since it is already a stable opaque value <= 64 bytes.
func (u *userAdapter) WebAuthnID() []byte { return []byte(u.user.ID) }

// WebAuthnName returns the username — used by the authenticator UI to
// disambiguate accounts.
func (u *userAdapter) WebAuthnName() string { return u.user.Username }

// WebAuthnDisplayName returns the display name (falls back to username when
// the user has not set one).
func (u *userAdapter) WebAuthnDisplayName() string {
	if u.user.Name != nil && *u.user.Name != "" {
		return *u.user.Name
	}
	return u.user.Username
}

// WebAuthnCredentials converts our DB rows into the go-webauthn shape so that
// BeginRegistration / BeginLogin can build the excludeCredentials /
// allowedCredentials lists, and FinishLogin can find the right key to verify.
func (u *userAdapter) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(u.keys))
	for _, k := range u.keys {
		credID, err := base64.RawURLEncoding.DecodeString(k.ID)
		if err != nil {
			continue
		}
		pubKey, err := base64.RawURLEncoding.DecodeString(k.PublicKey)
		if err != nil {
			continue
		}
		cred := webauthn.Credential{
			ID:        credID,
			PublicKey: pubKey,
			Authenticator: webauthn.Authenticator{
				SignCount: uint32(k.Counter),
			},
		}
		// BackupEligible / BackupState フラグを DB から復元する (#707)。
		// 登録時の credentialDeviceType / credentialBackedUp 列に upstream
		// Misskey TS と同じ規約で保存している (singleDevice = BE=false、
		// multiDevice = BE=true)。Cloud sync passkey の認証時に go-webauthn
		// が Flags.BackupEligible を AuthenticatorData の BE flag と比較する
		// ので、復元せずに default false のままだと必ず mismatch エラー
		// ("Backup Eligible flag inconsistency") になる。
		// NULL (#707 修正前の登録データ) は default false で復元する: 該当
		// passkey は再登録が必要。
		if k.CredentialDeviceType != nil && *k.CredentialDeviceType == "multiDevice" {
			cred.Flags.BackupEligible = true
		}
		if k.CredentialBackedUp != nil {
			cred.Flags.BackupState = *k.CredentialBackedUp
		}
		for _, t := range k.Transports {
			cred.Transport = append(cred.Transport, protocol.AuthenticatorTransport(t))
		}
		out = append(out, cred)
	}
	return out
}

// --- session storage ------------------------------------------------------

// loginSessionKey is the Redis key for the 2FA-step assertion challenge.
// Misskey TS の WebAuthnService.initiateAuthentication と同じく client は
// session id を round-trip しないため user 単位で 1 件の challenge だけ保持
// する。新規 challenge は既存を上書きする (#705)。
func loginSessionKey(userID string) string {
	return "twofa:webauthn:" + userID + ":login"
}

// registrationSessionKey is the Redis key for the upstream-compatible
// single-in-flight-per-user mode used by /api/i/2fa/{register-key,key-done}.
// Misskey TS の WebAuthnService と同じく client は session id を round-trip
// しないため user 単位で 1 件の challenge だけ保持する。新規 challenge は
// 既存を上書きする (#698)。
func registrationSessionKey(userID string) string {
	return "twofa:webauthn:" + userID + ":registration"
}

// passkeySessionKey is the Redis key for the passwordless `signin-with-passkey`
// flow. challenge は (まだ user が判明していないため) client 由来の random
// `context` (UUID) で keyed する。Misskey TS の
// WebAuthnService.initiateSignInWithPasskeyAuthentication と同じ命名規則。
func passkeySessionKey(ctxID string) string {
	return "twofa:webauthn:passkey:" + ctxID
}

// readRandom is a thin wrapper around crypto/rand.Read. Defined as a var so
// tests can stub it for deterministic backup-code generation
// (`backup_codes.go` consumes it).
var readRandom = cryptorand.Read

// --- public API -----------------------------------------------------------

// BeginRegistration starts a new credential registration. The SessionData
// is stored under a fixed per-user key (no session id round-trip), matching
// Misskey TS upstream の WebAuthnService.initiateRegistration (#698)。
//
// authenticatorSelection は Misskey TS upstream (`@simplewebauthn/server`) と
// 同じく residentKey=required / userVerification=preferred を要求する。これが
// 無いと一部の authenticator + browser 組合せ (特に passkey 経由の platform
// authenticator) でダイアログは表示されるが credential が返らないケースがある。
// preferred はブラウザへの要求だけで、FinishRegistration は UV を必須にして
// 検証する (upstream と同じ)。
func (s *WebAuthnService) BeginRegistration(ctx context.Context, user *model.User, existing []*model.UserSecurityKey) (*protocol.CredentialCreation, error) {
	if s == nil || s.wa == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	adapter := &userAdapter{user: user, keys: existing}
	creation, sd, err := s.wa.BeginRegistration(adapter, webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
		ResidentKey:      protocol.ResidentKeyRequirementRequired,
		UserVerification: protocol.VerificationPreferred,
	}))
	if err != nil {
		return nil, err
	}
	if err := s.putRegistrationSession(ctx, user.ID, sd); err != nil {
		return nil, err
	}
	return creation, nil
}

// FinishRegistration verifies the registration response from the browser and
// returns a Credential ready to be persisted as a UserSecurityKey row. The
// SessionData is loaded by user id alone (no client-supplied sessionID),
// matching Misskey TS WebAuthnService.verifyRegistration (#698).
//
// The attestation must carry the User Verified flag.
func (s *WebAuthnService) FinishRegistration(ctx context.Context, user *model.User, existing []*model.UserSecurityKey, req *http.Request) (*webauthn.Credential, error) {
	if s == nil || s.wa == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	sd, err := s.takeRegistrationSession(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	// upstream の verifyRegistration は `requireUserVerification: true` で検証する。
	// options は preferred なので、保存した session の値のままだと go-webauthn は
	// UV フラグを見ない。
	sd.UserVerification = protocol.VerificationRequired
	adapter := &userAdapter{user: user, keys: existing}
	cred, err := s.wa.FinishRegistration(adapter, *sd, req)
	if err != nil {
		return nil, err
	}
	return cred, nil
}

// putRegistrationSession overwrites any existing registration challenge so a
// fresh register-key call after an abandoned attempt always works.
func (s *WebAuthnService) putRegistrationSession(ctx context.Context, userID string, sd *webauthn.SessionData) error {
	if s == nil || s.redis == nil {
		return ErrWebAuthnNotConfigured
	}
	raw, err := json.Marshal(sd)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, registrationSessionKey(userID), raw, webAuthnSessionTTL).Err()
}

// takeRegistrationSession loads-and-deletes the registration session blob
// (single-use).
func (s *WebAuthnService) takeRegistrationSession(ctx context.Context, userID string) (*webauthn.SessionData, error) {
	if s == nil || s.redis == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	raw, err := s.redis.GetDel(ctx, registrationSessionKey(userID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrWebAuthnSessionNotFound
		}
		return nil, err
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(raw, &sd); err != nil {
		return nil, err
	}
	return &sd, nil
}

// BeginLogin starts an authentication assertion challenge for the given user.
// Used during the 2FA step of /api/signin-flow when a security key is the
// preferred second factor. The SessionData is keyed by user id alone — Misskey
// TS upstream の signin プロトコルが client へ session id を返さないため
// (#705)、同 user の新たな challenge は既存を上書きする。
//
// The browser is asked for userVerification=preferred like upstream;
// FinishLogin requires it regardless.
func (s *WebAuthnService) BeginLogin(ctx context.Context, user *model.User, existing []*model.UserSecurityKey) (*protocol.CredentialAssertion, error) {
	if s == nil || s.wa == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	adapter := &userAdapter{user: user, keys: existing}
	assertion, sd, err := s.wa.BeginLogin(adapter, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return nil, err
	}
	if err := s.putLoginSession(ctx, user.ID, sd); err != nil {
		return nil, err
	}
	return assertion, nil
}

// FinishLogin verifies the assertion response. The challenge is loaded by user
// id alone (no client-supplied session id), matching Misskey TS upstream の
// WebAuthnService.verifyAuthentication (#705). The returned Credential carries
// an updated counter; callers should persist it via UserSecurityKeyRepository
// to detect cloned authenticators on subsequent logins.
//
// The assertion must carry the User Verified flag, whether the key is the
// second factor or the only one. Returns ErrWebAuthnCounterRollback when the
// signature counter signals a cloned authenticator.
func (s *WebAuthnService) FinishLogin(ctx context.Context, user *model.User, existing []*model.UserSecurityKey, req *http.Request) (*webauthn.Credential, error) {
	if s == nil || s.wa == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	sd, err := s.takeLoginSession(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	// upstream の verifyAuthentication は 2 要素目でも
	// `requireUserVerification: true` で検証する。options は preferred なので、
	// 保存した session の値のままだと go-webauthn は UV フラグを見ない。
	sd.UserVerification = protocol.VerificationRequired
	adapter := &userAdapter{user: user, keys: existing}
	cred, err := s.wa.FinishLogin(adapter, *sd, req)
	if err != nil {
		return nil, err
	}
	if err := checkCounter(cred); err != nil {
		return nil, err
	}
	return cred, nil
}

// checkCounter rejects an assertion whose signature counter signals a cloned
// authenticator.
//
// upstream は @simplewebauthn の verifyAuthenticationResponse が
// `(newCounter > 0 || counter > 0) && newCounter <= counter` で throw するので
// ログインが失敗する。go-webauthn は CloneWarning を立てるだけで成功を返すので、
// ここで拒否する。**counter を持たない認証器 (常に 0) は誤検知しない** —
// `Authenticator.UpdateCounter` が 0 / 0 を CloneWarning にしない。
func checkCounter(cred *webauthn.Credential) error {
	if cred.Authenticator.CloneWarning {
		return ErrWebAuthnCounterRollback
	}
	return nil
}

// putLoginSession overwrites any in-flight login challenge for the user.
func (s *WebAuthnService) putLoginSession(ctx context.Context, userID string, sd *webauthn.SessionData) error {
	if s == nil || s.redis == nil {
		return ErrWebAuthnNotConfigured
	}
	raw, err := json.Marshal(sd)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, loginSessionKey(userID), raw, webAuthnSessionTTL).Err()
}

// takeLoginSession loads-and-deletes the login session blob (single-use).
func (s *WebAuthnService) takeLoginSession(ctx context.Context, userID string) (*webauthn.SessionData, error) {
	if s == nil || s.redis == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	raw, err := s.redis.GetDel(ctx, loginSessionKey(userID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrWebAuthnSessionNotFound
		}
		return nil, err
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(raw, &sd); err != nil {
		return nil, err
	}
	return &sd, nil
}

// PasskeyUserResolver is the lookup callback supplied by the API layer to map
// a passkey credential (rawID + userHandle) back to a model.User and the user's
// security keys list. The handler closes over the repositories so this package
// stays free of repository dependencies.
type PasskeyUserResolver func(rawID, userHandle []byte) (*model.User, []*model.UserSecurityKey, error)

// BeginPasskeyLogin starts a passwordless ("usernameless") authentication
// challenge. Misskey TS upstream の SigninWithPasskeyApiService が呼ぶ
// initiateSignInWithPasskeyAuthentication 互換 (#705)。
//
// `ctxID` は client 由来 (random UUID) で、challenge を Redis に保存する key
// として使う。FinishPasskeyLogin が同じ ctxID で取り出す。
func (s *WebAuthnService) BeginPasskeyLogin(ctx context.Context, ctxID string) (*protocol.CredentialAssertion, error) {
	if s == nil || s.wa == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	// options は upstream と同じ preferred。UV の要否は FinishPasskeyLogin が
	// required で検証する。
	assertion, sd, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return nil, err
	}
	if err := s.putPasskeySession(ctx, ctxID, sd); err != nil {
		return nil, err
	}
	return assertion, nil
}

// FinishPasskeyLogin verifies a passwordless assertion response. The browser
// returns a credential whose `userHandle` carries the user id we stored at
// registration time; resolver is invoked to load the User and its security
// keys for verification.
//
// 戻り値の *model.User は signin 成功時のユーザー、*webauthn.Credential は
// 更新された counter を含むので caller は UserSecurityKeyRepository.UpdateCounter
// で永続化する責務を負う。
func (s *WebAuthnService) FinishPasskeyLogin(ctx context.Context, ctxID string, req *http.Request, resolve PasskeyUserResolver) (*model.User, *webauthn.Credential, error) {
	if s == nil || s.wa == nil {
		return nil, nil, ErrWebAuthnNotConfigured
	}
	sd, err := s.takePasskeySession(ctx, ctxID)
	if err != nil {
		return nil, nil, err
	}
	// 保存した値に依らず required で検証する。upstream も
	// `requireUserVerification: true` で検証している。
	sd.UserVerification = protocol.VerificationRequired
	var resolvedUser *model.User
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, keys, err := resolve(rawID, userHandle)
		if err != nil {
			return nil, err
		}
		resolvedUser = u
		return &userAdapter{user: u, keys: keys}, nil
	}
	cred, err := s.wa.FinishDiscoverableLogin(handler, *sd, req)
	if err != nil {
		return nil, nil, err
	}
	if err := checkCounter(cred); err != nil {
		return nil, nil, err
	}
	return resolvedUser, cred, nil
}

// putPasskeySession overwrites any in-flight passkey challenge for the
// given context id. Returns ErrWebAuthnNotConfigured when redis is missing.
func (s *WebAuthnService) putPasskeySession(ctx context.Context, ctxID string, sd *webauthn.SessionData) error {
	if s == nil || s.redis == nil {
		return ErrWebAuthnNotConfigured
	}
	raw, err := json.Marshal(sd)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, passkeySessionKey(ctxID), raw, webAuthnSessionTTL).Err()
}

// takePasskeySession loads-and-deletes the passwordless session blob.
func (s *WebAuthnService) takePasskeySession(ctx context.Context, ctxID string) (*webauthn.SessionData, error) {
	if s == nil || s.redis == nil {
		return nil, ErrWebAuthnNotConfigured
	}
	raw, err := s.redis.GetDel(ctx, passkeySessionKey(ctxID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrWebAuthnSessionNotFound
		}
		return nil, err
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(raw, &sd); err != nil {
		return nil, err
	}
	return &sd, nil
}

// --- persistence helpers --------------------------------------------------

// CredentialToModel converts a freshly-registered webauthn Credential into
// the model.UserSecurityKey shape persisted by the repository. The caller
// supplies userID and the user-chosen `name`.
func CredentialToModel(cred *webauthn.Credential, userID, name string) *model.UserSecurityKey {
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	out := &model.UserSecurityKey{
		ID:        base64.RawURLEncoding.EncodeToString(cred.ID),
		UserID:    userID,
		Name:      name,
		PublicKey: base64.RawURLEncoding.EncodeToString(cred.PublicKey),
		Counter:   int64(cred.Authenticator.SignCount),
		LastUsed:  time.Now(),
	}
	if len(transports) > 0 {
		out.Transports = model.StringArray(transports)
	}
	// BackupEligible / BackupState フラグを upstream Misskey TS 互換の
	// credentialDeviceType / credentialBackedUp 列に保存する (#707)。
	// これを保存していないと WebAuthnCredentials() の復元時に
	// Flags.BackupEligible が default false になり、Cloud sync passkey
	// (登録時 BE=true) の認証時に go-webauthn の整合性チェックで mismatch
	// エラーになる。
	deviceType := "singleDevice"
	if cred.Flags.BackupEligible {
		deviceType = "multiDevice"
	}
	out.CredentialDeviceType = &deviceType
	backedUp := cred.Flags.BackupState
	out.CredentialBackedUp = &backedUp
	return out
}
