package paramschema

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/misc/jsstr"
)

// This file translates the `pattern` of a paramDef, which ajv evaluates as
// `new RegExp(pattern, "u")`, into an equivalent Go regexp.
//
// **字句を1つずつ読んで、意味の同じGoの構文に書き直す(#3673)。**そのままGoの
// regexpに渡すと、`\s`の範囲(JSはU+00A0、U+3000、U+FEFFなどを含み、Goは
// ASCIIの6文字だけ)と`.`の範囲(JSは\r、U+2028、U+2029にも当たらない)が
// ずれる。逆にGoだけが読む構文(`[[:alpha:]]`、`\pL`、`\Q…\E`、`(?i)`など)は、
// JSのuフラグでは別の意味か構文の誤りになる。そこで、書ける構文を限り、
// それ以外はcompileの誤りにする。本家に新しい構文が入ったら、テストが落ちて
// ここへ足すことになる。
//
// JSのuフラグの下では`\d`と`\w`はASCIIで、`\b`もASCIIの語の境界なので、
// Goと同じ。`^`と`$`はmフラグが無いので、入力の先頭と末尾にだけ当たる(Goの
// 既定と同じ)。
//
// 文字は全部`\x{…}`に直して出す。GoとJSで記号の意味が違っても、取り違えが
// 起きないようにするため。

// jsDot is JavaScript's `.` without the s flag: anything but a line terminator.
const jsDot = `[^\x{a}\x{d}\x{2028}\x{2029}]`

// compileJSRegexp compiles a JavaScript `u`-flag regular expression source
// into a Go regexp with the same test() result.
func compileJSRegexp(src string) (*regexp.Regexp, error) {
	g, err := translateJSRegexp(src)
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", src, err)
	}
	re, err := regexp.Compile(g)
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", src, err)
	}
	return re, nil
}

type jsRegexpParser struct {
	src []rune
	pos int
	out strings.Builder
	// atom is true right after something a quantifier may repeat.
	atom bool
}

func translateJSRegexp(src string) (string, error) {
	if !utf8.ValidString(src) {
		return "", fmt.Errorf("not valid UTF-8")
	}
	p := &jsRegexpParser{src: []rune(src)}
	depth := 0
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '\\':
			if err := p.escapeOutside(); err != nil {
				return "", err
			}
		case '[':
			if err := p.class(); err != nil {
				return "", err
			}
			p.atom = true
		case '(':
			if p.peekStr("(?:") {
				p.out.WriteString("(?:")
				p.pos += 3
			} else if p.peekStr("(?") {
				// 先読み・後読み・名前付きの組はGoに無いか、意味が違う。
				return "", fmt.Errorf("unsupported group at %d", p.pos)
			} else {
				p.out.WriteByte('(')
				p.pos++
			}
			p.atom = false
			depth++
		case ')':
			if depth == 0 {
				return "", fmt.Errorf("unbalanced ) at %d", p.pos)
			}
			depth--
			p.out.WriteByte(')')
			p.pos++
			p.atom = true
		case '^', '$', '|':
			p.out.WriteRune(c)
			p.pos++
			p.atom = false
		case '*', '+', '?':
			if err := p.quantifierTarget(); err != nil {
				return "", err
			}
			p.out.WriteRune(c)
			p.pos++
			p.lazy()
		case '{':
			if err := p.quantifierTarget(); err != nil {
				return "", err
			}
			if err := p.braces(); err != nil {
				return "", err
			}
			p.lazy()
		case '.':
			p.out.WriteString(jsDot)
			p.pos++
			p.atom = true
		case ']', '}':
			// uフラグでは、対になっていない]と}は構文の誤り。
			return "", fmt.Errorf("lone %q at %d", c, p.pos)
		default:
			writeLiteral(&p.out, c)
			p.pos++
			p.atom = true
		}
	}
	if depth != 0 {
		return "", fmt.Errorf("unbalanced (")
	}
	return p.out.String(), nil
}

func (p *jsRegexpParser) peekStr(s string) bool {
	r := []rune(s)
	if p.pos+len(r) > len(p.src) {
		return false
	}
	for i, c := range r {
		if p.src[p.pos+i] != c {
			return false
		}
	}
	return true
}

// quantifierTarget rejects a quantifier with nothing to repeat: at the start,
// after ( or |, after an assertion (^ $ \b \B), or after another quantifier.
// JavaScript's u flag makes all of them syntax errors.
func (p *jsRegexpParser) quantifierTarget() error {
	if !p.atom {
		return fmt.Errorf("nothing to repeat at %d", p.pos)
	}
	p.atom = false
	return nil
}

