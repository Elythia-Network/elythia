// Package jsstr reproduces JavaScript's handling of whitespace in strings,
// where upstream Misskey's behavior depends on it.
//
// Go の unicode.IsSpace と strings.TrimSpace は U+0085 (NEL) を空白として扱い、
// U+FEFF (BOM) を扱わない。JavaScript の \s と String.prototype.trim は逆なので、
// そのまま使うと本家と判定や保存値が分かれる (#3884)。
package jsstr

import (
	"strings"
	"unicode/utf16"
)

// SpaceClass is the set of IsSpace written as the inside of a Go (RE2)
// character class, for building regular expressions that match JavaScript's
// \s.
//
// RE2 の \s は ASCII の空白だけで \v も含まない。記号の意味の取り違えを避けるため、
// 文字は全部 \x{…} で書く。IsSpace と同じ集合であることはテストが全 rune で確かめる。
const SpaceClass = `\x{9}-\x{d}\x{20}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// IsSpace reports whether r matches JavaScript's \s and is removed by
// String.prototype.trim: the WhiteSpace and LineTerminator code points of
// ECMAScript (TAB, VT, FF, SP, NBSP, ZWNBSP, the Zs category, LF, CR, LS,
// PS).
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return 0x2000 <= r && r <= 0x200A
}

// Trim trims s like JavaScript's String.prototype.trim.
func Trim(s string) string { return strings.TrimFunc(s, IsSpace) }

// HasNonSpace reports whether s matches JavaScript's /[^\s]/.
func HasNonSpace(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return !IsSpace(r) }) >= 0
}

// Len returns the length of s in UTF-16 code units, like JavaScript's
// String.prototype.length.
func Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
