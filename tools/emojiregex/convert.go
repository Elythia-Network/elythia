package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The JavaScript regex is parsed into this small AST. Only the constructs the
// emoji-data regex actually uses are accepted; anything else is an error so a
// new construct in a future emoji-data cannot be translated silently wrong.

// alternation is `a|b|c`, either at the top level or inside a group.
type alternation struct {
	branches []*sequence
}

// sequence is a concatenation of items.
type sequence struct {
	items []*item
}

type itemKind int

const (
	kindChar      itemKind = iota // one character (code unit before pairing, code point after)
	kindClass                     // a character class
	kindGroup                     // (?:...) or (...)
	kindLookahead                 // (?!x) with a single character x
)

// item is one element of a sequence with an optional `?` quantifier.
type item struct {
	kind     itemKind
	ranges   []charRange  // kindChar (lo == hi) and kindClass
	group    *alternation // kindGroup
	optional bool
}

// charRange is an inclusive range of UTF-16 code units or code points.
type charRange struct {
	lo, hi rune
}

// jsParser parses a JavaScript regex source over UTF-16 code units, as a
// RegExp without the `u` flag sees it (mfm-js builds it with RegExp(source)).
type jsParser struct {
	units []rune
	pos   int
}

func parseJS(source string) (*alternation, error) {
	if !utf8.ValidString(source) {
		return nil, errors.New("source is not valid UTF-8")
	}
	// u フラグの無い RegExp はコードユニット単位で読むので、ソースに直接書かれた
	// アストラルの文字もサロゲートペアに分けてから解析する。
	var units []rune
	for _, u := range utf16.Encode([]rune(source)) {
		units = append(units, rune(u))
	}
	p := &jsParser{units: units}
	alt, err := p.parseAlternation()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.units) {
		return nil, fmt.Errorf("unexpected %q at %d", p.units[p.pos], p.pos)
	}
	return alt, nil
}

func (p *jsParser) peek() (rune, bool) {
	if p.pos >= len(p.units) {
		return 0, false
	}
	return p.units[p.pos], true
}

func (p *jsParser) hasPrefix(s string) bool {
	for i, c := range s {
		if p.pos+i >= len(p.units) || p.units[p.pos+i] != c {
			return false
		}
	}
	return true
}

func (p *jsParser) parseAlternation() (*alternation, error) {
	alt := &alternation{}
	for {
		seq, err := p.parseSequence()
		if err != nil {
			return nil, err
		}
		alt.branches = append(alt.branches, seq)
		if c, ok := p.peek(); ok && c == '|' {
			p.pos++
			continue
		}
		return alt, nil
	}
}

func (p *jsParser) parseSequence() (*sequence, error) {
	seq := &sequence{}
	for {
		c, ok := p.peek()
		if !ok || c == '|' || c == ')' {
			return seq, nil
		}
		it, err := p.parseAtom()
		if err != nil {
			return nil, err
		}
		if c, ok := p.peek(); ok {
			switch c {
			case '?':
				if it.kind == kindLookahead {
					return nil, fmt.Errorf("quantified lookahead at %d", p.pos)
				}
				it.optional = true
				p.pos++
				if c, ok := p.peek(); ok && (c == '?' || c == '*' || c == '+' || c == '{') {
					return nil, fmt.Errorf("unsupported quantifier %q at %d", c, p.pos)
				}
			case '*', '+', '{':
				return nil, fmt.Errorf("unsupported quantifier %q at %d", c, p.pos)
			}
		}
		seq.items = append(seq.items, it)
	}
}

func (p *jsParser) parseAtom() (*item, error) {
	c, _ := p.peek()
	switch c {
	case '(':
		return p.parseGroup()
	case '[':
		return p.parseClass()
	case '\\':
		u, err := p.parseEscape()
		if err != nil {
			return nil, err
		}
		return &item{kind: kindChar, ranges: []charRange{{u, u}}}, nil
	case '.', '^', '$', '*', '+', '?', '{', '}', ']':
		return nil, fmt.Errorf("unsupported syntax %q at %d", c, p.pos)
	}
	p.pos++
	return &item{kind: kindChar, ranges: []charRange{{c, c}}}, nil
}

