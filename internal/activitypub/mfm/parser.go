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

// slotMemo holds the pages of one (depth, inLink) slot. A page covers
// memoPageSize positions, or fewer at the end of a short source.
type slotMemo struct {
	one  [][]memoEntry
	scan [numScanKinds][][]int32
}

// memoTable caches parse results for one source string. Each run of quote
// lines is parsed as a separate string with its own table (see quoteAt).
//
// parseOne の結果 (ノードと終了位置) は src 上の位置・深さ・link ラベルの中か
// どうかだけで決まる (nestLimit と simple は 1 回の Parse の間は変わらない)。
// 深さは nestLimit による打ち切りで、inLink は link を試すかどうかで結果を
// 変えるので、どちらも表を分ける軸にする。病的な入力では全ての深さの全ての
// 位置を読むので、hash map ではなく位置で引くページ単位の配列にしてある
// (map だと実測で時間の過半が hash に消えた)。ページは触れたものだけ確保する。
type memoTable struct {
	srcLen    int
	nestLimit int
	budget    *workBudget
	slots     []*slotMemo // [slot(depth, inLink)]
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
	// parenOpens / parenClose は link の URL を読むための括弧の対応。
	// parenOpens は '(' の位置 (昇順)、parenClose[i] はそれに対応する ')' の位置か
	// -1。parenState は stopsState と同じ意味。
	parenOpens []int32
	parenClose []int32
	parenState int8
}

// stopKind names a construct that reads raw text up to a delimiter.
type stopKind uint8

const (
	stopPlainClose     stopKind = iota // </plain>
	stopMathBlockClose                 // \]
	stopMathInline                     // \) or a newline
	stopFnArgValue                     // ',', ' ' or ']'
	numStopKinds
)

