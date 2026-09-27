package server

import (
	"net"
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseTrustedDefault は extractIPFallback / IPExtractor wrap のテスト用に
// DefaultTrustProxy を *net.IPNet 配列に展開するヘルパ。
func parseTrustedDefault(t *testing.T) []*net.IPNet {
	t.Helper()
	nets, err := config.ParseTrustProxy(config.DefaultTrustProxy)
	require.NoError(t, err)
	return nets
}

// UDS 経由 (RemoteAddr 空 + nginx が XFF を埋めるケース) で client IP が
// 抽出できることを確認する。Echo 標準 extractor が壊れる #703 の根本原因
// に対応する fallback の主目的。
func TestExtractIPFallback_UDSWithXFF(t *testing.T) {
	trusted := parseTrustedDefault(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	// nginx -> mkgo (UDS) では RemoteAddr は "" のまま、XFF に
	// Cloudflare → nginx の経路が積まれる。
	req.RemoteAddr = ""
	req.Header.Set("X-Forwarded-For", "2405:6585:d720:e4:a090:d3d9:7372:f96a, 172.21.0.1")

	got := extractIPFallback(req, trusted)
	assert.Equal(t, "2405:6585:d720:e4:a090:d3d9:7372:f96a", got)
}

// 角括弧付き IPv6 (Cloudflare が時々送る形式) も剥がして parse できる。
func TestExtractIPFallback_BracketedIPv6(t *testing.T) {
	trusted := parseTrustedDefault(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "[2405:6585:d720:e4:a090:d3d9:7372:f96a], 172.21.0.1")

	got := extractIPFallback(req, trusted)
	assert.Equal(t, "2405:6585:d720:e4:a090:d3d9:7372:f96a", got)
}

// XFF が無く X-Real-IP のみ来るケース (一部 proxy / Cloudflare 単独) でも
// 取れる。
func TestExtractIPFallback_RealIPOnly(t *testing.T) {
	trusted := parseTrustedDefault(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Real-IP", "203.0.113.7")

	got := extractIPFallback(req, trusted)
	assert.Equal(t, "203.0.113.7", got)
}

// XFF / X-Real-IP どちらも無い場合 (例: 内部ヘルスチェック等) は空文字を
// 返す。これは fallback 経路として正しい挙動 (Echo 標準も同じ振る舞い)。
func TestExtractIPFallback_NoHeaders(t *testing.T) {
	trusted := parseTrustedDefault(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)

	got := extractIPFallback(req, trusted)
	assert.Equal(t, "", got)
}

// XFF 中に解析不能な値が混じっていても、その後ろの解析可能な untrusted IP
// を拾える (Echo 標準は早期 return で directIP を返してしまう)。
func TestExtractIPFallback_SkipsUnparseableEntries(t *testing.T) {
	trusted := parseTrustedDefault(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.42, garbage, 172.21.0.1")

	got := extractIPFallback(req, trusted)
	assert.Equal(t, "203.0.113.42", got)
}

// 全 IP が trusted ranges 内なら left-most の parseable IP を返す。
// 内部だけで完結する request 用の best-effort fallback。
func TestExtractIPFallback_AllTrustedReturnsLeftmost(t *testing.T) {
	trusted := parseTrustedDefault(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "10.1.2.3, 172.16.0.5")

	got := extractIPFallback(req, trusted)
	assert.Equal(t, "10.1.2.3", got)
}

// X-Real-IP に角括弧付き IPv6 が来てもそのまま剥がす。
func TestExtractIPFallback_RealIPBracketedIPv6(t *testing.T) {
	trusted := parseTrustedDefault(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Real-IP", "[2001:db8::1]")

	got := extractIPFallback(req, trusted)
	assert.Equal(t, "2001:db8::1", got)
}

// XFF 全部解析不能なら X-Real-IP に fallback する。
func TestExtractIPFallback_XFFAllUnparseableFallbackToRealIP(t *testing.T) {
	trusted := parseTrustedDefault(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "garbage1, garbage2")
	req.Header.Set("X-Real-IP", "203.0.113.99")

	got := extractIPFallback(req, trusted)
	assert.Equal(t, "203.0.113.99", got)
}

// 通常の TCP listen 経路 (RemoteAddr あり) では inner extractor の結果を
// そのまま返し fallback は走らない。wire path の regression を防ぐ。
func TestBuildIPExtractor_TCPDirectPath(t *testing.T) {
	extractor := buildIPExtractor(parseTrustedDefault(t))
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	// 通常 TCP では net.SplitHostPort で取れる形式
	req.RemoteAddr = "203.0.113.42:54321"

	got := extractor(req)
	assert.Equal(t, "203.0.113.42", got)
}

// inner extractor が untrusted な valid IP を返した場合そのまま返す。
// trustProxy 越しに正規にアクセスされたケース。
func TestBuildIPExtractor_TrustedProxyXFF(t *testing.T) {
	extractor := buildIPExtractor(parseTrustedDefault(t))
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.21.0.5:12345" // trusted (private)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 172.21.0.1")

	got := extractor(req)
	assert.Equal(t, "203.0.113.7", got)
}

// UDS 経由 (RemoteAddr="") で inner が空を返すケースを fallback が拾う。
// 本 PR の regression を直接 lock down する critical path。
func TestBuildIPExtractor_UDSFallbackPath(t *testing.T) {
	extractor := buildIPExtractor(parseTrustedDefault(t))
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "" // UDS
	req.Header.Set("X-Forwarded-For", "2405:6585:d720:e4:a090:d3d9:7372:f96a, 172.21.0.1")

	got := extractor(req)
	assert.Equal(t, "2405:6585:d720:e4:a090:d3d9:7372:f96a", got)
}

// trusted 引数が nil/空でも buildIPExtractor 自体は構築できる
// (trustProxy: false / [] のとき router はこの形で配線する)。
func TestBuildIPExtractor_NoTrustedRanges(t *testing.T) {
	extractor := buildIPExtractor(nil)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.42:54321"

	got := extractor(req)
	assert.Equal(t, "203.0.113.42", got)
}

func mustParseTrusted(t *testing.T, entries ...string) []*net.IPNet {
	t.Helper()
	if entries == nil {
		entries = []string{}
	}
	nets, err := config.ParseTrustProxy(entries)
	require.NoError(t, err)
	return nets
}

func xffRequest(remote, xff string) *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	return req
}

// 運営者が trustProxy を CDN の範囲だけにしたら、範囲外の private /
// loopback / link-local からの XFF は信頼しない。Echo の既定 (これらを
// 常に信頼する) が残っていると、ここで XFF の値が返っていた。
func TestBuildIPExtractor_OnlyConfiguredRangesAreTrusted(t *testing.T) {
	extractor := buildIPExtractor(mustParseTrusted(t, "203.0.113.0/24"))
	tests := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{name: "private peer outside range", remote: "10.0.0.5:1234", xff: "198.51.100.9", want: "10.0.0.5"},
		{name: "loopback peer outside range", remote: "127.0.0.1:1234", xff: "198.51.100.9", want: "127.0.0.1"},
		{name: "link-local peer outside range", remote: "169.254.1.1:1234", xff: "198.51.100.9", want: "169.254.1.1"},
		{name: "ipv6 unique-local peer outside range", remote: "[fd00::1]:1234", xff: "198.51.100.9", want: "fd00::1"},
		{name: "configured proxy is trusted", remote: "203.0.113.10:1234", xff: "198.51.100.9", want: "198.51.100.9"},
		{name: "private hop between client and trusted proxy is not skipped", remote: "203.0.113.10:1234", xff: "198.51.100.9, 10.0.0.7", want: "10.0.0.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractor(xffRequest(tt.remote, tt.xff)))
		})
	}
}

// 既定の trustProxy (upstream と同じ private + loopback) では、従来どおり
// 手前の proxy が private なら XFF のクライアントを採る。
func TestBuildIPExtractor_DefaultConfigUnchanged(t *testing.T) {
	nets, err := config.ParseTrustProxy(nil)
	require.NoError(t, err)
	extractor := buildIPExtractor(nets)
	tests := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{name: "docker bridge proxy", remote: "172.21.0.5:1234", xff: "203.0.113.7", want: "203.0.113.7"},
		{name: "loopback proxy", remote: "127.0.0.1:1234", xff: "203.0.113.7", want: "203.0.113.7"},
		{name: "ipv6 loopback proxy", remote: "[::1]:1234", xff: "2001:db8::7", want: "2001:db8::7"},
		{name: "10/8 chain", remote: "10.0.0.2:1234", xff: "203.0.113.7, 192.168.1.1", want: "203.0.113.7"},
		{name: "ipv6 unique-local proxy", remote: "[fd00::2]:1234", xff: "2001:db8::7", want: "2001:db8::7"},
		{name: "public peer cannot spoof", remote: "198.51.100.1:1234", xff: "203.0.113.7", want: "198.51.100.1"},
		{name: "no xff", remote: "172.21.0.5:1234", want: "172.21.0.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractor(xffRequest(tt.remote, tt.xff)))
		})
	}
}

