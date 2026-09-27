package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsPrivateIP_IPv4Private(t *testing.T) {
	tests := []struct {
		name string
		ip   string
	}{
		{"loopback", "127.0.0.1"},
		{"loopback high", "127.255.255.255"},
		{"10.x", "10.0.0.1"},
		{"10.x high", "10.255.255.255"},
		{"172.16.x", "172.16.0.1"},
		{"172.31.x", "172.31.255.255"},
		{"192.168.x", "192.168.1.1"},
		{"link-local", "169.254.1.1"},
		{"unspecified", "0.0.0.0"},
		{"zero network", "0.0.0.1"},
		{"CGN", "100.64.0.1"},
		{"documentation 192.0.2.x", "192.0.2.1"},
		{"documentation 198.51.100.x", "198.51.100.1"},
		{"documentation 203.0.113.x", "203.0.113.1"},
		{"benchmark 198.18.x", "198.18.0.1"},
		{"6to4 relay anycast", "192.88.99.1"},
		{"AS112 direct", "192.175.48.1"},
		{"AS112 v4", "192.31.196.1"},
		{"AMT anycast", "192.52.193.1"},
		{"multicast", "224.0.0.1"},
		{"reserved 240.x", "240.0.0.1"},
		{"broadcast", "255.255.255.255"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, isPrivateIP(net.ParseIP(tt.ip), nil), "%s should be private", tt.ip)
		})
	}
}

func TestIsPrivateIP_IPv6Private(t *testing.T) {
	tests := []struct {
		name string
		ip   string
	}{
		{"unspecified", "::"},
		{"loopback", "::1"},
		{"link-local", "fe80::1"},
		{"unique-local fd", "fd00::1"},
		{"unique-local fc", "fc00::1"},
		{"deprecated site-local", "fec0::1"},
		{"multicast", "ff00::1"},
		{"multicast all-nodes", "ff02::1"},
		{"discard", "100::1"},
		// embedded-v4 系 (NAT64 / 6to4 / rfc6145): 内部 IPv4 を埋め込んで NAT64/6to4
		// gateway 経由で到達しうるため遮断する。To4() では展開されない。
		{"rfc6145 ipv4-translatable", "0:0:0:0:ffff:0:7f00:1"},
		{"NAT64 well-known", "64:ff9b::1"},
		{"NAT64 well-known embeds loopback", "64:ff9b::7f00:1"},
		{"NAT64 local-use", "64:ff9b:1::1"},
		{"6to4", "2002::1"},
		{"6to4 embeds private v4", "2002:c0a8:1::1"},
		// 2001::/23 集約レンジ
		{"teredo", "2001::1"},
		{"benchmarking", "2001:2::1"},
		{"orchid2", "2001:20::1"},
		// documentation / special-use
		{"documentation db8", "2001:db8::1"},
		{"documentation 3fff", "3fff::1"},
		{"AS112 v6", "2620:4f:8000::1"},
		{"SRv6 SID", "5f00::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, isPrivateIP(net.ParseIP(tt.ip), nil), "%s should be private", tt.ip)
		})
	}
}

func TestIsPrivateIP_IPv4MappedIPv6(t *testing.T) {
	// ::ffff:127.0.0.1 はIPv4-mapped IPv6であり、中身の127.0.0.1で判定すべき
	ip := net.ParseIP("::ffff:127.0.0.1")
	assert.True(t, isPrivateIP(ip, nil))

	ip = net.ParseIP("::ffff:10.0.0.1")
	assert.True(t, isPrivateIP(ip, nil))
}

func TestIsPrivateIP_PublicIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
	}{
		{"Google DNS", "8.8.8.8"},
		{"Cloudflare DNS", "1.1.1.1"},
		{"random public", "203.104.209.7"},
		{"IPv6 public Google", "2001:4860:4860::8888"},
		// over-block 回帰: special-use レンジ拡張で正規 public IPv6 を誤遮断しない。
		// いずれも 2001::/23 等の拡張レンジの外 (第2 hextet > 0x01ff)。
		{"IPv6 public Cloudflare", "2606:4700:4700::1111"},
		{"IPv6 public HE tunnelbroker", "2001:470::1"},
		{"IPv6 public Quad9", "2620:fe::fe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.False(t, isPrivateIP(net.ParseIP(tt.ip), nil), "%s should be public", tt.ip)
		})
	}
}

