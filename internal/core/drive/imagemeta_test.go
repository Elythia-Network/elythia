package drive

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **本番のアップロードは `GenerateWebpublic` ではなく `ProcessAll` を通る (#3609)。**
// ここのテストは `hasStrippableMetadata` の中身を守るが、`ProcessAll` の呼び出し側の
// 条件は見ない。メタデータの条件を変えたら、`process_once_test.go` の
// `TestProcessAll_MatchesPerStepMethods` の表にも入力を足すこと。

// **メタデータの検出を JPEG 以外にも広げる (#3037)。**
//
// 以前は JPEG の APP1 しか見ていなかったので、PNG / WebP / TIFF に GPS を
// 埋めたまま 2048px 以下でアップロードすると webpublic が作られず、
// **他人に見せる url が原本を指したまま**になっていた。

// pngWithChunk builds a PNG with an extra chunk inserted before IEND.
func pngWithChunk(t *testing.T, typ string, payload []byte) []byte {
	t.Helper()
	src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	src.Set(0, 0, color.NRGBA{R: 1, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, src))
	base := buf.Bytes()

	iend := bytes.Index(base, []byte("IEND"))
	require.Positive(t, iend)
	iendStart := iend - 4

	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(len(payload)))
	chunk.WriteString(typ)
	chunk.Write(payload)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(payload)
	_ = binary.Write(&chunk, binary.BigEndian, crc.Sum32())

	var out bytes.Buffer
	out.Write(base[:iendStart])
	out.Write(chunk.Bytes())
	out.Write(base[iendStart:])
	return out.Bytes()
}

func TestHasStrippableMetadata_PNG(t *testing.T) {
	// **IEND の直前に置く。** `eXIf` は IDAT の後ろにも置けるので、IDAT で
	// 走査を止める実装だとここで落ちる。
	assert.True(t, hasStrippableMetadata(pngWithChunk(t, "eXIf", []byte("II\x2a\x00")), "image/png"),
		"PNG の eXIf を見落としている")

	xmp := append([]byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00"), []byte("<x:xmpmeta/>")...)
	assert.True(t, hasStrippableMetadata(pngWithChunk(t, "iTXt", xmp), "image/png"),
		"PNG の XMP を見落としている")

	// **無害な注記は拾わない。** `tEXt` の `Software` などで webpublic を
	// 作り始めると、ほぼ全ての PNG が再エンコードされる。
	assert.False(t, hasStrippableMetadata(pngWithChunk(t, "tEXt", []byte("Software\x00mk-go")), "image/png"))
	// XMP でない iTXt も拾わない。
	assert.False(t, hasStrippableMetadata(pngWithChunk(t, "iTXt", []byte("Comment\x00\x00\x00\x00\x00hi")), "image/png"))
}

// riffWebP builds a minimal VP8X WebP carrying the given chunks.
func riffWebP(chunks ...[2]string) []byte {
	var body bytes.Buffer
	// VP8X (10 バイト固定)。
	body.WriteString("VP8X")
	_ = binary.Write(&body, binary.LittleEndian, uint32(10))
	body.Write(make([]byte, 10))
	for _, c := range chunks {
		body.WriteString(c[0])
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(c[1])))
		body.WriteString(c[1])
		if len(c[1])%2 == 1 {
			body.WriteByte(0)
		}
	}
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(4+body.Len()))
	out.WriteString("WEBP")
	out.Write(body.Bytes())
	return out.Bytes()
}

func TestHasStrippableMetadata_WebP(t *testing.T) {
	assert.True(t, hasStrippableMetadata(riffWebP([2]string{"EXIF", "II\x2a\x00"}), "image/webp"),
		"WebP の EXIF を見落としている")
	assert.True(t, hasStrippableMetadata(riffWebP([2]string{"XMP ", "<x:xmpmeta/>"}), "image/webp"),
		"WebP の XMP を見落としている")
	// **奇数長チャンクの後ろも歩ける。** パディングを数えないと以降の
	// チャンクが読めなくなる。
	assert.True(t, hasStrippableMetadata(
		riffWebP([2]string{"ICCP", "odd"}, [2]string{"EXIF", "II\x2a\x00"}), "image/webp"),
		"奇数長チャンクのパディングを数えていない")
	assert.False(t, hasStrippableMetadata(riffWebP([2]string{"ALPH", "x"}), "image/webp"))
	assert.False(t, hasStrippableMetadata([]byte("RIFF\x00\x00\x00\x00WEBP"), "image/webp"))
}

// TIFF は IFD そのものがメタデータ。EXIF タグも GPS タグも本体に直接載る。
func TestHasStrippableMetadata_TIFF(t *testing.T) {
	assert.True(t, hasStrippableMetadata([]byte("II\x2a\x00\x08\x00\x00\x00"), "image/tiff"), "little endian")
	assert.True(t, hasStrippableMetadata([]byte("MM\x00\x2a\x00\x00\x00\x08"), "image/tiff"), "big endian")
}

