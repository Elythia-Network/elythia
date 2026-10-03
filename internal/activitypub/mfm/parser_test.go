package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_PlainText(t *testing.T) {
	nodes := Parse("hello world")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
	assert.Equal(t, "hello world", nodes[0].textValue())
}

func TestParse_EmptyInput(t *testing.T) {
	nodes := Parse("")
	assert.Nil(t, nodes)
}

func TestParse_Bold_Asterisks(t *testing.T) {
	nodes := Parse("**bold**")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeBold, nodes[0].Type)
	require.Len(t, nodes[0].Children, 1)
	assert.Equal(t, "bold", nodes[0].Children[0].textValue())
}

func TestParse_Bold_Tag(t *testing.T) {
	nodes := Parse("<b>bold</b>")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeBold, nodes[0].Type)
}

func TestParse_Bold_Underscores(t *testing.T) {
	nodes := Parse("__bold text__")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeBold, nodes[0].Type)
	require.Len(t, nodes[0].Children, 1)
	assert.Equal(t, "bold text", nodes[0].Children[0].textValue())
}

func TestParse_Italic_Asterisk(t *testing.T) {
	nodes := Parse("*italic*")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeItalic, nodes[0].Type)
}

func TestParse_Italic_Tag(t *testing.T) {
	nodes := Parse("<i>italic</i>")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeItalic, nodes[0].Type)
}

func TestParse_Italic_Underscore(t *testing.T) {
	nodes := Parse("_italic_")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeItalic, nodes[0].Type)
}

func TestParse_Strike_Wave(t *testing.T) {
	nodes := Parse("~~strike~~")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeStrike, nodes[0].Type)
	require.Len(t, nodes[0].Children, 1)
	assert.Equal(t, "strike", nodes[0].Children[0].textValue())
}

func TestParse_Strike_Tag(t *testing.T) {
	nodes := Parse("<s>strike</s>")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeStrike, nodes[0].Type)
}

func TestParse_InlineCode(t *testing.T) {
	nodes := Parse("`code`")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeInlineCode, nodes[0].Type)
	assert.Equal(t, "code", nodes[0].Props["code"])
}

func TestParse_BlockCode(t *testing.T) {
	input := "```go\nfmt.Println()\n```"
	nodes := Parse(input)
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeBlockCode, nodes[0].Type)
	assert.Equal(t, "fmt.Println()", nodes[0].Props["code"])
	assert.Equal(t, "go", nodes[0].Props["lang"])
}

func TestParse_BlockCode_NoLang(t *testing.T) {
	input := "```\nhello\n```"
	nodes := Parse(input)
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeBlockCode, nodes[0].Type)
	assert.Equal(t, "hello", nodes[0].Props["code"])
}

func TestParse_MathInline(t *testing.T) {
	nodes := Parse(`\(x^2\)`)
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeMathInline, nodes[0].Type)
	assert.Equal(t, "x^2", nodes[0].Props["formula"])
}

func TestParse_MathBlock(t *testing.T) {
	nodes := Parse(`\[E=mc^2\]`)
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeMathBlock, nodes[0].Type)
	assert.Equal(t, "E=mc^2", nodes[0].Props["formula"])
}

func TestParse_Quote(t *testing.T) {
	nodes := Parse("> quoted text")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeQuote, nodes[0].Type)
	require.Len(t, nodes[0].Children, 1)
	assert.Equal(t, "quoted text", nodes[0].Children[0].textValue())
}

func TestParse_Quote_MultiLine(t *testing.T) {
	nodes := Parse("> line 1\n> line 2")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeQuote, nodes[0].Type)
}

func TestParse_Center(t *testing.T) {
	nodes := Parse("<center>centered</center>")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeCenter, nodes[0].Type)
}

func TestParse_Small(t *testing.T) {
	nodes := Parse("<small>small text</small>")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeSmall, nodes[0].Type)
}

func TestParse_Plain(t *testing.T) {
	nodes := Parse("<plain>**not bold**</plain>")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodePlain, nodes[0].Type)
	require.Len(t, nodes[0].Children, 1)
	assert.Equal(t, "**not bold**", nodes[0].Children[0].textValue())
}