func (p *jsParser) parseGroup() (*item, error) {
	start := p.pos
	p.pos++ // (
	kind := kindGroup
	switch {
	case p.hasPrefix("?:"):
		p.pos += 2
	case p.hasPrefix("?!"):
		p.pos += 2
		kind = kindLookahead
	case p.hasPrefix("?"):
		return nil, fmt.Errorf("unsupported group at %d", start)
	}
	// 捕獲グループは mfm-js が result[0] しか見ないので、非捕獲と同じに扱う。
	alt, err := p.parseAlternation()
	if err != nil {
		return nil, err
	}
	if c, ok := p.peek(); !ok || c != ')' {
		return nil, fmt.Errorf("unclosed group at %d", start)
	}
	p.pos++
	if kind == kindLookahead {
		if len(alt.branches) != 1 || len(alt.branches[0].items) != 1 {
			return nil, fmt.Errorf("lookahead at %d must hold a single character", start)
		}
		it := alt.branches[0].items[0]
		if it.kind != kindChar || it.optional {
			return nil, fmt.Errorf("lookahead at %d must hold a single character", start)
		}
		return &item{kind: kindLookahead, ranges: it.ranges}, nil
	}
	return &item{kind: kindGroup, group: alt}, nil
}

func (p *jsParser) parseClass() (*item, error) {
	start := p.pos
	p.pos++ // [
	if c, ok := p.peek(); ok && c == '^' {
		return nil, fmt.Errorf("negated class at %d", start)
	}
	var ranges []charRange
	for {
		c, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("unclosed class at %d", start)
		}
		if c == ']' {
			p.pos++
			break
		}
		lo, err := p.classChar()
		if err != nil {
			return nil, err
		}
		hi := lo
		if c, ok := p.peek(); ok && c == '-' && p.pos+1 < len(p.units) && p.units[p.pos+1] != ']' {
			p.pos++
			if hi, err = p.classChar(); err != nil {
				return nil, err
			}
			if hi < lo {
				return nil, fmt.Errorf("reversed range at %d", start)
			}
		}
		ranges = append(ranges, charRange{lo, hi})
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("empty class at %d", start)
	}
	return &item{kind: kindClass, ranges: ranges}, nil
}

func (p *jsParser) classChar() (rune, error) {
	c, _ := p.peek()
	if c == '\\' {
		return p.parseEscape()
	}
	if c == '[' || c == '-' {
		return 0, fmt.Errorf("unsupported class syntax %q at %d", c, p.pos)
	}
	p.pos++
	return c, nil
}

// parseEscape reads `\uXXXX`. Other escapes are not used by emoji-data.
func (p *jsParser) parseEscape() (rune, error) {
	start := p.pos
	if !p.hasPrefix(`\u`) || p.pos+6 > len(p.units) {
		return 0, fmt.Errorf("unsupported escape at %d", start)
	}
	hex := string(p.units[p.pos+2 : p.pos+6])
	v, err := strconv.ParseUint(hex, 16, 16)
	if err != nil {
		return 0, fmt.Errorf("bad escape %q at %d", hex, start)
	}
	p.pos += 6
	return rune(v), nil
}

func isHighSurrogate(u rune) bool { return u >= 0xD800 && u <= 0xDBFF }
func isLowSurrogate(u rune) bool  { return u >= 0xDC00 && u <= 0xDFFF }
func isSurrogate(u rune) bool     { return u >= 0xD800 && u <= 0xDFFF }

