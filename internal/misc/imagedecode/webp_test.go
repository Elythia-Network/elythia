package imagedecode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"os"
	"runtime"
	"testing"

	gwebp "github.com/gen2brain/webp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vp8lHeader builds a VP8L bitstream that carries only a header declaring
// w x h. デコード前に拒否されることを見るテスト用なので、画素データは要らない。
func vp8lHeader(w, h int) []byte {
	p := make([]byte, 5)
	p[0] = 0x2f
	binary.LittleEndian.PutUint32(p[1:5], uint32(w-1)|uint32(h-1)<<14)
	return p
}

// vp8xCanvas builds a VP8X chunk with flags and canvas size.
func vp8xCanvas(flags byte, w, h int) []byte {
	p := make([]byte, 10)
	p[0] = flags
	putU24le(p[4:7], uint32(w-1))
	putU24le(p[7:10], uint32(h-1))
	return chunk("VP8X", p)
}

// anmf builds an ANMF chunk placing a frame of w x h at (x, y).
func anmf(x, y, w, h int, sub ...[]byte) []byte {
	p := make([]byte, 16)
	putU24le(p[0:3], uint32(x/2))
	putU24le(p[3:6], uint32(y/2))
	putU24le(p[6:9], uint32(w-1))
	putU24le(p[9:12], uint32(h-1))
	putU24le(p[12:15], 100)
	for _, s := range sub {
		p = append(p, s...)
	}
	return chunk("ANMF", p)
}

func animHeader() []byte { return chunk("ANIM", make([]byte, 6)) }

// encodeFrameChunks encodes img as a still WebP and returns its image chunks
// (ALPH / VP8 / VP8L), ready to be placed inside an ANMF.
func encodeFrameChunks(t *testing.T, img image.Image, lossless bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, gwebp.Encode(&buf, img, gwebp.Options{Lossless: lossless, Quality: 100}))
	chunks, _, err := webpChunks(buf.Bytes())
	require.NoError(t, err)
	var out []byte
	for _, c := range chunks {
		switch c.typ {
		case "ALPH", "VP8 ", "VP8L":
			out = append(out, chunk(c.typ, c.payload)...)
		}
	}
	require.NotEmpty(t, out)
	return out
}

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

// TestDecode_AnimatedWebPFrameLargerThanCanvasIsRefused pins the decode-bomb
// shape: a 1x1 canvas carrying huge frames. 修正前は imaging が全コマを
// デコードして 1.2GiB 確保していた。
func TestDecode_AnimatedWebPFrameLargerThanCanvasIsRefused(t *testing.T) {
	const big = 12000
	cases := []struct {
		name string
		data []byte
	}{
		{
			name: "ANMF declares a frame larger than the canvas",
			data: riffWebP(vp8xCanvas(webpFlagAnimation, 1, 1), animHeader(),
				anmf(0, 0, big, big, chunk("VP8L", vp8lHeader(big, big))),
				anmf(0, 0, big, big, chunk("VP8L", vp8lHeader(big, big)))),
		},
		{
			name: "ANMF fits the canvas but the bitstream is huge",
			data: riffWebP(vp8xCanvas(webpFlagAnimation, 1, 1), animHeader(),
				anmf(0, 0, 1, 1, chunk("VP8L", vp8lHeader(big, big)))),
		},
		{
			name: "frame offset pushes it outside the canvas",
			data: riffWebP(vp8xCanvas(webpFlagAnimation, 16, 16), animHeader(),
				anmf(8, 8, 16, 16, chunk("VP8L", vp8lHeader(16, 16)))),
		},
		{
			name: "still VP8X with a bitstream larger than the canvas",
			data: riffWebP(vp8xCanvas(0, 1, 1), chunk("VP8L", vp8lHeader(big, big))),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img, err := Decode(tc.data)
			require.Error(t, err)
			assert.Nil(t, img)
			assert.True(t, errors.Is(err, ErrMalformedWebP), "got %v", err)
		})
	}
}

