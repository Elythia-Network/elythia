package mfm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestToHTML_MatchesUpstream fixes ToHTML to upstream MfmService.toHtml
// (#3329). Expected HTML was produced by running mfm-js 0.26.0's parse and a
// copy of MfmService.toHtml on Node 26.4.0 (the version upstream runs on),
// with config.url = https://example.com.
//
// 本家は改行を CRLF / CR / LF で切って `<br />` にし、plain を `<span>`、数式
// ブロックを `<pre><code>` にする。ハッシュタグの href は encodeURIComponent、
// link / url / メンションの href は `new URL().href` で作る。ruby は引数を見ずに
// 子から作り、unixtime はミリ秒付きの ISO 8601 にする。mentionedRemoteUsers は
// 空で比べた (メンションは全て `${config.url}/${acct}` になる)。渡したときは
// TestToHTMLWithMentions_MatchesUpstream が見る。
func TestToHTML_MatchesUpstream(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\r\nb\rc\nd", "a<br />b<br />c<br />d"},
		{"a\n\nb", "a<br /><br />b"},
		{"\r\n", "<br />"},
		{"a\r", "a<br />"},
		{"<plain>**x**</plain>", "<span>**x**</span>"},
		{"<plain>a\nb</plain>", "<span>a<br />b</span>"},
		{"\\[\tx\\]", "<pre><code>\tx</code></pre>"},
		{"\\[\nx<y\n\\]", "<pre><code>x&lt;y</code></pre>"},
		{"#a&b", "<a href=\"https://example.com/tags/a%26b\" rel=\"tag\">#a&amp;b</a>"},
		{"#a+b=c", "<a href=\"https://example.com/tags/a%2Bb%3Dc\" rel=\"tag\">#a+b=c</a>"},
		{"#タグ", "<a href=\"https://example.com/tags/%E3%82%BF%E3%82%B0\" rel=\"tag\">#タグ</a>"},
		{"#a'b", "<a href=\"https://example.com/tags/a\" rel=\"tag\">#a</a>&#039;b"},
		{"#a!b~*", "<a href=\"https://example.com/tags/a\" rel=\"tag\">#a</a>!b~*"},
		{"https://Example.COM:443/a/./b/../c?x='\"#`f`", "<a href=\"https://example.com/a/c?x=\">https://Example.COM:443/a/./b/../c?x=</a>&#039;&quot;<a href=\"https://example.com/tags/%60f%60\" rel=\"tag\">#`f`</a>"},
		{"http://e.x:80", "<a href=\"http://e.x/\">http://e.x:80</a>"},
		{"https://e.x:8080/", "<a href=\"https://e.x:8080/\">https://e.x:8080/</a>"},
		{"https://e.x:0443/", "<a href=\"https://e.x/\">https://e.x:0443/</a>"},
		{"https://0x7f.1/", "<a href=\"https://127.0.0.1/\">https://0x7f.1/</a>"},
		{"https://127.1/", "<a href=\"https://127.0.0.1/\">https://127.1/</a>"},
		{"https://e.x/%2e%2E/x", "<a href=\"https://e.x/x\">https://e.x/%2e%2E/x</a>"},
		{"<https://e.x/a`b{c}d^e|f>", "<a href=\"https://e.x/a%60b%7Bc%7Dd%5Ee|f\">https://e.x/a`b{c}d^e|f</a>"},
		{"<https://例え.テスト/パス?く#ふ>", "<a href=\"https://xn--r8jz45g.xn--zckzah/%E3%83%91%E3%82%B9?%E3%81%8F#%E3%81%B5\">https://例え.テスト/パス?く#ふ</a>"},
		{"<https://ＥＸ.com/>", "<a href=\"https://ex.com/\">https://ＥＸ.com/</a>"},
		{"<https://e.x:65536/>", "https://e.x:65536/"},
		{"<https://xn--a.com/>", "https://xn--a.com/"},
		{"<https://[0:0:0:0:0:0:0:1]/>", "<a href=\"https://[::1]/\">https://[0:0:0:0:0:0:0:1]/</a>"},
		{"<https://e.x\\a\\b>", "<a href=\"https://e.x/a/b\">https://e.x\\a\\b</a>"},
		{"<https://u@v:w@e.x/>", "<a href=\"https://u%40v:w@e.x/\">https://u@v:w@e.x/</a>"},
		{"[l](https://E.X:443/a/../b)", "<a href=\"https://e.x/b\">l</a>"},
		{"[**l**](<https://e.x/a\"b>)", "<a href=\"https://e.x/a%22b\"><b>l</b></a>"},
		{"[l](<https://xn--a.com/>)", "[l](https://xn--a.com/)"},
		{"[l](<https://e.x:99999/>)", "[l](https://e.x:99999/)"},
		{"@alice", "<a href=\"https://example.com/@alice\" class=\"u-url mention\">@alice</a>"},
		{"@bob@REMOTE.Example", "<a href=\"https://example.com/@bob@REMOTE.Example\" class=\"u-url mention\">@bob@REMOTE.Example</a>"},
		{"@u@例え.テスト", "<a href=\"https://example.com/@u\" class=\"u-url mention\">@u</a>@例え.テスト"},
		{"@u@xn--a.com", "<a href=\"https://example.com/@u@xn--a.com\" class=\"u-url mention\">@u@xn--a.com</a>"},
		{"$[ruby 漢字 かんじ]", "<ruby>漢字<rp>(</rp><rt>かんじ</rt><rp>)</rp></ruby>"},
		{"$[ruby a b c]", "<ruby>a<rp>(</rp><rt>b</rt><rp>)</rp></ruby>"},
		{"$[ruby a  b]", "<ruby>a<rp>(</rp><rt></rt><rp>)</rp></ruby>"},
		{"$[ruby **a** \u3000b\ufeff]", "<ruby><b>a</b><rp>(</rp><rt>b</rt><rp>)</rp></ruby>"},
		{"$[ruby **a** b<i>c</i>]", "<ruby><b>a</b> b<rp>(</rp><rt></rt><rp>)</rp></ruby>"},
		{"$[unixtime 1234567890]", "<time datetime=\"2009-02-13T23:31:30.000Z\">2009-02-13T23:31:30.000Z</time>"},
		{"$[unixtime 0]", "<time datetime=\"1970-01-01T00:00:00.000Z\">1970-01-01T00:00:00.000Z</time>"},
		{"$[unixtime  -1]", "<time datetime=\"1969-12-31T23:59:59.000Z\">1969-12-31T23:59:59.000Z</time>"},
		{"$[unixtime 12abc]", "<time datetime=\"1970-01-01T00:00:12.000Z\">1970-01-01T00:00:12.000Z</time>"},
		{"$[unixtime 1e3]", "<time datetime=\"1970-01-01T00:00:01.000Z\">1970-01-01T00:00:01.000Z</time>"},
		{"$[unixtime +5]", "<time datetime=\"1970-01-01T00:00:05.000Z\">1970-01-01T00:00:05.000Z</time>"},
		{"$[unixtime 0x10]", "<time datetime=\"1970-01-01T00:00:00.000Z\">1970-01-01T00:00:00.000Z</time>"},
		{"$[unixtime 253402300800]", "<time datetime=\"+010000-01-01T00:00:00.000Z\">+010000-01-01T00:00:00.000Z</time>"},
		{"$[unixtime -62167219201]", "<time datetime=\"-000001-12-31T23:59:59.000Z\">-000001-12-31T23:59:59.000Z</time>"},
		{"$[unixtime 8640000000000]", "<time datetime=\"+275760-09-13T00:00:00.000Z\">+275760-09-13T00:00:00.000Z</time>"},
		{"$[unixtime 8640000000001]", "<i>8640000000001</i>"},
		{"$[unixtime -8640000000001]", "<i>-8640000000001</i>"},
		{"$[unixtime abc]", "<i>abc</i>"},
		{"$[unixtime **1**]", "<i><b>1</b></i>"},
		{"$[unixtime \ufeff7]", "<time datetime=\"1970-01-01T00:00:07.000Z\">1970-01-01T00:00:07.000Z</time>"},
		{"$[unixtime \u00857]", "<i>\u00857</i>"},
		{"$[unixtime 99999999999999999999999]", "<i>99999999999999999999999</i>"},
		{"<https://e.x/'a\"&>", "<a href=\"https://e.x/&#039;a%22&amp;\">https://e.x/&#039;a&quot;&amp;</a>"},
		{"[x'](<https://e.x/'a'>)", "<a href=\"https://e.x/&#039;a&#039;\">x&#039;</a>"},
		{"[](https://e.x)", "[](<a href=\"https://e.x/\">https://e.x</a>)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, ToHTML(Parse(tc.in), testHost))
		})
	}
}

