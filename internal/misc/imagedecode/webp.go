package imagedecode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"

	"github.com/kovidgoyal/imaging"
	"github.com/rwcarlsen/goexif/exif"
	exif_tiff "github.com/rwcarlsen/goexif/tiff"
)

// ErrMalformedWebP reports that a WebP container could not be validated
// before decoding (truncated chunks, missing bitstream, or dimensions that
// disagree between the container and the bitstream).
var ErrMalformedWebP = errors.New("imagedecode: malformed webp container")

// webpChunk is one RIFF chunk: its FourCC and payload (without padding).
type webpChunk struct {
	typ     string
	payload []byte
}

// webpVP8X flag bits (libwebp の mux_types.h と同じ値)。
const (
	webpFlagAnimation = 0x02
	webpFlagEXIF      = 0x08
	webpFlagAlpha     = 0x10
	webpFlagICC       = 0x20
)

// decodeWebP decodes a WebP after checking every dimension the decoder will
// allocate for.
//
// **`image.DecodeConfig` が返すのは VP8X の canvas 寸法だけ。** 上の pixel
// cap はそれしか見ていないが、デコーダが確保するのはビットストリームの
// 寸法で、しかも imaging の `webp.DecodeAnimated` は ANMF の各コマを canvas と
// 照合せずに**全コマ**デコードして保持する。canvas 1x1 に 12000x12000 の
// VP8L コマを 2 枚入れた 144 バイトのファイルで 1.2GiB 確保した (実測)。
// media proxy は未認証で任意 URL の画像をここへ通すので、プロセスごと
// OOM で落とせる。
//
// 対策は GIF / APNG と同じ方針で、**アニメーションは 1 コマ目だけを静止
// WebP に組み直してデコードする** (mk-go はアニメーションを出力しないので
// 残りのコマは元から捨てている)。組み直す前に、コマが canvas に収まること
// と、ビットストリームの寸法が宣言と一致することを確かめる。
func decodeWebP(data []byte, maxPixels int64) (image.Image, error) {
	chunks, end, err := webpChunks(data)
	if err != nil {
		return nil, err
	}
	// **検査した範囲と同じバイト列だけをデコーダへ渡す。** imaging のメタデータ
	// 読み取り (`webpmeta.parseWebpExtended`) は RIFF の宣言長を見ずに EOF まで
	// 歩くので、宣言長より後ろに置いた EXIF チャンクの長さで 4GiB を確保した
	// (54 バイトのファイル、実測)。
	data = data[:end]
	if len(chunks) == 0 {
		return nil, fmt.Errorf("%w: no chunks", ErrMalformedWebP)
	}
	first := chunks[0]
	switch first.typ {
	case "VP8 ", "VP8L":
		// 単純形式。canvas はビットストリームそのもの。
		w, h, err := webpBitstreamDims(first.typ, first.payload)
		if err != nil {
			return nil, err
		}
		if err := checkWebPPixels(w, h, maxPixels); err != nil {
			return nil, err
		}
		return decodeCheckedWebP(data)
	case "VP8X":
	default:
		return nil, fmt.Errorf("%w: unexpected first chunk %q", ErrMalformedWebP, first.typ)
	}

	if len(first.payload) < 10 {
		return nil, fmt.Errorf("%w: short VP8X", ErrMalformedWebP)
	}
	flags := first.payload[0]
	canvasW := int(u24le(first.payload[4:7])) + 1
	canvasH := int(u24le(first.payload[7:10])) + 1
	if err := checkWebPPixels(canvasW, canvasH, maxPixels); err != nil {
		return nil, err
	}

	if flags&webpFlagAnimation == 0 {
		// 拡張形式の静止画。ビットストリームは canvas と同じ寸法でなければ
		// ならない (WebP 仕様)。食い違いを許すと canvas だけ小さく宣言した
		// 爆弾がそのまま通る。
		for _, c := range chunks[1:] {
			if c.typ != "VP8 " && c.typ != "VP8L" {
				continue
			}
			w, h, err := webpBitstreamDims(c.typ, c.payload)
			if err != nil {
				return nil, err
			}
			if w != canvasW || h != canvasH {
				return nil, fmt.Errorf("%w: bitstream %dx%d != canvas %dx%d",
					ErrMalformedWebP, w, h, canvasW, canvasH)
			}
			return decodeCheckedWebP(data)
		}
		return nil, fmt.Errorf("%w: no bitstream", ErrMalformedWebP)
	}

	still, rect, err := firstWebPFrame(chunks, canvasW, canvasH)
	if err != nil {
		return nil, err
	}
	img, err := decodeCheckedWebP(still)
	if err != nil {
		return nil, err
	}
	// 1 コマ目が canvas 全体を覆わないときは、imaging (`populate_from_webp`)
	// と同じく透明な canvas の上に置く。**EXIF の向きで回転した結果は寸法が
	// コマと合わないので置かずに返す** — 向き付きかつオフセット付きの
	// アニメーション WebP という稀な形で、canvas 上の位置が変わるだけ。
	if rect == image.Rect(0, 0, canvasW, canvasH) || img.Bounds().Size() != rect.Size() {
		return img, nil
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, canvasW, canvasH))
	draw.Draw(canvas, rect, img, img.Bounds().Min, draw.Src)
	return canvas, nil
}

