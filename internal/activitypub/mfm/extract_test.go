package mfm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCollectEmojiCodes_Empty(t *testing.T) {
	assert.Nil(t, CollectEmojiCodes())
	assert.Nil(t, CollectEmojiCodes(""))
	assert.Nil(t, CollectEmojiCodes("", "", ""))
}

func TestCollectEmojiCodes_PlainText(t *testing.T) {
	assert.Nil(t, CollectEmojiCodes("hello world"))
}

func TestCollectEmojiCodes_Single(t *testing.T) {
	assert.Equal(t, []string{"foo"}, CollectEmojiCodes(":foo:"))
	assert.Equal(t, []string{"foo"}, CollectEmojiCodes("hello :foo: world"))
}

func TestCollectEmojiCodes_Multiple_DedupAndOrder(t *testing.T) {
	got := CollectEmojiCodes(":foo: :bar: :foo: :baz:")
	assert.Equal(t, []string{"foo", "bar", "baz"}, got)
}

func TestCollectEmojiCodes_NestedInsideMarkup(t *testing.T) {
	// MFM の bold / italic / fn 等の中にあっても拾えること
	got := CollectEmojiCodes("**:foo:** *:bar:* $[x2 :baz:]")
	assert.ElementsMatch(t, []string{"foo", "bar", "baz"}, got)
}

func TestCollectEmojiCodes_AcrossMultipleTexts(t *testing.T) {
	// text + cw のような複数 source からの dedup
	got := CollectEmojiCodes("body :foo:", "cw :bar:", "more :foo: :baz:")
	assert.Equal(t, []string{"foo", "bar", "baz"}, got)
}

func TestCollectEmojiCodes_IgnoresUnicodeEmoji(t *testing.T) {
	// Unicode emoji (😀) は NodeUnicodeEmoji なので含めない
	assert.Nil(t, CollectEmojiCodes("hello 😀 world"))
}

func TestCollectEmojiCodes_IgnoresMalformed(t *testing.T) {
	// 閉じ \`:\` が無いものは emoji code として parse されない
	assert.Nil(t, CollectEmojiCodes("hello :foo bar"))
	// 英数字 + _ + - 以外は emoji name として parse されない (parser.go::tryEmojiCode)
	assert.Nil(t, CollectEmojiCodes(":foo bar:"))
}

func TestCollectHashtags_Empty(t *testing.T) {
	assert.Nil(t, CollectHashtags())
	assert.Nil(t, CollectHashtags(""))
	assert.Nil(t, CollectHashtags("hello world"))
}

func TestCollectHashtags_Basic(t *testing.T) {
	assert.Equal(t, []string{"golang"}, CollectHashtags("hello #golang"))
	assert.Equal(t, []string{"go", "rust"}, CollectHashtags("learn #go and #rust"))
}

func TestCollectHashtags_DedupExactAndOrder(t *testing.T) {
	// exact dedup + first-occurrence 順 (case-insensitive dedup は呼び出し側 hashtag.Extract)
	assert.Equal(t, []string{"go", "GO"}, CollectHashtags("#go #go #GO"))
}

func TestCollectHashtags_NestedInsideMarkup(t *testing.T) {
	// fn ($[...]) など inline 子を持つノードの中の hashtag も AST walk で拾える
	assert.Equal(t, []string{"bar"}, CollectHashtags("$[x2 #bar]"))
	assert.ElementsMatch(t, []string{"foo", "bar"}, CollectHashtags("#foo $[spin #bar]"))
}

func TestCollectHashtags_ExcludesCodeURLMention(t *testing.T) {
	// code block / inline code / URL fragment / の中の # は hashtag にならない
	assert.Empty(t, CollectHashtags("```\n#secret\n```"))
	assert.Empty(t, CollectHashtags("inline `#var`"))
	assert.Empty(t, CollectHashtags("https://example.com/path#anchor"))
	// mention は hashtag ではない
	assert.Equal(t, []string{"real"}, CollectHashtags("@alice #real"))
}

func TestCollectHashtags_WordBoundary(t *testing.T) {
	// 直前が英数字なら hashtag にならない (foo#bar / #one#two の #two)
	assert.Nil(t, CollectHashtags("foo#bar"))
	assert.Equal(t, []string{"one"}, CollectHashtags("#one#two"))
}

func TestCollectHashtags_AcrossMultipleTexts(t *testing.T) {
	assert.Equal(t, []string{"foo", "bar"}, CollectHashtags("body #foo", "cw #bar"))
}

// ExtractCustomEmojis mirrors upstream extractCustomEmojisFromMfm: dedup'd in
// first-appearance order, nested nodes included, names over 100 UTF-16 code
// units skipped.
func TestExtractCustomEmojis(t *testing.T) {
	assert.Nil(t, ExtractCustomEmojis(nil))
	assert.Equal(t, []string{"a", "b"}, ExtractCustomEmojis(Parse("**:a:** $[x2 :b:] :a:")))
	assert.Nil(t, ExtractCustomEmojis(Parse("`:code:`")), "コードの中は emojiCode にならない")
	assert.Equal(t, []string{"code"}, ExtractCustomEmojis(ParseSimple("`:code:`")), "parseSimple はコードを解釈しない")

	long := strings.Repeat("a", 101)
	limit := strings.Repeat("b", 100)
	got := ExtractCustomEmojis([]*Node{
		withProp(NodeEmojiCode, "name", long),
		withProp(NodeEmojiCode, "name", limit),
	})
	assert.Equal(t, []string{limit}, got, "100 を超える名前は数えない")
}
