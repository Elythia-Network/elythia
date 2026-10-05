package csscolor_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/misc/csscolor"
)

// 実物の tinycolor2 が返した値と 1 件ずつ突き合わせる。
//
// vectors_test.go は node で tinycolor2 を実際に呼んで生成したもの。
// 「upstream に揃えた」を推論で書かないための実測 (#2726)。
func TestNormalize_MatchesTinycolor(t *testing.T) {
	for _, tc := range tinycolorVectors {
		hex, ok := csscolor.Normalize(tc.in)
		assert.Equal(t, tc.ok, ok, "isValid(%q)", tc.in)
		assert.Equal(t, tc.hex, hex, "toHexString(%q)", tc.in)
	}
}

// 色名テーブルは 149 件すべてが引けること。1 件でも欠けると
// `instance.themeColor` がその色名で null になる。
func TestNormalize_AllNames(t *testing.T) {
	for _, name := range csscolor.Names() {
		hex, ok := csscolor.Normalize(name)
		assert.True(t, ok, "name %q must parse", name)
		assert.Len(t, hex, 7, "name %q → %q", name, hex)
	}
	assert.Len(t, csscolor.Names(), 149)
}

// **桁数は縛られていない。** CSS_INTEGER は `\d+` なので 400 桁の数値が実際に
// 届きうる。JS の parseFloat は Infinity を返し、tinycolor はそれを clamp して
// 色にする。期待値はすべて実物の tinycolor2 で測ったもの (#2726)。
//
// 生成テーブル (vectors_test.go) には入れていない — 1 行が 400 文字を超えて
// 読めなくなるため。
func TestNormalize_HugeNumbers(t *testing.T) {
	big := strings.Repeat("9", 400)
	cases := []struct {
		in   string
		ok   bool
		want string
	}{
		{"rgb(" + big + ",0,0)", true, "#ff0000"},
		{"rgb(-" + big + ",0,0)", true, "#000000"},
		{"hsl(" + big + ",100%,50%)", true, "#ff0000"},
		// parseInt(255*255)/100 = 650.25 → 650.25 % 255 = 140.25 → 0x8c。
		{"rgb(" + big + "%,0,0)", true, "#8c0000"},
		{"hsv(" + big + "," + big + "," + big + ")", true, "#ff0000"},
		{"rgba(" + big + "," + big + "," + big + "," + big + ")", true, "#ffffff"},
		// 指数表記は **CSS unit の正規表現が `e` を通さない**ので matcher に
		// 一致しない (`parseFloatJS` 自体は `1e5` を 100000 と読む)。
		{"rgb(1e5,0,0)", false, ""},
	}
	for _, tc := range cases {
		hex, ok := csscolor.Normalize(tc.in)
		assert.Equal(t, tc.ok, ok, "isValid(%.20s...)", tc.in)
		assert.Equal(t, tc.want, hex, "toHexString(%.20s...)", tc.in)
	}
}
