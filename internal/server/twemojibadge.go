package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/labstack/echo/v4"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	xdraw "golang.org/x/image/draw"
)

// twemojiBadgeNamePattern mirrors upstream の `/^[0-9a-f-]+\.png$/`。
// 拡張子が .png でないもの (.svg 等) は 404 にする。
var twemojiBadgeNamePattern = regexp.MustCompile(`^[0-9a-f-]+\.png$`)

const (
	// badgeRenderSize is the size the SVG is rasterised at before processing.
	// upstream は density:1000 で読み込んでから 488x488 に resize する。
	badgeRenderSize = 488
	// badgePadding は upstream の extend({top:12, ...}) 相当。
	badgePadding = 12
	// badgeCanvasSize は 488 + 12*2 = 512。upstream の create({width:512}) と一致。
	badgeCanvasSize = badgeRenderSize + badgePadding*2
	// badgeOutputSize は最終的な出力サイズ (upstream: resize(96, 96))。
	badgeOutputSize = 96
	// badgeContrast は upstream の linear(1.75, -(128*1.75)+128) の傾き。
	badgeContrast = 1.75
)

// badgeCacheSize bounds how many rendered badges are kept in memory.
//
// 同梱の twemoji は 4000 件強で、1 件の PNG は数 KB なので全件でも数 MB。
// 4096 にしておけば実質 miss しない。
const badgeCacheSize = 4096

// twemojiBadgeHandler serves /twemoji-badge/:name.
type twemojiBadgeHandler struct {
	dir string
	// cache は生成済みの PNG。**`/api` の外なのでレート制限が掛からない**
	// (middleware は api グループにしか付いていない) 一方、1 リクエストあたり
	// SVG のラスタライズと 512x512 のピクセルループ 2 周で 15ms 前後かかる。
	// 名前空間は同梱ファイルで固定なので、素直に全部持ってよい。
	cache *lru.Cache[string, []byte]
	// mu は gen を守る。cache は自前で排他するが、番号を比べることと積むことを
	// 1 つのロックの中で行うために使う。
	mu sync.Mutex
	// gen は DropLocal で進める世代の番号 (#3776)。読み込みと描画はロックの外で
	// 走るので、捨てる前に読んだファイルの描画を、捨てた後に積まないために使う。
	// TTL が無いので、積むと差し替えたファイルを描き直さないまま残り続ける。
	gen uint64
}

func newTwemojiBadgeHandler(dir string) *twemojiBadgeHandler {
	// lru.New は size <= 0 でのみ error を返す。badgeCacheSize は正の定数。
	cache, _ := lru.New[string, []byte](badgeCacheSize)
	return &twemojiBadgeHandler{dir: dir, cache: cache}
}

// Serve renders a Twemoji SVG into the monochrome badge PNG used by Web Push.
//
// upstream ClientServerService の sharp パイプラインを再現する:
//
//	resize(488) -> greyscale -> normalise -> linear(1.75 コントラスト)
//	-> flatten(#000) -> extend(12px, #000) -> b-w
//	-> 512x512 の透明キャンバスと XOR 合成 -> resize(96) -> png
//
// 結果は「絵文字の形が抜けた黒い四角」で、通知バッジとして使われる。
func (h *twemojiBadgeHandler) Serve(c echo.Context) error {
	name := c.Param("*")
	if name == "" {
		name = c.Param("name")
	}
	if !twemojiBadgeNamePattern.MatchString(name) {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	// パターンで [0-9a-f-]+ に制限済みだが、念のため path traversal を潰す。
	base := strings.TrimSuffix(name, ".png")
	if base == "" || strings.Contains(base, "..") || strings.ContainsRune(base, filepath.Separator) {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	out, err := h.badge(base)
	if err != nil {
		return err
	}

	c.Response().Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	c.Response().Header().Set("Cache-Control", "max-age=2592000")
	return c.Blob(http.StatusOK, "image/png", out)
}

// badge returns the rendered PNG for base, rendering it on first use.
//
// **失敗はキャッシュしない。** ファイルが後から置かれる構成 (bind mount で
// 差し替える等) で、一度 404 になった名前が永久に 404 のままになる。
func (h *twemojiBadgeHandler) badge(base string) ([]byte, error) {
	if h.cache != nil {
		if out, ok := h.cache.Get(base); ok {
			return out, nil
		}
	}
	// 世代の番号はファイルを読む前に読む。
	h.mu.Lock()
	gen := h.gen
	h.mu.Unlock()
	svg, err := os.ReadFile(filepath.Join(h.dir, base+".svg"))
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusNotFound)
	}
	out, err := renderTwemojiBadge(svg)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError)
	}
	if h.cache != nil {
		h.mu.Lock()
		if h.gen == gen {
			h.cache.Add(base, out)
		}
		h.mu.Unlock()
	}
	return out, nil
}

