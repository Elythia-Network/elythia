package mfm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParse_SearchAfterNewlineMatchesMfmJs fixes how search and the block
// constructs read the newlines around them, matching mfm-js 0.26.0 (#3325).
// Expected trees were produced by running mfm-js's parse on each input
// (serializeTree is in mention_mfmjs_test.go).
//
// mfm-js の search・quote・codeBlock・mathBlock・center は改行の位置から試され、
// 前の改行を読む (quote は 2 つまで、他は 1 つ)。後ろの改行も読む (quote は
// 2 つまで、他は 1 つ)。改行の位置では次の行のハッシュタグなどより先に search が
// 試されるので、2 行目以降の行頭が何であっても検索になる。ただしブロックの直後の
// 行は改行を持たずに始まるので、行頭のハッシュタグなどが先に読まれる。
func TestParse_SearchAfterNewlineMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		// 2 行目以降の行頭が何であっても検索になる
		{"a\n#t 検索", "text:a|search"},
		{"a\n:x: 検索", "text:a|search"},
		{"a\n@a 検索", "text:a|search"},
		{"a\n😀 検索", "text:a|search"},
		{"a\nfoo 検索", "text:a|search"},
		{"a\r\n#t 検索", "text:a|search"},
		{"a\r#t 検索", "text:a|search"},
		{"a\n\n\n\n#t 検索", "text:a\n\n\n|search"},
		// 1 行目と、ブロックの直後の行は改行を持たずに始まる
		{"#t 検索", "hashtag:t|text: 検索"},
		{"> q\n#t 検索", "quote[text:q]|hashtag:t|text: 検索"},
		{"> q\n\n#t 検索", "quote[text:q]|hashtag:t|text: 検索"},
		{"> q\n\n\n#t 検索", "quote[text:q]|search"},
		{"> a\n> #t 検索", "quote[text:a|search]"},
		{"```\nx\n```\n#t 検索", "blockCode|hashtag:t|text: 検索"},
		{"```\nx\n```\n\n#t 検索", "blockCode|search"},
		{"\\[x\\]\n#t 検索", "mathBlock|hashtag:t|text: 検索"},
		{"\\[x\\]\n\n#t 検索", "mathBlock|search"},
		{"<center>x</center>\n#t 検索", "center[text:x]|hashtag:t|text: 検索"},
		{"a 検索\n#t 検索", "search|hashtag:t|text: 検索"},
		{"a 検索\n\n#t 検索", "search|search"},
		// ブロックが前後の改行を読む
		{"> a\n\n> b", "quote[text:a]|quote[text:b]"},
		{"a\n> q", "text:a|quote[text:q]"},
		{"a\n\n> q", "text:a|quote[text:q]"},
		{"a\n\n\n> q", "text:a\n|quote[text:q]"},
		{"a\n```\nx\n```\nb", "text:a|blockCode|text:b"},
		{"a\n\\[x\\]\nb", "text:a|mathBlock|text:b"},
		{"a\n<center>x</center>\nb", "text:a|center[text:x]|text:b"},
		{"foo 検索\nb", "search|text:b"},
		{"foo 検索\r\nb", "search|text:b"},
		// 前の改行を読んで行の途中で終わる数式ブロック・center は、検索に譲る
		{"a\n\\[x\\] b", "text:a\n\\[x\\] b"},
		{"a \\[x\\]", "text:a \\[x\\]"},
		{"a\n<center>x</center> 検索", "text:a|search"},
		// 検索の区切りと語
		{"a\nq検索", "text:a\nq検索"},
		{"a\nq 検索 ", "text:a\nq 検索 "},
		{"a\nq\t検索", "text:a|search"},
		{"a\nq\u3000検索", "text:a|search"},
		{"a\nq [検索]", "text:a|search"},
		{"a\nq [search]", "text:a|search"},
		{"a\nq SeArCh", "text:a|search"},
		{"a\nq [検索", "text:a\nq [検索"},
		{"a\n 検索", "text:a\n 検索"},
		{"a\n  検索", "text:a|search"},
		{"a\nq 検索 検索", "text:a|search"},
		// 装飾やリンクのラベルの中 (mfm-js の inline) には検索が無い
		{"<b>a\n#t 検索</b>", "bold[text:a\n|hashtag:t|text: 検索]"},
		{"<b>a\nfoo 検索\n</b>", "bold[text:a\nfoo 検索\n]"},
		{"$[x a\n#t 検索\n]", "fn[text:a\n|hashtag:t|text: 検索\n]"},
		{"[a\n#t 検索](https://x.example)", "text:[a\n|hashtag:t|text: 検索](|url:https://x.example|text:)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}

// TestParse_SearchQueryMatchesMfmJs fixes the query and content of search
// nodes to the values mfm-js 0.26.0 produces (#3325). content is query, the
// separator character and the button text; the HTML uses it as the link text
// (#3327).
func TestParse_SearchQueryMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, query, content string }{
		{"a\n#t 検索", "#t", "#t 検索"},
		{"a\nq 検索 検索", "q 検索", "q 検索 検索"},
		{"a\n  検索", " ", "  検索"},
		{"a\nq\u3000検索", "q", "q\u3000検索"},
		{"a\nq [search]", "q", "q [search]"},
		{"a\r\n:x: SEARCH\r\nb", ":x:", ":x: SEARCH"},
		{"> a\n> #t 検索", "#t", "#t 検索"},
		{" q  search", " q ", " q  search"},
		{"a\n x\t[SEARCH]\nb", " x", " x\t[SEARCH]"},
		{"a\nq [SeArCh]\n", "q", "q [SeArCh]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var got [][2]any
			var walk func([]*Node)
			walk = func(nodes []*Node) {
				for _, n := range nodes {
					if n.Type == NodeSearch {
						got = append(got, [2]any{n.Props["query"], n.Props["content"]})
					}
					walk(n.Children)
				}
			}
			walk(Parse(tc.in))
			assert.Equal(t, [][2]any{{tc.query, tc.content}}, got)
		})
	}
}

