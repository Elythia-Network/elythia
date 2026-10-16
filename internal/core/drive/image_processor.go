package drive

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"strings"

	"github.com/bbrks/go-blurhash"
	_ "github.com/gen2brain/avif" // AVIF input decode (mediaproxy と同じ wazero ベース)
	"github.com/gen2brain/webp"
	"github.com/kovidgoyal/imaging"

	"github.com/elythia-network/elythia/internal/misc/imagedecode"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// ProcessedImage holds the result of image transformation.
type ProcessedImage struct {
	Data     []byte
	MimeType string // e.g. "image/webp", "image/png"
}

// ImageProcessor handles thumbnail/webpublic generation and metadata
// extraction. テスト時はモックに差し替え可能。
type ImageProcessor interface {
	// GenerateThumbnail creates a thumbnail from the input image bytes.
	// Returns nil, nil if the input format is not supported.
	GenerateThumbnail(body []byte, mimeType string) (*ProcessedImage, error)

	// GenerateWebpublic creates a web-optimised version if needed.
	// Returns nil, nil if webpublic is not required.
	GenerateWebpublic(body []byte, mimeType string) (*ProcessedImage, error)

	// GetDimensions returns the pixel width and height of the image.
	GetDimensions(body []byte, mimeType string) (width, height int, err error)

	// CalculateBlurhash computes the BlurHash string for the image.
	CalculateBlurhash(body []byte, mimeType string) (string, error)
}

// VideoProcessor extracts thumbnails from video files.
type VideoProcessor interface {
	// GenerateThumbnail extracts a frame from the video and returns a
	// WebP thumbnail. Returns nil, nil if extraction fails or FFmpeg is
	// unavailable.
	//
	// ctx は ffmpeg の実行に渡る (#3037)。利用者が接続を切ったら外部
	// プロセスも止める。
	GenerateThumbnail(ctx context.Context, body []byte, mimeType string) (*ProcessedImage, error)
}

// Thumbnail / webpublic 生成パラメータ (Misskey TS 準拠)
const (
	thumbnailWidth  = 498
	thumbnailHeight = 422
	animThumbWidth  = 374
	animThumbHeight = 317
	webpublicMax    = 2048
	webpQuality     = 77
	blurhashSize    = 64
	blurhashXComp   = 5
	blurhashYComp   = 5
)

// isMimeImage returns true if the MIME type is a supported image format for
// thumbnail/webpublic generation.
func isMimeImage(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif",
		"image/webp", "image/bmp", "image/tiff",
		// IANA 公式名 (image/vnd.microsoft.icon) と古い慣例 (image/x-icon)
		// を両方許可。mediaproxy 側 isConvertibleImage と同じ alias を扱う
		// (#418)。
		"image/x-icon", "image/vnd.microsoft.icon",
		// APNG は 2 綴りある。判定器が改善されて `image/apng` を返すように
		// なった (#2319) ので両方受ける。片方だけだとサムネイルが生成されなく
		// なる (APNG の 1 フレーム目は valid な PNG なので stdlib で decode できる)。
		"image/vnd.mozilla.apng", "image/apng",
		// AVIF は gen2brain/avif (wazero) で decode できる。upstream は
		// sharp-convertible-image に含めており、webpublic を必ず WebP で作る。
		"image/avif":
		return true
	default:
		return false
	}
}

// isMimeVideo returns true if the MIME type represents a video format.
func isMimeVideo(mime string) bool {
	return strings.HasPrefix(mime, "video/")
}

// isAnimatedMime returns true if the MIME type is an animated image format
// (GIF or APNG). アニメーション画像はより小さいサムネイルを生成する。
func isAnimatedMime(mime string) bool {
	return mime == "image/gif" || mime == "image/vnd.mozilla.apng" || mime == "image/apng"
}

// ---------------------------------------------------------------------------
// DefaultImageProcessor — pure Go + gen2brain/webp (libwebp on wazero/WASM)
// ---------------------------------------------------------------------------

// DefaultImageProcessor implements ImageProcessor using pure Go libraries
// (disintegration/imaging) and gen2brain/webp for WebP encoding.
//
// gen2brain/webp は libwebp を WASM 化して wazero で実行するため cgo-free。
// Quality は int (0-100)、ただし 100 は lossless 扱いになる罠あり。本パッケージ
// は webpQuality = 77 固定なのでこの罠には踏み込まないが、定数を変える際は注意。
type DefaultImageProcessor struct{}

// NewDefaultImageProcessor creates a new DefaultImageProcessor.
func NewDefaultImageProcessor() *DefaultImageProcessor {
	return &DefaultImageProcessor{}
}

