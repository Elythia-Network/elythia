package idnhost

import (
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// MatchesBlockList reports whether host falls under any of patterns for a
// list whose effect is to restrict the host (blockedHosts / silencedHosts /
// mediaSilencedHosts and the like).
//
// A pattern matches when host equals it or is a subdomain of it (upstream
// `UtilityService.isBlockedHost`: `.${host}`.endsWith(`.${pattern}`)).
// In addition, **host is also compared with its port and trailing dot
// removed**, so `evil.example:8443` and `evil.example.` fall under a
// `evil.example` entry. A pattern that carries a port still matches only
// that port.
func MatchesBlockList(patterns []string, host string) bool {
	hostFull, hostBare := gateForms(host)
	if hostFull == "" {
		return false
	}
	for _, p := range patterns {
		pat, _ := gateForms(p)
		if pat == "" {
			continue
		}
		// 拒否側はポートと末尾ドットを落とした形でも照合する。upstream は
		// `user.host` に非既定ポートを残したまま suffix 一致を取るので、
		// `https://evil.example:8443/` で actor を公開するだけで `evil.example`
		// のブロックを丸ごと回避できる (ポートは相手が自由に選べる)。同じ
		// ホスト名の別ポートは同じ運営者の管理下にあるので、ブロックの意図から
		// 外す理由が無い。
		if gateSuffixMatch(hostFull, pat) || gateSuffixMatch(hostBare, pat) {
			return true
		}
	}
	return false
}

// MatchesAllowList reports whether host falls under any of patterns for a
// list whose effect is to admit the host (federationHosts under
// `federation: specified`).
//
// Unlike MatchesBlockList, **the port is not dropped**: `good.example` admits
// only the default-port authority, and `good.example:8443` must be listed
// explicitly (same as upstream `isFederationAllowedHost`). Case, trailing dot
// and IDN spelling are still folded because they name the same authority.
func MatchesAllowList(patterns []string, host string) bool {
	hostFull, _ := gateForms(host)
	if hostFull == "" {
		return false
	}
	for _, p := range patterns {
		pat, _ := gateForms(p)
		if pat == "" {
			continue
		}
		// 許可側はポートを落とさない。落とすと同じホスト名で別ポートに立てた
		// 任意のサービス (共用ホストの別テナント等) まで許可リストに入る = 許可を
		// 広げる方向に倒れるので、upstream と同じ厳密一致に留める。
		if gateSuffixMatch(hostFull, pat) {
			return true
		}
	}
	return false
}

// BareHost returns host normalized like the gate lists see it, with the port
// and a trailing dot removed (`evil.example:8443` -> `evil.example`). Returns
// "" when nothing comparable remains.
func BareHost(host string) string {
	_, bare := gateForms(host)
	return bare
}

// gateSuffixMatch is upstream's `.${host}`.endsWith(`.${pattern}`).
func gateSuffixMatch(host, pattern string) bool {
	return strings.HasSuffix("."+host, "."+pattern)
}

// gateForms normalizes a host (or list entry) into its comparable authority
// form (`name[:port]`) and its bare name without the port. Both are "" when
// nothing comparable remains.
func gateForms(s string) (full, bare string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if isASCII(s) {
		s = strings.ToLower(s)
	} else {
		// 保存済みの host は punycode (hostFromURI が Puny を通す) だが、管理画面で
		// 入れる一覧は Unicode のまま来うる。片側だけだと `パイ.example` の指定が
		// `xn--eckve.example` に当たらないので両辺を揃える。小文字化は upstream の
		// `toLowerCase()` と同じく Unicode を扱う caser で行う。**Caser は状態を
		// 持ちうるので goroutine 間で共有しない** (x/text/cases の doc)。ASCII は
		// 上の分岐で済むので、hot path (inbox 1 件ごと x 一覧の長さ) では作らない。
		s = Puny(cases.Lower(language.Und).String(s))
	}
	name, port := splitGatePort(s)
	// FQDN の末尾ドット (`evil.example.`) は同じ名前の別綴り。url.Hostname() は
	// これを残すので、剥がさないと suffix 一致から外れる。
	if !strings.HasPrefix(name, "[") {
		name = strings.TrimRight(name, ".")
	}
	if name == "" {
		return "", ""
	}
	if port != "" {
		return name + ":" + port, name
	}
	return name, name
}

// splitGatePort splits `name:port` / `[v6]:port` into name and port. A value
// that does not end in a numeric port (including a bare IPv6 literal) is
// returned whole as name. An empty port (`name:`) is dropped.
func splitGatePort(s string) (name, port string) {
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return s, ""
		}
		rest := s[end+1:]
		if rest == "" {
			return s, ""
		}
		if rest[0] == ':' && isDigits(rest[1:]) {
			return s[:end+1], rest[1:]
		}
		return s, ""
	}
	// 括弧の無いコロン 2 個以上は裸の IPv6 リテラルで、ポートとは区別できない。
	if strings.Count(s, ":") != 1 {
		return s, ""
	}
	i := strings.IndexByte(s, ':')
	if !isDigits(s[i+1:]) {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
