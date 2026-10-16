// Package wordmute implements Misskey's word-mute matcher (`checkWordMute`)
// for hard mute filtering on the server side (#787). The wire format is the
// jsonb array stored in user_profile.mutedWords / hardMutedWords:
//
//	[
//	  ["a", "b"],           // AND group: text must contain every element (case-sensitive)
//	  "/regex/i"            // RegExp literal: parsed as Go regexp
//	]
//
// The judgement follows upstream frontend utility/check-word-mute.ts, which is
// the matcher upstream actually applies (backend misc/check-word-mute.ts has no
// caller in 2026.10.0): keywords are compared case-sensitively, and a string
// entry that is not a /pattern/flags literal never matches.
//
// Frontend sends and renders the same shape, so persistence layer never
// transforms the structure.
package wordmute

import (
	"encoding/json"
	"errors"
	"hash/maphash"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Limits on regex-literal entries.
//
// **Go の regexp は線形だが、定数が命令数に比例する。** 実行は Pike VM で
// O(命令数 × 本文長) なので、巨大なパターンほど 1 回の判定が重くなる。
// ハードミュートはタイムラインの取得 (ノートごと) とストリームの配信 (購読者
// ごとに同期) で評価するので、1 人の設定の重さが CPU とトピック全体の配信に効く。
// 上限は、普通の使い方 (単語を並べた選択で数十命令、`.{0,200}語` のような
// 幅の指定で数百命令) が収まる値。命令数は繰り返し (`{n,m}`) の幅に比例して増える。
//
// upstream は RE2 (`max_mem` 既定 8MiB) でコンパイルし、それを超える巨大な
// パターンはコンパイルできない。Elythia は Go の regexp なので、上限を自前で置く。
const (
	// maxRegexPatternBytes bounds the source text of one literal. 構文解析より
	// 前に見るので、巨大なパターンの解析とコンパイルのメモリも使わせない。
	maxRegexPatternBytes = 4096
	// maxRegexInsts bounds the compiled program of one hard-mute literal.
	maxRegexInsts = 512
	// maxRegexInstsTotal bounds the sum over one user's hard-mute list.
	maxRegexInstsTotal = 1024
)

// Match returns true if the given combined text should be muted by any of
// the rules encoded in `rulesJSON` (the raw jsonb bytes from user_profile).
// `rulesJSON` may be nil / empty / `null` / `[]` — all of those mean no rule.
//
// `text` is the already-normalized search target. Callers that want to mute
// based on note content should pass `cw + "\n" + text` (matching upstream's
// concatenation). When the text is empty after trim, no rule can hit so the
// function returns false fast.
func Match(rulesJSON []byte, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	rs := parse(rulesJSON)
	if rs == nil || len(rs.rules) == 0 {
		return false
	}
	if !rs.hasRegex {
		return matchRules(rs.rules, text)
	}
	// **正規表現を含む規則は結果を覚える。** 同じ利用者がタイムラインを取り直す
	// たびに、同じノートへ同じ正規表現を当て直さない。鍵は規則の集合と本文の
	// hash で、利用者ではない (同じ規則を持つ複数の利用者でも 1 回で済む)。
	key := matchKey{ruleSet: rs.id, text: maphash.String(matchSeed, text)}
	if hit, ok := matchMemo.Get(key); ok {
		return hit
	}
	hit := matchRules(rs.rules, text)
	matchMemo.Add(key, hit)
	return hit
}

// matchMemoSize bounds the remembered (rule set, text) results.
const matchMemoSize = 16384

// matchKey identifies one evaluation: a parsed rule set and a text hash.
//
// 本文そのものではなく 64bit の hash を鍵にする (本文は 8KB まであり、覚える
// 件数ぶん保持するのは重い)。seed はプロセスごとの乱数なので、外から衝突を
// 狙えない。
type matchKey struct {
	ruleSet uint64
	text    uint64
}

var (
	matchSeed = maphash.MakeSeed()
	matchMemo *lru.Cache[matchKey, bool]
)

func matchRules(rules []rule, text string) bool {
	// **大文字と小文字を区別する。** 本家の判定 (frontend の checkWordMute) は
	// `text.includes(keyword)` で比べる。区別しないと、本家のクライアントが
	// 表示するノートをサーバーだけが隠す (#3369)。区別しないで隠したいときは
	// `/word/i` の正規表現を使う (本家と同じ)。
	for _, r := range rules {
		switch v := r.(type) {
		case andRule:
			hit := true
			for _, kw := range v {
				if !strings.Contains(text, kw) {
					hit = false
					break
				}
			}
			if hit {
				return true
			}
		case regexRule:
			if v.re != nil && v.re.MatchString(text) {
				return true
			}
		}
	}
	return false
}

// rule is one parsed entry of the muted-words array.
type rule any

// andRule represents `["a", "b"]` — every element must appear in the text
// (case-sensitive, like upstream `text.includes`).
type andRule []string

// regexRule wraps a compiled `/foo/flag` literal. Compilation failure
// silently downgrades to "never matches" to mirror upstream's try/catch
// (caller should not fail an entire timeline because one entry is malformed).
type regexRule struct {
	re *regexp.Regexp
}

// regexLiteral matches Misskey's `/pattern/flags` wire syntax with the same
// shape upstream reads (`/^\/(.+)\/(.*)$/` in JavaScript, both in i/update and in
// the frontend): any flags, and `.` does not cross JavaScript line terminators.
//
// **形は本家と同じにする (#3369)。** flags を `gimsuy` に限っていたので、本家と
// fork の frontend が受け付ける `/foo/v` や `/foo/d` を i/update が拒否していた。
var regexLiteral = regexp.MustCompile(`^/([^\n\r\x{2028}\x{2029}]+)/([^\n\r\x{2028}\x{2029}]*)$`)

// evaluableFlags reports whether the server evaluates a literal with these
// flags. The frontend (`new RegExp(pattern, flags)`) only accepts the flags
// `dgimsuvy`, each at most once and not `u` with `v`; for anything else its
// checkWordMute gets no regexp and never matches, so neither does the server.
//
// **`v` の付いたものはサーバーでは当てない。** `v` は文字クラスの書き方
// (集合の演算、入れ子のクラス、`\q{}`) を変えるが、Go の regexp では表せず、
// 同じ文字列が別の意味で読まれる (例えば `[a&&b]` を Go は 3 文字のクラスとして
// 読む)。当てると frontend が表示するノートを隠しうるので、判定しない側に倒す。
// frontend は本家と同じく `v` 付きでも判定する。
func evaluableFlags(flags string) bool {
	seen := map[rune]bool{}
	for _, f := range flags {
		if !strings.ContainsRune("dgimsuvy", f) || seen[f] {
			return false
		}
		seen[f] = true
	}
	// `u` と `v` の同時指定は frontend では例外になるが、`v` を当てない以上どちらも false。
	return !seen['v']
}

// inlineFlagPrefix detects an existing `(?flags)` or `(?flags:...)` inline
// flag group at the start of a pattern. 既に inline flag を含んでいるなら
// 二重指定を避けるため flag 注入を skip する。`(?:...)` (non-capture) や
// `(?P<name>...)` (named group) は flag 設定ではないので一致させない。
var inlineFlagPrefix = regexp.MustCompile(`^\(\?[imsU]+[):]`)

// parsedCacheSize は parsedCache の size cap。TS upstream の AhoCorasick
// cache (`acCache.size > 1000`) と同じ閾値で、active user × rule revision
// が多くなっても memory が monotonic に grow しないようにする (#790)。
const parsedCacheSize = 1000

// parsedCache stores the parsed rule slice keyed by the *raw* JSON bytes.
// Hot TL paths call Match many times with the same per-user rule set; parsing
// once per request is cheap but parsing per-note would be wasteful。LRU で
// size cap しているため、user が rule を更新して古い JSON byte 列の entry が
// dangling しても oldest から evict されて bound が保たれる。
//
// hashicorp/golang-lru/v2 は内部で mutex を持ち、Add/Get は thread-safe。
// 並行 hot path で何度呼ばれても race にならない。
var parsedCache *lru.Cache[string, *ruleSet]

// ruleSet is one parsed muted-words array.
type ruleSet struct {
	rules []rule
	// id identifies this parse for the match memo. 同じ JSON を解析し直したら
	// 別の id になる (古い id の結果は LRU から自然に消える)。
	id       uint64
	hasRegex bool
}

var ruleSetSeq atomic.Uint64

func init() {
	c, err := lru.New[string, *ruleSet](parsedCacheSize)
	if err != nil {
		// lru.New は size > 0 で panic しない実装なので実質到達しないが、
		// 万一に備えて compile-time に近い fail-fast にする。
		panic("wordmute: failed to init parsedCache: " + err.Error())
	}
	parsedCache = c
	m, err := lru.New[matchKey, bool](matchMemoSize)
	if err != nil {
		panic("wordmute: failed to init matchMemo: " + err.Error())
	}
	matchMemo = m
}

func parse(raw []byte) *ruleSet {
	if len(raw) == 0 {
		return nil
	}
	key := string(raw)
	if v, ok := parsedCache.Get(key); ok {
		return v
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	rs := &ruleSet{rules: make([]rule, 0, len(entries)), id: ruleSetSeq.Add(1)}
	// **上限を超える正規表現は評価しない** (ミュートしない側に倒す)。書き込みは
	// i/update が拒否するが、上限を入れる前に保存された設定や、TS 版から引き
	// 継いだ設定が残りうる。合計の上限は、先頭から順に足して、足すと超える
	// ものだけを落とす (後ろの小さいものは残る)。
	instsUsed := 0
	for _, e := range entries {
		r, insts := parseEntry(e)
		if r == nil {
			continue
		}
		if _, isRegex := r.(regexRule); isRegex {
			if instsUsed+insts > maxRegexInstsTotal {
				continue
			}
			instsUsed += insts
			rs.hasRegex = true
		}
		rs.rules = append(rs.rules, r)
	}
	parsedCache.Add(key, rs)
	return rs
}

// parseEntry parses one entry. For a regex literal it also returns the size of
// the compiled program; a literal over the per-entry limits is dropped.
func parseEntry(e json.RawMessage) (rule, int) {
	var s string
	if err := json.Unmarshal(e, &s); err == nil {
		// Misskey TS upstream allows /regex/flags literal as a string entry.
		if m := regexLiteral.FindStringSubmatch(s); m != nil && !evaluableFlags(m[2]) {
			return nil, 0
		}
		if re, insts, isLiteral, err := goRegexFromLiteral(s); isLiteral {
			if err != nil || insts > maxRegexInsts {
				return nil, 0
			}
			return regexRule{re: re}, insts
		}
		// **正規表現の形でない文字列は、何にも一致させない。** 本家の判定は文字列の
		// 要素を必ず正規表現として読み、`/.../` の形でなければ false を返す
		// (i/update も本家ではこの形を INVALID_REGEXP で拒む)。以前は部分一致の
		// 語として扱い、本家のクライアントが表示するノートを隠していた (#3369)。
		return nil, 0
	}
	var arr []string
	if err := json.Unmarshal(e, &arr); err == nil {
		out := make(andRule, 0, len(arr))
		for _, kw := range arr {
			if kw == "" {
				continue
			}
			out = append(out, kw)
		}
		if len(out) == 0 {
			return nil, 0
		}
		return out, 0
	}
	return nil, 0
}

// errRegexpTooLarge reports a literal whose source exceeds maxRegexPatternBytes.
var errRegexpTooLarge = errors.New("regexp literal too large")

// goRegexFromLiteral compiles a /pattern/flags literal into a Go regexp, applying
// the i/m/s inline-flag conversion and the y anchor (other flags are ignored,
// like node-re2 in upstream i/update; see literalProgram), and
// reports the number of instructions of the compiled program. Returns
// (nil, 0, false, nil) when s is not a /.../ literal, and (nil, 0, true, err)
// when it is a literal that fails to compile or is too large (#2106 L3
// validation reuse).
func goRegexFromLiteral(s string) (*regexp.Regexp, int, bool, error) {
	pattern, insts, isLiteral, err := literalProgram(s, true)
	if !isLiteral || err != nil {
		return nil, 0, isLiteral, err
	}
	// 評価しない大きさのものは regexp.Compile まで進めない (同じコンパイルを
	// もう一度払わない)。
	if insts > maxRegexInsts {
		return nil, insts, true, ErrRegexpTooComplex
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, 0, true, err
	}
	return re, insts, true, nil
}

// literalProgram parses a /pattern/flags literal with regexp/syntax and returns
// the Go pattern. With compile it also compiles the program and returns the
// number of instructions; a literal whose estimated size is far over
// maxRegexInsts is rejected before compiling.
//
// **コンパイルの前に大きさを見積もって断る。** コンパイルの費用は命令数に
// 比例し、短いパターンでも繰り返しの入れ子で数十万命令になりうる。断るための
// コンパイルだけで CPU を使わせない。
// 構文解析は Go 自身が大きさを制限しており、それ以上は膨らまない。
func literalProgram(s string, compile bool) (string, int, bool, error) {
	m := regexLiteral.FindStringSubmatch(s)
	if m == nil {
		return "", 0, false, nil
	}
	pattern, flags := m[1], m[2]
	if len(pattern) > maxRegexPatternBytes {
		return "", 0, true, errRegexpTooLarge
	}
	var inline string
	if strings.ContainsRune(flags, 'i') {
		inline += "i"
	}
	if strings.ContainsRune(flags, 'm') {
		inline += "m"
	}
	if strings.ContainsRune(flags, 's') {
		inline += "s"
	}
	// **`y` は先頭に固定する。** frontend は lastIndex を 0 に戻してから test する
	// ので、sticky な正規表現は本文の先頭でしか当たらない。無視すると、frontend が
	// 表示するノートをサーバーだけが隠す。`g` / `d` は判定の結果を変えない。`u` は
	// JavaScript では `.` や `\p{…}` の読み方を変えるが、Go の regexp は常に Unicode で
	// 読むので写すものが無い (u の無い正規表現との構文の差は docs/divergence/api.md)。
	// それ以外の flags は無視する (本家 i/update の node-re2 も知らない flags を
	// 無視して受け付ける。判定に使うかは parseEntry の evaluableFlags が決める)。
	sticky := strings.ContainsRune(flags, 'y')
	// 二重指定回避: pattern が既に inline flag group `(?ims)` で始まる場合のみ skip。
	if inline != "" && !inlineFlagPrefix.MatchString(pattern) {
		pattern = "(?" + inline + ")" + pattern
	}
	if sticky {
		pattern = `\A(?:` + pattern + `)`
	}
	// 命令数は regexp.Compile と同じ手順 (Parse → Simplify → Compile) で数える。
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", 0, true, err
	}
	if !compile {
		return pattern, 0, true, nil
	}
	if estimateInsts(parsed, estimateCap) > estimateCap {
		return "", 0, true, ErrRegexpTooComplex
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		return "", 0, true, err
	}
	return pattern, len(prog.Inst), true, nil
}

// estimateCap is the estimated size over which a literal is rejected without
// compiling. The estimate is rough, so the cap leaves a wide margin above
// maxRegexInsts; the exact limit is applied to the compiled program.
const estimateCap = 8 * maxRegexInsts

// estimateInsts estimates the number of instructions re compiles to. It stops
// counting once the estimate exceeds limit (and then returns a value above
// limit), so a deeply repeated pattern costs no more than walking its tree.
func estimateInsts(re *syntax.Regexp, limit int) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune) + 1
	case syntax.OpConcat, syntax.OpAlternate:
		n := 1
		for _, sub := range re.Sub {
			n += estimateInsts(sub, limit)
			if n > limit {
				return limit + 1
			}
		}
		return n
	case syntax.OpCapture, syntax.OpStar, syntax.OpPlus, syntax.OpQuest:
		n := 2
		for _, sub := range re.Sub {
			n += estimateInsts(sub, limit)
		}
		if n > limit {
			return limit + 1
		}
		return n
	case syntax.OpRepeat:
		if re.Max == 0 {
			// `{0}` は Simplify で空になり、命令を生まない。
			return 1
		}
		times := re.Max
		if times < re.Min {
			times = re.Min + 1
		}
		if times < 1 {
			times = 1
		}
		sub := 1
		for _, s := range re.Sub {
			sub += estimateInsts(s, limit)
		}
		if sub > limit || times > limit || sub*times > limit {
			return limit + 1
		}
		return sub*times + 1
	default:
		return 2
	}
}

