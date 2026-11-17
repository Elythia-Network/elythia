package notedelivery

import (
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
)

// canonicalHost puts host in the form the server stores a remote user's host
// in: lower case, punycode, no trailing dot and no default port (443 or 80).
//
// **比べる前に本体がそろえる。** プラグインが`Bad.Example`・`bad.example.`・
// `bad.example:443`・punycodeにしていない国際化ドメインのまま返すと、完全一致の
// excludeは一致せず、外したはずのサーバーへ届く。不具合が「届く」側に出るので、
// プラグインの役目にせず本体で吸収する (#4199)。比べる相手 (利用者の`host`、
// 照会してきた相手) にも同じ変換を掛けるので、どちらの綴りの揺れでもずれない。
func canonicalHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	u := &url.URL{Scheme: "https", Host: host}
	name, port := u.Hostname(), u.Port()
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return ""
	}
	u.Host = name
	if strings.Contains(name, ":") {
		// IPv6のリテラル。括弧を戻さないと、HostPortが最後の`:`の後ろをポートと
		// 読み、`[2001:db8::1]`が`[2001:db8:]:1`になる。
		u.Host = "[" + name + "]"
	}
	// `:80`も既定のポートとして外す。比べる相手の利用者の`host`は、actorのURLの
	// schemeごとの既定のポートを外した形で持つ (idnhost.HostPort) ので、
	// `http://bad.example/`のactorは`bad.example`になる。httpsを既定として
	// `bad.example:80`を残すと、excludeに`bad.example:80`と書いたときに一致せず、
	// 外したはずのサーバーへ届く。httpsで80番を使うサーバーは現実には無いので、
	// 同じサーバーとして扱う (#4202)。
	if port != "" && port != "80" {
		u.Host = net.JoinHostPort(name, port)
	}
	return idnhost.HostPort(u)
}

// validHost reports whether host (in canonicalHost's form) names a server:
// a DNS name of letters, digits, '-' and '_' (or an IP address), with an
// optional numeric port.
//
// **ホスト名として読めない値を見分ける。** `https://bad.example`・`bad.example/`・
// `*.bad.example`は、canonicalHostを通しても利用者の`host`のどれとも一致しない
// ので、excludeに書くと外したはずのサーバーへ届く。読めない値を含む条件は、
// 読めない条件と同じく「どこにも配らない」にする (#4202)。
func validHost(host string) bool {
	if host == "" {
		return false
	}
	name, port := host, ""
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return false
		}
		addr, err := netip.ParseAddr(host[1:end])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return false
		}
		rest := host[end+1:]
		if rest == "" {
			return true
		}
		if rest[0] != ':' {
			return false
		}
		return validPort(rest[1:])
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		name, port = host[:i], host[i+1:]
		if !validPort(port) {
			return false
		}
	}
	if len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

func validPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == port
}

// canonicalHostList returns hosts in canonicalHost's form. ok is false when
// any of them is not a host name (validHost).
func canonicalHostList(hosts []string) (out []string, ok bool) {
	out = make([]string, 0, len(hosts))
	for _, h := range hosts {
		c := canonicalHost(h)
		if !validHost(c) {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}
