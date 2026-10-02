package imagedecode

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"

	gwebp "github.com/gen2brain/webp"
	"github.com/kovidgoyal/imaging"
	"github.com/kovidgoyal/imaging/prism/meta/icc"
	"github.com/stretchr/testify/require"
)

func webPRangeRamp(alpha bool) *image.NRGBA {
	const width, height = 64, 16
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint8((x*255 + (width-1)/2) / (width - 1))
			a := uint8(255)
			if alpha && y >= height/2 {
				a = 96
			}
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: a})
		}
	}
	return img
}

func encodeRangeWebP(t *testing.T, img image.Image, lossless bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, gwebp.Encode(&buf, img, gwebp.Options{
		Lossless: lossless,
		Quality:  90,
		Method:   4,
		Exact:    true,
	}))
	return buf.Bytes()
}

func imageRange(t *testing.T, img image.Image) (uint8, uint8) {
	t.Helper()
	minV, maxV := uint8(255), uint8(0)
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			p := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if p.R < minV {
				minV = p.R
			}
			if p.R > maxV {
				maxV = p.R
			}
		}
	}
	return minV, maxV
}

func requireFullRange(t *testing.T, img image.Image) {
	t.Helper()
	minV, maxV := imageRange(t, img)
	require.LessOrEqual(t, minV, uint8(10), "black endpoint was lifted: min=%d", minV)
	require.GreaterOrEqual(t, maxV, uint8(245), "white endpoint was compressed: max=%d", maxV)
}

func lossyFrameChunks(t *testing.T, img image.Image) []byte {
	t.Helper()
	data := encodeRangeWebP(t, img, false)
	chunks, _, err := webpChunks(data)
	require.NoError(t, err)
	var out []byte
	for _, c := range chunks {
		switch c.typ {
		case "ALPH", "VP8 ":
			out = append(out, chunk(c.typ, c.payload)...)
		}
	}
	require.Contains(t, string(out), "VP8 ")
	return out
}

// VP8 decodes to studio-range YCbCr, VP8X + ALPH to NYCbCrA, while VP8L
// decodes directly to NRGBA. Only the two lossy variants need normalization.
func TestDecodeWebP_NormalizesLossyVariantsOnly(t *testing.T) {
	t.Run("VP8", func(t *testing.T) {
		img, err := Decode(encodeRangeWebP(t, webPRangeRamp(false), false))
		require.NoError(t, err)
		require.IsType(t, &limitedRangeWebPImage{}, img)
		requireFullRange(t, img)
	})

	t.Run("VP8X with alpha", func(t *testing.T) {
		img, err := Decode(encodeRangeWebP(t, webPRangeRamp(true), false))
		require.NoError(t, err)
		require.IsType(t, &limitedRangeWebPImage{}, img)
		requireFullRange(t, img)
		got := color.NRGBAModel.Convert(img.At(32, 12)).(color.NRGBA)
		require.InDelta(t, 96, got.A, 1)
	})

	t.Run("VP8L", func(t *testing.T) {
		src := webPRangeRamp(true)
		img, err := Decode(encodeRangeWebP(t, src, true))
		require.NoError(t, err)
		require.IsType(t, &image.NRGBA{}, img)
		nrgba := img.(*image.NRGBA)
		require.Equal(t, src.Pix, nrgba.Pix, "lossless RGB must pass through without range expansion")
	})

	t.Run("VP8 matches the full-color reference decoder", func(t *testing.T) {
		palette := []color.NRGBA{
			{R: 250, G: 20, B: 20, A: 255},
			{R: 20, G: 250, B: 20, A: 255},
			{R: 20, G: 20, B: 250, A: 255},
			{R: 245, G: 220, B: 30, A: 255},
		}
		src := image.NewNRGBA(image.Rect(0, 0, 64, 64))
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				src.SetNRGBA(x, y, palette[(x/32)+2*(y/32)])
			}
		}
		data := encodeRangeWebP(t, src, false)
		// DecodeAll uses libwebp's RGBA output path. The single-frame Decode
		// path deliberately returns raw YUVA planes and would reproduce the
		// range bug when converted through image.Image.At.
		decoded, err := gwebp.DecodeAll(bytes.NewReader(data))
		require.NoError(t, err)
		require.NotEmpty(t, decoded.Image)
		want := decoded.Image[0]
		got, err := Decode(data)
		require.NoError(t, err)
		for _, p := range []image.Point{{16, 16}, {48, 16}, {16, 48}, {48, 48}} {
			wantColor := color.NRGBAModel.Convert(want.At(p.X, p.Y)).(color.NRGBA)
			gotColor := color.NRGBAModel.Convert(got.At(p.X, p.Y)).(color.NRGBA)
			require.InDelta(t, wantColor.R, gotColor.R, 3, "red at %v", p)
			require.InDelta(t, wantColor.G, gotColor.G, 3, "green at %v", p)
			require.InDelta(t, wantColor.B, gotColor.B, 3, "blue at %v", p)
		}
	})
}

