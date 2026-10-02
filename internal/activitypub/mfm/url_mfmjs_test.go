package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
