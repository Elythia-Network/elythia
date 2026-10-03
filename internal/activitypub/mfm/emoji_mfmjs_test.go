package mfm

import (
	"encoding/json"
	"os"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParse_UnicodeEmojiMatchesMfmJs fixes Unicode emoji parsing to mfm-js
// 0.26.0 (#3324) for every entry of the emoji-data list that mfm-js depends on:
// alone, doubled, at the start of a search line, without U+FE0F, and with
// U+FE0E after the first code point. testdata/emoji_mfmjs.json was produced by
// running mfm-js (testdata/emoji_mfmjs.mjs).
//
// 以前の mk-go は先頭の文字の範囲と続けてよい文字で判定する近似で、一覧の 49 件で
// 食い違い、続けて並んだ絵文字を 1 つにつないでいた。
func TestParse_UnicodeEmojiMatchesMfmJs(t *testing.T) {
	b, err := os.ReadFile("testdata/emoji_mfmjs.json")
	require.NoError(t, err)
	var data struct {
		MfmJs     string      `json:"mfmjs"`
		EmojiData string      `json:"emojiData"`
		Cases     [][2]string `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(b, &data))
	// 正規表現を作り直したら期待値も作り直す (emoji-data か mfm-js の版がずれたら落とす)
	require.Equal(t, emojiDataVersion, data.EmojiData, "regenerate testdata/emoji_mfmjs.json with testdata/emoji_mfmjs.mjs")
	require.Equal(t, emojiMfmJsVersion, data.MfmJs, "regenerate testdata/emoji_mfmjs.json with testdata/emoji_mfmjs.mjs")
	// 一覧は 1,915 件 (17.0.0)。読み込みが空振りして緑にならないよう下限を置く
	require.Greater(t, len(data.Cases), 5000)
	failed := 0
	for _, c := range data.Cases {
		if got := serializeTree(Parse(c[0])); got != c[1] {
			failed++
			assert.Equal(t, c[1], got, "input %q (%U)", c[0], []rune(c[0]))
			if failed >= 20 {
				t.Fatal("too many mismatches")
			}
		}
	}
}

// TestParse_UnicodeEmojiEdgeCasesMatchMfmJs covers the inputs of #3324 and the
// parts of the regex the list does not reach. Expected trees were produced by
// running mfm-js's parse on each input.
func TestParse_UnicodeEmojiEdgeCasesMatchMfmJs(t *testing.T) {
	cases := []struct{ in, want string }{
		// 行の先頭の 1 文字で、その行が検索構文になるかが決まる
		{"\U0001F194 検索", "unicodeEmoji:\U0001F194|text: 検索"},
		{"\U0001F194 #tag 検索", "unicodeEmoji:\U0001F194|text: |hashtag:tag|text: 検索"},
		{"\u266a 検索", "search"},
		{"\u2192 search", "search"},
		{"\u2122 検索", "search"},
		{"\U0001F600 検索", "unicodeEmoji:\U0001F600|text: 検索"},
		{"> a\n\u266a 検索", "quote[text:a]|search"},
		// 一覧に無い記号は絵文字にしない
		{"\u266a", "text:\u266a"},
		{"\u2606", "text:\u2606"},
		{"\u2318", "text:\u2318"},
		{"\u2122", "text:\u2122"},
		{"\u2122\ufe0f", "unicodeEmoji:\u2122\ufe0f"},
		{"\u00a9", "text:\u00a9"},
		{"\u00a9\ufe0f", "unicodeEmoji:\u00a9\ufe0f"},
		{"\U0001FA00", "text:\U0001FA00"},
		// 並んだ絵文字は 1 つずつ
		{"\U0001F600\U0001F600", "unicodeEmoji:\U0001F600|unicodeEmoji:\U0001F600"},
		{"\U0001F600\U0001F44D", "unicodeEmoji:\U0001F600|unicodeEmoji:\U0001F44D"},
		{"\U0001F441\ufe0f\u200d\U0001F5E8\ufe0f", "unicodeEmoji:\U0001F441\ufe0f|text:\u200d|unicodeEmoji:\U0001F5E8\ufe0f"},
		// 否定の先読み: テキスト表示の異体字セレクタ U+FE0E が続くと絵文字にしない
		{"\u263a", "unicodeEmoji:\u263a"},
		{"\u263a\ufe0f", "unicodeEmoji:\u263a\ufe0f"},
		{"\u263a\ufe0e", "text:\u263a\ufe0e"},
		{"\u270c\U0001F3FB", "unicodeEmoji:\u270c\U0001F3FB"},
		{"\u270c\ufe0f\U0001F3FB", "unicodeEmoji:\u270c\ufe0f\U0001F3FB"},
		{"\u270c\ufe0e\U0001F3FB", "text:\u270c\ufe0e|unicodeEmoji:\U0001F3FB"},
		{"\u2764\ufe0e", "text:\u2764\ufe0e"},
		// U+FE0F だけを読むと、mfm-js は絵文字ではなく文字にする
		{"\ufe0f", "text:\ufe0f"},
		{"a\ufe0fb", "text:a\ufe0fb"},
		{"\U0001F600\ufe0f\ufe0f", "unicodeEmoji:\U0001F600|text:\ufe0f\ufe0f"},
		// 単独の継続文字は文字
		{"\u200d", "text:\u200d"},
		{"\U0001F3FB", "unicodeEmoji:\U0001F3FB"},
		{"\U000E007F", "text:\U000E007F"},
		// 旗 (タグ文字の並び) は途中で切れたら黒旗だけ
		{"\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F", "unicodeEmoji:\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F"},
		{"\U0001F3F4\U000E0067\U000E0062", "unicodeEmoji:\U0001F3F4|text:\U000E0067\U000E0062"},
		{"\U0001F1EF\U0001F1F5\U0001F1EF", "unicodeEmoji:\U0001F1EF\U0001F1F5|unicodeEmoji:\U0001F1EF"},
		// 末尾の ZWJ は文字に残す
		{"\U0001F468\u200d", "unicodeEmoji:\U0001F468|text:\u200d"},
		{"\U0001F468\u200d\U0001F469\u200d\U0001F467", "unicodeEmoji:\U0001F468\u200d\U0001F469\u200d\U0001F467"},
		// 書式の中でも同じ
		{"**\U0001F600\U0001F600**", "bold[unicodeEmoji:\U0001F600|unicodeEmoji:\U0001F600]"},
		{"<small>\u266a\U0001F194</small>", "small[text:\u266a|unicodeEmoji:\U0001F194]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, serializeTree(Parse(tc.in)))
		})
	}
}

// fullEmojiLen matches the unspecialized generated pattern, the reference the
// per-first-rune matchers must agree with.
func fullEmojiLen(s string) int {
	re := emojiFullHolds
	if runeAt(s, emojiLookaheadOffset) == emojiLookaheadRune {
		re = emojiFullFails
	}
	loc := re.FindStringIndex(s)
	if loc == nil {
		return 0
	}
	return loc[1]
}

var (
	emojiFullHolds = regexp.MustCompile(emojiPatternLookaheadHolds)
	emojiFullFails = regexp.MustCompile(emojiPatternLookaheadFails)
)

// TestUnicodeEmojiLen_AgreesWithTheFullPattern checks that the shortcuts of
// unicodeEmojiLen (the first-rune table, the per-first-rune pattern and the
// second-rune check) read exactly what the whole generated pattern reads.
//
// 先頭の文字ごとに絞った正規表現と 2 文字目の判定は、生成した正規表現の結果を
// 変えないための最適化なので、絞らない正規表現と突き合わせる。先頭は絵文字に
// なり得る全ての文字と、それ以外から間引いた文字。
func TestUnicodeEmojiLen_AgreesWithTheFullPattern(t *testing.T) {
	var firsts []rune
	for c := rune(0); c <= 0x1fbff; c++ {
		if unicode.Is(emojiFirstRunes, c) || c%97 == 0 {
			firsts = append(firsts, c)
		}
	}
	tails := []string{
		"", "a", " ", "\ufe0f", "\ufe0e", "\u200d", "\u20e3", "\ufe0f\u20e3", "\U0001F3FB", "\U0001F1F5", "\U0001F600",
		"\u200d\U0001F5E8\ufe0f", "\U0001F3FB\u200d\u2642\ufe0f", "\ufe0e\U0001F3FB", "\ufe0f\u200d\u2642\ufe0f\u200d\u27a1\ufe0f",
		"\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F",
	}
	matched := 0
	for _, f := range firsts {
		for _, tail := range tails {
			in := string(f) + tail
			want := fullEmojiLen(in)
			if got := unicodeEmojiLen(in); got != want {
				t.Fatalf("unicodeEmojiLen(%q (%U)) = %d, want %d", in, []rune(in), got, want)
			}
			if want > utf8.RuneLen(f) {
				matched++
			}
		}
	}
	// 2 文字以上の一致が 1 つも無いまま緑にならないよう、件数の下限を見る
	require.Greater(t, matched, 1000)
}

// TestEmojiFirstRunesMatchesThePattern checks the generated first-rune table
// against the first code points computed from the generated pattern itself.
//
// 表は生成ツールが JavaScript の正規表現から、こちらは Go の正規表現の構文木から
// 求める。別々に求めた 2 つが一致すれば、表が先頭になり得る文字を落としていない
// (落とすとその絵文字を読まなくなる)。
func TestEmojiFirstRunesMatchesThePattern(t *testing.T) {
	fromPattern, empty := firstRunesOf([]*syntax.Regexp{emojiSyntaxLookaheadHolds})
	require.False(t, empty)
	var fromTable []rune
	for _, r := range emojiFirstRunes.R16 {
		fromTable = append(fromTable, rune(r.Lo), rune(r.Hi))
	}
	for _, r := range emojiFirstRunes.R32 {
		fromTable = append(fromTable, rune(r.Lo), rune(r.Hi))
	}
	assert.Equal(t, mergeRunePairs(fromTable), mergeRunePairs(fromPattern))
	require.Greater(t, len(fromTable), 100)
}

func mergeRunePairs(pairs []rune) [][2]rune {
	var rs [][2]rune
	for i := 0; i+1 < len(pairs); i += 2 {
		rs = append(rs, [2]rune{pairs[i], pairs[i+1]})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i][0] < rs[j][0] })
	var out [][2]rune
	for _, r := range rs {
		if n := len(out); n > 0 && r[0] <= out[n-1][1]+1 {
			out[n-1][1] = max(out[n-1][1], r[1])
			continue
		}
		out = append(out, r)
	}
	return out
}

// TestTryUnicodeEmoji_ChargesTheWorkBudget checks that reading an emoji is
// counted as work like the other parsers.
func TestTryUnicodeEmoji_ChargesTheWorkBudget(t *testing.T) {
	in := "\U0001F468\u200d\U0001F469\u200d\U0001F467"
	s := newState(in, false)
	before := s.budget.used
	n := s.tryUnicodeEmoji()
	require.NotNil(t, n)
	assert.Equal(t, len(in), s.pos)
	assert.GreaterOrEqual(t, s.budget.used, before+len(in))
}

func TestUnicodeEmojiLen_DoesNotReadPastTheLongestMatch(t *testing.T) {
	// 最長の一致の後ろに何が続いても、読む長さは変わらない
	kiss := "\U0001F468\U0001F3FB\u200d\u2764\ufe0f\u200d\U0001F48B\u200d\U0001F468\U0001F3FF"
	require.LessOrEqual(t, len(kiss), emojiMaxBytes)
	for _, tail := range []string{"", strings.Repeat("\U0001F600", 100), strings.Repeat("\u200d", 100), "\xff\xfe"} {
		assert.Equal(t, len(kiss), unicodeEmojiLen(kiss+tail), "tail %q", tail)
	}
	assert.Equal(t, 0, unicodeEmojiLen(""))
	assert.Equal(t, 0, unicodeEmojiLen("\xff"))
	assert.Equal(t, utf8.RuneLen(0x1F600), unicodeEmojiLen("\U0001F600\xff"))
}

// TestEmojiMatcherFor_ConcurrentBuild builds matchers from many goroutines
// against an empty cache (run with -race).
func TestEmojiMatcherFor_ConcurrentBuild(t *testing.T) {
	saved := emojiMatchers.Load()
	emojiMatchers.Store(nil)
	t.Cleanup(func() { emojiMatchers.Store(saved) })

	inputs := []string{"\U0001F600", "\U0001F468\u200d\U0001F469", "1\u20e3", "\u263a\ufe0e", "\u00a9\ufe0f", "a"}
	want := make([]int, len(inputs))
	for i, in := range inputs {
		want[i] = fullEmojiLen(in)
	}
	done := make(chan []int)
	for range 8 {
		go func() {
			got := make([]int, len(inputs))
			for i, in := range inputs {
				got[i] = unicodeEmojiLen(in)
			}
			done <- got
		}()
	}
	for range 8 {
		assert.Equal(t, want, <-done)
	}
	// 同じ先頭の文字は 1 つの matcher を使い回す
	assert.Same(t, emojiMatcherFor(0x1F600, true), emojiMatcherFor(0x1F600, true))
}

func TestRuneAt(t *testing.T) {
	assert.Equal(t, 'a', runeAt("ab", 0))
	assert.Equal(t, 'b', runeAt("ab", 1))
	assert.Equal(t, rune(-1), runeAt("ab", 2))
	assert.Equal(t, rune(-1), runeAt("", 0))
}

func TestSpecializeFirst_RejectsUnknownConstructs(t *testing.T) {
	// 生成ツールが出さない形 (繰り返し・任意の 1 文字・最短一致) は、黙って違う判定に
	// せず落とす
	for _, p := range []string{`a*`, `a+?`, `.`, `a??`} {
		re := mustParseEmojiPattern(p)
		assert.Panics(t, func() { specializeFirst([]*syntax.Regexp{re}, 'a') }, p)
		if p != `a??` { // 先頭になり得る文字の集合は、最短一致でも変わらない
			assert.Panics(t, func() { firstRunesOf([]*syntax.Regexp{re}) }, p)
		}
	}
	assert.Panics(t, func() { stripFirst(mustParseEmojiPattern(`a*`)) })
	assert.Panics(t, func() { mustParseEmojiPattern(`(`) })
	assert.Nil(t, specializeFirst(nil, 'a'))
}

// TestSpecializeFirst_KeepsLeftmostFirstOrder checks the restriction to a
// first rune on small patterns with the constructs the emoji pattern can hold,
// including ones the current emoji-data does not use at the head of a branch.
func TestSpecializeFirst_KeepsLeftmostFirstOrder(t *testing.T) {
	patterns := []string{
		`a?ab|a`,
		`(?:a|ab)c?|b`,
		`(?:ab?)?a|b[ab]c`,
		`[a-c]x|a`,
		`(?:)ab|a`,
		`a[^\x00-\x{10FFFF}]|ab`,
		`(?:a|b)?(?:a|c)`,
	}
	inputs := []string{"a", "aa", "ab", "aab", "abc", "ac", "aac", "b", "ba", "bab", "bac", "c", "ca", "ax", "abx"}
	for _, p := range patterns {
		full := regexp.MustCompile(`^(?:` + p + `)`)
		for _, in := range inputs {
			first, _ := utf8.DecodeRuneInString(in)
			want := full.FindStringIndex(in)
			var got []int
			if spec := specializeFirst([]*syntax.Regexp{mustParseEmojiPattern(p)}, first); spec != nil {
				got = regexp.MustCompile(`^(?:` + spec.String() + `)`).FindStringIndex(in)
			}
			assert.Equal(t, want, got, "pattern %q input %q", p, in)
		}
	}
}
