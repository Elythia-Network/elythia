// Package imagedecode centralises the image decoding rules shared by the media
// proxy and the drive image processor.
//
// **2 箇所で同じ decode をしているので、規則はここに 1 つだけ置く (#2925)。**
// `internal/core/mediaproxy` は `internal/core/drive` を import しているため
// 逆向きには依存できず、片方だけ直すと**もう片方に同じバグが残る**。実際
// #2925 の初版がそれで、リモート URL の直 fetch だけが直り、ローカルに
// アップロードされた画像は真っ黒なサムネイルを storage に焼いたままだった。
package imagedecode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"

	"github.com/kovidgoyal/imaging"
)

// ErrTooManyPixels reports that the header declares more pixels than
// MaxPixels, so the image was refused **before** allocating its raster.
var ErrTooManyPixels = errors.New("imagedecode: declared dimensions exceed the pixel cap")

// ErrEncodedTooLarge reports that a WASM-backed decoder was asked to read more
// encoded bytes than SandboxedDecoderMaxBytes.
var ErrEncodedTooLarge = errors.New("imagedecode: encoded input is too large for the sandboxed decoder")

// SandboxedDecoderMaxBytes caps the encoded bytes handed to the wazero-backed
// decoders (AVIF / HEIC / HEIF / JPEG XL / WebP).
//
// **これらのデコーダは wasm の中で動き、上限を持たない (#3037)。**
// `gen2brain/*` は `wazero.NewRuntime(context.Background())` を package 内の
// `sync.Once` で 1 度だけ作る。`WithMemoryLimitPages` は未指定なので linear
// memory の上限は wazero の既定 **4GiB**、`WithCloseOnContextDone` も未使用
// なのでリクエストの context が届かず、**遅いデコードを中断できない**。
// runtime は package 内の変数で、**呼び出し側から設定を差し込む口が無い**
// (v0.4.4 で実測)。本番は `-tags nodynamic` なので必ずこの WASM 経路を通る。
//
// mk-go 側で握れるのは「何を、何バイト、何本同時に渡すか」だけ:
//
//   - 宣言寸法の cap (`MaxPixels` / `UpstreamMaxPixels`) が、確保の主項である
//     ラスタの大きさを縛る
//   - 同時実行枠 (`mediaproxy.acquireCPU` / `drive.acquireMediaSlot`) が、
//     同時に走る本数を縛る
//   - この定数が、wasm へ写す入力そのものを縛る
//
// **それでも「wasm 内部の上限」は無いまま**なので、細工したヘッダで
// デコーダに大きな確保をさせる余地は残る。消すには wazero の runtime を
// こちら側で作る必要があり、依存ライブラリを fork しない限りできない。
// `docs/divergence.md` に残存リスクとして記録してある。
//
// 32MiB は media proxy のダウンロード上限と同じ。実写真はこれをはるかに
// 下回る (iPhone の HEIC が 1-5MB、AVIF はさらに小さい) ので、**通る画像の
// 集合は実質変わらない**。超えたものはデコードしないだけで、アップロード
// 自体は従来どおり通る。
const SandboxedDecoderMaxBytes = 32 << 20

// MaxPixels caps width*height as declared by the image header.
//
// **デコードの前に見るのが要点。** デコーダはヘッダの寸法だけでラスタを確保する
// ので、デコード後に測っても遅い。BMP は 54 バイトのヘッダで 8.6GB を要求でき、
// PNG は zlib の圧縮率 1032:1 で 12MB から 65535x65535 (NRGBA 17GB) を作れる。
// **Go の大確保失敗は `throw("out of memory")` で recover できない**ので、
// echo の Recover ミドルウェアは効かずプロセスごと落ちる。
//
// 値は mediaproxy が従来デコード後に使っていた cap (8192x8192 = 64MP) と同じ。
// **media proxy については判定の位置を変えただけ**で、通る画像の集合は変わらない。
//
// **drive は別の cap を使う (`DecodeWithPixelCap`)。** あちらは develop の時点で
// cap が無かったので、この値をそのまま当てると**今まで通っていた 64MP 超の
// 実写真 (102MP の中判、パノラマ合成、高解像度スキャン) がサムネイル・
// blurhash・寸法・webpublic を全部失う**。webpublic が作られないと
// `GetPublicURL` が原本を指すので、EXIF の GPS が公開側へ出る側に倒れる。
var MaxPixels int64 = 8192 * 8192

