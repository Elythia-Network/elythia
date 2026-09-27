package mfm

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// types lists the node types of nodes (top level only).
func types(nodes []*Node) []NodeType {
	out := make([]NodeType, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Type)
	}
	return out
}

// TestParse_IndexedSyntaxPaths covers the success and failure paths of the
// constructs whose closing delimiter is looked up in a precomputed index
// (数式 / <plain> / link の括弧)。索引の引き方を誤ると閉じているのに失敗する
// か、閉じていないのに成功するので、両方向を並べておく。
func TestParse_IndexedSyntaxPaths(t *testing.T) {
	cases := []struct {
		in   string
		want []NodeType
	}{
		{`a \(x+1\) b`, []NodeType{NodeText, NodeMathInline, NodeText}},
		{`a \(x b`, []NodeType{NodeText}},
		{"a \\(x\ny\\) b", []NodeType{NodeText}},
		{"\\[x\ny\\]", []NodeType{NodeMathBlock}},
		{"a\n\\[x\\]\nb", []NodeType{NodeText, NodeMathBlock, NodeText}},
		{"\\[x", []NodeType{NodeText}},
		{"<plain>**a**</plain>", []NodeType{NodePlain}},
		{"<plain>a", []NodeType{NodeText}},
		{"<plain>a\nb</plain>", []NodeType{NodePlain}},
		{"[l](https://a.b/(c))", []NodeType{NodeLink}},
		{"?[l](https://a.b)", []NodeType{NodeLink}},
		{"[l](https://a.b", []NodeType{NodeText, NodeURL}},
		{"[l](https://a.b/(c)", []NodeType{NodeText, NodeURL}},
		{"[a\nb](https://a.b)", []NodeType{NodeText, NodeURL, NodeText}},
		{">> a", []NodeType{NodeQuote}},
		{"a\n> b", []NodeType{NodeText, NodeQuote}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, types(Parse(tc.in)))
		})
	}
}

func TestParse_NestedQuoteKeepsStructure(t *testing.T) {
	nodes := Parse(">> a")
	require.Len(t, nodes, 1)
	require.Len(t, nodes[0].Children, 1)
	assert.Equal(t, NodeQuote, nodes[0].Children[0].Type)
}

func TestParseSimple_Plain(t *testing.T) {
	assert.Equal(t, []NodeType{NodePlain, NodeText, NodeEmojiCode}, types(ParseSimple("<plain>a</plain> :x:")))
	assert.Nil(t, ParseSimple(""))
}

func TestParse_InvalidUTF8DoesNotPanic(t *testing.T) {
	for _, in := range []string{"> \xff", "<plain>\xff", "\xff\xfe<b>\xff", "[\xff](https://a.b)", "\\(\xff\\)"} {
		assert.NotPanics(t, func() { Parse(in) }, "%q", in)
		assert.NotPanics(t, func() { ParseSimple(in) }, "%q", in)
	}
}

// TestIndexedSyntax_FallbackMatchesIndex runs each construct with the memo
// storage already exhausted, which forces the character-by-character fallback,
// and checks it agrees with the indexed path.
//
// 索引を作れないのはメモ表の確保が上限に届いたときだけで、そのときは仕事量も
// 使い切った扱いになり、通常の Parse からはフォールバックに入らない。ここでは
// 構文ごとの関数を直接呼んで、両経路の結果を突き合わせる。
func TestIndexedSyntax_FallbackMatchesIndex(t *testing.T) {
	tries := map[string]func(*state) *Node{
		"mathInline": (*state).tryMathInline,
		"mathBlock":  (*state).tryMathBlock,
		"plain":      (*state).tryPlainTag,
		"link":       (*state).tryLink,
		"fn":         (*state).tryFn,
	}
	inputs := []string{
		`\(x+1\) b`, `\(x b`, "\\(x\ny\\)", `\(\)`,
		"\\[x\ny\\]", "\\[x", "\\[ \\]",
		"<plain>**a**</plain>", "<plain>a", "<plain>a\nb</plain>",
		"[l](https://a.b/(c))", "?[l](https://a.b)", "[l](https://a.b", "[l](https://a.b/(c)", "[a\nb](https://a.b)",
		"$[x.a=1,b=2 c]", "$[x.a=v c]", "$[x.a=1 c", "$[x c]",
	}
	for name, try := range tries {
		for _, in := range inputs {
			indexed := newState(in, false)
			fallback := newState(in, false)
			fallback.budget.memUsed = memoByteLimit
			want := try(indexed)
			got := try(fallback)
			assert.Equal(t, dumpTree(want), dumpTree(got), "%s %q", name, in)
			assert.Equal(t, indexed.pos, fallback.pos, "%s %q: position", name, in)
		}
	}
}

func dumpTree(n *Node) string {
	if n == nil {
		return "<nil>"
	}
	s := string(n.Type)
	for _, k := range []string{"text", "formula", "url", "silent", "name"} {
		if v, ok := n.Props[k]; ok {
			s += "|" + k + "=" + toString(v)
		}
	}
	if a, ok := n.Props["args"].(map[string]any); ok {
		keys := make([]string, 0, len(a))
		for k := range a {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s += "|arg:" + k + "=" + toString(a[k])
		}
	}
	s += "("
	for _, c := range n.Children {
		s += dumpTree(c) + ","
	}
	return s + ")"
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return "?"
}