func TestParse_Mention_Local(t *testing.T) {
	nodes := Parse("@alice")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeMention, nodes[0].Type)
	assert.Equal(t, "alice", nodes[0].Props["username"])
	assert.Nil(t, nodes[0].Props["host"])
	assert.Equal(t, "@alice", nodes[0].Props["acct"])
}

func TestParse_Mention_Remote(t *testing.T) {
	nodes := Parse("@bob@remote.example")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeMention, nodes[0].Type)
	assert.Equal(t, "bob", nodes[0].Props["username"])
	assert.Equal(t, "remote.example", nodes[0].Props["host"])
	assert.Equal(t, "@bob@remote.example", nodes[0].Props["acct"])
}

func TestParse_Mention_NotAfterAlphanumeric(t *testing.T) {
	nodes := Parse("a@alice")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_Hashtag(t *testing.T) {
	nodes := Parse("#hello")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeHashtag, nodes[0].Type)
	assert.Equal(t, "hello", nodes[0].Props["hashtag"])
}

func TestParse_Hashtag_DigitsOnly(t *testing.T) {
	nodes := Parse("#123")
	require.Len(t, nodes, 1)
	// 数字だけのハッシュタグは無効
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_Hashtag_NotAfterAlphanumeric(t *testing.T) {
	nodes := Parse("a#tag")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_EmojiCode(t *testing.T) {
	nodes := Parse(":thinking:")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeEmojiCode, nodes[0].Type)
	assert.Equal(t, "thinking", nodes[0].Props["name"])
}

// TestParse_EmojiCodeBoundaryMatchesMfmJs fixes the emojiCode boundary to
// mfm-js 0.26.0 (#3297). Expected values were produced by running mfm-js's
// parse / parseSimple on each input.
//
// mfm-js は閉じの `:` の直後が英数字かだけを見て、直前の文字は見ない。以前の
// mk-go は逆に直前を見ていたので、`1:a:` や `@foo:a:` が絵文字にならず、
// `:a:1` が絵文字になっていた。
func TestParse_EmojiCodeBoundaryMatchesMfmJs(t *testing.T) {
	cases := []struct {
		in           string
		full, simple []string
	}{
		{"1:a:", []string{"a"}, []string{"a"}},
		{"a:b: c", []string{"b"}, []string{"b"}},
		{"@foo:a:", []string{"a"}, []string{"a"}},
		{"#tag:a:", []string{"a"}, []string{"a"}},
		{":a:1", nil, nil},
		{":a:b", nil, nil},
		{"a:thinking:", []string{"thinking"}, []string{"thinking"}},
		{":a:", []string{"a"}, []string{"a"}},
		{":a: :b:", []string{"a", "b"}, []string{"a", "b"}},
		{":a::b:", []string{"a", "b"}, []string{"a", "b"}},
		{"a:b:c", nil, nil},
		{"x :a:", []string{"a"}, []string{"a"}},
		{"\u65e5\u672c:a:\u8a9e", []string{"a"}, []string{"a"}},
		{":a:\u65e5\u672c", []string{"a"}, []string{"a"}},
		{":a:_", []string{"a"}, []string{"a"}},
		{":a:-", []string{"a"}, []string{"a"}},
		{":A:Z", nil, nil},
		{":a:\n", []string{"a"}, []string{"a"}},
		{":a:.", []string{"a"}, []string{"a"}},
		{"**:a:**", []string{"a"}, []string{"a"}},
		{"(:a:)", []string{"a"}, []string{"a"}},
		{":a+b-c_d:", []string{"a+b-c_d"}, []string{"a+b-c_d"}},
		{"https://x.example/:a:", nil, []string{"a"}},
		{"[l](https://x.example):a:", []string{"a"}, []string{"a"}},
		{"`:a:`", nil, []string{"a"}},
		{"::a::", []string{"a"}, []string{"a"}},
		{":a:b:c:", []string{"c"}, []string{"c"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.full, emojiCodeNames(Parse(tc.in)), "parse")
			assert.Equal(t, tc.simple, emojiCodeNames(ParseSimple(tc.in)), "parseSimple")
		})
	}
}

