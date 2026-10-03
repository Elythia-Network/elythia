package mfm

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// Parse tokenizes and parses the input MFM string into an AST.
func Parse(input string) []*Node {
	if input == "" {
		return nil
	}
	// 不正な UTF-8 はパーサの中で rune と byte の長さが食い違い、slice の範囲外で
	// panic する (`"> \xff"` など)。JSON 由来の入力は置き換え済みだが、DB や
	// プラグインから来る文字列まで保証できないので入口で揃える
	input = strings.ToValidUTF8(input, "\uFFFD")
	s := newState(input, false)
	nodes := s.parseNodes(false)
	return mergeText(nodes)
}

// ParseSimple parses with limited syntax: only text, unicodeEmoji, emojiCode, plain.
func ParseSimple(input string) []*Node {
	if input == "" {
		return nil
	}
	input = strings.ToValidUTF8(input, "\uFFFD")
	s := newState(input, true)
	nodes := s.parseNodes(false)
	return mergeText(nodes)
}

// state tracks parser position and nesting.
type state struct {
	src       string
	pos       int
	depth     int
	nestLimit int
	inLink    bool
	simple    bool
	// fullDepth is the depth at which this state reads with mfm-js's full
	// parser (the top level, or the body of a quote). Children of nested
	// constructs are deeper and read with the inline parser.
	fullDepth int
	memo      *memoTable
	budget    *workBudget
}

type memoEntry struct {
	node *Node
	end  int32
}

// scanKind names the stop condition of a child loop, i.e. the closing
// delimiter that ends the children of a recursive construct.
type scanKind uint8

const (
	scanCenter scanKind = iota
	scanSmall
	scanBold
	scanItalic
	scanStrike
	scanBig
	scanBoldAsta
	scanStrikeWave
	scanFn
	scanLinkLabel
	numScanKinds
)

const (
	memoPageBits = 8
	memoPageSize = 1 << memoPageBits
)

// slotMemo holds the pages of one (depth, inLink, atFull) slot. A page covers
// memoPageSize positions, or fewer at the end of a short source.
type slotMemo struct {
	one  [][]memoEntry
	scan [numScanKinds][][]int32
}

// memoTable caches parse results for one source string. Each run of quote
// lines is parsed as a separate string with its own table (see quoteAt).
//
// parseOne の結果 (ノードと終了位置) は src 上の位置・深さ・link ラベルの中か
// どうか・その深さが block を読む最上位 (fullDepth) かどうかだけで決まる
// (nestLimit と simple は 1 回の Parse の間は変わらない)。深さは nestLimit に
// よる打ち切りで、inLink は link を試すかどうかで、最上位かどうかは center・
// 検索を試すかどうかとブロックの構文が前後の改行を読むかどうか (tryBlock) で
// 結果を変えるので、どれも表を分ける軸にする。病的な入力では全ての深さの全ての
// 位置を読むので、hash map ではなく位置で引くページ単位の配列にしてある
// (map だと実測で時間の過半が hash に消えた)。ページは触れたものだけ確保する。
type memoTable struct {
	srcLen    int
	nestLimit int
	budget    *workBudget
	slots     []*slotMemo // [slot(depth, inLink, depth == fullDepth)]
	// quoteStarts は ">" で始まる行の先頭位置 (昇順)。quoteOffsets / quoteRunOf は
	// 同じ添字で、その行が塊の中身のどこから始まるかと、どの塊に属するか。
	// quotesState は stopsState と同じ意味。
	quoteStarts  []int32
	quoteOffsets []int32
	quoteRunOf   []int32
	quoteRuns    []quoteRun
	quotesState  int8
	// stops[k] は区切りまで読む構文 k が止まる位置 (昇順)。stopsState[k] は
	// 0 = 未構築、1 = 構築済み、-1 = 使えない (src が不正な UTF-8 か確保の上限)。
	stops      [numStopKinds][]int32
	stopsState [numStopKinds]int8
	// linkTargets は link の飛び先を (位置, 深さ) ごとに読んだ結果 (linkTargetAt)。
	linkTargets map[linkTargetKey]linkTargetEntry
}

// stopKind names a construct that reads raw text up to a delimiter.
type stopKind uint8

const (
	stopPlainClose     stopKind = iota // </plain>
	stopMathBlockClose                 // \]
	stopMathInline                     // \) or a newline (CR or LF)
	stopURLAltEnd                      // '>', ' ', '\u3000' or '\t'
	stopCenterClose                    // </center>
	stopCodeBlockClose                 // a newline, ``` and a line end
	numStopKinds
)

// stopList returns the sorted offsets in src where the scan of kind stops,
// building it on first use. ok is false when the list cannot be used.
//
// <plain> / \[ / \( は開きの直後から区切りまで 1 文字ずつ読み、
// 閉じなければ末尾まで読んで失敗する。開きを並べると開始位置ごとに同じ区間を
// 読み直すので入力長の 2 乗になり、深さごとにも繰り返す (3000 バイトの
// 「\[」の並びで仕事量 9000 万、ローカルの上限の入力でも仕事量の上限に届いた)。
// 止まる位置を 1 度だけ列挙しておき、二分探索で引く。
//
// 元のループは rune 単位で進み、不正なバイトでは utf8.RuneLen(RuneError) = 3 で
// 進む。区切りは ASCII か U+3000 (先頭バイトで始まるので境界が一致する) なので、
// 正しい UTF-8 ならバイト単位で探した最初の出現と一致するが、不正な UTF-8 では
// 一致しないので元のループに任せる。
func (m *memoTable) stopList(src string, kind stopKind) ([]int32, bool) {
	switch m.stopsState[kind] {
	case 1:
		return m.stops[kind], true
	case -1:
		return nil, false
	}
	m.stopsState[kind] = -1
	if !utf8.ValidString(src) {
		return nil, false
	}
	var list []int32
	switch kind {
	case stopPlainClose:
		list = indexAll(src, "</plain>")
	case stopMathBlockClose:
		list = indexAll(src, "\\]")
	case stopCenterClose:
		list = indexAll(src, "</center>")
	case stopMathInline:
		for i := 0; i < len(src); i++ {
			// mfm-js の newLine は CR でも止まる
			if src[i] == '\n' || src[i] == '\r' || strings.HasPrefix(src[i:], "\\)") {
				list = append(list, int32(i))
			}
		}
	case stopCodeBlockClose:
		for i := 0; i < len(src); i++ {
			if l := newlineLenIn(src, i); l > 0 && strings.HasPrefix(src[i+l:], "```") && lineEndIn(src, i+l+3) {
				list = append(list, int32(i))
			}
		}
	case stopURLAltEnd:
		for i := 0; i < len(src); i++ {
			if c := src[i]; c == '>' || c == ' ' || c == '\t' || strings.HasPrefix(src[i:], "\u3000") {
				list = append(list, int32(i))
			}
		}
	}
	if !m.charge(4 * cap(list)) {
		return nil, false
	}
	m.stops[kind] = list
	m.stopsState[kind] = 1
	return list, true
}

// linkTargetKey identifies a cached link target. URL の括弧を何段まで読めるかが
// 深さで変わる (#3302) ので、位置だけで引くと深いところで読んだ失敗を浅い試行に
// 使い回してしまう。
type linkTargetKey struct {
	pos, depth int32
}

// linkTargetEntry caches the link target parsed at one offset.
type linkTargetEntry struct {
	end int32
	url string
	ok  bool
}

// linkTargetEntryCost is the memory charged per cached link target, on top of
// the URL itself.
const linkTargetEntryCost = 48

// linkTargetAt parses the link target starting at pos (just after "](") and
// returns the URL and the offset right after it, like mfm-js's
// alt([urlAlt, url]) in the link rule. ok is false when there is no URL.
//
// 結果は位置ごとに覚える。多数の `[` が同じ「](」に届き、リンクが成立しない
// 形 (`[[[[a](https://xxx...` で `)` が無いなど) では、同じ飛び先を `[` の数だけ
// 読み直して入力長の 2 乗になる。3000 バイトでも仕事量の上限に届き、以降が
// テキストになる (以前は飛び先を `)` まで何でも読んでいたので、括弧の対応を
// 1 度だけ求める parenMatch で同じことを防いでいた)。
func (s *state) linkTargetAt(pos int) (url string, end int, ok bool) {
	m := s.memo
	key := linkTargetKey{pos: int32(pos), depth: int32(s.depth)}
	if e, hit := m.linkTargets[key]; hit {
		return e.url, int(e.end), e.ok
	}
	save := s.pos
	s.pos = pos
	url, ok = s.readLinkTarget()
	end = s.pos
	s.pos = save
	if m.charge(linkTargetEntryCost + len(url)) {
		if m.linkTargets == nil {
			m.linkTargets = map[linkTargetKey]linkTargetEntry{}
		}
		m.linkTargets[key] = linkTargetEntry{end: int32(end), url: url, ok: ok}
	}
	return url, end, ok
}

// urlAltStop returns the offset of the first '>' or space at or after pos
// (len(src) when none), or -1 when the work budget runs out.
func (s *state) urlAltStop(pos int) int {
	save := s.pos
	s.pos = pos
	stop, ok := s.nextStop(stopURLAltEnd)
	s.pos = save
	if ok {
		return stop
	}
	// 索引を作れないとき (メモの確保の上限に届いたとき。Parse は入口で UTF-8 を
	// 正すので、不正な UTF-8 は内部の呼び出しからしか来ない) は 1 文字ずつ読むが、
	// 読んだ分を仕事量に数えて上限で打ち切る。
	for i := pos; i < len(s.src); {
		if s.budget.used++; s.budget.exhausted() {
			return -1
		}
		r, size := utf8.DecodeRuneInString(s.src[i:])
		if r == '>' || r == ' ' || r == '\u3000' || r == '\t' {
			return i
		}
		i += size
	}
	return len(s.src)
}

