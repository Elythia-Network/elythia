package mfm

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// mfm-js の unicodeEmoji は @misskey-dev/emoji-data の emojiRegex を今の位置に
// 固定して読む。その正規表現を tools/emojiregex でコードポイント単位の Go の
// 正規表現へ移したもの (emoji_regex_gen.go) を使う (#3324)。どちらも選択肢を
// 左から順に試すので、同じ位置から同じ長さを読む。
//
// 元の正規表現は否定の先読み (U+FE0E が続かないこと) を持ち、RE2 では書けない。
// 生成ツールが「全ての先読みがマッチの先頭から同じ文字数の位置で同じ 1 文字を
// 見ている」ことを確かめたうえで、成り立つ版と成り立たない版の 2 つを出している
// ので、入力のその位置の文字で使い分ける。

var (
	emojiSyntaxLookaheadHolds = mustParseEmojiPattern(emojiPatternLookaheadHolds)
	emojiSyntaxLookaheadFails = mustParseEmojiPattern(emojiPatternLookaheadFails)

	// emojiMatchers caches the matchers built so far. Readers load the map
	// without locking; a writer copies it under emojiMatchersMu.
	//
	// 解析は同じ位置で何度も絵文字を試すので、引くたびの費用が効く。sync.Map
	// (interface のキーのハッシュ) だと 1 回 50ns ほどかかっていた。先頭になる
	// 文字は高々 emoji-data の一覧の分なので、書き込みのたびに写しても総量は小さい。
	emojiMatchers   atomic.Pointer[map[emojiMatcherKey]*emojiMatcher]
	emojiMatchersMu sync.Mutex
)

// emojiMatcherKey is the first code point, shifted left by one, with the low bit
// set when the negative lookaheads succeed.
type emojiMatcherKey int32

// emojiMatcher is the emoji pattern restricted to matches that start with one
// code point.
type emojiMatcher struct {
	re *regexp.Regexp // nil when no match starts with the code point
	// single reports whether the code point alone is a match, and next holds
	// the [lo, hi] pairs of the code points that can follow it in a longer one.
	single bool
	next   []rune
}

func mustParseEmojiPattern(p string) *syntax.Regexp {
	re, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		panic(err)
	}
	return re
}

// unicodeEmojiLen returns the byte length of the emoji mfm-js's unicodeEmoji
// reads at the start of s, or 0 when it reads none.
func unicodeEmojiLen(s string) int {
	first, _ := utf8.DecodeRuneInString(s)
	// 先頭の文字で始まる選択肢が無ければ正規表現を動かさない。本文の大半
	// (ASCII の英数字やかな・漢字) はここで返る。
	if s == "" || !unicode.Is(emojiFirstRunes, first) {
		return 0
	}
	lookaheadOK := !emojiHasLookahead || runeAt(s, emojiLookaheadOffset) != emojiLookaheadRune
	m := emojiMatcherFor(first, lookaheadOK)
	if m.re == nil {
		return 0
	}
	// 2 文字目が長い一致に続き得ない文字なら、正規表現を動かさずに決まる。
	// 本文の絵文字の大半 (😀 の後ろに文字や別の絵文字が続く) と、数字や © の
	// 連続はここで返る。正規表現の照合は 1 回あたり 100ns〜1µs かかり、解析は
	// 同じ位置で何度もこれを呼ぶので、数字の多い本文が目に見えて遅くなった。
	firstLen := utf8.RuneLen(first)
	if second, size := utf8.DecodeRuneInString(s[firstLen:]); size == 0 || !classContains(m.next, second) {
		if m.single {
			return firstLen
		}
		return 0
	}
	// 一致は emojiMaxBytes を超えないので、その先は読ませない。正規表現の仕事を
	// 入力の残りの長さによらず一定に抑える。先読みは上で済ませてあるので、
	// 切った先を見る必要は無い (途中で切れたコードポイントは一致の外にしか来ない)。
	if len(s) > emojiMaxBytes {
		s = s[:emojiMaxBytes]
	}
	loc := m.re.FindStringIndex(s)
	if loc == nil {
		return 0
	}
	return loc[1]
}