// pairSurrogates rewrites the AST from UTF-16 code units to code points.
//
// u フラグの無い正規表現でアストラル文字を表せるのは「上位サロゲート 1 つの後ろに
// 下位サロゲート (かその範囲のクラス) が続く」形だけなので、この形を 1 文字に
// まとめる。それ以外の位置にサロゲートが残る (片割れだけにマッチしうる) 正規表現は
// コードポイント単位の Go の正規表現では表せないので、エラーにする。
func pairSurrogates(alt *alternation) error {
	for _, seq := range alt.branches {
		var out []*item
		for i := 0; i < len(seq.items); i++ {
			it := seq.items[i]
			switch it.kind {
			case kindGroup:
				if err := pairSurrogates(it.group); err != nil {
					return err
				}
				out = append(out, it)
				continue
			case kindLookahead:
				if isSurrogate(it.ranges[0].lo) {
					return errors.New("lookahead on a surrogate")
				}
				out = append(out, it)
				continue
			}
			hasSurrogate := false
			for _, r := range it.ranges {
				if r.lo <= 0xDFFF && r.hi >= 0xD800 {
					hasSurrogate = true
				}
			}
			if !hasSurrogate {
				out = append(out, it)
				continue
			}
			high := it.ranges[0].lo
			if it.kind != kindChar || !isHighSurrogate(high) || it.optional {
				return fmt.Errorf("unpaired surrogate %#x", it.ranges[0].lo)
			}
			if i+1 >= len(seq.items) {
				return fmt.Errorf("high surrogate %#x at the end of a sequence", high)
			}
			next := seq.items[i+1]
			// `😀?` は上位サロゲートだけでも一致するので、後ろの任意も拒む
			if (next.kind != kindChar && next.kind != kindClass) || next.optional {
				return fmt.Errorf("high surrogate %#x not followed by a low surrogate", high)
			}
			var ranges []charRange
			for _, r := range next.ranges {
				if !isLowSurrogate(r.lo) || !isLowSurrogate(r.hi) {
					return fmt.Errorf("high surrogate %#x followed by %#x-%#x", high, r.lo, r.hi)
				}
				ranges = append(ranges, charRange{
					utf16.DecodeRune(high, r.lo),
					utf16.DecodeRune(high, r.hi),
				})
			}
			out = append(out, &item{kind: next.kind, ranges: ranges})
			i++
		}
		seq.items = out
	}
	return nil
}

// width returns the minimum and maximum number of code points an item matches.
func (it *item) width() (int, int) {
	var lo, hi int
	switch it.kind {
	case kindChar, kindClass:
		lo, hi = 1, 1
	case kindLookahead:
		lo, hi = 0, 0
	case kindGroup:
		lo, hi = it.group.width()
	}
	if it.optional {
		lo = 0
	}
	return lo, hi
}

func (alt *alternation) width() (int, int) {
	minW, maxW := -1, 0
	for _, seq := range alt.branches {
		lo, hi := 0, 0
		for _, it := range seq.items {
			a, b := it.width()
			lo += a
			hi += b
		}
		if minW < 0 || lo < minW {
			minW = lo
		}
		if hi > maxW {
			maxW = hi
		}
	}
	return minW, maxW
}

// maxBytes returns the maximum UTF-8 length of a match.
func (alt *alternation) maxBytes() int {
	best := 0
	for _, seq := range alt.branches {
		n := 0
		for _, it := range seq.items {
			switch it.kind {
			case kindChar, kindClass:
				m := 0
				for _, r := range it.ranges {
					m = max(m, utf8.RuneLen(r.hi))
				}
				n += m
			case kindGroup:
				n += it.group.maxBytes()
			}
		}
		best = max(best, n)
	}
	return best
}

// lookahead describes where the negative lookaheads of the regex look.
type lookahead struct {
	found  bool
	offset int  // code points from the start of the match
	char   rune // the character that must not follow
}