// readLinkTarget reads `<https://...>` (mfm-js urlAlt, via readURLAlt) or a
// plain URL (mfm-js url, via tryURL).
func (s *state) readLinkTarget() (string, bool) {
	if s.peek() == '<' {
		return s.readURLAlt()
	}
	n := s.tryURL()
	if n == nil {
		return "", false
	}
	url, _ := n.Props["url"].(string)
	return url, url != ""
}

// readURLAlt reads mfm-js's urlAlt `<https://...>` at the current position:
// up to the closing '>' without a space, returning the URL without the
// brackets. The position moves past '>' only on success.
func (s *state) readURLAlt() (string, bool) {
	if s.peek() != '<' {
		return "", false
	}
	start := s.pos + 1
	bodyStart := start
	switch {
	case s.prefixAt(start, "https://"):
		bodyStart += len("https://")
	case s.prefixAt(start, "http://"):
		bodyStart += len("http://")
	default:
		return "", false
	}
	// 閉じの `>` か空白 (mfm-js の space は半角空白・全角空白・タブで、改行は
	// 含まない) まで読む。**1 文字ずつ読まない** — `[a](<https://x` の後に改行を
	// 挟んで並べると、どの `](` からも末尾まで読むことになり入力長の 2 乗になる。
	// 区切りの位置の索引から引く。
	stop := s.urlAltStop(bodyStart)
	if stop < 0 || stop >= len(s.src) || s.src[stop] != '>' || stop == bodyStart {
		return "", false
	}
	s.pos = stop + 1
	return s.src[start:stop], true
}

// tryURLAlt parses mfm-js's urlAlt in text: `<https://...>` becomes a URL
// node with the brackets prop.
//
// mfm-js は `<>` で囲むと、url の文字に無い日本語や記号も含めて 1 つの URL に
// する。#3302 で url の文字を mfm-js に揃えたので、これが無いと
// `<https://ja.wikipedia.org/wiki/日本>` が `wiki/` で切れる。
func (s *state) tryURLAlt() *Node {
	// リンクのラベルの中では URL にしない (mfm-js の notLinkLabel)。
	if s.inLink {
		return nil
	}
	url, ok := s.readURLAlt()
	if !ok {
		return nil
	}
	return &Node{Type: NodeURL, Props: map[string]any{"url": url, "brackets": true}}
}

// indexAll returns the offsets of every (possibly overlapping) occurrence of
// needle in src.
func indexAll(src, needle string) []int32 {
	var out []int32
	for i := 0; ; {
		j := strings.Index(src[i:], needle)
		if j < 0 {
			return out
		}
		out = append(out, int32(i+j))
		i += j + 1
	}
}

// nextStop returns the first offset at or after the current position where
// the scan of kind stops, or len(src) when it runs to the end. ok is false
// when the caller must fall back to reading rune by rune.
func (s *state) nextStop(kind stopKind) (int, bool) {
	list, ok := s.memo.stopList(s.src, kind)
	if !ok {
		return 0, false
	}
	s.budget.used++
	i := sort.Search(len(list), func(i int) bool { return int(list[i]) >= s.pos })
	if i == len(list) {
		return len(s.src), true
	}
	return int(list[i]), true
}

// newMemoTable returns an empty table. Everything in it is allocated on first
// use and charged against the budget.
func newMemoTable(srcLen, nestLimit int, budget *workBudget) *memoTable {
	return &memoTable{srcLen: srcLen, nestLimit: nestLimit, budget: budget}
}

// slotFor returns the pages of the slot for depth, inLink and whether depth is
// the block-reading top level (atFull), allocating the
// slot on first use, or nil when the position is out of range or the
// allocation is refused.
//
// quote の中身は quote ごとに別の表を持つので、表の入れ物やページを入力の長さに
// 関係なく固定の大きさで確保すると、短い quote を並べただけで確保量が入力の
// 数千倍になる (3000 バイトで 13MB。1 表あたり入れ物 10KB、触れたスロットごとに
// 4KB のページ)。入れ物はスロット単位で、ページは src の長さまでに切り詰めて
// 確保し、どちらも charge に含める。
func (m *memoTable) slotFor(pos, depth int, inLink, atFull bool) *slotMemo {
	if depth < 0 || depth > m.nestLimit || pos < 0 || pos > m.srcLen {
		return nil
	}
	i := depth * 4
	if inLink {
		i += 2
	}
	if atFull {
		i++
	}
	// 入れ物は、使ったうちで最も深いスロットまでだけ伸ばす。短い引用の中身は
	// 浅い深さしか読まないので、全ての深さの分を先に確保すると、短い引用を
	// 並べた入力で確保量の上限に届く (#3325)。伸ばすたびに新しい大きさの分を
	// charge するので、数える量は実際の確保の高々 2 倍
	if i >= len(m.slots) {
		n := min(max(i+1, 2*len(m.slots)), (m.nestLimit+1)*4)
		if !m.charge(n * int(unsafe.Sizeof((*slotMemo)(nil)))) {
			return nil
		}
		m.slots = append(make([]*slotMemo, 0, n), m.slots...)[:n]
	}
	sm := m.slots[i]
	if sm == nil {
		pages := m.srcLen>>memoPageBits + 1
		if !m.charge(int(unsafe.Sizeof(slotMemo{})) + pages*int(unsafe.Sizeof([]memoEntry(nil)))) {
			return nil
		}
		sm = &slotMemo{one: make([][]memoEntry, pages)}
		m.slots[i] = sm
	}
	return sm
}

// pageLen returns the number of cells of the page that holds pos.
func (m *memoTable) pageLen(pos int) int {
	return min(memoPageSize, m.srcLen+1-pos&^(memoPageSize-1))
}

// quoteRun is a maximal run of consecutive lines starting with ">", with the
// markers stripped and the lines joined, and the table used to parse it.
type quoteRun struct {
	inner string
	end   int // position right after the run in the parent source
	table *memoTable
}

// quoteAt returns the quote run that a quote starting at the line start pos
// reads and the offset in run.inner where it starts, building every run of
// src on first use. ok is false when pos does not start a quote line or the
// runs could not be allocated.
//
// quote は開始行から ">" で始まる行が続く限りを読むので、塊の途中の行から始めた
// quote の中身は、塊全体の中身の後ろ半分と一致する。行ごとに切り出して別の表で
// 読むと、閉じない <b> の下で各行の先頭から末尾までを切り出し・解析し直して行数の
// 2 乗になり (3000 バイトで仕事量 150 万、64KB の inbox 上限なら 2 乗で増える)、
// 同じ quote を別の深さから読み直すたびに表を作り直すと入れ子の段数に対して
// 指数的になる (11 段 125 バイトで仕事量の上限に届いた)。塊を 1 度だけ切り出し、
// 1 つの表を全ての開始行と深さで共有する。quote は最上位か引用の中身の最上位
// からしか読まないので、1 つの表を読む state の fullDepth は 1 通りに決まり、
// 表は深さをキーに含むので共有してよい (最上位かどうかの軸もキーに含めているが、
// これは守り)。また、
// 塊の途中から読むときの直前の文字は改行なので、行頭の判定も直前の文字の判定も
// 行ごとに切り出した場合と変わらない。
//
// 塊の一覧は 1 行ごとに charge しながら作る。作り終えてからまとめて charge すると、
// 短い quote を並べた 1MB の入力で上限を確かめる前に 100MB を確保した (実測)。
func (m *memoTable) quoteAt(src string, pos int) (*quoteRun, int, bool) {
	if m.quotesState == 0 && !m.buildQuotes(src) {
		return nil, 0, false
	}
	if m.quotesState != 1 {
		return nil, 0, false
	}
	i := sort.Search(len(m.quoteStarts), func(i int) bool { return int(m.quoteStarts[i]) >= pos })
	if i == len(m.quoteStarts) || int(m.quoteStarts[i]) != pos {
		return nil, 0, false
	}
	run := &m.quoteRuns[m.quoteRunOf[i]]
	if run.table == nil {
		if !m.charge(int(unsafe.Sizeof(memoTable{}))) {
			return nil, 0, false
		}
		run.table = newMemoTable(len(run.inner), m.nestLimit, m.budget)
	}
	return run, int(m.quoteOffsets[i]), true
}

