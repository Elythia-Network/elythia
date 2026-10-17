package wordmute

import "strings"

// This file decides whether the server may evaluate a hard-mute regex with Go's
// regexp at all. The frontend (upstream checkWordMute) evaluates the same
// literal with JavaScript's `new RegExp(pattern, flags)`; where the two engines
// can disagree, the server leaves the rule to the frontend.
//
// **JavaScript と Go で test() の結果が変わりうる構文を含む規則は、サーバーでは
// 判定しない (#3369)。** 以前は Go の regexp としてそのまま当てていたので、Go だけ
// が読む構文 (`(?i)`、`\p{Han}` を u なしで、`[[:alpha:]]`、`\Q…\E`、`\A` など)
// を、本家の画面では隠れないのにサーバーだけが当てていた。書き換えて同じ意味に
// するのではなく、字句を見て判定から外す。外した規則は frontend が本家と同じく
// 判定する。
//
// 差が本文の特定の文字でしか出ない構文 (絵文字と `.`、`\r` と `.` など) は、規則
// ごと外すと普通の規則まで外れるので、その文字が本文にあるときだけ判定しない
// (textGuard)。

// textGuard is a set of text features under which a rule is not evaluated.
type textGuard uint8

const (
	// guardSupplementary: the text has a supplementary character (an emoji).
	// Without the u flag JavaScript reads it as two UTF-16 code units, so `.`,
	// negated classes and `\W` `\D` `\S` match half of it; Go reads one rune.
	guardSupplementary textGuard = 1 << iota
	// guardLineTerminator: the text has `\r`, U+2028 or U+2029. JavaScript's
	// `.` does not match them and its m flag treats them as line breaks; Go
	// only knows `\n`.
	guardLineTerminator
	// guardUnicodeSpace: the text has a character that JavaScript's `\s`
	// matches and Go's (ASCII only) does not, such as U+3000 or `\v`.
	guardUnicodeSpace
	// guardFoldSpecial: the text has U+017F or U+212A. Go's (?i) matches them
	// with s and k; JavaScript without the u flag does not.
	guardFoldSpecial
)

// isJSTrimSpace reports whether JavaScript's String.prototype.trim removes r
// (WhiteSpace and LineTerminator; the same set migration 000133 uses).
func isJSTrimSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0D, r == 0x20, r == 0xA0, r == 0x1680,
		r >= 0x2000 && r <= 0x200A, r == 0x2028, r == 0x2029,
		r == 0x202F, r == 0x205F, r == 0x3000, r == 0xFEFF:
		return true
	}
	return false
}

// jsTrim removes leading and trailing characters like JavaScript's trim().
//
// **本文の前後は frontend (`text.trim()`) と同じ文字で除く (#3369)。** Go の
// strings.TrimSpace は U+0085 を除き U+FEFF を除かないので、先頭や末尾に固定する
// 規則 (`/^foo/`、`/foo$/`、`/foo/y`) が片方だけで当たっていた。
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSTrimSpace)
}

// textFeatures returns the guards that s triggers.
func textFeatures(s string) textGuard {
	var g textGuard
	for _, r := range s {
		switch {
		case r < 0x80:
			switch r {
			case '\r':
				g |= guardLineTerminator
			case '\v':
				g |= guardUnicodeSpace
			}
		case r >= 0x10000:
			g |= guardSupplementary
		case r == 0x2028 || r == 0x2029:
			g |= guardLineTerminator | guardUnicodeSpace
		case r == 0xA0 || r == 0x1680 || (r >= 0x2000 && r <= 0x200A) ||
			r == 0x202F || r == 0x205F || r == 0x3000 || r == 0xFEFF:
			g |= guardUnicodeSpace
		case r == 0x17F || r == 0x212A:
			g |= guardFoldSpecial
		}
	}
	return g
}