func (p *jsRegexpParser) lazy() {
	if p.pos < len(p.src) && p.src[p.pos] == '?' {
		p.out.WriteByte('?')
		p.pos++
	}
}

// braces copies a {n}, {n,} or {n,m} quantifier. JavaScript's u flag rejects
// a { that does not start one.
func (p *jsRegexpParser) braces() error {
	end := p.pos + 1
	for end < len(p.src) && p.src[end] != '}' {
		end++
	}
	if end >= len(p.src) {
		return fmt.Errorf("lone { at %d", p.pos)
	}
	body := string(p.src[p.pos+1 : end])
	lo, hi, comma := strings.Cut(body, ",")
	n, err := strconv.Atoi(lo)
	if err != nil || n < 0 || lo != strconv.Itoa(n) {
		return fmt.Errorf("bad quantifier {%s}", body)
	}
	if comma && hi != "" {
		m, err := strconv.Atoi(hi)
		if err != nil || m < n || hi != strconv.Itoa(m) {
			return fmt.Errorf("bad quantifier {%s}", body)
		}
	}
	p.out.WriteString("{" + body + "}")
	p.pos = end + 1
	return nil
}

// escapeOutside translates an escape outside a character class.
func (p *jsRegexpParser) escapeOutside() error {
	if p.pos+1 >= len(p.src) {
		return fmt.Errorf("trailing backslash")
	}
	p.atom = true
	switch p.src[p.pos+1] {
	case 'b':
		p.out.WriteString(`\b`)
		p.pos += 2
		p.atom = false
		return nil
	case 'B':
		// V8はuフラグでも、サロゲートの対の間(code unitの位置)で照合を試す。
		// そこは両側が単語の文字でないので\Bが当たる(/\B/u.test("Z\u{1F600}K")はtrue)。
		// Goはruneの境界しか試さないので、書き直せない。
		return fmt.Errorf(`\B at %d (V8 also matches it between the halves of a surrogate pair)`, p.pos)
	case 'd', 'D', 'w', 'W':
		p.out.WriteString(string(p.src[p.pos : p.pos+2]))
		p.pos += 2
		return nil
	case 's':
		p.out.WriteString("[" + jsstr.SpaceClass + "]")
		p.pos += 2
		return nil
	case 'S':
		p.out.WriteString("[^" + jsstr.SpaceClass + "]")
		p.pos += 2
		return nil
	case '-':
		// uフラグでは、`\-`は文字クラスの中でだけ書ける。
		return fmt.Errorf(`\- outside a class at %d`, p.pos)
	}
	r, err := p.charEscape(false)
	if err != nil {
		return err
	}
	writeLiteral(&p.out, r)
	return nil
}

// charEscape reads an escape that stands for one character (p.pos is at the
// backslash) and returns that character.
func (p *jsRegexpParser) charEscape(inClass bool) (rune, error) {
	c := p.src[p.pos+1]
	switch c {
	case 't':
		p.pos += 2
		return '\t', nil
	case 'n':
		p.pos += 2
		return '\n', nil
	case 'v':
		p.pos += 2
		return '\v', nil
	case 'f':
		p.pos += 2
		return '\f', nil
	case 'r':
		p.pos += 2
		return '\r', nil
	case '0':
		if p.pos+2 < len(p.src) && p.src[p.pos+2] >= '0' && p.src[p.pos+2] <= '9' {
			return 0, fmt.Errorf("octal escape at %d", p.pos)
		}
		p.pos += 2
		return 0, nil
	case 'x':
		r, ok := p.hex(p.pos+2, 2)
		if !ok {
			return 0, fmt.Errorf(`bad \x escape at %d`, p.pos)
		}
		p.pos += 4
		return r, nil
	case 'u':
		return p.unicodeEscape()
	case '^', '$', '\\', '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|', '/':
		p.pos += 2
		return c, nil
	case '-':
		if inClass {
			p.pos += 2
			return '-', nil
		}
	}
	// `\p{…}`、`\k<…>`、後方参照(`\1`)、`\c`などは使われていない。
	return 0, fmt.Errorf(`unsupported escape \%c at %d`, c, p.pos)
}

