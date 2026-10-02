package mfm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// serializeTree renders nodes in the same compact form as the mfm-js script
// used to produce the expectations below (type, the identifying prop, and
// children in brackets). Links carry their URL and a trailing "!" when silent;
// URLs written as `<https://...>` carry a trailing "<>".
func serializeTree(nodes []*Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		head := string(n.Type)
		switch n.Type {
		case NodeText:
			head += ":" + fmt.Sprint(n.Props["text"])
		case NodeMention:
			head += ":" + fmt.Sprint(n.Props["acct"])
		case NodeEmojiCode:
			head += ":" + fmt.Sprint(n.Props["name"])
		case NodeHashtag:
			head += ":" + fmt.Sprint(n.Props["hashtag"])
		case NodeURL:
			head += ":" + fmt.Sprint(n.Props["url"])
			if brackets, _ := n.Props["brackets"].(bool); brackets {
				head += "<>"
			}
		case NodeLink:
			head += ":" + fmt.Sprint(n.Props["url"])
			if silent, _ := n.Props["silent"].(bool); silent {
				head += "!"
			}
		}
		if len(n.Children) > 0 {
			head += "[" + serializeTree(n.Children) + "]"
		}
		parts = append(parts, head)
	}
	return strings.Join(parts, "|")
}

// TestParse_MentionMatchesMfmJs fixes mention parsing to mfm-js 0.26.0
// (#3300). Expected trees were produced by running mfm-js's parse on each
// input.
//
// 以前の mk-go は末尾の `.` / `-` を削っても読み進めた位置を戻さず、`@a. hi`
// の `.` が出力から消えていた。また mfm-js が不正として文字にする形
// (`@.a` / `@a-@h` など) をメンションにしていた。
func TestParse_MentionMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"@a. hi", "mention:@a|text:. hi"},
		{"@a@h. hi", "mention:@a@h|text:. hi"},
		{"@a@h.:b:", "mention:@a@h|text:.|emojiCode:b"},
		{"@.a:b:", "text:@.a|emojiCode:b"},
		{"@a-@h:b:", "text:@a-@h|emojiCode:b"},
		{"@alice. \u3042\u308a\u304c\u3068\u3046", "mention:@alice|text:. \u3042\u308a\u304c\u3068\u3046"},
		{"@a", "mention:@a"},
		{"@a.", "mention:@a|text:."},
		{"@a-", "mention:@a|text:-"},
		{"@a..", "mention:@a|text:.."},
		{"@a@", "mention:@a|text:@"},
		{"@a@h", "mention:@a@h"},
		{"@a@h.", "mention:@a@h|text:."},
		{"@a@.h", "text:@a@.h"},
		{"@a@-h", "text:@a@-h"},
		{"@-a", "text:@-a"},
		{"@.a", "text:@.a"},
		{"@a.@h", "text:@a.@h"},
		{"@a-.@h.x", "text:@a-.@h.x"},
		{"x@a", "text:x@a"},
		{"1@a", "text:1@a"},
		{"@a_b.c-d", "mention:@a_b.c-d"},
		{"@a@h.example.com.", "mention:@a@h.example.com|text:."},
		{"@a@h.example.com-", "mention:@a@h.example.com|text:-"},
		{"(@a)", "text:(|mention:@a|text:)"},
		{"**@a.**", "bold[mention:@a|text:.]"},
		{"@a b", "mention:@a|text: b"},
		{"@@a", "text:@|mention:@a"},
		{"@a@@h", "mention:@a|text:@|mention:@h"},
		{"@a@h@x", "mention:@a@h|text:@x"},
		{"[@a](https://x.example)", "link:https://x.example[text:@a]"},
		{"[#tag](https://x.example)", "link:https://x.example[text:#tag]"},
		{"[https://y.example](https://x.example)", "link:https://x.example[text:https://y.example]"},
		{"[:a:](https://x.example)", "link:https://x.example[emojiCode:a]"},
		{"[<b>a\n> @b #t https://q.example\n</b>](https://x.example)", "link:https://x.example[bold[text:a\n> @b #t https://q.example\n]]"},
		{"[$[x a\n> @b\n]](https://x.example)", "link:https://x.example[fn[text:a\n> @b\n]]"},
		{"@a@.", "text:@a@."},
		{"@a@-", "text:@a@-"},
		{"@a@..x", "text:@a@..x"},
		{"@A.B", "mention:@A.B"},
		{"@a-b-", "mention:@a-b|text:-"},
		{"@a.-@h", "text:@a.-@h"},
		{"@a@h-.", "mention:@a@h|text:-."},
		{"@ a", "text:@ a"},
		{"@\n", "text:@\n"},
		{"@a@h.:b: tail", "mention:@a@h|text:.|emojiCode:b|text: tail"},
		{"[#tag](foo)", "text:[|hashtag:tag|text:](foo)"},
		{"[@a@h](foo)", "text:[|mention:@a@h|text:](foo)"},
		{"[a](<https://x.example>)", "link:https://x.example[text:a]"},
		{"[a](https://x.example.)", "text:[a](|url:https://x.example|text:.)"},
		{"[a](mailto:x@example.com)", "text:[a](mailto:x@example.com)"},
		{"[a](https://x.example/(p))", "link:https://x.example/(p)[text:a]"},
		{"?[a](https://x.example)", "link:https://x.example![text:a]"},
		{"[a](<https://x y>)", "text:[a](<|url:https://x|text: y>)"},
		{"[a](<https://>)", "text:[a](<https://>)"},
		{"[a](https://x.example) b", "link:https://x.example[text:a]|text: b"},
		{"[a](https://x.example", "text:[a](|url:https://x.example"},
		{"[@a@h](<https://x.example>)", "link:https://x.example[text:@a@h]"},
		{"[a](https://x.example/p?q=1#f)", "link:https://x.example/p?q=1#f[text:a]"},
		{"[a](http://x.example)", "link:http://x.example[text:a]"},
		{"[a]( https://x.example)", "text:[a]( |url:https://x.example|text:)"},
		{"[a](http://a)", "link:http://a[text:a]"},
		{"http://a", "url:http://a"},
		{"https://a", "url:https://a"},
		{"[a](<https://x\ty>)", "text:[a](<|url:https://x|text:\ty>)"},
		{"[a](<https://x　y>)", "text:[a](<|url:https://x|text:　y>)"},
		{"[a](<http://x>)", "link:http://x[text:a]"},
		{"[a](<ftp://x>)", "text:[a](<ftp://x>)"},
		{"[a](<https://x )", "text:[a](<|url:https://x|text: )"},
		{"[a](<https://x\t)", "text:[a](<|url:https://x|text:\t)"},
		{"[a](<https://x\ny>)", "link:https://x\ny[text:a]"},
		{"?[:e: ~~s~~](http://a)", "link:http://a![emojiCode:e|text: |strike[text:s]]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}
