package twofactor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// finishPasskey runs one passkey ceremony: it issues a challenge, lets
// sign build the assertion over it, and verifies it against the stored key
// row with credentialID as the id the handler looked the key up by.
func finishPasskey(t *testing.T, svc *WebAuthnService, key *model.UserSecurityKey, credentialID string, sign func(challenge string) *http.Request) error {
	t.Helper()
	a, err := svc.BeginPasskeyLogin(context.Background(), "ctx-errors")
	require.NoError(t, err)
	resolve := func(string) (*model.User, []*model.UserSecurityKey, error) {
		return softUser, []*model.UserSecurityKey{key}, nil
	}
	_, _, err = svc.FinishPasskeyLogin(context.Background(), "ctx-errors", credentialID, sign(a.Response.Challenge.String()), resolve)
	return err
}

// otherKey returns an authenticator with the same credential id as auth but
// a different private key, so its signatures do not verify against auth's
// stored public key.
func otherKey(t *testing.T, auth *softAuthenticator) *softAuthenticator {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &softAuthenticator{t: t, key: k, credID: auth.credID}
}

// Upstream's @simplewebauthn returns `verified: false` (not a throw) for a
// well-formed signature that does not verify, which upstream answers with
// 932c904e instead of b18c89a7.
func TestFinishPasskeyLogin_SignatureMismatchIsNotVerified(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)
	key := auth.securityKey(softUser.ID, 0)
	forger := otherKey(t, auth)

	err := finishPasskey(t, svc, key, key.ID, func(ch string) *http.Request {
		return forger.assert(ch, softUser.ID, true, 0)
	})
	assert.ErrorIs(t, err, ErrWebAuthnAssertionNotVerified)
	assert.NotErrorIs(t, err, ErrWebAuthnVerificationFailed)
}

// @simplewebauthn checks the counter before the signature and throws, so an
// assertion with both a bad counter and a bad signature is b18c89a7.
func TestFinishPasskeyLogin_CounterRollbackBeatsSignatureMismatch(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)
	key := auth.securityKey(softUser.ID, 5)
	forger := otherKey(t, auth)

	err := finishPasskey(t, svc, key, key.ID, func(ch string) *http.Request {
		return forger.assert(ch, softUser.ID, true, 5)
	})
	assert.ErrorIs(t, err, ErrWebAuthnVerificationFailed)
	assert.NotErrorIs(t, err, ErrWebAuthnAssertionNotVerified)

	// counter が進んでいれば署名の不一致として扱う。
	err = finishPasskey(t, svc, key, key.ID, func(ch string) *http.Request {
		return forger.assert(ch, softUser.ID, true, 6)
	})
	assert.ErrorIs(t, err, ErrWebAuthnAssertionNotVerified)
}

// Every other rejection (here: missing UV and a counter rollback on a valid
// signature) is a verification failure, b18c89a7.
func TestFinishPasskeyLogin_OtherRejectionsAreVerificationFailures(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)

	key := auth.securityKey(softUser.ID, 0)
	err := finishPasskey(t, svc, key, key.ID, func(ch string) *http.Request {
		return auth.assert(ch, softUser.ID, false, 0)
	})
	assert.ErrorIs(t, err, ErrWebAuthnVerificationFailed)
	assert.NotErrorIs(t, err, ErrWebAuthnAssertionNotVerified)

	key = auth.securityKey(softUser.ID, 5)
	err = finishPasskey(t, svc, key, key.ID, func(ch string) *http.Request {
		return auth.assert(ch, softUser.ID, true, 5)
	})
	assert.ErrorIs(t, err, ErrWebAuthnCounterRollback)
	assert.ErrorIs(t, err, ErrWebAuthnVerificationFailed)
}

// Upstream verifies with the key it looked up by `response.id`, and
// @simplewebauthn throws when id and rawId differ. go-webauthn picks the key
// by rawId, so a mismatch must not verify against the rawId's key.
func TestFinishPasskeyLogin_IDMustMatchRawID(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)
	key := auth.securityKey(softUser.ID, 0)

	err := finishPasskey(t, svc, key, "c29tZS1vdGhlci1rZXk", func(ch string) *http.Request {
		return auth.assert(ch, softUser.ID, true, 0)
	})
	assert.ErrorIs(t, err, ErrWebAuthnVerificationFailed)

	require.NoError(t, finishPasskey(t, svc, key, key.ID, func(ch string) *http.Request {
		return auth.assert(ch, softUser.ID, true, 0)
	}))
}

// go-webauthn reports an unreadable stored public key with the same
// `invalid_signature` type as a mismatch, but @simplewebauthn throws on it,
// so it is b18c89a7 and not 932c904e.
func TestFinishPasskeyLogin_UnreadablePublicKeyIsVerificationFailure(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)
	key := auth.securityKey(softUser.ID, 0)
	key.PublicKey = base64.RawURLEncoding.EncodeToString([]byte("not a COSE key"))

	err := finishPasskey(t, svc, key, key.ID, func(ch string) *http.Request {
		return auth.assert(ch, softUser.ID, true, 0)
	})
	assert.ErrorIs(t, err, ErrWebAuthnVerificationFailed)
	assert.NotErrorIs(t, err, ErrWebAuthnAssertionNotVerified)
}

func TestIsSignatureMismatch(t *testing.T) {
	assert.False(t, isSignatureMismatch(assert.AnError))
	assert.False(t, isSignatureMismatch(ErrWebAuthnVerificationFailed))
}

// CredentialRequest hands go-webauthn the credential JSON as a POST body
// without touching the original request.
func TestCredentialRequest(t *testing.T) {
	orig := httptest.NewRequest(http.MethodGet, "/api/i/2fa/key-done?x=1", strings.NewReader(`{"original":true}`))
	orig.Header.Set("Content-Type", "text/plain")
	orig.Header.Set("Origin", "https://example.com")
	body := json.RawMessage(`{"id":"AAEC"}`)

	req := CredentialRequest(orig, body)

	assert.Equal(t, http.MethodPost, req.Method)
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Equal(t, "https://example.com", req.Header.Get("Origin"))
	assert.Equal(t, int64(len(body)), req.ContentLength)
	assert.Nil(t, req.GetBody)
	assert.Equal(t, orig.Context(), req.Context())
	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(body, got))

	assert.Equal(t, http.MethodGet, orig.Method, "the original request must not change")
	assert.Equal(t, "text/plain", orig.Header.Get("Content-Type"))
	rest, err := io.ReadAll(orig.Body)
	require.NoError(t, err)
	assert.Equal(t, `{"original":true}`, string(rest))
}
