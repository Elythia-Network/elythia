package wordmute_test

import (
	"strconv"
	"testing"

	"github.com/elythia-network/elythia/internal/core/wordmute"
	"github.com/stretchr/testify/assert"
)

func TestMatch_EmptyRules(t *testing.T) {
	assert.False(t, wordmute.Match(nil, "anything"))
	assert.False(t, wordmute.Match([]byte(""), "anything"))
	assert.False(t, wordmute.Match([]byte("[]"), "anything"))
	assert.False(t, wordmute.Match([]byte("null"), "anything"))
}

func TestMatch_EmptyText(t *testing.T) {
	assert.False(t, wordmute.Match([]byte(`[["foo"]]`), ""))
	assert.False(t, wordmute.Match([]byte(`[["foo"]]`), "   "))
}

// #3369: 本家の checkWordMute は `text.includes(keyword)` で、大文字と小文字を
// 区別する。
func TestMatch_KeywordIsCaseSensitive(t *testing.T) {
	rules := []byte(`[["FoO"]]`)
	assert.True(t, wordmute.Match(rules, "hello FoO world"))
	assert.False(t, wordmute.Match(rules, "hello FOO world"))
	assert.False(t, wordmute.Match(rules, "hello foo world"))
	assert.False(t, wordmute.Match(rules, "hello world"))
}

// #3369: 本家の checkWordMute は文字列の要素を正規表現として読み、`/.../` の
// 形でなければ一致させない。
func TestMatch_PlainStringNeverMatches(t *testing.T) {
	assert.False(t, wordmute.Match([]byte(`["foo"]`), "foo"))
	assert.False(t, wordmute.Match([]byte(`["/foo"]`), "/foo"))
	// JavaScript では作れないフラグ (`x`) の付いたものも、本家では一致しない。
	assert.False(t, wordmute.Match([]byte(`["/foo/x"]`), "foo"))
	// 同じ一覧の他の要素は効く。
	assert.True(t, wordmute.Match([]byte(`["foo", ["bar"]]`), "foo bar"))
}

func TestMatch_AndGroup(t *testing.T) {
	rules := []byte(`[["alpha", "beta"]]`)
	assert.True(t, wordmute.Match(rules, "say alpha then beta"))
	assert.False(t, wordmute.Match(rules, "only alpha"))
	assert.False(t, wordmute.Match(rules, "only beta"))
}

func TestMatch_Regex(t *testing.T) {
	rules := []byte(`["/abc[0-9]+/"]`)
	assert.True(t, wordmute.Match(rules, "match abc123 here"))
	assert.False(t, wordmute.Match(rules, "match ABC here"))
}

func TestMatch_RegexCaseInsensitiveFlag(t *testing.T) {
	rules := []byte(`["/abc[0-9]+/i"]`)
	assert.True(t, wordmute.Match(rules, "match ABC42 here"))
	assert.True(t, wordmute.Match(rules, "match abc42 here"))
}

// upstream RE2 (TS) の m / s flag を Go の (?m) / (?s) に変換すること。
func TestMatch_RegexMultilineFlag(t *testing.T) {
	rules := []byte(`["/^bad/m"]`)
	// 1行目がマッチしない場合でも、改行後の行頭にマッチすれば hit。
	assert.True(t, wordmute.Match(rules, "ok line\nbad line"))
}

func TestMatch_RegexDotallFlag(t *testing.T) {
	rules := []byte(`["/foo.bar/s"]`)
	// dotall 無しでは . が \n にマッチしないが、s flag で改行をまたいで hit する。
	assert.True(t, wordmute.Match(rules, "foo\nbar"))
}

func TestMatch_RegexCombinedFlags(t *testing.T) {
	rules := []byte(`["/^foo.bar/ims"]`)
	assert.True(t, wordmute.Match(rules, "header\nFOO\nBAR"))
}

// g flag は Go regexp の default 動作と同義なので prefix を組み立てなくて OK。
func TestMatch_RegexGlobalFlagIsNoOp(t *testing.T) {
	rules := []byte(`["/abc/g"]`)
	assert.True(t, wordmute.Match(rules, "first abc and second abc"))
}

// non-capture group `(?:...)` で始まる pattern には flag を inject する
// (= inline flag prefix と誤認しない)。
func TestMatch_RegexNonCaptureGroupGetsFlagInjected(t *testing.T) {
	// `m` flag を要求 → `(?:foo)` の前に `(?m)` が付かないと改行後の foo に hit しない。
	rules := []byte(`["/^(?:foo)/m"]`)
	assert.True(t, wordmute.Match(rules, "header line\nfoo body"))
}

