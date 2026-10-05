package colfit_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/colfit"
)

func TestFits(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want bool
	}{
		{"empty fits", "", 4, true},
		{"exactly max fits", "abcd", 4, true},
		{"over max does not fit", "abcde", 4, false},
		// PostgreSQL の varchar はコードポイントで数えるので、byte 実装なら落ちる。
		{"multibyte counted in runes", strings.Repeat("あ", 4), 4, true},
		{"multibyte over max", strings.Repeat("あ", 5), 4, false},
		{"NUL never fits", "a\x00b", 8, false},
		{"NUL alone never fits", "\x00", 8, false},
		// 不正な UTF-8 は幅に関わらず入らない。
		{"invalid UTF-8 never fits", "a\x80b", 8, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, colfit.Fits(tc.s, tc.max))
		})
	}
}

func TestToStorable(t *testing.T) {
	assert.Equal(t, "ab", colfit.ToStorable("a\x00b"))
	assert.Equal(t, "", colfit.ToStorable("\x00\x00"))
	// NUL も不正な UTF-8 も無い値は同じ文字列がそのまま返る (無駄な再構築を
	// しない)。
	assert.Equal(t, "plain", colfit.ToStorable("plain"))
	// **不正な UTF-8 は U+FFFD へ矯正する。** 落とす (削る) のではないのは、
	// `Text` が rune で数えて切るため。
	assert.Equal(t, "a\uFFFDb", colfit.ToStorable("a\x80b"))
	assert.Equal(t, "a\uFFFDb", colfit.ToStorable("a\xed\xa0\x80b"), "孤立サロゲート")
	assert.Equal(t, "\uFFFD", colfit.ToStorable("\xff"))
	// NUL と不正な UTF-8 が同時に来ても両方処理する。
	assert.Equal(t, "a\uFFFDb", colfit.ToStorable("a\x00\x80b"))
	// **矯正した結果が必ず列に入ること**まで見る。片方しか処理しない実装だと
	// ここで落ちる。
	for _, in := range []string{"a\x00b", "a\x80b", "a\x00\x80b", "\xed\xa0\x80"} {
		assert.True(t, colfit.Storable(colfit.ToStorable(in)), "矯正後も列に入らない: %q", in)
	}
}

func TestTruncateRunes(t *testing.T) {
	assert.Equal(t, "abc", colfit.TruncateRunes("abcdef", 3))
	assert.Equal(t, "abc", colfit.TruncateRunes("abc", 3))
	assert.Equal(t, strings.Repeat("あ", 3), colfit.TruncateRunes(strings.Repeat("あ", 5), 3))
	// max <= 0 は「切らない」。無制限の text 列 (note.text) 用。
	assert.Equal(t, "abcdef", colfit.TruncateRunes("abcdef", 0))
	assert.Equal(t, "abcdef", colfit.TruncateRunes("abcdef", -1))
}

func TestText(t *testing.T) {
	assert.Equal(t, "ab", colfit.Text("a\x00b", 8))
	// NUL を落としてから数えるので、NUL の分で切り詰まらない。
	assert.Equal(t, "abc", colfit.Text("a\x00bc", 3))
	// **合成済みの 1 rune で書く。** 結合文字 (a + U+0301) だと見た目 4 文字でも
	// 5 rune になり、「4 rune を 4 で切る」つもりのケースが「5 rune を 4 で切る」
	// ケースにすり替わる。列はコードポイントで数えるのでどちらも有効な入力だが、
	// テストの意図と一致させる。
	assert.Equal(t, "ábcd", colfit.Text("ábcd", 4))
	assert.Equal(t, "ábcd", colfit.Text("ábcde", 4))
	assert.Equal(t, "ab", colfit.Text("a\x00b", 0))
	assert.Equal(t, "", colfit.Text("\x00", 4))
	// 不正な UTF-8 は U+FFFD 1 rune として数える。byte で数えていると
	// `"a\x80b"` が 3 byte 扱いになり、切る位置がずれる。
	assert.Equal(t, "a\uFFFD", colfit.Text("a\x80b", 2))
	assert.Equal(t, "a\uFFFDb", colfit.Text("a\x80b", 8))
}