func TestParse_UnicodeEmoji(t *testing.T) {
	nodes := Parse("😀")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
	assert.Equal(t, "😀", nodes[0].Props["emoji"])
}

func TestParse_URL(t *testing.T) {
	nodes := Parse("https://example.com/path")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeURL, nodes[0].Type)
	assert.Equal(t, "https://example.com/path", nodes[0].Props["url"])
}

func TestParse_URL_TrailingPunctuation(t *testing.T) {
	nodes := Parse("https://example.com.")
	require.Len(t, nodes, 2) // URL + text "."
	assert.Equal(t, NodeURL, nodes[0].Type)
	assert.Equal(t, "https://example.com", nodes[0].Props["url"])
}

func TestParse_URL_WithParens(t *testing.T) {
	nodes := Parse("https://example.com/(path)")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeURL, nodes[0].Type)
	assert.Equal(t, "https://example.com/(path)", nodes[0].Props["url"])
}

func TestParse_Link(t *testing.T) {
	nodes := Parse("[label](https://example.com)")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeLink, nodes[0].Type)
	assert.Equal(t, "https://example.com", nodes[0].Props["url"])
	assert.Equal(t, false, nodes[0].Props["silent"])
}

func TestParse_Link_Silent(t *testing.T) {
	nodes := Parse("?[label](https://example.com)")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeLink, nodes[0].Type)
	assert.Equal(t, true, nodes[0].Props["silent"])
}

func TestParse_Fn(t *testing.T) {
	nodes := Parse("$[tada hello]")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeFn, nodes[0].Type)
	assert.Equal(t, "tada", nodes[0].Props["name"])
}

func TestParse_Fn_WithArgs(t *testing.T) {
	nodes := Parse("$[ruby.rt=text base]")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeFn, nodes[0].Type)
	assert.Equal(t, "ruby", nodes[0].Props["name"])
	args, ok := nodes[0].Props["args"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "text", args["rt"])
}

func TestParse_Fn_BoolArg(t *testing.T) {
	nodes := Parse("$[spin.x content]")
	require.Len(t, nodes, 1)
	args := nodes[0].Props["args"].(map[string]any)
	assert.Equal(t, true, args["x"])
}

func TestParse_Search(t *testing.T) {
	nodes := Parse("hello search")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeSearch, nodes[0].Type)
	assert.Equal(t, "hello", nodes[0].Props["query"])
}

func TestParse_Search_Japanese(t *testing.T) {
	nodes := Parse("hello 検索")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeSearch, nodes[0].Type)
}

func TestParse_NestedBoldItalic(t *testing.T) {
	nodes := Parse("**<i>bold italic</i>**")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeBold, nodes[0].Type)
	require.Len(t, nodes[0].Children, 1)
	assert.Equal(t, NodeItalic, nodes[0].Children[0].Type)
}

func TestParse_MixedContent(t *testing.T) {
	nodes := Parse("hello **bold** world")
	require.Len(t, nodes, 3)
	assert.Equal(t, NodeText, nodes[0].Type)
	assert.Equal(t, NodeBold, nodes[1].Type)
	assert.Equal(t, NodeText, nodes[2].Type)
}

func TestParse_MentionInText(t *testing.T) {
	nodes := Parse("hello @alice!")
	require.Len(t, nodes, 3)
	assert.Equal(t, NodeText, nodes[0].Type)
	assert.Equal(t, NodeMention, nodes[1].Type)
	assert.Equal(t, NodeText, nodes[2].Type)
}

func TestParseSimple_Text(t *testing.T) {
	nodes := ParseSimple("**not bold**")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
	assert.Equal(t, "**not bold**", nodes[0].textValue())
}

func TestParseSimple_EmojiCode(t *testing.T) {
	nodes := ParseSimple(":smile:")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeEmojiCode, nodes[0].Type)
}

