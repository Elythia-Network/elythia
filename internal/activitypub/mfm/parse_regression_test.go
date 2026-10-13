package mfm

import (
	"fmt"
	"strings"
	"testing"

	mfmparser "github.com/elythia-network/mfm"
	"github.com/stretchr/testify/assert"
)

// These cases pin the trees that ToHTML depends on for math and code blocks,
// so that an update of github.com/elythia-network/mfm that changes them fails
// here as well as in the HTML tests.

// serializeTreeDetailed renders nodes as type:props, with children in brackets.
func serializeTreeDetailed(nodes []*mfmparser.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		var head string
		switch n.Type {
		case mfmparser.NodeText:
			head = string(n.Type) + ":" + fmt.Sprint(n.Props["text"])
		case mfmparser.NodeInlineCode:
			head = string(n.Type) + ":" + fmt.Sprint(n.Props["code"])
		case mfmparser.NodeBlockCode:
			lang, _ := n.Props["lang"].(string)
			head = string(n.Type) + ":" + lang + ":" + fmt.Sprint(n.Props["code"])
		case mfmparser.NodeMathInline, mfmparser.NodeMathBlock:
			head = string(n.Type) + ":" + fmt.Sprint(n.Props["formula"])
		default:
			head = string(n.Type)
		}
		if len(n.Children) > 0 {
			head += "[" + serializeTreeDetailed(n.Children) + "]"
		}
		parts = append(parts, head)
	}
	return strings.Join(parts, "|")
}

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
			assert.Equal(t, tc.want, serializeTreeDetailed(mfmparser.Parse(tc.in)))
		})
	}
}

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
			assert.Equal(t, tc.want, serializeTreeDetailed(mfmparser.Parse(tc.in)))
		})
	}
}