// buildQuotes splits src into quote runs. It reports false, leaving the runs
// unusable, when the memory budget refuses them.
func (m *memoTable) buildQuotes(src string) bool {
	m.quotesState = -1
	var (
		b        strings.Builder
		inRun    bool
		lineCost = int(3 * unsafe.Sizeof(int32(0)))
	)
	flush := func(end int) {
		if !inRun {
			return
		}
		r := &m.quoteRuns[len(m.quoteRuns)-1]
		r.inner, r.end = b.String(), end
		b.Reset()
		inRun = false
	}
	// 行は mfm-js の newLine (CRLF / CR / LF) で切る。各行の中身は改行を含まず、
	// 中身は "\n" でつなぐ (mfm-js は contents.join("\n"))。LF だけで切ると、
	// `> a\rb` の CR の後ろまで引用に入り、`\r> a` の CR の直後を行頭とみなさない。
	for p := 0; p < len(src); {
		e := strings.IndexAny(src[p:], "\r\n")
		if e < 0 {
			e = len(src)
		} else {
			e += p
		}
		next := e + newlineLenIn(src, e)
		if src[p] != '>' {
			flush(p)
			p = next
			continue
		}
		c := p + 1
		// `>` の直後の空白は mfm-js の space.option() で、全角空白も 1 つ読む
		switch {
		case c < len(src) && (src[c] == ' ' || src[c] == '\t'):
			c++
		case strings.HasPrefix(src[c:], "\u3000"):
			c += len("\u3000")
		}
		cost := lineCost + e - c + 1
		if !inRun {
			cost += int(unsafe.Sizeof(quoteRun{}))
		}
		if !m.charge(cost) {
			m.quoteStarts, m.quoteOffsets, m.quoteRunOf, m.quoteRuns = nil, nil, nil, nil
			return false
		}
		if !inRun {
			m.quoteRuns = append(m.quoteRuns, quoteRun{})
			inRun = true
		} else {
			b.WriteByte('\n')
		}
		m.quoteStarts = append(m.quoteStarts, int32(p))
		m.quoteOffsets = append(m.quoteOffsets, int32(b.Len()))
		m.quoteRunOf = append(m.quoteRunOf, int32(len(m.quoteRuns)-1))
		b.WriteString(src[c:e])
		p = next
	}
	flush(len(src))
	m.quotesState = 1
	return true
}

// charge accounts n bytes of memo storage against the shared budget and
// reports whether the allocation may proceed.
//
// 仕事量の上限だけではメモリが縛れない。病的な入力は全ての深さ・全ての構文の
// 表を触るので、リモートノートのように長さを切り詰めずに届く本文では 1MB で
// 1.9GB を確保した (実測)。上限に達したら以降は構文を試さずテキストとして
// 読ませる (workBudget.exhausted が真になる) ので、確保はそこで止まる。
func (m *memoTable) charge(n int) bool {
	b := m.budget
	if b == nil {
		return true
	}
	if b.memUsed+n > memoByteLimit {
		b.used = b.limit + 1
		return false
	}
	b.memUsed += n
	return true
}

// oneEntry returns the cell for parseOne at pos, allocating its page.
func (m *memoTable) oneEntry(pos, depth int, inLink, atFull bool) *memoEntry {
	sm := m.slotFor(pos, depth, inLink, atFull)
	if sm == nil {
		return nil
	}
	pg := sm.one[pos>>memoPageBits]
	if pg == nil {
		n := m.pageLen(pos)
		if !m.charge(n * int(unsafe.Sizeof(memoEntry{}))) {
			return nil
		}
		pg = make([]memoEntry, n)
		sm.one[pos>>memoPageBits] = pg
	}
	return &pg[pos&(memoPageSize-1)]
}

// scanEntry returns the cell holding end+1 of the child loop of kind started
// at pos (0 = unknown), allocating its page.
func (m *memoTable) scanEntry(pos, depth int, inLink, atFull bool, kind scanKind) *int32 {
	sm := m.slotFor(pos, depth, inLink, atFull)
	if sm == nil {
		return nil
	}
	pages := sm.scan[kind]
	if pages == nil {
		if !m.charge(len(sm.one) * int(unsafe.Sizeof([]int32(nil)))) {
			return nil
		}
		pages = make([][]int32, len(sm.one))
		sm.scan[kind] = pages
	}
	pg := pages[pos>>memoPageBits]
	if pg == nil {
		n := m.pageLen(pos)
		if !m.charge(n * 4) {
			return nil
		}
		pg = make([]int32, n)
		pages[pos>>memoPageBits] = pg
	}
	return &pg[pos&(memoPageSize-1)]
}

// workBudget bounds the total parsing work of one Parse call. Quote bodies
// share the budget of the Parse call that contains them.
type workBudget struct {
	used    int
	limit   int
	memUsed int
}

// メモ化と区切り位置の索引で、閉じない構文を並べた入力も入力長に比例する仕事量に
// なる。ただし比例定数は深さ (最大 21 段) の分だけ大きく、「>>」の後ろに
// 「$[x.a=b __*https://a:」を並べると 1 バイトあたり約 85 になる (実測。閉じない
// 装飾は #3301 から読んだ範囲ごと文字にするので、それだけを並べた形は 1 バイト
// あたり 2 回)。見落とした経路があっても 1 回の Parse が際限なく走らないよう、
// 安全網として上限を置く。超えたら以降の未解析の位置は構文を試さずテキストとして読む。
//
// 基礎分はローカルの本文上限 (3000 文字) の入力が届かない大きさにしてある。
// 3000 バイトの病的な入力で実測した最大は約 51 万 (「[」を 1000 個並べた後ろに
// 長い飛び先を置く形。上の形は約 25 万) で、基礎分だけでその 4 倍ある。長さを
// 切り詰めずに届くリモートの本文は、病的な形なら上限かメモ表の上限に届いて
// 途中からテキストになる (通常の文章は 1 バイトあたり 2 未満なので仕事量の
// 上限には届かない)。
const (
	workBudgetBase    = 1 << 21
	workBudgetPerByte = 32
	// memoByteLimit はメモ表が 1 回の Parse で確保してよい総量。3000 バイトの
	// 病的な入力の実測は最大 3MB 程度で、病的な形は 17KB 程度から仕事量か
	// ここの上限に届く。
	memoByteLimit = 16 << 20
)

func (b *workBudget) exhausted() bool { return b.used > b.limit }

func newState(src string, simple bool) *state {
	budget := &workBudget{limit: workBudgetBase + workBudgetPerByte*len(src)}
	return &state{
		src:       src,
		nestLimit: 20,
		simple:    simple,
		memo:      newMemoTable(len(src), 20, budget),
		budget:    budget,
	}
}

func (s *state) remaining() string { return s.src[s.pos:] }
func (s *state) eof() bool         { return s.pos >= len(s.src) }

