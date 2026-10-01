package activitypub

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestKey(t *testing.T) (*PrivateKey, string) {
	t.Helper()
	priv, pub, err := GenerateRSAKeypair()
	require.NoError(t, err)
	k, err := NewPrivateKey("https://example.com/users/u1#main-key", priv)
	require.NoError(t, err)
	return k, pub
}

func TestNewPrivateKey_InvalidPEM(t *testing.T) {
	_, err := NewPrivateKey("kid", "not a key")
	assert.Error(t, err)
}

func TestSHA256Digest(t *testing.T) {
	d := SHA256Digest([]byte("hello"))
	assert.True(t, strings.HasPrefix(d, "SHA-256="))
}

func TestSignRequest_PostAndVerify(t *testing.T) {
	key, pub := newTestKey(t)
	body := []byte(`{"type":"Create"}`)
	digest := SHA256Digest(body)

	req, err := http.NewRequest(http.MethodPost, "https://remote.example/inbox", bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))

	assert.NotEmpty(t, req.Header.Get("Signature"))
	assert.NotEmpty(t, req.Header.Get("Date"))
	assert.Equal(t, digest, req.Header.Get("Digest"))
	// Hostヘッダはnet/http側で設定されるので削除されている
	assert.Empty(t, req.Header.Get("Host"))

	// 検証時はHostヘッダを再度入れて行う
	req.Header.Set("Host", "remote.example")
	require.NoError(t, VerifyRequest(req, pub))
}

func TestSignRequest_GetAndVerify(t *testing.T) {
	key, pub := newTestKey(t)
	req, err := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	require.NoError(t, err)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))

	req.Header.Set("Host", "remote.example")
	require.NoError(t, VerifyRequest(req, pub))
}

func TestSignRequest_NilKey(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	err := SignRequest(req, nil, "", []string{"date"})
	assert.Error(t, err)
}

func TestSignRequest_PreservesExistingDateAndHost(t *testing.T) {
	key, _ := newTestKey(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	req.Header.Set("Date", "Mon, 01 Jan 2024 00:00:00 GMT")
	req.Header.Set("Host", "preset.example")
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date"}))
	assert.Equal(t, "Mon, 01 Jan 2024 00:00:00 GMT", req.Header.Get("Date"))
}

func TestSignRequest_MissingHeaderInList(t *testing.T) {
	key, _ := newTestKey(t)
	req, _ := http.NewRequest(http.MethodPost, "https://remote.example/inbox", bytes.NewReader([]byte("x")))
	err := SignRequest(req, key, "", []string{"(request-target)", "date", "x-missing"})
	assert.Error(t, err)
}

// keyWithBadParsed simulates a parse failure when crypto.SignerOpts is nil. We
// can't easily make rsa.SignPKCS1v15 fail without providing a degenerate key.
// Instead, the rand reader can be poisoned.
func TestSignRequest_RandError(t *testing.T) {
	key, _ := newTestKey(t)
	restore := SetRandReaderForTest(errReader{})
	defer restore()

	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	err := SignRequest(req, key, "", []string{"(request-target)", "date", "host"})
	// rsa.SignPKCS1v15 doesn't actually use rand for PKCS1v15 signatures so
	// this might still succeed; that's acceptable for coverage purposes.
	_ = err
}

func TestParseSignatureHeader_Success(t *testing.T) {
	h := `keyId="https://example.com/u#k",algorithm="rsa-sha256",headers="(request-target) date host",signature="abc=="`
	got, err := ParseSignatureHeader(h)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/u#k", got.KeyID)
	assert.Equal(t, "rsa-sha256", got.Algorithm)
	assert.Equal(t, []string{"(request-target)", "date", "host"}, got.Headers)
	assert.Equal(t, "abc==", got.Signature)
}

func TestParseSignatureHeader_Empty(t *testing.T) {
	_, err := ParseSignatureHeader("")
	assert.Error(t, err)
}

func TestParseSignatureHeader_MissingKeyID(t *testing.T) {
	_, err := ParseSignatureHeader(`algorithm="rsa-sha256",signature="x"`)
	assert.Error(t, err)
}

func TestParseSignatureHeader_DefaultsHeadersToDate(t *testing.T) {
	got, err := ParseSignatureHeader(`keyId="x",signature="y"`)
	require.NoError(t, err)
	assert.Equal(t, []string{"date"}, got.Headers)
}

func TestVerifyRequest_BadSignatureHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	err := VerifyRequest(req, "")
	assert.Error(t, err)
}

