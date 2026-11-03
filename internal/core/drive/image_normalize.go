package drive

import (
	"image"
	"image/color"
	"image/draw"

	"github.com/kovidgoyal/imaging"
)

// detectionEdge は公式 sensitive-detector が受け付ける正規化済み画像の一辺
// (299x299、detector 側の maxImageWidth/Height と一致)。
const detectionEdge = 299

// NormalizeImageForDetection converts an uploaded image into the normalized
// form the official sensitive-detector expects: EXIF auto-orientation,
// 299x299 cover-crop resize, alpha flattened onto 18% gray, PNG encoding.
// upstream FileInfoService の sharp パイプライン
// (resize(299,299) → rotate() → flatten({r:119,g:119,b:119}) → png()) 相当。
func NormalizeImageForDetection(body []byte, mimeType string) ([]byte, error) {
	img, err := decodeForDetectionFunc(body, mimeType)
	if err != nil {
		return nil, err
	}
	return normalizeDecodedForDetection(img)
}

// decodeForDetectionFunc is the decoder NormalizeImageForDetection uses.
// テストでデコードの回数を数えるために差し替えられる。`ProcessAll` の
// `decodeForAltsFunc` と同じ `decodeImage` を指す (#3995)。
var decodeForDetectionFunc = decodeImage

// normalizeDecodedForDetection is NormalizeImageForDetection after the
// decode. アップロードの経路は `ProcessAll` がデコードした像をここへ渡し、
// 判定のためにもう一度デコードしない (#3995)。img は書き換えない。
func normalizeDecodedForDetection(img image.Image) ([]byte, error) {
	// sharp resize(299,299) の default fit は cover (アスペクト維持 + 中央
	// crop)。imaging.Fill が同じ挙動。withoutEnlargement:false = 小さい画像は
	// 拡大するが、Fill は常に指定サイズを出力するのでこれも一致する。
	img = imaging.Fill(img, detectionEdge, detectionEdge, imaging.Center, imaging.Lanczos)

	// 透過部分を 18% グレー rgb(119,119,119) で塗りつぶす (sharp flatten 相当)。
	flattened := image.NewNRGBA(image.Rect(0, 0, detectionEdge, detectionEdge))
	draw.Draw(flattened, flattened.Bounds(), image.NewUniform(color.NRGBA{R: 119, G: 119, B: 119, A: 255}), image.Point{}, draw.Src)
	draw.Draw(flattened, flattened.Bounds(), img, img.Bounds().Min, draw.Over)

	return encodePNG(flattened)
}