// jsRisk scans a /pattern/flags literal (flags already accepted by
// evaluableFlags). ok is false when the pattern uses syntax whose meaning may
// differ between JavaScript's RegExp and Go's regexp; otherwise guard lists the
// text features under which the rule must not be evaluated.
//
// 検出は広めにしている (判定しない側に倒すのが目的)。ASCII の語、`^` `$`、
// `|`、`(…)` `(?:…)`、量指定子、`[a-z]` のような文字クラス、`\d` `\w` `\b`、
// 記号のエスケープは、そのまま判定する。
func jsRisk(pattern, flags string) (guard textGuard, ok bool) {
	unicodeMode := false
	ignoreCase, multiline, dotAll := false, false, false
	for _, f := range flags {
		switch f {
		case 'u':
			unicodeMode = true
		case 'i':
			ignoreCase = true
		case 'm':
			multiline = true
		case 's':
			dotAll = true
		}
	}
	if ignoreCase && !unicodeMode {
		// u なしの i では、Go だけが s と U+017F、k と U+212A を同じ文字として比べる
		// (\w や [a-z] も同じ)。差はこの 2 文字が本文にあるときだけに出る。
		guard |= guardFoldSpecial
	}
	if multiline {
		// m の `^` `$` が `\r` などを行の区切りにするかどうか。
		guard |= guardLineTerminator
	}
	src := []rune(pattern)
	const (
		prevNone = iota // start, `(`, `|`: nothing to repeat
		prevAssertion
		prevAtom
		prevQuantified
	)
	prev := prevNone
	inClass := false
	classFirst := 0
	depth := 0
	var names map[string]bool
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inClass {
			switch {
			case c == ']' && i == classFirst:
				// `[]` と `[^]` は JavaScript では空のクラスと全文字、Go では `]` を含むクラス。
				return 0, false
			case c == ']':
				inClass = false
				prev = prevAtom
			case c == '[':
				// `[[:alpha:]]` は Go では POSIX のクラス、JavaScript では文字の並び。
				return 0, false
			case c == '\\':
				g, n, isClassEscape, ok := jsEscape(src, i, true, unicodeMode, ignoreCase)
				if !ok {
					return 0, false
				}
				guard |= g
				i += n
				// `[\d-z]` は u なしの JavaScript では `-` を文字として読む。
				if isClassEscape && i+2 < len(src) && src[i+1] == '-' && src[i+2] != ']' {
					return 0, false
				}
			default:
				if !literalOK(c, unicodeMode, ignoreCase, true) {
					return 0, false
				}
			}
			continue
		}
		switch c {
		case '\\':
			if i+1 < len(src) && (src[i+1] == 'b' || src[i+1] == 'B') {
				// u と i の両方があると、JavaScript の \b は U+017F と U+212A も単語の
				// 文字にする。Go の \b は ASCII だけ。
				if unicodeMode && ignoreCase {
					return 0, false
				}
				i++
				prev = prevAssertion
				continue
			}
			g, n, _, ok := jsEscape(src, i, false, unicodeMode, ignoreCase)
			if !ok {
				return 0, false
			}
			guard |= g
			i += n
			prev = prevAtom
		case '(':
			if i+1 < len(src) && src[i+1] == '?' {
				n, ok := groupPrefix(src, i)
				if !ok {
					return 0, false
				}
				if n > 2 {
					// 同じ名前のグループ (別の選択肢でなら) を読めるかは、ブラウザの版で違う
					// (node 22 は例外にする)。Go は受け付ける。
					name := string(src[i+3 : i+n])
					if names[name] {
						return 0, false
					}
					if names == nil {
						names = map[string]bool{}
					}
					names[name] = true
				}
				i += n
			}
			depth++
			prev = prevNone
		case ')':
			// 対応の無い `)` は JavaScript では構文エラー。Go でも普通はエラーだが、
			// y の `\A(?:…)` で包むと釣り合ってしまう (`)(?:^` が当たっていた)。
			if depth == 0 {
				return 0, false
			}
			depth--
			prev = prevAtom
		case '|':
			prev = prevNone
		case '^', '$':
			prev = prevAssertion
		case '.':
			if !unicodeMode {
				guard |= guardSupplementary
			}
			if !dotAll {
				guard |= guardLineTerminator
			}
			prev = prevAtom
		case '[':
			inClass = true
			classFirst = i + 1
			if i+1 < len(src) && src[i+1] == '^' {
				classFirst = i + 2
				i++
				if !unicodeMode {
					guard |= guardSupplementary
				}
			}
		case '*', '+', '?':
			// 繰り返す対象が無いもの (`^*`、`(*a)`) は JavaScript では構文エラー。
			if prev != prevAtom {
				return 0, false
			}
			if i+1 < len(src) && src[i+1] == '?' {
				i++
			}
			prev = prevQuantified
		case '{':
			n, ok := bracedQuantifier(src, i)
			if !ok || prev != prevAtom {
				// 量指定子にならない `{` の読み方は、u の有無と Go で違う。
				return 0, false
			}
			i += n
			if i+1 < len(src) && src[i+1] == '?' {
				i++
			}
			prev = prevQuantified
		case '}', ']':
			if unicodeMode {
				return 0, false // u 付きでは構文エラー
			}
			prev = prevAtom
		default:
			if !literalOK(c, unicodeMode, ignoreCase, false) {
				return 0, false
			}
			if !unicodeMode && c > 0xFFFF && i+1 < len(src) && strings.ContainsRune("*+?{", src[i+1]) {
				// u なしでは、絵文字の後ろの量指定子は下位のサロゲートだけに掛かる。
				return 0, false
			}
			prev = prevAtom
		}
	}
	if inClass || depth != 0 {
		return 0, false
	}
	return guard, true
}