// decodeImage decodes image bytes into an image.Image. Auto-orients JPEG
// images using EXIF orientation data. golang.org/x/image の webp/bmp/tiff
// デコーダは import _ で init 登録済み。
func decodeImage(body []byte, mimeType string) (image.Image, error) {
	// 規則は `internal/misc/imagedecode` に 1 つだけ置いてある (#2925)。
	// EXIF orientation の自動補正と、インターレース truecolor PNG の
	// decoder バグ回避を含む。**ここを直接 imaging.Decode に戻さないこと** —
	// media proxy 側と食い違い、ローカルにアップロードされた画像だけが
	// 真っ黒なサムネイルを storage に焼く形になる。
	// **cap は upstream の sharp に揃える。** media proxy の 64MP をそのまま
	// 当てると、develop では通っていた 64MP 超の実写真 (102MP の中判、パノラマ
	// 合成、高解像度スキャン) がサムネイル・blurhash・寸法・**webpublic** を
	// 全部失う。webpublic が作られないと `GetPublicURL` が原本を指すので、
	// EXIF の GPS が公開側へ出る側に倒れる — 同じ PR の別コミットが塞いだ
	// ばかりの穴を、この cap で開け直すことになる。
	//
	// upstream は `limitInputPixels` を上書きしないので、アップロードで通る
	// 上限は sharp の既定 (0x3FFF^2) になる。ここを揃えても宣言寸法での爆弾
	// (46341^2 = 21 億画素) は引き続き弾ける。
	img, err := imagedecode.DecodeWithPixelCap(body, imagedecode.UpstreamMaxPixels)
	if err != nil {
		return nil, fmt.Errorf("unsupported image format: %s: %w", mimeType, err)
	}
	return img, nil
}

// webpEncoderFunc / pngEncoderFunc はテスト時に差し替え可能なエンコーダ。
var webpEncoderFunc = defaultEncodeWebP
var pngEncoderFunc = defaultEncodePNG

func defaultEncodeWebP(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, webp.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func defaultEncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	// bytes.Buffer.Write は常に成功するため png.Encode はエラーを返さない
	_ = png.Encode(&buf, img)
	return buf.Bytes(), nil
}

// encodeWebP encodes an image.Image to WebP bytes with the given quality.
func encodeWebP(img image.Image, quality int) ([]byte, error) {
	return webpEncoderFunc(img, quality)
}

// encodePNG encodes an image.Image to PNG bytes.
func encodePNG(img image.Image) ([]byte, error) {
	return pngEncoderFunc(img)
}

// resizeFit resizes img to fit within maxW x maxH while preserving aspect
// ratio. 元画像が maxW x maxH 以下の場合は拡大しない。
func resizeFit(img image.Image, maxW, maxH int) image.Image {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= maxW && h <= maxH {
		return img
	}
	return imaging.Fit(img, maxW, maxH, imaging.Lanczos)
}

// thumbnailBox returns the bounding box the thumbnail is fitted into.
// アニメーション画像はより小さい箱に収める。
func thumbnailBox(mimeType string) (int, int) {
	if isAnimatedMime(mimeType) {
		return animThumbWidth, animThumbHeight
	}
	return thumbnailWidth, thumbnailHeight
}

func encodeThumbnail(thumb image.Image) (*ProcessedImage, error) {
	data, err := encodeWebP(thumb, webpQuality)
	if err != nil {
		return nil, err
	}
	return &ProcessedImage{Data: data, MimeType: "image/webp"}, nil
}

// skipWebpublicBeforeDecode reports whether webpublic is never produced for
// this input, judged from the bytes alone.
//
// **アニメーションを静止画に潰してまで縮めない。** `encodeWebP` は 1 枚しか
// 受けないので、ここで作ると 1 コマだけの webpublic になり、`GetPublicURL` を
// 通す経路 (他人に見せる側) が静止画に化ける。upstream も `isAnimated` なら
// webpublic を作らない (`DriveService.ts` の `!isAnimated`)。
//
// **ただしメタデータがあるときは作る** (upstream より厳しい側)。アニメーションを
// 保つために撮影情報を残すのは割に合わない。**実際に効くのは APNG と
// アニメーション WebP だけ** — `hasStrippableMetadata` は GIF を見ない
// (`imagemeta.go` は JPEG / PNG / WebP / TIFF のみ) ので、GIF に対しては
// この条件が常に真になる。
//
// **WebP は MIME では判定できない (#3128)。** アニメーションも静止画も
// `image/webp` なので、`isAnimatedMime` では拾えずに 1 コマへ潰れていた。
// コンテナを歩く判定を足す。
//
// **animated AVIF はまだ拾えない。** ISOBMFF を歩いて複数フレームを見る必要が
// あり、WebP の RIFF 走査とは別物。しかも `needsWebpublic` が
// `mimeType != "image/avif"` を条件にしているため、AVIF は寸法にもメタデータにも
// 関係なく必ず作られる。
func skipWebpublicBeforeDecode(body []byte, mimeType string) bool {
	return (isAnimatedMime(mimeType) || imagedecode.IsAnimatedWebP(body)) &&
		!hasStrippableMetadata(body, mimeType)
}