func TestParseSimple_UnicodeEmoji(t *testing.T) {
	nodes := ParseSimple("😀 hello")
	require.Len(t, nodes, 2)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
	assert.Equal(t, NodeText, nodes[1].Type)
}

func TestParseSimple_PlainTag(t *testing.T) {
	nodes := ParseSimple("<plain>text</plain>")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodePlain, nodes[0].Type)
}

func TestParse_Hashtag_WithBrackets(t *testing.T) {
	nodes := Parse("#tag(sub)")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeHashtag, nodes[0].Type)
	assert.Equal(t, "tag(sub)", nodes[0].Props["hashtag"])
}

func TestParse_MultilineQuote(t *testing.T) {
	input := "> line1\n> line2\nnot quote"
	nodes := Parse(input)
	// quote + text
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeQuote, nodes[0].Type)
}

func TestParse_Hashtag_JapaneseBrackets(t *testing.T) {
	nodes := Parse("#tag「sub」")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeHashtag, nodes[0].Type)
	assert.Equal(t, "tag「sub」", nodes[0].Props["hashtag"])
}

func TestParse_Hashtag_FullWidthParens(t *testing.T) {
	nodes := Parse("#tag（sub）")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeHashtag, nodes[0].Type)
	assert.Equal(t, "tag（sub）", nodes[0].Props["hashtag"])
}

func TestParse_Hashtag_SquareBrackets(t *testing.T) {
	nodes := Parse("#tag[sub]")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeHashtag, nodes[0].Type)
	assert.Equal(t, "tag[sub]", nodes[0].Props["hashtag"])
}

