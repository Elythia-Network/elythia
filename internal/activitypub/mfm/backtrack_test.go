package mfm

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseWithin fails the test when Parse does not return within d.
//
// 修正前のパーサは閉じない <b> を 1 段増やすごとに所要時間が倍になった
// (24 段で 30 秒)。落ちるときは永遠に返らないので、時間で打ち切る。
func parseWithin(t *testing.T, in string, d time.Duration) []*Node {
	t.Helper()
	done := make(chan []*Node, 1)
	go func() { done <- Parse(in) }()
	select {
	case n := <-done:
		return n
	case <-time.After(d):
		t.Fatalf("Parse did not finish within %v for %d bytes", d, len(in))
		return nil
	}
}

func TestParse_PathologicalInputsAreNotExponential(t *testing.T) {
	units := []string{
		"<b>", "<small>", "<center>", "<i>", "<s>",
		"**", "~~", "$[x ", "$[x.a=b ", "[", "[<b>", "?[",
		"<b>**~~$[x [<small><i>",
	}
	for _, u := range units {
		t.Run(u, func(t *testing.T) {
			// 200 段は修正前なら宇宙の寿命でも終わらない。時間は止まったことの検知に
			// だけ使う (CI の -race + atomic カバレッジでは 1 桁以上遅くなる)。
			// 3000 バイト級で上限に届かないことは仕事量で見る
			// (TestParse_LocalSizedPathologicalInputsStayWithinBudget)
			parseWithin(t, strings.Repeat(u, 200), 60*time.Second)
		})
	}
}

func TestParse_WorkAndMemoryStayBounded(t *testing.T) {
	for _, u := range []string{"<b>", "$[x ", "[<b>", "<b>**~~$[x [<small><i>"} {
		// 上限に届いた後の振る舞いを見るテストなので、予算を小さくして早く届かせる。
		// 本来の予算 (2^21 + 32/byte) のままだと CI の -race + atomic カバレッジで
		// このテストだけで 150 秒を超えた。-race 下では 1 桁以上遅くなるので、
		// 時間ではなく仕事量とメモリ量で見る
		in := strings.Repeat(u, (16<<10)/len(u))
		s := newState(in, false)
		s.budget.limit = 1<<16 + workBudgetPerByte*len(in)
		mergeText(s.parseNodes(false))
		require.True(t, s.budget.exhausted(), "the input must reach a cap for %q", u)
		assert.LessOrEqual(t, s.budget.memUsed, memoByteLimit, "memo storage must stay under the cap for %q", u)
		// 上限に達した後は、その時点で走っている子のループ (深さごとに高々 1 つ) が
		// 残りを 1 文字ずつテキストとして読むだけになる
		assert.LessOrEqual(t, s.budget.used, s.budget.limit+2*(s.nestLimit+2)*len(in), "work must stay linear for %q", u)
	}
}

func TestParse_DeepButBalancedNestingStillParses(t *testing.T) {
	// 上限 (20) の内側の正しい入れ子は従来どおり構文として読む
	in := strings.Repeat("<b>", 10) + "x" + strings.Repeat("</b>", 10)
	nodes := parseWithin(t, in, 5*time.Second)
	require.Len(t, nodes, 1)
	depth := 0
	for n := nodes[0]; n != nil; {
		require.Equal(t, NodeBold, n.Type)
		depth++
		if len(n.Children) != 1 || n.Children[0].Type != NodeBold {
			break
		}
		n = n.Children[0]
	}
	assert.Equal(t, 10, depth)
}

func TestParse_UnclosedPrefixKeepsLaterSyntax(t *testing.T) {
	// 閉じない開きタグが並んでも、その後ろの構文は失われない
	in := strings.Repeat("<b>", 50) + "**bold**"
	nodes := parseWithin(t, in, 5*time.Second)
	require.NotEmpty(t, nodes)
	last := nodes[len(nodes)-1]
	assert.Equal(t, NodeBold, last.Type)
	assert.Equal(t, strings.Repeat("<b>", 50), nodes[0].textValue())
}

func TestParse_TextNodesAreNotShared(t *testing.T) {
	// consumeChar は ASCII の 1 文字ノードを共有する。出力に出るテキストノードは
	// mergeText が作り直すので、呼び出し側が書き換えても他の Parse に波及しない
	a := Parse("a")
	require.Len(t, a, 1)
	a[0].Props["text"] = "changed"
	b := Parse("a")
	require.Len(t, b, 1)
	assert.Equal(t, "a", b[0].textValue())
	assert.NotSame(t, asciiText['a'], b[0])
}

func TestMergeText_DoesNotMutateInput(t *testing.T) {
	x, y := Text("x"), Text("y")
	out := mergeText([]*Node{x, y})
	require.Len(t, out, 1)
	assert.Equal(t, "xy", out[0].textValue())
	assert.Equal(t, "x", x.textValue())
	assert.Equal(t, "y", y.textValue())
}

// fill repeats unit after prefix up to about n bytes.
func fill(prefix, unit string, n int) string {
	return prefix + strings.Repeat(unit, (n-len(prefix))/len(unit))
}

