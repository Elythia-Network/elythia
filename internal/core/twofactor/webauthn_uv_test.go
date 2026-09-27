package twofactor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	softOrigin = "https://example.com"
	softRPID   = "example.com"
)

// softAuthenticator is a minimal ES256 authenticator that signs assertions
// in-process, so the tests exercise go-webauthn's real verification.
type softAuthenticator struct {
	t      *testing.T
	key    *ecdsa.PrivateKey
	credID []byte
}

func newSoftAuthenticator(t *testing.T) *softAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &softAuthenticator{t: t, key: key, credID: []byte("soft-credential-1")}
}

// securityKey returns the stored row for this authenticator with the given
// counter.
func (a *softAuthenticator) securityKey(userID string, counter int64) *model.UserSecurityKey {
	a.t.Helper()
	pub, err := a.key.PublicKey.ECDH()
	require.NoError(a.t, err)
	raw := pub.Bytes() // 0x04 || X || Y
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{
			KeyType:   int64(webauthncose.EllipticKey),
			Algorithm: int64(webauthncose.AlgES256),
		},
		Curve:  int64(webauthncose.P256),
		XCoord: raw[1:33],
		YCoord: raw[33:65],
	})
	require.NoError(a.t, err)
	return &model.UserSecurityKey{
		ID:        base64.RawURLEncoding.EncodeToString(a.credID),
		UserID:    userID,
		PublicKey: base64.RawURLEncoding.EncodeToString(cose),
		Counter:   counter,
	}
}

// attest builds a registration response ("none" attestation) for challenge.
func (a *softAuthenticator) attest(challenge string, uv bool) *http.Request {
	a.t.Helper()
	clientData, err := json.Marshal(map[string]any{
		"type":      "webauthn.create",
		"challenge": challenge,
		"origin":    softOrigin,
	})
	require.NoError(a.t, err)
	cosePub, err := base64.RawURLEncoding.DecodeString(a.securityKey(softUser.ID, 0).PublicKey)
	require.NoError(a.t, err)

	rpHash := sha256.Sum256([]byte(softRPID))
	flags := byte(protocol.FlagUserPresent) | byte(protocol.FlagAttestedCredentialData)
	if uv {
		flags |= byte(protocol.FlagUserVerified)
	}
	authData := make([]byte, 0, 128)
	authData = append(authData, rpHash[:]...)
	authData = append(authData, flags)
	authData = binary.BigEndian.AppendUint32(authData, 0)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(a.credID)))
	authData = append(authData, a.credID...)
	authData = append(authData, cosePub...)

	attObj, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	require.NoError(a.t, err)

	enc := base64.RawURLEncoding.EncodeToString
	body, err := json.Marshal(map[string]any{
		"id":    enc(a.credID),
		"rawId": enc(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    enc(clientData),
			"attestationObject": enc(attObj),
		},
	})
	require.NoError(a.t, err)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// assert signs the challenge and returns the request carrying the assertion.
func (a *softAuthenticator) assert(challenge string, userHandle string, uv bool, counter uint32) *http.Request {
	a.t.Helper()
	clientData, err := json.Marshal(map[string]any{
		"type":      "webauthn.get",
		"challenge": challenge,
		"origin":    softOrigin,
	})
	require.NoError(a.t, err)

	rpHash := sha256.Sum256([]byte(softRPID))
	flags := byte(protocol.FlagUserPresent)
	if uv {
		flags |= byte(protocol.FlagUserVerified)
	}
	authData := make([]byte, 0, 37)
	authData = append(authData, rpHash[:]...)
	authData = append(authData, flags)
	authData = binary.BigEndian.AppendUint32(authData, counter)

	cdHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	require.NoError(a.t, err)

	enc := base64.RawURLEncoding.EncodeToString
	body, err := json.Marshal(map[string]any{
		"id":    enc(a.credID),
		"rawId": enc(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    enc(clientData),
			"authenticatorData": enc(authData),
			"signature":         enc(sig),
			"userHandle":        enc([]byte(userHandle)),
		},
	})
	require.NoError(a.t, err)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func newSoftService(t *testing.T) *WebAuthnService {
	t.Helper()
	requireRedis(t)
	twofaTestRedis.FlushAll(context.Background())
	svc, err := NewWebAuthnService(softOrigin, "Misskey", twofaTestRedis.Client)
	require.NoError(t, err)
	return svc
}

var softUser = &model.User{ID: "alice", Username: "alice"}

// requireUVRejected asserts that err is go-webauthn's missing-UV rejection and
// not some unrelated failure of the synthesized assertion.
func requireUVRejected(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err, "UV フラグの無い assertion で鍵だけのログインが通った")
	var perr *protocol.Error
	require.ErrorAs(t, err, &perr)
	assert.Contains(t, perr.DevInfo, "User verification required")
}

// 2 要素目としての鍵も UV を必須にする (upstream の verifyAuthentication は
// `requireUserVerification: true`)。ブラウザへの options は upstream と同じ preferred。
func TestFinishLogin_SecondFactorRequiresUV(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)
	keys := []*model.UserSecurityKey{auth.securityKey(softUser.ID, 0)}

	a, err := svc.BeginLogin(context.Background(), softUser, keys)
	require.NoError(t, err)
	assert.Equal(t, protocol.VerificationPreferred, a.Response.UserVerification)

	_, err = svc.FinishLogin(context.Background(), softUser, keys,
		auth.assert(a.Response.Challenge.String(), softUser.ID, false, 0))
	requireUVRejected(t, err)

	a, err = svc.BeginLogin(context.Background(), softUser, keys)
	require.NoError(t, err)
	cred, err := svc.FinishLogin(context.Background(), softUser, keys,
		auth.assert(a.Response.Challenge.String(), softUser.ID, true, 0))
	require.NoError(t, err)
	assert.Equal(t, auth.credID, cred.ID)
}

