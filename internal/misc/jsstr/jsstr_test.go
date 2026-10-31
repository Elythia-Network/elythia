package jsstr

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JavaScript の trim() が除く文字は、node 22 で BMP の全域を回して数えた 25 文字。
// IsSpace がこの集合とちょうど同じであること (集合の外は除かない)。
func TestIsSpaceMatchesJavaScript(t *testing.T) {
	want := map[rune]bool{}
	for _, r := range []rune{
		0x0009, 0x000A, 0x000B, 0x000C, 0x000D, 0x0020, 0x00A0, 0x1680,
		0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007,
		0x2008, 0x2009, 0x200A, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF,
	} {
		want[r] = true
	}
	require.Len(t, want, 25)
	for r := rune(0); r <= 0x10FFFF; r++ {
		if want[r] != IsSpace(r) {
			t.Errorf("IsSpace(U+%04X) = %v", r, IsSpace(r))
		}
	}
	for r := range want {
		assert.Equal(t, "a", Trim(string(r)+"a"+string(r)), "U+%04X", r)
	}
}

// SpaceClass (正規表現の側) と IsSpace (関数の側) は同じ集合でなければならない。
// 片方だけ直すのが実際に起きた失敗なので、全 rune で突き合わせる (#2726)。
func TestSpaceClassMatchesIsSpace(t *testing.T) {
	re := regexp.MustCompile(`^[` + SpaceClass + `]$`)
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // サロゲートは文字列にできない
		}
		if got := re.MatchString(string(r)); got != IsSpace(r) {
			t.Errorf("SpaceClass matches U+%04X = %v, IsSpace = %v", r, got, IsSpace(r))
		}
	}
}

func TestLen(t *testing.T) {
	assert.Equal(t, 0, Len(""))
	assert.Equal(t, 3, Len("abc"))
	assert.Equal(t, 2, Len("あい"))
	// サロゲートペアになる文字は 2 単位
	assert.Equal(t, 4, Len("\U0001F600\U0001F600"))
	assert.Equal(t, 3, Len("a\U00020BB7"))
}

func TestIsSpace(t *testing.T) {
	for _, r := range []rune{'\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680, 0x2000, 0x2005, 0x200A, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF} {
		assert.True(t, IsSpace(r), "%U", r)
	}
	// U+0085 は Go の unicode.IsSpace では空白だが、JavaScript の \s ではない。
	for _, r := range []rune{0x0085, 0x1FFF, 0x200B, 0x180E, 'a', 0} {
		assert.False(t, IsSpace(r), "%U", r)
	}
}

func TestTrim(t *testing.T) {
	assert.Equal(t, "a b", Trim("\ufeff\u3000a b\u2028 "))
	assert.Equal(t, "\u0085", Trim(" \u0085 "))
	assert.Equal(t, "", Trim("\ufeff \t"))
}

func TestHasNonSpace(t *testing.T) {
	assert.True(t, HasNonSpace("\u0085"))
	assert.True(t, HasNonSpace(" a "))
	assert.False(t, HasNonSpace("\ufeff\u3000\t"))
	assert.False(t, HasNonSpace(""))
}
