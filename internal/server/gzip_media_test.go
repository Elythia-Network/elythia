package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// pngLikeBody returns n bytes that start with the PNG signature and are
// otherwise incompressible, like a real image.
func pngLikeBody(n int) []byte {
	r := rand.New(rand.NewPCG(3966, 1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	copy(b, "\x89PNG\r\n\x1a\n")
	return b
}

func gzipServe(t *testing.T, e *echo.Echo, method, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// /files は画像を gzip せず、Content-Length を付けたまま返す (#3966)。
// 本物の filesHandler を通す。
func TestGzipMiddleware_FilesImageIsNotCompressed(t *testing.T) {
	body := pngLikeBody(64 * 1024)
	const key = "img-key"
	lookup := &stubFilesLookup{byKey: map[string]*model.DriveFile{key: {ID: "f1"}}}
	primary := &memStorage{byKey: map[string]string{key: string(body)}}

	e := echo.New()
	e.Use(gzipMiddleware())
	e.GET("/files/:accessKey", filesHandler(lookup, primary, nil))

	rec := gzipServe(t, e, http.MethodGet, "/files/"+key, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Result().Header.Get(echo.HeaderContentType))
	assert.Empty(t, rec.Result().Header.Get(echo.HeaderContentEncoding), "an image must not be gzipped")
	assert.Equal(t, strconv.Itoa(len(body)), rec.Result().Header.Get(echo.HeaderContentLength))
	assert.True(t, bytes.Equal(body, rec.Body.Bytes()), "the body must be the stored bytes")

	// Range の 206 も包まない。Content-Range と本文の関係が保たれる。
	rec = gzipServe(t, e, http.MethodGet, "/files/"+key, map[string]string{"Range": "bytes=100-2099"})
	require.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Empty(t, rec.Result().Header.Get(echo.HeaderContentEncoding), "a 206 must not be gzipped")
	assert.Equal(t, "bytes 100-2099/"+strconv.Itoa(len(body)), rec.Result().Header.Get("Content-Range"))
	assert.Equal(t, "2000", rec.Result().Header.Get(echo.HeaderContentLength))
	assert.True(t, bytes.Equal(body[100:2100], rec.Body.Bytes()))
}

// 型ごとの判定。画像・動画・音声 (SVG を除く) と 206 は圧縮しない。
func TestGzipMiddleware_CompressionByContentType(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		status      int
		compressed  bool
	}{
		{"jpeg", "image/jpeg", http.StatusOK, false},
		{"webp", "image/webp", http.StatusOK, false},
		{"mp4", "video/mp4", http.StatusOK, false},
		{"audio with params", "audio/ogg; codecs=opus", http.StatusOK, false},
		{"image with malformed params", "image/png; name=a b", http.StatusOK, false},
		{"image with duplicate params", "image/png; a=1; a=2", http.StatusOK, false},
		{"upper case image", "IMAGE/PNG", http.StatusOK, false},
		{"svg stays compressed", "image/svg+xml", http.StatusOK, true},
		{"json", "application/json; charset=UTF-8", http.StatusOK, true},
		{"html", "text/html; charset=utf-8", http.StatusOK, true},
		{"activity json", "application/activity+json; charset=utf-8", http.StatusOK, true},
		{"partial text", "text/plain", http.StatusPartialContent, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := strings.Repeat("a", 4000)
			e := echo.New()
			e.Use(gzipMiddleware())
			e.GET("/x", func(c echo.Context) error {
				c.Response().Header().Set(echo.HeaderContentLength, strconv.Itoa(len(payload)))
				return c.Blob(tc.status, tc.contentType, []byte(payload))
			})
			rec := gzipServe(t, e, http.MethodGet, "/x", nil)
			require.Equal(t, tc.status, rec.Code)
			if !tc.compressed {
				assert.Empty(t, rec.Result().Header.Get(echo.HeaderContentEncoding))
				assert.Equal(t, strconv.Itoa(len(payload)), rec.Result().Header.Get(echo.HeaderContentLength))
				assert.Equal(t, payload, rec.Body.String())
				return
			}
			require.Equal(t, "gzip", rec.Result().Header.Get(echo.HeaderContentEncoding))
			assert.Empty(t, rec.Result().Header.Get(echo.HeaderContentLength))
			gr, err := gzip.NewReader(rec.Body)
			require.NoError(t, err)
			decoded, err := io.ReadAll(gr)
			require.NoError(t, err)
			assert.Equal(t, payload, string(decoded))
		})
	}
}

// Content-Type を付けずに書いた本文は、推定した型で判定する。
func TestGzipMiddleware_SniffedImageIsNotCompressed(t *testing.T) {
	body := pngLikeBody(4000)
	e := echo.New()
	e.Use(gzipMiddleware())
	e.GET("/x", func(c echo.Context) error {
		c.Response().WriteHeader(http.StatusOK)
		_, err := c.Response().Write(body)
		return err
	})
	rec := gzipServe(t, e, http.MethodGet, "/x", nil)
	assert.Equal(t, "image/png", rec.Result().Header.Get(echo.HeaderContentType))
	assert.Empty(t, rec.Result().Header.Get(echo.HeaderContentEncoding))
	assert.True(t, bytes.Equal(body, rec.Body.Bytes()))
}

// 本文の無い応答 (リダイレクト、HEAD) は状態と Content-Length をそのまま返す。
func TestGzipMiddleware_BodylessResponsesKeepStatusAndLength(t *testing.T) {
	e := echo.New()
	e.Use(gzipMiddleware())
	e.GET("/redirect", func(c echo.Context) error {
		return c.Redirect(http.StatusFound, "/elsewhere")
	})
	e.HEAD("/img", func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderContentType, "image/png")
		c.Response().Header().Set(echo.HeaderContentLength, "5000")
		return c.NoContent(http.StatusOK)
	})

	rec := gzipServe(t, e, http.MethodGet, "/redirect", nil)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/elsewhere", rec.Result().Header.Get(echo.HeaderLocation))
	assert.Empty(t, rec.Result().Header.Get(echo.HeaderContentEncoding))

	rec = gzipServe(t, e, http.MethodHead, "/img", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "5000", rec.Result().Header.Get(echo.HeaderContentLength))
	assert.Empty(t, rec.Result().Header.Get(echo.HeaderContentEncoding))
}