func TestDecode_WebPBitstreamOverPixelCapIsRefused(t *testing.T) {
	data := riffWebP(chunk("VP8L", vp8lHeader(16000, 16000)))
	_, err := DecodeWithPixelCap(data, 1000)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTooManyPixels), "got %v", err)
}

func TestDecode_AnimatedWebPReturnsFirstFrame(t *testing.T) {
	red := color.NRGBA{R: 255, A: 255}
	blue := color.NRGBA{B: 255, A: 255}

	t.Run("lossless, full canvas", func(t *testing.T) {
		data := riffWebP(vp8xCanvas(webpFlagAnimation, 16, 16), animHeader(),
			anmf(0, 0, 16, 16, encodeFrameChunks(t, solid(16, 16, red), true)),
			anmf(0, 0, 16, 16, encodeFrameChunks(t, solid(16, 16, blue), true)))
		img, err := Decode(data)
		require.NoError(t, err)
		assert.Equal(t, image.Rect(0, 0, 16, 16), img.Bounds())
		r, g, b, a := img.At(5, 5).RGBA()
		assert.Equal(t, [4]uint32{0xffff, 0, 0, 0xffff}, [4]uint32{r, g, b, a})
	})

	t.Run("first frame offset on a larger canvas", func(t *testing.T) {
		data := riffWebP(vp8xCanvas(webpFlagAnimation, 32, 32), animHeader(),
			anmf(8, 8, 16, 16, encodeFrameChunks(t, solid(16, 16, red), true)))
		img, err := Decode(data)
		require.NoError(t, err)
		assert.Equal(t, image.Rect(0, 0, 32, 32), img.Bounds())
		_, _, _, a := img.At(1, 1).RGBA()
		assert.Zero(t, a, "outside the first frame must stay transparent")
		r, _, _, a := img.At(10, 10).RGBA()
		assert.Equal(t, uint32(0xffff), r)
		assert.Equal(t, uint32(0xffff), a)
	})

	t.Run("lossy with ALPH", func(t *testing.T) {
		half := solid(16, 16, color.NRGBA{G: 255, A: 128})
		data := riffWebP(vp8xCanvas(webpFlagAnimation|webpFlagAlpha, 16, 16), animHeader(),
			anmf(0, 0, 16, 16, encodeFrameChunks(t, half, false)))
		img, err := Decode(data)
		require.NoError(t, err)
		_, _, _, a := img.At(4, 4).RGBA()
		assert.InDelta(t, 128*257, a, 4*257, "alpha must survive the still rebuild")
	})
}

func TestDecode_MalformedWebPDoesNotPanic(t *testing.T) {
	valid := riffWebP(vp8xCanvas(webpFlagAnimation, 16, 16), animHeader(),
		anmf(0, 0, 16, 16, chunk("VP8L", vp8lHeader(16, 16))))
	for i := 12; i < len(valid); i++ {
		assert.NotPanics(t, func() { _, _ = Decode(valid[:i]) }, "truncated at %d", i)
	}
	// ANMF の中身が 16 バイトの header に満たない。
	short := riffWebP(vp8xCanvas(webpFlagAnimation, 16, 16), chunk("ANMF", []byte{1, 2, 3}))
	_, err := Decode(short)
	assert.True(t, errors.Is(err, ErrMalformedWebP), "got %v", err)
}

func FuzzDecodeWebP(f *testing.F) {
	f.Add(riffWebP(vp8xCanvas(webpFlagAnimation, 16, 16), animHeader(),
		anmf(0, 0, 16, 16, chunk("VP8L", vp8lHeader(16, 16)))))
	f.Add(riffWebP(chunk("VP8L", vp8lHeader(4, 4))))
	f.Fuzz(func(t *testing.T, data []byte) {
		if !isWebP(data) {
			return
		}
		_, _ = decodeWebP(data, 1<<16)
	})
}

