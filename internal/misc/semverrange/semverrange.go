// Package semverrange reports whether a version satisfies a version range,
// with the same results as npm's semver package.
//
// This is a port of node-semver 7.8.5 (https://github.com/npm/node-semver),
// which is distributed under the ISC License:
//
//	Copyright (c) Isaac Z. Schlueter and Contributors
//
//	Permission to use, copy, modify, and/or distribute this software for any
//	purpose with or without fee is hereby granted, provided that the above
//	copyright notice and this permission notice appear in all copies.
//
//	THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
//	WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
//	MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
//	ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
//	WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
//	ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR
//	IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
//
// Only the path used by satisfies(version, range, { includePrerelease: true })
// with loose=false is ported.
package semverrange

// 本家Misskeyは配送停止の判定に semver.satisfies(version, range, { includePrerelease: true })
// を使う。範囲の解釈を独自に書くと、空白の詰め方や x-range の境界のような細部で結果が
// ずれるので、node-semver の文字列変換の手順(classes/range.js)をそのまま移している。
// 正規表現は internal/re.js の safeRe(繰り返しに上限を付けた版)を RE2 の構文で組み直した
// もので、上限も含めて同じにしている。上限は結果を変えることがある(例: build metadata を
// 消した跡に空白が2つ並ぶと、ハイフンの範囲として読まれなくなる)。

import (
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
)

const (
	// maxLength is MAX_LENGTH in internal/constants.js.
	maxLength = 256
	// maxSafeInteger is Number.MAX_SAFE_INTEGER.
	maxSafeInteger = 9007199254740991
	// maxSafeBuildLength is MAX_SAFE_BUILD_LENGTH in internal/constants.js.
	maxSafeBuildLength = maxLength - 6
)

// jsSpaceClass is the contents of a character class equal to JavaScript's \s,
// which also matches the characters String.prototype.trim removes.
//
// RE2 の \s は ASCII の空白だけで、\v も含まない。JS の \s は Unicode の空白と行終端子を含む。
const jsSpaceClass = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrim is String.prototype.trim.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

// TrimSpace removes leading and trailing white space the way JavaScript's
// String.prototype.trim does. Go の strings.TrimSpace とは集合が違う (U+0085 を
// 除き U+FEFF を除かない)。本家と同じ判定にしたい呼び出し側のために公開する。
func TrimSpace(s string) string { return jsTrim(s) }

// makeSafe mirrors makeSafeRegex in internal/re.js.
func makeSafe(src string) string {
	for _, rep := range []struct {
		token string
		max   int
	}{
		{`\s`, 1},
		{`\d`, maxLength},
		{`[a-zA-Z0-9-]`, maxSafeBuildLength},
	} {
		m := strconv.Itoa(rep.max)
		src = strings.ReplaceAll(src, rep.token+"*", rep.token+"{0,"+m+"}")
		src = strings.ReplaceAll(src, rep.token+"+", rep.token+"{1,"+m+"}")
	}
	return src
}

// toRE2 rewrites JavaScript's \s into an explicit class so that RE2 matches
// the same characters.
func toRE2(src string) string {
	src = strings.ReplaceAll(src, `[v=\s]`, `[v=`+jsSpaceClass+`]`)
	return strings.ReplaceAll(src, `\s`, `[`+jsSpaceClass+`]`)
}

func safeRe(src string) *regexp.Regexp {
	return regexp.MustCompile(toRE2(makeSafe(src)))
}