// TestToHTML_LineBreaksRoundTrip checks that FromHTML reads the `<br />` that
// ToHTML now writes for every kind of newline back as LF.
//
// mk-go 同士の chat など、HTML から本文を戻す経路がある。以前の ToHTML は CR を
// そのまま HTML に残していた。
func TestToHTML_LineBreaksRoundTrip(t *testing.T) {
	html := ToHTML(Parse("a\r\nb\rc\nd <plain>**e**</plain>"), testHost)
	assert.Equal(t, "a<br />b<br />c<br />d <span>**e**</span>", html)
	assert.Equal(t, "a\nb\nc\nd **e**", FromHTML(html))
}

// TestToHTML_RubyWithoutRubyTextIsItalic covers the inputs on which upstream's
// ruby throws (escapeHtml(undefined)): a single child without a half-width
// space, or a single child that is not text.
//
// 本家はノートの HTML を作れずに例外になる。mk-go は配送や描画を止めないよう、
// 本家の fnDefault と同じ斜体にする (docs/divergence.md)。
func TestToHTML_RubyWithoutRubyTextIsItalic(t *testing.T) {
	assert.Equal(t, "<i>base</i>", ToHTML(Parse("$[ruby.rt=x base]"), testHost))
	assert.Equal(t, "<i><b>a</b></i>", ToHTML(Parse("$[ruby **a**]"), testHost))
	assert.Equal(t, "<i></i>", ToHTML([]*Node{{Type: NodeFn, Props: map[string]any{"name": "ruby"}}}, testHost))
	assert.Equal(t, "<i></i>", ToHTML([]*Node{{Type: NodeFn, Props: map[string]any{"name": "unixtime"}}}, testHost))
}