// trustProxy: false / [] は XFF を一切信頼しない。
func TestBuildIPExtractor_TrustNone(t *testing.T) {
	extractor := buildIPExtractor(mustParseTrusted(t))
	assert.Equal(t, "10.0.0.5", extractor(xffRequest("10.0.0.5:1234", "198.51.100.9")))
	assert.Equal(t, "127.0.0.1", extractor(xffRequest("127.0.0.1:1234", "198.51.100.9")))
}

// 素の IP は /32 として扱い、その 1 台だけを信頼する。
func TestBuildIPExtractor_BareIP(t *testing.T) {
	extractor := buildIPExtractor(mustParseTrusted(t, "192.0.2.10"))
	assert.Equal(t, "198.51.100.9", extractor(xffRequest("192.0.2.10:1234", "198.51.100.9")))
	assert.Equal(t, "192.0.2.11", extractor(xffRequest("192.0.2.11:1234", "198.51.100.9")))
}

// 解釈できない trustProxy は起動エラーにする。以前は捨てて IPExtractor を
// 設定せずに起動し、Echo の既定 (XFF の最左を無条件に信頼) に落ちていた。
func TestNew_InvalidTrustProxyFailsStartup(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	restoreProcessGlobals(t)
	cfg := &config.Config{URL: "http://example.test", TrustProxy: []string{"not-an-ip"}}
	srv, err := New(cfg, nil, nil)
	require.Error(t, err)
	assert.Nil(t, srv)
	assert.Contains(t, err.Error(), "trustProxy")
}
