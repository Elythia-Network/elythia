package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const testHost = "example.com"

func TestToHTML_EmptyInput(t *testing.T) {
	assert.Equal(t, "", ToHTML(nil, testHost))
}

func TestToHTML_PlainText(t *testing.T) {
	nodes := Parse("hello world")
	assert.Equal(t, "hello world", ToHTML(nodes, testHost))
}

func TestToHTML_TextWithNewline(t *testing.T) {
	nodes := Parse("hello\nworld")
	assert.Equal(t, "hello<br />world", ToHTML(nodes, testHost))
}

func TestToHTML_HTMLEscape(t *testing.T) {
	nodes := []*Node{Text("<script>alert(1)</script>")}
	assert.Equal(t, "&lt;script&gt;alert(1)&lt;/script&gt;", ToHTML(nodes, testHost))
}

func TestToHTML_Bold(t *testing.T) {
	nodes := Parse("**bold**")
	assert.Equal(t, "<b>bold</b>", ToHTML(nodes, testHost))
}

func TestToHTML_Italic(t *testing.T) {
	nodes := Parse("<i>italic</i>")
	assert.Equal(t, "<i>italic</i>", ToHTML(nodes, testHost))
}

func TestToHTML_Strike(t *testing.T) {
	nodes := Parse("~~strike~~")
	assert.Equal(t, "<del>strike</del>", ToHTML(nodes, testHost))
}

func TestToHTML_Small(t *testing.T) {
	nodes := Parse("<small>small</small>")
	assert.Equal(t, "<small>small</small>", ToHTML(nodes, testHost))
}

func TestToHTML_Center(t *testing.T) {
	nodes := Parse("<center>centered</center>")
	assert.Equal(t, `<div style="text-align: center;">centered</div>`, ToHTML(nodes, testHost))
}

// TestToHTML_CenterMidLineIsText fixes that a <center> in the middle of a line
// stays text, as mfm-js reads it (#3328).
func TestToHTML_CenterMidLineIsText(t *testing.T) {
	nodes := Parse("x <center>a</center>")
	assert.True(t, IsSimple(nodes))
	assert.Equal(t, "x &lt;center&gt;a&lt;/center&gt;", ToHTML(nodes, testHost))
}

func TestToHTML_InlineCode(t *testing.T) {
	nodes := Parse("`code`")
	assert.Equal(t, "<code>code</code>", ToHTML(nodes, testHost))
}

func TestToHTML_InlineCode_Escapes(t *testing.T) {
	nodes := []*Node{{Type: NodeInlineCode, Props: map[string]any{"code": "<b>x</b>"}}}
	assert.Equal(t, "<code>&lt;b&gt;x&lt;/b&gt;</code>", ToHTML(nodes, testHost))
}

func TestToHTML_BlockCode(t *testing.T) {
	nodes := Parse("```\nhello\n```")
	assert.Equal(t, "<pre><code>hello</code></pre>", ToHTML(nodes, testHost))
}

func TestToHTML_MathInline(t *testing.T) {
	nodes := Parse(`\(x^2\)`)
	assert.Equal(t, "<code>x^2</code>", ToHTML(nodes, testHost))
}

func TestToHTML_MathBlock(t *testing.T) {
	nodes := Parse(`\[E=mc^2\]`)
	assert.Equal(t, "<pre><code>E=mc^2</code></pre>", ToHTML(nodes, testHost))
}

func TestToHTML_Quote(t *testing.T) {
	nodes := Parse("> quoted")
	assert.Equal(t, "<blockquote>quoted</blockquote>", ToHTML(nodes, testHost))
}

func TestToHTML_Search(t *testing.T) {
	nodes := Parse("hello search")
	assert.Contains(t, ToHTML(nodes, testHost), `href="https://www.google.com/search?q=hello"`)
	assert.Contains(t, ToHTML(nodes, testHost), "hello search</a>")
}

func TestToHTML_URL(t *testing.T) {
	nodes := Parse("https://example.com/path")
	result := ToHTML(nodes, testHost)
	assert.Contains(t, result, `href="https://example.com/path"`)
}

func TestToHTML_Link(t *testing.T) {
	nodes := Parse("[label](https://example.com)")
	result := ToHTML(nodes, testHost)
	assert.Equal(t, `<a href="https://example.com/">label</a>`, result)
}

func TestToHTML_Mention_Local(t *testing.T) {
	nodes := Parse("@alice")
	result := ToHTML(nodes, testHost)
	assert.Contains(t, result, `href="https://example.com/@alice"`)
	assert.Contains(t, result, `class="u-url mention"`)
	assert.Contains(t, result, "@alice</a>")
}