// emojiMatcherFor returns the emoji pattern restricted to matches starting with
// first, built once per first code point.
//
// 全体の正規表現 (約 1,100 命令) をそのまま動かすと、Go の regexp は 1 回の照合の
// 最初の 1 文字で全ての選択肢を候補に積むので、絵文字の多い本文では解析が 10 倍
// 以上遅くなった。先頭の文字で始まり得ない選択肢を落としても、残りの選択肢の
// 順序は変わらないので、左から順に試した結果は同じになる。
func emojiMatcherFor(first rune, lookaheadOK bool) *emojiMatcher {
	key := emojiMatcherKey(first << 1)
	if lookaheadOK {
		key |= 1
	}
	if cur := emojiMatchers.Load(); cur != nil {
		if m, ok := (*cur)[key]; ok {
			return m
		}
	}
	full := emojiSyntaxLookaheadFails
	if lookaheadOK {
		full = emojiSyntaxLookaheadHolds
	}
	m := &emojiMatcher{}
	if spec := specializeFirst([]*syntax.Regexp{full}, first); spec != nil {
		m.re = regexp.MustCompile(`^(?:` + spec.String() + `)`)
		m.next, m.single = firstRunesOf([]*syntax.Regexp{stripFirst(spec)})
	}
	emojiMatchersMu.Lock()
	defer emojiMatchersMu.Unlock()
	next := map[emojiMatcherKey]*emojiMatcher{key: m}
	if cur := emojiMatchers.Load(); cur != nil {
		if existing, ok := (*cur)[key]; ok {
			return existing
		}
		for k, v := range *cur {
			next[k] = v
		}
	}
	emojiMatchers.Store(&next)
	return m
}

// specializeFirst returns a regexp matching exactly the strings that the
// concatenation seq matches and that start with first, preferring them in the
// same leftmost-first order, or nil when there are none.
//
// 結合の先頭を展開していく: 選択は各選択肢に残りを付けたものの選択に、`?` は
// 「中身 + 残り」と「残り」の選択 (この順で試す) にする。先頭で 1 文字を読む
// 要素まで来たら、その文字が first のものだけを残す。
func specializeFirst(seq []*syntax.Regexp, first rune) *syntax.Regexp {
	if len(seq) == 0 {
		// 何も読まずに終わる経路は、first から始まる一致ではない
		return nil
	}
	head, rest := seq[0], seq[1:]
	with := func(subs ...*syntax.Regexp) []*syntax.Regexp {
		return append(append([]*syntax.Regexp{}, subs...), rest...)
	}
	switch head.Op {
	case syntax.OpLiteral:
		if len(head.Rune) == 0 || head.Rune[0] != first || head.Flags&syntax.FoldCase != 0 {
			return nil
		}
		return concat(with(head))
	case syntax.OpCharClass:
		if !classContains(head.Rune, first) {
			return nil
		}
		return concat(with(&syntax.Regexp{Op: syntax.OpLiteral, Rune: []rune{first}}))
	case syntax.OpEmptyMatch, syntax.OpBeginText:
		return specializeFirst(rest, first)
	case syntax.OpNoMatch:
		return nil
	case syntax.OpConcat:
		return specializeFirst(with(head.Sub...), first)
	case syntax.OpAlternate:
		var alts []*syntax.Regexp
		for _, sub := range head.Sub {
			if s := specializeFirst(with(sub), first); s != nil {
				alts = append(alts, s)
			}
		}
		return alternate(alts)
	case syntax.OpQuest:
		if head.Flags&syntax.NonGreedy != 0 {
			break
		}
		var alts []*syntax.Regexp
		if s := specializeFirst(with(head.Sub[0]), first); s != nil {
			alts = append(alts, s)
		}
		if s := specializeFirst(rest, first); s != nil {
			alts = append(alts, s)
		}
		return alternate(alts)
	}
	// 生成ツールが出さない形。黙って違う判定にしないよう落とす (全ての先頭の
	// 文字を試すテストがある)
	panic(fmt.Sprintf("mfm: unsupported emoji pattern construct %v", head.Op))
}

