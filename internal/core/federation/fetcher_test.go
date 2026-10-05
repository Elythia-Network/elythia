package federation

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genTestRSAKey produces a parsed *PrivateKey backed by a small RSA key.
// Key size is intentionally small (1024-bit) to keep tests fast — never
// use this helper in production paths.
func genTestRSAKey(t *testing.T) *activitypub.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	der := x509.MarshalPKCS1PrivateKey(key)
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
	pk, err := activitypub.NewPrivateKey("https://local.example/users/sys#main-key", pemStr)
	require.NoError(t, err)
	return pk
}

// stubSigner returns a canned PrivateKey. err != nil simulates the
// "instance.actor not yet provisioned" branch (= ErrNoSigner)。
type stubSigner struct {
	key *activitypub.PrivateKey
	err error
}

func (s *stubSigner) Signer() (*activitypub.PrivateKey, error) {
	return s.key, s.err
}

func TestAPFetcher_FetchObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	c := activitypub.NewClient(nil, "test")
	f := NewAPFetcher(c)
	body, err := f.FetchObject(srv.URL + "/users/alice")
	require.NoError(t, err)
	assert.Contains(t, string(body), "x")
}

// signer 未配線時は従来通り unsigned GET だけで動作する (#419 互換性)。
func TestAPFetcher_FetchObject_NoSigner_UnsignedOnly(t *testing.T) {
	var sawSig bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Signature") != "" {
			sawSig = true
		}
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	_, err := f.FetchObject(srv.URL + "/users/alice")
	require.NoError(t, err)
	assert.False(t, sawSig, "unsigned mode must not set Signature header")
}

// signer 配線済 + peer が signed GET を受け入れる場合は signed リクエストが
// 通り、unsigned へのフォールバックは発生しない (#419)。
func TestAPFetcher_FetchObject_SignedDefault_PeerAcceptsSigned(t *testing.T) {
	var calls int
	var firstHadSig bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 && r.Header.Get("Signature") != "" {
			firstHadSig = true
		}
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"id":"signed"}`))
	}))
	defer srv.Close()

	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	f.SetSigner(&stubSigner{key: genTestRSAKey(t)})
	body, err := f.FetchObject(srv.URL + "/users/alice")
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "should not fall back when signed succeeded")
	assert.True(t, firstHadSig, "signed GET must include Signature header")
	assert.Contains(t, string(body), "signed")
}

// IceShrimp.NET 等の authorized-fetch peer (signed のみ受理) でも、署名
// 鍵が wire されていれば 1 発で通る。これが #419 の core fix。
func TestAPFetcher_FetchObject_SignedDefault_AuthorizedFetchPeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Signature") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing signature"}`))
			return
		}
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer srv.Close()

	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	f.SetSigner(&stubSigner{key: genTestRSAKey(t)})
	body, err := f.FetchObject(srv.URL + "/users/alice")
	require.NoError(t, err)
	assert.Contains(t, string(body), "ok")
}

// signed GET が 401/403 を返した時は unsigned GET にフォールバックする
// (signature 検証が緩い peer での互換性確保, #419)。
func TestAPFetcher_FetchObject_FallsBackToUnsignedOnAuthError(t *testing.T) {
	var unsignedHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Signature") != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"bad sig"}`))
			return
		}
		unsignedHits++
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"id":"unsigned-ok"}`))
	}))
	defer srv.Close()

	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	f.SetSigner(&stubSigner{key: genTestRSAKey(t)})
	body, err := f.FetchObject(srv.URL + "/users/alice")
	require.NoError(t, err)
	assert.Equal(t, 1, unsignedHits, "expect single unsigned fallback")
	assert.Contains(t, string(body), "unsigned-ok")
}

// 404 / 5xx 等で unsigned リトライを飛ばすことで二重リクエストを抑制する
// (#419 Devin review)。signed の失敗をそのまま上位に返す。
func TestAPFetcher_FetchObject_DoesNotFallBackOnNotFound(t *testing.T) {
	var totalHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		totalHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	f.SetSigner(&stubSigner{key: genTestRSAKey(t)})
	_, err := f.FetchObject(srv.URL + "/users/alice")
	require.Error(t, err)
	assert.Equal(t, 1, totalHits, "404 must not trigger unsigned retry")
}

// SignerCredentials がエラー (instance.actor 未プロビジョン等) を返す場合は
// 即 unsigned へ落ちる。ErrNoSigner のセマンティクスを担保する。
func TestAPFetcher_FetchObject_SignerErrorSkipsToUnsigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"id":"y"}`))
	}))
	defer srv.Close()

	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	f.SetSigner(&stubSigner{err: ErrNoSigner})
	body, err := f.FetchObject(srv.URL + "/users/alice")
	require.NoError(t, err)
	assert.Contains(t, string(body), "y")
}