// firstWebPFrame rebuilds the first ANMF frame of an animated WebP as a still
// WebP and reports where the frame sits on the canvas.
//
// 残すのは 1 コマ目の ALPH / VP8 / VP8L と、トップレベルの ICCP / EXIF。
// 色変換 (ICC → sRGB) と EXIF の向き補正を静止画と同じ経路で受けるため。
func firstWebPFrame(chunks []webpChunk, canvasW, canvasH int) ([]byte, image.Rectangle, error) {
	var iccp, exif, anmf *webpChunk
	for i := range chunks {
		c := &chunks[i]
		switch c.typ {
		case "ICCP":
			if iccp == nil {
				iccp = c
			}
		case "EXIF":
			if exif == nil {
				exif = c
			}
		case "ANMF":
			if anmf == nil {
				anmf = c
			}
		}
	}
	if anmf == nil {
		return nil, image.Rectangle{}, fmt.Errorf("%w: animated without ANMF", ErrMalformedWebP)
	}
	p := anmf.payload
	if len(p) < 16 {
		return nil, image.Rectangle{}, fmt.Errorf("%w: short ANMF", ErrMalformedWebP)
	}
	x := int(u24le(p[0:3])) * 2
	y := int(u24le(p[3:6])) * 2
	w := int(u24le(p[6:9])) + 1
	h := int(u24le(p[9:12])) + 1
	if x+w > canvasW || y+h > canvasH {
		return nil, image.Rectangle{}, fmt.Errorf("%w: frame %dx%d+%d+%d exceeds canvas %dx%d",
			ErrMalformedWebP, w, h, x, y, canvasW, canvasH)
	}
	sub, err := webpChunksAt(p, 16)
	if err != nil {
		return nil, image.Rectangle{}, err
	}
	var alph, bits *webpChunk
	for i := range sub {
		c := &sub[i]
		switch c.typ {
		case "ALPH":
			if alph == nil && bits == nil {
				alph = c
			}
		case "VP8 ", "VP8L":
			if bits == nil {
				bits = c
			}
		}
	}
	if bits == nil {
		return nil, image.Rectangle{}, fmt.Errorf("%w: frame without bitstream", ErrMalformedWebP)
	}
	bw, bh, err := webpBitstreamDims(bits.typ, bits.payload)
	if err != nil {
		return nil, image.Rectangle{}, err
	}
	// **ビットストリームの寸法は ANMF の宣言と一致しなければならない。**
	// デコーダはビットストリームの寸法で確保するので、ここを見ないと
	// 「宣言は canvas に収まるが中身は巨大」が通る。
	if bw != w || bh != h {
		return nil, image.Rectangle{}, fmt.Errorf("%w: frame bitstream %dx%d != declared %dx%d",
			ErrMalformedWebP, bw, bh, w, h)
	}
	if bits.typ == "VP8L" {
		// VP8L は自前でアルファを持つので ALPH は使われない。
		alph = nil
	}

	var body bytes.Buffer
	body.WriteString("WEBP")
	needVP8X := iccp != nil || exif != nil || alph != nil
	if needVP8X {
		var flags byte
		if iccp != nil {
			flags |= webpFlagICC
		}
		if exif != nil {
			flags |= webpFlagEXIF
		}
		if alph != nil {
			flags |= webpFlagAlpha
		}
		vp8x := make([]byte, 10)
		vp8x[0] = flags
		putU24le(vp8x[4:7], uint32(w-1))
		putU24le(vp8x[7:10], uint32(h-1))
		writeWebPChunk(&body, "VP8X", vp8x)
		// **EXIF を ICCP の直後 (ビットストリームより前) に置き、ICCP は偶数長に
		// する。** imaging のメタデータ読み取りはチャンクを飛ばすときにパディングを
		// 数えないので、奇数長の ICCP の後ろでは 1 バイトずれて EXIF を見失い、
		// 向きの補正が落ちる。ICC プロファイルは自分の長さを中に持つので、末尾に
		// 0 を 1 バイト足しても意味は変わらない。ずれを使った確保は
		// checkImagingWebPMetadata が別に止める。
		if iccp != nil {
			writeWebPChunk(&body, "ICCP", evenPayload(iccp.payload))
		}
		if exif != nil {
			writeWebPChunk(&body, "EXIF", exif.payload)
		}
		if alph != nil {
			writeWebPChunk(&body, "ALPH", alph.payload)
		}
	}
	writeWebPChunk(&body, bits.typ, bits.payload)
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes(), image.Rect(x, y, x+w, y+h), nil
}

