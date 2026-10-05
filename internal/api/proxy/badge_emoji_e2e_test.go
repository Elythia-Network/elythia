package proxy

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/mediaproxy"
)

// makeColorPNG builds a 200x120 image with a red-to-blue horizontal gradient.
//
// **単色にしてはいけない (#2920)。** upstream は元画像の greyscale エントロピーが
// 0.1 未満なら 404 を返す (`Skip to provide badge`) ので、単色だとバッジが
// 作られない。以前この fixture は単色の赤で、**upstream が 404 を返す入力に対して
// 200 を固定していた**。
func makeColorPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 200, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 200; x++ {
			v := uint8(x * 255 / 199)
			img.Set(x, y, color.RGBA{R: 255 - v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// makeSolidPNG builds a 96x96 solid image, which upstream skips with a 404.
func makeSolidPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 96, 96))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// #2909: `/emoji/<name>.webp?badge=1` が組み立てる URL の形が、ハンドラ経由で
// 実際に badge (96x96、暗いところが透明な silhouette PNG) になること。
//
// **redirect の Location を固定するだけでは足りない。** `emoji_redirect_test.go` は
// entity.BadgeEmojiProxyURL が出す URL の形しか見ておらず、その URL を proxy が
// 実際に badge として解釈するかは検査していない (#2905 で「setter は固定したが
// 渡す 1 行が未検証」を踏んだのと同じ型)。ここは URL を直書きして**受け側の挙動**を
// 見る。`emoji=1` を足すと badge が奪われることも同時に固定するので、
// BadgeEmojiProxyURL が emoji=1 を付けてはいけない理由が根拠つきで残る。
func TestHandle_BadgeEmojiIsGrayscale96(t *testing.T) {
	pngData := makeColorPNG(t)
	imgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData)
	}))
	defer imgServer.Close()

	cfg := &config.Config{
		URL: "https://example.com", MediaProxy: "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{imgServer.URL + "/a.png": true}},
		cfg.MediaProxySecret, testAllowedCIDRs)
	h := NewHandler(svc, cfg)
	e := echo.New()

	// entity.BadgeEmojiProxyURL が出す形 (emoji.png + badge のみ)。
	rec := doRequest(e, h, http.MethodGet, "/proxy/emoji.png?url="+imgServer.URL+"/a.png&badge=1",
		map[string]string{"User-Agent": "TestBrowser/1.0"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"),
		"badge は PNG で返る")

	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 96, img.Bounds().Dx(), "badge は 96x96")
	assert.Equal(t, 96, img.Bounds().Dy(), "badge は 96x96")
	r, g, b, _ := img.At(48, 48).RGBA()
	assert.True(t, r == g && g == b,
		"badge はグレースケールでなければならない (got r=%d g=%d b=%d)", r, g, b)

	// **emoji=1 を足すと badge が奪われる。** BadgeEmojiProxyURL が
	// emoji=1 を付けてはいけない理由をここで固定する。
	rec = doRequest(e, h, http.MethodGet, "/proxy/emoji.png?url="+imgServer.URL+"/a.png&emoji=1&badge=1",
		map[string]string{"User-Agent": "TestBrowser/1.0"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEqual(t, "image/png", rec.Header().Get("Content-Type"),
		"emoji=1 が同居すると parseMode が ModeEmoji を返し、badge にならない")
}

// **単色の絵文字はハンドラ経由で 404 になる (#2920)。**
//
// upstream の `stats().entropy < 0.1` → `Skip to provide badge`。SW の
// create-notification.ts は `res.status !== 200` で iconUrl('plus') へ落ちるので、
// 判別できないバッジを配るより 404 の方が親切。**ハンドラまで通して見る** —
// processBadge が error を返しても、それが 404 になるかは handler の
// マッピング次第 (ErrNotFound だけが 404、他は 500)。
func TestHandle_BadgeEmojiSkipsSolidImage(t *testing.T) {
	pngData := makeSolidPNG(t)
	imgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData)
	}))
	defer imgServer.Close()

	cfg := &config.Config{
		URL: "https://example.com", MediaProxy: "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{imgServer.URL + "/s.png": true}},
		cfg.MediaProxySecret, testAllowedCIDRs)
	h := NewHandler(svc, cfg)
	e := echo.New()

	rec := doRequest(e, h, http.MethodGet, "/proxy/emoji.png?url="+imgServer.URL+"/s.png&badge=1",
		map[string]string{"User-Agent": "TestBrowser/1.0"})
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"単色のバッジは配らない (SW が iconUrl('plus') へ落ちる)")
}