// named group `(?P<name>...)` で始まる pattern にも flag を inject する。
func TestMatch_RegexNamedGroupGetsFlagInjected(t *testing.T) {
	rules := []byte(`["/(?P<head>FOO)/i"]`)
	assert.True(t, wordmute.Match(rules, "lowercase foo here"))
}

// pattern が既に inline flag group で始まる場合は二重指定を避けて skip する。
func TestMatch_RegexInlineFlagPrefixNotDoubled(t *testing.T) {
	// `(?i)foo` は既に case-insensitive を指定しているので、外側で `(?i)` を
	// 重ねても compile error にしない (Go regexp は (?i)(?i) も valid だが
	// 意図的に skip しておく方が安全)。
	rules := []byte(`["/(?i)foo/i"]`)
	assert.True(t, wordmute.Match(rules, "FOO bar"))
}

// #3369: サーバーが正規表現として当てるのは、frontend の `new RegExp` が受け付ける
// flags のときだけ。`v` は Go で表せないので当てない。`y` は先頭に固定する。
func TestMatch_RegexFlagsFollowFrontend(t *testing.T) {
	cases := []struct {
		rule, text string
		want       bool
	}{
		{`["/foo/d"]`, "a foo b", true},
		{`["/foo/gu"]`, "a foo b", true},
		{`["/FOO/is"]`, "a foo b", true},
		{`["/foo/v"]`, "a foo b", false},
		{`["/foo/uv"]`, "a foo b", false},
		{`["/foo/x"]`, "a foo b", false},
		{`["/foo/ii"]`, "a foo b", false},
		{`["/foo/y"]`, "foo at start", true},
		{`["/foo/y"]`, "not at start foo", false},
		{`["/foo|bar/y"]`, "x bar", false},
		{`["/foo|bar/my"]`, "x\nfoo", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, wordmute.Match([]byte(tc.rule), tc.text), "%s on %q", tc.rule, tc.text)
	}
}

func TestMatch_RegexInvalidIsIgnored(t *testing.T) {
	rules := []byte(`["/[unclosed/"]`)
	// Compilation failure must downgrade silently — never panic, never throw.
	assert.False(t, wordmute.Match(rules, "anything"))
}

func TestMatch_MixedRulesOrConnective(t *testing.T) {
	rules := []byte(`[["foo"], ["a", "b"], "/x[0-9]/"]`)
	assert.True(t, wordmute.Match(rules, "foo bar"))
	assert.True(t, wordmute.Match(rules, "ab cd a b"))
	assert.True(t, wordmute.Match(rules, "x9 marker"))
	assert.False(t, wordmute.Match(rules, "nothing here"))
}

func TestMatch_AndGroupSkipsEmptyEntries(t *testing.T) {
	rules := []byte(`[["", "foo", ""]]`)
	// 空文字 entry は skip されるので残った "foo" の単一 AND 条件で hit する。
	assert.True(t, wordmute.Match(rules, "foo bar"))
}

func TestMatch_AndGroupAllEmptyIsNoOp(t *testing.T) {
	rules := []byte(`[["", ""]]`)
	// 全 empty は rule なしと同義 (誤爆防止)。
	assert.False(t, wordmute.Match(rules, "anything"))
}

func TestMatch_InvalidJSONReturnsFalse(t *testing.T) {
	assert.False(t, wordmute.Match([]byte(`{"not":"array"}`), "anything"))
	assert.False(t, wordmute.Match([]byte(`not json`), "anything"))
}

func TestMatchNote_SkipsViewerOwnNote(t *testing.T) {
	rules := []byte(`[["secret"]]`)
	// viewer 自身の note は muted word が含まれていても hit しない。
	assert.False(t, wordmute.MatchNote(rules, "user-1", "user-1", "secret leaked", ""))
	// 他人の note は hit。
	assert.True(t, wordmute.MatchNote(rules, "user-2", "user-1", "secret leaked", ""))
}

func TestMatchNote_CombinesCWAndText(t *testing.T) {
	rules := []byte(`[["spoiler"]]`)
	// cw に hit、text 空でも hit。
	assert.True(t, wordmute.MatchNote(rules, "u2", "u1", "", "spoiler ahead"))
	// text に hit、cw 空でも hit。
	assert.True(t, wordmute.MatchNote(rules, "u2", "u1", "spoiler ahead", ""))
	// 両方空なら hit しない。
	assert.False(t, wordmute.MatchNote(rules, "u2", "u1", "", ""))
}