// Storable は長さを見ない。**幅を混ぜると、比較できるだけの値まで落とす。**
func TestStorable(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want bool
	}{
		{"素の文字列", "abc", true},
		{"空文字", "", true},
		{"長い文字列でも幅は見ない", strings.Repeat("x", 10000), true},
		{"非 ASCII", "こんにちは", true},
		{"NUL を含む", "a\x00b", false},
		{"NUL だけ", "\x00", false},
		{"末尾の NUL", "abc\x00", false},
		// **不正な UTF-8 も列に入らない** (SQLSTATE 22021)。以前は NUL しか
		// 見ておらず、クエリパラメータに `%80` を混ぜるだけで 500 を起こせた。
		{"孤立した継続バイト", "a\x80b", false},
		{"UTF-8 で符号化した孤立サロゲート", "a\xed\xa0\x80b", false},
		{"0xFF", "\xff", false},
		{"切れた multibyte", "\xe3\x81", false},
		// 正当な非 ASCII は落とさない。
		{"絵文字", "\U0001F600", true},
		{"結合文字", "a\u0301", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, colfit.Storable(tt.in))
		})
	}
}

// jsonb 列に入るかの判定。
//
// **判定するのは decode 前の生バイト列。** ガードの対象は `json.RawMessage` で、
// 列へ入るのはほどく前のバイトそのもの。1 稿目は「ほどいてから文字列を歩く」
// 実装で、**4 つある拒否形のうち 1 つしか見ていなかった** (敵対的レビューで
// 実測された)。
//
// 期待値は実 PostgreSQL で境界まで測ってある:
//
//	孤立サロゲートのエスケープ -> 22P02 invalid input syntax for type json
//	{"n":1e131071} -> OK    {"n":1e131072} -> 22003
//	{"n":1e-16383} -> OK    {"n":1e-16384} -> 22003
//	9 を 131072 個 -> OK    131073 個      -> 22003
//	小数 16383 桁  -> OK    16384 桁       -> 22003
//	生の 0x80 バイト           -> 22021
//	NUL のエスケープ           -> 22P05
func TestJSONStorable(t *testing.T) {
	esc := `\u0` + `000` // JSON 上の NUL エスケープ (6 文字)
	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		{"素のオブジェクト", `{"a":"b"}`, true},
		{"素の配列", `[1,2,"x"]`, true},
		{"空", ``, true},
		{"null", `null`, true},
		{"数値だけ", `42`, true},
		{"非 ASCII", `{"あ":"絵文字"}`, true},
		{"値に NUL", `{"a":"x` + esc + `y"}`, false},
		{"キーに NUL", `{"x` + esc + `y":"b"}`, false},
		{"配列の要素に NUL", `["ok","x` + esc + `y"]`, false},
		{"入れ子", `{"a":{"b":["x` + esc + `"]}}`, false},
		// **バックスラッシュ自体がエスケープされた形はただの 6 文字。**
		// 走査で `\\` を飛ばさないとここで誤検出する。
		{"エスケープされたバックスラッシュ", `{"a":"x\\` + `u0000y"}`, true},
		// 対になっていないサロゲート (22P02)。
		{"単独の上位サロゲート", `{"a":"\ud800"}`, false},
		{"単独の下位サロゲート", `{"a":"\udc00"}`, false},
		{"上位の後ろが別のエスケープ", `{"a":"\ud800\n"}`, false},
		{"上位の後ろが普通の文字", `{"a":"\ud800x"}`, false},
		// 正しい組は通す (絵文字はこの形で来る)。
		{"対になったサロゲート", `{"a":"😀"}`, true},
		// numeric の範囲 (22003)。
		{"指数が上限ちょうど", `{"n":1e131071}`, true},
		{"指数が上限超過", `{"n":1e131072}`, false},
		{"負の指数が上限ちょうど", `{"n":1e-16383}`, true},
		{"負の指数が上限超過", `{"n":1e-16384}`, false},
		{"int に入らない指数", `{"n":1e99999999999999999999}`, false},
		{"普通の小数", `{"n":1.5,"m":-0.25,"e":1e10}`, true},
		// 構文エラーは通す (呼び出し側の既存の扱いに任せる)。
		{"壊れた JSON", `{`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, colfit.JSONStorable([]byte(tt.raw)))
		})
	}

	// 桁数そのもので上限に当たる形 (指数を使わない)。
	assert.True(t, colfit.JSONStorable([]byte(`{"n":`+strings.Repeat("9", 131072)+`}`)))
	assert.False(t, colfit.JSONStorable([]byte(`{"n":`+strings.Repeat("9", 131073)+`}`)))
	assert.True(t, colfit.JSONStorable([]byte(`{"n":0.`+strings.Repeat("9", 16383)+`}`)))
	assert.False(t, colfit.JSONStorable([]byte(`{"n":0.`+strings.Repeat("9", 16384)+`}`)))

	// **生の不正 UTF-8 バイト列** (22021)。`json.RawMessage` はこれを
	// そのまま保持するので、decode 後の値からは見えない。
	assert.False(t, colfit.JSONStorable([]byte("{\"a\":\"x\x80y\"}")),
		"生の不正 UTF-8 を通している")
	// 生の NUL バイト。
	assert.False(t, colfit.JSONStorable([]byte("{\"a\":\"x\x00y\"}")))
}

