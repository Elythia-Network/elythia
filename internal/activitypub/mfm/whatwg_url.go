package mfm

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/bidi"
	"golang.org/x/text/unicode/norm"
)

// whatwgHref returns what JavaScript's `new URL(raw).href` returns for an
// http or https URL. ok is false when the WHATWG URL parser would throw, and
// also for every other scheme.
//
// 本家 MfmService.toHtml は link / url / mention の href を `new URL(x).href` で
// 正規化し、URL として読めなければリンクにしない。net/url はこの正規化をしない
// (ホストの小文字化、既定のポートの削除、`..` の解決、IDN の punycode 化、
// IPv4 の数の正規化、パスの文字の % エスケープなど) ので、WHATWG URL Standard の
// 基本の URL パーサのうち、base を持たない http / https の経路だけを書き写した。
// ToHTML へ来る URL は mfm-js の url / link か、メンションから作った URL なので、
// それ以外の scheme は扱わず、失敗として返す (javascript: などをリンクにしない)。
//
// 本家が動く Node 26 (ada 3.4) と合わせてある。たとえばパスの `^` は % エスケープ
// する (Node 22 はしない)。非 ASCII のホストは x/net/idna の UTS #46 に任せるので、
// Unicode の版や IDNA の実装の細部で Node と違うことがある (docs/divergence.md)。
func whatwgHref(raw string) (string, bool) {
	// 前後の C0 制御文字と空白を外し、途中のタブと改行を取り除く
	s := strings.TrimFunc(raw, func(r rune) bool { return r <= 0x20 })
	if strings.ContainsAny(s, "\t\n\r") {
		s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)
	}

	colon := strings.IndexByte(s, ':')
	if colon <= 0 || !isASCIIAlpha(s[0]) {
		return "", false
	}
	for i := 1; i < colon; i++ {
		c := s[i]
		if !isASCIIAlpha(c) && !isASCIIDigit(c) && c != '+' && c != '-' && c != '.' {
			return "", false
		}
	}
	scheme := strings.ToLower(s[:colon])
	var defaultPort string
	switch scheme {
	case "http":
		defaultPort = "80"
	case "https":
		defaultPort = "443"
	default:
		return "", false
	}

	// special な scheme では、scheme の後ろの / と \ を何個でも読み飛ばす
	rest := strings.TrimLeft(s[colon+1:], "/\\")
	authEnd := strings.IndexAny(rest, "/\\?#")
	if authEnd < 0 {
		authEnd = len(rest)
	}
	authority, rest := rest[:authEnd], rest[authEnd:]

	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")

	hostPort := authority
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		hostPort = authority[at+1:]
		if hostPort == "" {
			return "", false
		}
		// userinfo の中の `@` は userinfo の集合で %40 になる。`:` は最初の 1 つが
		// ユーザー名とパスワードの区切りで、残りはパスワードの中で %3A になる
		user, pass, _ := strings.Cut(authority[:at], ":")
		user = percentEncode(user, inUserinfoSet)
		pass = percentEncode(pass, inUserinfoSet)
		if user != "" || pass != "" {
			b.WriteString(user)
			if pass != "" {
				b.WriteByte(':')
				b.WriteString(pass)
			}
			b.WriteByte('@')
		}
	}

	// ポートの区切りは、[] の外にある最初の `:`
	hostEnd := len(hostPort)
	inBrackets := false
	for i := 0; i < len(hostPort); i++ {
		switch hostPort[i] {
		case '[':
			inBrackets = true
		case ']':
			inBrackets = false
		case ':':
			if !inBrackets {
				hostEnd = i
				i = len(hostPort)
			}
		}
	}
	if hostEnd == 0 {
		return "", false
	}
	host, ok := parseURLHost(hostPort[:hostEnd])
	if !ok {
		return "", false
	}
	b.WriteString(host)
	if hostEnd < len(hostPort) {
		port, ok := parseURLPort(hostPort[hostEnd+1:])
		if !ok {
			return "", false
		}
		if port != "" && port != defaultPort {
			b.WriteByte(':')
			b.WriteString(port)
		}
	}

	pathPart, query, fragment := rest, "", ""
	hasQuery, hasFragment := false, false
	if i := strings.IndexByte(pathPart, '#'); i >= 0 {
		pathPart, fragment, hasFragment = pathPart[:i], pathPart[i+1:], true
	}
	if i := strings.IndexByte(pathPart, '?'); i >= 0 {
		pathPart, query, hasQuery = pathPart[:i], pathPart[i+1:], true
	}
	for _, seg := range parseURLPath(pathPart) {
		b.WriteByte('/')
		b.WriteString(seg)
	}
	if hasQuery {
		b.WriteByte('?')
		b.WriteString(percentEncode(query, inSpecialQuerySet))
	}
	if hasFragment {
		b.WriteByte('#')
		b.WriteString(percentEncode(fragment, inFragmentSet))
	}
	return b.String(), true
}