// The sources below follow the createToken calls in internal/re.js.
const (
	srcLetterDashNumber       = `[a-zA-Z0-9-]`
	srcNumericIdentifier      = `0|[1-9]\d*`
	srcNumericIdentifierLoose = `\d+`
	srcNonNumericIdentifier   = `\d*[a-zA-Z-]` + srcLetterDashNumber + `*`
	srcMainVersion            = `(` + srcNumericIdentifier + `)\.(` + srcNumericIdentifier + `)\.(` + srcNumericIdentifier + `)`
	srcMainVersionLoose       = `(` + srcNumericIdentifierLoose + `)\.(` + srcNumericIdentifierLoose + `)\.(` + srcNumericIdentifierLoose + `)`
	srcPrereleaseIdentifier   = `(?:` + srcNonNumericIdentifier + `|` + srcNumericIdentifier + `)`
	srcPrereleaseIdentLoose   = `(?:` + srcNonNumericIdentifier + `|` + srcNumericIdentifierLoose + `)`
	srcPrerelease             = `(?:-(` + srcPrereleaseIdentifier + `(?:\.` + srcPrereleaseIdentifier + `)*))`
	srcPrereleaseLoose        = `(?:-?(` + srcPrereleaseIdentLoose + `(?:\.` + srcPrereleaseIdentLoose + `)*))`
	srcBuildIdentifier        = srcLetterDashNumber + `+`
	srcBuild                  = `(?:\+(` + srcBuildIdentifier + `(?:\.` + srcBuildIdentifier + `)*))`
	srcFullPlain              = `v?` + srcMainVersion + srcPrerelease + `?` + srcBuild + `?`
	srcLoosePlain             = `[v=\s]*` + srcMainVersionLoose + srcPrereleaseLoose + `?` + srcBuild + `?`
	srcGTLT                   = `((?:<|>)?=?)`
	srcXRangeIdentifier       = srcNumericIdentifier + `|x|X|\*`
	srcXRangePlain            = `[v=\s]*(` + srcXRangeIdentifier + `)` +
		`(?:\.(` + srcXRangeIdentifier + `)` +
		`(?:\.(` + srcXRangeIdentifier + `)` +
		`(?:` + srcPrerelease + `)?` + srcBuild + `?` +
		`)?)?`
	srcLoneTilde = `(?:~>?)`
	srcLoneCaret = `(?:\^)`
)

var (
	reFull           = safeRe(`^` + srcFullPlain + `$`)
	reBuild          = safeRe(srcBuild)
	reXRange         = safeRe(`^` + srcGTLT + `\s*` + srcXRangePlain + `$`)
	reTildeTrim      = safeRe(`(\s*)` + srcLoneTilde + `\s+`)
	reTilde          = safeRe(`^` + srcLoneTilde + srcXRangePlain + `$`)
	reCaretTrim      = safeRe(`(\s*)` + srcLoneCaret + `\s+`)
	reCaret          = safeRe(`^` + srcLoneCaret + srcXRangePlain + `$`)
	reComparator     = safeRe(`^` + srcGTLT + `\s*(` + srcFullPlain + `)$|^$`)
	reComparatorTrim = safeRe(`(\s*)` + srcGTLT + `\s*(` + srcLoosePlain + `|` + srcXRangePlain + `)`)
	reHyphenRange    = safeRe(`^\s*(` + srcXRangePlain + `)\s+-\s+(` + srcXRangePlain + `)\s*$`)
	reStar           = safeRe(`(<|>)?=?\s*\*`)
	reGTE0Pre        = safeRe(`^\s*>=\s*0\.0\.0-0\s*$`)

	// range.js は build metadata の除去にだけ、上限の無い元の正規表現(src)を使う。
	reBuildStrip = regexp.MustCompile(toRE2(srcBuild))
	// SPACE_CHARACTERS in range.js and the /\s+/ used by split.
	reSpaces = regexp.MustCompile(toRE2(`\s+`))
	// numeric in internal/identifiers.js and the test in the SemVer constructor.
	reNumeric = regexp.MustCompile(`^[0-9]+$`)
)

// Satisfies reports whether version satisfies rng, as
// semver.satisfies(version, rng, { includePrerelease: true }) does in
// node-semver 7.8.5 (loose=false). It returns false when rng is not a valid
// range or version is not a valid version, as satisfies does.
func Satisfies(version, rng string) bool {
	set, ok := cachedParseRange(rng)
	if !ok {
		return false
	}
	// Range.test: 空文字は SemVer を作る前に false を返す。
	if version == "" {
		return false
	}
	v, ok := parseSemVer(version)
	if !ok {
		return false
	}
	for _, comps := range set {
		if testSet(comps, v) {
			return true
		}
	}
	return false
}

// testSet mirrors testSet in range.js. With includePrerelease the
// prerelease filtering never applies.
func testSet(comps []*comparator, v *semVer) bool {
	for _, c := range comps {
		if !c.test(v) {
			return false
		}
	}
	return true
}

// jsSplitSpaces is s.split(/\s+/).
func jsSplitSpaces(s string) []string {
	return reSpaces.Split(s, -1)
}

