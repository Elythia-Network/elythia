package mfm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParse_HashtagMatchesMfmJs fixes hashtag parsing to mfm-js 0.26.0
// (#3318). Expected trees were produced by running mfm-js's parse on each
// input (serializeTree is in mention_mfmjs_test.go).
//
// mfm-js の hashtag は ` \u3000\t.,!?'"#:/[]【】()「」（）<>` と改行で止まり、
// `()` / `[]` / `「」` / `（）` は閉じているときだけ nestLimit まで含める。以前の
// mk-go は `/` や `【】` も読み、閉じていない括弧も読み進めていた。
func TestParse_HashtagMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"#tag/foo bar", "hashtag:tag|text:/foo bar"},
		{"#a【b】", "hashtag:a|text:【b】"},
		{"#a】b", "hashtag:a|text:】b"},
		{"#】", "text:#】"},
		{"#a」b", "hashtag:a|text:」b"},
		{"#a）b", "hashtag:a|text:）b"},
		{"#a)b", "hashtag:a|text:)b"},
		{"#)", "text:#)"},
		{"#a(b c)", "hashtag:a|text:(b c)"},
		{"#abc[", "hashtag:abc|text:["},
		{"#[", "text:#["},
		{"a #[b", "text:a #[b"},
		{"[#/", "text:[#/"},
		{"#a b", "hashtag:a b"},
		{"#a(b", "hashtag:a|text:(b"},
		{"#a[b]c", "hashtag:a[b]c"},
		{"#(a)", "hashtag:(a)"},
		{"#[a]", "hashtag:[a]"},
		{"#a]b", "hashtag:a|text:]b"},
		{"(#tag)", "text:(|hashtag:tag|text:)"},
		{"[#tag]", "text:[|hashtag:tag|text:]"},
		{"#1", "text:#1"},
		{"#(1)", "hashtag:(1)"},
		{"#a.b", "hashtag:a|text:.b"},
		{"#a,b", "hashtag:a|text:,b"},
		{"#a!b", "hashtag:a|text:!b"},
		{"#a?b", "hashtag:a|text:?b"},
		{"#a'b", "hashtag:a|text:'b"},
		{"#a\"b", "hashtag:a|text:\"b"},
		{"#a#b", "hashtag:a|text:#b"},
		{"#a:b", "hashtag:a|text::b"},
		{"#a<b", "hashtag:a|text:<b"},
		{"#a>b", "hashtag:a|text:>b"},
		{"#a　b", "hashtag:a|text:　b"},
		{"#a\tb", "hashtag:a|text:\tb"},
		{"#a\nb", "hashtag:a|text:\nb"},
		{"#a\rb", "hashtag:a|text:\rb"},
		{"#a「b」c", "hashtag:a「b」c"},
		{"#a「b", "hashtag:a|text:「b"},
		{"#a（b）c", "hashtag:a（b）c"},
		{"#a（b", "hashtag:a|text:（b"},
		{"#a(b]c", "hashtag:a|text:(b]c"},
		{"#a[b)c", "hashtag:a|text:[b)c"},
		{"#a((b))", "hashtag:a((b))"},
		{"#a([b])", "hashtag:a([b])"},
		{"#a(b)(c)", "hashtag:a(b)(c)"},
		{"#a()", "hashtag:a()"},
		{"#()", "hashtag:()"},
		{"#日本語", "hashtag:日本語"},
		{"#😀a", "hashtag:😀a"},
		{"x#tag", "text:x#tag"},
		{"1#tag", "text:1#tag"},
		{"_#tag", "text:_|hashtag:tag"},
		{"あ#tag", "text:あ|hashtag:tag"},
		{"[#tag](https://e.x)", "link:https://e.x[text:#tag]"},
		{"[a #tag](https://e.x)", "link:https://e.x[text:a #tag]"},
		{"#a\u2028b", "hashtag:a\u2028b"},
		{"#a\u200bb", "hashtag:a\u200bb"},
		{"#a(((((((((((((((((((b)))))))))))))))))))", "hashtag:a(((((((((((((((((((b)))))))))))))))))))"},
		{"#a((((((((((((((((((((b))))))))))))))))))))", "hashtag:a((((((((((((((((((((b))))))))))))))))))))"},
		{"#a(((((((((((((((((((((b)))))))))))))))))))))", "hashtag:a|text:(((((((((((((((((((((b)))))))))))))))))))))"},
		{"#a((((((((((((((((((((((b))))))))))))))))))))))", "hashtag:a|text:((((((((((((((((((((((b))))))))))))))))))))))"},
		{"<b>#a((((((((((((((((((b))))))))))))))))))</b>", "bold[hashtag:a((((((((((((((((((b))))))))))))))))))]"},
		{"<b>#a(((((((((((((((((((b)))))))))))))))))))</b>", "bold[hashtag:a(((((((((((((((((((b)))))))))))))))))))]"},
		{"<b>#a((((((((((((((((((((b))))))))))))))))))))</b>", "bold[hashtag:a|text:((((((((((((((((((((b))))))))))))))))))))]"},
		{"<b>#a(((((((((((((((((((((b)))))))))))))))))))))</b>", "bold[hashtag:a|text:(((((((((((((((((((((b)))))))))))))))))))))]"},
		{"<b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b>#ab</b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b>", "bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[hashtag:ab]]]]]]]]]]]]]]]]]]]"},
		{"<b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b>#a(b)</b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b>", "bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[hashtag:a(b)]]]]]]]]]]]]]]]]]]]"},
		{"<b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b><b>#a((b))</b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b></b>", "bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[bold[hashtag:a|text:((b))]]]]]]]]]]]]]]]]]]]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}
