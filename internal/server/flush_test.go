package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serveFlush(t *testing.T, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/flush", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	require.NoError(t, flushPage("https://example.test")(echo.New().NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec
}

// /flush は本家と同じく、Clear-Site-Data と、それが効かなかったときに flush.js が
// 見る Cookie と、flush.js を読むページを返す (#4204)。
func TestFlushPage(t *testing.T) {
	rec := serveFlush(t, "")
	assert.Equal(t, `"*"`, rec.Header().Get("Clear-Site-Data"))
	assert.Equal(t, "http-flush-failed=1; Path=/flush; Max-Age=60", rec.Header().Get("Set-Cookie"))
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html"))
	assert.Contains(t, rec.Body.String(), `<script src="/static-assets/misc/flush.js"></script>`)
	assert.Contains(t, rec.Body.String(), `<div id="msg"></div>`)
}

func TestFlushPage_ClearSiteDataOnlyForOwnHTTPSOrigin(t *testing.T) {
	for origin, want := range map[string]bool{
		"https://example.test":      true,
		"https://Example.TEST:443":  true,
		"http://example.test":       false,
		"https://other.example":     false,
		"https://example.test:8443": false,
		"://broken":                 false,
	} {
		rec := serveFlush(t, origin)
		assert.Equal(t, want, rec.Header().Get("Clear-Site-Data") != "", "origin %q", origin)
		// Cookie は Clear-Site-Data を付けないときも付ける (flush.js が自分で消す合図)。
		assert.NotEmpty(t, rec.Header().Get("Set-Cookie"), "origin %q", origin)
	}
}

// Clear-Site-Data is sent only for a top-level navigation to /flush, not when
// the page is loaded as a subresource, even from the same origin. A browser
// that sends no Sec-Fetch-* headers still gets it.
func TestFlushPage_NoClearSiteDataForSubresource(t *testing.T) {
	cases := []struct {
		dest, mode string
		want       bool
	}{
		{"document", "navigate", true},
		{"", "", true},
		{"document", "", true},
		{"", "navigate", true},
		{"image", "no-cors", false},
		{"empty", "cors", false},
		{"iframe", "navigate", false},
		{"document", "same-origin", false},
		{"document", "no-cors", false},
		{"document", "cors", false},
		{"empty", "same-origin", false},
		{"image", "", false},
		{"", "cors", false},
		{"", "same-origin", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/flush", nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if tc.dest != "" {
			req.Header.Set("Sec-Fetch-Dest", tc.dest)
		}
		if tc.mode != "" {
			req.Header.Set("Sec-Fetch-Mode", tc.mode)
		}
		rec := httptest.NewRecorder()
		require.NoError(t, flushPage("https://example.test")(echo.New().NewContext(req, rec)))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, tc.want, rec.Header().Get("Clear-Site-Data") != "", "dest %q mode %q", tc.dest, tc.mode)
		// Cookie は付けない場合も付ける (flush.js が自分で消す合図。本家と同じ)。
		assert.NotEmpty(t, rec.Header().Get("Set-Cookie"), "dest %q mode %q", tc.dest, tc.mode)
	}
}

// 登録されていないと、/flush は SPA catchall の通常の画面になって何も消えない
// (起動の失敗の画面 (public/loader/boot.js) のリンクも効かない)。
func TestFlushPage_RegisteredOnBuiltServer(t *testing.T) {
	routes, _ := builtEchoRoutes(t)
	for _, r := range routes {
		if r.Method == http.MethodGet && r.Path == "/flush" {
			assert.Contains(t, r.Name, "flushPage")
			return
		}
	}
	t.Fatal("GET /flush is not registered")
}

// When another site sent the browser to /flush (a link, a script), nothing is
// cleared: the page asks first, and its link (a same-origin navigation) clears.
func TestFlushPage_AsksWhenAnotherSiteSentTheBrowser(t *testing.T) {
	for value, asks := range map[string]bool{
		"cross-site":  true,
		"same-site":   true,
		"same-origin": false,
		"none":        false,
		"":            false,
	} {
		req := httptest.NewRequest(http.MethodGet, "/flush", nil)
		if value != "" {
			req.Header.Set("Sec-Fetch-Site", value)
		}
		rec := httptest.NewRecorder()
		require.NoError(t, flushPage("https://example.test")(echo.New().NewContext(req, rec)))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"), value)
		if asks {
			assert.Empty(t, rec.Header().Get("Clear-Site-Data"), value)
			assert.Empty(t, rec.Header().Get("Set-Cookie"), value)
			assert.NotContains(t, rec.Body.String(), "flush.js", value)
			assert.Contains(t, rec.Body.String(), `<a href="/flush">`, value)
		} else {
			assert.Equal(t, `"*"`, rec.Header().Get("Clear-Site-Data"), value)
			assert.Contains(t, rec.Body.String(), "flush.js", value)
		}
	}
}