// **壊れた入力で読み過ぎない。** 長さを信用して飛ばすと範囲外を読む。
func TestHasStrippableMetadata_MalformedInputIsSafe(t *testing.T) {
	require.NotPanics(t, func() {
		// PNG signature + 途中で切れたチャンクヘッダ。
		assert.False(t, hasStrippableMetadata([]byte("\x89PNG\r\n\x1a\n\x00\x00"), "image/png"))
		// length が本体より大きい PNG チャンク。
		big := append([]byte("\x89PNG\r\n\x1a\n"), 0xFF, 0xFF, 0xFF, 0xF0)
		big = append(big, []byte("eXIf")...)
		assert.False(t, hasStrippableMetadata(big, "image/png"))
		// size が本体より大きい WebP チャンク。
		w := []byte("RIFF\x00\x00\x00\x00WEBPVP8X\xff\xff\xff\xf0")
		assert.False(t, hasStrippableMetadata(w, "image/webp"))
		assert.False(t, hasStrippableMetadata([]byte("RIFF"), "image/webp"))
		assert.False(t, hasStrippableMetadata([]byte("II"), "image/tiff"))
	})
}

// 判定は中身で行う。MIME が嘘でも実体に従う。
func TestHasStrippableMetadata_IgnoresLyingMIME(t *testing.T) {
	jpegWithExif := makeTestJPEGWithExif(10, 10)
	assert.True(t, hasStrippableMetadata(jpegWithExif, "image/png"), "MIME を信じて中身を見ていない")
	assert.False(t, hasStrippableMetadata(makeTestPNG(10, 10), "image/jpeg"))
}

// **`GenerateWebpublic` が実際に `hasStrippableMetadata` を見ていること。**
//
// ヘルパー単体のテストだけでは配線が固定できない (敵対的レビューで実測:
// `hasStrippableMetadata` を修正前の JPEG 限定に戻してもテストが緑のままだった)。
// 2048px 以下でも**メタデータがあれば webpublic を作る**、というのがこの分岐の
// 仕事で、作られないと `GetPublicURL` が原本を指して GPS が公開側へ出る。
func TestGenerateWebpublic_UsesTheWidenedMetadataDetection(t *testing.T) {
	p := NewDefaultImageProcessor()

	// eXIf を持つ小さい PNG。寸法は 2048 以下なので、メタデータを見て
	// いなければ webpublic は作られない。
	withMeta := pngWithChunk(t, "eXIf", []byte("II\x2a\x00\x08\x00\x00\x00"))
	got, err := p.GenerateWebpublic(withMeta, "image/png")
	require.NoError(t, err)
	assert.NotNil(t, got, "PNG の eXIf を見ていない (webpublic が作られない)")

	// ImageMagick が書く zTXt 形式も同じ。
	withRaw := pngWithChunk(t, "zTXt", []byte("Raw profile type exif\x00\x00"))
	got, err = p.GenerateWebpublic(withRaw, "image/png")
	require.NoError(t, err)
	assert.NotNil(t, got, "ImageMagick の Raw profile type exif を見ていない")

	// **メタデータが無ければ作らない。** これが無いと「常に作る」実装でも
	// 上のテストが通り、2048px 以下の全画像が再エンコードされる。
	plain := pngWithChunk(t, "tEXt", []byte("Software\x00mk-go"))
	got, err = p.GenerateWebpublic(plain, "image/png")
	require.NoError(t, err)
	assert.Nil(t, got, "無害な注記で webpublic を作っている")
}

// JPEG の XMP / IPTC も見る (upstream の `exif ?? iptc ?? xmp ?? tifftagPhotoshop`)。
//
// **GPS を XMP にだけ持つ JPEG** は Lightroom / Photoshop の書き出しでよくある。
// EXIF だけ見ていると 2048px 以下でそれが素通りする。
func TestHasStrippableMetadata_JPEGXMPAndIPTC(t *testing.T) {
	base := makeTestJPEG(10, 10)
	for name, marker := range map[string]string{
		"XMP":    "http://ns.adobe.com/xap/1.0/\x00",
		"拡張 XMP": "http://ns.adobe.com/xmp/extension/",
		"IPTC":   "Photoshop 3.0\x00",
	} {
		t.Run(name, func(t *testing.T) {
			// SOI の直後にマーカーを差し込む (先頭 64KB 以内であればよい)。
			doctored := append(append([]byte{}, base[:2]...), append([]byte(marker), base[2:]...)...)
			assert.True(t, hasStrippableMetadata(doctored, "image/jpeg"), "%s を見落としている", name)
		})
	}
	assert.False(t, hasStrippableMetadata(base, "image/jpeg"), "素の JPEG を拾っている")
}