// ErrInvalidRegexp is returned by ValidateMutedWords when a regex-literal entry
// fails to compile. Callers map it to the upstream INVALID_REGEXP API error.
var ErrInvalidRegexp = errors.New("invalid regexp literal in muted words")

// ErrRegexpTooComplex is returned by ValidateHardMutedWords when a regex-literal
// entry, or the list as a whole, exceeds the evaluation limits. Callers map it
// to INVALID_REGEXP as well (upstream rejects patterns RE2 cannot compile with
// the same error).
var ErrRegexpTooComplex = errors.New("regexp literal in muted words is too complex")

// ValidateMutedWords reports whether every string entry in the muted-words JSON
// is a /pattern/flags literal that compiles, returning ErrInvalidRegexp on the
// first failure (#2106 L3 / #3369, upstream i/update.ts validateMuteWordRegex).
// A string that is not a literal is rejected too, like upstream. AND-group arrays
// and shape errors are ignored here (shape is handled by the caller's normalize
// step).
//
// mutedWords (soft mute) is evaluated only by clients, so the instruction limits
// do not apply; the source length limit does (it keeps this validation itself
// cheap).
func ValidateMutedWords(raw json.RawMessage) error {
	_, err := validate(raw, false)
	return err
}

// ValidateHardMutedWords is ValidateMutedWords plus the evaluation limits that
// apply to hardMutedWords, which the server matches against every note.
func ValidateHardMutedWords(raw json.RawMessage) error {
	insts, err := validate(raw, true)
	if err != nil {
		return err
	}
	total := 0
	for _, n := range insts {
		if n > maxRegexInsts {
			return ErrRegexpTooComplex
		}
		total += n
	}
	if total > maxRegexInstsTotal {
		return ErrRegexpTooComplex
	}
	return nil
}