// ipv4-mapped IPv6 の意図的 divergence を lock する: mk-go は To4() で埋め込み v4 を
// 評価するため public 埋め込み (::ffff:8.8.8.8) は許可、private 埋め込み
// (::ffff:127.0.0.1) は遮断する。upstream は ipv4-mapped を一律遮断するが、mk-go の
// 方が精密 (内部到達しうるものだけ遮断) という divergence を回帰で固定する。
func TestIsPrivateIP_IPv4MappedDivergence(t *testing.T) {
	assert.False(t, isPrivateIP(net.ParseIP("::ffff:8.8.8.8"), nil),
		"public ipv4-mapped は許可される (upstream との意図的乖離)")
	assert.True(t, isPrivateIP(net.ParseIP("::ffff:127.0.0.1"), nil),
		"private ipv4-mapped は遮断される")
	assert.True(t, isPrivateIP(net.ParseIP("::ffff:169.254.169.254"), nil),
		"link-local (cloud metadata) の ipv4-mapped は遮断される")
}

func TestIsPrivateIP_AllowedCIDR(t *testing.T) {
	_, allowedNet, _ := net.ParseCIDR("10.0.0.0/24")
	allowedNets := []*net.IPNet{allowedNet}

	// 10.0.0.1 はプライベートだが allowedNets に含まれるので許可
	assert.False(t, isPrivateIP(net.ParseIP("10.0.0.1"), allowedNets))

	// 10.1.0.1 は allowedNets に含まれないのでブロック
	assert.True(t, isPrivateIP(net.ParseIP("10.1.0.1"), allowedNets))
}

func TestIsPrivateIP_AllowedCIDR_IPv6(t *testing.T) {
	_, allowedNet, _ := net.ParseCIDR("fd00::/64")
	allowedNets := []*net.IPNet{allowedNet}

	assert.False(t, isPrivateIP(net.ParseIP("fd00::1"), allowedNets))
	assert.True(t, isPrivateIP(net.ParseIP("fd01::1"), allowedNets))
}

func TestNewSSRFSafeTransport_InvalidCIDR(t *testing.T) {
	// 不正なCIDRは無視される（panicしない）
	tr := NewSSRFSafeTransport([]string{"invalid-cidr", "10.0.0.0/8"})
	assert.NotNil(t, tr)
}

func TestNewSSRFSafeTransport_NilCIDRs(t *testing.T) {
	tr := NewSSRFSafeTransport(nil)
	assert.NotNil(t, tr)
}

