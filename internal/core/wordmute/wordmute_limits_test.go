package wordmute

import (
	"encoding/json"
	"hash/maphash"
	"regexp/syntax"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hardMutedWords の正規表現の大きさの上限 (1 件と合計) を検査すること。
//
// 上限の値はリテラルで書く (定数を参照すると、緩める変異と一緒に期待値が動く)。
func TestValidateHardMutedWords_Limits(t *testing.T) {
	ok := json.RawMessage(`["/(?i)spam|scam/", "keyword", ["a", "b"]]`)
	assert.NoError(t, ValidateHardMutedWords(ok))

	// 幅の指定 (`.{0,200}`) を含む普通の大きさは通る (405 命令)。
	assert.NoError(t, ValidateHardMutedWords(mustJSON(t, []string{"/.{0,200}特定語/"})))

	// 1 件で 512 命令を超える (803 命令)。
	big := mustJSON(t, []string{"/" + strings.Repeat(".?", 400) + "b/"})
	assert.ErrorIs(t, ValidateHardMutedWords(big), ErrRegexpTooComplex)
	assert.NoError(t, ValidateMutedWords(big), "mutedWords には命令数の上限を掛けない")

	// 1 件ずつは上限内でも、合計で 1024 命令を超える。
	mid := "/" + strings.Repeat(".?", 200) + "b/" // 403 命令
	three := mustJSON(t, []string{mid, mid, mid})
	assert.ErrorIs(t, ValidateHardMutedWords(three), ErrRegexpTooComplex)
	two := mustJSON(t, []string{mid, mid})
	assert.NoError(t, ValidateHardMutedWords(two))

	// 4096 バイトを超えるパターンは、コンパイルする前に両方とも拒否する。
	huge := mustJSON(t, []string{"/" + strings.Repeat("a", 4097) + "/"})
	assert.ErrorIs(t, ValidateMutedWords(huge), ErrInvalidRegexp)
	assert.ErrorIs(t, ValidateHardMutedWords(huge), ErrInvalidRegexp)
	edge := mustJSON(t, []string{"/" + strings.Repeat("a", 200) + "/"})
	assert.NoError(t, ValidateMutedWords(edge))
}

// 上限を超える正規表現 (上限より前に保存された設定) は評価しないこと。
func TestMatch_SkipsRegexOverLimits(t *testing.T) {
	text := "aaaab"
	// 1 件で上限を超えるので、一致するパターンでもミュートしない。
	big := mustJSON(t, []string{"/" + strings.Repeat("a?", 400) + "b/"}) // 803 命令
	assert.False(t, Match(big, text), "上限を超える正規表現を評価した")
	// 小さければ同じ意味のパターンでミュートする (対照)。
	small := mustJSON(t, []string{"/" + strings.Repeat("a?", 4) + "b/"})
	assert.True(t, Match(small, text))

	// 合計の上限: 先頭の 2 件 (405 命令ずつ) の後ろの 3 件目 (303 命令、
	// 一致する) は合計が 1024 を超えるので落とす。
	mid := "/" + strings.Repeat("x?", 200) + "zzz/"
	third := "/" + strings.Repeat("a?", 150) + "b/"
	over := mustJSON(t, []string{mid, mid, third})
	assert.False(t, Match(over, text), "合計の上限を超えた後ろの正規表現を評価した")
	// 対照: 前が 1 件なら合計は上限内で、3 件目が効く。
	under := mustJSON(t, []string{mid, third})
	assert.True(t, Match(under, text))
	// 正規表現でない語は合計に数えない。
	words := mustJSON(t, []string{mid, mid, "aaaab"})
	assert.True(t, Match(words, text))
}

// 正規表現を含む規則の判定結果を覚えること (同じノートに当て直さない)。
func TestMatch_MemoizesRegexResults(t *testing.T) {
	rules := mustJSON(t, []string{"/memo-[0-9]+/"})
	text := "memo-123 " + strings.Repeat("z", 100)
	assert.True(t, Match(rules, text))
	rs := parse(rules)
	require.NotNil(t, rs)
	got, ok := matchMemo.Get(matchKey{ruleSet: rs.id, text: maphash.String(matchSeed, text)})
	require.True(t, ok, "結果を覚えていない")
	assert.True(t, got)
	// 覚えた結果を使うこと: 覚えた値を書き換えると、それが返る。
	matchMemo.Add(matchKey{ruleSet: rs.id, text: maphash.String(matchSeed, text)}, false)
	assert.False(t, Match(rules, text), "覚えた結果を使っていない")
}

// 見積もりで明らかに大きいパターンは、コンパイルせずに断ること。見積もりは
// 実際の命令数を下回らないこと (上限内のものを見積もりで断らない)。
func TestEstimateInsts(t *testing.T) {
	for _, p := range []string{
		"(?i)spam|scam", ".{0,200}特定語", strings.Repeat(".?", 200) + "b",
		"(foo|bar)+baz", "[a-z]{3,}x", "^(?:a|b|c)*$",
	} {
		re, err := syntax.Parse(p, syntax.Perl)
		require.NoError(t, err)
		prog, err := syntax.Compile(re.Simplify())
		require.NoError(t, err)
		assert.GreaterOrEqual(t, estimateInsts(re, 1<<30), len(prog.Inst), p)
	}
	// 入れ子の繰り返しは見積もりの途中で打ち切り、上限を超えたと返す。
	re, err := syntax.Parse("(?:(?:a{1000}){1000}){1000}", syntax.Perl)
	if err == nil {
		assert.Greater(t, estimateInsts(re, 4096), 4096)
	}
	re, err = syntax.Parse("(?:"+strings.Repeat("a", 900)+"){1000}", syntax.Perl)
	require.NoError(t, err)
	assert.Greater(t, estimateInsts(re, 4096), 4096)
	_, _, _, err = literalProgram("/(?:"+strings.Repeat("a", 900)+"){1000}/", true)
	assert.ErrorIs(t, err, ErrRegexpTooComplex)

	// `{0}` の中身は命令を生まないので、見積もりにも数えない (実際は数十命令の
	// パターンを見積もりで断らない)。
	_, n, _, err := literalProgram("/(?:(?:(?:(?:(a)){9,11}){4,12}){0,0}){8,15}x/", true)
	require.NoError(t, err)
	assert.LessOrEqual(t, n, maxRegexInsts)
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
