package mediaproxy

import (
	"encoding/binary"
	"errors"
	"runtime"
	"testing"

	"github.com/elythia-network/elythia/internal/misc/imagedecode"
	"github.com/stretchr/testify/require"
)

func tgaWithDeclaredSize(w, h int, body []byte) []byte {
	b := make([]byte, 18)
	b[2] = 2
	binary.LittleEndian.PutUint16(b[12:14], uint16(w))
	binary.LittleEndian.PutUint16(b[14:16], uint16(h))
	b[16] = 32
	b[17] = 8
	return append(b, body...)
}

// decodeImage の TGA 分岐が cap 付きの入口を通ること。
//
// **これが無いと差し戻しを検出できない。** 素の `tga.Decode` に戻しても
// 他のテストは緑のまま通る (TGA を食わせるテストが 1 つも無かった)。
func TestDecodeImage_TGAGoesThroughPixelCap(t *testing.T) {
	t.Parallel()

	for _, mime := range []string{"image/x-tga", "image/x-targa"} {
		t.Run(mime, func(t *testing.T) {
			data := tgaWithDeclaredSize(16384, 16384, make([]byte, 64))

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)

			img, err := decodeImage(data, mime)

			runtime.ReadMemStats(&after)

			require.Error(t, err)
			require.Nil(t, img)
			require.True(t, errors.Is(err, imagedecode.ErrTooManyPixels),
				"decodeImage must refuse the declared size, got %v", err)

			// 宣言どおりなら 1 GiB。確保の前に弾けていることを見る。
			allocated := after.TotalAlloc - before.TotalAlloc
			require.Less(t, allocated, uint64(64<<20),
				"raster must not be allocated (allocated %d bytes)", allocated)
		})
	}
}

// 通常の TGA は引き続きデコードできること (cap が通る集合を狭めていない)。
func TestDecodeImage_TGAStillDecodes(t *testing.T) {
	t.Parallel()

	data := tgaWithDeclaredSize(2, 2, make([]byte, 2*2*4))
	img, err := decodeImage(data, "image/x-tga")
	require.NoError(t, err)
	require.NotNil(t, img)
	require.Equal(t, 2, img.Bounds().Dx())
}