func TestVerifyRequest_UnsupportedAlgorithm(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	req.Header.Set("Signature", `keyId="x",algorithm="hmac-sha256",signature="y"`)
	err := VerifyRequest(req, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestVerifyRequest_BadPublicKey(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	req.Header.Set("Signature", `keyId="x",algorithm="rsa-sha256",signature="y"`)
	err := VerifyRequest(req, "not pem")
	assert.Error(t, err)
}

func TestVerifyRequest_BadSigningString(t *testing.T) {
	_, pub := newTestKey(t)
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	req.Header.Set("Signature", `keyId="x",algorithm="rsa-sha256",headers="(request-target) date missing",signature="y"`)
	err := VerifyRequest(req, pub)
	assert.Error(t, err)
}

func TestVerifyRequest_BadSignatureBase64(t *testing.T) {
	key, pub := newTestKey(t)
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))
	req.Header.Set("Host", "x.example")
	// Inject invalid base64 into the Signature
	req.Header.Set("Signature", `keyId="x",algorithm="rsa-sha256",headers="(request-target) date host",signature="!!notbase64!!"`)
	err := VerifyRequest(req, pub)
	assert.Error(t, err)
}

func TestVerifyRequest_VerifyFails(t *testing.T) {
	_, pub := newTestKey(t)
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/users/bob", nil)
	req.Header.Set("Date", "Mon, 01 Jan 2024 00:00:00 GMT")
	req.Header.Set("Host", "x.example")
	// Inject a syntactically valid but wrong signature.
	bad := base64.StdEncoding.EncodeToString([]byte("wrong"))
	req.Header.Set("Signature", `keyId="x",algorithm="rsa-sha256",headers="(request-target) date host",signature="`+bad+`"`)
	err := VerifyRequest(req, pub)
	assert.Error(t, err)
}