// webpChunks walks the top-level chunks of a RIFF/WEBP file and returns them
// with the end offset of the RIFF container.
func webpChunks(data []byte) ([]webpChunk, int, error) {
	if !isWebP(data) {
		return nil, 0, fmt.Errorf("%w: not RIFF/WEBP", ErrMalformedWebP)
	}
	// RIFF の宣言長より後ろは見ない (末尾に付いたゴミをチャンクと読まない)。
	// 呼び出し側はこの範囲だけをデコーダへ渡す。
	end := len(data)
	if riffEnd := 8 + int(binary.LittleEndian.Uint32(data[4:8])); riffEnd >= 12 && riffEnd < end {
		end = riffEnd
	}
	chunks, err := webpChunksAt(data[:end], 12)
	return chunks, end, err
}

// webpChunksAt walks RIFF chunks in data starting at pos.
//
// **宣言長を信用しない。** 残りバイト数を毎回確かめ、越えたら壊れた
// ファイルとして扱う。
func webpChunksAt(data []byte, pos int) ([]webpChunk, error) {
	var out []webpChunk
	for pos < len(data) {
		if pos+8 > len(data) {
			return nil, fmt.Errorf("%w: truncated chunk header", ErrMalformedWebP)
		}
		typ := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		start := pos + 8
		// size < 0 は 32bit 環境で uint32 が int に収まらないとき。
		if size < 0 || size > len(data)-start {
			return nil, fmt.Errorf("%w: chunk %q overruns container", ErrMalformedWebP, typ)
		}
		out = append(out, webpChunk{typ: typ, payload: data[start : start+size]})
		pos = start + size + (size & 1)
	}
	return out, nil
}