func (p *jsRegexpParser) hex(at, n int) (rune, bool) {
	if at+n > len(p.src) {
		return 0, false
	}
	// nは2か4なので16bitに収まる。bitSizeで上限を明示し、runeへの変換で溢れないようにする。
	v, err := strconv.ParseUint(string(p.src[at:at+n]), 16, 16)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

// unicodeEscape reads \uXXXX (combining a surrogate pair) or \u{X...}.
func (p *jsRegexpParser) unicodeEscape() (rune, error) {
	if p.pos+2 < len(p.src) && p.src[p.pos+2] == '{' {
		end := p.pos + 3
		for end < len(p.src) && p.src[end] != '}' {
			end++
		}
		if end >= len(p.src) || end == p.pos+3 {
			return 0, fmt.Errorf(`bad \u{} escape at %d`, p.pos)
		}
		v, err := strconv.ParseUint(string(p.src[p.pos+3:end]), 16, 32)
		if err != nil || v > utf8.MaxRune {
			return 0, fmt.Errorf(`bad \u{} escape at %d`, p.pos)
		}
		p.pos = end + 1
		return rune(v), nil
	}
	r, ok := p.hex(p.pos+2, 4)
	if !ok {
		return 0, fmt.Errorf(`bad \u escape at %d`, p.pos)
	}
	p.pos += 6
	if r >= 0xD800 && r <= 0xDBFF && p.peekStr(`\u`) {
		if lo, ok := p.hex(p.pos+2, 4); ok && lo >= 0xDC00 && lo <= 0xDFFF {
			p.pos += 6
			return 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00), nil
		}
	}
	if r >= 0xD800 && r <= 0xDFFF {
		// 対になっていないサロゲートは、Goの文字列(UTF-8)には現れない。
		return 0, fmt.Errorf("lone surrogate at %d", p.pos)
	}
	return r, nil
}

// class translates a character class starting at '['.
func (p *jsRegexpParser) class() error {
	start := p.pos
	p.pos++
	var b strings.Builder
	b.WriteByte('[')
	if p.pos < len(p.src) && p.src[p.pos] == '^' {
		b.WriteByte('^')
		p.pos++
	}
	empty := true
	for {
		if p.pos >= len(p.src) {
			return fmt.Errorf("unterminated class at %d", start)
		}
		if p.src[p.pos] == ']' {
			p.pos++
			break
		}
		lo, set, err := p.classAtom()
		if err != nil {
			return err
		}
		// a-bの範囲。`-`の後ろが`]`なら、`-`はただの文字。
		if p.pos+1 < len(p.src) && p.src[p.pos] == '-' && p.src[p.pos+1] != ']' {
			p.pos++
			hi, hiSet, err := p.classAtom()
			if err != nil {
				return err
			}
			if set != "" || hiSet != "" {
				return fmt.Errorf("class escape in a range at %d", start)
			}
			if lo > hi {
				return fmt.Errorf("range out of order at %d", start)
			}
			writeLiteral(&b, lo)
			b.WriteByte('-')
			writeLiteral(&b, hi)
		} else if set != "" {
			b.WriteString(set)
		} else {
			writeLiteral(&b, lo)
		}
		empty = false
	}
	if empty {
		// `[]` (何にも当たらない)と`[^]` (何にでも当たる)はGoで書けない。
		return fmt.Errorf("empty class at %d", start)
	}
	b.WriteByte(']')
	p.out.WriteString(b.String())
	return nil
}

// classAtom reads one class member: a character (set == "") or a class
// escape such as \d (set holds its Go form).
func (p *jsRegexpParser) classAtom() (r rune, set string, err error) {
	c := p.src[p.pos]
	if c != '\\' {
		p.pos++
		return c, "", nil
	}
	if p.pos+1 >= len(p.src) {
		return 0, "", fmt.Errorf("trailing backslash")
	}
	switch p.src[p.pos+1] {
	case 'd', 'D', 'w', 'W':
		set = string(p.src[p.pos : p.pos+2])
		p.pos += 2
		return 0, set, nil
	case 's':
		p.pos += 2
		return 0, jsstr.SpaceClass, nil
	case 'S':
		// クラスの中の補集合はGoで書けない(使われていない)。
		return 0, "", fmt.Errorf(`\S inside a class at %d`, p.pos)
	case 'b':
		// クラスの中の\bは後退(U+0008)。
		p.pos += 2
		return '\b', "", nil
	}
	r, err = p.charEscape(true)
	return r, "", err
}

func writeLiteral(b *strings.Builder, r rune) {
	fmt.Fprintf(b, `\x{%x}`, r)
}
