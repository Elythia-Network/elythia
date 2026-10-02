package mfm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParse_CenterMatchesMfmJs fixes <center> parsing to mfm-js 0.26.0
// (#3328). Expected trees were produced by running mfm-js's parse on each
// input.
//
// 以前の mk-go は center を <b> などと同じく行の途中でも読み、閉じの直後が
// 行の終わりかも、前後の改行も見ていなかった。装飾の子 (inline) の中でも読んでいた。
func TestParse_CenterMatchesMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<center>a</center>", "center[text:a]"},
		{"x <center>a</center>", "text:x <center>a</center>"},
		{"x <center>a 検索</center>", "text:x <center>a 検索</center>"},
		{"<center>a 検索</center>", "center[text:a 検索]"},
		{"<center>a</center> x", "text:<center>a</center> x"},
		{"<center>a</center>x", "text:<center>a</center>x"},
		{"<center>a</center>\nx", "center[text:a]|text:x"},
		{"<center>a</center>\n\nx", "center[text:a]|text:\nx"},
		{"<center>a</center>\r\nx", "center[text:a]|text:x"},
		{"<center>a</center>\rx", "center[text:a]|text:x"},
		{"a\n<center>b</center>", "text:a|center[text:b]"},
		{"a\n\n<center>b</center>", "text:a\n|center[text:b]"},
		{"a\r\n<center>b</center>", "text:a|center[text:b]"},
		{"a\r<center>b</center>", "text:a|center[text:b]"},
		{"\n<center>a</center>", "center[text:a]"},
		{"\n\n<center>a</center>\n\n", "text:\n|center[text:a]|text:\n"},
		{"<center>\na\n</center>", "center[text:a]"},
		{"<center>\r\na\r\n</center>", "center[text:a]"},
		{"<center>\n\na\n\n</center>", "center[text:\na\n]"},
		{"<center>\n</center>", "text:<center>\n</center>"},
		{"<center></center>", "text:<center></center>"},
		{"<center>\na</center>", "center[text:a]"},
		{"<center>a\nb</center>", "center[text:a\nb]"},
		{"<center>**a**</center>", "center[bold[text:a]]"},
		{"<center><b>a</b> @u :e: #t</center>", "center[bold[text:a]|text: |mention:@u|text: |emojiCode:e|text: |hashtag:t]"},
		{"<center><center>a</center></center>", "text:<center><center>a</center></center>"},
		{"<center>a\n<center>b</center>\n</center>", "center[text:a\n<center>b]|text:</center>"},
		{"<center>a", "text:<center>a"},
		{"<center>a\n</center", "text:<center>a\n</center"},
		{"<center>a</center><center>b</center>", "text:<center>a</center><center>b</center>"},
		{"<center>a</center>\n<center>b</center>", "center[text:a]|center[text:b]"},
		{"<center>a</center>\n\n<center>b</center>", "center[text:a]|center[text:b]"},
		{"> q\n<center>a</center>", "quote[text:q]|center[text:a]"},
		{"> <center>a</center>", "quote[center[text:a]]"},
		{"> x <center>a</center>", "quote[text:x <center>a</center>]"},
		{"> <center>a</center>\n> b", "quote[center[text:a]|text:b]"},
		{"```\ncode\n```\n<center>a</center>", "blockCode|center[text:a]"},
		{"<b><center>a</center></b>", "bold[text:<center>a</center>]"},
		{"<b>\n<center>a</center>\n</b>", "bold[text:\n<center>a</center>\n]"},
		{"$[x \n<center>a</center>\n]", "fn[text:\n<center>a</center>\n]"},
		{"[\n<center>a</center>\n](https://e.com)", "text:[|center[text:a]|text:](|url:https://e.com|text:)"},
		{"<center>a</center></center>", "text:<center>a</center></center>"},
		{"<center>a</center> </center>\nx", "text:<center>a</center> </center>\nx"},
		{"<center>**a</center>**</center>", "center[bold[text:a</center>]]"},
		{"<center>`a</center>`</center>", "center[inlineCode]"},
		{"<center>a\n\n</center>", "center[text:a\n]"},
		{"<center>\r\r\na</center>", "center[text:\r\na]"},
		{"<center>a\r\n\r\n</center>", "center[text:a\r\n]"},
		{"<center>> a</center>", "center[text:> a]"},
		{"<small>\n<center>a</center>\n</small>", "small[text:\n<center>a</center>\n]"},
		{"x\n<center>a</center> y\n<center>b</center>", "text:x\n<center>a</center> y|center[text:b]"},
		{"<center>a</center>\n", "center[text:a]"},
		{">>>>>>>>>>>>>>>>>>> <center>ab</center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[center[text:ab]]]]]]]]]]]]]]]]]]]]"},
		{">>>>>>>>>>>>>>>>>>> <center>a**b**\n</center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[text:<center>a|bold[text:b]]]]]]]]]]]]]]]]]]]]|text:</center>"},
		{">>>>>>>>>>>>>>>>>>> <center></center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[text:<center></center>]]]]]]]]]]]]]]]]]]]"},
		{">>>>>>>>>>>>>>>>>> <center>a**b**</center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[center[text:a|bold[text:b]]]]]]]]]]]]]]]]]]]]"},
		{">>>>>>>>>>>>>>>>>>> <center>a @b :c: #d **e** 😀\n</center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[text:<center>a |mention:@b|text: |emojiCode:c|text: |hashtag:d|text: |bold[text:e]|text: |unicodeEmoji:😀]]]]]]]]]]]]]]]]]]]|text:</center>"},
		{">>>>>>>>>>>>>>>>>>> <center>a @b :c: #d **e** 😀</center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[center[text:a @b :c: #d **e** 😀]]]]]]]]]]]]]]]]]]]]"},
		{">>>>>>>>>>>>>>>>>> <center>a @b :c: #d **e** 😀</center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[center[text:a |mention:@b|text: |emojiCode:c|text: |hashtag:d|text: |bold[text:e]|text: |unicodeEmoji:😀]]]]]]]]]]]]]]]]]]]"},
		{">>>>>>>>>>>>>>>>>>>> <center>a</center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[text:<center>a</center>]]]]]]]]]]]]]]]]]]]]"},
		{">>>>>>>>>>>>>>>>>>>>> <center>a</center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[text:> <center>a</center>]]]]]]]]]]]]]]]]]]]]"},
		{">>>>>>>>>>>>>>>>>>> <center>a\n> </center>", "quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[quote[text:<center>a]]]]]]]]]]]]]]]]]]|text:</center>]"},
		{"> <center>a\n> </center>\n> b", "quote[center[text:a]|text:b]"},
		{"<center>a</center>\n<center>b", "center[text:a]|text:<center>b"},
		{"<center>a\n</center>\n</center>", "center[text:a]|text:</center>"},
		{"<center>\n\n</center>", "text:<center>\n\n</center>"},
		{"<center>\r\n</center>", "text:<center>\r\n</center>"},
		{"<center>$[x a</center>]</center>", "center[fn[text:a</center>]]"},
		{"a\n<center>b</center>c\n<center>d</center>", "text:a\n<center>b</center>c|center[text:d]"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, serializeTree(Parse(tc.in)), "input %q", tc.in)
	}
}

