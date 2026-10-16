package drive

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **本番のアップロードは `GenerateWebpublic` ではなく `ProcessAll` を通る (#3609)。**
// このファイルのテストは共通の判定 (`skipWebpublicBeforeDecode` /
// `needsWebpublic`) の中身を守るが、`ProcessAll` の呼び出し側の条件は見ない。
// アニメーションやメタデータの条件を変えたら、`process_once_test.go` の
// `TestProcessAll_MatchesPerStepMethods` の表にも入力を足すこと。

// animatedGIF builds a multi-frame GIF of the given size.
func animatedGIF(t *testing.T, w, h, frames int) []byte {
	t.Helper()
	pal := color.Palette{color.Black, color.White}
	g := &gif.GIF{}
	for i := 0; i < frames; i++ {
		fr := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		fr.SetColorIndex(0, 0, uint8(i%2))
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	require.NoError(t, gif.EncodeAll(&buf, g))
	return buf.Bytes()
}

// **アニメーションを静止画に潰してまで縮めない (#3124 の続き)。**
//
// `encodeWebP` は 1 枚しか受けないので、アニメーション画像に webpublic を作ると
// 1 コマだけの静止画になる。`GetPublicURL` を通す経路 (= 他人に見せる側) が
// そこを指すため、アイコンやバナーがアニメーションを失う。upstream も
// `isAnimated` なら webpublic を作らない。
func TestGenerateWebpublic_AnimatedIsNotFlattened(t *testing.T) {
	p := &DefaultImageProcessor{}

	// 2048px を超えるアニメーション GIF。従来は「大きいから」という理由だけで
	// webpublic が作られ、1 コマの静止画になっていた。
	big := animatedGIF(t, 3000, 10, 2)
	require.True(t, hasStrippableMetadata(big, "image/gif") == false,
		"GIF はメタデータを持たない前提 (持つなら下の分岐に入る)")

	got, err := p.GenerateWebpublic(big, "image/gif")
	require.NoError(t, err)
	assert.Nil(t, got, "アニメーションには webpublic を作らない")

	// 2048px 以下は元から作られない (satisfyWebpublic)。ここが変わっていない
	// ことも見ておく。
	small := animatedGIF(t, 64, 64, 2)
	got, err = p.GenerateWebpublic(small, "image/gif")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// **メタデータがあるときは作る。** アニメーションを保つために撮影情報を残すのは
// 割に合わない。upstream より厳しい側に倒している。
func TestGenerateWebpublic_AnimatedWithMetadataIsStillStripped(t *testing.T) {
	p := &DefaultImageProcessor{}

	// APNG の MIME を名乗る PNG に eXIf を入れる。`isAnimatedMime` は MIME を
	// 見るので、これで「アニメーション扱い + メタデータあり」の組になる。
	body := pngWithChunk(t, "eXIf", []byte("Exif\x00\x00II*\x00"))
	require.True(t, hasStrippableMetadata(body, "image/apng"),
		"eXIf が検出されている前提")

	got, err := p.GenerateWebpublic(body, "image/apng")
	require.NoError(t, err)
	require.NotNil(t, got, "メタデータがあるならアニメーションより除去を優先する")
	assert.False(t, hasStrippableMetadata(got.Data, got.MimeType),
		"作った webpublic にメタデータが残っていない")
}

// **アニメーション WebP にも webpublic を作らない (#3128)。** `isAnimatedMime` は
// MIME 判定なので WebP を拾えず、他人に見せる側が 1 コマの静止画になっていた。
//
// **フィクスチャはデコードできるものを使う。** ヘッダだけの合成バイト列だと
// `decodeImage` の失敗で同じく `nil, nil` になり、判定を外しても落ちない。
func TestGenerateWebpublic_AnimatedWebPIsNotFlattened(t *testing.T) {
	p := &DefaultImageProcessor{}

	// 2048px を超えるので、判定が無ければ「大きいから」という理由で作られる。
	big := animatedWebPFixture(t, 3000, 8)
	require.False(t, hasStrippableMetadata(big, "image/webp"), "メタデータは持たない前提")
	// フィクスチャが本当にデコードできること (= 上の理由が成立すること) を先に見る。
	img, err := decodeImage(big, "image/webp")
	require.NoError(t, err)
	require.NotNil(t, img)

	got, err := p.GenerateWebpublic(big, "image/webp")
	require.NoError(t, err)
	assert.Nil(t, got, "アニメーション WebP には webpublic を作らない")

	// 2048px 以下は元から作られない (satisfyWebpublic)。
	small := animatedWebPFixture(t, 64, 64)
	got, err = p.GenerateWebpublic(small, "image/webp")
	require.NoError(t, err)
	assert.Nil(t, got)
}