// parseURLPort parses the digits after the host's port separator. It returns
// "" for an empty port.
func parseURLPort(s string) (string, bool) {
	if s == "" {
		return "", true
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if !isASCIIDigit(s[i]) {
			return "", false
		}
		n = n*10 + int(s[i]-'0')
		if n > 65535 {
			return "", false
		}
	}
	return strconv.Itoa(n), true
}

// parseURLPath splits a special URL's path into percent-encoded segments,
// resolving `.` and `..`. It always returns at least one segment.
func parseURLPath(p string) []string {
	if p != "" && (p[0] == '/' || p[0] == '\\') {
		p = p[1:]
	}
	var segs []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i < len(p) && p[i] != '/' && p[i] != '\\' {
			continue
		}
		seg := percentEncode(p[start:i], inPathSet)
		atEnd := i == len(p)
		switch {
		case isDoubleDotSegment(seg):
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			if atEnd {
				segs = append(segs, "")
			}
		case isSingleDotSegment(seg):
			if atEnd {
				segs = append(segs, "")
			}
		default:
			segs = append(segs, seg)
		}
		start = i + 1
	}
	return segs
}

func isSingleDotSegment(s string) bool {
	return s == "." || strings.EqualFold(s, "%2e")
}

func isDoubleDotSegment(s string) bool {
	switch strings.ToLower(s) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

// percentEncode UTF-8 percent-encodes every code point of s that is in the
// given set or above U+007E (the C0 control percent-encode set is part of
// every set used here).
func percentEncode(s string, inSet func(c byte) bool) string {
	const hex = "0123456789ABCDEF"
	need := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7E || inSet(c) {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E || inSet(c) {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0F])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func inFragmentSet(c byte) bool {
	return c == ' ' || c == '"' || c == '<' || c == '>' || c == '`'
}

func inQuerySet(c byte) bool {
	return c == ' ' || c == '"' || c == '#' || c == '<' || c == '>'
}

func inSpecialQuerySet(c byte) bool { return inQuerySet(c) || c == '\'' }

func inPathSet(c byte) bool {
	return inQuerySet(c) || c == '?' || c == '^' || c == '`' || c == '{' || c == '}'
}

func inUserinfoSet(c byte) bool {
	if inPathSet(c) {
		return true
	}
	switch c {
	case '/', ':', ';', '=', '@', '[', '\\', ']', '|':
		return true
	}
	return false
}

// urlIDNA is UTS #46 ToASCII with the flags the URL Standard's domain to ASCII
// uses (CheckHyphens=false, CheckJoiners=true, UseSTD3ASCIIRules=false,
// Transitional_Processing=false, VerifyDnsLength=false), except CheckBidi.
//
// URL Standard は CheckBidi=true だが、Node 26 (ada) は Bidi の規則を検査せず、
// `https://abא.x/` も `https://١٢.x/` も読む。本家に合わせて検査しない。
var urlIDNA = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	idna.CheckJoiners(true),
	idna.VerifyDNSLength(false),
)

// parseURLHost parses and serializes a special URL's host.
func parseURLHost(h string) (string, bool) {
	if h[0] == '[' {
		if len(h) < 2 || h[len(h)-1] != ']' {
			return "", false
		}
		addr, ok := parseIPv6(h[1 : len(h)-1])
		if !ok {
			return "", false
		}
		return "[" + serializeIPv6(addr) + "]", true
	}
	domain := strings.ToValidUTF8(percentDecode(h), "\ufffd")
	ascii, ok := domainToASCII(domain)
	if !ok || ascii == "" {
		return "", false
	}
	for i := 0; i < len(ascii); i++ {
		if isForbiddenDomainByte(ascii[i]) {
			return "", false
		}
	}
	if endsInNumber(ascii) {
		v, ok := parseIPv4(ascii)
		if !ok {
			return "", false
		}
		return strconv.Itoa(int(v>>24)) + "." + strconv.Itoa(int(v>>16&0xFF)) + "." +
			strconv.Itoa(int(v>>8&0xFF)) + "." + strconv.Itoa(int(v&0xFF)), true
	}
	return ascii, true
}

// domainToASCII follows the URL Standard's "domain to ASCII" with beStrict=false.
func domainToASCII(domain string) (string, bool) {
	// ASCII だけで xn-- で始まるラベルが無ければ、UTS #46 は ASCII の小文字化と
	// 同じになる (URL Standard が明記している近道)
	fast := true
	for i := 0; i < len(domain); i++ {
		if domain[i] >= utf8.RuneSelf {
			fast = false
			break
		}
	}
	for _, label := range strings.Split(roughUTS46Map(domain), ".") {
		if strings.HasPrefix(label, "xn--") {
			// UTS #46 は xn-- で始まるラベルに非 ASCII の文字があると失敗にし、
			// Node は中身の無い `xn--` も読まない。x/net/idna はどちらも通すので、
			// ここで落とす
			if len(label) == 4 || !isASCIIString(label) {
				return "", false
			}
			fast = false
		}
	}
	if fast {
		return strings.ToLower(domain), true
	}
	// `xn---abc` のように、punycode の区切りの `-` が先頭にしかないラベルは、
	// RFC 3492 では基本文字が空というだけで正しく、Node も読むが、x/net/idna の
	// decode は失敗にする。区切りを外したラベル (`xn--abc`、同じ文字列に decode
	// される) を代わりに検査し、出力では元の綴りに戻す
	labels := strings.Split(domain, ".")
	bare := make([]bool, len(labels))
	for i, label := range labels {
		if len(label) > 5 && strings.EqualFold(label[:5], "xn---") && !strings.Contains(label[5:], "-") {
			labels[i] = label[:4] + label[5:]
			bare[i] = true
		}
	}
	out, err := urlIDNA.ToASCII(strings.Join(labels, "."))
	if err != nil {
		return "", false
	}
	outLabels := strings.Split(out, ".")
	for _, label := range outLabels {
		if len(label) > 4 && label[:4] == "xn--" {
			u, err := idna.Punycode.ToUnicode(label)
			if err != nil || !adaBidiLabelOK(u) {
				return "", false
			}
		}
	}
	restored := false
	for i, b := range bare {
		if !b {
			continue
		}
		// mapping でラベルの数が変わると元のラベルと対応が取れない。別のホストを
		// 指す href を作るより、リンクにしない方へ倒す
		if len(outLabels) != len(labels) || !strings.HasPrefix(outLabels[i], "xn--") {
			return "", false
		}
		outLabels[i] = "xn---" + outLabels[i][4:]
		restored = true
	}
	if restored {
		out = strings.Join(outLabels, ".")
	}
	return out, true
}

// roughUTS46Map approximates the UTS #46 mapping closely enough to see which
// labels start with "xn--" after mapping: the full stops that map to `.`,
// NFKC (fullwidth forms), full lowercasing (U+0130 becomes "i" + U+0307) and
// the common code points mapped to nothing.
//
// 対応付けの全体は x/net/idna の中にあり外から呼べない。ここは xn-- で始まる
// ラベルを見つけるためだけに使うので、それに効く文字だけを扱う。
func roughUTS46Map(domain string) string {
	if isASCIIString(domain) {
		return strings.ToLower(domain)
	}
	s := strings.NewReplacer("\u3002", ".", "\uff0e", ".", "\uff61", ".").Replace(domain)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == 0x00AD, r == 0x034F, r == 0x200B, r == 0x2060, r == 0xFEFF,
			0x180B <= r && r <= 0x180F, 0xFE00 <= r && r <= 0xFE0F:
			return -1
		}
		return r
	}, s)
	// cases.Caser は状態を持つので、呼ぶたびに作る
	return norm.NFKC.String(cases.Lower(language.Und).String(norm.NFKC.String(s)))
}

