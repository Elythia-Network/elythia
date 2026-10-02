package mfm

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// serializeTreeWithFn is serializeTree plus the fn name and arguments
// (`fn:name.k=v,k2`, keys sorted), matching the mfm-js script used for the
// expectations below.
func serializeTreeWithFn(nodes []*Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.Type != NodeFn {
			head := serializeTree([]*Node{{Type: n.Type, Props: n.Props}})
			if len(n.Children) > 0 {
				head += "[" + serializeTreeWithFn(n.Children) + "]"
			}
			parts = append(parts, head)
			continue
		}
		head := "fn:" + fmt.Sprint(n.Props["name"])
		if args, _ := n.Props["args"].(map[string]any); len(args) > 0 {
			keys := make([]string, 0, len(args))
			for k := range args {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i, k := range keys {
				if v, ok := args[k].(string); ok {
					keys[i] = k + "=" + v
				}
			}
			head += "." + strings.Join(keys, ",")
		}
		if len(n.Children) > 0 {
			head += "[" + serializeTreeWithFn(n.Children) + "]"
		}
		parts = append(parts, head)
	}
	return strings.Join(parts, "|")
}

// TestParse_UnclosedDecorationMatchesMfmJs fixes the decorations built with
// mfm-js 0.26.0's seqOrText (<b> <i> <s> <small> ** *** ~~ $[) to mfm-js
// (#3301). Expected trees were produced by running mfm-js's parse on each
// input.
//
// seqOrText は開きが合って後ろが失敗すると、読んだ範囲 (開きと中身) を
// まるごと文字にする。中身は閉じか末尾 (~~ は改行) まで読むので、閉じが無いと
// 中のカスタム絵文字・ハッシュタグも文字になる。以前の mk-go は 1 文字進めて
// 読み直したので、`<b>a:b:` の `b` を絵文字にし、`**#tag**` を `**` と
// タグ `tag**` にしていた。
func TestParse_UnclosedDecorationMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		// issue の例
		{"<b>a:b:", "text:<b>a:b:"},
		{"**:a b:b:", "text:**:a b:b:"},
		{"<b>x :b:", "text:<b>x :b:"},
		{"**x :b:", "text:**x :b:"},
		{"$[x :b:", "text:$[x :b:"},
		{"**#tag**", "text:**#tag**"},
		{"~~#a~~", "text:~~#a~~"},
		{"x **#a** y", "text:x **#a** y"},
		{"<b>#a</b>", "bold[hashtag:a]"},
		{"#a **b**", "hashtag:a|text: |bold[text:b]"},
		// HTML 形式のタグ
		{"<i>a:b:", "text:<i>a:b:"},
		{"<s>a:b:", "text:<s>a:b:"},
		{"<small>a:b:", "text:<small>a:b:"},
		{"<b>a <i>x</b> :e:", "text:<b>a <i>x</b> :e:"},
		{"<b>**x</b>**", "text:<b>**x</b>**"},
		// 中身が無いときは開きだけを文字にして、その後ろから読み直す
		{"<b></b>:e:", "text:<b></b>|emojiCode:e"},
		{"**", "text:**"},
		{"****:a:", "text:****:a:"},
		{"**a** **b :e:", "bold[text:a]|text: **b :e:"},
		// ** の中身は改行で止まらない
		{"**a\nb**", "bold[text:a\nb]"},
		{"**a\n:e:", "text:**a\n:e:"},
		// ~~ の中身は改行 (CRLF / CR / LF) で止まる
		{"~~a\n:e:", "text:~~a\n|emojiCode:e"},
		{"~~a\r\n:e:", "text:~~a\r\n|emojiCode:e"},
		{"~~a\r:e:", "text:~~a\r|emojiCode:e"},
		{"~~\n:e:", "text:~~\n|emojiCode:e"},
		{"~~a~~ ~~b :e:", "strike[text:a]|text: ~~b :e:"},
		// *** (big) は ** より先に試される
		{"***a***", "fn:tada[text:a]"},
		{"***:a:", "text:***:a:"},
		{"***a**", "text:***a**"},
		{"******:a:", "text:******:a:"},
		{"***a*** **b**", "fn:tada[text:a]|text: |bold[text:b]"},
		// $[ は関数名・引数・空白・中身・] のどこで失敗したかで、文字にする範囲が変わる
		{"$[:e:", "text:$[|emojiCode:e"},
		{"$[x:e:", "text:$[x|emojiCode:e"},
		{"$[x.:e:", "text:$[x.|emojiCode:e"},
		{"$[x.a,:e:", "text:$[x.a,|emojiCode:e"},
		{"$[x.a=:e:", "text:$[x.a=|emojiCode:e"},
		{"$[x :e:", "text:$[x :e:"},
		{"$[x ]:e:", "text:$[x ]|emojiCode:e"},
		{"$[x.a=あ b]", "text:$[x.a=あ b]"},
		{"$[x.a, b]", "text:$[x.a, b]"},
		{"$[x. a]", "text:$[x. a]"},
		{"$[x.a=b-c.d,e b]", "fn:x.a=b-c.d,e[text:b]"},
		{"$[x.a=b,c=d e]", "fn:x.a=b,c=d[text:e]"},
		{"$[x.a= b]", "text:$[x.a= b]"},
		{"$[[a](https://e.x)", "text:$[|link:https://e.x[text:a]"},
		{"$[x[a](https://e.x)", "text:$[x|link:https://e.x[text:a]"},
		{"$[x.[a](https://e.x)", "text:$[x.|link:https://e.x[text:a]"},
		{"$[x.a,[a](https://e.x)", "text:$[x.a,|link:https://e.x[text:a]"},
		// リンクのラベルの中の閉じない <b> は末尾まで読むので、リンクも閉じない
		{"[<b>](https://e.x) :e:", "text:[<b>](https://e.x) :e:"},
		{"<b>[#t](x)</b>", "bold[text:[|hashtag:t|text:](x)]"},
		// リンクのラベルは CR でも止まる。止まらないと、閉じない ~~ を含む $[ が
		// リンクで閉じてしまう
		{"[a\rb](https://e.x/)", "text:[a\rb](|url:https://e.x/|text:)"},
		{"$[x [a\r~~b](https://e.x/)]", "text:$[x [a\r~~b](https://e.x/)]"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, serializeTreeWithFn(Parse(tc.in)), "input %q", tc.in)
	}
}

