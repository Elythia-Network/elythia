package i

import (
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noneAttestation builds a "none" attestation over challenge for a fresh
// ES256 key, as a browser returns it from navigator.credentials.create.
func noneAttestation(t *testing.T, challenge string, credID []byte) json.RawMessage {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := key.PublicKey.ECDH()
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

	clientData, err := json.Marshal(map[string]any{
		"type": "webauthn.create", "challenge": challenge, "origin": "https://example.com",
	})
	require.NoError(t, err)
	rpHash := sha256.Sum256([]byte("example.com"))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, byte(protocol.FlagUserPresent)|byte(protocol.FlagUserVerified)|byte(protocol.FlagAttestedCredentialData))
	authData = binary.BigEndian.AppendUint32(authData, 0)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(credID)))
	authData = append(authData, credID...)
	authData = append(authData, cose...)
	attObj, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	require.NoError(t, err)

	enc := base64.RawURLEncoding.EncodeToString
	out, err := json.Marshal(map[string]any{
		"id": enc(credID), "rawId": enc(credID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": enc(clientData), "attestationObject": enc(attObj)},
	})
	require.NoError(t, err)
	return out
}

// i/2fa/key-done hands the credential member of its body to go-webauthn as
// a request of its own (twofactor.CredentialRequest), so a valid attestation
// registers the key.
func TestTwoFAKeyDone_RegistersKey(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")

	rec := postExtra(h.TwoFARegisterKey, `{"password":"pass","token":"backup1"}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var options struct {
		Challenge string `json:"challenge"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &options))
	require.NotEmpty(t, options.Challenge)

	credID := []byte("key-done-credential")
	body, err := json.Marshal(map[string]any{
		"password": "pass", "token": "backup2", "name": "my key",
		"credential": noneAttestation(t, options.Challenge, credID),
	})
	require.NoError(t, err)
	rec = postExtra(h.TwoFAKeyDone, string(body), user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored, err := skRepo.FindByID(base64.RawURLEncoding.EncodeToString(credID))
	require.NoError(t, err)
	assert.Equal(t, "u1", stored.UserID)
	assert.Equal(t, "my key", stored.Name)
}
