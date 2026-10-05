package proxy

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/mediaproxy"
)

// --- mock AllowlistChecker ---

type mockAllowlist struct {
	allowed map[string]bool
}

func (m *mockAllowlist) IsAllowedURL(_ context.Context, url string) (bool, error) {
	return m.allowed[url], nil
}

// --- mock Storage ---

type mockStorage struct {
	files map[string][]byte
}

func (m *mockStorage) Put(_ string, _ io.Reader) (string, error) { return "", nil }
func (m *mockStorage) Delete(_ string) error                     { return nil }
func (m *mockStorage) Get(key string) (io.ReadCloser, error) {
	data, ok := m.files[key]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return io.NopCloser(&byteReader{data: data}), nil
}

type byteReader struct {
	data []byte
	pos  int
}

func (b *byteReader) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}

// testAllowedCIDRs はテストでhttptest (127.0.0.1) への接続を許可するCIDRリスト
var testAllowedCIDRs = []string{"127.0.0.0/8", "::1/128"}

// --- test helpers ---

func makePNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf byteWriter
	_ = png.Encode(&buf, img)
	return buf.data
}

type byteWriter struct {
	data []byte
}

func (w *byteWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

func setupHandler(t *testing.T, allowedURLs map[string]bool) (*Handler, *echo.Echo, *httptest.Server) {
	t.Helper()

	imgData := makePNG()
	imgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.png" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgData)
	}))

	cfg := &config.Config{
		URL:                       "https://example.com",
		MediaProxy:                "https://example.com/proxy",
		ExternalMediaProxyEnabled: false,
		MediaProxySecret:          []byte("test-secret"),
		UserAgent:                 "Misskey/2026.5.4 (https://example.com)",
	}

	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: allowedURLs},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)

	h := NewHandler(svc, cfg)
	e := echo.New()
	return h, e, imgServer
}

func doRequest(e *echo.Echo, h *Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h.Handle(c)
	return rec
}

// --- tests ---

func TestHandle_ValidHMACSignature(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	url := imgServer.URL + "/avatar.png"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+url+"&sig="+sig,
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Cache-Control"), "max-age=31536000")
	// Output format negotiation depends on the request's Accept header, so
	// shared caches MUST key on Accept (#637 review UR-012).
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
}