// DropLocal drops the rendered badges held by this process (#3776), so the
// next request reads the file again. 他のプロセスへは伝えない。
func (h *twemojiBadgeHandler) DropLocal() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	if h.cache != nil {
		h.cache.Purge()
	}
}

// renderTwemojiBadge rasterises the SVG and applies the upstream pipeline.
func renderTwemojiBadge(svg []byte) ([]byte, error) {
	rendered, err := rasterizeSVG(svg, badgeRenderSize)
	if err != nil {
		return nil, err
	}

	// greyscale -> normalise -> contrast -> flatten(#000) を 1 パスで行う。
	// flatten は「透明部分を黒で塗る」なので、alpha で黒へ寄せれば等価。
	mask := image.NewGray(image.Rect(0, 0, badgeCanvasSize, badgeCanvasSize))
	lo, hi := greyRange(rendered)
	for y := 0; y < badgeRenderSize; y++ {
		for x := 0; x < badgeRenderSize; x++ {
			r, g, b, a := rendered.At(x, y).RGBA()
			// Rec.601 luma。sharp の greyscale と同じ係数。
			lum := (299*float64(r>>8) + 587*float64(g>>8) + 114*float64(b>>8)) / 1000
			lum = normalise(lum, lo, hi)
			// linear(1.75, -(128*1.75)+128)
			v := badgeContrast*lum - 128*badgeContrast + 128
			// flatten({background:'#000'}): 透明なほど黒に倒す。
			v *= float64(a>>8) / 255
			mask.SetGray(x+badgePadding, y+badgePadding, color.Gray{Y: clamp8(v)})
		}
	}
	// extend の余白は #000 のまま (Gray のゼロ値が黒)。

	// upstream は透明キャンバスと mask を boolean 'eor' (XOR) で合成する。
	// キャンバスは全ビット 0 なので、XOR の結果は mask の反転ではなく mask
	// そのもの… ではなく、b-w colorspace 上で 0 との XOR = 各ビット反転無し。
	// 実際に必要なのは「絵文字の形が抜けた黒い板」なので、明度を反転して
	// alpha に載せ替える。
	badge := image.NewNRGBA(image.Rect(0, 0, badgeCanvasSize, badgeCanvasSize))
	for y := 0; y < badgeCanvasSize; y++ {
		for x := 0; x < badgeCanvasSize; x++ {
			v := mask.GrayAt(x, y).Y
			badge.SetNRGBA(x, y, color.NRGBA{R: 0, G: 0, B: 0, A: 255 - v})
		}
	}

	small := image.NewNRGBA(image.Rect(0, 0, badgeOutputSize, badgeOutputSize))
	xdraw.CatmullRom.Scale(small, small.Bounds(), badge, badge.Bounds(), xdraw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, small); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rasterizeSVG renders an SVG document into an RGBA image of the given size.
func rasterizeSVG(svg []byte, size int) (*image.RGBA, error) {
	icon, err := oksvg.ReadIconStream(bytes.NewReader(svg))
	if err != nil {
		return nil, err
	}
	icon.SetTarget(0, 0, float64(size), float64(size))

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	scanner := rasterx.NewScannerGV(size, size, img, img.Bounds())
	icon.Draw(rasterx.NewDasher(size, size, scanner), 1.0)
	return img, nil
}

// greyRange returns the min/max luma of the opaque pixels, used to reproduce
// sharp's normalise() (stretch to full range).
func greyRange(img *image.RGBA) (lo, hi float64) {
	lo, hi = 255, 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			if a>>8 == 0 {
				continue
			}
			lum := (299*float64(r>>8) + 587*float64(g>>8) + 114*float64(bl>>8)) / 1000
			if lum < lo {
				lo = lum
			}
			if lum > hi {
				hi = lum
			}
		}
	}
	if hi <= lo {
		return 0, 255
	}
	return lo, hi
}

func normalise(v, lo, hi float64) float64 {
	if hi <= lo {
		return v
	}
	return (v - lo) * 255 / (hi - lo)
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v)
}