func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// adaBidiLabelOK reports whether Node's URL parser (ada) accepts label under
// its Bidi check.
//
// ada の検査は RFC 5893 そのものではない。Node 26 で 1〜3 文字のラベルを Bidi の
// 種別の組み合わせ全部について試した結果に合わせてある:
//
//   - 先頭が L のラベルは、最後の (NSM でない) 文字より前の文字にだけ規則 5
//     (L / EN / ES / CS / ET / ON / BN / NSM だけ) を当てる。規則 6 は見ない。
//     `aא.x` は通り、`aאא.x` は落ちる
//   - それ以外は、ラベルが R / AL / AN を含むときだけ規則 2〜4 を当てる
//     (規則 1 は見ない)。`١٢.x` と `=١.x` は通り、`١a.x`、`١=.x`、`1é١.x` は落ちる
//   - ドメイン全体が Bidi ドメインかどうかは見ない (`a=.١` は通る)
func adaBidiLabelOK(label string) bool {
	classes := make([]bidi.Class, 0, len(label))
	for _, r := range label {
		classes = append(classes, bidiClass(r))
	}
	lastNonNSM := len(classes) - 1
	for lastNonNSM >= 0 && classes[lastNonNSM] == bidi.NSM {
		lastNonNSM--
	}
	if lastNonNSM < 0 {
		return false
	}
	if classes[0] == bidi.L {
		for _, c := range classes[:lastNonNSM] {
			switch c {
			case bidi.L, bidi.EN, bidi.ES, bidi.CS, bidi.ET, bidi.ON, bidi.BN, bidi.NSM:
			default:
				return false
			}
		}
		return true
	}
	rtl := false
	for _, c := range classes {
		if c == bidi.R || c == bidi.AL || c == bidi.AN {
			rtl = true
			break
		}
	}
	if !rtl {
		return true
	}
	hasEN, hasAN := false, false
	for _, c := range classes {
		switch c {
		case bidi.R, bidi.AL, bidi.ES, bidi.CS, bidi.ET, bidi.ON, bidi.BN, bidi.NSM:
		case bidi.EN:
			hasEN = true
		case bidi.AN:
			hasAN = true
		default:
			return false
		}
	}
	if hasEN && hasAN {
		return false
	}
	switch classes[lastNonNSM] {
	case bidi.R, bidi.AL, bidi.EN, bidi.AN:
		return true
	}
	return false
}

