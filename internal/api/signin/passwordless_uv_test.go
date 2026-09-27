package signin_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/shiroha-a/mk/internal/api/signin"
	"github.com/shiroha-a/mk/internal/core/twofactor"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uvTestKey is an in-process ES256 authenticator used to drive signin-flow
// through go-webauthn's real assertion verification.
type uvTestKey struct {
	key    *ecdsa.PrivateKey
	credID []byte
}

func newUVTestKey(t *testing.T) *uvTestKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &uvTestKey{key: k, credID: []byte("uv-test-credential")}
}

func (k *uvTestKey) row(t *testing.T, userID string) *model.UserSecurityKey {
	t.Helper()
	pub, err := k.key.PublicKey.ECDH()
	require.NoError(t, err)
	raw := pub.Bytes()
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{
			KeyType:   int64(webauthncose.EllipticKey),
			Algorithm: int64(webauthncose.AlgES256),
		},
		Curve:  int64(webauthncose.P256),
		XCoord: raw[1:33],
		YCoord: raw[33:65],
	})
	require.NoError(t, err)
	return &model.UserSecurityKey{
		ID:        base64.RawURLEncoding.EncodeToString(k.credID),
		UserID:    userID,
		PublicKey: base64.RawURLEncoding.EncodeToString(cose),
	}
}

// credential returns the JSON of an assertion over challenge, with or
// without the User Verified flag.
func (k *uvTestKey) credential(t *testing.T, challenge, userID string, uv bool) json.RawMessage {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{
		"type": "webauthn.get", "challenge": challenge, "origin": "https://example.com",
	})
	require.NoError(t, err)
	rpHash := sha256.Sum256([]byte("example.com"))
	flags := byte(protocol.FlagUserPresent)
	if uv {
		flags |= byte(protocol.FlagUserVerified)
	}
	authData := append(append([]byte{}, rpHash[:]...), flags)
	authData = binary.BigEndian.AppendUint32(authData, 0)
	cdHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	out, err := json.Marshal(map[string]any{
		"id": enc(k.credID), "rawId": enc(k.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    enc(clientData),
			"authenticatorData": enc(authData),
			"signature":         enc(sig),
			"userHandle":        enc([]byte(userID)),
		},
	})
	require.NoError(t, err)
	return out
}

func setupPasswordlessUV(t *testing.T) (*signin.Handler, *uvTestKey) {
	t.Helper()
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	key := newUVTestKey(t)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{"u1": {key.row(t, "u1")}}})
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	repo.Profiles["u1"].UsePasswordLessLogin = true
	return h, key
}

func signinFlowChallenge(t *testing.T, h *signin.Handler, password string) (challenge, uv string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"username": "alice", "password": password})
	require.NoError(t, err)
	rec := doPost(h.SigninFlow, string(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Next        string `json:"next"`
		AuthRequest struct {
			Challenge        string `json:"challenge"`
			UserVerification string `json:"userVerification"`
		} `json:"authRequest"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "passkey", resp.Next)
	return resp.AuthRequest.Challenge, resp.AuthRequest.UserVerification
}

func signinFlowWithCredential(t *testing.T, h *signin.Handler, password string, cred json.RawMessage) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{"username": "alice", "password": password, "credential": cred})
	require.NoError(t, err)
	return doPost(h.SigninFlow, string(body)).Code
}

// usePasswordLessLogin でパスワードが合っていないとき、鍵が唯一の要素になる。
// UV (PIN / 生体認証) の無い assertion で通ると、鍵を拾った相手がそれだけで
// ログインできる。ブラウザへの options は upstream と同じ preferred。
func TestSigninFlow_PasswordlessKeyRequiresUV(t *testing.T) {
	h, key := setupPasswordlessUV(t)

	challenge, uv := signinFlowChallenge(t, h, "wrong")
	assert.Equal(t, string(protocol.VerificationPreferred), uv)
	assert.Equal(t, http.StatusForbidden,
		signinFlowWithCredential(t, h, "wrong", key.credential(t, challenge, "u1", false)),
		"UV の無い鍵だけでログインできた")

	challenge, _ = signinFlowChallenge(t, h, "wrong")
	assert.Equal(t, http.StatusOK,
		signinFlowWithCredential(t, h, "wrong", key.credential(t, challenge, "u1", true)))
}

// パスワード + 2 要素目としての鍵も UV を要求する (upstream の
// verifyAuthentication は `requireUserVerification: true`)。
func TestSigninFlow_SecondFactorKeyRequiresUV(t *testing.T) {
	h, key := setupPasswordlessUV(t)

	challenge, uv := signinFlowChallenge(t, h, "pass")
	assert.Equal(t, string(protocol.VerificationPreferred), uv)
	assert.Equal(t, http.StatusForbidden,
		signinFlowWithCredential(t, h, "pass", key.credential(t, challenge, "u1", false)),
		"UV の無い鍵が 2 要素目として通った")

	challenge, _ = signinFlowChallenge(t, h, "pass")
	assert.Equal(t, http.StatusOK,
		signinFlowWithCredential(t, h, "pass", key.credential(t, challenge, "u1", true)))
}

// challenge は user 単位で 1 件しか無い。正しいパスワードで始めた challenge に
// パスワード無しの credential を載せても UV を要求すること。
func TestSigninFlow_PasswordlessKeyRequiresUVOnPasswordChallenge(t *testing.T) {
	h, key := setupPasswordlessUV(t)

	challenge, _ := signinFlowChallenge(t, h, "pass")
	assert.Equal(t, http.StatusForbidden,
		signinFlowWithCredential(t, h, "wrong", key.credential(t, challenge, "u1", false)))
}