func TestNormalizeLossyWebPRange_DoesNotAllocatePixelBuffer(t *testing.T) {
	src := image.NewYCbCr(image.Rect(0, 0, 4096, 4096), image.YCbCrSubsampleRatio420)
	got := normalizeLossyWebPRange(src)
	require.IsType(t, &limitedRangeWebPImage{}, got)
	wrapper := got.(*limitedRangeWebPImage)
	require.Same(t, src, wrapper.ycbcr)

	allocs := testing.AllocsPerRun(100, func() {
		got = normalizeLossyWebPRange(src)
	})
	require.LessOrEqual(t, allocs, float64(1), "normalization must allocate only the small wrapper, not a pixel buffer")
}

func syntheticWideGamutICC(t *testing.T) []byte {
	t.Helper()
	data := append([]byte(nil), oddICC(t)...)
	require.GreaterOrEqual(t, len(data), 132)
	tagCount := int(binary.BigEndian.Uint32(data[128:132]))
	found := false
	for i := 0; i < tagCount; i++ {
		entry := 132 + i*12
		require.LessOrEqual(t, entry+12, len(data))
		if string(data[entry:entry+4]) != "rXYZ" {
			continue
		}
		offset := int(binary.BigEndian.Uint32(data[entry+4 : entry+8]))
		require.LessOrEqual(t, offset+20, len(data))
		require.Equal(t, "XYZ ", string(data[offset:offset+4]))
		// Change the red primary's X coordinate. The source profile is CC0;
		// this deterministic mutation creates a synthetic non-sRGB profile.
		binary.BigEndian.PutUint32(data[offset+8:offset+12], 0x00009000)
		found = true
		break
	}
	require.True(t, found)
	profile, err := icc.DecodeProfile(bytes.NewReader(data))
	require.NoError(t, err)
	require.False(t, profile.IsSRGB())
	return data
}

func TestDecodeWebP_AnimatedVP8KeepsRangeAndMetadata(t *testing.T) {
	frame := lossyFrameChunks(t, webPRangeRamp(false))

	t.Run("animation", func(t *testing.T) {
		data := riffWebP(vp8xCanvas(webpFlagAnimation, 64, 16), animHeader(), anmf(0, 0, 64, 16, frame))
		img, err := Decode(data)
		require.NoError(t, err)
		requireFullRange(t, img)
	})

	t.Run("ICC and EXIF orientation", func(t *testing.T) {
		data := riffWebP(
			vp8xCanvas(webpFlagAnimation|webpFlagICC|webpFlagEXIF, 64, 16),
			chunk("ICCP", oddICC(t)), animHeader(), anmf(0, 0, 64, 16, frame),
			chunk("EXIF", exifOrientation(6)),
		)
		img, err := Decode(data)
		require.NoError(t, err)
		require.Equal(t, image.Rect(0, 0, 16, 64), img.Bounds())
		requireFullRange(t, img)
	})

	t.Run("range normalization precedes non-sRGB ICC conversion", func(t *testing.T) {
		profileData := syntheticWideGamutICC(t)
		data := riffWebP(
			vp8xCanvas(webpFlagAnimation|webpFlagICC, 64, 16),
			chunk("ICCP", profileData), animHeader(), anmf(0, 0, 64, 16, frame),
		)
		got, err := Decode(data)
		require.NoError(t, err)

		reference, err := gwebp.DecodeAll(bytes.NewReader(data))
		require.NoError(t, err)
		require.NotEmpty(t, reference.Image)
		profile, err := icc.DecodeProfile(bytes.NewReader(profileData))
		require.NoError(t, err)
		want, err := imaging.ConvertToSRGB(profile, imaging.Relative, true, reference.Image[0])
		require.NoError(t, err)

		transformed := false
		for _, p := range []image.Point{{0, 2}, {32, 2}, {63, 2}} {
			wantColor := color.NRGBAModel.Convert(want.At(p.X, p.Y)).(color.NRGBA)
			gotColor := color.NRGBAModel.Convert(got.At(p.X, p.Y)).(color.NRGBA)
			rawColor := color.NRGBAModel.Convert(reference.Image[0].At(p.X, p.Y)).(color.NRGBA)
			if absByteDiff(wantColor.R, rawColor.R) > 3 || absByteDiff(wantColor.G, rawColor.G) > 3 || absByteDiff(wantColor.B, rawColor.B) > 3 {
				transformed = true
			}
			require.InDelta(t, wantColor.R, gotColor.R, 3, "red at %v", p)
			require.InDelta(t, wantColor.G, gotColor.G, 3, "green at %v", p)
			require.InDelta(t, wantColor.B, gotColor.B, 3, "blue at %v", p)
		}
		require.True(t, transformed, "synthetic profile must exercise a non-no-op ICC conversion")
	})
}

func absByteDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}