// needsWebpublic reports whether a decoded image of the given size needs a
// webpublic variant.
//
// AVIF は Mastodon / MS Edge が表示できないため、寸法やメタデータに
// 関わらず必ず WebP の webpublic を作る (upstream DriveService の
// satisfyWebpublic は `type !== 'image/avif'` を条件に含めている)。
// SVG は現在未対応なのでここには来ない (isMimeImage で弾かれる)。
func needsWebpublic(body []byte, mimeType string, w, h int) bool {
	if mimeType == "image/avif" || hasStrippableMetadata(body, mimeType) {
		return true
	}
	return w > webpublicMax || h > webpublicMax
}

// encodeWebpublic encodes an already resized webpublic image.
// PNG は透過を維持するため PNG で出力、それ以外は WebP。
func encodeWebpublic(resized image.Image, mimeType string) (*ProcessedImage, error) {
	if mimeType == "image/png" {
		data, err := encodePNG(resized)
		if err != nil {
			return nil, err
		}
		return &ProcessedImage{Data: data, MimeType: "image/png"}, nil
	}
	data, err := encodeWebP(resized, webpQuality)
	if err != nil {
		return nil, err
	}
	return &ProcessedImage{Data: data, MimeType: "image/webp"}, nil
}

// blurhashOf computes the BlurHash of a decoded image.
func blurhashOf(img image.Image) string {
	// BlurHash 用に 64x64 にリサイズ
	small := imaging.Fit(img, blurhashSize, blurhashSize, imaging.Lanczos)

	// go-blurhash は image.Image を受け取る。NRGBA に変換しておく。
	bounds := small.Bounds()
	nrgba := image.NewNRGBA(bounds)
	draw.Draw(nrgba, bounds, small, bounds.Min, draw.Src)

	// blurhash.Encode は xComp/yComp が 1-9 の範囲内であれば常に成功する
	hash, _ := blurhash.Encode(blurhashXComp, blurhashYComp, nrgba)
	return hash
}

// fitDims returns the size `imaging.Fit(img, maxW, maxH, ...)` produces for a
// srcW x srcH source that does not already fit in the box.
//
// **途中の縮小からサムネイルを作るときに、寸法を元画像から決めるために使う。**
// `imaging.Fit` は縦横比を切り捨てで計算するので、2048 に縮めた像へ同じ
// `Fit` を掛けると、元画像から作った場合と 1px ずれることがある。
// `Fit` が高さ (幅) 0 を出したときに `Resize` が縦横比から 1px 以上へ
// 直す規則も、元画像の寸法で再現する。
func fitDims(srcW, srcH, maxW, maxH int) (int, int) {
	srcAspectRatio := float64(srcW) / float64(srcH)
	maxAspectRatio := float64(maxW) / float64(maxH)
	var newW, newH int
	if srcAspectRatio > maxAspectRatio {
		newW = maxW
		newH = int(float64(newW) / srcAspectRatio)
	} else {
		newH = maxH
		newW = int(float64(newH) * srcAspectRatio)
	}
	if newW == 0 {
		newW = int(math.Max(1.0, math.Floor(float64(newH)*float64(srcW)/float64(srcH)+0.5)))
	}
	if newH == 0 {
		newH = int(math.Max(1.0, math.Floor(float64(newW)*float64(srcH)/float64(srcW)+0.5)))
	}
	return newW, newH
}

// ImageAlts is everything ProcessAll derives from one decode.
type ImageAlts struct {
	Width, Height int
	Blurhash      string
	// Thumbnail / Webpublic are nil when not produced (best-effort).
	Thumbnail *ProcessedImage
	Webpublic *ProcessedImage
}

// BatchImageProcessor is an ImageProcessor that can derive all upload
// variants from a single decode.
//
// `processImage` はこれを実装していればこちらを使い、していなければ
// `ImageProcessor` の 4 つのメソッドを順に呼ぶ (テスト用の fake はそちら)。
//
// **`*DefaultImageProcessor` を具体型で埋め込んだ wrapper に注意。** `ProcessAll`
// が昇格するので、個別のメソッドを上書きしても `processImage` はそれを呼ばない。
// 個別のメソッドを差し替えたい wrapper は、interface の `ImageProcessor` を
// 埋め込む (emojiimport のテストの `webpublicOnlyProcessor` がこの形)。
type BatchImageProcessor interface {
	ImageProcessor
	// ProcessAll returns the dimensions, blurhash, thumbnail and webpublic
	// of the image. It returns nil when the input is not an image or cannot
	// be decoded (the upload itself is never rejected for that).
	ProcessAll(body []byte, mimeType string) *ImageAlts
}

