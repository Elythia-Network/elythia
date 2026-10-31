package csscolor

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// parseFloatJS は JS の parseFloat をなぞる。
//
// **渡ってくるのは CSS unit だけではない** — `convertToPercentage` は
// `String(1e-9 * 100) + "%"` を返すので `"1.0000000000000001e-7%"` のような
// **regexp が通し得ない文字列**も届く (指数部を読む分岐がある理由)。ただし
// どちらの経路も必ず数字を含むので digit 無しにはならない。JS の意味は
// そのまま持たせておく (将来の呼び出しで暗黙に 0 として扱われないように)。
func TestParseFloatJS(t *testing.T) {
	assert.Equal(t, 50.0, parseFloatJS("50%"))
	assert.Equal(t, -60.0, parseFloatJS("-60"))
	assert.Equal(t, 0.5, parseFloatJS(".5"))
	assert.Equal(t, 7.0, parseFloatJS("+7"))
	assert.Equal(t, 1.5, parseFloatJS("1.5.5"), "2 つ目の小数点で止まる")
	assert.True(t, math.IsNaN(parseFloatJS("")))
	assert.True(t, math.IsNaN(parseFloatJS("abc")))
	assert.True(t, math.IsNaN(parseFloatJS("+")))
	assert.True(t, math.IsNaN(parseFloatJS(".")))
	// 桁溢れは Infinity (JS の parseFloat と同じ)。NaN にすると以降が全部
	// NaN になり丸めが未定義になる。
	assert.True(t, math.IsInf(parseFloatJS(strings.Repeat("9", 400)), 1))
	assert.True(t, math.IsInf(parseFloatJS("-"+strings.Repeat("9", 400)), -1))
}

// parseIntJS は JS の `parseInt(s, 10)`。**`e` で止まる**のが要点で、
// `parseInt(1e-7)` が 1 になるのはこのため。
func TestParseIntJS(t *testing.T) {
	assert.Equal(t, 1.0, parseIntJS("1e-7"))
	assert.Equal(t, 0.0, parseIntJS("0.00001"))
	assert.Equal(t, 650.0, parseIntJS("650.25"))
	assert.Equal(t, -12.0, parseIntJS("-12abc"))
	assert.Equal(t, 7.0, parseIntJS("  +7  "))
	assert.True(t, math.IsNaN(parseIntJS("abc")))
	assert.True(t, math.IsNaN(parseIntJS("")))
	assert.True(t, math.IsNaN(parseIntJS("-")))
}

// jsNumberToString は ECMAScript の Number::toString。**Go の `g` 書式では
// 代用できない** — 指数表記へ切り替わる境界が違う (Go は 1e-4 付近、JS は 1e-6)。
// 期待値は node の `String(n)` で測ったもの。
func TestJSNumberToString(t *testing.T) {
	cases := map[float64]string{
		0:        "0",
		-0.5:     "-0.5",
		1:        "1",
		100:      "100",
		0.000001: "0.000001",
		1e-7:     "1e-7",
		1e-20:    "1e-20",
		1e20:     "100000000000000000000",
		1e21:     "1e+21",
		1.5e21:   "1.5e+21",
		// **`0.1 + 0.2` と書かない。** Go は untyped constant として畳むので
		// ちょうど 0.3 になり、JS の浮動小数演算の結果と違う値を測ってしまう。
		0.30000000000000004:   "0.30000000000000004",
		9.999999999999999e-06: "0.000009999999999999999",
	}
	for in, want := range cases {
		assert.Equal(t, want, jsNumberToString(in), "jsNumberToString(%v)", in)
	}
	assert.Equal(t, "Infinity", jsNumberToString(math.Inf(1)))
	assert.Equal(t, "-Infinity", jsNumberToString(math.Inf(-1)))
	assert.Equal(t, "NaN", jsNumberToString(math.NaN()))
}

// bound01 の早期 return と clamp。ここが変わると `rgb(255,0,0)` が黒に落ちる。
func TestBound01(t *testing.T) {
	assert.Equal(t, 1.0, bound01("255", 255))
	assert.Equal(t, 0.0, bound01("0", 255))
	assert.Equal(t, 1.0, bound01("300", 255), "clamp してから max 一致で 1")
	assert.Equal(t, 0.0, bound01("-5", 255), "負は 0 に clamp する")
	assert.Equal(t, 1.0, bound01("100%", 255))
	assert.Equal(t, 1.0, bound01("1.0", 255), "小数点を含む 1 は 100% 扱い")
	assert.InDelta(t, 1.0/255.0, bound01("1", 255), 1e-12)
}