// TestParse_DecorationAtNestLimitMatchesMfmJs fixes how mfm-js 0.26.0 reads
// the children at the nest limit (#3301): mfm-js's nest reads them one
// character at a time, so a decoration there still reads up to its close, and
// the whole range becomes text when the close is missing.
func TestParse_DecorationAtNestLimitMatchesMfmJs(t *testing.T) {
	wrap := func(d int, s string) string { return strings.Repeat("<b>", d) + s + strings.Repeat("</b>", d) }
	bolds := func(d int, s string) string { return strings.Repeat("bold[", d) + s + strings.Repeat("]", d) }
	cases := []struct{ in, want string }{
		// 上限の手前ではタグが閉じの ** を読むので、** は閉じずに末尾までが文字になる
		{wrap(18, "**#t**"), "text:" + wrap(18, "**#t**")},
		// 上限では中身を 1 文字ずつ読むので、** が閉じる
		{wrap(19, "**#t**"), bolds(19, "bold[text:#t]")},
		{wrap(19, ":e:#t"), bolds(19, "emojiCode:e|hashtag:t")},
		{wrap(20, ":e:#t"), bolds(20, "text::e:#t")},
		{wrap(19, "**:e:"), "text:" + wrap(19, "**:e:")},
		{wrap(19, "$[x.a=b :e:]"), bolds(19, "fn:x.a=b[text::e:]")},
		{strings.Repeat("<b>", 19) + "a:e:", "text:" + strings.Repeat("<b>", 19) + "a:e:"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, serializeTreeWithFn(Parse(tc.in)), "input %q", tc.in)
	}
}