// validate parses every regex literal and, with compile, returns their
// instruction counts.
func validate(raw json.RawMessage, compile bool) ([]int, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, nil
	}
	var insts []int
	for _, e := range entries {
		var s string
		if err := json.Unmarshal(e, &s); err != nil {
			continue // array (AND group), not a regex literal
		}
		_, n, isLiteral, err := literalProgram(s, compile)
		if !isLiteral {
			// **`/.../` の形でない文字列は拒否する** (本家 i/update の
			// validateMuteWordRegex と同じ。#3369)。受け付けても何にも当たらない。
			return nil, ErrInvalidRegexp
		}
		if errors.Is(err, ErrRegexpTooComplex) {
			return nil, err
		}
		if err != nil {
			return nil, ErrInvalidRegexp
		}
		insts = append(insts, n)
	}
	return insts, nil
}

// MatchNote is a convenience wrapper that mirrors upstream check-word-mute.ts:
// it builds the search text from cw + "\n" + text (both nullable) and skips
// notes authored by the viewer. Caller passes the viewer's own user ID; an
// empty viewerID disables the self-skip.
func MatchNote(rulesJSON []byte, noteUserID, viewerID, noteText, noteCW string) bool {
	if viewerID != "" && noteUserID == viewerID {
		return false
	}
	combined := strings.TrimSpace(noteCW + "\n" + noteText)
	return Match(rulesJSON, combined)
}

