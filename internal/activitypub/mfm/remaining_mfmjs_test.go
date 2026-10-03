package mfm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// serializeTreeDetailed extends serializeTree with the props that the
// remaining-divergence cases depend on: inline / block code (with the
// language), and inline / block math.
func serializeTreeDetailed(nodes []*Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		var head string
		switch n.Type {
		case NodeInlineCode:
			head = string(n.Type) + ":" + fmt.Sprint(n.Props["code"])
		case NodeBlockCode:
			lang, _ := n.Props["lang"].(string)
			head = string(n.Type) + ":" + lang + ":" + fmt.Sprint(n.Props["code"])
		case NodeMathInline, NodeMathBlock:
			head = string(n.Type) + ":" + fmt.Sprint(n.Props["formula"])
		default:
			head = serializeTree([]*Node{{Type: n.Type, Props: n.Props}})
		}
		if len(n.Children) > 0 {
			head += "[" + serializeTreeDetailed(n.Children) + "]"
		}
		parts = append(parts, head)
	}
	return strings.Join(parts, "|")
}

// TestParse_RemainingDivergencesMatchMfmJs fixes the differences collected in
// #3329 to mfm-js 0.26.0. Expected trees (parse and parseSimple) were produced
// by running mfm-js on each input.
//
// mfm-js の newLine は CRLF / CR / LF のどれかで、lineBegin は CR の直後も行頭と
// みなす。以前の mk-go は引用・行内コード・数式・コードブロックで LF だけを見て
// いた。斜体・`__` の中身は mfm-js の space (半角空白・全角空白・タブ) と ASCII の
// 英数字だけで、unicode.IsSpace の CR・NBSP・垂直タブは含まない。コードブロック・
// `<plain>`・リンクのラベルは中身が 1 文字以上要る (many(1))。
func TestParse_RemainingDivergencesMatchMfmJs(t *testing.T) {
	cases := []struct{ in, want, wantSimple string }{
		// 引用: CR で行を切り、CR の直後も行頭。中身は "\n" でつなぐ
		{"> a\rb", "quote[text:a]|text:b", "text:> a\rb"},
		{"> a\r\nb", "quote[text:a]|text:b", "text:> a\r\nb"},
		{"> a\r> b", "quote[text:a\nb]", "text:> a\r> b"},
		{"> a\r\n> b", "quote[text:a\nb]", "text:> a\r\n> b"},
		{"x\r> a", "text:x|quote[text:a]", "text:x\r> a"},
		{"\r> #t 検索", "quote[hashtag:t|text: 検索]", "text:\r> #t 検索"},
		{"\r\n> #t", "quote[hashtag:t]", "text:\r\n> #t"},
		// 引用: `>` の直後の全角空白も 1 つだけ読む
		{">\u3000q", "quote[text:q]", "text:>\u3000q"},
		{">\u3000\u3000q", "quote[text:\u3000q]", "text:>\u3000\u3000q"},
		{"> \u3000q", "quote[text:\u3000q]", "text:> \u3000q"},
		{">\tq", "quote[text:q]", "text:>\tq"},
		// 行内コード・数式は CR でも止まる
		{"`a\rb`", "text:`a\rb`", "text:`a\rb`"},
		{"`a\r\nb`", "text:`a\r\nb`", "text:`a\r\nb`"},
		{"`a\nb`", "text:`a\nb`", "text:`a\nb`"},
		{"\\(a\rb\\)", "text:\\(a\rb\\)", "text:\\(a\rb\\)"},
		{"\\(a\nb\\)", "text:\\(a\nb\\)", "text:\\(a\nb\\)"},
		{"\\(ab\\)", "mathInline:ab", "text:\\(ab\\)"},
		// 斜体・`__` の中身は ASCII の英数字と mfm-js の space だけ
		{"*a\rb*", "text:*a\rb*", "text:*a\rb*"},
		{"_a\rb_", "text:_a\rb_", "text:_a\rb_"},
		{"__a\rb__", "text:__a\rb__", "text:__a\rb__"},
		{"*a\u00a0b*", "text:*a\u00a0b*", "text:*a\u00a0b*"},
		{"*a\u000bb*", "text:*a\u000bb*", "text:*a\u000bb*"},
		{"*a\u3000b*", "italic[text:a\u3000b]", "text:*a\u3000b*"},
		{"__a\tb__", "bold[text:a\tb]", "text:__a\tb__"},
		{"*a b*", "italic[text:a b]", "text:*a b*"},
		// コードブロック: 中身は 1 文字以上、改行は CR / CRLF も、閉じの直後は行の終わり
		{"```\n\n```", "text:```\n\n```", "text:```\n\n```"},
		{"```\r\n\r\n```", "text:```\r\n\r\n```", "text:```\r\n\r\n```"},
		{"```\na\n```", "blockCode::a", "text:```\na\n```"},
		{"```js\r\na\r\n```", "blockCode:js:a", "text:```js\r\na\r\n```"},
		{"```\ra\r```", "blockCode::a", "text:```\ra\r```"},
		{"\r```\na\n```", "blockCode::a", "text:\r```\na\n```"},
		{"```\na\n```x", "text:```\na\n```x", "text:```\na\n```x"},
		{"```\na\n```x\n```", "blockCode::a\n```x", "text:```\na\n```x\n```"},
		{"```\n```\n```", "blockCode::```", "text:```\n```\n```"},
		{"a\n```\nb\n```\nc", "text:a|blockCode::b|text:c", "text:a\n```\nb\n```\nc"},
		// <plain>: 開きの直後と閉じの直前の改行を 1 つずつ外し、中身は 1 文字以上
		{"<plain></plain>", "text:<plain></plain>", "text:<plain></plain>"},
		{"<plain>\n</plain>", "text:<plain>\n</plain>", "text:<plain>\n</plain>"},
		{"<plain>\r\n</plain>", "text:<plain>\r\n</plain>", "text:<plain>\r\n</plain>"},
		{"<plain>\n\n</plain>", "text:<plain>\n\n</plain>", "text:<plain>\n\n</plain>"},
		{"<plain>\na\n</plain>", "plain[text:a]", "plain[text:a]"},
		{"<plain>\r\na\r\n</plain>", "plain[text:a]", "plain[text:a]"},
		{"<plain>\r\ra\r\r</plain>", "plain[text:\ra\r]", "plain[text:\ra\r]"},
		{"<plain>a</plain>", "plain[text:a]", "plain[text:a]"},
		{"<plain> </plain>", "plain[text: ]", "plain[text: ]"},
		// リンク: ラベルは 1 つ以上要る
		{"[](https://e.x)", "text:[](|url:https://e.x|text:)", "text:[](https://e.x)"},
		{"?[](https://e.x)", "text:?[](|url:https://e.x|text:)", "text:?[](https://e.x)"},
		{"[ ](https://e.x)", "link:https://e.x[text: ]", "text:[ ](https://e.x)"},
		{"[a](https://e.x)", "link:https://e.x[text:a]", "text:[a](https://e.x)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTreeDetailed(Parse(tc.in)), "Parse")
			assert.Equal(t, tc.wantSimple, serializeTreeDetailed(ParseSimple(tc.in)), "ParseSimple")
		})
	}
}