// TestToHTML_RubyTextIsEscaped checks that both parts of the ruby are escaped.
// The nodes are built directly because the parser does not read these
// characters inside a fn.
//
// ルビは属性ではなく要素の中身に書くので、エスケープが抜けると任意の HTML を
// 連合先へ送ることになる。
func TestToHTML_RubyTextIsEscaped(t *testing.T) {
	one := &Node{Type: NodeFn, Props: map[string]any{"name": "ruby"}, Children: []*Node{Text(`<i>x <b>"'&`)}}
	assert.Equal(t, "<ruby>&lt;i&gt;x<rp>(</rp><rt>&lt;b&gt;&quot;&#039;&amp;</rt><rp>)</rp></ruby>", ToHTML([]*Node{one}, testHost))
	two := &Node{Type: NodeFn, Props: map[string]any{"name": "ruby"}, Children: []*Node{Text("<x>"), Text(` <b>"'& `)}}
	assert.Equal(t, "<ruby>&lt;x&gt;<rp>(</rp><rt>&lt;b&gt;&quot;&#039;&amp;</rt><rp>)</rp></ruby>", ToHTML([]*Node{two}, testHost))
}

// TestToHTML_MentionHrefFailureIsText checks that a mention whose href cannot
// be parsed as a URL is written as its acct, as upstream does.
func TestToHTML_MentionHrefFailureIsText(t *testing.T) {
	n := &Node{Type: NodeMention, Props: map[string]any{"username": "u", "host": "e.x", "acct": "@u@<e>"}}
	broken := "https://e.x:99999/"
	mentioned := []MentionedRemoteUser{{URI: "https://e.x/u", URL: &broken, Username: "u", Host: &[]string{"e.x"}[0]}}
	assert.Equal(t, "@u@&lt;e&gt;", ToHTMLWithMentions([]*Node{n}, testHost, mentioned))
}