func TestNewSSRFSafeTransport_BlocksLoopback(t *testing.T) {
	// httptestサーバーは127.0.0.1で起動するのでSSRF保護でブロックされるはず
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tr := NewSSRFSafeTransport(nil)
	client := &http.Client{Transport: tr}

	_, err := client.Get(ts.URL + "/test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "private IP blocked")
}

// IPv6 unspecified `::` 宛は SSRF block される。Linux では connect(::) が
// loopback (::1) にルートされるため、`http://0.0.0.0:PORT/` を弾く保護を
// `http://[::]:PORT/` で回避できてはならない (`::/128` を privateRanges に
// 追加して 0.0.0.0/8 と対称化した)。
func TestNewSSRFSafeTransport_BlocksIPv6Unspecified(t *testing.T) {
	tr := NewSSRFSafeTransport(nil)
	client := &http.Client{Transport: tr}

	// dial 前の isPrivateIP(::) 判定で弾かれるため実際の接続は発生しない。
	_, err := client.Get("http://[::]:80/test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "private IP blocked")
}

// NAT64 well-known prefix (64:ff9b::/96) 宛は SSRF block される。末尾 32bit に
// 内部 IPv4 を埋め込めるため、NAT64 gateway 経由の内部到達を dial 前に遮断する。
func TestNewSSRFSafeTransport_BlocksNAT64(t *testing.T) {
	tr := NewSSRFSafeTransport(nil)
	client := &http.Client{Transport: tr}

	// 64:ff9b::7f00:1 = NAT64 representation of 127.0.0.1
	_, err := client.Get("http://[64:ff9b::7f00:1]:80/test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "private IP blocked")
}

func TestNewSSRFSafeTransport_AllowsLoopbackWithCIDR(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tr := NewSSRFSafeTransport([]string{"127.0.0.0/8"})
	client := &http.Client{Transport: tr}

	resp, err := client.Get(ts.URL + "/test")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNewSSRFSafeTransport_DNSResolutionFailure(t *testing.T) {
	tr := NewSSRFSafeTransport(nil)
	client := &http.Client{Transport: tr}

	_, err := client.Get("http://nonexistent.invalid/test")
	assert.Error(t, err)
}

// proxy 経路で外向きリクエストが proxy server に届くこと、proxy 自体が
// プライベート IP でも SSRF block されないこと (#485)。
func TestNewSSRFSafeTransport_ForwardsThroughProxy(t *testing.T) {
	hits := 0
	// proxy 役の httptest server。HTTP の forward proxy は absolute-form
	// URL (例: GET http://example.com/foo HTTP/1.1) を受け取るので
	// req.URL を見て応答するだけで proxy として振る舞える。
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		// proxy は absolute-form URL を受け取る
		assert.Equal(t, "example.com", r.URL.Host)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("via-proxy"))
	}))
	defer proxy.Close()

	// httptest は 127.0.0.1 で起動するので allowedCIDRs 無しでも proxy
	// 接続自体は SSRF skip される (proxyAddr 一致)。
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(publicLookup))
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://example.com/path")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, hits)
}

// bypass list に列挙された host は proxy を経由せず direct dial になる。
// direct dial は SSRF check 対象なので、loopback への bypass は CIDR 許可
// が無い限りブロックされる。
func TestNewSSRFSafeTransport_BypassRoutesDirectAndKeepsSSRF(t *testing.T) {
	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	directHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	targetURL, err := url.Parse(target.URL)
	require.NoError(t, err)
	// bypass で direct dial になるが、target も loopback なので SSRF を
	// 許可するため CIDR 127.0.0.0/8 を allowedCIDRs に入れる。
	tr := NewSSRFSafeTransport(
		[]string{"127.0.0.0/8"},
		WithProxy(proxy.URL, []string{targetURL.Hostname()}),
	)
	client := &http.Client{Transport: tr}

	resp, err := client.Get(target.URL + "/x")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 0, proxyHits, "bypass-listed host must not go through proxy")
	assert.Equal(t, 1, directHits)
}

// proxy URL が不正な場合は proxy 設定が無視され direct dial にフォール
// バックする (起動時に panic させない)。
func TestNewSSRFSafeTransport_InvalidProxyURLFallsBack(t *testing.T) {
	tr := NewSSRFSafeTransport(nil, WithProxy("://not-a-url", nil))
	assert.NotNil(t, tr)
	assert.Nil(t, tr.Proxy, "invalid proxy URL must leave Transport.Proxy unset")
}

// WithProxy("", ...) は no-op (proxy 不使用)。
func TestNewSSRFSafeTransport_EmptyProxyURLNoOp(t *testing.T) {
	tr := NewSSRFSafeTransport(nil, WithProxy("", []string{"example.com"}))
	assert.Nil(t, tr.Proxy, "empty proxy URL must not enable Transport.Proxy")
}

