package mediaproxy

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"io"
	"testing"

	"github.com/gen2brain/avif"
	gwebp "github.com/gen2brain/webp"
	"github.com/stretchr/testify/require"
)

// lossyWebPRangeFixture creates a synthetic grayscale ramp. The opaque form is
// encoded as VP8; adding a non-opaque row makes libwebp emit VP8X + ALPH + VP8.
// No production image or instance-specific data is used by this regression test.
func lossyWebPRangeFixture(t *testing.T, withAlpha bool) []byte {
	t.Helper()
	const width, height = 64, 16
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint8((x*255 + (width-1)/2) / (width - 1))
			a := uint8(255)
			if withAlpha && y >= height/2 {
				a = 96
			}
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: a})
		}
	}

	var buf bytes.Buffer
	require.NoError(t, gwebp.Encode(&buf, img, gwebp.Options{
		Quality: 90,
		Method:  4,
		Exact:   true,
	}))
	data := buf.Bytes()
	if withAlpha {
		require.Equal(t, "VP8X", string(data[12:16]))
		require.Contains(t, string(data), "ALPH")
	} else {
		require.Equal(t, "VP8 ", string(data[12:16]))
	}
	return data
}

func decodeRangeOutput(t *testing.T, format OutputFormat, data []byte) image.Image {
	t.Helper()
	var (
		img image.Image
		err error
	)
	if format == FormatAVIF {
		img, err = avif.Decode(bytes.NewReader(data))
	} else {
		var decoded *gwebp.WEBP
		decoded, err = gwebp.DecodeAll(bytes.NewReader(data))
		if err == nil && len(decoded.Image) != 0 {
			// DecodeAll asks libwebp for RGBA output. The single-frame Decode
			// path returns raw YUVA planes and is the behavior under test.
			img = decoded.Image[0]
		}
	}
	require.NoError(t, err)
	require.NotNil(t, img)
	return img
}

func TestNormalizeForResize_MaterializesOnlyAlphaWebP(t *testing.T) {
	opaque, err := decodeImage(lossyWebPRangeFixture(t, false), "image/webp")
	require.NoError(t, err)
	require.Same(t, opaque, normalizeForResize(opaque),
		"opaque VP8 must stay lazy instead of allocating a full-size NRGBA buffer")

	withAlpha, err := decodeImage(lossyWebPRangeFixture(t, true), "image/webp")
	require.NoError(t, err)
	normalized := normalizeForResize(withAlpha)
	require.NotSame(t, withAlpha, normalized)
	require.IsType(t, &image.NRGBA{}, normalized,
		"alpha WebP needs materialization to preserve transparent RGB during resize")
}

// TestProcessResize_WebPDecodeReencodePreservesRange is intentionally an
// end-to-end decode -> resize/no-op -> encode test. Comparing the output with a
// libwebp decode of the same synthetic input catches studio-range YCbCr values
// being treated as full-range RGB (black becomes about 16 and white about 235).
func TestProcessResize_WebPDecodeReencodePreservesRange(t *testing.T) {
	const (
		height       = 16
		channelDelta = 10
	)
	s := testService(nil)

	for _, withAlpha := range []bool{false, true} {
		name := "vp8"
		if withAlpha {
			name = "vp8x-alpha"
		}
		source := lossyWebPRangeFixture(t, withAlpha)
		for _, output := range []OutputFormat{FormatWebP, FormatAVIF} {
			t.Run(fmt.Sprintf("%s-to-%d", name, output), func(t *testing.T) {
				result, err := s.processResize(source, "image/webp", 0, height, output)
				require.NoError(t, err)
				defer result.Body.Close()
				encoded, err := io.ReadAll(result.Body)
				require.NoError(t, err)
				got := decodeRangeOutput(t, output, encoded)

				for _, x := range []int{0, 16, 32, 48, 63} {
					for _, y := range []int{2, 12} {
						if !withAlpha && y == 12 {
							continue
						}
						v := uint8((x*255 + 63/2) / 63)
						alpha := uint8(255)
						if withAlpha && y >= height/2 {
							alpha = 96
						}
						wantPixel := color.NRGBA{R: v, G: v, B: v, A: alpha}
						gotPixel := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA)
						require.InDelta(t, wantPixel.R, gotPixel.R, channelDelta, "R at (%d,%d): want=%v got=%v", x, y, wantPixel, gotPixel)
						require.InDelta(t, wantPixel.G, gotPixel.G, channelDelta, "G at (%d,%d): want=%v got=%v", x, y, wantPixel, gotPixel)
						require.InDelta(t, wantPixel.B, gotPixel.B, channelDelta, "B at (%d,%d): want=%v got=%v", x, y, wantPixel, gotPixel)
						require.InDelta(t, wantPixel.A, gotPixel.A, 2, "A at (%d,%d): want=%v got=%v", x, y, wantPixel, gotPixel)
					}
				}
			})
		}
	}
}