// TestParse_MathBlockMatchesMfmJs fixes the math block to mfm-js 0.26.0
// (#3329). Expected trees were produced by running mfm-js on each input.
//
// mfm-js は開きの直後と閉じの直前の改行を 1 つずつ外すだけで、空白は削らない。
// 以前の mk-go は TrimSpace で前後の空白をすべて削り、空白だけの中身を数式に
// しなかった。
func TestParse_MathBlockMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\\[\tx\\]", "mathBlock:\tx"},
		{"\\[\n\tx \n\\]", "mathBlock:\tx "},
		{"\\[\n\n x\n\n\\]", "mathBlock:\n x\n"},
		{"\\[\r\nx\r\n\\]", "mathBlock:x"},
		{"\\[\rx\r\\]", "mathBlock:x"},
		{"\\[\n\n\n\\]", "mathBlock:\n"},
		{"\\[ \\]", "mathBlock: "},
		{"\\[\n\\]", "text:\\[\n\\]"},
		{"\\[x\n\n\\]", "mathBlock:x\n"},
		{"\\[\r\n\r\n\\]", "text:\\[\r\n\r\n\\]"},
		{"\\[\n\r\n\\]", "text:\\[\n\r\n\\]"},
		{"a\\[x\\]", "text:a\\[x\\]"},
		{"\\[x\\]b", "text:\\[x\\]b"},
		{"a\n\\[x\\]\nb", "text:a|mathBlock:x|text:b"},
		{"\\[\u3000x\u3000\\]", "mathBlock:\u3000x\u3000"},
		{"\\[\\]", "text:\\[\\]"},
		{"\\[x\\]\\]", "text:\\[x\\]\\]"},
		{"\\[\\]x\\]", "text:\\[\\]x\\]"},
		{"\\[a\r\\]", "mathBlock:a"},
		{"\\[a\r\n\n\\]", "mathBlock:a\r\n"},
		{"\\[\n\nx\\]", "mathBlock:\nx"},
		{"\\[x\\]\n\n", "mathBlock:x|text:\n"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTreeDetailed(Parse(tc.in)))
		})
	}
}

// TestMathBlock_FallbackMatchesIndex checks that the math block reads the same
// close with and without the index of closes.
//
// 索引を作れないのはメモ表の上限に届いたときだけなので、state を直接作って
// 索引を使えない状態にする。
func TestMathBlock_FallbackMatchesIndex(t *testing.T) {
	inputs := []string{
		"\\[x\\]", "\\[\nx\n\\]", "\\[\r\nx\r\n\\]", "\\[\rx\r\\]", "\\[\n\\]", "\\[\\]", "\\[x", "\\[ \\]x\\]", "\\[a\r\n\n\\]",
	}
	for _, in := range inputs {
		indexed := newState(in, false)
		fallback := newState(in, false)
		fallback.memo.stopsState[stopMathBlockClose] = -1
		want := indexed.tryMathBlock()
		got := fallback.tryMathBlock()
		assert.Equal(t, serializeTreeDetailed(nodesOf(want)), serializeTreeDetailed(nodesOf(got)), "%q", in)
		assert.Equal(t, indexed.pos, fallback.pos, "%q: position", in)
		assert.Equal(t, int8(1), indexed.memo.stopsState[stopMathBlockClose], "%q: index not used", in)
	}
}