// TestTryCenterTag_ChildrenAtDepthLimitAreText covers a center whose children
// would sit at the nesting limit. mfm-js's nest then reads every child as a
// single character, so the children are plain text up to the first
// </center> (or the newline before it).
//
// Parse からは 19 段の quote の中でしか届かず、quote の中身には CR が残らないので、
// 改行の形と不正な UTF-8 は state を直接作って確かめる
func TestTryCenterTag_ChildrenAtDepthLimitAreText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<center>a @b **c**</center>", "center[text:a @b **c**]"},
		{"<center>a\r\n</center>", "center[text:a]"},
		{"<center>a\r</center>", "center[text:a]"},
		{"<center>a\n</center>\nx", "center[text:a]"},
		{"<center>a\xff</center>", "center[text:a\xff]"},
		{"<center>a", ""},
		{"<center>a\xff", ""},
		{"<center>\n</center>", ""},
		{"<center>a</center>x", ""},
	}
	for _, tc := range cases {
		s := newState(tc.in, false)
		s.depth, s.fullDepth = s.nestLimit-1, s.nestLimit-1
		got := ""
		if n := s.tryCenterTag(); n != nil {
			got = serializeTree([]*Node{n})
		} else {
			assert.Equal(t, 0, s.pos, "input %q: position must be restored", tc.in)
		}
		assert.Equal(t, tc.want, got, "input %q", tc.in)
	}
}

// TestParse_UnclosedCenterLinesStayLinear fixes that unclosed centers, tried
// at every line start, cost work proportional to the input length.
//
// 開きの直後から閉じを探す区間を行ごとに辿り直すと行数の 2 乗になる (32KB で
// 1 バイトあたり 100 を超え、仕事量の上限に届く)。メモ化が効いていれば
// 1 バイトあたり 8 程度 (実測)
func TestParse_UnclosedCenterLinesStayLinear(t *testing.T) {
	for _, unit := range []string{
		"<center>a\n",
		"\r\n<center>\r\n**",
		strings.Repeat(">", 19) + " <center>a\n",
	} {
		in := fill("", unit, 32000)
		s := newState(in, false)
		mergeText(s.parseNodes(false))
		assert.Less(t, s.budget.used, 16*len(in), "unit %q", unit)
	}
	// 開きを並べ、最後だけ閉じの直後が行末でない形。閉じの行末を子を集めた後で
	// 確かめると、開きの数だけ末尾まで読み直して 2 乗になる。
	for _, unit := range []string{"<center>a\n", "<center>a\r\n"} {
		in := fill("", unit, 32000) + "</center>x"
		s := newState(in, false)
		mergeText(s.parseNodes(false))
		assert.Less(t, s.budget.used, 16*len(in), "unit %q + </center>x", unit)
	}
}

// TestParse_QuoteMemoSeparatesTopLevelCenter fixes the tree of an input whose
// quote run could once be read from different top-level depths; it now matches
// mfm-js 0.26.0.
//
// #3301 で quote などの block 構文を最上位と引用の中身でだけ試すようにしたので、
// 1 つの表を別の最上位の深さから読む経路は無くなり、この入力は mfm-js と同じ木に
// なる (最上位の center が 2 行目の閉じで閉じる)。最上位かどうかの軸は守りとして
// 表に残しており、このテストはその入力で mfm-js と一致することを固定する。
func TestParse_QuoteMemoSeparatesTopLevelCenter(t *testing.T) {
	q := strings.Repeat(">", 18) + " "
	in := "<center>\n" + q + "<center>`</center>`\n" + q + "<center>y</center>\n" + q + "</center>"
	assert.Equal(t,
		"center[text:"+q+"<center>|inlineCode|text:\n"+q+"<center>y]|"+strings.Repeat("quote[", 18)+"text:</center>"+strings.Repeat("]", 18),
		serializeTree(Parse(in)))
}