// Note is the part of one note, and of the notes it embeds, that the hard-mute
// decision reads. Callers build it from their own representation (REST from
// the repository model, streaming from the published payload).
type Note struct {
	UserID string
	Text   string
	CW     string
	// Hidden reports that the viewer is not allowed to see this note's content
	// (upstream NoteEntityService.hideNote blanks text and cw). A hidden note
	// never matches.
	Hidden bool
	// PureRenote reports that the note is a renote without its own content.
	PureRenote bool
	Reply      *Note
	Renote     *Note
}

// MatchAppearNote reports whether n must be hidden from viewerID by the hard
// mute rules. It mirrors upstream frontend use-note.ts: the note shown for a
// pure renote is the renoted note (getAppearNote), and checkNoteWordMute checks
// that note, its reply target and its renote (quote) target, each with
// checkWordMute (so each one skips its own author's viewer).
func MatchAppearNote(rulesJSON []byte, viewerID string, n *Note) bool {
	if n == nil || len(rulesJSON) == 0 {
		return false
	}
	appear := n
	if n.PureRenote && n.Renote != nil {
		appear = n.Renote
	}
	return matchVisible(rulesJSON, viewerID, appear) ||
		matchVisible(rulesJSON, viewerID, appear.Reply) ||
		matchVisible(rulesJSON, viewerID, appear.Renote)
}

func matchVisible(rulesJSON []byte, viewerID string, n *Note) bool {
	// **隠される中身では判定しない。** 本家は hideNote で本文を消した後の packed
	// note を判定するので一致しない。見えないノートの中身は判定に使わない。
	if n == nil || n.Hidden {
		return false
	}
	return MatchNote(rulesJSON, n.UserID, viewerID, n.Text, n.CW)
}