// Redis に残っている session が preferred のままでも required で検証する
// (options は preferred なので、保存値を信じると UV を見ない)。
func TestFinishLogin_RequiresUVRegardlessOfStoredSession(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)
	keys := []*model.UserSecurityKey{auth.securityKey(softUser.ID, 0)}

	a, err := svc.BeginLogin(context.Background(), softUser, keys)
	require.NoError(t, err)
	sd, err := svc.takeLoginSession(context.Background(), softUser.ID)
	require.NoError(t, err)
	assert.Equal(t, protocol.VerificationPreferred, sd.UserVerification)
	require.NoError(t, svc.putLoginSession(context.Background(), softUser.ID, sd))

	_, err = svc.FinishLogin(context.Background(), softUser, keys,
		auth.assert(a.Response.Challenge.String(), softUser.ID, false, 0))
	requireUVRejected(t, err)
}

func TestPasskeyLogin_RequiresUV(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)
	keys := []*model.UserSecurityKey{auth.securityKey(softUser.ID, 0)}
	resolve := func(_, userHandle []byte) (*model.User, []*model.UserSecurityKey, error) {
		require.Equal(t, softUser.ID, string(userHandle))
		return softUser, keys, nil
	}

	a, err := svc.BeginPasskeyLogin(context.Background(), "ctx-uv")
	require.NoError(t, err)
	assert.Equal(t, protocol.VerificationPreferred, a.Response.UserVerification,
		"options は upstream と同じ preferred")
	_, _, err = svc.FinishPasskeyLogin(context.Background(), "ctx-uv",
		auth.assert(a.Response.Challenge.String(), softUser.ID, false, 0), resolve)
	requireUVRejected(t, err)

	a, err = svc.BeginPasskeyLogin(context.Background(), "ctx-uv2")
	require.NoError(t, err)
	u, cred, err := svc.FinishPasskeyLogin(context.Background(), "ctx-uv2",
		auth.assert(a.Response.Challenge.String(), softUser.ID, true, 0), resolve)
	require.NoError(t, err)
	assert.Equal(t, softUser, u)
	assert.Equal(t, auth.credID, cred.ID)
}

// 登録も UV を必須にする (upstream の verifyRegistration は
// `requireUserVerification: true`)。UV の無い鍵を登録できると、2 要素目としても
// パスキーとしても使えない鍵が残る。
func TestFinishRegistration_RequiresUV(t *testing.T) {
	svc := newSoftService(t)
	auth := newSoftAuthenticator(t)

	c, err := svc.BeginRegistration(context.Background(), softUser, nil)
	require.NoError(t, err)
	assert.Equal(t, protocol.VerificationPreferred, c.Response.AuthenticatorSelection.UserVerification,
		"options は upstream と同じ preferred")
	_, err = svc.FinishRegistration(context.Background(), softUser, nil,
		auth.attest(c.Response.Challenge.String(), false))
	requireUVRejected(t, err)

	c, err = svc.BeginRegistration(context.Background(), softUser, nil)
	require.NoError(t, err)
	cred, err := svc.FinishRegistration(context.Background(), softUser, nil,
		auth.attest(c.Response.Challenge.String(), true))
	require.NoError(t, err)
	assert.Equal(t, auth.credID, cred.ID)
}

// 署名カウンタが進まない assertion は複製された認証器の兆候なので拒否する
// (upstream の @simplewebauthn と同じ)。counter を持たない認証器 (常に 0) は
// 誤検知しない。
func TestLogin_RejectsCounterRollback(t *testing.T) {
	cases := []struct {
		name    string
		stored  int64
		counter uint32
		reject  bool
	}{
		{name: "advanced", stored: 5, counter: 6, reject: false},
		{name: "equal", stored: 5, counter: 5, reject: true},
		{name: "rolled back", stored: 5, counter: 3, reject: true},
		{name: "reset to zero", stored: 5, counter: 0, reject: true},
		{name: "counterless authenticator", stored: 0, counter: 0, reject: false},
	}
	for _, tc := range cases {
		t.Run("second factor/"+tc.name, func(t *testing.T) {
			svc := newSoftService(t)
			auth := newSoftAuthenticator(t)
			keys := []*model.UserSecurityKey{auth.securityKey(softUser.ID, tc.stored)}
			a, err := svc.BeginLogin(context.Background(), softUser, keys)
			require.NoError(t, err)
			_, err = svc.FinishLogin(context.Background(), softUser, keys,
				auth.assert(a.Response.Challenge.String(), softUser.ID, true, tc.counter))
			if tc.reject {
				assert.ErrorIs(t, err, ErrWebAuthnCounterRollback)
			} else {
				assert.NoError(t, err)
			}
		})
		t.Run("passkey/"+tc.name, func(t *testing.T) {
			svc := newSoftService(t)
			auth := newSoftAuthenticator(t)
			keys := []*model.UserSecurityKey{auth.securityKey(softUser.ID, tc.stored)}
			resolve := func(_, _ []byte) (*model.User, []*model.UserSecurityKey, error) {
				return softUser, keys, nil
			}
			a, err := svc.BeginPasskeyLogin(context.Background(), "ctx-counter")
			require.NoError(t, err)
			_, _, err = svc.FinishPasskeyLogin(context.Background(), "ctx-counter",
				auth.assert(a.Response.Challenge.String(), softUser.ID, true, tc.counter), resolve)
			if tc.reject {
				assert.ErrorIs(t, err, ErrWebAuthnCounterRollback)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