// webpBitstreamDims reads the pixel dimensions from a VP8 or VP8L bitstream
// header.
func webpBitstreamDims(typ string, p []byte) (int, int, error) {
	switch typ {
	case "VP8L":
		// 1 バイトの signature (0x2f) の後ろに、14bit の width-1 と
		// 14bit の height-1 が little endian で続く。
		if len(p) < 5 || p[0] != 0x2f {
			return 0, 0, fmt.Errorf("%w: bad VP8L header", ErrMalformedWebP)
		}
		bits := binary.LittleEndian.Uint32(p[1:5])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	case "VP8 ":
		// 3 バイトの frame tag、start code 9d 01 2a、14bit の width と height
		// (上位 2bit はスケール指定)。
		if len(p) < 10 || p[3] != 0x9d || p[4] != 0x01 || p[5] != 0x2a {
			return 0, 0, fmt.Errorf("%w: bad VP8 header", ErrMalformedWebP)
		}
		w := int(binary.LittleEndian.Uint16(p[6:8]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(p[8:10]) & 0x3fff)
		if w == 0 || h == 0 {
			return 0, 0, fmt.Errorf("%w: zero VP8 dimensions", ErrMalformedWebP)
		}
		return w, h, nil
	}
	return 0, 0, fmt.Errorf("%w: unknown bitstream %q", ErrMalformedWebP, typ)
}

// checkWebPPixels applies the declared-pixel cap to one WebP raster.
func checkWebPPixels(w, h int, maxPixels int64) error {
	if int64(w)*int64(h) > maxPixels {
		return fmt.Errorf("%w: %dx%d", ErrTooManyPixels, w, h)
	}
	return nil
}

func u24le(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}

func putU24le(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
}

func writeWebPChunk(buf *bytes.Buffer, typ string, payload []byte) {
	buf.WriteString(typ)
	_ = binary.Write(buf, binary.LittleEndian, uint32(len(payload)))
	buf.Write(payload)
	if len(payload)%2 == 1 {
		buf.WriteByte(0)
	}
}

// decodeCheckedWebP hands data to imaging after confirming that imaging's
// metadata reader will not allocate more than the bytes it is given.
func decodeCheckedWebP(data []byte) (image.Image, error) {
	if err := checkImagingWebPMetadata(data); err != nil {
		return nil, err
	}
	decoded, _, err := imaging.DecodeAll(
		bytes.NewReader(data),
		imaging.AutoOrientation(false),
		imaging.ColorSpace(imaging.NO_CHANGE_OF_COLORSPACE),
	)
	if err != nil {
		return nil, err
	}

	// Normalize VP8 planes before imaging performs ICC conversion or EXIF
	// orientation. Those transforms allocate RGB images via image.Image.At;
	// doing them first would bake the studio-range values into RGB and make the
	// original YCbCr planes impossible to recover accurately.
	for _, frame := range decoded.Frames {
		frame.Image = normalizeLossyWebPRange(frame.Image)
	}
	if decoded.DefaultImage != nil {
		decoded.DefaultImage = normalizeLossyWebPRange(decoded.DefaultImage)
	}
	if err := applyWebPICC(decoded); err != nil {
		return nil, err
	}
	if err := applyWebPOrientation(decoded); err != nil {
		return nil, err
	}
	return decoded.SingleFrame(), nil
}

// applyWebPICC mirrors imaging's default sRGB conversion after the VP8 range
// normalization. WebP metadata supports ICCP but not CICP, so no CICP branch
// is needed here.
func applyWebPICC(decoded *imaging.Image) error {
	if decoded.Metadata == nil {
		return nil
	}
	profile, err := decoded.Metadata.ICCProfile()
	if err != nil || profile == nil {
		return err
	}
	for _, frame := range decoded.Frames {
		frame.Image, err = imaging.ConvertToSRGB(profile, imaging.Relative, true, frame.Image)
		if err != nil {
			return err
		}
	}
	return nil
}

// applyWebPOrientation mirrors imaging.AutoOrientation(true), but runs after
// the VP8 range and ICC conversions above.
func applyWebPOrientation(decoded *imaging.Image) error {
	if decoded.Metadata == nil {
		return nil
	}
	exifData, err := decoded.Metadata.Exif()
	if err != nil || exifData == nil {
		return err
	}
	tag, err := exifData.Get(exif.Orientation)
	if err != nil || tag == nil || tag.Format() != exif_tiff.IntVal {
		return nil
	}
	orientation, err := tag.Int(0)
	if err != nil {
		return nil
	}
	switch orientation {
	case 2:
		decoded.FlipH()
	case 3:
		decoded.Rotate180()
	case 4:
		decoded.FlipV()
	case 5:
		decoded.Transpose()
	case 6:
		decoded.Rotate270()
	case 7:
		decoded.Transverse()
	case 8:
		decoded.Rotate90()
	}
	return nil
}

// normalizeLossyWebPRange converts the studio-range YCbCr planes returned by
// the VP8 decoder into a straight-alpha NRGBA image. Eager conversion keeps
// downstream imaging operations on their optimized concrete-image paths;
// exposing the conversion through image.Image.At would allocate an interface
// value for every source pixel during resize.
//
// This normalization deliberately lives in the WebP decoder instead of a
// generic YCbCr helper: JPEG decoders have their own range semantics and must
// not be changed based on the behavior observed for VP8.
func normalizeLossyWebPRange(img image.Image) image.Image {
	var (
		ycbcr *image.YCbCr
		alpha *image.NYCbCrA
	)
	switch src := img.(type) {
	case *image.YCbCr:
		ycbcr = src
	case *image.NYCbCrA:
		ycbcr = &src.YCbCr
		alpha = src
	default:
		return img
	}
	b := ycbcr.Bounds()
	dst := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl := limitedYCbCrToRGB(
				ycbcr.Y[ycbcr.YOffset(x, y)],
				ycbcr.Cb[ycbcr.COffset(x, y)],
				ycbcr.Cr[ycbcr.COffset(x, y)],
			)
			a := uint8(255)
			if alpha != nil {
				a = alpha.A[alpha.AOffset(x, y)]
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i] = r
			dst.Pix[i+1] = g
			dst.Pix[i+2] = bl
			dst.Pix[i+3] = a
		}
	}
	return dst
}