// replaceFirst is String.prototype.replace with a non-global regexp and a
// replacement function receiving the submatches.
func replaceFirst(re *regexp.Regexp, s string, fn func(m []string) string) string {
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]] + fn(submatches(s, loc)) + s[loc[1]:]
}

// replaceAll is String.prototype.replace with a global regexp. None of the
// global regexps used here can match the empty string.
func replaceAll(re *regexp.Regexp, s string, fn func(m []string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:loc[0]])
		b.WriteString(fn(submatches(s, loc)))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// submatches returns the submatches with non-participating groups as "",
// which every caller treats the same as JavaScript's undefined.
func submatches(s string, loc []int) []string {
	m := make([]string, len(loc)/2)
	for i := range m {
		if loc[2*i] >= 0 {
			m[i] = s[loc[2*i]:loc[2*i+1]]
		}
	}
	return m
}

// rangeCacheMax bounds the parsed ranges kept by cachedParseRange.
const rangeCacheMax = 256

type parsedRange struct {
	set [][]*comparator
	ok  bool
}

// rangeCache keeps parsed ranges, like the LRU in range.js.
//
// **配送のたびに同じ範囲を解析し直さない。** 範囲は管理者の設定で、配送 1 件
// ごとに判定が走る。解析は長さにほぼ比例し (1MiB で約 1.4 秒)、本家も解析結果を
// キャッシュに持つ。解析した comparator は読むだけなので共有してよい。上限を
// 超えたら丸ごと捨てる (設定の範囲は普通数件なので、LRU にするほどではない)。
var rangeCache = struct {
	sync.Mutex
	m map[string]parsedRange
}{m: make(map[string]parsedRange)}

func cachedParseRange(rng string) ([][]*comparator, bool) {
	rangeCache.Lock()
	if p, hit := rangeCache.m[rng]; hit {
		rangeCache.Unlock()
		return p.set, p.ok
	}
	rangeCache.Unlock()
	set, ok := parseRange(rng)
	rangeCache.Lock()
	if len(rangeCache.m) >= rangeCacheMax {
		rangeCache.m = make(map[string]parsedRange)
	}
	rangeCache.m[rng] = parsedRange{set: set, ok: ok}
	rangeCache.Unlock()
	return set, ok
}

// parseRange mirrors the Range constructor. It returns false where the
// constructor throws.
func parseRange(rng string) ([][]*comparator, bool) {
	raw := reSpaces.ReplaceAllString(jsTrim(rng), " ")
	var set [][]*comparator
	for _, part := range strings.Split(raw, "||") {
		comps, ok := parseRangePart(jsTrim(part))
		if !ok {
			return nil, false
		}
		if len(comps) > 0 {
			set = append(set, comps)
		}
	}
	if len(set) == 0 {
		return nil, false
	}
	if len(set) > 1 {
		first := set[0]
		var kept [][]*comparator
		for _, c := range set {
			if !c[0].isNullSet() {
				kept = append(kept, c)
			}
		}
		set = kept
		if len(set) == 0 {
			set = [][]*comparator{first}
		} else if len(set) > 1 {
			for _, c := range set {
				if len(c) == 1 && c[0].value == "" {
					set = [][]*comparator{c}
					break
				}
			}
		}
	}
	return set, true
}

// parseRangePart mirrors Range.parseRange.
func parseRangePart(r string) ([]*comparator, bool) {
	r = reBuildStrip.ReplaceAllString(r, "")
	r = replaceFirst(reHyphenRange, r, hyphenReplace)
	r = replaceAll(reComparatorTrim, r, func(m []string) string { return m[1] + m[2] + m[3] })
	r = replaceAll(reTildeTrim, r, func(m []string) string { return m[1] + "~" })
	r = replaceAll(reCaretTrim, r, func(m []string) string { return m[1] + "^" })

	pieces := strings.Split(r, " ")
	for i, p := range pieces {
		pieces[i] = parseComparator(p)
	}
	list := jsSplitSpaces(strings.Join(pieces, " "))

	comps := make([]*comparator, 0, len(list))
	for _, c := range list {
		// replaceGTE0: includePrerelease なので >=0.0.0-0 を * と同じに扱う。
		c = jsTrim(c)
		if reGTE0Pre.MatchString(c) {
			c = ""
		}
		comp, ok := newComparator(c)
		if !ok {
			return nil, false
		}
		comps = append(comps, comp)
	}

	// 本家は Map で value の重複を落とす。挿入順は最初の出現、値は最後の出現になる。
	var result []*comparator
	index := map[string]int{}
	for _, comp := range comps {
		if comp.isNullSet() {
			return []*comparator{comp}, true
		}
		if i, ok := index[comp.value]; ok {
			result[i] = comp
			continue
		}
		index[comp.value] = len(result)
		result = append(result, comp)
	}
	if len(result) > 1 {
		if i, ok := index[""]; ok {
			result = append(result[:i], result[i+1:]...)
		}
	}
	return result, true
}