// literalOK reports whether a literal pattern character means the same in both
// engines.
func literalOK(c rune, unicodeMode, ignoreCase, inClass bool) bool {
	// ASCII の外の文字の大文字と小文字の対応は、JavaScript (u なしは toUpperCase、
	// u 付きは case folding) と Go の (?i) で一部違う (`ß` と `ẞ` など)。
	if ignoreCase && c >= 0x80 {
		return false
	}
	// u なしでは、文字クラスの中の絵文字は 2 つのサロゲートとして読まれる。
	if inClass && !unicodeMode && c > 0xFFFF {
		return false
	}
	return true
}

// jsEscape checks the escape starting at src[i] == '\\' (other than `\b` /
// `\B` outside a class). n is the number of runes consumed after the
// backslash; isClassEscape reports `\d` `\w` `\s` and their negations.
func jsEscape(src []rune, i int, inClass, unicodeMode, ignoreCase bool) (guard textGuard, n int, isClassEscape, ok bool) {
	if i+1 >= len(src) {
		return 0, 0, false, false
	}
	c := src[i+1]
	switch c {
	case 'd':
		return 0, 1, true, true
	case 'w':
		return 0, 1, true, true
	case 'D', 'W':
		if !unicodeMode {
			guard |= guardSupplementary
		}
		return guard, 1, true, true
	case 's', 'S':
		// JavaScript の \s は Unicode の空白 (U+3000 など) と \v も含み、Go は ASCII の 5 文字だけ。
		guard |= guardUnicodeSpace
		if c == 'S' && !unicodeMode {
			guard |= guardSupplementary
		}
		return guard, 1, true, true
	case 't', 'n', 'r', 'f', 'v':
		return 0, 1, false, true
	case 'x':
		// `\x41` は両方で同じ。`\x{41}` は Go だけが読む。
		if i+3 >= len(src) || !isHex(src[i+2]) || !isHex(src[i+3]) {
			return 0, 0, false, false
		}
		if ignoreCase && (hexVal(src[i+2]) >= 8) {
			return 0, 0, false, false // ASCII の外の文字に i が掛かる
		}
		return 0, 3, false, true
	case '-':
		if unicodeMode && !inClass {
			return 0, 0, false, false // u 付きでは構文エラー
		}
		return 0, 1, false, true
	}
	// 記号のエスケープは両方で文字そのもの。それ以外 (英字・数字・その他の文字) は、
	// JavaScript では u の有無で文字そのもの (`\A` は `A`) か構文エラー、Go では別の
	// 意味 (`\A` は先頭、`\pL` は文字の性質、`\Q` は引用、`\1` は Go で不可) になる。
	if c < 0x80 && strings.ContainsRune(`^$\.*+?()[]{}|/`, c) {
		return 0, 1, false, true
	}
	return 0, 0, false, false
}

// groupPrefix checks `(?` at src[i]: only `(?:` and `(?<name>` are read the
// same way. n is the number of runes consumed after `(`.
func groupPrefix(src []rune, i int) (int, bool) {
	if i+2 >= len(src) {
		return 0, false
	}
	switch src[i+2] {
	case ':':
		return 2, true
	case '<':
		// 名前付きのグループ。`(?<=` `(?<!` (後読み) は Go に無い。名前は両方が読む
		// ASCII の識別子だけにする (Go は数字で始まる名前も受け付ける)。
		j := i + 3
		for ; j < len(src) && src[j] != '>'; j++ {
			c := src[j]
			if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (j > i+3 && c >= '0' && c <= '9')) {
				return 0, false
			}
		}
		if j == i+3 || j >= len(src) {
			return 0, false
		}
		return j - i, true
	}
	// `(?=` `(?!` (先読み、Go に無い)、`(?i)` `(?P<n>` (Go だけ)、`(?i:` (新しい
	// ブラウザだけ) など。
	return 0, false
}

// bracedQuantifier checks `{n}`, `{n,}` or `{n,m}` at src[i] and returns the
// number of runes after `{` up to and including `}`.
func bracedQuantifier(src []rune, i int) (int, bool) {
	j := i + 1
	// **2 桁以上で 0 から始まる数は読まない。** JavaScript は `{01}` を `{1}` と読み、
	// Go は量指定子にせず `{01}` を文字として読む。
	digits := func() bool {
		start := j
		for j < len(src) && src[j] >= '0' && src[j] <= '9' {
			j++
		}
		return j > start && (j-start == 1 || src[start] != '0')
	}
	if !digits() {
		return 0, false
	}
	if j < len(src) && src[j] == ',' {
		j++
		if j < len(src) && src[j] != '}' && !digits() {
			return 0, false
		}
	}
	if j >= len(src) || src[j] != '}' {
		return 0, false
	}
	return j - i, true
}

func isHex(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c rune) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}