// TestParse_RemainingDivergencesExtraction checks the outputs that the tree
// differences in #3329 changed: hashtags, IsSimple and the HTML.
func TestParse_RemainingDivergencesExtraction(t *testing.T) {
	// CR の直後の引用は、中の行の検索より先に引用になり、ハッシュタグが取れる
	assert.Equal(t, []string{"t"}, CollectHashtags("\r> #t 検索"))
	// 空のラベルのリンクは空の <a> にしない
	assert.Equal(t, `[](<a href="https://e.x/">https://e.x</a>)`, ToHTML(Parse("[](https://e.x)"), testHost))
	assert.True(t, IsSimple(Parse("`a\rb`")))
	assert.True(t, IsSimple(Parse("<plain></plain>")))
	assert.True(t, IsSimple(Parse("```\n\n```")))
}

// TestCodeBlock_FallbackMatchesIndex checks that the code block reads the
// same closing position when the index of closes cannot be built.
//
// 索引を作れないのはメモ表の上限に届いたときだけで、通常の Parse からは入らない
// ので、state を直接作って索引を使えない状態にする。
func TestCodeBlock_FallbackMatchesIndex(t *testing.T) {
	inputs := []string{
		"```\na\n```", "```js\r\na\r\n```", "```\ra\r```", "```\n\n```", "```\na\n```x",
		"```\na\n```x\n```", "```\n```\n```", "```\na", "```", "```a\n```b\n```",
	}
	for _, in := range inputs {
		indexed := newState(in, false)
		fallback := newState(in, false)
		fallback.memo.stopsState[stopCodeBlockClose] = -1
		want := indexed.tryCodeBlock()
		got := fallback.tryCodeBlock()
		assert.Equal(t, serializeTreeDetailed(nodesOf(want)), serializeTreeDetailed(nodesOf(got)), "%q", in)
		assert.Equal(t, indexed.pos, fallback.pos, "%q: position", in)
		if strings.ContainsAny(in, "\r\n") {
			// 開きの行が閉じていれば、閉じの位置は索引から引く
			assert.Equal(t, int8(1), indexed.memo.stopsState[stopCodeBlockClose], "%q: index not used", in)
		}
	}
}