func bidiClass(r rune) bidi.Class {
	p, _ := bidi.LookupRune(r)
	return p.Class()
}

func isForbiddenDomainByte(c byte) bool {
	if c <= 0x20 || c == 0x7F {
		return true
	}
	switch c {
	case '#', '%', '/', ':', '<', '>', '?', '@', '[', '\\', ']', '^', '|':
		return true
	}
	return false
}

// percentDecode decodes %XX sequences byte-wise and leaves any other `%` as is.
func percentDecode(s string) string {
	if strings.IndexByte(s, '%') < 0 {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isASCIIHex(s[i+1]) && isASCIIHex(s[i+2]) {
			b = append(b, hexVal(s[i+1])<<4|hexVal(s[i+2]))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}

// endsInNumber reports whether the last (non-empty-trailing) label of s is
// a number, which makes the host an IPv4 address.
func endsInNumber(s string) bool {
	parts := strings.Split(s, ".")
	if parts[len(parts)-1] == "" {
		if len(parts) == 1 {
			return false
		}
		parts = parts[:len(parts)-1]
	}
	last := parts[len(parts)-1]
	if last != "" {
		all := true
		for i := 0; i < len(last); i++ {
			if !isASCIIDigit(last[i]) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	_, ok := parseIPv4Number(last)
	return ok
}

// ipv4Overflow is a sentinel above every valid IPv4 part.
const ipv4Overflow = uint64(1) << 33

// parseIPv4Number parses one part of an IPv4 address in decimal, octal
// (leading 0) or hexadecimal (0x). Values that cannot be valid saturate at
// ipv4Overflow.
func parseIPv4Number(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	radix := uint64(10)
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s, radix = s[2:], 16
	} else if len(s) >= 2 && s[0] == '0' {
		s, radix = s[1:], 8
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d uint64
		switch {
		case isASCIIDigit(c):
			d = uint64(c - '0')
		case radix == 16 && isASCIIHex(c):
			d = uint64(hexVal(c))
		default:
			return 0, false
		}
		if d >= radix {
			return 0, false
		}
		if v < ipv4Overflow {
			v = v*radix + d
		}
		if v > ipv4Overflow {
			v = ipv4Overflow
		}
	}
	return v, true
}

// parseIPv4 follows the URL Standard's IPv4 parser.
func parseIPv4(s string) (uint32, bool) {
	parts := strings.Split(s, ".")
	if parts[len(parts)-1] == "" && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 4 {
		return 0, false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := parseIPv4Number(p)
		if !ok {
			return 0, false
		}
		nums[i] = n
	}
	for _, n := range nums[:len(nums)-1] {
		if n > 255 {
			return 0, false
		}
	}
	last := nums[len(nums)-1]
	if last >= uint64(1)<<(8*(5-len(nums))) {
		return 0, false
	}
	v := last
	for i, n := range nums[:len(nums)-1] {
		v += n << (8 * (3 - i))
	}
	return uint32(v), true
}

// parseIPv6 follows the URL Standard's IPv6 parser.
func parseIPv6(in string) ([8]uint16, bool) {
	var addr [8]uint16
	pieceIndex, compress, p := 0, -1, 0
	n := len(in)
	if p < n && in[p] == ':' {
		if p+1 >= n || in[p+1] != ':' {
			return addr, false
		}
		p += 2
		pieceIndex++
		compress = pieceIndex
	}
	for p < n {
		if pieceIndex == 8 {
			return addr, false
		}
		if in[p] == ':' {
			if compress != -1 {
				return addr, false
			}
			p++
			pieceIndex++
			compress = pieceIndex
			continue
		}
		value, length := 0, 0
		for length < 4 && p < n && isASCIIHex(in[p]) {
			value = value*16 + int(hexVal(in[p]))
			p++
			length++
		}
		if p < n && in[p] == '.' {
			if length == 0 {
				return addr, false
			}
			p -= length
			if pieceIndex > 6 {
				return addr, false
			}
			numbersSeen := 0
			for p < n {
				ipv4Piece := -1
				if numbersSeen > 0 {
					if in[p] == '.' && numbersSeen < 4 {
						p++
					} else {
						return addr, false
					}
				}
				if p >= n || !isASCIIDigit(in[p]) {
					return addr, false
				}
				for p < n && isASCIIDigit(in[p]) {
					num := int(in[p] - '0')
					switch ipv4Piece {
					case -1:
						ipv4Piece = num
					case 0:
						return addr, false
					default:
						ipv4Piece = ipv4Piece*10 + num
					}
					if ipv4Piece > 255 {
						return addr, false
					}
					p++
				}
				addr[pieceIndex] = addr[pieceIndex]*0x100 + uint16(ipv4Piece)
				numbersSeen++
				if numbersSeen == 2 || numbersSeen == 4 {
					pieceIndex++
				}
			}
			if numbersSeen != 4 {
				return addr, false
			}
			break
		} else if p < n && in[p] == ':' {
			p++
			if p >= n {
				return addr, false
			}
		} else if p < n {
			return addr, false
		}
		addr[pieceIndex] = uint16(value)
		pieceIndex++
	}
	if compress != -1 {
		swaps := pieceIndex - compress
		pieceIndex = 7
		for pieceIndex != 0 && swaps > 0 {
			addr[pieceIndex], addr[compress+swaps-1] = addr[compress+swaps-1], addr[pieceIndex]
			pieceIndex--
			swaps--
		}
	} else if pieceIndex != 8 {
		return addr, false
	}
	return addr, true
}

// serializeIPv6 compresses the first longest run (of two or more) of zero
// pieces to "::" and writes the rest in lowercase hex.
func serializeIPv6(addr [8]uint16) string {
	compress, best := -1, 1
	for i := 0; i < 8; {
		if addr[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && addr[j] == 0 {
			j++
		}
		if j-i > best {
			compress, best = i, j-i
		}
		i = j
	}
	var b strings.Builder
	ignore0 := false
	for i := 0; i < 8; i++ {
		if ignore0 && addr[i] == 0 {
			continue
		}
		ignore0 = false
		if compress == i {
			if i == 0 {
				b.WriteString("::")
			} else {
				b.WriteByte(':')
			}
			ignore0 = true
			continue
		}
		b.WriteString(strconv.FormatUint(uint64(addr[i]), 16))
		if i != 7 {
			b.WriteByte(':')
		}
	}
	return b.String()
}

func isASCIIAlpha(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }
func isASCIIHex(c byte) bool {
	return isASCIIDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case isASCIIDigit(c):
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