func TestToHTML_Mention_Remote(t *testing.T) {
	nodes := Parse("@bob@remote.example")
	result := ToHTML(nodes, testHost)
	assert.Contains(t, result, `href="https://remote.example/@bob"`)
	assert.Contains(t, result, "@bob@remote.example</a>")
}

func TestToHTML_Hashtag(t *testing.T) {
	nodes := Parse("#hello")
	result := ToHTML(nodes, testHost)
	assert.Contains(t, result, `href="https://example.com/tags/hello"`)
	assert.Contains(t, result, `rel="tag"`)
	assert.Contains(t, result, "#hello</a>")
}

func TestToHTML_UnicodeEmoji(t *testing.T) {
	nodes := []*Node{withProp(NodeUnicodeEmoji, "emoji", "😀")}
	assert.Equal(t, "😀", ToHTML(nodes, testHost))
}

func TestToHTML_EmojiCode(t *testing.T) {
	nodes := Parse(":thinking:")
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "\u200b:thinking:\u200b", result)
}

func TestToHTML_Plain(t *testing.T) {
	nodes := Parse("<plain>**not bold**</plain>")
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "<span>**not bold**</span>", result)
}

func TestToHTML_Fn_Unixtime(t *testing.T) {
	nodes := Parse("$[unixtime 0]")
	result := ToHTML(nodes, testHost)
	assert.Equal(t, `<time datetime="1970-01-01T00:00:00.000Z">1970-01-01T00:00:00.000Z</time>`, result)
}

func TestToHTML_Fn_Ruby(t *testing.T) {
	nodes := Parse("$[ruby base ruby]")
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "<ruby>base<rp>(</rp><rt>ruby</rt><rp>)</rp></ruby>", result)
}

func TestToHTML_Fn_Unknown(t *testing.T) {
	nodes := Parse("$[sparkle content]")
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "<i>content</i>", result)
}

func TestToHTML_NestedBoldItalic(t *testing.T) {
	nodes := Parse("**<i>bold italic</i>**")
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "<b><i>bold italic</i></b>", result)
}

func TestToHTML_MixedContent(t *testing.T) {
	nodes := Parse("hello **bold** world")
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "hello <b>bold</b> world", result)
}

func TestIsSimple_SimpleNodes(t *testing.T) {
	nodes := Parse("hello @alice #tag https://example.com :emoji: 😀")
	assert.True(t, IsSimple(nodes))
}

func TestIsSimple_WithBold(t *testing.T) {
	nodes := Parse("**bold**")
	assert.False(t, IsSimple(nodes))
}

func TestIsSimple_WithCode(t *testing.T) {
	nodes := Parse("`code`")
	assert.False(t, IsSimple(nodes))
}

func TestIsSimple_Empty(t *testing.T) {
	assert.True(t, IsSimple(nil))
}

func TestToHTML_Fn_Ruby_NoArgs(t *testing.T) {
	// ruby without rt arg → italic fallback
	nodes := []*Node{{
		Type:     NodeFn,
		Props:    map[string]any{"name": "ruby"},
		Children: []*Node{Text("base")},
	}}
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "<i>base</i>", result)
}

func TestToHTML_Link_JavascriptXSS(t *testing.T) {
	// javascript: スキームはリンク化せず、本家が URL を読めないときと同じ
	// `[文字](url)` の文字にする。mfm-js の link は http(s) しか作らないので
	// 本家では起きないが、本家の `new URL()` は javascript: を読んでリンクにする
	nodes := []*Node{{
		Type:     NodeLink,
		Props:    map[string]any{"url": `javascript:alert("1")`},
		Children: []*Node{Text("click me")},
	}}
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "[click me](javascript:alert(&quot;1&quot;))", result)
	assert.NotContains(t, result, "<a")
}

func TestToHTML_Link_DataScheme(t *testing.T) {
	// data: スキームもリンク化しない
	nodes := []*Node{{
		Type:     NodeLink,
		Props:    map[string]any{"url": "data:text/html,<script>alert(1)</script>"},
		Children: []*Node{Text("label")},
	}}
	result := ToHTML(nodes, testHost)
	assert.NotContains(t, result, "<a")
	assert.Contains(t, result, "label")
}

func TestToHTML_Fn_Unixtime_Invalid(t *testing.T) {
	// non-numeric → italic fallback
	nodes := []*Node{{
		Type:     NodeFn,
		Props:    map[string]any{"name": "unixtime"},
		Children: []*Node{Text("invalid")},
	}}
	result := ToHTML(nodes, testHost)
	assert.Equal(t, "<i>invalid</i>", result)
}
