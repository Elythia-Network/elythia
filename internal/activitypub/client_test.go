package activitypub

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_PostSigned(t *testing.T) {
	key, pub := newTestKey(t)

	var seenSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSig = r.Header.Get("Signature")
		r.Header.Set("Host", r.Host)
		if err := VerifyRequest(r, pub); err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(nil, "mk-go-test")
	resp, err := c.PostSigned(srv.URL+"/inbox", []byte(`{"type":"Create"}`), key)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, seenSig)
}

func TestClient_PostSigned_BadURL(t *testing.T) {
	key, _ := newTestKey(t)
	c := NewClient(nil, "")
	_, err := c.PostSigned("://bad", []byte("x"), key)
	assert.Error(t, err)
}

func TestClient_PostSigned_SignError(t *testing.T) {
	c := NewClient(nil, "")
	_, err := c.PostSigned("https://x.example/", []byte("x"), nil)
	assert.Error(t, err)
}

func TestClient_GetSigned(t *testing.T) {
	key, pub := newTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Host", r.Host)
		if err := VerifyRequest(r, pub); err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"id":"https://example.com/users/u1"}`))
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	resp, err := c.GetSigned(srv.URL+"/users/u1", key, "")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "u1")
}

func TestClient_GetSigned_WithUserAgent(t *testing.T) {
	key, _ := newTestKey(t)
	var seenUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(nil, "mk-go-test/1.0")
	resp, err := c.GetSigned(srv.URL+"/", key, "")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, "mk-go-test/1.0", seenUA)
}

func TestClient_GetSigned_AcceptOverride(t *testing.T) {
	key, _ := newTestKey(t)
	var seenAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	resp, err := c.GetSigned(srv.URL+"/", key, "application/json")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, "application/json", seenAccept)
}

func TestClient_GetSigned_BadURL(t *testing.T) {
	key, _ := newTestKey(t)
	c := NewClient(nil, "")
	_, err := c.GetSigned("://bad", key, "")
	assert.Error(t, err)
}

func TestClient_GetSigned_SignError(t *testing.T) {
	c := NewClient(nil, "")
	_, err := c.GetSigned("https://x.example/", nil, "")
	assert.Error(t, err)
}

func TestClient_FetchJSON(t *testing.T) {
	key, _ := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	got, err := c.FetchJSON(srv.URL+"/x", key)
	require.NoError(t, err)
	assert.Contains(t, string(got), "ok")
}

func TestClient_FetchJSON_NonOK(t *testing.T) {
	key, _ := newTestKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchJSON(srv.URL, key)
	assert.Error(t, err)
}

func TestClient_FetchJSON_ResponseTooLarge(t *testing.T) {
	// #323: MaxBodyBytes を超えたレスポンスは ErrResponseTooLarge でエラーになる。
	key, _ := newTestKey(t)
	oversized := make([]byte, int(MaxBodyBytes)+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchJSON(srv.URL, key)
	require.Error(t, err)
	assert.True(t, errors.Is(err, safehttp.ErrResponseTooLarge))
}

func TestClient_FetchUnsigned_ResponseTooLarge(t *testing.T) {
	oversized := make([]byte, int(MaxBodyBytes)+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchUnsigned(srv.URL)
	require.Error(t, err)
	assert.True(t, errors.Is(err, safehttp.ErrResponseTooLarge))
}

func TestClient_FetchJSON_Error(t *testing.T) {
	key, _ := newTestKey(t)
	c := NewClient(nil, "")
	_, err := c.FetchJSON("://bad", key)
	assert.Error(t, err)
}

func TestClient_FetchHTML(t *testing.T) {
	const body = `<html><head><link rel="icon" href="/x.png"></head></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.Header.Get("Accept"), "text/html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(nil, "mk-test/1.0")
	got, err := c.FetchHTML(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
}

func TestClient_FetchHTML_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchHTML(srv.URL)
	assert.Error(t, err)
}

func TestClient_FetchHTML_BadURL(t *testing.T) {
	c := NewClient(nil, "")
	_, err := c.FetchHTML("://bad")
	assert.Error(t, err)
}

func TestClient_FetchHTML_NonHTMLContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchHTML(srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected content-type")
}