func TestHandle_AllowlistedURL(t *testing.T) {
	imgURL := ""
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()
	imgURL = imgServer.URL + "/avatar.png"

	// 再作成してallowlistにURLを含める
	h2, e2, imgServer2 := setupHandler(t, map[string]bool{imgURL: true})
	_ = h
	_ = e
	defer imgServer2.Close()

	// allowlist内のURLはimgServerのURLなので、imgServer2からは取得できないが、
	// allowlistのURLが直接一致するかをテストしたいのでimgServer自体を使う
	cfg := &config.Config{
		URL:                       "https://example.com",
		MediaProxy:                "https://example.com/proxy",
		ExternalMediaProxyEnabled: false,
		MediaProxySecret:          []byte("test-secret"),
		UserAgent:                 "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{imgURL: true}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)
	_ = h2
	_ = e2

	req := httptest.NewRequest(http.MethodGet, "/proxy/image.webp?url="+imgURL, nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler.Handle(c)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandle_UnauthorizedURL(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+imgServer.URL+"/avatar.png",
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandle_UnauthorizedURL_WithFallback(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+imgServer.URL+"/avatar.png&fallback=1",
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Cache-Control"), "max-age=300")
}

func TestHandle_MissingUserAgent(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+imgServer.URL+"/avatar.png",
		map[string]string{})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandle_RecursiveProxy(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+imgServer.URL+"/avatar.png",
		map[string]string{"User-Agent": "Misskey/2026.5.4 (https://other.example)"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandle_MissingURL(t *testing.T) {
	h, e, _ := setupHandler(t, map[string]bool{})

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp",
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandle_NotFound_WithFallback(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	url := imgServer.URL + "/missing.png"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+url+"&sig="+sig+"&fallback=1",
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Cache-Control"), "max-age=300")
}

func TestHandle_EmojiMode(t *testing.T) {
	imgURL := ""
	_, _, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()
	imgURL = imgServer.URL + "/emoji.png"

	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{imgURL: true}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)

	req := httptest.NewRequest(http.MethodGet, "/proxy/image.webp?url="+imgURL+"&emoji=1", nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler.Handle(c)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/webp", rec.Header().Get("Content-Type"))
}

func TestHandle_ExternalProxyRedirect(t *testing.T) {
	cfg := &config.Config{
		URL:                       "https://example.com",
		MediaProxy:                "https://proxy.example.com",
		ExternalMediaProxyEnabled: true,
		MediaProxySecret:          []byte("test-secret"),
		UserAgent:                 "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/proxy/image.webp?url=https://remote.example/img.png",
		nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler.Handle(c)

	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "proxy.example.com")
	assert.Contains(t, rec.Header().Get("Cache-Control"), "max-age=259200")
}

func TestHandle_ExternalProxyRedirect_SkippedWithOrigin(t *testing.T) {
	imgURL := ""
	_, _, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()
	imgURL = imgServer.URL + "/img.png"

	cfg := &config.Config{
		URL:                       "https://example.com",
		MediaProxy:                "https://proxy.example.com",
		ExternalMediaProxyEnabled: true,
		MediaProxySecret:          []byte("test-secret"),
		UserAgent:                 "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{imgURL: true}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)

	req := httptest.NewRequest(http.MethodGet,
		"/proxy/image.webp?url="+imgURL+"&origin=1",
		nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler.Handle(c)

	// origin=1なので外部プロキシにリダイレクトせず直接処理する
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestParseMode(t *testing.T) {
	e := echo.New()

	tests := []struct {
		name     string
		query    string
		expected mediaproxy.ProxyMode
	}{
		{"default", "/proxy/image.webp?url=x", mediaproxy.ModeDefault},
		{"emoji", "/proxy/image.webp?url=x&emoji=1", mediaproxy.ModeEmoji},
		{"avatar", "/proxy/image.webp?url=x&avatar=1", mediaproxy.ModeAvatar},
		{"static", "/proxy/image.webp?url=x&static=1", mediaproxy.ModeStatic},
		{"preview", "/proxy/image.webp?url=x&preview=1", mediaproxy.ModePreview},
		{"badge", "/proxy/image.webp?url=x&badge=1", mediaproxy.ModeBadge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.query, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			assert.Equal(t, tt.expected, parseMode(c))
		})
	}
}

func TestIsProxyFilename(t *testing.T) {
	assert.True(t, isProxyFilename("image.webp"))
	assert.True(t, isProxyFilename("preview.webp"))
	assert.True(t, isProxyFilename("static.webp"))
	assert.True(t, isProxyFilename("emoji.webp"))
	assert.True(t, isProxyFilename("emoji.png"))
	assert.True(t, isProxyFilename("avatar.webp"))
	assert.False(t, isProxyFilename("example.com/img.png"))
	assert.False(t, isProxyFilename("random.txt"))
}

func TestHandle_PathBasedURL(t *testing.T) {
	_, _, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	// imgServer.URL is like "http://127.0.0.1:PORT"
	// Path-based: /proxy/127.0.0.1:PORT/img.png → https://127.0.0.1:PORT/img.png
	host := imgServer.URL[len("http://"):]
	proxyURL := host + "/img.png"

	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{"https://" + proxyURL: true}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)

	// HTTPS is prepended by the handler for path-based URLs, but imgServer uses HTTP.
	// Use HMAC to authorize directly
	fullURL := "https://" + proxyURL
	sig := mediaproxy.SignURL([]byte("test-secret"), fullURL)

	req := httptest.NewRequest(http.MethodGet, "/proxy/"+proxyURL+"?sig="+sig, nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler.Handle(c)

	// HTTPS URL won't connect to the HTTP test server, so it'll fail to fetch.
	// But the path-based URL extraction + authorization should have been exercised.
	//
	// **接続できなかったので 502 (#3034)。** 以前は `fetchRemote` が
	// transport エラーを `ErrNotFound` に潰していたので 404 だった。
	// **1 点に固定する** — 旧版の `404 || 500` は、まさに今回変わった値を
	// 選択肢に含んでいたので、挙動が変わっても緑のまま通った。
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestHandle_NotFound_NoFallback(t *testing.T) {
	_, _, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	url := imgServer.URL + "/missing.png"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/proxy/image.webp?url="+url+"&sig="+sig, nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)
	_ = handler.Handle(c)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Cache-Control"), "max-age=86400")
}

func TestHandle_InternalError_WithFallback(t *testing.T) {
	// リモートサーバーが不正なレスポンスを返すケース
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("not an image"))
	}))
	defer ts.Close()

	url := ts.URL + "/bad.js"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)

	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)

	req := httptest.NewRequest(http.MethodGet, "/proxy/image.webp?url="+url+"&sig="+sig+"&fallback=1", nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler.Handle(c)

	assert.Equal(t, http.StatusOK, rec.Code) // fallback returns 200 with dummy PNG
	assert.Contains(t, rec.Header().Get("Cache-Control"), "max-age=300")
}

func TestHandle_InternalError_NoFallback(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("not an image"))
	}))
	defer ts.Close()

	url := ts.URL + "/bad.js"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)

	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)

	req := httptest.NewRequest(http.MethodGet, "/proxy/image.webp?url="+url+"&sig="+sig, nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler.Handle(c)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Header().Get("Cache-Control"), "max-age=300")
	// 500 の原因がログに残る (#3383)。status だけでは変換の失敗と区別できない。
	assert.Contains(t, logs.String(), "mediaproxy: proxy failed")
	assert.Contains(t, logs.String(), `err="mediaproxy: rejected MIME type`)
	assert.Contains(t, logs.String(), "mode=default")
	assert.Contains(t, logs.String(), "url="+url)
}