// IPv6 host の proxy URL に明示 port が無いとき、u.Host は "[::1]"
// のように bracket 付きで返る。これを net.JoinHostPort にそのまま渡すと
// "[[::1]]:80" の二重 bracket になり、後段の DialContext 比較で proxy
// 一致と判定されず SSRF check が走る → loopback (::1) として block
// されてしまう。u.Hostname() で剥がしてから net.JoinHostPort に渡す
// ことで http.Transport が dial する正規形 ("[::1]:80") と一致する。
//
// 直接 proxyAddr を assert できないので、実 dial を試して
// ErrSSRFBlocked が返らない (= proxy 一致判定が機能している) ことで
// 間接的に検証する。dial 自体はループバック上の閉じた port なので
// "connection refused" 系のエラーになるが、それは bug fix の対象外。
// #496: WithOutgoingAddress / WithAddressFamily の挙動を確認する。
// LocalAddr は実 bind しないと kernel が valid 検証しないので、Transport
// 構築 + Dialer 経由で「不正値は無視」「正常値が反映される」だけ確認。

// 不正な IP 文字列を渡しても panic せず Transport は使えるまま。
// (内部 Dialer.LocalAddr は nil のまま = kernel auto-pick)。
func TestNewSSRFSafeTransport_OutgoingAddressInvalidIgnored(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tr := NewSSRFSafeTransport([]string{"127.0.0.0/8"}, WithOutgoingAddress("not-an-ip"))
	client := &http.Client{Transport: tr}
	resp, err := client.Get(ts.URL)
	require.NoError(t, err)
	resp.Body.Close()
}

// 正常な loopback IP を LocalAddr に指定してもループバック宛なら通る
// (実 bind を kernel が許可する経路)。
func TestNewSSRFSafeTransport_OutgoingAddressLoopbackBinds(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tr := NewSSRFSafeTransport([]string{"127.0.0.0/8"}, WithOutgoingAddress("127.0.0.1"))
	client := &http.Client{Transport: tr}
	resp, err := client.Get(ts.URL)
	require.NoError(t, err)
	resp.Body.Close()
}