// parseComparator mirrors parseComparator in range.js.
func parseComparator(comp string) string {
	comp = replaceFirst(reBuild, comp, func([]string) string { return "" })
	comp = mapSpaces(jsTrim(comp), replaceCaret)
	comp = mapSpaces(jsTrim(comp), replaceTilde)
	comp = mapSpaces(comp, replaceXRange)
	// replaceStars: 非 global なので、最初の1つだけを消す。
	return replaceFirst(reStar, jsTrim(comp), func([]string) string { return "" })
}

func mapSpaces(s string, fn func(string) string) string {
	parts := jsSplitSpaces(s)
	for i, p := range parts {
		parts[i] = fn(p)
	}
	return strings.Join(parts, " ")
}

// isX mirrors isX in range.js.
func isX(id string) bool {
	return id == "" || id == "x" || id == "X" || id == "*"
}

// incr returns the decimal form of +s + 1 for a string of digits.
//
// JS は Number で足すので、MAX_SAFE_INTEGER を超える値は丸められ、1e21 以上は指数表記になる。
// どちらの形でも、続く Comparator の解析で SemVer が上限超過か形の不一致で例外を投げ、
// 範囲全体が不正になる。ここで正確な値を返しても上限を超える点は変わらないので、結果は同じ。
func incr(s string) string {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		// 呼び出し元は数字の列だけを渡す。
		panic("semverrange: not a number: " + s)
	}
	return n.Add(n, big.NewInt(1)).String()
}

// replaceTilde mirrors replaceTilde in range.js with includePrerelease.
func replaceTilde(comp string) string {
	m := reTilde.FindStringSubmatch(comp)
	if m == nil {
		return comp
	}
	M, mi, p, pr := m[1], m[2], m[3], m[4]
	switch {
	case isX(M):
		return ""
	case isX(mi):
		return ">=" + M + ".0.0-0 <" + incr(M) + ".0.0-0"
	case isX(p):
		return ">=" + M + "." + mi + ".0-0 <" + M + "." + incr(mi) + ".0-0"
	case pr != "":
		return ">=" + M + "." + mi + "." + p + "-" + pr + " <" + M + "." + incr(mi) + ".0-0"
	default:
		return ">=" + M + "." + mi + "." + p + " <" + M + "." + incr(mi) + ".0-0"
	}
}

// replaceCaret mirrors replaceCaret in range.js with includePrerelease.
func replaceCaret(comp string) string {
	m := reCaret.FindStringSubmatch(comp)
	if m == nil {
		return comp
	}
	M, mi, p, pr := m[1], m[2], m[3], m[4]
	switch {
	case isX(M):
		return ""
	case isX(mi):
		return ">=" + M + ".0.0-0 <" + incr(M) + ".0.0-0"
	case isX(p):
		if M == "0" {
			return ">=" + M + "." + mi + ".0-0 <" + M + "." + incr(mi) + ".0-0"
		}
		return ">=" + M + "." + mi + ".0-0 <" + incr(M) + ".0.0-0"
	}
	lower := ">=" + M + "." + mi + "." + p
	if pr != "" {
		lower += "-" + pr
	}
	if M == "0" {
		if mi == "0" {
			return lower + " <" + M + "." + mi + "." + incr(p) + "-0"
		}
		return lower + " <" + M + "." + incr(mi) + ".0-0"
	}
	return lower + " <" + incr(M) + ".0.0-0"
}