// TestMathBlock_FallbackCountsWork checks that the fallback search for the
// close counts the bytes it reads against the work budget.
func TestMathBlock_FallbackCountsWork(t *testing.T) {
	s := newState(strings.Repeat("\\[a\n", 3000), false)
	s.memo.stopsState[stopMathBlockClose] = -1
	mergeText(s.parseNodes(false))
	assert.True(t, s.budget.exhausted(), "used %d of %d", s.budget.used, s.budget.limit)
}

// TestParse_CodeBlockLangTrimMatchesMfmJs fixes the trimming of the code
// block's language name to JavaScript's String.prototype.trim (#3329).
// Expected trees were produced by running mfm-js on each input.
//
// Go の TrimSpace は U+0085 を削り U+FEFF を削らないが、JS の trim は逆。
func TestParse_CodeBlockLangTrimMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"```\u0085js\u0085\ncode\n```", "blockCode:\u0085js\u0085:code"},
		{"```\ufeffjs\ufeff\ncode\n```", "blockCode:js:code"},
		{"```\u2028js\u2029\ncode\n```", "blockCode:js:code"},
		{"```\u00a0\ncode\n```", "blockCode::code"},
		{"```\ufeff\ncode\n```", "blockCode::code"},
		{"```\u0085\ncode\n```", "blockCode:\u0085:code"},
		{"```\u1680\u2000\u200a\u202f\u205f\u3000js\u000b\u000c\ncode\n```", "blockCode:js:code"},
		{"```\u200bjs\ncode\n```", "blockCode:\u200bjs:code"},
		{"```\u180ejs\ncode\n```", "blockCode:\u180ejs:code"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTreeDetailed(Parse(tc.in)))
		})
	}
}

