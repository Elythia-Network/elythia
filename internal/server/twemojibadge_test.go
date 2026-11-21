package server

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 単純な赤い四角。ラスタライズと後段の処理が通ることを確かめられればよい。
const testSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 36 36"><path fill="#FF0000" d="M4 4h28v28H4z"/></svg>`

func newBadgeTestHandler(t *testing.T) *twemojiBadgeHandler {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1f004.svg"), []byte(testSVG), 0o600))
	return newTwemojiBadgeHandler(dir)
}

func doBadgeReq(t *testing.T, h *twemojiBadgeHandler, name string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	c.SetPathValues(echo.PathValues{{Name: "*", Value: name}})
	if err := h.Serve(c); err != nil {
		if he, ok := err.(*echo.HTTPError); ok {
			rec.Code = he.Code
			return rec
		}
		t.Fatal(err)
	}
	return rec
}

func TestTwemojiBadge_ServesPNG(t *testing.T) {
	rec := doBadgeReq(t, newBadgeTestHandler(t), "1f004.png")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get(echo.HeaderContentType))
	assert.Equal(t, "max-age=2592000", rec.Header().Get("Cache-Control"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'none'")

	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err, "PNG としてデコードできること")
	assert.Equal(t, image.Rect(0, 0, badgeOutputSize, badgeOutputSize), img.Bounds())
}

// upstream は .svg を 404 にする (PNG のみ受け付ける)。
func TestTwemojiBadge_RejectsNonPNG(t *testing.T) {
	h := newBadgeTestHandler(t)
	for _, name := range []string{"1f004.svg", "1f004", "1f004.jpg"} {
		assert.Equal(t, http.StatusNotFound, doBadgeReq(t, h, name).Code, name)
	}
}

// upstream の正規表現 ^[0-9a-f-]+\.png$ に合わないものは弾く。
func TestTwemojiBadge_RejectsInvalidName(t *testing.T) {
	h := newBadgeTestHandler(t)
	for _, name := range []string{"../etc/passwd.png", "ZZZZ.png", "sub/dir.png", ".png", "1f004.png.svg"} {
		assert.Equal(t, http.StatusNotFound, doBadgeReq(t, h, name).Code, name)
	}
}

func TestTwemojiBadge_UnknownEmojiIs404(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, doBadgeReq(t, newBadgeTestHandler(t), "deadbeef.png").Code)
}

// バッジは「絵文字の形が抜けた板」なので、全面が透明でも不透明でもない。
func TestRenderTwemojiBadge_ProducesMask(t *testing.T) {
	out, err := renderTwemojiBadge([]byte(testSVG))
	require.NoError(t, err)

	img, err := png.Decode(bytes.NewReader(out))
	require.NoError(t, err)

	var opaque, transparent int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			switch {
			case a>>8 > 200:
				opaque++
			case a>>8 < 50:
				transparent++
			}
		}
	}
	assert.Greater(t, opaque, 0, "不透明な領域があること")
	assert.Greater(t, transparent, 0, "絵文字の形が抜けていること")
}

// 壊れた入力でも panic せず PNG を返すこと。oksvg は不正な XML でもエラーを
// 返さないことがあるので、「落ちない」ことだけを保証する。
func TestRenderTwemojiBadge_BrokenInputDoesNotPanic(t *testing.T) {
	for _, in := range []string{"not an svg", "", "<svg>"} {
		out, err := renderTwemojiBadge([]byte(in))
		if err == nil {
			_, derr := png.Decode(bytes.NewReader(out))
			assert.NoError(t, derr, "エラーを返さないなら PNG として妥当であること")
		}
	}
}

func TestClamp8(t *testing.T) {
	assert.EqualValues(t, 0, clamp8(-10))
	assert.EqualValues(t, 255, clamp8(300))
	assert.EqualValues(t, 128, clamp8(128.4))
}

func TestNormalise(t *testing.T) {
	assert.InDelta(t, 0, normalise(10, 10, 200), 0.01)
	assert.InDelta(t, 255, normalise(200, 10, 200), 0.01)
	// レンジが潰れている場合はそのまま返す。
	assert.InDelta(t, 42, normalise(42, 100, 100), 0.01)
}

// 2 回目はキャッシュから返すこと。
//
// **この endpoint は `/api` の外なのでレート制限が掛からない** (middleware は
// api グループにしか付いていない) 一方、1 回のラスタライズが 15ms 前後かかる。
// 元の SVG を消しても 2 回目が返ることで、再生成していないことを示す。
func TestTwemojiBadge_CachesRenderedPNG(t *testing.T) {
	dir := t.TempDir()
	svgPath := filepath.Join(dir, "1f004.svg")
	require.NoError(t, os.WriteFile(svgPath, []byte(testSVG), 0o600))
	h := newTwemojiBadgeHandler(dir)

	first := doBadgeReq(t, h, "1f004.png")
	require.Equal(t, http.StatusOK, first.Code)

	require.NoError(t, os.Remove(svgPath))

	second := doBadgeReq(t, h, "1f004.png")
	require.Equal(t, http.StatusOK, second.Code, "2 回目が再生成に落ちている")
	assert.Equal(t, first.Body.Bytes(), second.Body.Bytes())
}

// 見つからなかった名前はキャッシュしないこと。**キャッシュすると、後から
// ファイルを置いた構成で永久に 404 のままになる。**
func TestTwemojiBadge_DoesNotCacheMisses(t *testing.T) {
	dir := t.TempDir()
	h := newTwemojiBadgeHandler(dir)

	require.Equal(t, http.StatusNotFound, doBadgeReq(t, h, "1f004.png").Code)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "1f004.svg"), []byte(testSVG), 0o600))
	assert.Equal(t, http.StatusOK, doBadgeReq(t, h, "1f004.png").Code)
}
