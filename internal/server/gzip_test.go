package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 1KB 以上の JSON body を要求すると gzip-encoded で返ってくる。
func TestGzipMiddleware_CompressesLargeJSON(t *testing.T) {
	e := echo.New()
	e.Use(echomw.GzipWithConfig(gzipConfig()))

	// 1KB 以上の payload。MinLength=1024 を超えさせる。
	payload := map[string]any{"text": strings.Repeat("a", 2000)}
	e.GET("/api/big", func(c echo.Context) error {
		return c.JSON(http.StatusOK, payload)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/big", nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "gzip", rec.Header().Get(echo.HeaderContentEncoding),
		"large JSON response should be gzip-encoded")

	// gzip decode して内容が一致することを確認する。
	gr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	defer gr.Close()
	decoded, err := io.ReadAll(gr)
	require.NoError(t, err)
	assert.Contains(t, string(decoded), strings.Repeat("a", 2000))
}

// 1KB 未満の小さな body は gzip しない (MinLength を下回るため、
// overhead が savings を上回る判断)。
func TestGzipMiddleware_SkipsSmallBody(t *testing.T) {
	e := echo.New()
	e.Use(echomw.GzipWithConfig(gzipConfig()))

	e.GET("/api/small", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
	})

	req := httptest.NewRequest(http.MethodGet, "/api/small", nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding),
		"small body should bypass gzip per MinLength config")
}

// /streaming は WebSocket upgrade 経路なので Skipper で除外する。
// AcceptEncoding に gzip が入っていても Content-Encoding は付かない。
func TestGzipMiddleware_SkipsStreamingRoute(t *testing.T) {
	e := echo.New()
	e.Use(echomw.GzipWithConfig(gzipConfig()))

	// 1KB 以上の本文を返しても、/streaming パスは Skipper で gzip を
	// 切るので Content-Encoding が付かないこと。
	e.GET("/streaming", func(c echo.Context) error {
		return c.String(http.StatusOK, strings.Repeat("x", 2000))
	})

	req := httptest.NewRequest(http.MethodGet, "/streaming", nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding),
		"/streaming must bypass gzip to keep WebSocket frames intact")
}

// /backup-download は数 GB の dump をそのまま渡すので gzip しない (#3462)。
// gzip すると Content-Length が消え、Range の応答まで包まれる。
func TestGzipMiddleware_SkipsBackupDownload(t *testing.T) {
	e := echo.New()
	e.Use(echomw.GzipWithConfig(gzipConfig()))
	e.GET("/backup-download", func(c echo.Context) error {
		return c.String(http.StatusOK, strings.Repeat("x", 2000))
	})
	req := httptest.NewRequest(http.MethodGet, "/backup-download?token=x", nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding))
}

// AcceptEncoding に gzip が含まれない client には raw body を返す
// (Echo middleware の負荷ハンドリング)。
func TestGzipMiddleware_DoesNotCompressWithoutAcceptEncoding(t *testing.T) {
	e := echo.New()
	e.Use(echomw.GzipWithConfig(gzipConfig()))

	e.GET("/api/big", func(c echo.Context) error {
		return c.String(http.StatusOK, strings.Repeat("a", 2000))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/big", nil)
	// no Accept-Encoding header
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding))
}