// replaceXRange mirrors replaceXRange in range.js with includePrerelease.
func replaceXRange(comp string) string {
	comp = jsTrim(comp)
	m := reXRange.FindStringSubmatch(comp)
	if m == nil {
		return comp
	}
	gtlt, M, mi, p := m[1], m[2], m[3], m[4]
	if (isX(M) && !isX(mi)) || (isX(mi) && p != "" && !isX(p)) {
		return comp
	}
	xM := isX(M)
	xm := xM || isX(mi)
	xp := xm || isX(p)
	anyX := xp
	if gtlt == "=" && anyX {
		gtlt = ""
	}
	const pr = "-0"

	switch {
	case xM:
		if gtlt == ">" || gtlt == "<" {
			return "<0.0.0-0"
		}
		return "*"
	case gtlt != "" && anyX:
		if xm {
			mi = "0"
		}
		p = "0"
		switch gtlt {
		case ">":
			gtlt = ">="
			if xm {
				M = incr(M)
				mi = "0"
			} else {
				mi = incr(mi)
			}
			p = "0"
		case "<=":
			gtlt = "<"
			if xm {
				M = incr(M)
			} else {
				mi = incr(mi)
			}
		}
		return gtlt + M + "." + mi + "." + p + pr
	case xm:
		return ">=" + M + ".0.0" + pr + " <" + incr(M) + ".0.0-0"
	case xp:
		return ">=" + M + "." + mi + ".0" + pr + " <" + M + "." + incr(mi) + ".0-0"
	}
	return m[0]
}

// hyphenReplace mirrors hyphenReplace in range.js with includePrerelease.
func hyphenReplace(m []string) string {
	from, fM, fm, fp, fpr := m[1], m[2], m[3], m[4], m[5]
	tM, tm, tp, tpr := m[8], m[9], m[10], m[11]

	switch {
	case isX(fM):
		from = ""
	case isX(fm):
		from = ">=" + fM + ".0.0-0"
	case isX(fp):
		from = ">=" + fM + "." + fm + ".0-0"
	case fpr != "":
		from = ">=" + from
	default:
		from = ">=" + from + "-0"
	}

	var to string
	switch {
	case isX(tM):
		to = ""
	case isX(tm):
		to = "<" + incr(tM) + ".0.0-0"
	case isX(tp):
		to = "<" + tM + "." + incr(tm) + ".0-0"
	case tpr != "":
		to = "<=" + tM + "." + tm + "." + tp + "-" + tpr
	default:
		to = "<" + tM + "." + tm + "." + incr(tp) + "-0"
	}
	return jsTrim(from + " " + to)
}

// comparator mirrors classes/comparator.js.
type comparator struct {
	operator string
	semver   *semVer // nil means ANY
	value    string
}

func (c *comparator) isNullSet() bool { return c.value == "<0.0.0-0" }

// newComparator mirrors the Comparator constructor. It returns false where
// the constructor throws.
func newComparator(comp string) (*comparator, bool) {
	comp = strings.Join(jsSplitSpaces(jsTrim(comp)), " ")
	m := reComparator.FindStringSubmatch(comp)
	if m == nil {
		return nil, false
	}
	c := &comparator{operator: m[1]}
	if c.operator == "=" {
		c.operator = ""
	}
	if m[2] == "" {
		return c, true
	}
	v, ok := parseSemVer(m[2])
	if !ok {
		return nil, false
	}
	c.semver = v
	c.value = c.operator + v.version
	return c, true
}

func (c *comparator) test(v *semVer) bool {
	if c.semver == nil {
		return true
	}
	cmp := v.compare(c.semver)
	switch c.operator {
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	default:
		return cmp == 0
	}
}

// identifier is a prerelease identifier: a number below MAX_SAFE_INTEGER, or
// a string.
type identifier struct {
	isNum bool
	num   int64
	str   string
}

func (id identifier) String() string {
	if id.isNum {
		return strconv.FormatInt(id.num, 10)
	}
	return id.str
}

// semVer mirrors classes/semver.js.
type semVer struct {
	major, minor, patch int64
	prerelease          []identifier
	version             string
}