// UpstreamMaxPixels mirrors sharp's default `limitInputPixels` (0x3FFF^2)。
//
// upstream Misskey は `DriveService` / `ImageProcessingService` のどちらでも
// `limitInputPixels` を上書きしていないので、アップロード経路で通る画像の
// 上限はこの値になる。drive をこれに揃えると「upstream で通る写真が mk-go で
// だけ劣化する」形を作らずに、宣言寸法での爆弾 (46341^2 = 21 億画素) は
// 引き続き弾ける。
const UpstreamMaxPixels int64 = 0x3FFF * 0x3FFF

// Decode decodes image bytes, working around a decoder bug for a narrow class
// of PNG.
//
// 既定は `imaging.Decode` — EXIF の向きを補正し、ICC/CICP を sRGB へ変換し、
// `import _` で登録済みの webp/bmp/tiff も読める。
func Decode(data []byte) (image.Image, error) {
	return DecodeWithPixelCap(data, MaxPixels)
}

// DecodeWithPixelCap is Decode with an explicit declared-pixel ceiling.
//
// **呼び出し側で上限が違う。** media proxy はリモートの任意 URL を未認証で
// 引く経路なので厳しく (64MP)、drive は認証済みのアップロードで
// `maxFileSize` と同時実行枠にも縛られるので upstream と同じ値にする。
func DecodeWithPixelCap(data []byte, maxPixels int64) (image.Image, error) {
	// **wasm のデコーダへ渡す前に入力の大きさを見る (#3037)。**
	// 下の `image.DecodeConfig` は**ヘッダを読むためだけでも wasm を起動して
	// 入力を丸ごと linear memory へ写す**ので、この判定はその前に置く。
	if ExceedsSandboxedDecoderSize(data) {
		return nil, fmt.Errorf("%w: %d bytes", ErrEncodedTooLarge, len(data))
	}
	// EXIF / ICC は imaging がデコードの途中で宣言値のまま確保する。どの形式でも
	// imaging へ渡す前に見る (checkEmbeddedMetadata の doc)。
	if err := checkEmbeddedMetadata(data); err != nil {
		return nil, err
	}
	// **ラスタを確保する前にヘッダの寸法を見る。** `image.DecodeConfig` は
	// 登録済みデコーダのヘッダだけを読むので、ここで弾けば巨大な確保が起きない。
	//
	// **読めなかったら通す。** TGA のように magic bytes を持たない形式や、
	// `image.RegisterFormat` されていない形式はここで判定できない。判定できない
	// ことを理由に拒否すると、これまで読めていた画像が読めなくなる (この変更は
	// 通る集合を変えないという前提で入れている)。その場合は従来どおり
	// デコード後の cap が受け止める。
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		px := int64(cfg.Width) * int64(cfg.Height)
		if px > maxPixels {
			return nil, fmt.Errorf("%w: %dx%d", ErrTooManyPixels, cfg.Width, cfg.Height)
		}
		// **画素数だけでは確保量が決まらない (#3037)。** 16bit の PNG は
		// `image.NRGBA64` へ展開されるので**1 画素 8 バイト**、8bit の倍を
		// 食う。cap ちょうどの 64MP なら 512MB で、同時実行枠 (既定
		// `GOMAXPROCS/2`) が 4 あれば 2GB を超える。cap を通っているので
		// 今までは何も止めなかった。
		//
		// 予算は「cap ちょうどの 8bit 画像」= `maxPixels * 4` バイト。
		// 8bit の画像にとっては従来と同じ判定で、**通る集合は変わらない**。
		bpp := decodedBytesPerPixelFor(data, cfg.ColorModel)
		if px*bpp > maxPixels*rasterBytesPerPixelBaseline {
			return nil, fmt.Errorf("%w: %dx%d at %d bytes/pixel",
				ErrTooManyPixels, cfg.Width, cfg.Height, bpp)
		}
	}
	// **アニメーションは 1 コマだけ読む。** `imaging.Decode` は内部で
	// `DecodeAll` を呼び、GIF なら `gif.DecodeAll`、APNG なら
	// `apng.DecodeAll` で**全コマをメモリに載せてから** `SingleFrame()` で
	// 1 枚だけ返す。mk-go はアニメーションを出力しない (`encodeWebP` は
	// `image.Image` 1 枚しか受けない) ので、残りは確保した直後に捨てている。
	//
	// 上の pixel cap は**1 コマぶん**しか見ないので、ここを塞がないとコマ数で
	// 掛け算できる。GIF のコマは LZW 圧縮で、透明な全画面コマは極端に縮むため、
	// 小さなファイルから数十 GB を要求できる。
	//
	// **GIF は常にこの経路。** imaging の `gifmeta` は `HasFrames: true` を
	// 無条件に立てるので、1 コマの GIF でも `DecodeAll` に入る。
	if isGIF(data) {
		img, err := gif.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		// imaging の `SingleFrame()` は `NormalizeOrigin` を通した像を返す。
		// GIF のコマは原点がずれうるので、揃えないと**返る像の Bounds.Min が
		// 経路によって変わる**。
		return normalizeOrigin(img), nil
	}
	if isAnimatedPNG(data) {
		// **アニメーション用のチャンクだけ落として imaging に渡す。**
		//
		// `png.Decode` で済ませると 1 コマ目は取れるが、**ICC / CICP →
		// sRGB 変換が落ちて色が変わる** (実測で Display P3 の APNG が
		// R チャンネル 32/255 ずれた)。カスタム絵文字は APNG がよく使われる
		// ので、静止画化のたびに色が変わるのは受け入れられない。
		//
		// `acTL` を落とせば imaging の `pngmeta` は `HasFrames` を立てず、
		// 単一画像の枝 (`apng.Decode` → `fix_colors` → `fix_orientation`) を
		// 通る。フレームの増幅は起きないまま、色の扱いは普通の PNG と同じに
		// なる。**捨てるのは `acTL` / `fcTL` / `fdAT` の 3 種だけ**で、
		// `iCCP` / `cICP` / `eXIf` は残す。
		if still := stripAPNGAnimation(data); still != nil {
			return imaging.Decode(bytes.NewReader(still), imaging.AutoOrientation(true))
		}
		// 書き換えられなかった (壊れている) ときは stdlib で 1 コマ目だけ読む。
		return png.Decode(bytes.NewReader(data))
	}
	if IsBrokenInterlacedPNG(data) {
		// **stdlib で読む。** 下記の条件の PNG は imaging が全画素 0 にする。
		// **EXIF の向きと ICC→sRGB 変換は失われる**が、代わりに得られるのは
		// 「真っ黒な画像」なので、そちらの方がましという判断。
		return png.Decode(bytes.NewReader(data))
	}
	if isWebP(data) {
		return decodeWebP(data, maxPixels)
	}
	return imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
}