// findLookahead checks that every negative lookahead tests the same character
// at the same fixed offset from the start of the match.
//
// RE2 は先読みを持たない。先読みが全て「マッチの先頭から決まった文字数の位置に、
// 決まった 1 文字があるか」を見ているなら、1 回のマッチの中で全ての先読みは同じ
// 結果になる。そこで「全て成り立つ」版と「全て成り立たない」版の 2 つの正規表現を
// 作り、入力のその位置の文字でどちらを使うかを選べば、元と同じ判定になる。位置や
// 文字がばらばらの先読みはこの方法では表せないので、エラーにする。
func findLookahead(alt *alternation) (lookahead, error) {
	var la lookahead
	var walk func(a *alternation, lo, hi int) error
	walk = func(a *alternation, lo, hi int) error {
		for _, seq := range a.branches {
			curLo, curHi := lo, hi
			for _, it := range seq.items {
				switch it.kind {
				case kindLookahead:
					if curLo != curHi {
						return fmt.Errorf("lookahead at a variable offset %d-%d", curLo, curHi)
					}
					c := it.ranges[0].lo
					if la.found && (la.offset != curLo || la.char != c) {
						return fmt.Errorf("lookaheads differ: %d/%#x and %d/%#x", la.offset, la.char, curLo, c)
					}
					la = lookahead{found: true, offset: curLo, char: c}
				case kindGroup:
					if err := walk(it.group, curLo, curHi); err != nil {
						return err
					}
				}
				a, b := it.width()
				curLo += a
				curHi += b
			}
		}
		return nil
	}
	if err := walk(alt, 0, 0); err != nil {
		return lookahead{}, err
	}
	return la, nil
}

// firstRunes returns the sorted, merged set of code points a match can start
// with. The regex never matches the empty string (checked by the caller), so
// every match starts with one of these.
func (alt *alternation) firstRunes() []charRange {
	var out []charRange
	var collect func(a *alternation)
	collect = func(a *alternation) {
		for _, seq := range a.branches {
			for _, it := range seq.items {
				switch it.kind {
				case kindChar, kindClass:
					out = append(out, it.ranges...)
				case kindGroup:
					collect(it.group)
				}
				if lo, _ := it.width(); lo > 0 {
					break
				}
			}
		}
	}
	collect(alt)
	return mergeRanges(out)
}

func mergeRanges(rs []charRange) []charRange {
	sort.Slice(rs, func(i, j int) bool { return rs[i].lo < rs[j].lo })
	var out []charRange
	for _, r := range rs {
		if n := len(out); n > 0 && r.lo <= out[n-1].hi+1 {
			out[n-1].hi = max(out[n-1].hi, r.hi)
			continue
		}
		out = append(out, r)
	}
	return out
}

// lookaheadMode selects how negative lookaheads are rendered.
type lookaheadMode int

const (
	lookaheadHolds lookaheadMode = iota // the lookahead succeeds: render as empty
	lookaheadFails                      // the lookahead fails: render as a class that matches nothing
)

// renderRE2 writes the AST as a Go regexp/syntax pattern.
func (alt *alternation) renderRE2(b *strings.Builder, mode lookaheadMode) {
	for i, seq := range alt.branches {
		if i > 0 {
			b.WriteByte('|')
		}
		for _, it := range seq.items {
			it.renderRE2(b, mode)
		}
	}
}

func (it *item) renderRE2(b *strings.Builder, mode lookaheadMode) {
	switch it.kind {
	case kindChar:
		writeRune(b, it.ranges[0].lo)
	case kindClass:
		b.WriteByte('[')
		for _, r := range it.ranges {
			writeRune(b, r.lo)
			if r.hi != r.lo {
				b.WriteByte('-')
				writeRune(b, r.hi)
			}
		}
		b.WriteByte(']')
	case kindGroup:
		b.WriteString("(?:")
		it.group.renderRE2(b, mode)
		b.WriteByte(')')
	case kindLookahead:
		if mode == lookaheadFails {
			// どの文字にもマッチしないクラス。この分岐ごと失敗する
			b.WriteString(`[^\x00-\x{10FFFF}]`)
		}
		// 成り立つ版では何も書かない (空文字列にマッチする)
	}
	if it.optional {
		b.WriteByte('?')
	}
}

func writeRune(b *strings.Builder, r rune) {
	if r < utf8.RuneSelf && (r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
		b.WriteRune(r)
		return
	}
	fmt.Fprintf(b, `\x{%X}`, r)
}