func (s *state) peek() rune {
	if s.eof() {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(s.src[s.pos:])
	return r
}

func (s *state) advance(n int) {
	s.pos += n
	s.budget.used++
}

func (s *state) hasPrefix(prefix string) bool {
	return strings.HasPrefix(s.remaining(), prefix)
}

// parseNodes parses until EOF or (if inQuote) until a line doesn't start with >.
func (s *state) parseNodes(inQuote bool) []*Node {
	var nodes []*Node
	for !s.eof() {
		if inQuote {
			// quoteの中で行頭が>でなくなったら終了
			if s.pos > 0 && s.src[s.pos-1] == '\n' && !s.hasPrefix(">") {
				break
			}
		}
		node := s.parseOne()
		if node == nil {
			break
		}
		nodes = append(nodes, node)
	}
	return nodes
}

func (s *state) parseOne() *Node {
	if s.eof() {
		return nil
	}
	s.budget.used++
	if s.simple {
		return s.parseSimpleOne()
	}
	// mfm-js の nest は、深さが上限に届いた子を構文として読まず 1 文字ずつ読む
	// (alt([seq(nestable, parser), char]))。装飾は中身を閉じまで読むので、上限の
	// 下でも閉じが無ければ読んだ範囲がまるごと文字になる。
	if s.depth >= s.nestLimit {
		return s.consumeChar()
	}
	// 深さ 0 かつ link ラベルの外で読む位置は最上位のループが 1 度ずつ読むだけで、
	// 読み直されることが無い (子のループは深さ 1 以上か link ラベルの中)。
	// 素のテキストで表を膨らませないよう記録しない
	if s.depth == 0 && !s.inLink {
		return s.parseFullOne()
	}
	// 閉じない <b> などは失敗するたびに 1 文字進めて、同じ位置を別の親から
	// 読み直す。メモ化しないと入力長に対して指数時間になる
	e := s.memo.oneEntry(s.pos, s.depth, s.inLink, s.depth == s.fullDepth)
	if e == nil {
		return s.parseFullOne()
	}
	if e.node != nil {
		s.pos = int(e.end)
		return e.node
	}
	n := s.parseFullOne()
	// parseFullOne は EOF 以外で nil を返さないので、node != nil を「記録済み」に使える
	*e = memoEntry{node: n, end: int32(s.pos)}
	return n
}

func (s *state) parseSimpleOne() *Node {
	if s.budget.exhausted() {
		return s.consumeChar()
	}
	if n := s.tryUnicodeEmoji(); n != nil {
		return n
	}
	if n := s.tryEmojiCode(); n != nil {
		return n
	}
	if n := s.tryPlainTag(); n != nil {
		return n
	}
	return s.consumeChar()
}

func (s *state) parseFullOne() *Node {
	if s.budget.exhausted() {
		return s.consumeChar()
	}
	// mfm-js の alt() 順序に従う
	if n := s.tryUnicodeEmoji(); n != nil {
		return n
	}
	if n := s.tryCenterTag(); n != nil {
		return n
	}
	if n := s.trySmallTag(); n != nil {
		return n
	}
	if n := s.tryPlainTag(); n != nil {
		return n
	}
	if n := s.tryBoldTag(); n != nil {
		return n
	}
	if n := s.tryItalicTag(); n != nil {
		return n
	}
	if n := s.tryStrikeTag(); n != nil {
		return n
	}
	if n := s.tryURLAlt(); n != nil {
		return n
	}
	if n := s.tryBig(); n != nil {
		return n
	}
	if n := s.tryBoldAsta(); n != nil {
		return n
	}
	if n := s.tryItalicAsta(); n != nil {
		return n
	}
	if n := s.tryBoldUnder(); n != nil {
		return n
	}
	if n := s.tryItalicUnder(); n != nil {
		return n
	}
	if n := s.tryBlock(1, 0, false, (*state).tryCodeBlock); n != nil {
		return n
	}
	if n := s.tryInlineCode(); n != nil {
		return n
	}
	if n := s.tryBlock(2, 1, false, (*state).tryQuote); n != nil {
		return n
	}
	if n := s.tryBlock(1, 1, true, (*state).tryMathBlock); n != nil {
		return n
	}
	if n := s.tryMathInline(); n != nil {
		return n
	}
	if n := s.tryStrikeWave(); n != nil {
		return n
	}
	if n := s.tryFn(); n != nil {
		return n
	}
	if n := s.tryMention(); n != nil {
		return n
	}
	if n := s.tryHashtag(); n != nil {
		return n
	}
	if n := s.tryEmojiCode(); n != nil {
		return n
	}
	if !s.inLink {
		if n := s.tryLink(); n != nil {
			return n
		}
	}
	if n := s.tryURL(); n != nil {
		return n
	}
	if n := s.tryBlock(1, 1, false, (*state).trySearch); n != nil {
		return n
	}
	return s.consumeChar()
}

// fullContext reports whether the current position is read by mfm-js's full
// parser (the top level or the contents of a quote) rather than by the inline
// parser of a construct's children. Only the full parser has the block
// constructs and search.
//
// block 構文は full でだけ試す (#3301) ので、引用の中身の深さは最上位と同じに
// なり、同じ深さの位置が full のこともそうでないこともある経路は今は無い。
// メモの表は守りとして depth == fullDepth かどうかでも分けてある (slotFor)。
// link のラベルは 1 段深く読む (#3301) ので深さの比較だけでも外れるが、
// ラベルの中を full に数えないことを inLink でも明示しておく。
func (s *state) fullContext() bool {
	return !s.simple && !s.inLink && s.depth == s.fullDepth
}

// skipNewlines returns the position after up to n newlines (CRLF, CR or LF,
// like mfm-js's newLine) starting at pos.
func (s *state) skipNewlines(pos, n int) int {
	for ; n > 0; n-- {
		l := s.newlineLenAt(pos)
		if l == 0 {
			break
		}
		pos += l
	}
	return pos
}

// tryBlock tries a block construct the way mfm-js's full parser does: the
// construct first reads up to lead newlines (newLine.option()) and, once it
// matched, up to trail more newlines after it. When whole is set, the
// construct must also start at a line begin and end at a line end (mfm-js's
// lineBegin and lineEnd), which try does not check itself. Outside a full
// context it fails without trying, as mfm-js's inline has no block constructs.
//
// mfm-js の検索・引用・コードブロック・数式ブロックは改行の位置から試され、
// 前の改行を自分のノードに含める。改行の位置では次の行のハッシュタグ・
// メンション・絵文字などはまだ試されないので、行頭がそれらでも検索になる
// (#3325)。前の改行を読んで失敗したときは、改行を読まずにやり直さない
// (mfm-js の option は後戻りしない)。後ろの改行は、引用が 2 つ、他は 1 つまで
// 読む。引用の塊とコードブロックは、その 1 つ目を try が既に読んでいる。
// center は前後の改行と行頭・行末を tryCenterTag が自分で確かめる (#3328) ので、
// ここを通さない。
//
// 改行の位置から試すと、行の先頭と末尾を確かめない数式ブロックは前の行の
// 改行を読んで行の途中のものまで拾い、検索より先に成功してしまうので、
// full では whole で確かめる。
func (s *state) tryBlock(lead, trail int, whole bool, try func(*state) *Node) *Node {
	if !s.fullContext() {
		return nil
	}
	save := s.pos
	s.pos = s.skipNewlines(save, lead)
	if whole && !s.atLineBegin() {
		s.pos = save
		return nil
	}
	n := try(s)
	if n == nil || whole && !s.lineEndAt(s.pos) {
		s.pos = save
		return nil
	}
	s.pos = s.skipNewlines(s.pos, trail)
	return n
}

// asciiText holds shared single-byte text nodes returned by consumeChar.
//
// 失敗した試行の中で読んだ 1 文字ずつのノードは大半が捨てられるので、ASCII は
// 共有して割り当てを省く。出力に出るテキストノードは mergeText が必ず作り直す
// ので、共有ノードが呼び出し側へ渡って書き換えられることは無い
var asciiText = func() (t [utf8.RuneSelf]*Node) {
	for i := range t {
		t[i] = Text(string(rune(i)))
	}
	return t
}()

// consumeChar takes one rune and returns it as a text node.
func (s *state) consumeChar() *Node {
	r, size := utf8.DecodeRuneInString(s.remaining())
	s.advance(size)
	if r < utf8.RuneSelf {
		return asciiText[r]
	}
	return Text(string(r))
}

// nest runs parser function with incremented depth. nil on limit.
func (s *state) nest(fn func() []*Node) []*Node {
	s.depth++
	if s.depth > s.nestLimit {
		s.depth--
		return nil
	}
	result := fn()
	s.depth--
	return result
}

// prefixAt reports whether src has prefix p at byte offset pos.
func (s *state) prefixAt(pos int, p string) bool {
	return pos <= len(s.src) && strings.HasPrefix(s.src[pos:], p)
}

// stopsAt reports whether the child loop of kind ends at the current position.
func (s *state) stopsAt(kind scanKind) bool {
	switch kind {
	case scanCenter:
		// mfm-js は `notMatch(seq(newLine.option(), close))` で止まるので、閉じの
		// 直前の改行は子に含めない
		return s.centerCloseAt(s.pos)
	case scanSmall:
		return s.hasPrefix("</small>")
	case scanBold:
		return s.hasPrefix("</b>")
	case scanItalic:
		return s.hasPrefix("</i>")
	case scanStrike:
		return s.hasPrefix("</s>")
	case scanBig:
		return s.hasPrefix("***")
	case scanBoldAsta:
		// mfm-js の boldAsta は改行で止まらない (`**a⏎b**` は太字)。
		return s.hasPrefix("**")
	case scanStrikeWave:
		// mfm-js の newLine は CRLF / CR / LF のどれか。
		return s.hasPrefix("~~") || s.peek() == '\n' || s.peek() == '\r'
	case scanFn:
		return s.peek() == ']'
	case scanLinkLabel:
		// ~~ と同じく、mfm-js の newLine は CR でも止まる
		return s.peek() == ']' || s.peek() == '\n' || s.peek() == '\r'
	}
	return true
}

// scanEnd returns the position where the child loop of kind, started at the
// current position with the current depth and inLink, stops: the first
// position reached by successive parseOne calls that is EOF or satisfies
// stopsAt. The current position is left unchanged.
//
// 同じ位置から始まる子のループは、止まる条件が同じなら必ず同じ位置で止まる。
// 開きタグが失敗して 1 文字進むたびに同じ区間を末尾まで辿り直すと、parseOne を
// メモ化していても入力長の 2 乗になるので、止まる位置を経路上の全位置に記録する。
func (s *state) scanEnd(kind scanKind) int {
	start := s.pos
	var path []int
	end := start
	for {
		if s.eof() || s.stopsAt(kind) {
			end = s.pos
			break
		}
		if e := s.memo.scanEntry(s.pos, s.depth, s.inLink, s.depth == s.fullDepth, kind); e != nil && *e != 0 {
			end = int(*e) - 1
			break
		}
		path = append(path, s.pos)
		if s.parseOne() == nil {
			end = s.pos
			break
		}
	}
	for _, p := range path {
		if e := s.memo.scanEntry(p, s.depth, s.inLink, s.depth == s.fullDepth, kind); e != nil {
			*e = int32(end) + 1
		}
	}
	s.pos = start
	return end
}

// collectTo parses nodes from the current position up to end, which must be
// a position scanEnd returned for the same depth and inLink. Every step is a
// memo hit because scanEnd already walked the same chain.
func (s *state) collectTo(end int) []*Node {
	var nodes []*Node
	for s.pos < end {
		n := s.parseOne()
		if n == nil {
			break
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// --- Block-level parsers ---

func (s *state) tryQuote() *Node {
	// mfm-js の block 構文 (quote / codeBlock / mathBlock / search) は full でだけ
	// 読み、inline (装飾や center の子) には無い。子で読むと、閉じのある装飾も
	// 閉じを block に飲まれて丸ごと文字になり、後ろのメンションやタグが落ちる (#3301)。
	if s.depth != s.fullDepth {
		return nil
	}
	// リンクのラベルの中では引用にしない (#3300)。mfm-js の inline には quote が
	// 無いので、ラベルの中の `> ` は文字のまま。部分の state には inLink が
	// 渡らないので、引用にするとラベルの中でメンション・ハッシュタグ・URL・
	// 入れ子のリンクの判定を抜けてしまう。
	if s.inLink {
		return nil
	}
	// 行頭もしくはテキスト先頭のみ。mfm-js の lineBegin は CR の直後も行頭とみなす
	if !s.atLineBegin() {
		return nil
	}
	if !s.hasPrefix(">") {
		return nil
	}
	run, offset, ok := s.memo.quoteAt(s.src, s.pos)
	if !ok {
		return nil
	}
	s.budget.used++
	save := s.pos
	s.pos = run.end
	children := s.nest(func() []*Node {
		sub := &state{src: run.inner, pos: offset, depth: s.depth, fullDepth: s.depth, nestLimit: s.nestLimit, memo: run.table, budget: s.budget}
		return sub.parseNodes(false)
	})
	if children == nil {
		s.pos = save
		return nil
	}
	return withChildren(NodeQuote, mergeText(children))
}

func (s *state) tryCodeBlock() *Node {
	// mfm-js の block 構文 (quote / codeBlock / mathBlock / search) は full でだけ
	// 読み、inline (装飾や center の子) には無い。子で読むと、閉じのある装飾も
	// 閉じを block に飲まれて丸ごと文字になり、後ろのメンションやタグが落ちる (#3301)。
	if s.depth != s.fullDepth {
		return nil
	}
	// mfm-js 0.26.0 の codeBlock:
	//
	//	seq(newLine.option(), lineBegin, mark, (notMatch(newLine) char)*, newLine,
	//	    (notMatch(seq(newLine, mark, lineEnd)) char)+, newLine, mark, lineEnd,
	//	    newLine.option())
	//
	// 前の改行は tryBlock が読む。改行は CRLF / CR / LF のどれでもよく、中身は
	// 1 文字以上要る (` ```⏎⏎``` ` は文字)。閉じの ``` の直後は行の終わりでなければ
	// ならない。以前は LF だけを見て、閉じの後ろの同じ行の文字も飲み込み、中身が
	// 空でも受け付けていた。
	if !s.atLineBegin() || !s.hasPrefix("```") {
		return nil
	}
	save := s.pos
	langStart := s.pos + 3
	langEnd := len(s.src)
	if i := strings.IndexAny(s.src[langStart:], "\r\n"); i >= 0 {
		langEnd = langStart + i
	}
	s.budget.used += langEnd - langStart
	if langEnd == len(s.src) {
		return nil
	}
	lang := strings.TrimSpace(s.src[langStart:langEnd])
	codeStart := langEnd + s.newlineLenAt(langEnd)
	s.pos = codeStart
	closeAt, ok := s.codeBlockClose()
	if !ok || closeAt == codeStart {
		s.pos = save
		return nil
	}
	code := s.src[codeStart:closeAt]
	s.pos = closeAt + s.newlineLenAt(closeAt) + len("```")
	s.advance(s.newlineLen())
	props := map[string]any{"code": code}
	if lang != "" {
		props["lang"] = lang
	}
	return &Node{Type: NodeBlockCode, Props: props}
}

// codeBlockClose returns the first position at or after the current one where
// a newline, ``` and a line end follow. ok is false when there is none or the
// work budget runs out.
//
// 開きを並べて閉じを置かない入力 (```a⏎ の繰り返し) では、開きごとに末尾まで
// 探すと入力長の 2 乗になるので、閉じの位置の索引から引く。
func (s *state) codeBlockClose() (int, bool) {
	if c, ok := s.nextStop(stopCodeBlockClose); ok {
		return c, c < len(s.src)
	}
	for i := s.pos; i < len(s.src); i++ {
		if s.budget.used++; s.budget.exhausted() {
			return 0, false
		}
		if l := s.newlineLenAt(i); l > 0 && s.prefixAt(i+l, "```") && s.lineEndAt(i+l+3) {
			return i, true
		}
	}
	return 0, false
}

func (s *state) tryMathBlock() *Node {
	// mfm-js の block 構文 (quote / codeBlock / mathBlock / search) は full でだけ
	// 読み、inline (装飾や center の子) には無い。子で読むと、閉じのある装飾も
	// 閉じを block に飲まれて丸ごと文字になり、後ろのメンションやタグが落ちる (#3301)。
	if s.depth != s.fullDepth {
		return nil
	}
	if !s.hasPrefix("\\[") {
		return nil
	}
	save := s.pos
	s.advance(2)
	start := s.pos
	if end, ok := s.nextStop(stopMathBlockClose); ok {
		if end < len(s.src) {
			if formula := strings.TrimSpace(s.src[start:end]); formula != "" {
				s.pos = end + 2
				return withProp(NodeMathBlock, "formula", formula)
			}
		}
		s.pos = save
		return nil
	}
	for !s.eof() {
		if s.hasPrefix("\\]") {
			formula := strings.TrimSpace(s.src[start:s.pos])
			if formula == "" {
				break
			}
			s.advance(2)
			return withProp(NodeMathBlock, "formula", formula)
		}
		s.advance(utf8.RuneLen(s.peek()))
	}
	s.pos = save
	return nil
}

// tryCenterTag follows mfm-js 0.26.0's centerTag:
//
//	seq(newLine.option(), lineBegin, open, newLine.option(),
//	    seq(notMatch(seq(newLine.option(), close)), nest(r.inline)).select(1).many(1),
//	    newLine.option(), close, lineEnd, newLine.option())
//
// center は full にだけあり inline には無いので、装飾やリンクのラベルの子では
// 読まない (#3328)。行の先頭で始まり閉じの直後が行の終わりのときだけ読み、
// 開きの前・開きの直後・閉じの直前・閉じの直後の改行を 1 つずつ飲み込む。
// 以前は <b> などと同じく行の途中でも読んだので、`x <center>a</center>` が
// 連合へ送る HTML で中央寄せになり、IsSimple も false になっていた。
func (s *state) tryCenterTag() *Node {
	const openTag, closeTag = "<center>", "</center>"
	// 装飾やリンクのラベルの子は fullDepth より深いので、ここで外れる。深さの
	// 上限の位置は parseOne が 1 文字ずつ文字として読むので、ここへ来ない
	if s.depth != s.fullDepth {
		return nil
	}
	save := s.pos
	s.advance(s.newlineLen())
	if !s.atLineBegin() || !s.hasPrefix(openTag) {
		s.pos = save
		return nil
	}
	s.advance(len(openTag))
	s.advance(s.newlineLen())
	start := s.pos

	var children []*Node
	if s.depth+1 < s.nestLimit {
		s.depth++
		end := s.scanEnd(scanCenter)
		// 閉じの直後が行の終わりかを、子を集める前に確かめる。集めてから捨てると、
		// 開きを並べて最後だけ行末でない閉じを置いた入力で、開きの数だけ末尾まで
		// 読み直して入力長の 2 乗になる。
		if end > start && s.centerCloseAt(end) && s.lineEndAt(end+s.newlineLenAt(end)+len(closeTag)) {
			children = s.collectTo(end)
		}
		s.depth--
	} else if end := s.centerTextEnd(); end > start && s.centerCloseAt(end) {
		// 子の深さが上限に届くと、mfm-js の nest は子を 1 文字ずつ文字として読む
		children = []*Node{Text(s.src[start:end])}
		s.pos = end
	}
	if children == nil {
		s.pos = save
		return nil
	}
	s.advance(s.newlineLen())
	s.advance(len(closeTag))
	if !s.eof() && s.peek() != '\n' && s.peek() != '\r' {
		s.pos = save
		return nil
	}
	s.advance(s.newlineLen())
	return withChildren(NodeCenter, mergeText(children))
}

// centerCloseAt reports whether an optional newline and </center> follow pos.
func (s *state) centerCloseAt(pos int) bool {
	return s.prefixAt(pos+s.newlineLenAt(pos), "</center>")
}

// centerTextEnd returns where the children of a center whose children are
// read as plain text stop: the first </center>, or the newline right before
// it. Without a </center> it returns a position where centerCloseAt fails.
func (s *state) centerTextEnd() int {
	c, ok := s.nextStop(stopCenterClose)
	if !ok {
		c = len(s.src)
		if i := strings.Index(s.remaining(), "</center>"); i >= 0 {
			c = s.pos + i
		}
	}
	switch {
	case c-2 >= s.pos && s.src[c-2:c] == "\r\n":
		return c - 2
	case c-1 >= s.pos && (s.src[c-1] == '\n' || s.src[c-1] == '\r'):
		return c - 1
	}
	return c
}

// newlineLen returns the length of the newline at the current position.
func (s *state) newlineLen() int { return s.newlineLenAt(s.pos) }

// lineEndAt reports whether pos is the end of input or a line break.
func (s *state) lineEndAt(pos int) bool { return lineEndIn(s.src, pos) }

// lineEndIn reports mfm-js's lineEnd at pos in src: the end of src or CR or LF.
func lineEndIn(src string, pos int) bool {
	return pos >= len(src) || src[pos] == '\n' || src[pos] == '\r'
}

// newlineLenAt returns the length of the newline (CRLF, CR or LF, in
// mfm-js's order) at pos, or 0.
func (s *state) newlineLenAt(pos int) int { return newlineLenIn(s.src, pos) }

// newlineLenIn returns the length of mfm-js's newLine at pos in src (CRLF,
// CR or LF, tried in that order), or 0.
func newlineLenIn(src string, pos int) int {
	if pos < 0 || pos >= len(src) {
		return 0
	}
	switch src[pos] {
	case '\r':
		if pos+1 < len(src) && src[pos+1] == '\n' {
			return 2
		}
		return 1
	case '\n':
		return 1
	}
	return 0
}

// atLineBegin reports mfm-js's lineBegin: the start of the source or right
// after CR or LF.
func (s *state) atLineBegin() bool {
	return s.pos == 0 || s.src[s.pos-1] == '\n' || s.src[s.pos-1] == '\r'
}

func (s *state) trySmallTag() *Node {
	return s.tryWrapped("<small>", "</small>", NodeSmall, scanSmall)
}

func (s *state) tryPlainTag() *Node {
	if !s.hasPrefix("<plain>") {
		return nil
	}
	// mfm-js 0.26.0 の plainTag:
	//
	//	seq(open, newLine.option(),
	//	    (notMatch(seq(newLine.option(), close)) char)+.text(),
	//	    newLine.option(), close)
	//
	// 開きの直後と閉じの直前の改行を 1 つずつ中身から外し、中身は 1 文字以上要る
	// (`<plain></plain>` は文字)。以前は開きから閉じまでをそのまま中身にしていた。
	save := s.pos
	s.advance(len("<plain>"))
	s.advance(s.newlineLen())
	start := s.pos
	c, ok := s.nextStop(stopPlainClose)
	if !ok {
		c = len(s.src)
		if i := strings.Index(s.remaining(), "</plain>"); i >= 0 {
			c = s.pos + i
		}
		s.budget.used += c - s.pos
	}
	if c >= len(s.src) {
		s.pos = save
		return nil
	}
	end := c
	switch {
	case c-2 >= start && s.src[c-2:c] == "\r\n":
		end = c - 2
	case c-1 >= start && (s.src[c-1] == '\n' || s.src[c-1] == '\r'):
		end = c - 1
	}
	if end == start {
		s.pos = save
		return nil
	}
	s.pos = c + len("</plain>")
	return &Node{Type: NodePlain, Children: []*Node{Text(s.src[start:end])}}
}

func (s *state) tryBoldTag() *Node {
	return s.tryWrapped("<b>", "</b>", NodeBold, scanBold)
}

func (s *state) tryItalicTag() *Node {
	return s.tryWrapped("<i>", "</i>", NodeItalic, scanItalic)
}

func (s *state) tryStrikeTag() *Node {
	return s.tryWrapped("<s>", "</s>", NodeStrike, scanStrike)
}

// --- Inline parsers ---

// tryBig parses mfm-js's big `***...***`, which becomes `$[tada ...]`.
func (s *state) tryBig() *Node {
	n := s.tryWrapped("***", "***", NodeFn, scanBig)
	if n != nil && n.Type == NodeFn {
		n.Props = map[string]any{"name": "tada"}
	}
	return n
}

func (s *state) tryBoldAsta() *Node {
	return s.tryWrapped("**", "**", NodeBold, scanBoldAsta)
}

func (s *state) tryItalicAsta() *Node {
	if !s.hasPrefix("*") || s.hasPrefix("**") {
		return nil
	}
	// 直前が英数字なら失敗
	if s.pos > 0 && isAlphanumeric(s.prevRune()) {
		return nil
	}
	return s.tryWrappedAlphaSpace("*", "*", NodeItalic)
}

func (s *state) tryBoldUnder() *Node {
	if !s.hasPrefix("__") {
		return nil
	}
	return s.tryWrappedAlphaSpace("__", "__", NodeBold)
}

func (s *state) tryItalicUnder() *Node {
	if !s.hasPrefix("_") || s.hasPrefix("__") {
		return nil
	}
	if s.pos > 0 && isAlphanumeric(s.prevRune()) {
		return nil
	}
	return s.tryWrappedAlphaSpace("_", "_", NodeItalic)
}

func (s *state) tryStrikeWave() *Node {
	return s.tryWrapped("~~", "~~", NodeStrike, scanStrikeWave)
}

// tryWrapped parses open, children up to close, and close, like mfm-js's
// seqOrText(open, seq(notMatch(close), nest(inline)).select(1).many(1), close).
// When open matches but the rest does not, it returns what it read as a single
// text node. kind gives where the children stop.
//
// mfm-js の seqOrText は、開きが合って後ろが失敗すると、失敗した部品の手前まで
// 読んだ範囲 (開き + 中身) をまるごと文字として返し、その位置では他の構文を
// 試さない。中身は閉じか末尾 (~~ は改行) まで読むので、閉じが無ければ中の
// カスタム絵文字やハッシュタグも文字に飲み込まれる (#3301)。以前は 1 文字だけ
// 進めて読み直していたので、中の `:emoji:` や `#tag` を拾っていた。
func (s *state) tryWrapped(open, close string, nodeType NodeType, kind scanKind) *Node {
	if !s.hasPrefix(open) {
		return nil
	}
	save := s.pos
	s.advance(len(open))
	children, ok := s.wrappedBody(close, kind)
	if !ok {
		return Text(s.src[save:s.pos])
	}
	return withChildren(nodeType, children)
}

// wrappedBody reads mfm-js's `seq(notMatch(close), nest(inline)).many(1)` and
// then close from the current position. On success it moves past close. On
// failure it reports false with the position at the end of what seqOrText
// keeps as text: unchanged when there are no children, or right after them
// when close is missing.
func (s *state) wrappedBody(close string, kind scanKind) ([]*Node, bool) {
	if s.eof() || s.stopsAt(kind) {
		return nil, false
	}
	s.depth++
	end := s.scanEnd(kind)
	ok := s.prefixAt(end, close)
	var children []*Node
	if ok {
		children = s.collectTo(end)
	}
	s.depth--
	s.pos = end
	if !ok {
		return nil, false
	}
	s.advance(len(close))
	return mergeText(children), true
}

// tryWrappedAlphaSpace parses mfm-js's `seq(mark, alt([alphaAndNum,
// space]).many(1), mark)`: one or more ASCII letters, digits or spaces
// (U+0020, U+3000 or a tab) between open and close.
//
// 以前は閉じまでの中身を unicode.IsSpace で確かめていたので、mfm-js の space に
// 無い CR・NBSP・垂直タブなども中身にできた (`*a\rb*` が斜体になった)。
func (s *state) tryWrappedAlphaSpace(open, close string, nodeType NodeType) *Node {
	start := s.pos + len(open)
	i := start
	for i < len(s.src) {
		if c := s.src[i]; isAlphanumeric(rune(c)) || c == ' ' || c == '\t' {
			i++
			continue
		}
		if strings.HasPrefix(s.src[i:], "\u3000") {
			i += len("\u3000")
			continue
		}
		break
	}
	s.budget.used += i - start
	if i == start || !s.prefixAt(i, close) {
		return nil
	}
	s.pos = i + len(close)
	return withChildren(nodeType, []*Node{Text(s.src[start:i])})
}

func (s *state) tryInlineCode() *Node {
	if s.peek() != '`' || s.hasPrefix("```") {
		return nil
	}
	save := s.pos
	s.advance(1)
	start := s.pos
	for !s.eof() {
		ch := s.peek()
		if ch == '`' {
			code := s.src[start:s.pos]
			if code == "" {
				break
			}
			s.advance(1)
			return withProp(NodeInlineCode, "code", code)
		}
		// mfm-js の newLine は CR でも止まる
		if ch == '\n' || ch == '\r' || ch == 0xb4 { // ´ acute accent
			break
		}
		s.advance(utf8.RuneLen(ch))
	}
	s.pos = save
	return nil
}

func (s *state) tryMathInline() *Node {
	if !s.hasPrefix("\\(") {
		return nil
	}
	save := s.pos
	s.advance(2)
	start := s.pos
	if end, ok := s.nextStop(stopMathInline); ok {
		if end > start && s.prefixAt(end, "\\)") {
			s.pos = end + 2
			return withProp(NodeMathInline, "formula", s.src[start:end])
		}
		s.pos = save
		return nil
	}
	for !s.eof() {
		if s.hasPrefix("\\)") {
			formula := s.src[start:s.pos]
			if formula == "" {
				break
			}
			s.advance(2)
			return withProp(NodeMathInline, "formula", formula)
		}
		if s.peek() == '\n' || s.peek() == '\r' {
			break
		}
		s.advance(utf8.RuneLen(s.peek()))
	}
	s.pos = save
	return nil
}

func (s *state) tryFn() *Node {
	if !s.hasPrefix("$[") {
		return nil
	}
	// mfm-js の fn は seqOrText("$[", 関数名, 引数.option(), " ", 中身, "]") で、
	// 途中の部品が失敗すると、そこまで読んだ範囲を文字にする (tryWrapped と同じ)。
	// 文字にする範囲を合わせるため、関数名と引数は mfm-js の正規表現が読む
	// 範囲だけを読む。
	save := s.pos
	s.advance(2)
	name := s.readFnWord(isFnNameChar)
	if name == "" {
		return Text(s.src[save:s.pos])
	}
	args := s.readFnArgs()
	if s.peek() != ' ' {
		return Text(s.src[save:s.pos])
	}
	s.advance(1)
	children, ok := s.wrappedBody("]", scanFn)
	if !ok {
		return Text(s.src[save:s.pos])
	}

	props := map[string]any{"name": name}
	if args != nil {
		props["args"] = args
	}
	return &Node{Type: NodeFn, Props: props, Children: children}
}

// isFnNameChar reports whether r is in mfm-js's /[a-z0-9_]/i, used for fn
// names and argument keys.
func isFnNameChar(r rune) bool {
	return isASCIIAlphanumeric(r) || r == '_'
}

// isFnArgValueChar reports whether r is in mfm-js's /[a-z0-9_.-]/i, used for
// fn argument values.
func isFnArgValueChar(r rune) bool {
	return isFnNameChar(r) || r == '.' || r == '-'
}

// readFnWord reads the longest run of runes satisfying ok and returns it.
func (s *state) readFnWord(ok func(rune) bool) string {
	start := s.pos
	for !s.eof() && ok(s.peek()) {
		s.advance(1) // ok は ASCII だけを受け付ける
	}
	return s.src[start:s.pos]
}

// readFnArgs reads mfm-js's fn arguments `.k=v,k2` at the current position.
// It returns nil, leaving the position unchanged, when there is no "." or no
// valid first key. Otherwise it stops after the last complete argument, so a
// trailing "," or "=" without a valid key or value is left unread, as in
// mfm-js's arg.sep(",", 1) and seq("=", value).option().
func (s *state) readFnArgs() map[string]any {
	if s.peek() != '.' {
		return nil
	}
	save := s.pos
	s.advance(1)
	if !isFnNameChar(s.peek()) {
		s.pos = save
		return nil
	}
	args := map[string]any{}
	for {
		key := s.readFnWord(isFnNameChar)
		args[key] = true
		if s.peek() == '=' {
			eq := s.pos
			s.advance(1)
			if v := s.readFnWord(isFnArgValueChar); v != "" {
				args[key] = v
			} else {
				s.pos = eq
			}
		}
		if s.peek() != ',' {
			return args
		}
		// "," の後ろに項目が無ければ、"," を読まずに終わる
		comma := s.pos
		s.advance(1)
		if !isFnNameChar(s.peek()) {
			s.pos = comma
			return args
		}
	}
}

func (s *state) tryMention() *Node {
	if s.peek() != '@' {
		return nil
	}
	// リンクのラベルの中ではメンションにしない (mfm-js の notLinkLabel)。
	if s.inLink {
		return nil
	}
	// 直前が英数字なら失敗
	if s.pos > 0 && isAlphanumeric(s.prevRune()) {
		return nil
	}
	save := s.pos
	s.advance(1) // skip @
	username := s.consumeIdent()
	if username == "" {
		s.pos = save
		return nil
	}
	var host string
	hasHost := false
	if !s.eof() && s.peek() == '@' {
		hostStart := s.pos
		s.advance(1)
		if h := s.consumeIdent(); h != "" {
			host, hasHost = h, true
		} else {
			// @user@ のようなパターンはホスト無しに戻す
			s.pos = hostStart
		}
	}
	end := s.pos

	// 以下は mfm-js 0.26.0 の mention と同じ判定 (#3300)。
	//   - host の末尾の `.` / `-` は削る。削って空になったら不正
	//   - username の末尾の `.` / `-` は、host が無いときだけ削る。host があれば不正
	//   - username / host が `.` / `-` で始まれば不正
	// 不正なら読んだ範囲をまるごと文字にする (mfm-js は invalidMention で
	// input.slice(index, resultIndex) を返す)。正しければ `@name@host` の長さだけ
	// 進め、削った `.` / `-` は後ろの文字として残す。以前は削った分の位置を
	// 戻していなかったので、`@a. hi` の `.` が出力から消えていた。
	invalid := false
	if hasHost {
		if trimmed := strings.TrimRight(host, ".-"); len(trimmed) != len(host) {
			host = trimmed
			if host == "" {
				invalid = true
				hasHost = false
			}
		}
	}
	if trimmed := strings.TrimRight(username, ".-"); len(trimmed) != len(username) {
		if !hasHost {
			username = trimmed
		} else {
			invalid = true
		}
	}
	if username == "" || strings.HasPrefix(username, ".") || strings.HasPrefix(username, "-") {
		invalid = true
	}
	if hasHost && (strings.HasPrefix(host, ".") || strings.HasPrefix(host, "-")) {
		invalid = true
	}
	if invalid {
		return Text(s.src[save:end])
	}

	props := map[string]any{"username": username}
	acct := "@" + username
	if hasHost {
		props["host"] = host
		acct += "@" + host
	}
	props["acct"] = acct
	s.pos = save + len(acct)
	return &Node{Type: NodeMention, Props: props}
}

func (s *state) consumeIdent() string {
	start := s.pos
	for !s.eof() {
		ch := s.peek()
		if isASCIIAlphanumeric(ch) || ch == '_' || ch == '-' || ch == '.' {
			s.advance(1)
			continue
		}
		break
	}
	return s.src[start:s.pos]
}

func (s *state) tryHashtag() *Node {
	if s.peek() != '#' {
		return nil
	}
	// リンクのラベルの中ではハッシュタグにしない (mfm-js の notLinkLabel、#3300)。
	if s.inLink {
		return nil
	}
	if s.pos > 0 && isAlphanumeric(s.prevRune()) {
		return nil
	}
	save := s.pos
	start := s.pos + 1 // skip #

	// mfm-js の hashtag と同じく、使える文字の並びと、閉じた括弧 (`()` / `[]` /
	// `「」` / `（）`) だけを読む (#3318)。以前は `/` や `【】` も読み、閉じていない
	// 括弧も読み進め、入れ子の上限も無かったので、`#tag/foo` を 1 つのタグにしていた。
	end := s.hashtagItems(start, s.depth, true)
	s.budget.used += end - start
	tag := s.src[start:end]
	if tag == "" {
		s.pos = save
		return nil
	}
	// 数字だけのハッシュタグは無効
	if isDigitsOnly(tag) {
		s.pos = save
		return nil
	}
	s.pos = end
	return &Node{Type: NodeHashtag, Props: map[string]any{"hashtag": tag}}
}

// isHashtagChar reports whether r is one of mfm-js's hashTagChar: anything but
// the stop characters ` \u3000\t.,!?'"#:/[]【】()「」（）<>` and line breaks.
func isHashtagChar(r rune) bool {
	switch r {
	case ' ', '\u3000', '\t', '\r', '\n', '.', ',', '!', '?', '\'', '"', '#', ':', '/',
		'[', ']', '【', '】', '(', ')', '「', '」', '（', '）', '<', '>':
		return false
	}
	return true
}

// hashtagOpenClose maps the brackets mfm-js's hashtag reads as a group to
// their closing bracket.
var hashtagOpenClose = map[rune]rune{'(': ')', '[': ']', '「': '」', '（': '）'}

// hashtagItems reads mfm-js's `innerItem.many(0)` of the hashtag rule from i
// and returns where it stopped. depth is the nest depth the items are read at;
// top marks the hashtag's own items, which are not wrapped in nest.
func (s *state) hashtagItems(i, depth int, top bool) int {
	for {
		next, ok := s.hashtagItem(i, depth, top)
		if !ok {
			return i
		}
		i = next
	}
}

// hashtagItem reads one mfm-js hashtag innerItem at i: a balanced bracket group
// whose contents are read one nest level deeper, or a single hashtag character.
//
// 括弧の中は mfm-js の nest(innerItem, hashTagChar) で、深さを 1 つ上げてから
// 上限未満なら innerItem を、届いていれば hashTagChar だけを読む (括弧は
// hashTagChar に無いので、そこで止まる)。タグ直下の項目は nest を通らないので、
// 上限に関係なく括弧を開ける。閉じが無ければ括弧ごと失敗し、タグはその手前で終わる。
func (s *state) hashtagItem(i, depth int, top bool) (int, bool) {
	if i >= len(s.src) {
		return i, false
	}
	r, size := utf8.DecodeRuneInString(s.src[i:])
	if isHashtagChar(r) {
		return i + size, true
	}
	closeCh, ok := hashtagOpenClose[r]
	if !ok {
		return i, false
	}
	if !top && depth >= s.nestLimit {
		return i, false
	}
	s.budget.used++
	j := s.hashtagItems(i+size, depth+1, false)
	if j >= len(s.src) {
		return i, false
	}
	c, csize := utf8.DecodeRuneInString(s.src[j:])
	if c != closeCh {
		return i, false
	}
	return j + csize, true
}

func (s *state) tryEmojiCode() *Node {
	if s.peek() != ':' {
		return nil
	}
	// 区切りは mfm-js 0.26.0 の emojiCode と同じく**閉じの `:` の直後**だけを見る
	// (#3297)。mfm-js の先頭側の `alt([lineBegin, side])` は今の位置 (`:` 自身) に
	// notMatch を掛けるだけで直前の文字を見ないので、`1:a:` や `@foo:a:` も絵文字に
	// なる。以前の mk-go は逆に直前の文字を見ていた。
	save := s.pos
	s.advance(1) // skip :
	start := s.pos
	for !s.eof() {
		ch := s.peek()
		if ch == ':' {
			name := s.src[start:s.pos]
			if name == "" {
				break
			}
			// 英数字+_+-のみ
			if !isEmojiName(name) {
				break
			}
			s.advance(1)
			// 閉じの直後が英数字なら絵文字にしない (mfm-js の `alt([lineEnd, side])`)。
			if !s.eof() && isAlphanumeric(s.peek()) {
				break
			}
			return withProp(NodeEmojiCode, "name", name)
		}
		if ch == '\n' || unicode.IsSpace(ch) {
			break
		}
		s.advance(utf8.RuneLen(ch))
	}
	s.pos = save
	return nil
}

func (s *state) tryURL() *Node {
	if !(s.hasPrefix("https://") || s.hasPrefix("http://")) {
		return nil
	}
	// リンクのラベルの中では URL にしない (mfm-js の notLinkLabel、#3300)。
	if s.inLink {
		return nil
	}
	save := s.pos
	start := s.pos

	// プロトコル部分を消費
	schemeLen := len("http://")
	if s.hasPrefix("https://") {
		schemeLen = len("https://")
	}
	s.advance(schemeLen)
	if s.eof() {
		s.pos = save
		return nil
	}

	// URL の文字を読む。mfm-js の url と同じく、使える文字は ASCII の
	// [.,a-z0-9_/:%#@$&?!~=+-] で、`(...)` / `[...]` は閉じているときだけ含める
	// (#3302)。以前は空白と一部の記号以外を何でも含めたので、`https://e.x/p:あ`
	// のように URL の後ろに続く文字まで URL にしていた。
	end := s.urlItems(start+schemeLen, s.depth, true)
	// 読んだ分を仕事量に数える。以前は advance が 1 バイトずつ数えていた。
	s.budget.used += end - start
	if end == start+schemeLen {
		// scheme の後に 1 文字も無い (mfm-js の innerItem.many(1) が失敗する)。
		s.pos = save
		return nil
	}
	url := s.src[start:end]
	// 末尾の `.` / `,` は削り、後ろの文字として残す。削って scheme だけになったら、
	// mfm-js は読んだ範囲をまるごと文字にする。
	trimmed := strings.TrimRight(url, ".,")
	if len(trimmed) <= schemeLen {
		s.pos = end
		return Text(url)
	}
	s.pos = start + len(trimmed)
	return withProp(NodeURL, "url", trimmed)
}

// isURLChar reports whether b is one of mfm-js's url characters
// ([.,a-z0-9_/:%#@$&?!~=+-], case-insensitive).
func isURLChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte(".,_/:%#@$&?!~=+-", b) >= 0
}

// urlItems reads mfm-js's `innerItem.many(0)` of the url rule from i and
// returns where it stopped. depth is the nest depth the items are read at;
// top marks the url's own items, which are not wrapped in nest.
func (s *state) urlItems(i, depth int, top bool) int {
	for {
		next, ok := s.urlItem(i, depth, top)
		if !ok {
			return i
		}
		i = next
	}
}

// urlItem reads one mfm-js url innerItem at i: a balanced `(...)` or `[...]`
// whose contents are read one nest level deeper, or a single url character.
//
// mfm-js の nest(innerItem, urlChar) は深さを 1 つ上げてから、上限未満なら
// innerItem を、届いていれば urlChar だけを読む (括弧は urlChar に無いので、そこで
// 止まる)。url 直下の項目は nest を通らないので、上限に関係なく括弧を開ける。
// 閉じが無ければ括弧ごと失敗し、URL はその手前で終わる。
func (s *state) urlItem(i, depth int, top bool) (int, bool) {
	if i >= len(s.src) {
		return i, false
	}
	c := s.src[i]
	if isURLChar(c) {
		return i + 1, true
	}
	var closeCh byte
	switch c {
	case '(':
		closeCh = ')'
	case '[':
		closeCh = ']'
	default:
		return i, false
	}
	if !top && depth >= s.nestLimit {
		return i, false
	}
	s.budget.used++
	j := s.urlItems(i+1, depth+1, false)
	if j >= len(s.src) || s.src[j] != closeCh {
		return i, false
	}
	return j + 1, true
}

func (s *state) tryLink() *Node {
	// ?[label](url) or [label](url)
	silent := false
	if s.hasPrefix("?[") {
		silent = true
	} else if s.peek() != '[' {
		return nil
	}
	save := s.pos
	if silent {
		s.advance(2)
	} else {
		s.advance(1)
	}

	// label (] まで。途中の改行で失敗)。mfm-js はラベルを nest(labelInline) で
	// 読むので、ラベルの中身は 1 段深い。深さを変えずに読むと、ラベルの中で
	// 入れ子にできる段数が mfm-js より 1 段多くなる
	oldInLink := s.inLink
	s.inLink = true
	s.depth++
	labelStart := s.pos
	end := s.scanEnd(scanLinkLabel)
	// ラベルは mfm-js の many(1) で 1 つ以上要る。空のラベル (`[](https://...)`) は
	// リンクにせず、`[` を文字にして読み進める (以前は空の <a> を出していた)
	ok := end > labelStart && s.prefixAt(end, "](")
	var labelNodes []*Node
	if ok {
		labelNodes = s.collectTo(end)
	}
	s.depth--
	s.inLink = oldInLink
	if !ok {
		s.pos = save
		return nil
	}
	s.advance(2) // skip ](

	// 飛び先は mfm-js と同じく URL (`https?://...` か `<https?://...>`) に限り、
	// 直後に `)` が要る (#3300)。以前は `)` までを何でも受け付けたので、
	// `[@a](x)` もリンクになり、ラベルの中の判定 (inLink) が本家と食い違った。
	url, end, ok := s.linkTargetAt(s.pos)
	if !ok || !s.prefixAt(end, ")") {
		s.pos = save
		return nil
	}
	s.pos = end + 1 // skip )

	props := map[string]any{"url": url, "silent": silent}
	return &Node{Type: NodeLink, Props: props, Children: mergeText(labelNodes)}
}

// trySearch reads mfm-js's search: at a line begin, at least one character
// followed by a space (U+0020, U+3000 or a tab), a button (`検索`, `search`,
// `[検索]` or `[search]`, ASCII case-insensitive) and the line end. tryBlock
// reads the newlines before and after it.
//
// query は語の前の文字列をそのまま (前後の空白も削らずに) 使い、content は
// mfm-js と同じく query + 区切りの 1 文字 + ボタンの文字 (`[検索]` なら括弧も)
// にする。content は HTML のリンクの文字に使う (#3327)。
//
// mfm-js の inline (装飾などの中身) には search が無いので、full で読む位置
// だけで試す。以前は行を TrimSpace して語で終わるかだけを見ていたので、
// `q検索` や `q 検索 ` も検索にしていた。行頭の位置で試していた間は、行頭の
// ハッシュタグなどが先に読まれるので表に出にくかったが、改行の位置から試すと
// 食い違う (#3325)。
func (s *state) trySearch() *Node {
	if !s.fullContext() || !s.atLineBegin() {
		return nil
	}
	// 語の前に 1 文字以上要る。行が区切りと語だけ (` 検索`) のときは、1 文字目を
	// 読んだ後に区切りが残らないので、下のループで自然に失敗する
	start := s.pos
	i := start
	defer func() { s.budget.used += i - start }()
	for i < len(s.src) {
		if c := s.src[i]; c == '\n' || c == '\r' {
			break
		}
		_, size := utf8.DecodeRuneInString(s.src[i:])
		i += size
		if end, ok := s.searchButtonAt(i); ok {
			s.pos = end
			return &Node{Type: NodeSearch, Props: map[string]any{
				"query":   s.src[start:i],
				"content": s.src[start:end],
			}}
		}
	}
	return nil
}

// searchButtonAt reports whether a space, a search button and the line end
// follow at pos, and returns the position of the line end.
func (s *state) searchButtonAt(pos int) (int, bool) {
	switch {
	case s.prefixAt(pos, " "), s.prefixAt(pos, "\t"):
		pos++
	case s.prefixAt(pos, "\u3000"):
		pos += len("\u3000")
	default:
		return 0, false
	}
	var end int
	if s.prefixAt(pos, "[") {
		end = searchWordEnd(s.src, pos+1)
		if end < 0 || !s.prefixAt(end, "]") {
			return 0, false
		}
		end++
	} else if end = searchWordEnd(s.src, pos); end < 0 {
		return 0, false
	}
	if !s.lineEndAt(end) {
		return 0, false
	}
	return end, true
}

// searchWordEnd returns the position after `検索` or ASCII case-insensitive
// `search` at pos, or -1.
func searchWordEnd(src string, pos int) int {
	if strings.HasPrefix(src[pos:], "検索") {
		return pos + len("検索")
	}
	const word = "search"
	if len(src)-pos < len(word) {
		return -1
	}
	for i := 0; i < len(word); i++ {
		if c := src[pos+i]; c != word[i] && c != word[i]-'a'+'A' {
			return -1
		}
	}
	return pos + len(word)
}

// --- Helpers ---

func (s *state) prevRune() rune {
	if s.pos <= 0 {
		return 0
	}
	r, _ := utf8.DecodeLastRuneInString(s.src[:s.pos])
	return r
}

func isAlphanumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isASCIIAlphanumeric(r rune) bool {
	return isAlphanumeric(r)
}

func isDigitsOnly(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func isEmojiName(s string) bool {
	for _, r := range s {
		if !isASCIIAlphanumeric(r) && r != '_' && r != '+' && r != '-' {
			return false
		}
	}
	return len(s) > 0
}

// mergeText combines adjacent text nodes.
//
// 入力のノードは書き換えない。parseOne のメモは同じノードを複数の試行へ返すので、
// 前のテキストノードへ連結する形だと別の試行が持つノードまで書き換わる。連続する
// テキストは 1 つの Builder にまとめる (1 文字ずつ連結すると長さの 2 乗になる)。
func mergeText(nodes []*Node) []*Node {
	if len(nodes) == 0 {
		return nil
	}
	var result []*Node
	var buf strings.Builder
	inText := false
	flush := func() {
		if inText {
			result = append(result, Text(buf.String()))
			buf.Reset()
			inText = false
		}
	}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Type == NodeText {
			buf.WriteString(n.textValue())
			inText = true
			continue
		}
		flush()
		result = append(result, n)
	}
	flush()
	return result
}
