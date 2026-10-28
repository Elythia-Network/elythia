package jsstr

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
