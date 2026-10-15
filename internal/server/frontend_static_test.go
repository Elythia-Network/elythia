package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// newFrontendBuildEcho registers the production /vite route behind the
// production global gzip middleware, over a build directory holding:
//
//	ja-JP/bSIsCqU1u-0qk-1YEG.js (+ .br / .gz)   hashed, precompressed
//	assets/bSIsCqU1u-14CNCjXV.css               hashed, no precompressed
//	manifest.json (+ .br)                       not hashed
//	loader/boot.js                              not hashed
func newFrontendBuildEcho(t *testing.T) *echo.Echo {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	big := strings.Repeat("console.log('plain');", 100)
	write("ja-JP/bSIsCqU1u-0qk-1YEG.js", big)
	write("ja-JP/bSIsCqU1u-0qk-1YEG.js.br", "BROTLI-BYTES")
	write("ja-JP/bSIsCqU1u-0qk-1YEG.js.gz", "GZIP-BYTES")
	write("assets/bSIsCqU1u-14CNCjXV.css", strings.Repeat(".a{color:red}", 200))
	write("manifest.json", `{"src/_boot_.ts":{}}`)
	write("manifest.json.br", "BROTLI-MANIFEST")
	write("loader/boot.js", strings.Repeat("boot();", 300))

	e := echo.New()
	e.Use(echomw.GzipWithConfig(gzipConfig()))
	registerFrontendAssets(e, &config.Config{}, "/vite", dir, "http://127.0.0.1:1")
	return e
}

func getFrontend(e *echo.Echo, path, acceptEncoding string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptEncoding != "" {
		req.Header.Set(echo.HeaderAcceptEncoding, acceptEncoding)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestFrontendBuild_PrecompressedVariants(t *testing.T) {
	e := newFrontendBuildEcho(t)
	tests := []struct {
		name, accept, wantEncoding, wantBody string
	}{
		{name: "brotli preferred", accept: "gzip, deflate, br, zstd", wantEncoding: "br", wantBody: "BROTLI-BYTES"},
		{name: "gzip when brotli is not accepted", accept: "gzip, deflate", wantEncoding: "gzip", wantBody: "GZIP-BYTES"},
		{name: "gzip when brotli is refused", accept: "br;q=0, gzip", wantEncoding: "gzip", wantBody: "GZIP-BYTES"},
		{name: "identity without Accept-Encoding", accept: "", wantEncoding: "", wantBody: strings.Repeat("console.log('plain');", 100)},
		{name: "higher q wins over the default order", accept: "br;q=0.5, gzip", wantEncoding: "gzip", wantBody: "GZIP-BYTES"},
		{name: "wildcard accepts brotli", accept: "*", wantEncoding: "br", wantBody: "BROTLI-BYTES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := getFrontend(e, "/vite/ja-JP/bSIsCqU1u-0qk-1YEG.js", tt.accept)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tt.wantEncoding, rec.Header().Get(echo.HeaderContentEncoding))
			// 事前圧縮の本文をそのまま返す (gzip の middleware で包み直さない)。
			assert.Equal(t, tt.wantBody, rec.Body.String())
			assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get(echo.HeaderContentType))
			assert.Equal(t, hashedAssetCacheControl, rec.Header().Get(echo.HeaderCacheControl))
			if tt.wantEncoding != "" {
				assert.Equal(t, []string{echo.HeaderAcceptEncoding}, rec.Header().Values(echo.HeaderVary))
				assert.Equal(t, strconv.Itoa(len(tt.wantBody)), rec.Header().Get(echo.HeaderContentLength))
			}
		})
	}
}

// 事前圧縮のファイルが無ければ、これまでどおりその場で gzip する。
func TestFrontendBuild_DynamicGzipFallback(t *testing.T) {
	e := newFrontendBuildEcho(t)
	rec := getFrontend(e, "/vite/assets/bSIsCqU1u-14CNCjXV.css", "gzip, br")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "gzip", rec.Header().Get(echo.HeaderContentEncoding))
	assert.Equal(t, hashedAssetCacheControl, rec.Header().Get(echo.HeaderCacheControl))
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	plain, err := io.ReadAll(zr)
	require.NoError(t, err)
	// 1 回だけ圧縮されている (二重に包まれていれば、ここで gzip の本文が出る)。
	assert.Equal(t, strings.Repeat(".a{color:red}", 200), string(plain))
}

func TestFrontendBuild_UnhashedFilesAreNotImmutable(t *testing.T) {
	e := newFrontendBuildEcho(t)
	for _, path := range []string{"/vite/manifest.json", "/vite/loader/boot.js"} {
		rec := getFrontend(e, path, "gzip, br")
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Empty(t, rec.Header().Get(echo.HeaderCacheControl), path)
	}
	// hash の無いファイルは、.br があっても使わない (古いまま残りうる、#3593)。
	rec := getFrontend(e, "/vite/manifest.json", "br")
	assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding))
	assert.JSONEq(t, `{"src/_boot_.ts":{}}`, rec.Body.String())
}

