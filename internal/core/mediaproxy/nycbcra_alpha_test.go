package mediaproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kovidgoyal/imaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lossyAlphaWebP is a 128x128 lossy WebP (VP8X + ALPH + VP8, 4:2:0) whose left
// half is fully transparent red and right half an opaque gradient.
//
// **左右で alpha が分かれているのが要点** — `imaging.Clone` は行の先頭画素の
// alpha を行全体に適用するので、この形だと**画像が丸ごと透明になる**。
func lossyAlphaWebP(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "lossy-alpha.webp"))
	require.NoError(t, err)
	return data
}

// **`imaging.Clone` は NYCbCrA の alpha を行単位で壊す (#2925)。**
//
// `kovidgoyal/imaging` の scanner は、サブサンプル (4:2:0 / 4:2:2 / 4:4:0 =
// lossy WebP の通常形) の分岐で alpha の index を内側ループで進めない
// (`nrgba/scanner.go`)。結果、**行の全画素がその行の先頭画素の alpha**になる。
//
// `draw.Draw` / `At()` は逆に alpha は正しいが、premultiplied な値しか出さない
// ので**完全に透明な画素の RGB が 0 に潰れる**。plane を自分で読む
// `normalizeForResize` だけが両方とも正しく取れる。
func TestNormalizeForResize_KeepsPerPixelAlpha(t *testing.T) {
	img, err := decodeImage(lossyAlphaWebP(t), "image/webp")
	require.NoError(t, err)
	_, ok := img.(straightNRGBAImage)
	require.True(t, ok, "前提: lossy WebP は straight-alpha pixel access を提供する (got %T)", img)

	got := imaging.Clone(normalizeForResize(img))
	at := func(x, y int) uint8 { return got.Pix[got.PixOffset(x, y)+3] }

	// 左半分は透明、右半分は不透明。**行単位で潰れると右半分も 0 になる。**
	assert.EqualValues(t, 0, at(10, 64), "左半分は透明のまま")
	assert.EqualValues(t, 255, at(120, 64), "右半分の不透明が残ること")

}

// **透明部の RGB も保つ。** `draw.Draw` / `At()` 経由だと 0 に潰れる。
func TestNormalizeForResize_KeepsTransparentRGB(t *testing.T) {
	img, err := decodeImage(lossyAlphaWebP(t), "image/webp")
	require.NoError(t, err)

	got := imaging.Clone(normalizeForResize(img))
	i := got.PixOffset(10, 64)
	assert.Greater(t, int(got.Pix[i]), 128,
		"透明部の赤が残ること (At() 経由だと 0 になる)")
	assert.EqualValues(t, 0, got.Pix[i+3], "alpha は 0 のまま")
}

// **badge が丸ごと空にならないこと (#2925)。**
//
// `processBadge` が `normalizeForResize` を通さないと、上の alpha 破壊で
// 画像全体が透明になり、mask が全画素 0 になる。元画像の entropy は閾値より
// 上なので 404 にもならず、**見えないバッジを 200 で配る**。
func TestProcessBadge_LossyAlphaWebPIsNotEmpty(t *testing.T) {
	s := testService(nil)
	res, err := s.processBadge(lossyAlphaWebP(t), "image/webp")
	require.NoError(t, err)
	defer res.Body.Close()

	out := decodeBadge(t, res)
	nonZero := 0
	for i := 0; i < badgeSize*badgeSize; i++ {
		if out.Pix[i*4] > 16 {
			nonZero++
		}
	}
	assert.Greater(t, nonZero, badgeSize*badgeSize/20,
		"バッジが真っ黒になっている (alpha が行単位で潰れている)")
}