// limitedYCbCrToRGB applies the BT.601 studio-range conversion used by VP8.
// The integer coefficients are the conventional 8-bit form of
// 1.164*(Y-16), 1.596*Cr, 0.392*Cb, 0.813*Cr and 2.017*Cb.
func limitedYCbCrToRGB(y, cb, cr uint8) (uint8, uint8, uint8) {
	c := int(y) - 16
	d := int(cb) - 128
	e := int(cr) - 128
	return clampWebPByte((298*c + 409*e + 128) >> 8),
		clampWebPByte((298*c - 100*d - 208*e + 128) >> 8),
		clampWebPByte((298*c + 516*d + 128) >> 8)
}

func clampWebPByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// checkImagingWebPMetadata replays how kovidgoyal/imaging (v1.8.21,
// `prism/meta/webpmeta.parseWebpExtended`) walks a WebP to collect ICC and
// EXIF data, and refuses input on which that walk would allocate a buffer
// larger than the remaining bytes.
//
// **imaging はチャンクの長さを検査せずに `make([]byte, length)` する。** しかも
// チャンクを飛ばすときにパディングを数えないので、こちらの走査 (仕様どおり
// パディングを飛ばす) とは別の位置を読む。奇数長のビットストリームの
// パディングを 'E' にして後ろに "XIF\0" 型のチャンクを置くと、imaging には
// 長さ 0x10000000 の EXIF に見えた (1MiB のファイルで 256MiB、実測)。
// 仕様側の検査では拾えないので、imaging と同じ歩き方をここで再現する。
func checkImagingWebPMetadata(data []byte) error {
	const vp8xStart = 20 // "RIFF" + size + "WEBP" + "VP8X" + size
	if len(data) < vp8xStart+10 || string(data[12:16]) != "VP8X" {
		return nil
	}
	if binary.LittleEndian.Uint32(data[16:20]) != 10 {
		return nil // imaging はここでエラーにして何も確保しない
	}
	flags := data[vp8xStart]
	hasICC := flags&webpFlagICC != 0
	hasEXIF := flags&webpFlagEXIF != 0
	if !hasICC && !hasEXIF {
		return nil
	}
	pos := vp8xStart + 10
	header := func() (string, int, bool) {
		if pos+8 > len(data) {
			return "", 0, false
		}
		typ := string(data[pos : pos+4])
		n := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		return typ, n, true
	}
	tooLong := func(typ string, n int) error {
		if n < 0 || n > len(data)-pos {
			return fmt.Errorf("%w: %s chunk of %d bytes overruns the file", ErrMalformedWebP, typ, n)
		}
		return nil
	}
	if hasICC {
		typ, n, ok := header()
		if !ok {
			return nil
		}
		if typ == "ICCP" {
			if err := tooLong(typ, n); err != nil {
				return err
			}
			pos += n
		}
	}
	if hasEXIF {
		for {
			typ, n, ok := header()
			if !ok {
				return nil
			}
			if typ == "EXIF" {
				return tooLong(typ, n)
			}
			if n < 0 || n > len(data)-pos {
				return nil // imaging の skip は EOF で止まる
			}
			pos += n
		}
	}
	return nil
}

// evenPayload returns p padded with a zero byte to an even length.
func evenPayload(p []byte) []byte {
	if len(p)%2 == 0 {
		return p
	}
	// 容量を長さで切った slice に append するので、必ず新しい配列へ複製される
	// (入力のバイト列を書き換えない)。長さ + 1 を自分で計算して確保しない。
	return append(p[:len(p):len(p)], 0)
}