// TestCodeBlock_FallbackCountsWork checks that the fallback search for the
// close counts the bytes it reads and stops at the work budget.
//
// 索引を使えないとき、開きの行を並べた入力は開きごとに末尾まで読む。読んだ分を
// 仕事量に数えないと、入力長の 2 乗になっても上限で打ち切られない。
func TestCodeBlock_FallbackCountsWork(t *testing.T) {
	s := newState(strings.Repeat("```a\n", 3000), false)
	s.memo.stopsState[stopCodeBlockClose] = -1
	mergeText(s.parseNodes(false))
	assert.True(t, s.budget.exhausted(), "used %d of %d", s.budget.used, s.budget.limit)
}

// TestCodeBlock_UnclosedOpensUseIndex checks that the closes are looked up in
// the index: the same input that exhausts the budget through the fallback
// stays within it.
func TestCodeBlock_UnclosedOpensUseIndex(t *testing.T) {
	s := newState(strings.Repeat("```a\n", 3000), false)
	mergeText(s.parseNodes(false))
	assert.False(t, s.budget.exhausted(), "used %d of %d", s.budget.used, s.budget.limit)
}

// TestMathInline_FallbackMatchesIndex checks that inline math stops at CR,
// CRLF and LF the same way with and without the index of stops.
//
// 索引を作れないのはメモ表の上限に届いたときだけなので、state を直接作って
// 索引を使えない状態にする。
func TestMathInline_FallbackMatchesIndex(t *testing.T) {
	inputs := []string{
		"\\(ab\\)", "\\(a\rb\\)", "\\(a\r\nb\\)", "\\(a\nb\\)", "\\(\\)", "\\(a\r", "\\(a",
	}
	for _, in := range inputs {
		indexed := newState(in, false)
		fallback := newState(in, false)
		fallback.memo.stopsState[stopMathInline] = -1
		want := indexed.tryMathInline()
		got := fallback.tryMathInline()
		assert.Equal(t, serializeTreeDetailed(nodesOf(want)), serializeTreeDetailed(nodesOf(got)), "%q", in)
		assert.Equal(t, indexed.pos, fallback.pos, "%q: position", in)
		assert.Equal(t, int8(1), indexed.memo.stopsState[stopMathInline], "%q: index not used", in)
		assert.Equal(t, int8(-1), fallback.memo.stopsState[stopMathInline], "%q: fallback not used", in)
	}
	// CR で止まるので数式にならない
	assert.Nil(t, newState("\\(a\rb\\)", false).tryMathInline())
}

func nodesOf(n *Node) []*Node {
	if n == nil {
		return nil
	}
	return []*Node{n}
}

// TestToHTML_EscapeMatchesUpstream fixes the HTML escaping to upstream
// MfmService's escapeHtml (& < > " ' to &amp; &lt; &gt; &quot; &#039;) in
// text, attributes and code (#3329).
func TestToHTML_EscapeMatchesUpstream(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a'b"c&d<e>f`, "a&#039;b&quot;c&amp;d&lt;e&gt;f"},
		{"`'\"&<>`", "<code>&#039;&quot;&amp;&lt;&gt;</code>"},
		{"```\n'\"\n```", "<pre><code>&#039;&quot;</code></pre>"},
		{`\('"\)`, "<code>&#039;&quot;</code>"},
		// href は new URL().href で作るので `"` は %22 になる
		{"<https://e.x/'a\"&>", `<a href="https://e.x/&#039;a%22&amp;">https://e.x/&#039;a&quot;&amp;</a>`},
		{"[x'](<https://e.x/'a'>)", `<a href="https://e.x/&#039;a&#039;">x&#039;</a>`},
		{"https://e.x/?a=1&b=2", `<a href="https://e.x/?a=1&amp;b=2">https://e.x/?a=1&amp;b=2</a>`},
		{"@a'", `<a href="https://` + testHost + `/@a" class="u-url mention">@a</a>&#039;`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, ToHTML(Parse(tc.in), testHost))
		})
	}
}
