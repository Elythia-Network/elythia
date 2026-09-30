package federation_test

import (
	"fmt"
	"testing"

	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawAttachments returns n valid AP Document entries (image/png, no dimensions
// → 先頭取得 (probe) が発火する形)。
func rawAttachments(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"type":      "Document",
			"mediaType": "image/png",
			"url":       fmt.Sprintf("https://media.example/files/%d.png", i),
		})
	}
	return out
}

func rawEmojiTags(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"type": "Emoji",
			"name": fmt.Sprintf(":e%d:", i),
			"icon": map[string]any{"type": "Image", "url": fmt.Sprintf("https://media.example/e%d.png", i)},
		})
	}
	return out
}

// 添付は件数の上限で打ち切ること。1 件につき drive_file の SELECT + INSERT と
// (image かつ width/height 欠落なら) 外向き GET が直列で走るので、上限が無いと
// 署名付き POST 1 通で inbox worker を長時間占有できる。
//
// 上限値はローカルの受け入れ上限 (notes/create の fileIds maxItems: 16) と同じ。
func TestExtractAttachments_CapsCount(t *testing.T) {
	// 境界ちょうどは全部通る。
	assert.Len(t, federation.ExtractAttachments(rawAttachments(16), false), 16)
	// +1 で打ち切られる。
	assert.Len(t, federation.ExtractAttachments(rawAttachments(17), false), 16)
	// 大量でも同じ。
	assert.Len(t, federation.ExtractAttachments(rawAttachments(3000), false), 16)
	// 上限未満は素通り (上限を「常に 16 件に詰める」実装と区別する)。
	assert.Len(t, federation.ExtractAttachments(rawAttachments(3), false), 3)
}

// 打ち切りは**採用した件数**で数えること。raw の要素数で数えると、type が
// 合わない要素を前に並べるだけで有効な添付が削られる。
func TestExtractAttachments_CapCountsAcceptedEntries(t *testing.T) {
	raw := make([]any, 0, 66)
	for i := 0; i < 50; i++ {
		// validDocumentTypes 外 + url 無しで、どちらの理由でも落ちる要素。
		raw = append(raw, map[string]any{"type": "Tombstone"})
	}
	raw = append(raw, rawAttachments(16)...)

	got := federation.ExtractAttachments(raw, false)
	require.Len(t, got, 16, "採用されない要素が cap を食っている")
	assert.Equal(t, "https://media.example/files/0.png", got[0].URL)
}

// 絵文字 tag も同様に打ち切ること。1 件につき emoji 行の INSERT / UPDATE が
// 走り、名前は note.emojis / user.emojis (varchar(128)[]) にも載る。
//
// **hashtag の 32 には揃えない (レビュー M3)。** あちらは upstream 自身が切って
// いるが絵文字は切っておらず、33 種類以上使うノート (絵文字アート) は珍しく
// ない。実用上まず届かない値まで上げて、無制限の fan-out だけを止める。
func TestExtractEmojiTags_CapsCount(t *testing.T) {
	assert.Len(t, federation.ExtractEmojiTags(rawEmojiTags(128)), 128)
	assert.Len(t, federation.ExtractEmojiTags(rawEmojiTags(129)), 128)
	assert.Len(t, federation.ExtractEmojiTags(rawEmojiTags(5000)), 128)
	assert.Len(t, federation.ExtractEmojiTags(rawEmojiTags(4)), 4)
	// 絵文字アートの実サイズが落ちないこと。
	assert.Len(t, federation.ExtractEmojiTags(rawEmojiTags(64)), 64)
}

// 絵文字も採用件数で数えること。
func TestExtractEmojiTags_CapCountsAcceptedEntries(t *testing.T) {
	raw := make([]any, 0, 228)
	for i := 0; i < 100; i++ {
		raw = append(raw, map[string]any{"type": "Hashtag", "name": "#x"})
	}
	raw = append(raw, rawEmojiTags(128)...)

	got := federation.ExtractEmojiTags(raw)
	require.Len(t, got, 128)
	assert.Equal(t, ":e0:", got[0].Name)
}
