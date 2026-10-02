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
		// 時間ではなく仕事量とメモリ量で見る。
		// 閉じない装飾を読んだ範囲ごと文字にするようになって (#3301)、これらの
		// 入力は 1 バイトあたり数回の仕事で読み終わるので、上限は入力長より小さくする
		in := strings.Repeat(u, (16<<10)/len(u))
		s := newState(in, false)
		s.budget.limit = len(in) / 4
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

func TestParse_UnclosedPrefixSwallowsLaterSyntax(t *testing.T) {
	// mfm-js と同じく、閉じない開きタグは末尾までを文字にするので、後ろの構文も
	// 文字になる (#3301)。開きタグより前の構文は残る
	in := strings.Repeat("<b>", 50) + "**bold**"
	nodes := parseWithin(t, in, 5*time.Second)
	require.Len(t, nodes, 1)
	assert.Equal(t, in, nodes[0].textValue())

	nodes = parseWithin(t, "**bold**"+strings.Repeat("<b>", 50), 5*time.Second)
	require.Len(t, nodes, 2)
	assert.Equal(t, NodeBold, nodes[0].Type)
	assert.Equal(t, strings.Repeat("<b>", 50), nodes[1].textValue())
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
		"unclosed bold":         strings.Repeat("<b>", 1000),
		"unclosed link label":   strings.Repeat("[<b>", 750),
		"quote chain bold":      quoteChain(20, "<b>"),
		"quote chain italic":    quoteChain(20, "<i>"),
		"quote chain fn":        quoteChain(20, "$[x "),
		"unclosed plain":        fill("", "<plain>", 3000),
		"unclosed math block":   fill("", "\\[", 3000),
		"unclosed fn arg value": fill("", "$[x.k=v", 3000),
		"unclosed inline math":  fill("", "<b>\\(", 3000),
		"unclosed link url":     fill(strings.Repeat("<b>", 20), "[a](", 3000),
		"link urls":             fill(strings.Repeat("<b>", 20), "[a](https://x", 3000),
		// 多数の `[` が同じ長い飛び先に届く形。飛び先を位置ごとに覚えないと、同じ
		// URL を `[` の数だけ読み直す (#3300)。
		"labels sharing a url":    strings.Repeat("[", 1000) + "a](https://" + strings.Repeat("x", 1978),
		"unclosed url parens":     fill("", "https://a(", 3000),
		"unclosed url brackets":   fill("<b>", "https://a[(", 3000),
		"unclosed url alt":        fill("", "[a](<https://x\n", 3000),
		"unclosed hashtag parens": fill("", "#a(", 3000),
		"unclosed hashtag groups": fill(strings.Repeat("<b>", 20), "#a(「[（", 3000),
		"short quotes":            fill(strings.Repeat("<b>", 7), ":```js\n\n> ", 3000),
		"quote lines under limit": fill(strings.Repeat("<b>", 20), "\n> ", 3000),
		"quote lines with bold":   fill("", "<b>\n> ", 3000),
		"deep mixed":              fill(strings.Repeat("<b>", 18), "~~$[x.a=b *", 3000),
		"mixed":                   fill("", "<b>**~~$[x [<small><i>\\[`<plain>\\(", 3000),
		// center は行の先頭ごとに試す。開きの直後から閉じを探す区間を毎回辿り直すと
		// 行数の 2 乗になる (#3328)
		"unclosed center lines": fill("", "<center>a\n", 3000),
		"unclosed center crlf":  fill("", "\r\n<center>\r\n**", 3000),
		"center lines at limit": fill("", strings.Repeat(">", 19)+" <center>a\n", 3000),
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
	// 入れ子の quote の各段に閉じない構文がある形。quote の中身の表を深さごとに
	// 作り直していた実装は 11 段で上限に届き、quote の中身を丸ごとテキストにした。
	// 閉じない <b> は mfm-js と同じく末尾までを文字にするので (#3301)、各段に
	// 残るのは改行で止まる ~~ の形
	nodes := parseWithin(t, quoteChain(20, "<b>"), 5*time.Second)
	require.Len(t, nodes, 1)
	assert.Equal(t, quoteChain(20, "<b>"), nodes[0].textValue())

	nodes = parseWithin(t, quoteChain(20, "~~a"), 5*time.Second)
	require.Len(t, nodes, 2)
	quotes := 0
	for n := nodes[1]; ; n = n.Children[1] {
		require.Equal(t, NodeQuote, n.Type)
		quotes++
		require.NotEmpty(t, n.Children)
		assert.Equal(t, "~~a", strings.TrimSuffix(n.Children[0].textValue(), "\n"))
		if len(n.Children) < 2 {
			break
		}
	}
	assert.Equal(t, 20, quotes)
}

