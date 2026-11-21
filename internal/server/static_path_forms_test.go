package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/frontendutil"
)

// staticPathFormsEcho registers each static route the way the server does,
// over a directory with x.js, sub/y.js and a content-hashed file with its
// brotli variant.
func staticPathFormsEcho(t *testing.T) *echo.Echo {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"x.js":               "x",
		"sub/y.js":           "y",
		"app-ABCDEFGH.js":    "app",
		"app-ABCDEFGH.js.br": "br",
	} {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	e := echo.New()
	e.GET("/vite/*", serveFrontendBuild(dir))
	e.GET("/embed_vite/*", serveFrontendBuild(dir))
	serveStaticAssetDir(e, "/twemoji", dir)
	serveStaticAssetDir(e, "/fluent-emoji", dir)
	e.GET("/assets/*", frontendutil.AssetsHandler(dir, t.TempDir()))
	e.Static("/client-assets", dir)
	e.Static("/static-assets", dir)
	return e
}

// TestStaticRoutes_PathForms pins how each static route answers paths with an
// empty, "." or ".." segment, or an encoded "/", after the move to echo v5
// (#4241). docs/divergence/api.md の「エンコードした`/`を含む静的ファイルの
// URL」の表は、このテストの値から書いている。
func TestStaticRoutes_PathForms(t *testing.T) {
	e := staticPathFormsEcho(t)
	for _, tc := range []struct {
		target string
		br     bool
		want   int
		// precompressed は、事前圧縮の変種 (本文 "br") が返ったことを確かめる。
		precompressed bool
	}{
		// frontend のビルド (/vite)。echo の StaticDirectoryHandler に任せる経路。
		{"/vite/x.js", false, http.StatusOK, false},
		{"/vite//x.js", false, http.StatusNotFound, false},
		{"/vite/%2Fx.js", false, http.StatusNotFound, false},
		{"/vite/./x.js", false, http.StatusNotFound, false},
		{"/vite/sub/../x.js", false, http.StatusNotFound, false},
		// 事前圧縮の変種を返す経路 (hash の付いた名前を br で求める)。
		{"/vite/app-ABCDEFGH.js", true, http.StatusOK, true},
		{"/vite//app-ABCDEFGH.js", true, http.StatusOK, true},
		{"/vite/%2Fapp-ABCDEFGH.js", true, http.StatusNotFound, false},
		{"/vite/./app-ABCDEFGH.js", true, http.StatusNotFound, false},
		{"/vite/sub/../app-ABCDEFGH.js", true, http.StatusNotFound, false},
		// 変種を求めないと、hash の付いた名前の // も echo の検査で 404 になる。
		{"/vite//app-ABCDEFGH.js", false, http.StatusNotFound, false},
		// /embed_vite も同じ handler。
		{"/embed_vite/x.js", false, http.StatusOK, false},
		{"/embed_vite//x.js", false, http.StatusNotFound, false},
		{"/embed_vite/%2Fx.js", false, http.StatusNotFound, false},
		{"/embed_vite/./x.js", false, http.StatusNotFound, false},
		{"/embed_vite/sub/../x.js", false, http.StatusNotFound, false},
		// /twemoji (serveStaticAssetDir) は自前でパスを解決する。
		{"/twemoji/x.js", false, http.StatusOK, false},
		{"/twemoji//x.js", false, http.StatusOK, false},
		{"/twemoji/%2Fx.js", false, http.StatusOK, false},
		{"/twemoji/./x.js", false, http.StatusOK, false},
		{"/twemoji/sub/../x.js", false, http.StatusOK, false},
		{"/fluent-emoji/x.js", false, http.StatusOK, false},
		{"/fluent-emoji//x.js", false, http.StatusOK, false},
		{"/fluent-emoji/./x.js", false, http.StatusOK, false},
		{"/fluent-emoji/%2Fx.js", false, http.StatusOK, false},
		{"/fluent-emoji/sub/../x.js", false, http.StatusOK, false},
		// /assets (frontendutil.AssetsHandler) も自前で解決するが、アンエスケープしない。
		{"/assets/x.js", false, http.StatusOK, false},
		{"/assets//x.js", false, http.StatusOK, false},
		{"/assets/%2Fx.js", false, http.StatusNotFound, false},
		{"/assets/./x.js", false, http.StatusOK, false},
		{"/assets/sub/../x.js", false, http.StatusOK, false},
		// e.Static (/client-assets)。既定ではアンエスケープしない。
		{"/client-assets/x.js", false, http.StatusOK, false},
		{"/client-assets//x.js", false, http.StatusNotFound, false},
		{"/client-assets/%2Fx.js", false, http.StatusNotFound, false},
		{"/client-assets/./x.js", false, http.StatusNotFound, false},
		{"/client-assets/sub/../x.js", false, http.StatusNotFound, false},
		{"/static-assets/x.js", false, http.StatusOK, false},
		{"/static-assets//x.js", false, http.StatusNotFound, false},
		{"/static-assets/./x.js", false, http.StatusNotFound, false},
		{"/static-assets/%2Fx.js", false, http.StatusNotFound, false},
		{"/static-assets/sub/../x.js", false, http.StatusNotFound, false},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.target, nil)
		if tc.br {
			req.Header.Set(echo.HeaderAcceptEncoding, "br")
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		assert.Equal(t, tc.want, rec.Code, tc.target)
		if tc.precompressed {
			assert.Equal(t, "br", rec.Header().Get(echo.HeaderContentEncoding), tc.target)
			assert.Equal(t, "br", rec.Body.String(), tc.target)
		}
	}
}