// addressFamily=ipv4 で IPv6 のみ解決される host への接続を弾く。
// httptest の listener は IPv4 にバインドされるが、LookupIPAddr が ::1 も
// 返してくるかは環境次第。確実な検証には normalize/match を直叩きする。
func TestNormalizeFamily(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ipv4", "ipv4"},
		{"IPv4", "ipv4"},
		{"ipv6", "ipv6"},
		{" IPv6 ", "ipv6"},
		{"dual", ""},
		{"", ""},
		{"garbage", ""},
	}
	for _, tc := range cases {
		if got := normalizeFamily(tc.in); got != tc.want {
			t.Errorf("normalizeFamily(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMatchFamily(t *testing.T) {
	if !matchFamily(net.ParseIP("1.2.3.4"), "ipv4") {
		t.Error("1.2.3.4 should match ipv4")
	}
	if matchFamily(net.ParseIP("1.2.3.4"), "ipv6") {
		t.Error("1.2.3.4 should NOT match ipv6")
	}
	if !matchFamily(net.ParseIP("2001:db8::1"), "ipv6") {
		t.Error("2001:db8::1 should match ipv6")
	}
	// IPv4-mapped IPv6 は ipv4 として扱う。
	if !matchFamily(net.ParseIP("::ffff:1.2.3.4"), "ipv4") {
		t.Error("::ffff:1.2.3.4 should match ipv4 (4-in-6)")
	}
}

// addressFamily=ipv4 でも 127.0.0.1 自体は IPv4 なので解決され、その後の
// SSRF check で private IP として block される。family filter 経路を抜けて
// 後段の SSRF 判定に到達することの確認。
func TestNewSSRFSafeTransport_AddressFamilyIPv4PassesThrough(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tr := NewSSRFSafeTransport(nil, WithAddressFamily("ipv4"))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	_, err := client.Get(ts.URL)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSRFBlocked, "ipv4 family must reach SSRF check, not bypass it")
}

// addressFamily=ipv6 で IPv4-only host (httptest) を解決すると
// no ipv6 address エラーで dial 失敗。
func TestNewSSRFSafeTransport_AddressFamilyIPv6FiltersOutIPv4(t *testing.T) {
	tr := NewSSRFSafeTransport([]string{"127.0.0.0/8"}, WithAddressFamily("ipv6"))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	// IPv4-only な test 用ドメイン: localhost は実際 ::1 も返すので使えない。
	// 127.0.0.1 を直接渡す (IPv4 数値リテラル → DNS 不要だが Go の resolver
	// が wrap で 127.0.0.1 を返す経路 = familyFilter が IPv4 を弾く)。
	_, err := client.Get("http://127.0.0.1:1/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ipv6 address",
		"ipv6 family must drop the only-IPv4 resolution result")
}

func TestNewSSRFSafeTransport_IPv6ProxyDefaultPort(t *testing.T) {
	// #486 regression は「`http://[::1]` (= port 80 default) を proxy URL に
	// 指定したとき SSRF guard が `[[::1]]:80` と二重 bracket せず正しく
	// `[::1]:80` で比較できる」ことを「dial が SSRF block 以外のエラーで
	// 失敗する」観測経由で覆う。原作者コメント (fef6210e) のとおり「dial 自体は
	// ループバック上の閉じた port なので connection refused 系のエラーになる」
	// 前提に依存しているため、開発機で [::]:80 に nginx 等が listen していると
	// dial が成功して err=nil になり require.Error が trip する。
	// CI runner は port 80 に何も listen していないので無条件に通る。
	// 開発機向けの fail-safe として probe を入れ、listener 検出時は skip する。
	if conn, perr := net.DialTimeout("tcp6", "[::1]:80", 200*time.Millisecond); perr == nil {
		_ = conn.Close()
		t.Skip("something is listening on [::1]:80 (e.g. local nginx); cannot observe #486 regression via dial-fail branch")
	}

	tr := NewSSRFSafeTransport(nil, WithProxy("http://[::1]", nil), withLookup(publicLookup))
	require.NotNil(t, tr)
	require.NotNil(t, tr.Proxy)

	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	_, err := client.Get("http://example.com/")
	require.Error(t, err, "dial to ::1:80 will fail (no listener) but must not be SSRF-blocked")
	assert.NotErrorIs(t, err, ErrSSRFBlocked,
		"proxy connection must skip SSRF check even when proxy host is IPv6 loopback without explicit port")
}

// publicLookup resolves every host (IP literals included) to a public address
// so proxy tests do not depend on the sandbox's DNS.
func publicLookup(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.215.14")}}, nil
}

// fakeLookup は host ごとの解決結果を返す resolver。IP リテラルは本物の
// resolver と同じくそのまま返す。
func fakeLookup(table map[string][]string) lookupFunc {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IPAddr{{IP: ip}}, nil
		}
		addrs, ok := table[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		out := make([]net.IPAddr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, net.IPAddr{IP: net.ParseIP(a)})
		}
		return out, nil
	}
}

// countingProxy は受け取った absolute-form URL の host を記録する forward proxy。
func countingProxy(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var hosts []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.URL.Host)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)
	return proxy, &hosts
}

// proxy 経由では mk-go が宛先へ dial しないので、宛先の検査は proxy へ渡す
// 前に行わなければならない。内部 IP リテラル / 内部へ解決されるホスト名 /
// 一部だけ内部を含む解決結果はどれも proxy に届かず ErrSSRFBlocked になる。
func TestNewSSRFSafeTransport_ProxyBlocksPrivateDestinations(t *testing.T) {
	lookup := fakeLookup(map[string][]string{
		"internal.example": {"10.0.0.5"},
		"mixed.example":    {"93.184.215.14", "169.254.169.254"},
		"v6.example":       {"::1"},
	})
	cases := []struct {
		name string
		url  string
	}{
		{"metadata literal", "http://169.254.169.254/latest/meta-data/"},
		{"rfc1918 literal with port", "http://10.0.0.5:9200/"},
		{"ipv6 loopback literal", "http://[::1]:8080/"},
		{"https literal via CONNECT", "https://127.0.0.1/"},
		{"hostname resolving to private", "http://internal.example/"},
		{"hostname with one private address", "http://mixed.example/"},
		{"hostname resolving to ipv6 loopback", "http://v6.example/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxy, hosts := countingProxy(t)
			tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(lookup))
			client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

			_, err := client.Get(tc.url)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSSRFBlocked)
			assert.Empty(t, *hosts, "private destination must not reach the proxy")
		})
	}
}