// FetchUnsigned / FetchJSON が non-2xx で StatusError を返すことを確認する。
// IceShrimp.NET の 401 を上位で識別するための土台 (#419)。
func TestActivityPubClient_FetchUnsigned_NonOkReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := activitypub.NewClient(nil, "test").FetchUnsigned(srv.URL + "/")
	require.Error(t, err)
	var se *activitypub.StatusError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, http.StatusUnauthorized, se.StatusCode)
}

// 互換性: 既存の fmt.Errorf("unexpected status: ...").Error() 文字列依存
// callsite が無いか軽くチェックする (regression 防止)。
func TestStatusError_PreservesLegacyErrorString(t *testing.T) {
	se := &activitypub.StatusError{StatusCode: 401, Status: "401 Unauthorized", URL: "x"}
	assert.True(t, strings.HasPrefix(se.Error(), "unexpected status:"))
}

// #739: APFetcher.FetchObjectUnsigned (peer 認証不要 endpoint 用) と
// FetchJSON (Iceshrimp.NET nodeinfo 互換) を coverage 化。
func TestAPFetcher_FetchObjectUnsigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"object":"x"}`))
	}))
	defer srv.Close()
	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	body, err := f.FetchObjectUnsigned(srv.URL)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"object":"x"`)
}

func TestAPFetcher_FetchJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// nodeinfo 経路は plain JSON Accept を要求する
		assert.Contains(t, r.Header.Get("Accept"), "application/json")
		_, _ = w.Write([]byte(`{"version":"2.1"}`))
	}))
	defer srv.Close()
	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	body, err := f.FetchJSON(srv.URL)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"version":"2.1"`)
}

func TestAPFetcher_FetchHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><link rel="icon" href="/a.png"></head></html>`))
	}))
	defer srv.Close()

	c := activitypub.NewClient(nil, "test")
	f := NewAPFetcher(c)
	body, err := f.FetchHTML(srv.URL)
	require.NoError(t, err)
	assert.Contains(t, string(body), "<link rel=\"icon\"")
}

// 疎通の診断 (#3055) 用の署名だけの取得は、401 / 403 でも署名なしへ落ちない。
// 落ちると「こちらの署名が拒否されている」を診断できない。
func TestAPFetcher_FetchObjectSignedOnly_NeverFallsBack(t *testing.T) {
	var unsigned, signed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Signature") == "" {
			unsigned++
			w.Header().Set("Content-Type", "application/activity+json")
			_, _ = w.Write([]byte(`{"id":"x"}`))
			return
		}
		signed++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	f.SetSigner(&stubSigner{key: genTestRSAKey(t)})
	_, err := f.FetchObjectSignedOnly(srv.URL + "/users/alice")
	var se *activitypub.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusUnauthorized, se.StatusCode)
	assert.Equal(t, 1, signed)
	assert.Zero(t, unsigned, "no unsigned retry")
}

func TestAPFetcher_FetchObjectSignedOnly_NoSigner(t *testing.T) {
	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	_, err := f.FetchObjectSignedOnly("https://remote.example/users/alice")
	assert.ErrorIs(t, err, ErrNoSigner)

	f.SetSigner(&stubSigner{err: ErrNoSigner})
	_, err = f.FetchObjectSignedOnly("https://remote.example/users/alice")
	assert.ErrorIs(t, err, ErrNoSigner)

	f.SetSigner(&stubSigner{})
	_, err = f.FetchObjectSignedOnly("https://remote.example/users/alice")
	assert.ErrorIs(t, err, ErrNoSigner, "a nil key is treated as no signer")
}
