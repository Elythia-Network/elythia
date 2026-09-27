package config

import (
	"fmt"
	"net"
	"strings"
)

// DefaultTrustProxy is the default set of CIDR ranges for trusted proxies,
// matching the TypeScript Misskey defaults (private IP ranges).
var DefaultTrustProxy = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.1/32",
	"::1/128",
	"fc00::/7",
}

// trustProxyNames are the named ranges proxy-addr (used by upstream's
// Fastify trustProxy) understands.
var trustProxyNames = map[string][]string{
	"loopback":    {"127.0.0.0/8", "::1/128"},
	"linklocal":   {"169.254.0.0/16", "fe80::/10"},
	"uniquelocal": {"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"},
}

// resolveTrustProxy validates the raw trustProxy value and returns the
// normalized CIDR list.
//
//   - unset (nil): DefaultTrustProxy.
//   - list or comma-separated string: each entry is a CIDR, a bare IP
//     (treated as /32 or /128) or a proxy-addr name. An empty list or empty
//     string trusts no proxy, as with upstream (`config.trustProxy ?? default`
//     keeps `[]`, and Fastify treats an empty string as falsy).
//   - false: trusts no proxy (Fastify semantics).
//   - true / numbers: rejected, see below.
//
// Any entry that cannot be parsed is a startup error.
//
// **true は受け付けない。** Fastify の trustProxy: true は「全ての hop を信頼
// する」= X-Forwarded-For の最左をそのままクライアント IP にする設定で、
// 前段を経由せずに直接届くリクエストが IP を自由に詐称できる。rate limit /
// signin 記録 / IP 履歴がすべてこの値に依存するので、互換のために受け入れる
// より、CIDR を列挙させるほうが安全。hop 数 (number) も同じ理由で受けない —
// mk-go の extractor は信頼範囲で数えるので、hop 数を正確に写せない。
//
// **解釈できない値を黙って捨てない。** 以前は Warn して捨てており、全部捨てると
// IPExtractor が未設定のまま残って Echo の既定 (XFF の最左を無条件に信頼) に
// 落ちていた。`trustProxy: true` (weak decode で ["1"]) や CIDR 無しの IP で
// 実際にそうなる。
func resolveTrustProxy(raw any) ([]string, error) {
	var entries []string
	switch v := raw.(type) {
	case nil:
		return DefaultTrustProxy, nil
	case bool:
		if v {
			return nil, fmt.Errorf("config: trustProxy: true (trust every hop) is not supported because it lets any client spoof its IP via X-Forwarded-For; list the reverse proxy addresses/CIDRs instead, or use false to trust none")
		}
		return []string{}, nil
	case string:
		// 環境変数 (MK_TRUSTPROXY) からは常に文字列で届く。
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			return resolveTrustProxy(true)
		case "false":
			return []string{}, nil
		}
		entries = strings.Split(v, ",")
	case []string:
		entries = v
	case []any:
		entries = make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("config: trustProxy entry %v (%T) is not a string; quote addresses/CIDRs", e, e)
			}
			entries = append(entries, s)
		}
	default:
		return nil, fmt.Errorf("config: trustProxy has unsupported type %T (a hop count is not supported); list the reverse proxy addresses/CIDRs, or use false to trust none", raw)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		nets, err := parseTrustProxyEntry(e)
		if err != nil {
			return nil, err
		}
		for _, n := range nets {
			out = append(out, n.String())
		}
	}
	return out, nil
}

// parseTrustProxyEntry parses a single CIDR, bare IP or proxy-addr name.
func parseTrustProxyEntry(entry string) ([]*net.IPNet, error) {
	if names, ok := trustProxyNames[strings.ToLower(entry)]; ok {
		nets := make([]*net.IPNet, 0, len(names))
		for _, c := range names {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				return nil, fmt.Errorf("config: trustProxy name %q: %w", entry, err)
			}
			nets = append(nets, n)
		}
		return nets, nil
	}
	if strings.Contains(entry, "/") {
		_, n, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("config: invalid trustProxy entry %q: %w", entry, err)
		}
		return []*net.IPNet{n}, nil
	}
	ip := net.ParseIP(entry)
	if ip == nil {
		return nil, fmt.Errorf("config: invalid trustProxy entry %q: expected a CIDR, an IP address, or one of loopback / linklocal / uniquelocal", entry)
	}
	if v4 := ip.To4(); v4 != nil {
		return []*net.IPNet{{IP: v4, Mask: net.CIDRMask(32, 32)}}, nil
	}
	return []*net.IPNet{{IP: ip, Mask: net.CIDRMask(128, 128)}}, nil
}

// ParseTrustProxy converts trustProxy entries (CIDRs, bare IPs or proxy-addr
// names) into parsed ranges. A nil slice means DefaultTrustProxy; an empty
// non-nil slice yields no ranges (trust no proxy). Any unparsable entry is an
// error.
func ParseTrustProxy(entries []string) ([]*net.IPNet, error) {
	if entries == nil {
		entries = DefaultTrustProxy
	}
	nets := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		parsed, err := parseTrustProxyEntry(strings.TrimSpace(e))
		if err != nil {
			return nil, err
		}
		nets = append(nets, parsed...)
	}
	return nets, nil
}