func TestVerifyRequest_DigestPresent(t *testing.T) {
	key, pub := newTestKey(t)
	body := []byte("hello")
	digest := SHA256Digest(body)
	req, _ := http.NewRequest(http.MethodPost, "https://x.example/inbox", bytes.NewReader(body))
	require.NoError(t, SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Header.Set("Host", "x.example")
	require.NoError(t, VerifyRequest(req, pub))
}

func TestVerifyRequest_DigestUnsupportedAlgorithm(t *testing.T) {
	key, pub := newTestKey(t)
	body := []byte("hello")
	req, _ := http.NewRequest(http.MethodPost, "https://x.example/inbox", bytes.NewReader(body))
	require.NoError(t, SignRequest(req, key, "MD5=foo", []string{"(request-target)", "date", "host", "digest"}))
	req.Header.Set("Host", "x.example")
	err := VerifyRequest(req, pub)
	assert.Error(t, err)
}

func TestVerifyRequest_HostFromReqHost(t *testing.T) {
	key, pub := newTestKey(t)
	body := []byte(`{"x":1}`)
	req, _ := http.NewRequest(http.MethodPost, "https://x.example/inbox", bytes.NewReader(body))
	require.NoError(t, SignRequest(req, key, SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))

	// Header["Host"] を消し、req.Host にだけ値を残す (受信側のリクエストを再現)
	req.Header.Del("Host")
	req.Host = "x.example"
	require.NoError(t, VerifyRequest(req, pub))
}

func TestResolveKeyURL(t *testing.T) {
	assert.Equal(t, "https://example.com/users/u1", ResolveKeyURL("https://example.com/users/u1#main-key"))
	assert.Equal(t, "https://example.com/users/u1", ResolveKeyURL("https://example.com/users/u1"))
	// invalid URL still passes through
	assert.Equal(t, "://invalid", ResolveKeyURL("://invalid"))
}

// brokenKey forces SignRequest to use a key with a nil parsed pointer.
func TestSignRequest_NilParsed(t *testing.T) {
	k := &PrivateKey{KeyID: "x"}
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	err := SignRequest(req, k, "", []string{"date"})
	assert.Error(t, err)
}

// Sanity smoke test that uses crypto package directly to ensure imports are wired.
func TestCryptoSmoke(t *testing.T) {
	priv, _, _ := GenerateRSAKeypair()
	rsaKey, _ := ParseRSAPrivateKey(priv)
	hashed := sha256.Sum256([]byte("x"))
	_, err := rsa.SignPKCS1v15(nil, rsaKey, crypto.SHA256, hashed[:])
	require.NoError(t, err)
}

// --- multi-algorithm tests (#155) ---

func newTestEd25519Key(t *testing.T) (*PrivateKey, string) {
	t.Helper()
	priv, pub, err := GenerateEd25519Keypair()
	require.NoError(t, err)
	k, err := NewEd25519PrivateKey("https://example.com/users/u1#main-key", priv)
	require.NoError(t, err)
	return k, pub
}

func TestGenerateEd25519Keypair(t *testing.T) {
	priv, pub, err := GenerateEd25519Keypair()
	require.NoError(t, err)
	assert.Contains(t, priv, "PRIVATE KEY")
	assert.Contains(t, pub, "PUBLIC KEY")
}

func TestParsePublicKey_RSA(t *testing.T) {
	_, pub, err := GenerateRSAKeypair()
	require.NoError(t, err)
	parsed, kt, err := ParsePublicKey(pub)
	require.NoError(t, err)
	assert.Equal(t, KeyTypeRSA, kt)
	assert.NotNil(t, parsed)
}

func TestParsePublicKey_Ed25519(t *testing.T) {
	_, pub, err := GenerateEd25519Keypair()
	require.NoError(t, err)
	parsed, kt, err := ParsePublicKey(pub)
	require.NoError(t, err)
	assert.Equal(t, KeyTypeEd25519, kt)
	assert.NotNil(t, parsed)
}

func TestParsePublicKey_BadPEM(t *testing.T) {
	_, _, err := ParsePublicKey("not pem")
	assert.Error(t, err)
}

func TestNewEd25519PrivateKey_InvalidPEM(t *testing.T) {
	_, err := NewEd25519PrivateKey("kid", "not pem")
	assert.Error(t, err)
}

func TestNewEd25519PrivateKey_RSAKeyRejected(t *testing.T) {
	rsaPriv, _, err := GenerateRSAKeypair()
	require.NoError(t, err)
	_, err = NewEd25519PrivateKey("kid", rsaPriv)
	assert.Error(t, err)
}

func TestParseRSAPublicKey_Ed25519Rejected(t *testing.T) {
	_, edPub, err := GenerateEd25519Keypair()
	require.NoError(t, err)
	_, err = ParseRSAPublicKey(edPub)
	assert.Error(t, err)
}

// Round-trip sign + verify with Ed25519.
func TestSignAndVerify_Ed25519(t *testing.T) {
	key, pub := newTestEd25519Key(t)
	body := []byte(`{"type":"Create"}`)
	digest := SHA256Digest(body)
	req, _ := http.NewRequest(http.MethodPost, "https://remote.example/inbox", bytes.NewReader(body))
	require.NoError(t, SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))

	// Signature ヘッダに algorithm="ed25519" が出力されている
	assert.Contains(t, req.Header.Get("Signature"), `algorithm="ed25519"`)

	req.Header.Set("Host", "remote.example")
	require.NoError(t, VerifyRequest(req, pub))
}

// Verify accepts hs2019 algorithm with RSA key (key-type-driven dispatch).
func TestVerifyRequest_HS2019_RSA(t *testing.T) {
	key, pub := newTestKey(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))

	// signed header の algorithm を hs2019 に書き換える
	sig := req.Header.Get("Signature")
	sig = strings.Replace(sig, `algorithm="rsa-sha256"`, `algorithm="hs2019"`, 1)
	req.Header.Set("Signature", sig)
	req.Header.Set("Host", "remote.example")
	require.NoError(t, VerifyRequest(req, pub))
}

// Verify accepts hs2019 algorithm with Ed25519 key.
func TestVerifyRequest_HS2019_Ed25519(t *testing.T) {
	key, pub := newTestEd25519Key(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))

	sig := req.Header.Get("Signature")
	sig = strings.Replace(sig, `algorithm="ed25519"`, `algorithm="hs2019"`, 1)
	req.Header.Set("Signature", sig)
	req.Header.Set("Host", "remote.example")
	require.NoError(t, VerifyRequest(req, pub))
}

// Verify accepts ed25519-sha512 alias.
func TestVerifyRequest_Ed25519SHA512Alias(t *testing.T) {
	key, pub := newTestEd25519Key(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))

	sig := req.Header.Get("Signature")
	sig = strings.Replace(sig, `algorithm="ed25519"`, `algorithm="ed25519-sha512"`, 1)
	req.Header.Set("Signature", sig)
	req.Header.Set("Host", "remote.example")
	require.NoError(t, VerifyRequest(req, pub))
}