// usesSandboxedDecoder reports whether data would be decoded by one of the
// wazero-backed decoders.
//
// **magic bytes だけで見る。** `image.DecodeConfig` に判定させると、その
// 判定自体が wasm を起動してしまう (塞ごうとしている経路そのもの)。
func usesSandboxedDecoder(data []byte) bool {
	return isISOBMFFImage(data) || isJPEGXL(data) || isWebP(data)
}

// ExceedsSandboxedDecoderSize reports whether data would be refused by
// DecodeWithPixelCap purely because of its encoded size.
//
// **呼び出し側が「デコードできない」を握り潰せないようにするための述語
// (#3037 レビュー 2 周目)。** drive は代替画像の生成を best-effort にして
// いるが、webpublic が作られないと `GetPublicURL` が原本を指すので、
// **EXIF の GPS がそのまま公開側へ出る**。この上限は #3037 が新しく入れた
// ものなので、それで落ちる入力だけは「作れないなら受け取らない」に倒せる
// ように、判定だけを切り出してある。
func ExceedsSandboxedDecoderSize(data []byte) bool {
	return len(data) > SandboxedDecoderMaxBytes && usesSandboxedDecoder(data)
}

// sandboxedISOBMFFBrands are the `ftyp` major brands decoded by gen2brain/avif
// and gen2brain/heic.
var sandboxedISOBMFFBrands = map[string]struct{}{
	// AVIF (still / sequence)。
	"avif": {}, "avis": {},
	// HEIC / HEIF。`mif1` / `msf1` は汎用の image item / sequence brand で、
	// iPhone の HEIC はこちらを名乗ることがある。
	"heic": {}, "heix": {}, "hevc": {}, "hevx": {},
	"heim": {}, "heis": {}, "hevm": {}, "hevs": {},
	"mif1": {}, "msf1": {},
}

