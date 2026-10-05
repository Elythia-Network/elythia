package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"net/http"
	"strconv"

	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// Misskey TSのgen-identicon.tsと同じ仕様で生成する
const (
	identiconSize   = 128
	identiconN      = 5
	identiconMargin = identiconSize / 4 // 32px
)

// TSのcolors配列と同じグラデーションペア
var gradientColors = [][2]color.RGBA{
	{hexColor("#FF512F"), hexColor("#DD2476")},
	{hexColor("#FF61D2"), hexColor("#FE9090")},
	{hexColor("#72FFB6"), hexColor("#10D164")},
	{hexColor("#FD8451"), hexColor("#FFBD6F")},
	{hexColor("#305170"), hexColor("#6DFC6B")},
	{hexColor("#00C0FF"), hexColor("#4218B8")},
	{hexColor("#009245"), hexColor("#FCEE21")},
	{hexColor("#0100EC"), hexColor("#FB36F4")},
	{hexColor("#FDABDD"), hexColor("#374A5A")},
	{hexColor("#38A2D7"), hexColor("#561139")},
	{hexColor("#121C84"), hexColor("#8278DA")},
	{hexColor("#5761B2"), hexColor("#1FC5A8")},
	{hexColor("#FFDB01"), hexColor("#0E197D")},
	{hexColor("#FF3E9D"), hexColor("#0E1F40")},
	{hexColor("#766eff"), hexColor("#00d4ff")},
	{hexColor("#9bff6e"), hexColor("#00d4ff")},
	{hexColor("#ff6e94"), hexColor("#00d4ff")},
	{hexColor("#ffa96e"), hexColor("#00d4ff")},
	{hexColor("#ffa96e"), hexColor("#ff009d")},
	{hexColor("#ffdd6e"), hexColor("#ff009d")},
}

func hexColor(hex string) color.RGBA {
	if hex[0] == '#' {
		hex = hex[1:]
	}
	r, _ := strconv.ParseUint(hex[0:2], 16, 8)
	g, _ := strconv.ParseUint(hex[2:4], 16, 8)
	b, _ := strconv.ParseUint(hex[4:6], 16, 8)
	return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
}

// seedからTS互換の乱数生成器を作る (random-seed ライブラリ互換)
func seedRand(seed string) *rand.Rand {
	// seedの各文字コードを足し合わせてint64に変換
	var h int64
	for _, c := range seed {
		h = h*31 + int64(c)
	}
	return rand.New(rand.NewSource(h))
}

// identiconHandler generates an identicon PNG matching Misskey TS's gen-identicon.ts.
// metaRepo を渡すと meta.enableIdenticonGeneration フラグを参照する。フラグが
// false なら 1x1 透明 PNG を返す (本家挙動と互換: avatar fallback の URL が
// 404 にならず、空画像で済むのでフロントの onerror フローが乱れない)。
// metaRepo が nil の場合は常に identicon を生成する (テストや初期セットアップ用)。
func identiconHandler(metaRepo repository.MetaRepository) echo.HandlerFunc {
	return func(c echo.Context) error {
		seed := c.Param("x")
		if seed == "" {
			seed = "default"
		}

		// meta フラグチェック (false なら無効化)
		if metaRepo != nil {
			if m, err := metaRepo.Fetch(); err == nil && !m.EnableIdenticonGeneration {
				return serveTransparentPixel(c)
			}
		}

		rng := seedRand(seed)
		img := generateIdenticon(rng)

		c.Response().Header().Set("Content-Type", "image/png")
		c.Response().Header().Set("Cache-Control", "public, max-age=86400")
		// identicon は mk-go が実際に PNG バイトを返す数少ない画像 route。
		// upstream の identicon には CSP が無いが、他のアセット route と同じ
		// 扱いに揃える。`default-src 'none'` は画像配信に何の制約も課さない
		// 一方、万一 PNG 以外が返る経路が生まれても実行を防ぐ。upstream より
		// 厳しい側の divergence (docs/divergence.md)。
		c.Response().Header().Set("Content-Security-Policy", assetCSP)
		c.Response().WriteHeader(http.StatusOK)
		return png.Encode(c.Response(), img)
	}
}

// transparentPixel は事前生成した 1x1 完全透明 PNG。disabled identicon に使う。
// init で 1 回だけ encode するので request 毎の cost は 0。
var transparentPixel = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0, G: 0, B: 0, A: 0})
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}()

func serveTransparentPixel(c echo.Context) error {
	c.Response().Header().Set("Content-Type", "image/png")
	c.Response().Header().Set("Cache-Control", "public, max-age=86400")
	c.Response().Header().Set("Content-Security-Policy", assetCSP)
	return c.Blob(http.StatusOK, "image/png", transparentPixel)
}

func generateIdenticon(rng *rand.Rand) *image.RGBA {
	actualSize := identiconSize - (identiconMargin * 2)
	cellSize := actualSize / identiconN
	sideN := identiconN / 2

	img := image.NewRGBA(image.Rect(0, 0, identiconSize, identiconSize))

	// グラデーション背景
	bgPair := gradientColors[rng.Intn(len(gradientColors))]
	for y := 0; y < identiconSize; y++ {
		for x := 0; x < identiconSize; x++ {
			// 左上→右下の線形グラデーション
			t := float64(x+y) / float64(2*identiconSize)
			r := lerp(bgPair[0].R, bgPair[1].R, t)
			g := lerp(bgPair[0].G, bgPair[1].G, t)
			b := lerp(bgPair[0].B, bgPair[1].B, t)
			img.Set(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}

	// サイドビットマップ生成 (rand(3) == 0 でon)
	side := make([][]bool, sideN)
	for x := range side {
		side[x] = make([]bool, identiconN)
		for y := range side[x] {
			side[x][y] = rng.Intn(3) == 0
		}
	}

	// 中央列
	center := make([]bool, identiconN)
	for i := range center {
		center[i] = rng.Intn(3) == 0
	}

	// 白いブロックを描画
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	for x := 0; x < identiconN; x++ {
		for y := 0; y < identiconN; y++ {
			isCenter := x == (identiconN-1)/2
			if isCenter && !center[y] {
				continue
			}
			isLeft := x < (identiconN-1)/2
			if isLeft && !side[x][y] {
				continue
			}
			isRight := x > (identiconN-1)/2
			if isRight && !side[sideN-(x-sideN)][y] {
				continue
			}

			ax := identiconMargin + (cellSize * x)
			ay := identiconMargin + (cellSize * y)
			fillRect(img, ax, ay, cellSize, cellSize, white)
		}
	}

	return img
}

func lerp(a, b uint8, t float64) uint8 {
	return uint8(float64(a)*(1-t) + float64(b)*t)
}

func fillRect(img *image.RGBA, x0, y0, w, h int, c color.RGBA) {
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			img.Set(x, y, c)
		}
	}
}