var _ BatchImageProcessor = (*DefaultImageProcessor)(nil)

// decodeForAltsFunc is the decoder ProcessAll uses. テストで呼ばれた回数と
// 返したエラー (pixel cap の sentinel) を数えるために差し替えられる。
var decodeForAltsFunc = decodeImage

// ProcessAll decodes the image once and derives the dimensions, blurhash,
// thumbnail and webpublic from it. 結果は個別のメソッドと同じ (#3609)。
// サムネイルだけは、webpublic を縮小して作った場合にその縮小から作るので、
// 寸法と形式は同じで画素がわずかに違う。
func (p *DefaultImageProcessor) ProcessAll(body []byte, mimeType string) *ImageAlts {
	if !isMimeImage(mimeType) {
		return nil
	}
	// **デコードは 1 回だけ。** 以前は寸法・blurhash・サムネイル・webpublic が
	// それぞれ `decodeImage` を呼んでいた (#3609)。cap も同じ `decodeImage` を
	// 通るので、ピクセル数の上限は変わらない。読めない画像は全部を諦めるだけで、
	// アップロードは拒否しない (個別のメソッドでも 4 つとも失敗していた)。
	img, err := decodeForAltsFunc(body, mimeType)
	if err != nil {
		return nil
	}
	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	out := &ImageAlts{Width: srcW, Height: srcH}

	// blurhash は元の像から作る。サムネイルから作ると値が変わる。
	out.Blurhash = blurhashOf(img)

	thumbSrc := img
	if !skipWebpublicBeforeDecode(body, mimeType) && needsWebpublic(body, mimeType, srcW, srcH) {
		// サムネイルは 2048 の縮小から作る。4032→498 の Lanczos が 2048→498 に
		// なり、12MP で 132ms → 48ms (#3609)。
		//
		// **ここから先で元の像を読まない。** 元の像 (12MP の JPEG で約 18MB) は
		// webpublic のエンコード (WASM で数十 MB を確保する) の間に GC が回収
		// できる。読むと、1 回だけデコードする形のほうが個別に処理していたころより
		// 長く大きな像を抱えることになる。エンコードに入る時点の生存ヒープは
		// 個別の処理と同じだった (#3609 の計測)。
		thumbSrc = resizeFit(img, webpublicMax, webpublicMax)
		out.Webpublic, _ = encodeWebpublic(thumbSrc, mimeType)
	}

	tw, th := thumbnailBox(mimeType)
	thumb := thumbSrc
	if srcW > tw || srcH > th {
		w, h := fitDims(srcW, srcH, tw, th)
		thumb = imaging.Resize(thumbSrc, w, h, imaging.Lanczos)
	}
	// エンコードの失敗はサムネイルなしで続行 (個別のメソッドと同じ best-effort)。
	out.Thumbnail, _ = encodeThumbnail(thumb)
	return out
}

func (p *DefaultImageProcessor) GenerateThumbnail(body []byte, mimeType string) (*ProcessedImage, error) {
	if !isMimeImage(mimeType) {
		return nil, nil
	}
	img, err := decodeImage(body, mimeType)
	if err != nil {
		return nil, nil // デコード失敗はサムネイルなしで続行
	}

	tw, th := thumbnailBox(mimeType)
	return encodeThumbnail(resizeFit(img, tw, th))
}

func (p *DefaultImageProcessor) GenerateWebpublic(body []byte, mimeType string) (*ProcessedImage, error) {
	if !isMimeImage(mimeType) {
		return nil, nil
	}
	// **デコードより前に置く。** 判定はバイト列だけで決まるので、作らないと
	// 決まっている入力をデコードする必要が無い。
	if skipWebpublicBeforeDecode(body, mimeType) {
		return nil, nil
	}

	img, err := decodeImage(body, mimeType)
	if err != nil {
		return nil, nil
	}

	bounds := img.Bounds()
	if !needsWebpublic(body, mimeType, bounds.Dx(), bounds.Dy()) {
		return nil, nil
	}
	return encodeWebpublic(resizeFit(img, webpublicMax, webpublicMax), mimeType)
}

func (p *DefaultImageProcessor) GetDimensions(body []byte, mimeType string) (int, int, error) {
	if !isMimeImage(mimeType) {
		return 0, 0, fmt.Errorf("not an image: %s", mimeType)
	}
	img, err := decodeImage(body, mimeType)
	if err != nil {
		return 0, 0, err
	}
	bounds := img.Bounds()
	return bounds.Dx(), bounds.Dy(), nil
}

func (p *DefaultImageProcessor) CalculateBlurhash(body []byte, mimeType string) (string, error) {
	if !isMimeImage(mimeType) {
		return "", fmt.Errorf("not an image: %s", mimeType)
	}
	img, err := decodeImage(body, mimeType)
	if err != nil {
		return "", err
	}
	return blurhashOf(img), nil
}