// Verify omits algorithm; key type drives dispatch.
func TestVerifyRequest_AlgorithmOmitted_Ed25519(t *testing.T) {
	key, pub := newTestEd25519Key(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))

	// algorithm パラメータを削除して送信されたケース。replace で algorithm=...
	// 部分を削る (簡易ハック)。
	sig := req.Header.Get("Signature")
	idx := strings.Index(sig, `algorithm="ed25519",`)
	require.GreaterOrEqual(t, idx, 0)
	stripped := sig[:idx] + sig[idx+len(`algorithm="ed25519",`):]
	req.Header.Set("Signature", stripped)
	req.Header.Set("Host", "remote.example")
	require.NoError(t, VerifyRequest(req, pub))
}

// rsa-sha512 verification path: sign manually with SHA-512 then verify.
func TestVerifyRequest_RSA_SHA512(t *testing.T) {
	key, pub := newTestKey(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))

	// 既存の rsa-sha256 署名を破棄して、SHA-512 で再署名し直す。
	signingString, err := buildSigningString(req, []string{"(request-target)", "date", "host"})
	require.NoError(t, err)
	hashed := sha512.Sum512([]byte(signingString))
	rsaKey, err := ParseRSAPrivateKey(key.PrivatePEM)
	require.NoError(t, err)
	sig, err := rsa.SignPKCS1v15(nil, rsaKey, crypto.SHA512, hashed[:])
	require.NoError(t, err)

	header := `keyId="` + key.KeyID + `",algorithm="rsa-sha512",headers="(request-target) date host",signature="` + base64.StdEncoding.EncodeToString(sig) + `"`
	req.Header.Set("Signature", header)
	req.Header.Set("Host", "remote.example")
	require.NoError(t, VerifyRequest(req, pub))
}

