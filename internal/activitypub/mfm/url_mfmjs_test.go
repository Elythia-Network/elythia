package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParse_URLMatchesMfmJs fixes URL parsing to mfm-js 0.26.0 (#3302).
// Expected trees were produced by running mfm-js's parse on each input
// (serializeTree is in mention_mfmjs_test.go).
//
// mfm-js の url は ASCII の [.,a-z0-9_/:%#@$&?!~=+-] と、閉じた `(...)` /
// `[...]` だけを読む。以前の mk-go は空白と一部の記号以外を何でも読んだので、
// URL の後ろに続く日本語や記号まで URL にしていた。
func TestParse_URLMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://e.x/p:あ:b:", "url:https://e.x/p:|text:あ|emojiCode:b"},
		{"https://e.x/あ", "url:https://e.x/|text:あ"},
		{"https://e.x/p'q", "url:https://e.x/p|text:'q"},
		{"https://e.x/p\"q", "url:https://e.x/p|text:\"q"},
		{"https://e.x/p*q", "url:https://e.x/p|text:*q"},
		{"https://e.x/p;q", "url:https://e.x/p|text:;q"},
		{"https://e.x/p|q", "url:https://e.x/p|text:|q"},
		{"https://e.x/p{q}", "url:https://e.x/p|text:{q}"},
		{"https://e.x/p^q", "url:https://e.x/p|text:^q"},
		{"https://e.x/p\\q", "url:https://e.x/p|text:\\q"},
		{"https://e.x/(a)", "url:https://e.x/(a)"},
		{"https://e.x/(a", "url:https://e.x/|text:(a"},
		{"https://e.x/a)", "url:https://e.x/a|text:)"},
		{"https://e.x/[a]", "url:https://e.x/[a]"},
		{"https://e.x/[a", "url:https://e.x/|text:[a"},
		{"https://e.x/((a))", "url:https://e.x/((a))"},
		{"https://e.x/(a]", "url:https://e.x/|text:(a]"},
		{"https://e.x/(a)b)", "url:https://e.x/(a)b|text:)"},
		{"(https://e.x/a)", "text:(|url:https://e.x/a|text:)"},
		{"[https://e.x/a]", "text:[|url:https://e.x/a|text:]"},
		{"https://e.x.", "url:https://e.x|text:."},
		{"https://e.x,.", "url:https://e.x|text:,."},
		{"https://..", "text:https://.."},
		{"https://.", "text:https://."},
		{"https://e.x/a.b", "url:https://e.x/a.b"},
		{"https://e.x/?q=1&r=2#f", "url:https://e.x/?q=1&r=2#f"},
		{"https://e.x/%E3%81%82", "url:https://e.x/%E3%81%82"},
		{"https://E.X/A", "url:https://E.X/A"},
		{"HTTPS://e.x", "text:HTTPS://e.x"},
		{"https://e.x/a b", "url:https://e.x/a|text: b"},
		{"https://a(((((((((((((((((((((((((b)))))))))))))))))))))))))", "url:https://a|text:(((((((((((((((((((((((((b)))))))))))))))))))))))))"},
		{"https://a(((((((((((((((((((b)))))))))))))))))))", "url:https://a(((((((((((((((((((b)))))))))))))))))))"},
		{"https://a((((((((((((((((((((b))))))))))))))))))))", "url:https://a((((((((((((((((((((b))))))))))))))))))))"},
		{"<b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b>https://a((b))</b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b>", "bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[url:https://a((b))]]]]]]]]]]]]]]]]]]"},
		{"**https://a(b)**", "bold[url:https://a(b)]"},
		{"https://a<b>", "url:https://a|text:<b>"},
		{"https://e.x/a　b", "url:https://e.x/a|text:　b"},
		{"https://e.x/a\tb", "url:https://e.x/a|text:\tb"},
		{"x https://e.x/a.", "text:x |url:https://e.x/a|text:."},
		{"https://e.x/@a:b:", "url:https://e.x/@a:b:"},
		{"<center>[y](https://a((((((((((((((((((((b)))))))))))))))))))))", "text:<center>|link:https://a((((((((((((((((((((b))))))))))))))))))))[text:y]"},
		{"<center><b>[y](https://a(((((((((((((((((((b))))))))))))))))))))</b>", "text:<center>|bold[link:https://a(((((((((((((((((((b)))))))))))))))))))[text:y]]"},
		{"https://e.x/a_b-c~d!e$f+g,h", "url:https://e.x/a_b-c~d!e$f+g,h"},
		{"https://a(((((((((((((((((((((b)))))))))))))))))))))", "url:https://a|text:(((((((((((((((((((((b)))))))))))))))))))))"},
		{"<b>https://a((((((((((((((((((((b))))))))))))))))))))</b>", "bold[url:https://a|text:((((((((((((((((((((b))))))))))))))))))))]"},
		{"<b>https://a(((((((((((((((((((b)))))))))))))))))))</b>", "bold[url:https://a(((((((((((((((((((b)))))))))))))))))))]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}

// TestParse_URLAltMatchesMfmJs fixes `<https://...>` in text to mfm-js
// 0.26.0's urlAlt. Expected trees were produced by running mfm-js's parse;
// a trailing "<>" marks the brackets prop.
//
// `<>` で囲んだ URL は、url の文字に無い日本語や記号も含めて 1 つの URL になる。
// 閉じの `>` の前に空白があれば urlAlt にならない (改行は空白に含まない)。
func TestParse_URLAltMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<https://ja.wikipedia.org/wiki/日本>", "url:https://ja.wikipedia.org/wiki/日本<>"},
		{"<https://e.x/a|b>", "url:https://e.x/a|b<>"},
		{"a<https://e.x/p>b", "text:a|url:https://e.x/p<>|text:b"},
		{"<https://e.x/a b>", "text:<|url:https://e.x/a|text: b>"},
		{"<https://e.x/a\tb>", "text:<|url:https://e.x/a|text:\tb>"},
		{"<https://e.x/a\nb>", "url:https://e.x/a\nb<>"},
		{"<https://>", "text:<https://>"},
		{"<http://e.x/>", "url:http://e.x/<>"},
		{"<ftp://e.x/>", "text:<ftp://e.x/>"},
		{"<https://e.x/a", "text:<|url:https://e.x/a"},
		{"<https://e.x/a>>", "url:https://e.x/a<>|text:>"},
		{"<https://e.x/a<b>", "url:https://e.x/a<b<>"},
		{"[<https://e.x/a>](https://e.x/b)", "link:https://e.x/b[text:<https://e.x/a>]"},
		{"[a](<https://e.x/日本>)", "link:https://e.x/日本[text:a]"},
		{"<b><https://e.x/日本></b>", "bold[url:https://e.x/日本<>]"},
		{"**<https://e.x/日本>**", "bold[url:https://e.x/日本<>]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}