// 本文の前に Flush するストリーム (SSE など) は、今どおり gzip で流れる。
func TestGzipMiddleware_FlushedStreamIsStillCompressed(t *testing.T) {
	e := echo.New()
	e.Use(gzipMiddleware())
	e.GET("/sse", func(c echo.Context) error {
		res := c.Response()
		res.Header().Set(echo.HeaderContentType, "text/event-stream")
		res.WriteHeader(http.StatusOK)
		res.Flush()
		_, err := res.Write([]byte("data: hello\n\n"))
		return err
	})
	rec := gzipServe(t, e, http.MethodGet, "/sse", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "gzip", rec.Result().Header.Get(echo.HeaderContentEncoding))
	assert.True(t, rec.Flushed)
	gr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	decoded, err := io.ReadAll(gr)
	require.NoError(t, err)
	assert.Equal(t, "data: hello\n\n", string(decoded))
}

// 自分で Content-Encoding を付けた応答を二重に圧縮しない。
func TestGzipMiddleware_DoesNotRecompressEncodedBody(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(pngLikeBody(4000))
	require.NoError(t, gw.Close())
	pre := buf.Bytes()

	e := echo.New()
	e.Use(gzipMiddleware())
	e.GET("/pre", func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderContentEncoding, "gzip")
		return c.Blob(http.StatusOK, "text/plain", pre)
	})
	rec := gzipServe(t, e, http.MethodGet, "/pre", nil)
	// echo の gzip middleware は後始末で、送った後のヘッダーの map から
	// Content-Encoding: gzip を消す。クライアントに届くのは送った時点の
	// ヘッダーなので、そちらで見る。
	assert.Equal(t, "gzip", rec.Result().Header.Get(echo.HeaderContentEncoding))
	require.Greater(t, len(pre), 1024, "the body must exceed MinLength to reach the gzip writer")
	assert.True(t, bytes.Equal(pre, rec.Body.Bytes()), "an encoded body must pass through as is")
}

// 内側の middleware が http.Hijacker を型アサーションで取る形でも、接続を
// 引き取れることを固定する (#3966 のレビュー)。
func TestGzipMiddleware_HijackThroughTypeAssertion(t *testing.T) {
	e := echo.New()
	e.Use(gzipMiddleware())
	e.GET("/ws", func(c echo.Context) error {
		hj, ok := c.Response().Writer.(http.Hijacker)
		if !ok {
			return c.String(http.StatusInternalServerError, "not a hijacker")
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return c.String(http.StatusInternalServerError, err.Error())
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nhijacked")
		return rw.Flush()
	})
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\n\r\n"))
	require.NoError(t, err)
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "hijacked", string(body))
}