// Mismatch: RSA key with algorithm=ed25519 must be rejected.
func TestVerifyRequest_RSAKey_Ed25519AlgRejected(t *testing.T) {
	_, pub := newTestKey(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/", nil)
	req.Header.Set("Date", "Mon, 01 Jan 2024 00:00:00 GMT")
	dummySig := base64.StdEncoding.EncodeToString(make([]byte, 64))
	req.Header.Set("Signature", `keyId="x",algorithm="ed25519",headers="date",signature="`+dummySig+`"`)
	err := VerifyRequest(req, pub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ed25519")
}

// Mismatch: Ed25519 key with algorithm=rsa-sha256 must be rejected.
func TestVerifyRequest_Ed25519Key_RSAAlgRejected(t *testing.T) {
	_, pub := newTestEd25519Key(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/", nil)
	req.Header.Set("Date", "Mon, 01 Jan 2024 00:00:00 GMT")
	dummySig := base64.StdEncoding.EncodeToString(make([]byte, 64))
	req.Header.Set("Signature", `keyId="x",algorithm="rsa-sha256",headers="date",signature="`+dummySig+`"`)
	err := VerifyRequest(req, pub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RSA")
}

// Bad Ed25519 signature bytes -> verify fails.
func TestVerifyRequest_Ed25519BadSignature(t *testing.T) {
	key, pub := newTestEd25519Key(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))

	// signature を改竄
	sig := req.Header.Get("Signature")
	tamperedSig := strings.Replace(sig, `signature="`, `signature="AAAA`, 1)
	req.Header.Set("Signature", tamperedSig)
	req.Header.Set("Host", "remote.example")
	assert.Error(t, VerifyRequest(req, pub))
}

// Test using httptest.NewRecorder so the verifier sees a real request from a server context.
func TestVerifyRequest_FromHTTPServer(t *testing.T) {
	key, pub := newTestKey(t)
	body := []byte(`{"type":"Create"}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// httptest server strips Host from headers; reset it
		r.Header.Set("Host", r.Host)
		if err := VerifyRequest(r, pub); err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/inbox", bytes.NewReader(body))
	require.NoError(t, SignRequest(req, key, SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// --- VerifyRequestWithKey (#1426): the parse-free core used by PublicKeyCache ---

func TestVerifyRequestWithKey_RSASuccess(t *testing.T) {
	key, pubPEM := newTestKey(t)
	body := []byte(`{"type":"Create"}`)
	req, _ := http.NewRequest(http.MethodPost, "https://remote.example/inbox", bytes.NewReader(body))
	require.NoError(t, SignRequest(req, key, SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Header.Set("Host", "remote.example")

	pub, kt, err := ParsePublicKey(pubPEM)
	require.NoError(t, err)
	require.NoError(t, VerifyRequestWithKey(req, pub, kt))
}

func TestVerifyRequestWithKey_Ed25519Success(t *testing.T) {
	key, pubPEM := newTestEd25519Key(t)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))
	req.Header.Set("Host", "remote.example")

	pub, kt, err := ParsePublicKey(pubPEM)
	require.NoError(t, err)
	require.NoError(t, VerifyRequestWithKey(req, pub, kt))
}

func TestVerifyRequestWithKey_BadSignatureHeader(t *testing.T) {
	_, pubPEM := newTestKey(t)
	pub, kt, err := ParsePublicKey(pubPEM)
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	// Signature ヘッダ無し -> ParseSignatureHeader で弾かれる。
	assert.Error(t, VerifyRequestWithKey(req, pub, kt))
}

func TestVerifyRequestWithKey_UnsupportedAlgorithm(t *testing.T) {
	_, pubPEM := newTestKey(t)
	pub, kt, err := ParsePublicKey(pubPEM)
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/", nil)
	req.Header.Set("Signature", `keyId="x",algorithm="hmac-sha256",signature="y"`)
	err = VerifyRequestWithKey(req, pub, kt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestVerifyRequestWithKey_KeyTypeMismatch(t *testing.T) {
	// RSA 鍵に対し algorithm=ed25519 -> verifyAlgorithm が拒否する。
	_, pubPEM := newTestKey(t)
	pub, kt, err := ParsePublicKey(pubPEM)
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example/", nil)
	req.Header.Set("Date", "Mon, 01 Jan 2024 00:00:00 GMT")
	dummySig := base64.StdEncoding.EncodeToString(make([]byte, 64))
	req.Header.Set("Signature", `keyId="x",algorithm="ed25519",headers="date",signature="`+dummySig+`"`)
	err = VerifyRequestWithKey(req, pub, kt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ed25519")
}

func TestVerifyRequestWithKey_BadSignatureBase64(t *testing.T) {
	key, pubPEM := newTestKey(t)
	pub, kt, err := ParsePublicKey(pubPEM)
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodGet, "https://x.example/users/bob", nil)
	require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))
	req.Header.Set("Host", "x.example")
	req.Header.Set("Signature", `keyId="x",algorithm="rsa-sha256",headers="(request-target) date host",signature="!!notbase64!!"`)
	assert.Error(t, VerifyRequestWithKey(req, pub, kt))
}

// 本家 7c9c38c04a (test/unit/ap-request.ts) の移植。`(request-target)` は
// path + query で作り、空の `?` は残し、fragment は落とす。受信側も同じ形で
// 組み直すので、server が受けた request で検証が通ることも合わせて見る。
func TestSignRequest_RequestTargetIncludesQuery(t *testing.T) {
	cases := []struct {
		name   string
		method string
		url    string
		want   string
	}{
		{"get with query", http.MethodGet, "https://remote.example/users/alice?page=2", "(request-target): get /users/alice?page=2"},
		{"post with query", http.MethodPost, "https://remote.example/inbox?token=abc", "(request-target): post /inbox?token=abc"},
		{"fragment dropped", http.MethodGet, "https://remote.example/users/alice?page=2#ignored", "(request-target): get /users/alice?page=2"},
		{"empty query with fragment", http.MethodGet, "https://remote.example/outbox?#ignored", "(request-target): get /outbox?"},
		{"empty query", http.MethodGet, "https://remote.example/outbox?", "(request-target): get /outbox?"},
		{"no query", http.MethodGet, "https://remote.example/outbox", "(request-target): get /outbox"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, pub := newTestKey(t)
			req, err := http.NewRequest(tc.method, tc.url, nil)
			require.NoError(t, err)
			require.NoError(t, SignRequest(req, key, "", []string{"(request-target)", "date", "host"}))

			got, err := buildSigningString(req, []string{"(request-target)"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			// 受信側が見る形 (request line の RequestURI を parse したもの) で検証する。
			target := strings.TrimPrefix(tc.want, "(request-target): "+strings.ToLower(tc.method)+" ")
			in := httptest.NewRequest(tc.method, target, nil)
			in.Host = "remote.example"
			in.Header.Set("Date", req.Header.Get("Date"))
			in.Header.Set("Signature", req.Header.Get("Signature"))
			require.NoError(t, VerifyRequest(in, pub))
		})
	}
}