func TestHandle_CacheHeaders(t *testing.T) {
	_, _, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()
	imgURL := imgServer.URL + "/img.png"

	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}

	t.Run("success has immutable cache", func(t *testing.T) {
		svc := mediaproxy.NewService(
			cfg.URL, cfg.UserAgent,
			&mockStorage{files: map[string][]byte{}},
			&mockAllowlist{allowed: map[string]bool{imgURL: true}},
			cfg.MediaProxySecret,
			testAllowedCIDRs,
		)
		handler := NewHandler(svc, cfg)

		req := httptest.NewRequest(http.MethodGet, "/proxy/image.webp?url="+imgURL, nil)
		req.Header.Set("User-Agent", "TestBrowser/1.0")
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(req, rec)
		_ = handler.Handle(c)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Cache-Control"), "immutable")
	})

	t.Run("forbidden has day cache", func(t *testing.T) {
		svc := mediaproxy.NewService(
			cfg.URL, cfg.UserAgent,
			&mockStorage{files: map[string][]byte{}},
			&mockAllowlist{allowed: map[string]bool{}},
			cfg.MediaProxySecret,
			nil,
		)
		handler := NewHandler(svc, cfg)

		req := httptest.NewRequest(http.MethodGet, "/proxy/image.webp?url="+imgURL, nil)
		req.Header.Set("User-Agent", "TestBrowser/1.0")
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(req, rec)
		_ = handler.Handle(c)

		require.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Header().Get("Cache-Control"), "max-age=86400")
	})
}