// 変種を選ばなかった応答にも Vary: Accept-Encoding を 1 つだけ付ける。
func TestFrontendBuild_HashedFilesAlwaysVary(t *testing.T) {
	e := newFrontendBuildEcho(t)
	for _, tc := range []struct{ path, accept string }{
		{"/vite/ja-JP/bSIsCqU1u-0qk-1YEG.js", ""},
		{"/vite/ja-JP/bSIsCqU1u-0qk-1YEG.js", "br;q=0, gzip;q=0"},
		{"/vite/assets/bSIsCqU1u-14CNCjXV.css", "gzip"},
		{"/vite/assets/bSIsCqU1u-14CNCjXV.css", ""},
	} {
		rec := getFrontend(e, tc.path, tc.accept)
		require.Equal(t, http.StatusOK, rec.Code, tc)
		assert.Equal(t, []string{echo.HeaderAcceptEncoding}, rec.Header().Values(echo.HeaderVary), tc)
	}
}

// 前提条件で 412 になった応答には、本文が無いので Content-Length を付けない。
func TestFrontendBuild_PreconditionFailedHasNoContentLength(t *testing.T) {
	e := newFrontendBuildEcho(t)
	req := httptest.NewRequest(http.MethodGet, "/vite/ja-JP/bSIsCqU1u-0qk-1YEG.js", nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "br")
	req.Header.Set("If-Unmodified-Since", "Mon, 01 Jan 2001 00:00:00 GMT")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
	assert.Empty(t, rec.Header().Get(echo.HeaderContentLength))
	assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding))
	assert.Empty(t, rec.Body.String())
}

func TestFrontendBuild_MissingAndOddPaths(t *testing.T) {
	e := newFrontendBuildEcho(t)
	for _, path := range []string{
		"/vite/ja-JP/bSIsCqU1u-zzzzzzzz.js",
		"/vite/../devmode.go",
		"/vite/ja-JP%2fbSIsCqU1u-0qk-1YEG.js",
	} {
		rec := getFrontend(e, path, "gzip, br")
		assert.NotEqual(t, http.StatusOK, rec.Code, path)
		assert.Empty(t, rec.Header().Get(echo.HeaderCacheControl), path)
	}
	// ディレクトリは echo の静的配信と同じく、末尾の / へ寄せる。
	rec := getFrontend(e, "/vite/ja-JP", "")
	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
}

func TestFrontendBuild_ConditionalRequest(t *testing.T) {
	e := newFrontendBuildEcho(t)
	first := getFrontend(e, "/vite/ja-JP/bSIsCqU1u-0qk-1YEG.js", "br")
	lm := first.Header().Get(echo.HeaderLastModified)
	require.NotEmpty(t, lm)
	req := httptest.NewRequest(http.MethodGet, "/vite/ja-JP/bSIsCqU1u-0qk-1YEG.js", nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "br")
	req.Header.Set(echo.HeaderIfModifiedSince, lm)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Equal(t, hashedAssetCacheControl, rec.Header().Get(echo.HeaderCacheControl), "304 も同じ指示")
	assert.Empty(t, rec.Header().Get(echo.HeaderContentLength))
}

// ファイルはあっても応答がエラーになったら immutable を付けない。
func TestFrontendBuild_ErrorResponseIsNotImmutable(t *testing.T) {
	e := newFrontendBuildEcho(t)
	for _, accept := range []string{"br", ""} {
		req := httptest.NewRequest(http.MethodGet, "/vite/ja-JP/bSIsCqU1u-0qk-1YEG.js", nil)
		req.Header.Set(echo.HeaderAcceptEncoding, accept)
		req.Header.Set("Range", "bytes=999999-")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code, accept)
		assert.Empty(t, rec.Header().Get(echo.HeaderCacheControl), accept)
	}
	// Range が満たせるなら 206 で、immutable を付ける。Content-Length は部分の長さ。
	req := httptest.NewRequest(http.MethodGet, "/vite/ja-JP/bSIsCqU1u-0qk-1YEG.js", nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "br")
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "BROT", rec.Body.String())
	assert.Equal(t, "4", rec.Header().Get(echo.HeaderContentLength))
	assert.Equal(t, hashedAssetCacheControl, rec.Header().Get(echo.HeaderCacheControl))
}

func TestSelfEncodedRoutesSkipGlobalGzip(t *testing.T) {
	skip := gzipConfig().Skipper
	e := echo.New()
	for path, want := range map[string]bool{
		"/vite/*":       true,
		"/embed_vite/*": true,
		"/api/emojis":   true,
		"/api/emoji":    false,
		"/api/notes":    false,
	} {
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		c.SetPath(path)
		assert.Equal(t, want, skip(c), path)
	}
}

// 事前圧縮を作る側 (frontend/scripts/precompress.mjs) と、使う側の hash の判定は
// 同じ規則にする。ずれると、作られない変種を探すか、使われない変種を作る。
func TestPrecompressScriptUsesTheSameHashRule(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "frontend", "scripts", "precompress.mjs"))
	require.NoError(t, err)
	assert.Contains(t, string(src), "export const hashedNameRe = /"+hashedAssetNameRe.String()+"/;")
}