// TestDecodeWebP_Branches exercises decodeWebP directly so that each guard is
// hit on its own (Decode の前段の DecodeConfig が先に弾くケースも含めて)。
func TestDecodeWebP_Branches(t *testing.T) {
	red := solid(8, 8, color.NRGBA{R: 255, A: 255})
	var lossless, lossy bytes.Buffer
	require.NoError(t, gwebp.Encode(&lossless, red, gwebp.Options{Lossless: true}))
	require.NoError(t, gwebp.Encode(&lossy, solid(8, 8, color.NRGBA{G: 255, A: 100}), gwebp.Options{Quality: 90}))
	lossyChunks, _, err := webpChunks(lossy.Bytes())
	require.NoError(t, err)
	require.Equal(t, "VP8X", lossyChunks[0].typ, "lossy with alpha is expected to use the extended format")

	vp8 := func(w, h int) []byte {
		p := make([]byte, 10)
		p[3], p[4], p[5] = 0x9d, 0x01, 0x2a
		binary.LittleEndian.PutUint16(p[6:8], uint16(w))
		binary.LittleEndian.PutUint16(p[8:10], uint16(h))
		return p
	}

	ok := []struct {
		name string
		data []byte
	}{
		{"simple VP8L", lossless.Bytes()},
		{"extended still with ALPH", lossy.Bytes()},
		{"trailing garbage after RIFF size", append(append([]byte{}, lossless.Bytes()...), 'X', 'Y', 'Z')},
		{"animated with ICCP and EXIF kept", riffWebP(
			vp8xCanvas(webpFlagAnimation|webpFlagICC|webpFlagEXIF, 8, 8),
			chunk("ICCP", []byte{0}), animHeader(),
			anmf(0, 0, 8, 8, encodeFrameChunks(t, red, true)),
			chunk("EXIF", []byte{0}))},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			// ICCP / EXIF の中身はダミーなので imaging が読めない可能性はあるが、
			// 検査段階 (ErrMalformedWebP / ErrTooManyPixels) では弾かれないこと。
			_, err := decodeWebP(tc.data, 1<<20)
			if err != nil {
				assert.False(t, errors.Is(err, ErrMalformedWebP), "got %v", err)
				assert.False(t, errors.Is(err, ErrTooManyPixels), "got %v", err)
			}
		})
	}

	bad := []struct {
		name string
		data []byte
		want error
	}{
		{"not webp", []byte("RIFF\x00\x00\x00\x00JUNK"), ErrMalformedWebP},
		{"no chunks", []byte("RIFF\x04\x00\x00\x00WEBP"), ErrMalformedWebP},
		{"unexpected first chunk", riffWebP(chunk("ICCP", []byte{0})), ErrMalformedWebP},
		{"short VP8X", riffWebP(chunk("VP8X", []byte{0, 0})), ErrMalformedWebP},
		{"canvas over cap", riffWebP(vp8xCanvas(0, 2000, 2000), chunk("VP8L", vp8lHeader(2000, 2000))), ErrTooManyPixels},
		{"simple VP8 over cap", riffWebP(chunk("VP8 ", vp8(2000, 2000))), ErrTooManyPixels},
		{"simple VP8 bad start code", riffWebP(chunk("VP8 ", make([]byte, 10))), ErrMalformedWebP},
		{"simple VP8 zero size", riffWebP(chunk("VP8 ", vp8(0, 4))), ErrMalformedWebP},
		{"simple VP8L bad signature", riffWebP(chunk("VP8L", []byte{0, 0, 0, 0, 0})), ErrMalformedWebP},
		{"still VP8X without bitstream", riffWebP(vp8xCanvas(0, 8, 8), chunk("EXIF", []byte{0})), ErrMalformedWebP},
		{"still VP8X with broken bitstream", riffWebP(vp8xCanvas(0, 8, 8), chunk("VP8L", []byte{1})), ErrMalformedWebP},
		{"animated without ANMF", riffWebP(vp8xCanvas(webpFlagAnimation, 8, 8), animHeader()), ErrMalformedWebP},
		{"frame without bitstream", riffWebP(vp8xCanvas(webpFlagAnimation, 8, 8), animHeader(),
			anmf(0, 0, 8, 8, chunk("ALPH", []byte{0}))), ErrMalformedWebP},
		{"frame with broken sub-chunks", riffWebP(vp8xCanvas(webpFlagAnimation, 8, 8), animHeader(),
			anmf(0, 0, 8, 8, []byte("VP8L\xff\xff\xff\xff"))), ErrMalformedWebP},
		{"frame with broken bitstream", riffWebP(vp8xCanvas(webpFlagAnimation, 8, 8), animHeader(),
			anmf(0, 0, 8, 8, chunk("VP8 ", []byte{1, 2}))), ErrMalformedWebP},
		{"unknown bitstream type", riffWebP(vp8xCanvas(webpFlagAnimation, 8, 8), animHeader(),
			anmf(0, 0, 8, 8, chunk("VP8L", vp8lHeader(8, 8))[:0])), ErrMalformedWebP},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeWebP(tc.data, 1<<20)
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.want), "got %v", err)
		})
	}

	t.Run("unknown bitstream fourcc", func(t *testing.T) {
		_, _, err := webpBitstreamDims("VP9 ", nil)
		assert.True(t, errors.Is(err, ErrMalformedWebP))
	})
}

