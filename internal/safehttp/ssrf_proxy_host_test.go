package safehttp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unicode の IDN は net/http が dial する形 (punycode) で検査すること。生の
// Unicode を resolver に渡すと必ず解決に失敗し、proxy 設定時は IDN の URL が
// 一切取得できなくなる。全角英字のような mapping 対象の綴りも、接続先の名前で
// 判定される。
func TestNewSSRFSafeTransport_ProxyResolvesIDNAsDialed(t *testing.T) {
	proxy, hosts := countingProxy(t)
	lookup := fakeLookup(map[string][]string{
		"xn--eckve.example": {"93.184.215.14"},
		"evil.example":      {"10.0.0.5"},
	})
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(lookup))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	resp, err := client.Get("http://パイ.example/a")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Len(t, *hosts, 1)

	_, err = client.Get("http://ｅｖｉｌ.example/")
	assert.ErrorIs(t, err, ErrSSRFBlocked, "mapping 後の名前 (evil.example) で判定する")
	assert.Len(t, *hosts, 1)

	// Lookup が変換できない host は検証できないので proxy に渡さない。
	_, err = client.Get("http://パイ_x.example/")
	require.Error(t, err)
	assert.Len(t, *hosts, 1)
}

// bypass の照合も正規化した host で行う (upstream の `new URL().hostname` と
// 同じ)。一覧に Unicode で書いた IDN も、大文字で書いた名前も当たる。
func TestNewSSRFSafeTransport_ProxyBypassMatchesNormalizedHost(t *testing.T) {
	var direct atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		direct.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	tu, err := url.Parse(target.URL)
	require.NoError(t, err)

	proxy, hosts := countingProxy(t)
	base := fakeLookup(map[string][]string{
		"xn--eckve.example": {"127.0.0.1"},
		"upper.example":     {"127.0.0.1"},
	})
	// 本物の resolver と同じく大文字小文字を区別しない (dial 側には net/http が
	// ASCII の host を書かれたまま渡す)。
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return base(ctx, strings.ToLower(host))
	}
	tr := NewSSRFSafeTransport([]string{"127.0.0.0/8"},
		WithProxy(proxy.URL, []string{"パイ.example", "UPPER.example"}), withLookup(lookup))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	for _, h := range []string{"パイ.example", "Upper.Example"} {
		resp, err := client.Get("http://" + h + ":" + tu.Port() + "/")
		require.NoError(t, err, h)
		_ = resp.Body.Close()
	}
	assert.Equal(t, int32(2), direct.Load(), "bypass の宛先は直接つなぐ")
	assert.Empty(t, *hosts)
}

// 通過した宛先は短い間だけ覚え、keep-alive のリクエストごとに DNS を引かない。
// 期限が切れたら引き直す。拒否した宛先は覚えない。
func TestNewSSRFSafeTransport_ProxyCachesPassedDestination(t *testing.T) {
	proxy, _ := countingProxy(t)
	var mu sync.Mutex
	calls := map[string]int{}
	base := fakeLookup(map[string][]string{
		"remote.example":   {"93.184.215.14"},
		"internal.example": {"10.0.0.5"},
	})
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		mu.Lock()
		calls[host]++
		mu.Unlock()
		return base(ctx, host)
	}
	now := time.Unix(1_700_000_000, 0)
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(lookup), withClock(clock))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	get := func(u string) error {
		resp, err := client.Get(u)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	require.NoError(t, get("http://remote.example/1"))
	require.NoError(t, get("http://Remote.Example/2"))
	mu.Lock()
	assert.Equal(t, 1, calls["remote.example"], "TTL 内は引き直さない")
	mu.Unlock()

	clockMu.Lock()
	now = now.Add(proxyCheckTTL)
	clockMu.Unlock()
	require.NoError(t, get("http://remote.example/3"))
	mu.Lock()
	assert.Equal(t, 2, calls["remote.example"], "期限切れで引き直す")
	mu.Unlock()

	assert.ErrorIs(t, get("http://internal.example/"), ErrSSRFBlocked)
	assert.ErrorIs(t, get("http://internal.example/"), ErrSSRFBlocked)
	mu.Lock()
	assert.Equal(t, 2, calls["internal.example"], "拒否は覚えない")
	mu.Unlock()
}

// 上限に達したら丸ごと捨てる (個別の追い出しはしない)。
func TestProxyCheckCache_ClearsWhenFull(t *testing.T) {
	c := newProxyCheckCache(nil)
	for i := 0; i < proxyCheckCacheMax; i++ {
		c.store(string(rune('a'+i%26)) + time.Duration(i).String())
	}
	require.Len(t, c.m, proxyCheckCacheMax)
	c.store("new.example")
	assert.Len(t, c.m, 1)
	assert.True(t, c.fresh("new.example"))
	assert.False(t, c.fresh("missing.example"))
}

// inet_aton 形式の数値表記は proxy 側の getaddrinfo が 127.0.0.1 などと解釈
// しうるので、手元の解決結果にかかわらず proxy に渡さない。厳密な dotted-quad
// と、数値で終わらない名前は従来どおり。
func TestNewSSRFSafeTransport_ProxyRejectsLooseNumericHosts(t *testing.T) {
	proxy, hosts := countingProxy(t)
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(publicLookup))
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	for _, u := range []string{
		"http://0x7f.1/",
		"http://2130706433/",
		"http://127.1/",
		"http://0177.0.0.1/",
		"http://0x7F000001/",
		"http://127.0.0.1./",
		"http://foo.0x10/",
	} {
		_, err := client.Get(u)
		assert.ErrorIs(t, err, ErrSSRFBlocked, u)
	}
	assert.Empty(t, *hosts, "緩い数値表記は proxy に届かない")

	for _, u := range []string{"http://93.184.215.14/", "http://v1.example/", "http://1x.example/"} {
		resp, err := client.Get(u)
		require.NoError(t, err, u)
		_ = resp.Body.Close()
	}
	assert.Len(t, *hosts, 3)
}

func TestEndsInNumber(t *testing.T) {
	for in, want := range map[string]bool{
		"127.1":         true,
		"2130706433":    true,
		"0x7f.1":        true,
		"a.0x":          true,
		"a.0XFF":        true,
		"1.2.3.4":       true,
		"1.2.3.4.":      true,
		"example.com":   false,
		"1x.example":    false,
		"a.0xg":         false,
		"a.":            false,
		"":              false,
		".":             false,
		"::1":           false,
		"example.com.5": true,
	} {
		assert.Equal(t, want, endsInNumber(in), in)
	}
}

func TestProxyDestHost(t *testing.T) {
	got, err := proxyDestHost("Remote.Example")
	require.NoError(t, err)
	assert.Equal(t, "remote.example", got)
	got, err = proxyDestHost("ｅｖｉｌ.example")
	require.NoError(t, err)
	assert.Equal(t, "evil.example", got)
	_, err = proxyDestHost("パイ_x.example")
	assert.Error(t, err)
}