// 解決できない宛先は検証できないので proxy に渡さない (fail closed)。
func TestNewSSRFSafeTransport_ProxyBlocksUnresolvableDestination(t *testing.T) {
	proxy, hosts := countingProxy(t)
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(fakeLookup(nil)))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	_, err := client.Get("http://nowhere.example/")
	require.Error(t, err)
	var dnsErr *net.DNSError
	assert.True(t, errors.As(err, &dnsErr), "lookup failure must surface: %v", err)
	assert.Empty(t, *hosts)
}

// 外部アドレスへ解決される宛先は従来どおり proxy に届く。
func TestNewSSRFSafeTransport_ProxyForwardsPublicDestination(t *testing.T) {
	proxy, hosts := countingProxy(t)
	lookup := fakeLookup(map[string][]string{"remote.example": {"93.184.215.14", "2606:2800:21f:cb07:6820:80da:af6b:8b2c"}})
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(lookup))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	for _, u := range []string{"http://remote.example/a", "http://93.184.215.14:8080/b"} {
		resp, err := client.Get(u)
		require.NoError(t, err, u)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}
	assert.Equal(t, []string{"remote.example", "93.184.215.14:8080"}, *hosts)
}

// allowedPrivateNetworks に入れた範囲は proxy 経由でも許可される (dial 時の
// 判定と同じ関数を使う)。範囲外の private は引き続き拒否。
func TestNewSSRFSafeTransport_ProxyHonoursAllowedPrivateNetworks(t *testing.T) {
	proxy, hosts := countingProxy(t)
	lookup := fakeLookup(map[string][]string{"es.internal": {"10.0.0.5"}})
	tr := NewSSRFSafeTransport([]string{"10.0.0.0/24"}, WithProxy(proxy.URL, nil), withLookup(lookup))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	for _, u := range []string{"http://10.0.0.5:9200/", "http://es.internal/"} {
		resp, err := client.Get(u)
		require.NoError(t, err, u)
		_ = resp.Body.Close()
	}
	assert.Equal(t, []string{"10.0.0.5:9200", "es.internal"}, *hosts)

	_, err := client.Get("http://10.0.1.5/")
	assert.ErrorIs(t, err, ErrSSRFBlocked)
	assert.Len(t, *hosts, 2, "address outside the allowlist must not reach the proxy")
}

// redirect 先も proxy へ渡す前に検査される (Transport.Proxy はリクエスト毎に
// 呼ばれる)。外部 → 内部への redirect は proxy に 1 回目しか届かない。
func TestNewSSRFSafeTransport_ProxyChecksRedirectTarget(t *testing.T) {
	var hosts []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.URL.Host)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer proxy.Close()

	lookup := fakeLookup(map[string][]string{"remote.example": {"93.184.215.14"}})
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(lookup))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	_, err := client.Get("http://remote.example/start")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSRFBlocked)
	assert.Equal(t, []string{"remote.example"}, hosts)
}

// bypass 経路は proxy を経由せず、dial 時の検査が効く (resolver hook も dial 側に効く)。
func TestNewSSRFSafeTransport_BypassHostStillCheckedAtDial(t *testing.T) {
	proxy, hosts := countingProxy(t)
	lookup := fakeLookup(map[string][]string{"bypass.example": {"127.0.0.1"}})
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, []string{"bypass.example"}), withLookup(lookup))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	_, err := client.Get("http://bypass.example/")
	assert.ErrorIs(t, err, ErrSSRFBlocked)
	assert.Empty(t, *hosts)
}

// http.Transport は Host の無い URL を Proxy callback より前に弾くので、
// 空 host の枝は直接呼んで固定する。
func TestCheckProxiedHost_EmptyHost(t *testing.T) {
	err := checkProxiedHost(context.Background(), publicLookup, "", nil)
	assert.ErrorIs(t, err, ErrSSRFBlocked)
}