// utf16Len is the JavaScript length of s.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// parseSafeInt returns +s for a string of digits without leading zeros and
// whether it is at most max.
func parseSafeInt(s string, max int64) (int64, bool) {
	// 先頭の0は正規表現で除かれているので、17桁以上は必ず MAX_SAFE_INTEGER を超える。
	if len(s) > 16 {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > max {
		return 0, false
	}
	return n, true
}

// parseSemVer mirrors the SemVer constructor with loose=false. It returns
// false where the constructor throws.
func parseSemVer(version string) (*semVer, bool) {
	if utf16Len(version) > maxLength {
		return nil, false
	}
	m := reFull.FindStringSubmatch(jsTrim(version))
	if m == nil {
		return nil, false
	}
	v := &semVer{}
	var ok bool
	if v.major, ok = parseSafeInt(m[1], maxSafeInteger); !ok {
		return nil, false
	}
	if v.minor, ok = parseSafeInt(m[2], maxSafeInteger); !ok {
		return nil, false
	}
	if v.patch, ok = parseSafeInt(m[3], maxSafeInteger); !ok {
		return nil, false
	}
	if m[4] != "" {
		for _, id := range strings.Split(m[4], ".") {
			// 本家は MAX_SAFE_INTEGER 未満の数字だけを数として持ち、それ以上は文字列のまま持つ。
			if reNumeric.MatchString(id) {
				if n, ok := parseSafeInt(id, maxSafeInteger-1); ok {
					v.prerelease = append(v.prerelease, identifier{isNum: true, num: n})
					continue
				}
			}
			v.prerelease = append(v.prerelease, identifier{str: id})
		}
	}
	v.version = strconv.FormatInt(v.major, 10) + "." + strconv.FormatInt(v.minor, 10) + "." + strconv.FormatInt(v.patch, 10)
	if len(v.prerelease) > 0 {
		ids := make([]string, len(v.prerelease))
		for i, id := range v.prerelease {
			ids[i] = id.String()
		}
		v.version += "-" + strings.Join(ids, ".")
	}
	return v, true
}

// compare mirrors SemVer.compare.
func (v *semVer) compare(o *semVer) int {
	if v.version == o.version {
		return 0
	}
	if c := v.compareMain(o); c != 0 {
		return c
	}
	return v.comparePre(o)
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (v *semVer) compareMain(o *semVer) int {
	if c := cmpInt(v.major, o.major); c != 0 {
		return c
	}
	if c := cmpInt(v.minor, o.minor); c != 0 {
		return c
	}
	return cmpInt(v.patch, o.patch)
}

func (v *semVer) comparePre(o *semVer) int {
	switch {
	case len(v.prerelease) > 0 && len(o.prerelease) == 0:
		return -1
	case len(v.prerelease) == 0 && len(o.prerelease) > 0:
		return 1
	case len(v.prerelease) == 0 && len(o.prerelease) == 0:
		return 0
	}
	for i := 0; ; i++ {
		switch {
		case i >= len(v.prerelease) && i >= len(o.prerelease):
			return 0
		case i >= len(o.prerelease):
			return 1
		case i >= len(v.prerelease):
			return -1
		}
		a, b := v.prerelease[i], o.prerelease[i]
		if a == b {
			continue
		}
		return compareIdentifiers(a, b)
	}
}

// numericValue is +id for an identifier that is all digits. Go の ParseFloat は
// JS の Number と同じく最も近い double に丸める。
func numericValue(id identifier) float64 {
	if id.isNum {
		return float64(id.num)
	}
	f, _ := strconv.ParseFloat(id.str, 64)
	return f
}

// compareIdentifiers mirrors compareIdentifiers in internal/identifiers.js.
//
// MAX_SAFE_INTEGER 以上の数字の識別子は文字列のまま持たれ、比較のときに double へ
// 変換される。丸めで違う識別子が等しくなることがあるが、本家に合わせてそのまま 0 を返す。
func compareIdentifiers(a, b identifier) int {
	if a.isNum && b.isNum {
		return cmpInt(a.num, b.num)
	}
	anum := a.isNum || reNumeric.MatchString(a.str)
	bnum := b.isNum || reNumeric.MatchString(b.str)
	if anum && bnum {
		fa, fb := numericValue(a), numericValue(b)
		switch {
		case fa == fb:
			return 0
		case fa < fb:
			return -1
		}
		return 1
	}
	switch {
	case a == b:
		return 0
	case anum && !bnum:
		return -1
	case bnum && !anum:
		return 1
	case a.str < b.str:
		return -1
	}
	return 1
}