// memo の表のキーの各軸 (inLink と深さ) が結果を変える入力で、出力を固定する。
// どれも mfm-js 0.26.0 の出力と一致し、軸を表のキーから外す変異で出力が変わる
// ことを確かめた値。閉じない装飾を読み直さなくなったので (#3301)、読み直しは
// 失敗すると戻る構文 (リンク・center) の中でだけ起きる。どちらも最上位の center が
// 閉じずに失敗し、その中身 (1 段深い) で読んだ結果が表に残る形
// (最上位かどうかの軸は TestParse_QuoteMemoSeparatesTopLevelCenter)。
func TestParse_MemoKeyGolden(t *testing.T) {
	bolds := func(s string) string { return strings.Repeat("<b>", 18) + s + strings.Repeat("</b>", 18) }
	boldTree := func(s string) string { return strings.Repeat("bold[", 18) + s + strings.Repeat("]", 18) }
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// center の中では 19 段目のリンクの飛び先の括弧が深さの上限に届いて失敗し、
			// ラベルの位置をラベルの外として読む。最上位では 18 段目のリンクが成功し、
			// 同じ位置を同じ深さのラベルの中で読む。inLink を表の軸から外すと、
			// ラベルの外で読んだ結果 (ハッシュタグ) をラベルの中で使い回して出力が変わる
			name: "inLink axis",
			in:   "<center>" + bolds("[#t](https://e.x/((a)))"),
			want: "text:<center>|" + boldTree("link:https://e.x/((a))[text:#t]"),
		},
		{
			// ハッシュタグの括弧は深さの上限までしか読まない。深さを表の軸から外すと、
			// 失敗した center の中 (1 段深い) で読んだ結果を使い回して出力が変わる
			name: "depth axis",
			in:   "<center>" + bolds("#a((b))"),
			want: "text:<center>|" + boldTree("hashtag:a((b))"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(parseWithin(t, tc.in, 5*time.Second)))
		})
	}
}

// `<https://...>` の飛び先は閉じの `>` か空白まで読む。改行では止まらないので、
// 1 文字ずつ読むと `[a](<https://x` を改行を挟んで並べただけで、どの `](` からも
// 末尾まで読んで入力長の 2 乗になる (#3300。1MB で 30 秒台)。区切りの位置の索引から
// 引いていることを確かめる。時間で見ると CI の負荷で揺れるので、索引が作られて
// 使われたことを見る。
func TestParse_UnclosedURLAltUsesStopIndex(t *testing.T) {
	s := newState(strings.Repeat("[a](<https://x\n", 2000), false)
	mergeText(s.parseNodes(false))
	assert.Equal(t, int8(1), s.memo.stopsState[stopURLAltEnd], "区切りの索引を作って引いていない")
	assert.False(t, s.budget.exhausted())
}

// 区切りの索引を作れないとき (メモの上限に届いた、不正な UTF-8) は 1 文字ずつ
// 読むが、読んだ分を仕事量に数えて上限で打ち切る。数えないと、上の形で入力長の
// 2 乗になっても上限に届かない (#3300)。Parse は入口で UTF-8 を正すので、索引を
// 作れない状態は state を直接作って再現する。
func TestParse_UnclosedURLAltFallbackCountsWork(t *testing.T) {
	s := newState(strings.Repeat("[a](<https://x\n", 14000), false)
	s.memo.stopsState[stopURLAltEnd] = -1 // 索引を作れない状態
	mergeText(s.parseNodes(false))
	assert.True(t, s.budget.exhausted(), "1 文字ずつ読んだ分を仕事量に数えていない (used %d of %d)", s.budget.used, s.budget.limit)
}