func concat(subs []*syntax.Regexp) *syntax.Regexp {
	if len(subs) == 1 {
		return subs[0]
	}
	return &syntax.Regexp{Op: syntax.OpConcat, Sub: subs}
}

func alternate(subs []*syntax.Regexp) *syntax.Regexp {
	switch len(subs) {
	case 0:
		return nil
	case 1:
		return subs[0]
	}
	return &syntax.Regexp{Op: syntax.OpAlternate, Sub: subs}
}

// stripFirst removes the first code point from a pattern built by
// specializeFirst, whose every branch starts with a literal.
func stripFirst(re *syntax.Regexp) *syntax.Regexp {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) == 1 {
			return &syntax.Regexp{Op: syntax.OpEmptyMatch}
		}
		return &syntax.Regexp{Op: syntax.OpLiteral, Rune: re.Rune[1:]}
	case syntax.OpConcat:
		return concat(append([]*syntax.Regexp{stripFirst(re.Sub[0])}, re.Sub[1:]...))
	case syntax.OpAlternate:
		subs := make([]*syntax.Regexp, len(re.Sub))
		for i, sub := range re.Sub {
			subs[i] = stripFirst(sub)
		}
		return alternate(subs)
	}
	panic(fmt.Sprintf("mfm: unexpected specialized emoji pattern %v", re.Op))
}

// firstRunesOf returns the sorted [lo, hi] pairs of the code points a match of
// the concatenation seq can start with, and whether it can match the empty
// string.
func firstRunesOf(seq []*syntax.Regexp) ([]rune, bool) {
	if len(seq) == 0 {
		return nil, true
	}
	head, rest := seq[0], seq[1:]
	with := func(subs ...*syntax.Regexp) []*syntax.Regexp {
		return append(append([]*syntax.Regexp{}, subs...), rest...)
	}
	switch head.Op {
	case syntax.OpLiteral:
		return []rune{head.Rune[0], head.Rune[0]}, false
	case syntax.OpCharClass:
		return head.Rune, false
	case syntax.OpEmptyMatch, syntax.OpBeginText:
		return firstRunesOf(rest)
	case syntax.OpNoMatch:
		return nil, false
	case syntax.OpConcat:
		return firstRunesOf(with(head.Sub...))
	case syntax.OpAlternate, syntax.OpQuest:
		branches := head.Sub
		var out []rune
		empty := false
		for _, sub := range branches {
			rs, e := firstRunesOf(with(sub))
			out = append(out, rs...)
			empty = empty || e
		}
		if head.Op == syntax.OpQuest {
			rs, e := firstRunesOf(rest)
			out = append(out, rs...)
			empty = empty || e
		}
		return out, empty
	}
	panic(fmt.Sprintf("mfm: unsupported emoji pattern construct %v", head.Op))
}

// classContains reports whether the [lo, hi] pairs of a syntax.OpCharClass hold r.
func classContains(ranges []rune, r rune) bool {
	for i := 0; i+1 < len(ranges); i += 2 {
		if ranges[i] <= r && r <= ranges[i+1] {
			return true
		}
	}
	return false
}

// runeAt returns the n-th code point of s (0-based), or -1 past the end.
func runeAt(s string, n int) rune {
	for i := 0; ; i++ {
		r, size := utf8.DecodeRuneInString(s)
		if size == 0 {
			return -1
		}
		if i == n {
			return r
		}
		s = s[size:]
	}
}

func (s *state) tryUnicodeEmoji() *Node {
	rest := s.remaining()
	n := unicodeEmojiLen(rest)
	if n == 0 {
		return nil
	}
	s.budget.used += n
	s.advance(n)
	// mfm-js は U+FE0F だけを読んだときは絵文字ではなく文字として返す
	if rest[:n] == "\ufe0f" {
		return Text(rest[:n])
	}
	return withProp(NodeUnicodeEmoji, "emoji", rest[:n])
}