// isISOBMFFImage reports whether data is an ISOBMFF file whose major brand is
// one of the sandboxed image brands.
//
// 形は `<4 byte size><"ftyp"><4 byte major brand>`。
func isISOBMFFImage(data []byte) bool {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return false
	}
	_, ok := sandboxedISOBMFFBrands[string(data[8:12])]
	return ok
}

// isJPEGXL reports whether data is a JPEG XL codestream or container.
func isJPEGXL(data []byte) bool {
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0x0A {
		return true // 素の codestream
	}
	// ISOBMFF 風のコンテナ。**判定は登録側の magic に合わせる** —
	// `gen2brain/jpegxl` が `image.RegisterFormat` するのは `????JXL`
	// (7 バイト、末尾スペース無し) なので、`JXL ` + `0D0A870A` まで要求すると
	// **1 バイト違う入力が guard を抜けて wasm へ全量渡る** (#3037 レビュー)。
	return len(data) >= 7 && string(data[4:7]) == "JXL"
}

// isWebP reports whether data is a RIFF/WEBP file.
//
// **WebP も対象にする。** `gen2brain/webp` と `golang.org/x/image/webp` の
// 両方が "webp" を `image.RegisterFormat` するので、どちらが引かれるかは
// package の初期化順に依存する。前者を引いたときだけ無防備、という形を
// 残さない。
func isWebP(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

// rasterBytesPerPixelBaseline is the bytes/pixel that MaxPixels was sized for.
//
// 8bit の RGBA / NRGBA が 4 バイト。`MaxPixels` はこの前提で決めた値なので、
// バイト予算に読み替えるときも同じ係数を使う。
const rasterBytesPerPixelBaseline int64 = 4

// decodedBytesPerPixelFor is decodedBytesPerPixel with the encoded bytes at
// hand, so it can correct the cases where the reported color model lies.
//
// **`image.DecodeConfig` は PNG の `tRNS` を読まない (#3037 レビューで実測)。**
// 非パレット画像では `tRNS` に当たる前に break するので、グレースケールは
// `tRNS` の有無に関わらず `Gray` / `Gray16` と報告される。ところがデコーダは
// `tRNS` があると**アルファ付きへ展開する** — 16bit グレースケールは
// `image.NRGBA64` (1 画素 8 バイト、報告値の 4 倍)、8bit は `image.NRGBA`
// (4 バイト、報告値の 4 倍)。
//
// 実測: 8x8 の Gray16 PNG に `tRNS` を 1 つ足すと、`DecodeConfig` は
// `Gray16Model` のままで `imaging.Decode` の戻りが `*image.NRGBA64` になる。
// cap ちょうどの 64MP なら 512MB、drive の 268MP なら 2GiB の単一確保で、
// **バイト予算を入れた意味がそこだけ消えていた**。
//
// 真偽値ではなく実際の確保量を見積もるので、ここを直しても 8bit の普通の
// 画像 (tRNS 無し) の判定は変わらない。
func decodedBytesPerPixelFor(data []byte, m color.Model) int64 {
	bpp := decodedBytesPerPixel(m)
	switch m {
	case color.GrayModel, color.Gray16Model:
		// グレースケールだけが報告値と食い違う。truecolor は
		// `tRNS` があっても RGBA / NRGBA64 で同じバイト数になる。
		if isPNG(data) && hasChunk(data, "tRNS") {
			if m == color.Gray16Model {
				return 8
			}
			return 4
		}
	}
	return bpp
}

// isPNG reports whether data starts with the PNG signature.
func isPNG(data []byte) bool {
	const sig = "\x89PNG\r\n\x1a\n"
	return len(data) >= 8 && string(data[:8]) == sig
}

// decodedBytesPerPixel estimates the per-pixel cost of the decoded raster.
//
// **`image.DecodeConfig` が返す色モデルで決まる。** 16bit の PNG は
// `NRGBA64` / `RGBA64` / `Gray16` になり、素の Go デコーダがその形で
// ラスタを確保する。
//
// **分からないものは 4 と見なす** (YCbCr / CMYK / パレット)。どれも 4 を
// 超えないので、既定へ倒しても予算を踏み越えない。
func decodedBytesPerPixel(m color.Model) int64 {
	switch m {
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	case color.Gray16Model:
		return 2
	case color.GrayModel:
		return 1
	default:
		return 4
	}
}

// isGIF reports whether data starts with a GIF signature.
func isGIF(data []byte) bool {
	return len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a")
}

// isAnimatedPNG reports whether data is a PNG carrying an `acTL` chunk.
//
// `acTL` は仕様上 `IDAT` より前に置かれる。`hasChunk` は `IDAT` で走査を
// 止めるので、**アニメーションでない PNG を誤って stdlib 経路へ落とさない**
// (あちらは ICC→sRGB 変換を失う)。
func isAnimatedPNG(data []byte) bool {
	const sig = "\x89PNG\r\n\x1a\n"
	if len(data) < 8 || string(data[:8]) != sig {
		return false
	}
	return hasChunk(data, "acTL")
}

// normalizeOrigin moves an image's bounds to (0, 0) without copying pixels
// when the concrete type allows it.
//
// `kovidgoyal/imaging` の `NormalizeOrigin` と同じことをする。GIF の 1 コマ目は
// 論理画面の中でずれた位置に置けるので、揃えないと `Bounds().Min` が経路に
// よって変わる。
func normalizeOrigin(src image.Image) image.Image {
	r := src.Bounds()
	if r.Min.X == 0 && r.Min.Y == 0 {
		return src
	}
	if p, ok := src.(*image.Paletted); ok {
		// Pix / Stride はそのままでよい。`PixOffset` は Rect.Min からの
		// 相対で計算されるので、Rect を移すだけで整合する。
		dst := *p
		dst.Rect = image.Rect(0, 0, r.Dx(), r.Dy())
		return &dst
	}
	dst := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(dst, dst.Bounds(), src, r.Min, draw.Src)
	return dst
}

// apngAnimationChunks are the chunk types that make a PNG an APNG.
var apngAnimationChunks = map[string]struct{}{
	"acTL": {}, "fcTL": {}, "fdAT": {},
}

// stripAPNGAnimation rewrites an APNG as a plain still PNG.
//
// 返すのは `acTL` / `fcTL` / `fdAT` を取り除いたバイト列。**チャンクの CRC は
// そのまま使える** — 落とすだけで中身は触らないため。走査中に形が合わなければ
// nil を返して呼び出し側の fallback に任せる。
//
// **`IDAT` で止めない。** `fcTL` は IDAT の前にも後ろにも現れる。
func stripAPNGAnimation(data []byte) []byte {
	const sigLen = 8
	if len(data) < sigLen {
		return nil
	}
	out := make([]byte, 0, len(data))
	out = append(out, data[:sigLen]...)

	pos := sigLen
	dropped := false
	for {
		if pos == len(data) {
			break
		}
		if pos+8 > len(data) {
			return nil
		}
		length := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		if length < 0 {
			return nil
		}
		end := pos + 12 + length // length + type + data + CRC
		if end < pos || end > len(data) {
			return nil
		}
		typ := string(data[pos+4 : pos+8])
		if _, drop := apngAnimationChunks[typ]; drop {
			dropped = true
		} else {
			out = append(out, data[pos:end]...)
		}
		pos = end
		if typ == "IEND" {
			break
		}
	}
	if !dropped {
		return nil
	}
	return out
}

// IsBrokenInterlacedPNG reports whether data is a PNG that
// `kovidgoyal/imaging` v1.8.21 decodes to all-zero pixels **without returning
// an error**.
//
// **条件は 3 つすべて。** colorType=2 (truecolor) / bit depth=8 / tRNS 無し、
// かつインターレース (Adam7)。このときだけ imaging は独自の `*nrgb.Image` を
// 選ぶ (`apng/reader.go` の `cbTC8 && !useTransparent`) が、その Adam7 の
// pass 合成に `*nrgb.Image` の case が無く、stride が 0 のまま no-op copy に
// なる。gray / gray+alpha / palette / RGBA / 16bit RGB / tRNS 付き truecolor は
// いずれも別の型になり正しく読める (34 通りの組み合わせで実測)。
//
// **条件を広げてはいけない。** インターレースだけで判定すると、壊れていない
// 種別まで stdlib へ回して ICC→sRGB 変換を落とす (Display P3 の PNG で
// チャンネル差が最大 31/255 出る、実測)。
func IsBrokenInterlacedPNG(data []byte) bool {
	const sig = "\x89PNG\r\n\x1a\n"
	// signature 8 + length 4 + type 4 + IHDR 13 = 29 バイト。
	// IHDR は bitDepth(24) / colorType(25) / interlace(28)。
	if len(data) < 29 || string(data[:8]) != sig || string(data[12:16]) != "IHDR" {
		return false
	}
	if data[24] != 8 || data[25] != 2 || data[28] != 1 {
		return false
	}
	return !hasChunk(data, "tRNS")
}

// hasChunk walks the PNG chunk list looking for typ. **長さを信用して読み飛ばす
// のではなく、毎回残りバイト数を確かめる** — 細工した length で範囲外を読まない
// ようにするため。
func hasChunk(data []byte, typ string) bool {
	pos := 8 // signature の後ろから
	for {
		// length 4 + type 4 + data + CRC 4
		if pos+8 > len(data) {
			return false
		}
		// **`length < 0` の検査は要らない。** byte を 24 bit 左シフトしても
		// int (64bit) では正のままなので到達しない。範囲外は下の
		// `next > len(data)` で止める。
		length := int(data[pos])<<24 | int(data[pos+1])<<16 | int(data[pos+2])<<8 | int(data[pos+3])
		if string(data[pos+4:pos+8]) == typ {
			return true
		}
		if string(data[pos+4:pos+8]) == "IDAT" {
			// tRNS は IDAT より前に来る (PNG 仕様)。以降は読む必要が無い。
			return false
		}
		next := pos + 12 + length
		if next <= pos || next > len(data) {
			return false
		}
		pos = next
	}
}

// IsAnimatedWebP reports whether data is a WebP whose RIFF container declares
// animation.
//
// **MIME では判定できない。** アニメーション WebP も静止 WebP も `image/webp`
// なので、コンテナを歩いて `VP8X` の ANIMATION フラグか `ANIM` / `ANMF` チャンクを
// 見るしかない (GIF / APNG は MIME が形式そのものを表すので判定が要らない)。
//
// **デコードしない。** ここで必要なのは「アニメーションか」だけなので、
// ヘッダを読むに留める (デコードするときは `decodeWebP` が 1 コマ目だけを
// 静止 WebP に組み直す)。
//
// 歩き方は `internal/core/drive/imagemeta.go` の `webpHasMetadata` と同じ。
// チャンクは偶数境界に揃えられ、長さが container を越えたら打ち切る。
func IsAnimatedWebP(data []byte) bool {
	if !isWebP(data) {
		return false
	}
	pos := 12 // "RIFF" + size + "WEBP"
	for {
		if pos+8 > len(data) {
			return false
		}
		typ := string(data[pos : pos+4])
		// `size < 0` の検査は要らない。uint32 を int (64bit) にしても正のまま
		// なので到達しない (同じファイルの `hasChunk` と同じ判断)。
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		switch typ {
		case "ANIM", "ANMF":
			return true
		case "VP8X":
			// flags は payload の 1 バイト目。libwebp の ANIMATION_FLAG は 0x02。
			//
			// **宣言長が 1 以上であることを要求する。** 0 だと `data[pos+8]` は
			// VP8X の payload ではなく**次のチャンクの 4CC の 1 バイト目**になる。
			// `VP8 ` / `VP8L` / `VP8X` はどれも `'V'` (0x56) で始まり 0x02 の
			// ビットが立っているので、静止 WebP を「アニメーション」と誤判定する。
			// **倒れる側が安全でない** — 誤検出は「webpublic を作らない」= 公開側が
			// 原本を指す方向に倒れ、同じ細工は `webpHasMetadata` の走査も同時に
			// 潰すので、メタデータの除去ごと外れる。WebP 仕様では VP8X の
			// chunk size は 10 固定。
			if size >= 1 && pos+9 <= len(data) && data[pos+8]&0x02 != 0 {
				return true
			}
		}
		// チャンクは偶数境界に揃えられる。`size >= 0` なので `next > pos` は
		// 常に真 (= 無限ループにならない)。container を越えたら次の周回の
		// `pos+8 > len(data)` が打ち切る。
		pos += 8 + size + (size & 1)
	}
}