// TestParse_BlocksInQuoteContentMatchMfmJs fixes how block constructs are
// read in quote content, which mfm-js reads with its full parser, including
// after a <center> that fails to close. Expected trees were produced by
// running mfm-js 0.26.0's parse.
//
// 引用の中身は full なので、数式ブロックは行の先頭で始まり行の末尾で終わる
// ときだけ読む (tryBlock の whole)。fn の中身は inline なので数式ブロックを
// 試さず、`\]` の `]` で fn が閉じる (#3301)。閉じない center の後ろでは、
// 引用を最上位から読み直しても同じ木になる。
func TestParse_BlocksInQuoteContentMatchMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{">> $[x \\[x\\]\n", "quote[quote[fn[text:\\[x\\]]]"},
		{"<center>\n>> $[x \\[x\\]\n", "text:<center>|quote[quote[fn[text:\\[x\\]]]"},
		{"> ~~x\n> $[x \\[x\\]</b>\n", "quote[text:~~x\n|fn[text:\\[x\\]|text:</b>]"},
		{"<center>\n> ~~x\n> $[x \\[x\\]</b>\n", "text:<center>|quote[text:~~x\n|fn[text:\\[x\\]|text:</b>]"},
		{"<center>\n> a\n> \\[x\\]\n", "text:<center>|quote[text:a|mathBlock]"},
		{"<center>\n> \\[x\\] b\n", "text:<center>|quote[text:\\[x\\] b]"},
		{"> a \\[x\\]\n", "quote[text:a \\[x\\]]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}

// TestParse_SearchLineHidesHashtagAndEmoji checks the effects the issue
// reported: a hashtag or a custom emoji at the head of a later search line is
// part of the query, so it is neither a note tag nor an extracted emoji.
func TestParse_SearchLineHidesHashtagAndEmoji(t *testing.T) {
	assert.Empty(t, CollectHashtags("a\n#t 検索"))
	assert.Empty(t, ExtractCustomEmojis(Parse("a\n:x: 検索")))
	assert.Equal(t, []string{"t"}, CollectHashtags("> q\n\n#t 検索"))
}

// TestParse_ManyNewlinesStayLinear checks that trying search and the block
// constructs at every newline does not rescan the following lines: a long
// run of newlines, of short lines and of quote lines stays within a work per
// byte that a quadratic scan would exceed many times over.
//
// 実測は 1 バイトあたり最大 4.75 (long lines。search が行を読んだ分も仕事量に数える)。
// 行頭でない位置でも search が行末まで読むと、long lines で仕事量の上限に届く。
// quote lines は引用ごとに表を持つので、表の入れ物を全ての深さの分だけ先に
// 確保すると 16K 個でメモの確保量の上限に届く (slotFor)。
func TestParse_ManyNewlinesStayLinear(t *testing.T) {
	cases := map[string]string{
		"newlines":      strings.Repeat("\n", 16<<10),
		"crlf":          strings.Repeat("\r\n", 8<<10),
		"short lines":   strings.Repeat("a\n", 8<<10),
		"search lines":  strings.Repeat("#t 検索\n", 2<<10),
		"almost search": strings.Repeat("#t 検索 x\n\n", 2<<10),
		"quote lines":   strings.Repeat("> a\n\n\n", 16<<10),
		"long lines":    strings.Repeat(strings.Repeat("a ", 4<<10)+"\n", 4),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			s := newState(in, false)
			mergeText(s.parseNodes(false))
			require.False(t, s.budget.exhausted(), "used %d of %d", s.budget.used, s.budget.limit)
			assert.Less(t, s.budget.used, 8*len(in), "work per byte")
		})
	}
}
