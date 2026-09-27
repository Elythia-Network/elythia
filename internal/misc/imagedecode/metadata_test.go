package imagedecode

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tiffWithEntry builds a little-endian TIFF block with one IFD entry.
func tiffWithEntry(tag, typ uint16, count, value uint32) []byte {
	b := []byte("II*\x00\x08\x00\x00\x00\x01\x00")
	e := make([]byte, 12)
	binary.LittleEndian.PutUint16(e[0:2], tag)
	binary.LittleEndian.PutUint16(e[2:4], typ)
	binary.LittleEndian.PutUint32(e[4:8], count)
	binary.LittleEndian.PutUint32(e[8:12], value)
	b = append(b, e...)
	return append(b, 0, 0, 0, 0)
}

// overflowingExif is the goexif bomb: 4 * 0x40000001 wraps to 4 in uint32, so
// goexif reads the value inline and then allocates 0x40000001 int64s (8GiB).
func overflowingExif() []byte { return tiffWithEntry(0x0100, 4, 0x40000001, 0) }

func jpegWithSegments(t *testing.T, segs ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8)), nil))
	src := buf.Bytes()
	out := append([]byte{}, src[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, src[2:]...)
}

func jpegSegment(marker byte, payload []byte) []byte {
	s := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(s[2:4], uint16(len(payload)+2))
	return append(s, payload...)
}

func pngWithChunks(t *testing.T, chunks ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8))))
	src := buf.Bytes()
	// signature 8 + IHDR 25 の後ろに差し込む
	out := append([]byte{}, src[:33]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, src[33:]...)
}

func iccpBody(t *testing.T, profile []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, err := zw.Write(profile)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return append([]byte("icc\x00\x00"), z.Bytes()...)
}

func nanoICC(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/sRGB-v2-nano.icc")
	require.NoError(t, err)
	return b
}

func TestDecode_RefusesMetadataBombs(t *testing.T) {
	hugeTagTable := nanoICC(t)
	binary.BigEndian.PutUint32(hugeTagTable[128:132], 0x10000000)

	tagPastEnd := nanoICC(t)
	binary.BigEndian.PutUint32(tagPastEnd[132+8:132+12], 0x7fffffff)

	// 実在の TRC タグ (curv) の要素数を書き換える
	curveBomb := nanoICC(t)
	ci := bytes.Index(curveBomb, []byte("curv"))
	require.Positive(t, ci)
	binary.BigEndian.PutUint32(curveBomb[ci+8:ci+12], 0x7fffffff)

	webpWith := func(typ string, flags byte, payload []byte) []byte {
		frame := encodeFrameChunks(t, solid(8, 8, color.NRGBA{R: 255, A: 255}), true)
		return riffWebP(vp8xCanvas(flags, 8, 8), chunk(typ, payload), frame)
	}

	cases := map[string][]byte{
		"JPEG EXIF count overflow": jpegWithSegments(t, jpegSegment(0xE1, append([]byte("Exif\x00\x00"), overflowingExif()...))),
		"JPEG ICC tag table":       jpegWithSegments(t, jpegSegment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), hugeTagTable...))),
		"PNG eXIf count overflow":  pngWithChunks(t, pngChunk("eXIf", overflowingExif())),
		"PNG iCCP tag past end":    pngWithChunks(t, pngChunk("iCCP", iccpBody(t, tagPastEnd))),
		"PNG iCCP curve":           pngWithChunks(t, pngChunk("iCCP", iccpBody(t, curveBomb))),
		"PNG iCCP zlib bomb":       pngWithChunks(t, pngChunk("iCCP", iccpBody(t, make([]byte, maxICCProfileBytes+1)))),
		"PNG chunk length wraps":   pngWithChunks(t, []byte("\xff\xff\xff\xfceXIf")),
		"WebP EXIF count overflow": webpWith("EXIF", webpFlagEXIF, overflowingExif()),
		"WebP ICCP tag table":      webpWith("ICCP", webpFlagICC, hugeTagTable),
		"TIFF IFD loop":            append(tiffWithEntry(0x0100, 3, 1, 8)[:22], 8, 0, 0, 0),
		"TIFF sub-IFD overflow": func() []byte {
			b := tiffWithEntry(0x8769, 4, 1, 26)
			return append(b, overflowingExif()[8:]...)
		}(),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var err error
			alloc := allocDuring(func() { _, err = Decode(data) })
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrMalformedMetadata), "got %v", err)
			assert.Less(t, alloc, uint64(64<<20), "allocated %d bytes", alloc)
		})
	}
}

func TestDecode_KeepsValidMetadata(t *testing.T) {
	orientation6 := exifOrientation(6)

	t.Run("JPEG EXIF orientation is still applied", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 16)), nil))
		src := buf.Bytes()
		data := append(append(append([]byte{}, src[:2]...), jpegSegment(0xE1, append([]byte("Exif\x00\x00"), orientation6...))...), src[2:]...)
		img, err := Decode(data)
		require.NoError(t, err)
		assert.Equal(t, image.Rect(0, 0, 16, 8), img.Bounds())
	})
	t.Run("PNG with a real iCCP", func(t *testing.T) {
		_, err := Decode(pngWithChunks(t, pngChunk("iCCP", iccpBody(t, nanoICC(t)))))
		require.NoError(t, err)
	})
	t.Run("JPEG with a real ICC", func(t *testing.T) {
		_, err := Decode(jpegWithSegments(t, jpegSegment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), nanoICC(t)...))))
		require.NoError(t, err)
	})
}

func TestCheckTIFFStructure_Edges(t *testing.T) {
	assert.NoError(t, checkTIFFStructure(nil))
	assert.NoError(t, checkTIFFStructure([]byte("XX*\x00\x08\x00\x00\x00")))
	// 大きい端でも型の大きさ込みで入力に収まるなら通す
	assert.NoError(t, checkTIFFStructure(tiffWithEntry(0x0100, 1, 4, 0)))
	// 未知の型は goexif がエラーにして確保しない
	assert.NoError(t, checkTIFFStructure(tiffWithEntry(0x0100, 99, 0xffffffff, 0)))
	// big endian
	be := []byte("MM\x00*\x00\x00\x00\x08\x00\x01\x01\x00\x00\x04\x40\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00")
	assert.True(t, errors.Is(checkTIFFStructure(be), ErrMalformedMetadata))
}

func TestCheckICCProfile_Edges(t *testing.T) {
	assert.NoError(t, checkICCProfile(make([]byte, 10)))
	assert.NoError(t, checkICCProfile(nanoICC(t)))
	inTable := nanoICC(t)
	binary.BigEndian.PutUint32(inTable[132+4:132+8], 10) // offset inside the header
	assert.True(t, errors.Is(checkICCProfile(inTable), ErrMalformedMetadata))
}

func TestInflateICCP_NotDecompressible(t *testing.T) {
	p, err := inflateICCP([]byte("no-terminator"))
	assert.NoError(t, err)
	assert.Nil(t, p)
	p, err = inflateICCP([]byte("icc\x00\x00not zlib"))
	assert.NoError(t, err)
	assert.Nil(t, p)
}