func TestHandle_TooLarge(t *testing.T) {
	// Content-Lengthが超過するレスポンスを返すサーバー
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "999999999")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	url := ts.URL + "/huge.png"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)

	cfg := &config.Config{
		URL:              "https://example.com",
		MediaProxy:       "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(
		cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{}},
		cfg.MediaProxySecret,
		testAllowedCIDRs,
	)
	handler := NewHandler(svc, cfg)

	req := httptest.NewRequest(http.MethodGet, "/proxy/image.webp?url="+url+"&sig="+sig, nil)
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = handler.Handle(c)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// #2106 L43: mk-go 自身の UA (mk-go/) も recursive proxy として弾く。
func TestHandle_RecursiveProxy_MkGoUA(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+imgServer.URL+"/avatar.png",
		map[string]string{"User-Agent": "mk-go/0.9.1 (https://other.example)"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// #2905: `static` は mode と直交する軸。
//
// **parseMode は emoji を static より先に見る。** `?emoji=1&static=1` は
// 「emoji のリサイズ寸法で、ただし静止画」なので、mode を ModeStatic に倒すと
// 寸法まで変わってしまう。静止画かどうかは parseAnimated が別に判定する
// (upstream の `animated: !('static' in query)` と同じ)。
func TestParseAnimated(t *testing.T) {
	e := echo.New()

	tests := []struct {
		name  string
		query string
		want  bool
		mode  mediaproxy.ProxyMode
	}{
		{"クエリ無しはアニメーションを保つ", "/proxy/image.webp?url=x", true, mediaproxy.ModeDefault},
		{"emoji のみ", "/proxy/image.webp?url=x&emoji=1", true, mediaproxy.ModeEmoji},
		// **これが #2905 の本体。** mode は ModeEmoji のまま、animated だけ false。
		{"emoji + static", "/proxy/image.webp?url=x&emoji=1&static=1", false, mediaproxy.ModeEmoji},
		{"avatar + static", "/proxy/image.webp?url=x&avatar=1&static=1", false, mediaproxy.ModeAvatar},
		{"static 単独", "/proxy/image.webp?url=x&static=1", false, mediaproxy.ModeStatic},
		// **値は問わない。** upstream は fastify の `'static' in query` なので、
		// 空値でも静止画になる。emoji_redirect.go の判定とも揃える。
		{"static=0 でも静止画扱い", "/proxy/image.webp?url=x&emoji=1&static=0", false, mediaproxy.ModeEmoji},
		{"static= (空値) でも静止画扱い", "/proxy/image.webp?url=x&emoji=1&static=", false, mediaproxy.ModeEmoji},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.query, nil)
			c := e.NewContext(req, httptest.NewRecorder())
			assert.Equal(t, tt.want, parseAnimated(c), "parseAnimated")
			assert.Equal(t, tt.mode, parseMode(c), "mode は static で変わらないこと")
		})
	}
}

// **`/proxy/*` にも `filesHandler` と同じ 3 点を付ける (#3037)。**
//
// `nosniff` は #2782 で全応答に付き、Content-Type も `browsersafeMIMEs` に
// 絞ってあるので既知の経路は塞がっているが、**この origin は自分のドメイン**
// なので 1 つ取りこぼすと同一オリジンの XSS になる。mk-go は自分のファイル
// 配信で 3 重に塞いでいるのだから、こちらだけ 1 枚薄いままにしない。
func TestHandle_SetsContentSecurityHeaders(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	url := imgServer.URL + "/avatar.png"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)
	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+url+"&sig="+sig,
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t,
		"default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'",
		rec.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "inline", rec.Header().Get("Content-Disposition"))
}

// **ダミー画像に倒れた応答にも付ける。** 失敗経路だけ薄くならないように。
func TestHandle_FallbackAlsoSetsContentSecurityHeaders(t *testing.T) {
	h, e, imgServer := setupHandler(t, map[string]bool{})
	defer imgServer.Close()

	// 署名は通るが取得に失敗する URL は fallback (ダミー PNG) になる。
	url := imgServer.URL + "/missing.png"
	sig := mediaproxy.SignURL([]byte("test-secret"), url)
	rec := doRequest(e, h, http.MethodGet,
		"/proxy/image.webp?url="+url+"&sig="+sig+"&fallback=1",
		map[string]string{"User-Agent": "TestBrowser/1.0"})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t,
		"default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'",
		rec.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "inline", rec.Header().Get("Content-Disposition"))
}