// allocDuring reports how many bytes f allocated.
func allocDuring(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// losslessChunk encodes a solid w x h image and returns its VP8L chunk.
func losslessChunk(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, gwebp.Encode(&buf, solid(w, h, color.NRGBA{R: 255, A: 255}), gwebp.Options{Lossless: true}))
	chunks, _, err := webpChunks(buf.Bytes())
	require.NoError(t, err)
	for _, c := range chunks {
		if c.typ == "VP8L" {
			return chunk("VP8L", c.payload)
		}
	}
	t.Fatal("no VP8L chunk")
	return nil
}

// TestDecode_WebPExifPastRIFFEndIsNotRead pins that bytes after the declared
// RIFF size never reach imaging. imaging の EXIF 読み取りは RIFF の宣言長を見ずに
// EOF まで歩き、見つけた長さをそのまま確保する (修正前は 54 バイトで 4GiB)。
func TestDecode_WebPExifPastRIFFEndIsNotRead(t *testing.T) {
	bits := losslessChunk(t, 8, 8)
	if n := binary.LittleEndian.Uint32(bits[4:8]); n%2 == 1 {
		// 宣言長が奇数だと imaging の読み取りがパディングでずれて EXIF に届かず、
		// 切り詰めを外した変異でも落ちなくなる。偶数長に揃える
		bits = chunk("VP8L", append(append([]byte{}, bits[8:8+n]...), 0))
	}
	body := riffWebP(vp8xCanvas(webpFlagEXIF, 8, 8), bits)
	evil := append(append([]byte{}, body...), []byte("EXIF\xf0\xff\xff\xff")...)
	var err error
	alloc := allocDuring(func() { _, err = Decode(evil) })
	require.NoError(t, err)
	assert.Less(t, alloc, uint64(16<<20), "allocated %d bytes", alloc)
}

// TestDecode_WebPPaddingMisalignmentIsRefused pins the replay of imaging's
// metadata walk. imaging はパディングを数えないので、奇数長チャンクの後ろを
// 1 バイトずれて読み、偽の EXIF 長で確保する (1MiB のファイルで 256MiB)。
func TestDecode_WebPPaddingMisalignmentIsRefused(t *testing.T) {
	bits := losslessChunk(t, 8, 8)
	payload := bits[8 : 8+binary.LittleEndian.Uint32(bits[4:8])]
	if len(payload)%2 == 0 {
		// 末尾に 1 バイト足して奇数長にする (VP8L は余りを読まない)
		payload = append(append([]byte{}, payload...), 0)
	}
	odd := chunk("VP8L", payload)
	odd[len(odd)-1] = 'E' // パディング位置。imaging はここから "EXIF" と読む
	// imaging には長さ 0x10000000 に見える: 0x00 + size の下位 3 バイト
	fake := append([]byte("XIF\x00\x00\x00\x10\x00"), make([]byte, 1<<20)...)
	data := riffWebP(vp8xCanvas(webpFlagEXIF, 8, 8), odd, fake)

	var err error
	alloc := allocDuring(func() { _, err = Decode(data) })
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMalformedWebP), "got %v", err)
	assert.Less(t, alloc, uint64(64<<20), "allocated %d bytes", alloc)
}

