package notedelivery

import (
	"net"
	"net/url"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
)

// canonicalHost puts host in the form the server stores a remote user's host
// in: lower case, punycode, no trailing dot and no default port.
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
	if port != "" {
		u.Host = net.JoinHostPort(name, port)
	}
	return idnhost.HostPort(u)
}

func canonicalHosts(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, canonicalHost(h))
	}
	return out
}
