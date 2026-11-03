package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/mediaproxy"
)

// 変換した画像の応答に Content-Length を付けることを固定する (#3966)。
// 付けないと chunked になり、受け手は全体の大きさを知らずに読むことになる。

func TestHandle_SetsContentLengthFromResult(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	url := imgServer.URL + "/avatar.png"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+url+"&sig="+sig,
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotZero(t, rec.Body.Len())
	assert.Equal(t, strconv.Itoa(rec.Body.Len()), rec.Header().Get(echo.HeaderContentLength))
}

// sizedProxy returns a result whose Size is set by the test.
type sizedProxy struct {
	body []byte
	size int64
}

func (sizedProxy) Authorize(context.Context, string, string) error { return nil }

func (s sizedProxy) Fetch(context.Context, string, mediaproxy.ProxyMode, mediaproxy.OutputFormat, bool) (*mediaproxy.ProxyResult, error) {
	return &mediaproxy.ProxyResult{
		Body:        io.NopCloser(bytes.NewReader(s.body)),
		ContentType: "image/png",
		Size:        s.size,
	}, nil
}

func TestHandle_OmitsContentLengthWhenSizeUnknown(t *testing.T) {
	// Size を埋めない結果に Content-Length: 0 を付けると、本文が切り捨てられる。
	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	h := NewHandler(sizedProxy{body: []byte("not-empty")}, cfg)

	rec := doRequest(echo.New(), h, http.MethodGet,
		"/proxy/image.webp?url=https%3A%2F%2Fremote.example%2Fa.png",
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get(echo.HeaderContentLength))
	assert.Equal(t, "not-empty", rec.Body.String())
}
