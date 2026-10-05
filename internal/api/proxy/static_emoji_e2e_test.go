package proxy

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/mediaproxy"
)

func makeAnimatedGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}}
	frames := make([]*image.Paletted, 0, 2)
	for i := 0; i < 2; i++ {
		img := image.NewPaletted(image.Rect(0, 0, 4, 4), pal)
		img.SetColorIndex(0, 0, uint8(i))
		frames = append(frames, img)
	}
	var buf bytes.Buffer
	require.NoError(t, gif.EncodeAll(&buf, &gif.GIF{Image: frames, Delay: []int{0, 0}}))
	return buf.Bytes()
}

// #2905: `?emoji=1&static=1` がハンドラ経由で静止画になること。
//
// **parseAnimated の単体テストだけでは足りない。** その結果を Fetch へ渡す
// 1 行を消しても単体テストは緑のまま通る (実測)。#2868 で踏んだ「setter は
// 固定したが渡す 1 行が未検証」と同じ型なので、ハンドラを実際に叩いて確かめる。
func TestHandle_StaticEmojiIsNotAnimated(t *testing.T) {
	gifData := makeAnimatedGIF(t)
	imgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(gifData)
	}))
	defer imgServer.Close()

	cfg := &config.Config{
		URL: "https://example.com", MediaProxy: "https://example.com/proxy",
		MediaProxySecret: []byte("test-secret"),
		UserAgent:        "Misskey/2026.5.4 (https://example.com)",
	}
	svc := mediaproxy.NewService(cfg.URL, cfg.UserAgent,
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{imgServer.URL + "/a.gif": true}},
		cfg.MediaProxySecret, testAllowedCIDRs)
	h := NewHandler(svc, cfg)
	e := echo.New()

	// static 無し: アニメーションのまま返る。
	rec := doRequest(e, h, http.MethodGet, "/proxy/emoji.webp?url="+imgServer.URL+"/a.gif&emoji=1", map[string]string{"User-Agent": "TestBrowser/1.0"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/gif", rec.Header().Get("Content-Type"),
		"static 無しではアニメーションを保つこと")

	// static あり: 静止画へ変換される。
	rec = doRequest(e, h, http.MethodGet, "/proxy/emoji.webp?url="+imgServer.URL+"/a.gif&emoji=1&static=1", map[string]string{"User-Agent": "TestBrowser/1.0"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEqual(t, "image/gif", rec.Header().Get("Content-Type"),
		"static=1 なのに GIF のまま返っている (parseAnimated の結果が Fetch に渡っていない)")
}