func TestParse_UnicodeEmoji_WithSkinTone(t *testing.T) {
	// 👋🏻 = U+1F44B U+1F3FB
	nodes := Parse("👋🏻")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_DingbatRange(t *testing.T) {
	// ✅ = U+2705
	nodes := Parse("✅")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_RegionalIndicator(t *testing.T) {
	// 🇯🇵 = U+1F1EF U+1F1F5
	nodes := Parse("🇯🇵")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_Arrow(t *testing.T) {
	// ⬛ = U+2B1B
	nodes := Parse("⬛")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_LetterlikeSymbol(t *testing.T) {
	// ™ = U+2122。mfm-js (emoji-data の正規表現) は U+FE0F が続くときだけ絵文字にする (#3324)
	nodes := Parse("™")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
	nodes = Parse("™\ufe0f")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_ArrowRange(t *testing.T) {
	// ← = U+2190 は emoji-data の一覧に無いので、mfm-js と同じく文字 (#3324)。
	// ↔ = U+2194 は一覧にある
	nodes := Parse("←")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
	nodes = Parse("↔")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_Copyright(t *testing.T) {
	// © = U+00A9。mfm-js は U+FE0F が続くときだけ絵文字にする (#3324)
	nodes := Parse("©")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
	nodes = Parse("©\ufe0f")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_Registered(t *testing.T) {
	// ® = U+00AE。mfm-js は U+FE0F が続くときだけ絵文字にする (#3324)
	nodes := Parse("®")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
	nodes = Parse("®\ufe0f")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_ExclamationMarks(t *testing.T) {
	// ‼ = U+203C
	nodes := Parse("‼")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_MiscTechnical(t *testing.T) {
	// ⌚ = U+231A
	nodes := Parse("⌚")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_SupplementalSymbols(t *testing.T) {
	// 🤔 = U+1F914
	nodes := Parse("🤔")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_UnicodeEmoji_ChessSymbol(t *testing.T) {
	// U+1FA00 (Chess Symbols) は emoji-data の一覧に無いので、mfm-js と同じく文字 (#3324)
	nodes := Parse("\U0001FA00")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_UnicodeEmoji_ExtendedA(t *testing.T) {
	// 🪐 = U+1FA90
	nodes := Parse("🪐")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_URL_SquareBracketNesting(t *testing.T) {
	nodes := Parse("https://example.com/[path]")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeURL, nodes[0].Type)
	assert.Equal(t, "https://example.com/[path]", nodes[0].Props["url"])
}

func TestParse_URL_HTTP(t *testing.T) {
	nodes := Parse("http://example.com/path")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeURL, nodes[0].Type)
}

func TestParse_Link_NotInLink(t *testing.T) {
	// リンク内にリンクはネストできない
	nodes := Parse("[outer [inner](https://inner.com)](https://outer.com)")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeLink, nodes[0].Type)
}

func TestParse_Bold_Unclosed(t *testing.T) {
	nodes := Parse("**unclosed")
	// unclosed markup → text
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_InlineCode_Empty(t *testing.T) {
	nodes := Parse("``") // empty code
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_MathInline_Empty(t *testing.T) {
	nodes := Parse(`\(\)`)
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_MathBlock_Empty(t *testing.T) {
	nodes := Parse(`\[\]`)
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_Fn_MissingSpace(t *testing.T) {
	// $[name] without space → not a fn
	nodes := Parse("$[name]")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_Fn_EmptyName(t *testing.T) {
	nodes := Parse("$[ content]")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_EmojiCode_Invalid(t *testing.T) {
	// emoji code with invalid character
	nodes := Parse(":not valid:")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_Mention_Trailing_Dot(t *testing.T) {
	nodes := Parse("@user.")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeMention, nodes[0].Type)
	assert.Equal(t, "user", nodes[0].Props["username"])
}

func TestParse_Quote_NotAtLineStart(t *testing.T) {
	nodes := Parse("text > not a quote")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_CodeBlock_Unclosed(t *testing.T) {
	nodes := Parse("```\nunclosed code")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_PlainTag_Unclosed(t *testing.T) {
	nodes := Parse("<plain>unclosed")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_HTMLTag_Unclosed(t *testing.T) {
	nodes := Parse("<b>unclosed")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_Link_NoURL(t *testing.T) {
	nodes := Parse("[label]()")
	require.True(t, len(nodes) >= 1)
	// empty URL → not a link
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_Fn_MultipleArgs(t *testing.T) {
	nodes := Parse("$[spin.speed=0.5s,alternate content]")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeFn, nodes[0].Type)
	args := nodes[0].Props["args"].(map[string]any)
	assert.Equal(t, "0.5s", args["speed"])
	assert.Equal(t, true, args["alternate"])
}

func TestParse_UnicodeEmoji_ZWJ(t *testing.T) {
	// 👨‍💻 = U+1F468 U+200D U+1F4BB (ZWJ sequence)
	nodes := Parse("👨\u200d💻")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeUnicodeEmoji, nodes[0].Type)
}

func TestParse_Search_BracketSuffix(t *testing.T) {
	nodes := Parse("query [検索]")
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeSearch, nodes[0].Type)
}

func TestParse_Search_NotMidLine(t *testing.T) {
	nodes := Parse("hello\nquery search")
	// 2行目が search
	found := false
	for _, n := range nodes {
		if n.Type == NodeSearch {
			found = true
		}
	}
	assert.True(t, found)
}

func TestParse_InlineCode_AcuteAccent(t *testing.T) {
	// ´ 内のコードは失敗
	nodes := Parse("`co´de`")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_StrikeWave_WithNewline(t *testing.T) {
	// strikeWave は改行を含まない
	nodes := Parse("~~str\nike~~")
	require.True(t, len(nodes) >= 1)
	assert.NotEqual(t, NodeStrike, nodes[0].Type)
}

func TestParse_Bold_Empty(t *testing.T) {
	nodes := Parse("****")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

func TestParse_ItalicAsta_AfterAlpha(t *testing.T) {
	// 直前が英数字なら italic にならない
	nodes := Parse("a*b*")
	require.True(t, len(nodes) >= 1)
	assert.Equal(t, NodeText, nodes[0].Type)
}

// emojiCodeNames returns the emojiCode names in nodes in document order
// (mfm-js の `extract(nodes, n => n.type === 'emojiCode')` と同じ並び)。
func emojiCodeNames(nodes []*Node) []string {
	var out []string
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Type == NodeEmojiCode {
			out = append(out, n.Props["name"].(string))
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return out
}