// TestWhatwgHref fixes whatwgHref to `new URL(x).href` on Node 26.4.0.
// Expected values were produced by running Node on each input; false means
// Node throws.
func TestWhatwgHref(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"https://e.x", "https://e.x/", true},
		{"HTTPS://E.X/A", "https://e.x/A", true},
		{"http://e.x:80/", "http://e.x/", true},
		{"https://e.x:443", "https://e.x/", true},
		{"https://e.x:", "https://e.x/", true},
		{"https://e.x:08080/", "https://e.x:8080/", true},
		{"https://e.x:65535/", "https://e.x:65535/", true},
		{"https://e.x:65536/", "", false},
		{"https://e.x:1a/", "", false},
		{"https://:80/", "", false},
		{"  https://e.x/ \u0001", "https://e.x/", true},
		{"ht\ttps://e\n.x/a\rb", "https://e.x/ab", true},
		{"https:e.x/a", "https://e.x/a", true},
		{"https:\\\\e.x\\a", "https://e.x/a", true},
		{"https:////e.x/", "https://e.x/", true},
		{"1https://e.x/", "", false},
		{"ht@tps://e.x/", "", false},
		{":https", "", false},
		{"javascript:alert(1)", "", false}, // Node は読むが、http / https 以外は扱わない
		{"data:text/html,x", "", false},    // Node は読むが、http / https 以外は扱わない
		{"ftp://e.x/", "", false},          // Node は読むが、http / https 以外は扱わない
		{"e.x/a", "", false},
		{"https://u:p@e.x/", "https://u:p@e.x/", true},
		{"https://u@v@e.x/", "https://u%40v@e.x/", true},
		{"https://u:p:q@e.x/", "https://u:p%3Aq@e.x/", true},
		{"https://@e.x/", "https://e.x/", true},
		{"https://:@e.x/", "https://e.x/", true},
		{"https://e.x@/", "", false},
		{"https://u^|;=[]@e.x/", "https://u%5E%7C%3B%3D%5B%5D@e.x/", true},
		{"https://ü:é@e.x/", "https://%C3%BC:%C3%A9@e.x/", true},
		{"https://e.x/a/./b/../c", "https://e.x/a/c", true},
		{"https://e.x/a/..", "https://e.x/", true},
		{"https://e.x/a/.", "https://e.x/a/", true},
		{"https://e.x/..", "https://e.x/", true},
		{"https://e.x/%2e/%2E%2e/%2e./.%2E/a", "https://e.x/a", true},
		{"https://e.x/a\\b/../c", "https://e.x/a/c", true},
		{"https://e.x/a%2fb", "https://e.x/a%2fb", true},
		{"https://e.x/%zz/%41", "https://e.x/%zz/%41", true},
		{"https://e.x/ \"<>`{}^|?q", "https://e.x/%20%22%3C%3E%60%7B%7D%5E|?q", true},
		{"https://e.x/\u0000\u001f\u007f\u0080あ", "https://e.x/%00%1F%7F%C2%80%E3%81%82", true},
		{"https://e.x/?a b\"#'<>`{}", "https://e.x/?a%20b%22#'%3C%3E%60{}", true},
		{"https://e.x/#a b\"'<>`{}#", "https://e.x/#a%20b%22'%3C%3E%60{}#", true},
		{"https://e.x/a?b'c#d'e", "https://e.x/a?b%27c#d'e", true},
		// punycode の区切りが先頭にしかないラベルは Node が読む (x/net/idna は読まない)
		{"https://XN---0X1F.x/", "https://xn---0x1f.x/", true},
		{"https://xn---.x/", "", false},
		{"https://xn---0x1f-a.x/", "", false},
		{"https://a.xn---ab/", "", false},
		// U+0130 は mapping で "i" + U+0307 になり、xn-- の後ろに非 ASCII が残る
		{"https://XN--İxn--/", "", false},
		// 全角は NFKC で xn-- になる
		{"https://ｘｎ--Ä-.x/", "", false},
		{"https://ＸＮ－－.x/", "", false},
		{"https://%45.x/", "https://e.x/", true},
		{"https://e.x?", "https://e.x/?", true},
		{"https://e.x#", "https://e.x/#", true},
		{"https://e.x/??##", "https://e.x/??##", true},
		{"https://0x7f.1/", "https://127.0.0.1/", true},
		{"https://127.1/", "https://127.0.0.1/", true},
		{"https://0300.0250.0.1/", "https://192.168.0.1/", true},
		{"https://4294967295/", "https://255.255.255.255/", true},
		{"https://4294967296/", "", false},
		{"https://1.2.3.256/", "", false},
		{"https://1.2.3.4.5/", "", false},
		{"https://1..2/", "", false},
		{"https://1.2.3.4./", "https://1.2.3.4/", true},
		{"https://0x/", "https://0.0.0.0/", true},
		{"https://0x.0x.0/", "https://0.0.0.0/", true},
		{"https://a.0x/", "", false},
		{"https://a.09/", "", false},
		{"https://a.08b/", "https://a.08b/", true},
		{"https://09/", "", false},
		{"https://1.0xg/", "https://1.0xg/", true},
		{"https://99999999999999999999/", "", false},
		{"https://[::1]/", "https://[::1]/", true},
		{"https://[0:0:0:0:0:0:0:1]/", "https://[::1]/", true},
		{"https://[1:0:0:2:0:0:0:3]/", "https://[1:0:0:2::3]/", true},
		{"https://[1:0:0:0:2:0:0:3]/", "https://[1::2:0:0:3]/", true},
		{"https://[::ffff:1.2.3.4]/", "https://[::ffff:102:304]/", true},
		{"https://[::1.2.3.4]/", "https://[::102:304]/", true},
		{"https://[::01.2.3.4]/", "", false},
		{"https://[::1.2.3]/", "", false},
		{"https://[::1.2.3.4.5]/", "", false},
		{"https://[1::2::3]/", "", false},
		{"https://[1:2:3:4:5:6:7:8:9]/", "", false},
		{"https://[1:2:3:4:5:6:7::]/", "https://[1:2:3:4:5:6:7:0]/", true},
		{"https://[::1:2:3:4:5:6:7]/", "https://[0:1:2:3:4:5:6:7]/", true},
		{"https://[:1]/", "", false},
		{"https://[1:]/", "", false},
		{"https://[12345::]/", "", false},
		{"https://[::1]:8080/", "https://[::1]:8080/", true},
		{"https://[::1/", "", false},
		{"https://[a:b:c:d:e:f:1.2.3.4]/", "https://[a:b:c:d:e:f:102:304]/", true},
		{"https://[a:b:c:d:e:f:a:1.2.3.4]/", "", false},
		{"https://[1:2:3:4:5:6:7:1.2.3.4]/", "", false},
		{"https://[::256.1.1.1]/", "", false},
		{"https://[::1.]/", "", false},
		{"https://[x]/", "", false},
		{"https://%65.x/", "https://e.x/", true},
		{"https://%/", "", false},
		{"https://a%2.x/", "", false},
		{"https://%ff.x/", "", false},
		{"https://%e4%be%8b.x/", "https://xn--fsq.x/", true},
		{"https://a b/", "", false},
		{"https://a<b/", "", false},
		{"https://a^b/", "", false},
		{"https://a|b/", "", false},
		{"https://a\"b/", "https://a\"b/", true},
		{"https://a{b}`/", "https://a{b}`/", true},
		{"https://例え.テスト/", "https://xn--r8jz45g.xn--zckzah/", true},
		{"https://ＥＸ.com/", "https://ex.com/", true},
		{"https://ß.de/", "https://xn--zca.de/", true},
		{"https://İ.x/", "https://xn--i-9bb.x/", true},
		{"https://xn--a.com/", "", false},
		{"https://xn--r8jz45g.x/", "https://xn--r8jz45g.x/", true},
		{"https://XN--R8JZ45G.x/", "https://xn--r8jz45g.x/", true},
		{"https://xn--.x/", "", false},
		{"https://\u00adxn--.x/", "", false},
		{"https://xn--Ä-.x/", "", false},
		{"https://a。b/", "https://a.b/", true},
		{"https://a\u200db/", "", false},
		{"https://a\u00adb/", "https://ab/", true},
		{"https://１２７.０.０.１/", "https://127.0.0.1/", true},
		{"https://-a-.x/", "https://-a-.x/", true},
		{"https://a--b.x/", "https://a--b.x/", true},
		{"https://ab--c.x/", "https://ab--c.x/", true},
		{"https://aא.x/", "https://xn--a-0hc.x/", true},
		{"https://aאא.x/", "", false},
		{"https://١٢.x/", "https://xn--9hbc.x/", true},
		{"https://١a.x/", "", false},
		{"https://=١.x/", "https://xn--=-bqc.x/", true},
		{"https://١=.x/", "", false},
		{"https://1é١.x/", "", false},
		{"https://a=.١/", "https://a=.xn--9hb/", true},
		{"https://א\u05b0.x/", "https://xn--7cb7d.x/", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := whatwgHref(tc.in)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestJSTrim checks the whitespace set of String.prototype.trim.
func TestJSTrim(t *testing.T) {
	ws := "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"
	assert.Equal(t, "a b", jsTrim(ws+"a b"+ws))
	// JS の trim は U+0085・U+180E・U+200B を空白として扱わない
	for _, s := range []string{"\u0085a", "\u180ea", "\u200ba", "a\u0085", "a\u200b"} {
		assert.Equal(t, s, jsTrim(s))
	}
}