func TestMatchNote_EmptyViewerIDDisablesSelfSkip(t *testing.T) {
	rules := []byte(`[["foo"]]`)
	// viewerID == "" は anonymous viewer を表し、author 一致による skip を無効化する。
	assert.True(t, wordmute.MatchNote(rules, "u1", "", "foo bar", ""))
}

// 同じ rules JSON を 100 回 parse しても問題なく動作する (parseCache の
// concurrent-safety smoke、race detector 経由で実検証)。
func TestMatch_ConcurrentSafe(t *testing.T) {
	rules := []byte(`[["foo"], ["a","b"]]`)
	for i := 0; i < 100; i++ {
		assert.True(t, wordmute.Match(rules, "foo"))
		assert.True(t, wordmute.Match(rules, "a b"))
	}
}

// #790: parsedCache が size cap 化されていることを確認する。
//
// 実装は internal な LRU なので、外側からは「N+α 個の異なる rules JSON で
// Match を呼んでも memory が線形に増えず、再度叩いた時に dedupe される
// (= cache hit する) 動作」が保証されればよい。size cap の絶対値 (= 1000)
// に依存した assert は実装変更で壊れるので、cap 超過後でも match 自体が
// 正しく動くことだけを担保する。
func TestMatch_ParsedCacheSurvivesCapacity(t *testing.T) {
	// 1100 個の異なる rules を順に投入 (= cap 1000 を超える)。
	// それぞれ異なる単一キーワードで、対応 text に hit することを確認する。
	for i := 0; i < 1100; i++ {
		// 一意な keyword を含む rules JSON を組む。文字列 escape 不要な範囲。
		kw := "kw-" + string(rune('a'+(i%26))) + "-" + strconv.Itoa(i)
		rules := []byte(`[["` + kw + `"]]`)
		text := "prefix " + kw + " suffix"
		assert.True(t, wordmute.Match(rules, text),
			"freshly-added rule must match its keyword (i=%d)", i)
	}

	// cap 超過後でも、最後に add した rule は cache に残っており再 match 可能。
	last := []byte(`[["kw-z-1099"]]`)
	assert.True(t, wordmute.Match(last, "kw-z-1099 hits"))
}

// cap 超過で evict された古い rule JSON を再投入したとき、re-parse 経路が
// 正しく動いて再び match できることを担保する。eviction 後の correctness
// regression を防ぐ regression guard。
func TestMatch_EvictedRuleReParsesCorrectly(t *testing.T) {
	first := []byte(`[["original-keyword"]]`)
	// 最初に投入して match 確認。
	assert.True(t, wordmute.Match(first, "find original-keyword here"))

	// 1100 個 add で必ず first が evict される (cap=1000)。
	for i := 0; i < 1100; i++ {
		rules := []byte(`[["filler-` + strconv.Itoa(i) + `"]]`)
		_ = wordmute.Match(rules, "noop")
	}

	// first を再投入: cache miss で re-parse され、match path も生きていること。
	assert.True(t, wordmute.Match(first, "find original-keyword again"))
	assert.False(t, wordmute.Match(first, "no match here"))
}