// stopList returns the sorted offsets in src where the scan of kind stops,
// building it on first use. ok is false when the list cannot be used.
//
// <plain> / \[ / \( / $[x.k=v の値は開きの直後から区切りまで 1 文字ずつ読み、
// 閉じなければ末尾まで読んで失敗する。開きを並べると開始位置ごとに同じ区間を
// 読み直すので入力長の 2 乗になり、深さごとにも繰り返す (3000 バイトの
// 「\[」の並びで仕事量 9000 万、ローカルの上限の入力でも仕事量の上限に届いた)。
// 止まる位置を 1 度だけ列挙しておき、二分探索で引く。
//
// 元のループは rune 単位で進み、不正なバイトでは utf8.RuneLen(RuneError) = 3 で
// 進む。区切りは全て ASCII なので、正しい UTF-8 ならバイト単位で探した最初の
// 出現と一致するが、不正な UTF-8 では一致しないので元のループに任せる。
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
	case stopMathInline:
		for i := 0; i < len(src); i++ {
			if src[i] == '\n' || strings.HasPrefix(src[i:], "\\)") {
				list = append(list, int32(i))
			}
		}
	case stopFnArgValue:
		for i := 0; i < len(src); i++ {
			if c := src[i]; c == ',' || c == ' ' || c == ']' {
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

// parenMatch returns the offset of the ')' that closes the '(' at open without
// crossing whitespace, or -1 when there is none. ok is false when the caller
// must fall back to reading rune by rune.
//
// link の URL は「](」の直後から括弧の深さを数えて対応する ')' まで読み、空白に
// 当たれば失敗する。「[a](」を並べると開始位置ごとに末尾まで読み直して入力長の
// 2 乗になる (3000 バイトで仕事量 2300 万) ので、空白で区切った範囲の中の括弧の
// 対応を 1 度だけ求めておく。URL の開始直前の '(' に対応する ')' が、元のループが
// 深さ 0 に戻る位置そのもの。
func (s *state) parenMatch(open int) (int, bool) {
	m := s.memo
	switch m.parenState {
	case -1:
		return 0, false
	case 0:
		m.parenState = -1
		if !utf8.ValidString(s.src) {
			return 0, false
		}
		var opens, closes, stack []int32
		for i, r := range s.src {
			switch {
			case r == '(':
				stack = append(stack, int32(len(opens)))
				opens = append(opens, int32(i))
				closes = append(closes, -1)
			case r == ')':
				if n := len(stack); n > 0 {
					closes[stack[n-1]] = int32(i)
					stack = stack[:n-1]
				}
			case unicode.IsSpace(r):
				stack = stack[:0]
			}
		}
		if !m.charge(4 * (cap(opens) + cap(closes) + cap(stack))) {
			return 0, false
		}
		m.parenOpens, m.parenClose, m.parenState = opens, closes, 1
	}
	s.budget.used++
	i := sort.Search(len(m.parenOpens), func(i int) bool { return int(m.parenOpens[i]) >= open })
	if i == len(m.parenOpens) || int(m.parenOpens[i]) != open {
		return -1, true
	}
	return int(m.parenClose[i]), true
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

// slotFor returns the pages of the slot for depth and inLink, allocating the
// slot on first use, or nil when the position is out of range or the
// allocation is refused.
//
// quote の中身は quote ごとに別の表を持つので、表の入れ物やページを入力の長さに
// 関係なく固定の大きさで確保すると、短い quote を並べただけで確保量が入力の
// 数千倍になる (3000 バイトで 13MB。1 表あたり入れ物 10KB、触れたスロットごとに
// 4KB のページ)。入れ物はスロット単位で、ページは src の長さまでに切り詰めて
// 確保し、どちらも charge に含める。
func (m *memoTable) slotFor(pos, depth int, inLink bool) *slotMemo {
	if depth < 0 || depth > m.nestLimit || pos < 0 || pos > m.srcLen {
		return nil
	}
	i := depth * 2
	if inLink {
		i++
	}
	if m.slots == nil {
		n := (m.nestLimit + 1) * 2
		if !m.charge(n * int(unsafe.Sizeof((*slotMemo)(nil)))) {
			return nil
		}
		m.slots = make([]*slotMemo, n)
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
// 1 つの表を全ての開始行と深さで共有する。表は深さをキーに含むので共有してよく、
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
	for p := 0; p < len(src); {
		e := strings.IndexByte(src[p:], '\n')
		if e < 0 {
			e = len(src)
		} else {
			e += p
		}
		next := min(e+1, len(src))
		if src[p] != '>' {
			flush(p)
			p = next
			continue
		}
		c := p + 1
		if c < len(src) && (src[c] == ' ' || src[c] == '\t') {
			c++
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
func (m *memoTable) oneEntry(pos, depth int, inLink bool) *memoEntry {
	sm := m.slotFor(pos, depth, inLink)
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
func (m *memoTable) scanEntry(pos, depth int, inLink bool, kind scanKind) *int32 {
	sm := m.slotFor(pos, depth, inLink)
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
// なる。ただし比例定数は深さ (最大 21 段) の分だけ大きく、閉じない <b> を 18 段
// 重ねた後ろに「~~$[x.a=b *」を並べると 1 バイトあたり約 240 になる (実測)。
// 見落とした経路があっても 1 回の Parse が際限なく走らないよう、安全網として
// 上限を置く。超えたら以降の未解析の位置は構文を試さずテキストとして読む。
//
// 基礎分はローカルの本文上限 (3000 文字) の入力が届かない大きさにしてある。
// 3000 バイトの病的な入力で実測した最大は約 71 万 (上の形) で、基礎分だけで
// その 3 倍ある。長さを切り詰めずに届くリモートの本文は、病的な形なら上限か
// メモ表の上限に届いて途中からテキストになる (通常の文章は 1 バイトあたり
// 10 未満なので届かない)。
const (
	workBudgetBase    = 1 << 21
	workBudgetPerByte = 32
	// memoByteLimit はメモ表が 1 回の Parse で確保してよい総量。3000 バイトの
	// 病的な入力の実測は最大 4MB 程度で、12KB を超える病的な入力はここに届く。
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
	// 深さ 0 かつ link ラベルの外で読む位置は最上位のループが 1 度ずつ読むだけで、
	// 読み直されることが無い (子のループは深さ 1 以上か link ラベルの中)。
	// 素のテキストで表を膨らませないよう記録しない
	if s.depth == 0 && !s.inLink {
		return s.parseFullOne()
	}
	// 閉じない <b> などは失敗するたびに 1 文字進めて、同じ位置を別の親から
	// 読み直す。メモ化しないと入力長に対して指数時間になる
	e := s.memo.oneEntry(s.pos, s.depth, s.inLink)
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
	if n := s.tryCodeBlock(); n != nil {
		return n
	}
	if n := s.tryInlineCode(); n != nil {
		return n
	}
	if n := s.tryQuote(); n != nil {
		return n
	}
	if n := s.tryMathBlock(); n != nil {
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
	if n := s.trySearch(); n != nil {
		return n
	}
	return s.consumeChar()
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
		return s.hasPrefix("</center>")
	case scanSmall:
		return s.hasPrefix("</small>")
	case scanBold:
		return s.hasPrefix("</b>")
	case scanItalic:
		return s.hasPrefix("</i>")
	case scanStrike:
		return s.hasPrefix("</s>")
	case scanBoldAsta:
		return s.hasPrefix("**") || s.peek() == '\n'
	case scanStrikeWave:
		return s.hasPrefix("~~") || s.peek() == '\n'
	case scanFn:
		return s.peek() == ']'
	case scanLinkLabel:
		return s.peek() == ']' || s.peek() == '\n'
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
		if e := s.memo.scanEntry(s.pos, s.depth, s.inLink, kind); e != nil && *e != 0 {
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
		if e := s.memo.scanEntry(p, s.depth, s.inLink, kind); e != nil {
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
	// 行頭もしくはテキスト先頭のみ
	if s.pos > 0 && s.src[s.pos-1] != '\n' {
		return nil
	}
	if !s.hasPrefix(">") {
		return nil
	}
	// 深さ上限では nest が失敗するので、塊を引かずに失敗する (結果は同じ)
	if s.depth >= s.nestLimit {
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
		sub := &state{src: run.inner, pos: offset, depth: s.depth, nestLimit: s.nestLimit, memo: run.table, budget: s.budget}
		return sub.parseNodes(false)
	})
	if children == nil {
		s.pos = save
		return nil
	}
	return withChildren(NodeQuote, mergeText(children))
}

func (s *state) tryCodeBlock() *Node {
	if s.pos > 0 && s.src[s.pos-1] != '\n' {
		return nil
	}
	if !s.hasPrefix("```") {
		return nil
	}
	save := s.pos
	s.advance(3) // skip ```
	// optional lang
	langStart := s.pos
	for !s.eof() && s.peek() != '\n' {
		s.advance(utf8.RuneLen(s.peek()))
	}
	lang := strings.TrimSpace(s.src[langStart:s.pos])
	if s.eof() {
		s.pos = save
		return nil
	}
	s.advance(1) // skip \n

	codeStart := s.pos
	for !s.eof() {
		if s.hasPrefix("\n```") {
			code := s.src[codeStart:s.pos]
			s.advance(4) // skip \n```
			// 行末まで消費 (改行 or EOF)
			for !s.eof() && s.peek() != '\n' {
				s.advance(1)
			}
			if !s.eof() {
				s.advance(1)
			}
			props := map[string]any{"code": code}
			if lang != "" {
				props["lang"] = lang
			}
			return &Node{Type: NodeBlockCode, Props: props}
		}
		s.advance(utf8.RuneLen(s.peek()))
	}
	s.pos = save
	return nil
}

func (s *state) tryMathBlock() *Node {
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

func (s *state) tryCenterTag() *Node {
	return s.tryHTMLTag("<center>", "</center>", NodeCenter, scanCenter)
}

func (s *state) trySmallTag() *Node {
	return s.tryHTMLTag("<small>", "</small>", NodeSmall, scanSmall)
}

func (s *state) tryPlainTag() *Node {
	if !s.hasPrefix("<plain>") {
		return nil
	}
	save := s.pos
	s.advance(7) // <plain>
	start := s.pos
	if end, ok := s.nextStop(stopPlainClose); ok {
		if end < len(s.src) {
			s.pos = end + 8 // </plain>
			return &Node{Type: NodePlain, Children: []*Node{Text(s.src[start:end])}}
		}
		s.pos = save
		return nil
	}
	for !s.eof() {
		if s.hasPrefix("</plain>") {
			text := s.src[start:s.pos]
			s.advance(8) // </plain>
			return &Node{Type: NodePlain, Children: []*Node{Text(text)}}
		}
		s.advance(utf8.RuneLen(s.peek()))
	}
	s.pos = save
	return nil
}

func (s *state) tryBoldTag() *Node {
	return s.tryHTMLTag("<b>", "</b>", NodeBold, scanBold)
}

func (s *state) tryItalicTag() *Node {
	return s.tryHTMLTag("<i>", "</i>", NodeItalic, scanItalic)
}

func (s *state) tryStrikeTag() *Node {
	return s.tryHTMLTag("<s>", "</s>", NodeStrike, scanStrike)
}

// tryHTMLTag は <tag>...</tag> 形式のパースを試みる。children は再帰パースする。
func (s *state) tryHTMLTag(open, close string, nodeType NodeType, kind scanKind) *Node {
	if !s.hasPrefix(open) {
		return nil
	}
	save := s.pos
	s.advance(len(open))

	// 深さ上限を超えたときは中身を読まず、開きタグの直後がそのまま閉じタグか
	// だけを見る (空要素として成功しうる)
	var children []*Node
	if s.depth < s.nestLimit {
		s.depth++
		if end := s.scanEnd(kind); s.prefixAt(end, close) {
			children = s.collectTo(end)
		}
		s.depth--
	}
	if s.hasPrefix(close) {
		s.advance(len(close))
		return withChildren(nodeType, mergeText(children))
	}
	s.pos = save
	return nil
}

// --- Inline parsers ---

func (s *state) tryBoldAsta() *Node {
	if !s.hasPrefix("**") {
		return nil
	}
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
	if !s.hasPrefix("~~") {
		return nil
	}
	return s.tryWrapped("~~", "~~", NodeStrike, scanStrikeWave)
}

// tryWrapped は open...close で囲まれた部分を再帰パースする。
// children の途中に改行が来たら失敗する。
func (s *state) tryWrapped(open, close string, nodeType NodeType, kind scanKind) *Node {
	save := s.pos
	s.advance(len(open))
	if s.eof() || s.hasPrefix(close) || s.depth >= s.nestLimit {
		s.pos = save
		return nil
	}
	s.depth++
	end := s.scanEnd(kind)
	if !s.prefixAt(end, close) {
		s.depth--
		s.pos = save
		return nil
	}
	children := s.collectTo(end)
	s.depth--
	s.advance(len(close))
	return withChildren(nodeType, mergeText(children))
}

// tryWrappedAlphaSpace は英数字+空白のみを含むラップされた部分をパースする。
func (s *state) tryWrappedAlphaSpace(open, close string, nodeType NodeType) *Node {
	save := s.pos
	s.advance(len(open))
	start := s.pos
	for !s.eof() {
		if s.hasPrefix(close) {
			content := s.src[start:s.pos]
			if content == "" {
				break
			}
			// 英数字+空白のみか確認
			if !isAlphaSpaceOnly(content) {
				break
			}
			s.advance(len(close))
			return withChildren(nodeType, []*Node{Text(content)})
		}
		if s.peek() == '\n' {
			break
		}
		s.advance(utf8.RuneLen(s.peek()))
	}
	s.pos = save
	return nil
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
		if ch == '\n' || ch == 0xb4 { // ´ acute accent
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
		if s.peek() == '\n' {
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
	save := s.pos
	s.advance(2)
	// 関数名をパース
	nameStart := s.pos
	for !s.eof() {
		ch := s.peek()
		if isASCIIAlphanumeric(ch) || ch == '_' {
			s.advance(1)
			continue
		}
		break
	}
	name := s.src[nameStart:s.pos]
	if name == "" {
		s.pos = save
		return nil
	}

	// 引数をパース (ドットで始まる)
	var args map[string]any
	if !s.eof() && s.peek() == '.' {
		s.advance(1)
		args = s.parseFnArgs()
	}

	// スペース区切り
	if s.eof() || s.peek() != ' ' {
		s.pos = save
		return nil
	}
	s.advance(1)

	// children (] まで)。深さ上限を超えたときは中身を読まず、直後が ] かだけを見る
	var children []*Node
	if s.depth < s.nestLimit {
		s.depth++
		if end := s.scanEnd(scanFn); s.prefixAt(end, "]") {
			children = s.collectTo(end)
		}
		s.depth--
	}
	if !s.hasPrefix("]") {
		s.pos = save
		return nil
	}
	s.advance(1)

	props := map[string]any{"name": name}
	if args != nil {
		props["args"] = args
	}
	return &Node{Type: NodeFn, Props: props, Children: mergeText(children)}
}

func (s *state) parseFnArgs() map[string]any {
	args := map[string]any{}
	for !s.eof() {
		keyStart := s.pos
		for !s.eof() {
			ch := s.peek()
			if isASCIIAlphanumeric(ch) || ch == '_' {
				s.advance(1)
				continue
			}
			break
		}
		key := s.src[keyStart:s.pos]
		if key == "" {
			break
		}
		if !s.eof() && s.peek() == '=' {
			s.advance(1)
			valStart := s.pos
			if end, ok := s.nextStop(stopFnArgValue); ok {
				s.pos = end
			}
			for !s.eof() {
				ch := s.peek()
				if ch == ',' || ch == ' ' || ch == ']' {
					break
				}
				s.advance(utf8.RuneLen(ch))
			}
			args[key] = s.src[valStart:s.pos]
		} else {
			args[key] = true
		}
		if !s.eof() && s.peek() == ',' {
			s.advance(1)
			continue
		}
		break
	}
	return args
}

func (s *state) tryMention() *Node {
	if s.peek() != '@' {
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
	if !s.eof() && s.peek() == '@' {
		s.advance(1)
		host = s.consumeIdent()
		if host == "" {
			// @user@ のようなパターンはホスト無しに戻す
			s.pos -= 1 // @ を戻す
		}
	}
	// 末尾のドット・ハイフンを削る
	username = strings.TrimRight(username, ".-")
	host = strings.TrimRight(host, ".-")
	if username == "" {
		s.pos = save
		return nil
	}

	props := map[string]any{"username": username}
	acct := "@" + username
	if host != "" {
		props["host"] = host
		acct += "@" + host
	}
	props["acct"] = acct
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
	if s.pos > 0 && isAlphanumeric(s.prevRune()) {
		return nil
	}
	save := s.pos
	s.advance(1) // skip #

	start := s.pos
	s.consumeHashtagContent()
	tag := s.src[start:s.pos]
	if tag == "" {
		s.pos = save
		return nil
	}
	// 数字だけのハッシュタグは無効
	if isDigitsOnly(tag) {
		s.pos = save
		return nil
	}
	return &Node{Type: NodeHashtag, Props: map[string]any{"hashtag": tag}}
}

func (s *state) consumeHashtagContent() {
	for !s.eof() {
		ch := s.peek()
		// ハッシュタグで無効な文字
		if ch == '#' || unicode.IsSpace(ch) || ch == '.' || ch == ',' || ch == '!' ||
			ch == '?' || ch == '\'' || ch == '"' || ch == ':' || ch == '<' || ch == '>' {
			return
		}
		// 括弧のネスト
		switch ch {
		case '(':
			s.advance(1)
			s.consumeHashtagContent()
			if !s.eof() && s.peek() == ')' {
				s.advance(1)
			}
			continue
		case '[':
			s.advance(1)
			s.consumeHashtagContent()
			if !s.eof() && s.peek() == ']' {
				s.advance(1)
			}
			continue
		case '「':
			s.advance(utf8.RuneLen(ch))
			s.consumeHashtagContent()
			if !s.eof() && s.peek() == '」' {
				s.advance(utf8.RuneLen('」'))
			}
			continue
		case '（':
			s.advance(utf8.RuneLen(ch))
			s.consumeHashtagContent()
			if !s.eof() && s.peek() == '）' {
				s.advance(utf8.RuneLen('）'))
			}
			continue
		case ')', ']', '」', '）':
			return
		}
		s.advance(utf8.RuneLen(ch))
	}
}

func (s *state) tryEmojiCode() *Node {
	if s.peek() != ':' {
		return nil
	}
	// 境界チェック
	if s.pos > 0 && isAlphanumeric(s.prevRune()) {
		return nil
	}
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

func (s *state) tryUnicodeEmoji() *Node {
	rest := s.remaining()
	if len(rest) == 0 {
		return nil
	}
	r, size := utf8.DecodeRuneInString(rest)
	if r == utf8.RuneError {
		return nil
	}

	// 絵文字判定: Emoji/Symbol カテゴリ + FE0F (variation selector) の組み合わせ
	if !isEmojiStart(r) {
		return nil
	}

	// 連続する絵文字関連コードポイントを消費
	end := size
	for end < len(rest) {
		nextR, nextSize := utf8.DecodeRuneInString(rest[end:])
		if nextR == utf8.RuneError {
			break
		}
		if isEmojiContinuation(nextR) {
			end += nextSize
			continue
		}
		break
	}

	emoji := rest[:end]
	// FE0F 単体は絵文字ではない
	if emoji == "\ufe0f" {
		return nil
	}
	s.advance(end)
	return withProp(NodeUnicodeEmoji, "emoji", emoji)
}

func (s *state) tryURL() *Node {
	if !(s.hasPrefix("https://") || s.hasPrefix("http://")) {
		return nil
	}
	save := s.pos
	start := s.pos

	// プロトコル部分を消費
	if s.hasPrefix("https://") {
		s.advance(8)
	} else {
		s.advance(7)
	}
	if s.eof() {
		s.pos = save
		return nil
	}

	// URL文字を消費 (括弧ネスト対応)
	s.consumeURLChars()
	url := s.src[start:s.pos]
	// 末尾の句読点を削る
	url = strings.TrimRight(url, ".,")
	s.pos = start + len(url)

	if len(url) <= 8 { // プロトコルのみ
		s.pos = save
		return nil
	}
	return withProp(NodeURL, "url", url)
}

func (s *state) consumeURLChars() {
	for !s.eof() {
		ch := s.peek()
		if unicode.IsSpace(ch) || ch == '<' || ch == '>' || ch == '"' || ch == '\'' {
			return
		}
		switch ch {
		case '(':
			s.advance(1)
			s.consumeURLChars()
			if !s.eof() && s.peek() == ')' {
				s.advance(1)
			}
			continue
		case '[':
			s.advance(1)
			s.consumeURLChars()
			if !s.eof() && s.peek() == ']' {
				s.advance(1)
			}
			continue
		case ')', ']':
			return
		}
		s.advance(utf8.RuneLen(ch))
	}
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

	// label (] まで。途中の改行で失敗)
	oldInLink := s.inLink
	s.inLink = true
	end := s.scanEnd(scanLinkLabel)
	if !s.prefixAt(end, "](") {
		s.inLink = oldInLink
		s.pos = save
		return nil
	}
	labelNodes := s.collectTo(end)
	s.inLink = oldInLink
	s.advance(2) // skip ](

	urlStart := s.pos
	if end, ok := s.parenMatch(urlStart - 1); ok {
		if end <= urlStart {
			s.pos = save
			return nil
		}
		s.pos = end + 1
		props := map[string]any{"url": s.src[urlStart:end], "silent": silent}
		return &Node{Type: NodeLink, Props: props, Children: mergeText(labelNodes)}
	}
	parenDepth := 1
	for !s.eof() && parenDepth > 0 {
		ch := s.peek()
		if ch == '(' {
			parenDepth++
		} else if ch == ')' {
			parenDepth--
			if parenDepth == 0 {
				break
			}
		} else if ch == '\n' || unicode.IsSpace(ch) {
			s.pos = save
			return nil
		}
		s.advance(utf8.RuneLen(ch))
	}
	url := s.src[urlStart:s.pos]
	if url == "" || !s.hasPrefix(")") {
		s.pos = save
		return nil
	}
	s.advance(1) // skip )

	props := map[string]any{"url": url, "silent": silent}
	return &Node{Type: NodeLink, Props: props, Children: mergeText(labelNodes)}
}

func (s *state) trySearch() *Node {
	// 行頭から: "query 検索" / "query search" / "query [検索]" / "query [search]"
	if s.pos > 0 && s.src[s.pos-1] != '\n' {
		return nil
	}
	save := s.pos
	// 行末まで読む
	lineStart := s.pos
	for !s.eof() && s.peek() != '\n' {
		s.advance(utf8.RuneLen(s.peek()))
	}
	line := s.src[lineStart:s.pos]

	// 検索キーワードで終わるか確認
	trimmedLine := strings.TrimSpace(line)
	for _, suffix := range []string{" 検索", " search", " [検索]", " [search]", " Search", " SEARCH"} {
		trimmedSuffix := strings.TrimSpace(suffix)
		if strings.HasSuffix(trimmedLine, trimmedSuffix) {
			query := strings.TrimSpace(trimmedLine[:len(trimmedLine)-len(trimmedSuffix)])
			if query == "" {
				continue
			}
			if !s.eof() {
				s.advance(1) // skip \n
			}
			return &Node{Type: NodeSearch, Props: map[string]any{"query": query}}
		}
	}
	s.pos = save
	return nil
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

func isAlphaSpaceOnly(s string) bool {
	for _, r := range s {
		if !isAlphanumeric(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
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

// isEmojiStart は絵文字シーケンスの先頭文字として妥当か判定する。
// Unicode Emoji カテゴリの文字や、一般的な絵文字開始コードポイントを対象。
func isEmojiStart(r rune) bool {
	// 基本的な絵文字範囲
	if r >= 0x1F600 && r <= 0x1F64F { // Emoticons
		return true
	}
	if r >= 0x1F300 && r <= 0x1F5FF { // Misc Symbols and Pictographs
		return true
	}
	if r >= 0x1F680 && r <= 0x1F6FF { // Transport and Map
		return true
	}
	if r >= 0x1F700 && r <= 0x1F77F { // Alchemical Symbols
		return true
	}
	if r >= 0x1F780 && r <= 0x1F7FF { // Geometric Shapes Extended
		return true
	}
	if r >= 0x1F800 && r <= 0x1F8FF { // Supplemental Arrows-C
		return true
	}
	if r >= 0x1F900 && r <= 0x1F9FF { // Supplemental Symbols and Pictographs
		return true
	}
	if r >= 0x1FA00 && r <= 0x1FA6F { // Chess Symbols
		return true
	}
	if r >= 0x1FA70 && r <= 0x1FAFF { // Symbols and Pictographs Extended-A
		return true
	}
	if r >= 0x2600 && r <= 0x26FF { // Misc symbols
		return true
	}
	if r >= 0x2700 && r <= 0x27BF { // Dingbats
		return true
	}
	if r >= 0x231A && r <= 0x23F3 { // Misc Technical (watch, hourglass)
		return true
	}
	if r >= 0x2300 && r <= 0x23FF { // Misc Technical
		return true
	}
	if r >= 0x2B05 && r <= 0x2B55 { // Arrows, geometric
		return true
	}
	if r >= 0x200D && r <= 0x200D { // ZWJ
		return true
	}
	if r >= 0xFE00 && r <= 0xFE0F { // Variation selectors
		return true
	}
	if r == 0x203C || r == 0x2049 { // ‼ ⁉
		return true
	}
	if r == 0x20E3 { // Combining enclosing keycap
		return true
	}
	if r >= 0x2100 && r <= 0x214F { // Letterlike symbols (™ etc)
		return true
	}
	if r >= 0x2190 && r <= 0x21FF { // Arrows
		return true
	}
	// keycap base digits 0-9, *, #
	if r == '#' || r == '*' || (r >= '0' && r <= '9') {
		// keycapとして使われる可能性があるが、FE0Fが続かないと絵文字にならない
		return false
	}
	// Regional indicators
	if r >= 0x1F1E0 && r <= 0x1F1FF {
		return true
	}
	// © ®
	if r == 0xA9 || r == 0xAE {
		return true
	}
	return false
}

// isEmojiContinuation は絵文字シーケンスの継続文字として妥当か判定する。
func isEmojiContinuation(r rune) bool {
	if isEmojiStart(r) {
		return true
	}
	// ZWJ (Zero Width Joiner)
	if r == 0x200D {
		return true
	}
	// Variation selectors
	if r >= 0xFE00 && r <= 0xFE0F {
		return true
	}
	// Combining enclosing keycap
	if r == 0x20E3 {
		return true
	}
	// Skin tone modifiers
	if r >= 0x1F3FB && r <= 0x1F3FF {
		return true
	}
	// Tag characters (used in flag sequences)
	if r >= 0xE0020 && r <= 0xE007F {
		return true
	}
	return false
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
