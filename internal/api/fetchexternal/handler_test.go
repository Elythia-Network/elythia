package fetchexternal

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/testutil"
)

func hashOf(data string) string {
	sum := sha512.Sum512([]byte(strings.ReplaceAll(data, "\r\n", "\n")))
	return hex.EncodeToString(sum[:])
}

func do(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	_ = h.Fetch(e.NewContext(req, rec))
	return rec
}

// resourceServer serves a fixed {type, data} JSON.
func resourceServer(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch_HashMatch(t *testing.T) {
	data := "theme-content-string"
	srv := resourceServer(t, `{"type":"theme","data":"`+data+`"}`)
	h := New(srv.Client(), "mk-test")

	rec := do(t, h, `{"url":"`+srv.URL+`","hash":"`+hashOf(data)+`"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "theme", out["type"])
	assert.Equal(t, data, out["data"])
}

func TestFetch_HashMismatch(t *testing.T) {
	srv := resourceServer(t, `{"type":"theme","data":"actual"}`)
	h := New(srv.Client(), "mk-test")
	rec := do(t, h, `{"url":"`+srv.URL+`","hash":"`+hashOf("expected-different")+`"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXT_RESOURCE_HASH_DIDNT_MATCH")
}

func TestFetch_InvalidSchema(t *testing.T) {
	// data が string でない → invalid schema。
	srv := resourceServer(t, `{"type":"theme","data":{"nested":true}}`)
	h := New(srv.Client(), "mk-test")
	rec := do(t, h, `{"url":"`+srv.URL+`","hash":"x"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXT_RESOURCE_RETURNED_INVALID_SCHEMA")
}

func TestFetch_Non2xxIsInvalidSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	h := New(srv.Client(), "mk-test")
	rec := do(t, h, `{"url":"`+srv.URL+`","hash":"x"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXT_RESOURCE_RETURNED_INVALID_SCHEMA")
}

func TestFetch_ConnectionFailureIsInvalidSchema(t *testing.T) {
	// 到達不能なサーバへの fetch 失敗も invalidSchema に落ちる。
	url := "http://" + testutil.DeadAddr(t)
	h := New(http.DefaultClient, "mk-test")
	rec := do(t, h, `{"url":"`+url+`","hash":"x"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXT_RESOURCE_RETURNED_INVALID_SCHEMA")
}

// noNetworkClient fails the test when the handler sends a request. The
// inputs it is used with must be rejected before any fetch.
//
// 検査を外す変異で、本物の外部へ取得しに行かないようにする (#3845)。
func noNetworkClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected outbound request to %s", r.URL)
		return nil, errors.New("no network in this test")
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetch_MissingParams(t *testing.T) {
	h := New(noNetworkClient(t), "mk-test")
	for _, body := range []string{`{}`, `{"url":"https://e.com"}`, `{"hash":"x"}`} {
		rec := do(t, h, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	}
}

func TestFetch_NonHTTPScheme(t *testing.T) {
	h := New(noNetworkClient(t), "mk-test")
	rec := do(t, h, `{"url":"file:///etc/passwd","hash":"x"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

func TestFetch_CRLFNormalizedHash(t *testing.T) {
	// data に CRLF を含む場合、hash は LF 正規化後で計算される。
	raw := "line1\r\nline2"
	srv := resourceServer(t, `{"type":"t","data":`+mustJSON(raw)+`}`)
	h := New(srv.Client(), "mk-test")
	rec := do(t, h, `{"url":"`+srv.URL+`","hash":"`+hashOf(raw)+`"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// 本家の paramDef は url・hash を required の string とするだけなので、空の hash も
// 通し、取得した本文のハッシュと一致しないので EXT_RESOURCE_HASH_DIDNT_MATCH になる。
// 空の url と相対の url は取得の前に 400 にする (#3845)。
func TestFetch_EmptyStringsLikeUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"theme","data":"x"}`))
	}))
	t.Cleanup(srv.Close)
	rec := do(t, New(srv.Client(), "mk-test"), `{"url":"`+srv.URL+`","hash":""}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXT_RESOURCE_HASH_DIDNT_MATCH")

	h := New(noNetworkClient(t), "mk-test")
	for _, body := range []string{`{"url":"","hash":"x"}`, `{"url":"hello","hash":""}`, `{"url":null,"hash":"x"}`} {
		rec := do(t, h, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM", body)
	}
}