// **`json.RawMessage` は decode しないので生の形が残る。** ここが
// `JSONStorable` を生バイト列に対する判定にしている理由。
func TestJSONStorable_RawMessageKeepsTheOriginalBytes(t *testing.T) {
	var req struct {
		V json.RawMessage `json:"v"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"v":"\ud800"}`), &req))
	assert.Equal(t, `"\ud800"`, string(req.V), "decoder が RawMessage を正規化している")
	assert.False(t, colfit.JSONStorable(req.V))

	// プレーンな string に decode する field では U+FFFD へ矯正される
	// (こちらは `Storable` の担当で、そもそも列に届かない)。
	var plain struct {
		V string `json:"v"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"v":"\ud800"}`), &plain))
	assert.Equal(t, "�", plain.V)
}

// **走査の端の条件を踏む。** `jsonEscapesStorable` / `parseHex4` は壊れた
// JSON の途中で打ち切る枝を多く持ち、通常の入力では通らない。ここが未実行の
// まま残ると、**打ち切りの条件を 1 つ緩めても緑のまま**になる。
func TestJSONStorable_TruncatedAndMalformedEscapes(t *testing.T) {
	bs := "\\"    // JSON 上のバックスラッシュ 1 文字
	u := bs + "u" // エスケープの開始
	hi := u + "d800"

	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		// バックスラッシュで終わる (次の 1 バイトが無い)。
		{"末尾がバックスラッシュ", `{"a":"x` + bs, true},
		// エスケープの開始だが 4 桁ぶんのバイトが足りない。
		{"エスケープの開始で終わる", `{"a":"` + u, true},
		{"16 進が 1 桁足りない", `{"a":"` + u + `004`, true},
		// 16 進でない文字。
		{"16 進でない", `{"a":"` + u + `zzzz"}`, true},
		{"大文字の 16 進", `{"a":"` + u + `00E9"}`, true},
		// 上位サロゲートの直後が切れている / 対になっていない。
		{"上位サロゲートで終わる", `{"a":"` + hi, false},
		{"上位の後ろがエスケープでない", `{"a":"` + hi + bs + `t"}`, false},
		{"下位サロゲートの 16 進が壊れている", `{"a":"` + hi + u + `zzzz"}`, false},
		// 文字列の外のバックスラッシュは無視する。
		{"文字列の外", `{` + bs + `}`, true},
		// 文字列が閉じたあとは生の文字列。
		{"閉じたあと", `{"a":"b"} ` + u + `0000`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, colfit.JSONStorable([]byte(tt.raw)))
		})
	}
}