// TestParse_LinkLabelNestsOneLevelMatchesMfmJs fixes that a link label is read
// one nesting level deeper, like mfm-js 0.26.0's nest(labelInline). Expected
// trees were produced by running mfm-js's parse on each input.
//
// ラベルを深さを変えずに読むと、ラベルの中の装飾が mfm-js より 1 段深くまで
// 構文として読まれる。上限の 1 段手前の装飾の中身は、mfm-js では 1 文字ずつ
// 読まれて `**x` などが文字になるが、深さを変えずに読むと閉じない装飾として
// 読み、閉じの `</i>` を飲み込んでリンクごと文字にしていた。
func TestParse_LinkLabelNestsOneLevelMatchesMfmJs(t *testing.T) {
	label := func(open, close string, d int, s string) string {
		return strings.Repeat(open, d) + s + strings.Repeat(close, d)
	}
	link := func(open, close string, d int, s string) string {
		return "[" + label(open, close, d, s) + "](https://e.x)"
	}
	nested := func(head string, d int, s string) string {
		return strings.Repeat(head+"[", d) + s + strings.Repeat("]", d)
	}
	cases := []struct{ in, want string }{
		{link("<i>", "</i>", 19, "**x"), "link:https://e.x[" + nested("italic", 19, "text:**x") + "]"},
		{link("<s>", "</s>", 19, "***x"), "link:https://e.x[" + nested("strike", 19, "text:***x") + "]"},
		{link("<small>", "</small>", 19, "**x"), "link:https://e.x[" + nested("small", 19, "text:**x") + "]"},
		{link("$[fg.color=f00 ", "]", 19, "***x"), "link:https://e.x[" + nested("fn:fg.color=f00", 19, "text:***x") + "]"},
		{link("<i>", "</i>", 19, "~~$["), "link:https://e.x[" + nested("italic", 19, "text:~~$[") + "]"},
		{"?" + link("<i>", "</i>", 19, "***"), "link:https://e.x![" + nested("italic", 19, "text:***") + "]"},
		// 上限では中身を 1 文字ずつ読むので、絵文字やハッシュタグにならない
		{link("<i>", "</i>", 19, ":e: #t"), "link:https://e.x[" + nested("italic", 19, "text::e: #t") + "]"},
		{link("<i>", "</i>", 18, ":e:"), "link:https://e.x[" + nested("italic", 18, "emojiCode:e") + "]"},
		// 1 段手前では ** を構文として読み、閉じが無いので末尾までが文字になる
		{link("<i>", "</i>", 18, "**x"), "text:" + link("<i>", "</i>", 18, "**x")},
		// 上限を超えた <i> は文字になり、最初の </i> で 19 段目が閉じる
		{link("<i>", "</i>", 20, "**x"), "link:https://e.x[" + nested("italic", 19, "text:<i>**x") + "|text:</i>]"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, serializeTreeWithFn(Parse(tc.in)), "input %q", tc.in)
	}
}

// TestParse_BlocksAreNotReadInsideInlineMatchesMfmJs fixes that quote / code
// block / math block / search are read only where mfm-js reads full syntax (the
// top level and quote content), not inside decorations or center (#3301).
// Expected trees were produced by running mfm-js 0.26.0's parse.
//
// 子で引用などを読むと、閉じのある装飾も閉じを飲まれて丸ごと文字になり、後ろの
// メンション・タグ・絵文字が落ちる。
func TestParse_BlocksAreNotReadInsideInlineMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<b>見出し\n> 引用</b>\n本文 #tag @user :e:", "bold[text:見出し\n> 引用]|text:\n本文 |hashtag:tag|text: |mention:@user|text: |emojiCode:e"},
		{"**a\n> b**\n#t", "bold[text:a\n> b]|text:\n|hashtag:t"},
		{"$[x a\n> b]\n@u", "fn[text:a\n> b]|text:\n|mention:@u"},
		{"<b>a\n```\nc\n```</b> #t", "bold[text:a\n```\nc\n```]|text: |hashtag:t"},
		{"<b>a\n\\[x\\]</b> #t", "bold[text:a\n\\[x\\]]|text: |hashtag:t"},
		{"<b>a\nfoo 検索</b> #t", "bold[text:a\nfoo 検索]|text: |hashtag:t"},
		{"<center>a\n> b</center>", "center[text:a\n> b]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}