// quoteChain returns an unclosed unit followed by depth lines, each quoted one
// level deeper than the previous one and ending with the same unit.
func quoteChain(depth int, unit string) string {
	var b strings.Builder
	b.WriteString(unit + "\n")
	for k := 1; k <= depth; k++ {
		b.WriteString(strings.Repeat(">", k) + " " + unit + "\n")
	}
	return b.String()
}

func TestParse_LocalSizedPathologicalInputsStayWithinBudget(t *testing.T) {
	// ローカルの本文上限 (3000 文字) に収まる入力は、どれだけ病的でも仕事量と
	// メモ表の上限に届かず、旧実装と同じく最後まで構文として読む。届くと以降が
	// テキストになり出力が変わるので、メモ化や区切りの索引が効いていない経路は
	// ここで落ちる (時間では見ない。-race 下では桁で遅くなる)
	cases := map[string]string{
		"unclosed bold":           strings.Repeat("<b>", 1000),
		"unclosed link label":     strings.Repeat("[<b>", 750),
		"quote chain bold":        quoteChain(20, "<b>"),
		"quote chain italic":      quoteChain(20, "<i>"),
		"quote chain fn":          quoteChain(20, "$[x "),
		"unclosed plain":          fill("", "<plain>", 3000),
		"unclosed math block":     fill("", "\\[", 3000),
		"unclosed fn arg value":   fill("", "$[x.k=v", 3000),
		"unclosed inline math":    fill("", "<b>\\(", 3000),
		"unclosed link url":       fill(strings.Repeat("<b>", 20), "[a](", 3000),
		"short quotes":            fill(strings.Repeat("<b>", 7), ":```js\n\n> ", 3000),
		"quote lines under limit": fill(strings.Repeat("<b>", 20), "\n> ", 3000),
		"quote lines with bold":   fill("", "<b>\n> ", 3000),
		"deep mixed":              fill(strings.Repeat("<b>", 18), "~~$[x.a=b *", 3000),
		"mixed":                   fill("", "<b>**~~$[x [<small><i>\\[`<plain>\\(", 3000),
	}
	for name, in := range cases {
		for _, simple := range []bool{false, true} {
			s := newState(in, simple)
			mergeText(s.parseNodes(false))
			assert.False(t, s.budget.exhausted(), "%s (simple=%v): used %d of %d", name, simple, s.budget.used, s.budget.limit)
			assert.Less(t, s.budget.memUsed, memoByteLimit/2, "%s (simple=%v)", name, simple)
		}
	}
}

func TestParse_QuoteChainKeepsNestedQuotes(t *testing.T) {
	// 入れ子の quote の各段に閉じない <b> がある形。quote の中身の表を深さごとに
	// 作り直していた実装は 11 段で上限に届き、quote の中身を丸ごとテキストにした
	nodes := parseWithin(t, quoteChain(20, "<b>"), 5*time.Second)
	require.Len(t, nodes, 2)
	quotes := 0
	for n := nodes[1]; ; n = n.Children[1] {
		require.Equal(t, NodeQuote, n.Type)
		quotes++
		require.NotEmpty(t, n.Children)
		if len(n.Children) < 2 {
			assert.Equal(t, "<b>", n.Children[0].textValue())
			break
		}
		assert.Equal(t, "<b>\n", n.Children[0].textValue())
	}
	assert.Equal(t, 20, quotes)
}

// memo と区切りの索引を入れる前の実装と同じ出力であることを、キーの各軸が
// 結果を変える入力で固定する。どれも旧実装の出力と突き合わせて確かめた値。
func TestParse_MemoKeyGolden(t *testing.T) {
	text := func(s string) *Node { return Text(s) }
	cases := []struct {
		name string
		in   string
		want []*Node
	}{
		{
			// link のラベルの中では link を試さない。inLink を表の軸から外すと、
			// ラベルの外で読んだ結果 (link) をラベルの中で使い回して出力が変わる
			name: "inLink axis",
			in:   "b~~$[x [a](https://e.com)",
			want: []*Node{
				text("b~~$"),
				{Type: NodeLink, Props: map[string]any{"url": "https://e.com", "silent": false}, Children: []*Node{text("x [a")}},
			},
		},
		{
			// 深さの上限では入れ子を読まない。深さを表の軸から外すと、浅い位置で
			// 読んだ結果を上限の深さで使い回して出力が変わる
			name: "depth axis center",
			in:   "<center>" + strings.Repeat("<b>", 19) + "<i>x</i></center>",
			want: []*Node{{Type: NodeCenter, Children: []*Node{
				text(strings.Repeat("<b>", 19)),
				withChildren(NodeItalic, []*Node{text("x")}),
			}}},
		},
		{
			name: "depth axis strike",
			in:   "<b>" + strings.Repeat("<s>", 20) + "**a**</b>",
			want: []*Node{{Type: NodeBold, Children: []*Node{
				text(strings.Repeat("<s>", 20)),
				withChildren(NodeBold, []*Node{text("a")}),
			}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseWithin(t, tc.in, 5*time.Second))
		})
	}
}