// TestDecode_AnimatedWebPDecodesOnlyTheFirstFrame pins the core of the
// defence: later frames are never decoded. 検査は 1 コマ目しか見ないので、
// 組み直した still ではなく元のバイト列を渡すと 2 コマ目以降を全部確保する。
func TestDecode_AnimatedWebPDecodesOnlyTheFirstFrame(t *testing.T) {
	const side = 2048
	frame := losslessChunk(t, side, side)
	chunks := [][]byte{vp8xCanvas(webpFlagAnimation, side, side), animHeader()}
	for i := 0; i < 6; i++ {
		chunks = append(chunks, anmf(0, 0, side, side, frame))
	}
	data := riffWebP(chunks...)
	var err error
	alloc := allocDuring(func() { _, err = Decode(data) })
	require.NoError(t, err)
	// 1 コマ分のラスタは 16MiB。全コマをデコードすると 6 倍以上になる
	assert.Less(t, alloc, uint64(48<<20), "allocated %d bytes", alloc)
}

// exifOrientation builds a little-endian TIFF with one Orientation entry,
// padded to an odd length so that the rebuild has to even it out.
func exifOrientation(o uint16) []byte {
	b := []byte("II*\x00\x08\x00\x00\x00\x01\x00")
	entry := make([]byte, 12)
	binary.LittleEndian.PutUint16(entry[0:2], 0x0112)
	binary.LittleEndian.PutUint16(entry[2:4], 3)
	binary.LittleEndian.PutUint32(entry[4:8], 1)
	binary.LittleEndian.PutUint16(entry[8:10], o)
	b = append(b, entry...)
	return append(b, 0, 0, 0, 0) // next IFD
}

func TestDecode_AnimatedWebPKeepsExifOrientation(t *testing.T) {
	frame := encodeFrameChunks(t, solid(8, 16, color.NRGBA{R: 255, A: 255}), true)
	data := riffWebP(vp8xCanvas(webpFlagAnimation|webpFlagEXIF, 8, 16), animHeader(),
		anmf(0, 0, 8, 16, frame), chunk("EXIF", exifOrientation(6)))
	img, err := Decode(data)
	require.NoError(t, err)
	// Orientation 6 は 90 度回転なので 8x16 が 16x8 になる
	assert.Equal(t, image.Rect(0, 0, 16, 8), img.Bounds())
}

// oddICC returns a real sRGB profile padded to an odd length.
//
// sRGB-v2-nano.icc は saucecontrol/Compact-ICC-Profiles (CC0) のもの。ICC は
// ヘッダに自分の長さを持つので、末尾に 1 バイト足しても同じプロファイルとして読まれる。
func oddICC(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/sRGB-v2-nano.icc")
	require.NoError(t, err)
	if len(b)%2 == 0 {
		b = append(b, 0)
	}
	return b
}

func TestDecode_AnimatedWebPKeepsExifOrientationAfterOddICCP(t *testing.T) {
	// 奇数長の ICCP の後ろでも imaging が EXIF を見つけられること
	frame := encodeFrameChunks(t, solid(8, 16, color.NRGBA{R: 255, A: 255}), true)
	data := riffWebP(vp8xCanvas(webpFlagAnimation|webpFlagICC|webpFlagEXIF, 8, 16),
		chunk("ICCP", oddICC(t)), animHeader(),
		anmf(0, 0, 8, 16, frame), chunk("EXIF", exifOrientation(6)))
	img, err := Decode(data)
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 16, 8), img.Bounds())
}

func TestEvenPayload_DoesNotAliasInput(t *testing.T) {
	src := []byte{1, 2, 3, 9}
	odd := src[:3]
	got := evenPayload(odd)
	assert.Equal(t, []byte{1, 2, 3, 0}, got)
	assert.Equal(t, byte(9), src[3], "input backing array must not be overwritten")
	even := []byte{1, 2}
	assert.Equal(t, even, evenPayload(even))
}
