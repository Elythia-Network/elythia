package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServeStaticAssetDir_SetsCacheControlHeader verifies that
// serveStaticAssetDir attaches the long-lived Cache-Control header used
// by /twemoji and /fluent-emoji routes. Regression guard for the upstream
// Misskey TS `ms('30 days')` cache policy.
func TestServeStaticAssetDir_SetsCacheControlHeader(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1f4dd.png"), []byte("fake-png"), 0o644))

	e := echo.New()
	serveStaticAssetDir(e, "/fluent-emoji", dir)

	req := httptest.NewRequest(http.MethodGet, "/fluent-emoji/1f4dd.png", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=2592000, immutable", rec.Header().Get("Cache-Control"),
		"long-lived static asset route must set 30-day immutable Cache-Control")
	assert.Equal(t, "fake-png", rec.Body.String())
}

// TestServeStaticAssetDir_NonExistentFile returns 404 without panic when
// the requested asset does not exist (Echo's c.File contract).
func TestServeStaticAssetDir_NonExistentFile(t *testing.T) {
	dir := t.TempDir()
	e := echo.New()
	serveStaticAssetDir(e, "/fluent-emoji", dir)

	req := httptest.NewRequest(http.MethodGet, "/fluent-emoji/missing.png", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestServeStaticAssetDir_TwemojiPrefix verifies the same helper works
// for the /twemoji route prefix, not just /fluent-emoji (both routes
// share the helper but a regression on prefix routing would only show
// up when both prefixes are exercised).
func TestServeStaticAssetDir_TwemojiPrefix(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1f004.svg"), []byte("<svg/>"), 0o644))

	e := echo.New()
	serveStaticAssetDir(e, "/twemoji", dir)

	req := httptest.NewRequest(http.MethodGet, "/twemoji/1f004.svg", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=2592000, immutable", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "<svg/>", rec.Body.String())
}

// TestServeStaticAssetDir_MalformedEscapeReturns400 verifies that an
// invalid URL escape (e.g. truncated `%`) returns 400 Bad Request rather
// than 500. Echo.Static's bare error path becomes 500; this helper
// intentionally diverges to give proper client-error semantics.
//
// 注: `httptest.NewRequest` は内部で `url.Parse` を呼ぶため `%ZZ` を
// 含む URL を渡すと panic する。`net/http` 経路でも request line parse
// で reject されるはずだが、不正 request が handler に到達した場合の
// defense-in-depth として error mapping を保証する。test は
// URL.RawPath を直接組み立てて Echo router の wildcard match 経路を
// bypass せずに verify する。
func TestServeStaticAssetDir_MalformedEscapeReturns400(t *testing.T) {
	dir := t.TempDir()
	e := echo.New()
	serveStaticAssetDir(e, "/fluent-emoji", dir)

	// URL を手動構築: Path は decoded、RawPath に malformed escape を残す。
	// Echo router は wildcard `*` match で raw segment を取るため、handler
	// 側の PathUnescape が呼ばれ error を返す経路に乗る。
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/fluent-emoji/x.png", RawPath: "/fluent-emoji/%ZZ.png"},
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"malformed URL escape must return 400, not 500")
}

// TestServeStaticAssetDir_PathTraversalBlocked verifies the filepath.Clean
// guard rejects "../" traversal attempts (matches Echo's own Static safety).
func TestServeStaticAssetDir_PathTraversalBlocked(t *testing.T) {
	dir := t.TempDir()
	// 親ディレクトリにファイルを置いて、`../parent.txt` で抜けられたら検知。
	parent := filepath.Dir(dir)
	require.NoError(t, os.WriteFile(filepath.Join(parent, "parent.txt"), []byte("secret"), 0o644))
	defer os.Remove(filepath.Join(parent, "parent.txt"))

	e := echo.New()
	serveStaticAssetDir(e, "/fluent-emoji", dir)

	req := httptest.NewRequest(http.MethodGet, "/fluent-emoji/..%2Fparent.txt", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// filepath.Clean("/" + "../parent.txt") = "/parent.txt" → join with dir
	// → dir/parent.txt (= 存在しない) → 404。secret 内容は絶対に返らない。
	assert.NotEqual(t, http.StatusOK, rec.Code, "path traversal must not succeed")
	assert.NotContains(t, rec.Body.String(), "secret")
}

// upstream (`ClientServerService.ts` の /twemoji /fluent-emoji) が付けている CSP を
// 揃える (#2404)。SVG が単体で開かれても何も実行できない状態にする。
func TestServeStaticAssetDir_SetsAssetCSP(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1f600.svg"), []byte("<svg/>"), 0o644))

	e := echo.New()
	serveStaticAssetDir(e, "/twemoji", dir)

	req := httptest.NewRequest(http.MethodGet, "/twemoji/1f600.svg", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "default-src 'none'; style-src 'unsafe-inline'",
		rec.Header().Get("Content-Security-Policy"),
		"値は upstream の文字列と完全一致させる (独自に強めない)")
}