func TestClient_FetchHTML_ResponseTooLarge(t *testing.T) {
	oversized := make([]byte, int(MaxHTMLBodyBytes)+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// body sniffingで application/octet-stream に推定されてContent-Type
		// checkに引っかからないよう明示的にtext/htmlをセットする。
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchHTML(srv.URL)
	require.Error(t, err)
	assert.True(t, errors.Is(err, safehttp.ErrResponseTooLarge))
}

func TestNewClient_DefaultHTTPClient(t *testing.T) {
	c := NewClient(nil, "")
	assert.NotNil(t, c)
}

func TestClient_FetchUnsigned_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write([]byte(`{"id":"https://example.com/x"}`))
	}))
	defer srv.Close()

	c := NewClient(nil, "test-ua")
	body, err := c.FetchUnsigned(srv.URL + "/x")
	require.NoError(t, err)
	assert.Contains(t, string(body), "https://example.com/x")
}

func TestClient_FetchUnsigned_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchUnsigned(srv.URL)
	assert.Error(t, err)
}

func TestClient_FetchUnsigned_BadURL(t *testing.T) {
	c := NewClient(nil, "")
	_, err := c.FetchUnsigned("://bad")
	assert.Error(t, err)
}

// TestClient_FetchUnsignedJSON_AcceptHeader pins the Accept header used
// for non-AP discovery endpoints (#474). Iceshrimp.NET returns a 406
// envelope when the AP MIME types are sent, so plain JSON Accept must
// be preserved.
func TestClient_FetchUnsignedJSON_AcceptHeader(t *testing.T) {
	var seenAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"links":[]}`))
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchUnsignedJSON(srv.URL + "/.well-known/nodeinfo")
	require.NoError(t, err)
	assert.Equal(t, "application/json, */*", seenAccept,
		"plain JSON Accept must be sent so strict implementations like Iceshrimp.NET do not 406")
}

func TestClient_FetchUnsignedJSON_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(nil, "")
	_, err := c.FetchUnsignedJSON(srv.URL)
	assert.Error(t, err)
}

func TestClient_FetchUnsigned_NetworkError(t *testing.T) {
	c := NewClient(nil, "")
	_, err := c.FetchUnsigned("http://127.0.0.1:1/")
	assert.Error(t, err)
}

// TestValidateAPContentType pins upstream validateContentTypeSetAsActivityPub
// parity (#1828): only application/activity+json (any params) or
// application/ld+json carrying the ActivityStreams profile is accepted; a
// missing or non-AP Content-Type is rejected with ErrInvalidAPContentType.
func TestValidateAPContentType(t *testing.T) {
	cases := []struct {
		name        string
		contentType string // empty = do not set the header at all
		ok          bool
	}{
		{"missing content-type", "", false},
		{"activity+json", "application/activity+json", true},
		{"activity+json with charset", "application/activity+json; charset=utf-8", true},
		{"activity+json uppercase", "APPLICATION/ACTIVITY+JSON", true},
		{"ld+json with AS profile", `application/ld+json; profile="https://www.w3.org/ns/activitystreams"`, true},
		{"ld+json without semicolon", "application/ld+json", false},
		{"ld+json without AS profile", "application/ld+json; charset=utf-8", false},
		{"plain json", "application/json", false},
		{"text/html error page", "text/html; charset=utf-8", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				} else {
					// Go の http server は body sniffing で Content-Type を
					// 自動付与するため、未設定ケースは空文字を明示的に上書きする。
					w.Header()["Content-Type"] = nil
				}
				_, _ = w.Write([]byte(`{"@context":"https://www.w3.org/ns/activitystreams","id":"https://x/1"}`))
			}))
			defer srv.Close()

			_, err := NewClient(nil, "test").FetchUnsigned(srv.URL)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrInvalidAPContentType)
			}
		})
	}
}

// **redirect は追従し、飛び先を呼び出し側へ返す。** 止めるかどうかは呼び出し側の
// 判断 (nodeinfo は最終 URL が要求した host と違えば本文を読み戻さない)。
// `CheckRedirect` で止める形は `allowExternalApRedirect` の設定を上書きして
// しまうので採らない (3 周目レビュー M1)。
func TestFetchUnsignedJSONWithURL_FollowsCrossHostRedirect(t *testing.T) {
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer victim.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, victim.URL+"/ni", http.StatusFound)
	}))
	defer origin.Close()

	c := NewClient(origin.Client(), "test")
	body, finalURL, err := c.FetchUnsignedJSONWithURL(origin.URL + "/.well-known/nodeinfo")
	require.NoError(t, err, "`.well-known` の委譲を落としている")
	require.JSONEq(t, `{"ok":true}`, string(body))
	require.Contains(t, finalURL, victim.URL, "飛び先を呼び出し側へ返していない")
}