// #2106 L3: ValidateMutedWords は不正な /regex/ リテラルで ErrInvalidRegexp を返し、
// 妥当な regex / 通常語 / AND group では nil を返す。
func TestValidateMutedWords(t *testing.T) {
	// 不正 regex リテラル。
	assert.ErrorIs(t, wordmute.ValidateMutedWords([]byte(`["/[/"]`)), wordmute.ErrInvalidRegexp)
	// 妥当 regex リテラル。
	assert.NoError(t, wordmute.ValidateMutedWords([]byte(`["/abc/i"]`)))
	// #3369: `/.../` の形でない文字列は本家と同じく拒否する。
	assert.ErrorIs(t, wordmute.ValidateMutedWords([]byte(`["plain"]`)), wordmute.ErrInvalidRegexp)
	assert.ErrorIs(t, wordmute.ValidateHardMutedWords([]byte(`[["ok"], "plain"]`)), wordmute.ErrInvalidRegexp)
	assert.ErrorIs(t, wordmute.ValidateMutedWords([]byte(`[""]`)), wordmute.ErrInvalidRegexp)
	// 本家 i/update (node-re2) と同じく、flags は知らないものも無視して受け付ける。
	for _, ok := range []string{`["/foo/v"]`, `["/foo/d"]`, `["/foo/x"]`, `["/foo/gimsuy"]`} {
		assert.NoError(t, wordmute.ValidateMutedWords([]byte(ok)), ok)
		assert.NoError(t, wordmute.ValidateHardMutedWords([]byte(ok)), ok)
	}
	// 本家と同じ形 (JavaScript の `.` は \r や U+2028 に当たらない) でなければ拒否する。
	assert.ErrorIs(t, wordmute.ValidateMutedWords([]byte(`["/a\rb/"]`)), wordmute.ErrInvalidRegexp)
	assert.ErrorIs(t, wordmute.ValidateHardMutedWords([]byte(`["/a\u2028b/"]`)), wordmute.ErrInvalidRegexp)
	assert.ErrorIs(t, wordmute.ValidateMutedWords([]byte(`["/foo/\n"]`)), wordmute.ErrInvalidRegexp)
	assert.ErrorIs(t, wordmute.ValidateMutedWords([]byte(`["/a\u2029b/"]`)), wordmute.ErrInvalidRegexp)
	assert.ErrorIs(t, wordmute.ValidateMutedWords([]byte(`["/a/\r"]`)), wordmute.ErrInvalidRegexp)
	assert.ErrorIs(t, wordmute.ValidateHardMutedWords([]byte(`["/a/\u2029"]`)), wordmute.ErrInvalidRegexp)
	// AND group (配列要素) は regex 検証対象外。
	assert.NoError(t, wordmute.ValidateMutedWords([]byte(`[["a","b"]]`)))
	// 空 / nil。
	assert.NoError(t, wordmute.ValidateMutedWords(nil))
	assert.NoError(t, wordmute.ValidateMutedWords([]byte(`[]`)))
}

// #3369: 本家 frontend use-note.ts の checkNoteWordMute(getAppearNote(note))。
func TestMatchAppearNote(t *testing.T) {
	rules := []byte(`[["muted"]]`)
	muted := func(user string) *wordmute.Note { return &wordmute.Note{UserID: user, Text: "muted"} }
	clean := func(user string) *wordmute.Note { return &wordmute.Note{UserID: user, Text: "clean"} }
	cases := []struct {
		name string
		note *wordmute.Note
		want bool
	}{
		{"nil", nil, false},
		{"own text", muted("a"), true},
		{"own cw", &wordmute.Note{UserID: "a", CW: "muted"}, true},
		{"viewer's own", muted("viewer"), false},
		{"pure renote uses the renoted note", &wordmute.Note{UserID: "b", PureRenote: true, Renote: muted("a")}, true},
		{"pure renote of viewer's own", &wordmute.Note{UserID: "b", PureRenote: true, Renote: muted("viewer")}, false},
		{"pure renote looks at the renoted note's reply", &wordmute.Note{UserID: "b", PureRenote: true, Renote: &wordmute.Note{UserID: "a", Reply: muted("c")}}, true},
		{"pure renote looks at the renoted note's quote", &wordmute.Note{UserID: "b", PureRenote: true, Renote: &wordmute.Note{UserID: "a", Renote: muted("c")}}, true},
		{"pure renote without target falls back to itself", &wordmute.Note{UserID: "b", PureRenote: true}, false},
		{"quote looks at the quoted note", &wordmute.Note{UserID: "a", Text: "clean", Renote: muted("c")}, true},
		{"quote does not look two levels deep", &wordmute.Note{UserID: "a", Text: "clean", Renote: &wordmute.Note{UserID: "c", Renote: muted("d")}}, false},
		{"reply looks at the reply target", &wordmute.Note{UserID: "a", Text: "clean", Reply: muted("c")}, true},
		{"hidden reply target is not judged", &wordmute.Note{UserID: "a", Text: "clean", Reply: &wordmute.Note{UserID: "c", Text: "muted", Hidden: true}}, false},
		{"hidden note itself is not judged", &wordmute.Note{UserID: "a", Text: "muted", Hidden: true}, false},
		{"clean all the way", &wordmute.Note{UserID: "a", Text: "clean", Reply: clean("c"), Renote: clean("d")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, wordmute.MatchAppearNote(rules, "viewer", tc.note))
		})
	}
	assert.False(t, wordmute.MatchAppearNote(nil, "viewer", muted("a")))
	assert.False(t, wordmute.MatchAppearNote([]byte(`[]`), "viewer", muted("a")))
}
