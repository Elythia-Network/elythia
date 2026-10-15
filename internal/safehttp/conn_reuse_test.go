package safehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 計測の条件。既定の配送 worker 数 (mkqdriver の defaultQueueConcurrency の
// deliver = 16) が、同じ相手の shared inbox へ送り続ける形を模す。
const (
	reuseWorkers    = 16
	reusePerWorker  = 50
	reuseBodyBytes  = 2800 // Create(Note) の activity と同程度
	reuseServerWait = 2 * time.Millisecond
)

// reuseResult is what one run of the parallel-delivery simulation observed.
type reuseResult struct {
	newConns   int64 // TCP connections accepted by the server
	handshakes int64 // TLS handshakes completed by the client
	lookups    int64 // DNS lookups issued by the SSRF dialer
	elapsed    time.Duration
}

// newTLS11Server starts an HTTP/1.1-only TLS server on loopback that counts
// accepted connections. The handler drains the body, waits a little to
// stand in for the remote's processing time, and answers 202 like an inbox.
func newTLS11Server(t testing.TB, newConns *int64) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(reuseServerWait)
		w.WriteHeader(http.StatusAccepted)
	}))
	// HTTP/2 の相手は 1 本に多重化するので差が出ない。HTTP/1.1 だけを話す
	// 相手 (nginx で http2 を有効にしていない構成など) を模す。
	srv.EnableHTTP2 = false
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt64(newConns, 1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

// runParallelDelivery POSTs reuseWorkers*reusePerWorker activities to one
// remote through tr, from reuseWorkers goroutines, and reports how many
// connections, handshakes and DNS lookups that took.
//
// 宛先は名前 (example.com: httptest の証明書が持つ名前) で指定し、SSRF の
// dialer の lookup を数える hook でループバックへ向ける。
func runParallelDelivery(t testing.TB, configure func(*http.Transport)) reuseResult {
	t.Helper()
	var res reuseResult
	srv, pool := newTLS11Server(t, &res.newConns)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		atomic.AddInt64(&res.lookups, 1)
		if host != "example.com" {
			return nil, errors.New("unexpected host " + host)
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	tr := NewSSRFSafeTransport([]string{"127.0.0.1/32"}, withLookup(lookup))
	tr.TLSClientConfig = &tls.Config{
		RootCAs: pool,
		VerifyConnection: func(tls.ConnectionState) error {
			atomic.AddInt64(&res.handshakes, 1)
			return nil
		},
	}
	if configure != nil {
		configure(tr)
	}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	target := "https://" + net.JoinHostPort("example.com", port) + "/inbox"
	body := bytes.Repeat([]byte("a"), reuseBodyBytes)

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < reuseWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < reusePerWorker; i++ {
				resp, err := client.Post(target, "application/activity+json", bytes.NewReader(body))
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusAccepted {
					t.Errorf("status = %d", resp.StatusCode)
					return
				}
			}
		}()
	}
	wg.Wait()
	res.elapsed = time.Since(start)
	return res
}

// TestNewSSRFSafeTransport_ReusesConnectionsUnderParallelDelivery pins the
// per-host idle pool: 16 workers delivering to one HTTP/1.1 remote must not
// re-handshake for most requests.
//
// net/http の既定 (MaxIdleConnsPerHost = 2) では、返した接続のうち 2 本を
// 超える分が捨てられ、800 回のうち 200 回以上で TCP + TLS の握手と DNS の
// 問い合わせをやり直していた (#3591)。直した後は 20〜30 回 (worker の 16 本に、
// dial の途中で別の接続が返って余った分が idle に残る分が乗る)。
// 上限は -race と CI の混み具合で揺れても落ちないよう worker 数の 4 倍に置く。
// 既定の 2 に戻すと 200 回を超えるので、この上限でも十分に区別できる。
func TestNewSSRFSafeTransport_ReusesConnectionsUnderParallelDelivery(t *testing.T) {
	res := runParallelDelivery(t, nil)
	t.Logf("requests=%d newConns=%d handshakes=%d lookups=%d elapsed=%v",
		reuseWorkers*reusePerWorker, res.newConns, res.handshakes, res.lookups, res.elapsed)

	const limit = 4 * reuseWorkers
	assert.LessOrEqual(t, res.newConns, int64(limit), "TCP connections")
	assert.LessOrEqual(t, res.handshakes, int64(limit), "TLS handshakes")
	assert.LessOrEqual(t, res.lookups, int64(limit), "DNS lookups")
	// 接続を張るたびに検査 (DNS を引いて IP を確かめる) をしていること。
	// 数えた lookup が 0 なら、計測が SSRF の dialer を通っていない。
	assert.Positive(t, res.lookups)
	assert.Equal(t, res.newConns, res.lookups, "every new connection goes through the SSRF dial check")
}

// BenchmarkSSRFSafeTransport_ParallelDelivery reports the cost of one
// simulated delivery burst (16 workers x 50 POSTs to an HTTP/1.1 TLS remote)
// with the production idle-pool setting and with net/http's default of 2.
//
//	go test ./internal/safehttp -run '^$' -bench ParallelDelivery -benchtime 5x
func BenchmarkSSRFSafeTransport_ParallelDelivery(b *testing.B) {
	cases := []struct {
		name      string
		configure func(*http.Transport)
	}{
		{"production", nil},
		{"netHTTPDefaultPerHost2", func(tr *http.Transport) { tr.MaxIdleConnsPerHost = 0 }},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			var conns, handshakes, lookups int64
			for i := 0; i < b.N; i++ {
				res := runParallelDelivery(b, tc.configure)
				conns += res.newConns
				handshakes += res.handshakes
				lookups += res.lookups
			}
			b.ReportMetric(float64(conns)/float64(b.N), "conns/op")
			b.ReportMetric(float64(handshakes)/float64(b.N), "handshakes/op")
			b.ReportMetric(float64(lookups)/float64(b.N), "lookups/op")
		})
	}
}

// TestSSRFSafeTransport_ReusedTransportStillBlocksPrivateDestinations checks
// that keeping connections alive does not let a later request skip the dial
// check: after a successful request to an allowed remote leaves an idle
// connection in the pool, requests to names resolving to private addresses
// are still rejected by the same transport.
//
// 接続の使い回しの単位は (scheme, host:port, proxy) なので、別の名前は idle の
// 接続を拾わずに DialContext を通る。ここが変わると検査を飛ばせてしまう。
func TestSSRFSafeTransport_ReusedTransportStillBlocksPrivateDestinations(t *testing.T) {
	var newConns int64
	srv, pool := newTLS11Server(t, &newConns)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	var mu sync.Mutex
	resolve := map[string]string{
		"example.com":       "127.0.0.1",       // allowedPrivateNetworks で許可した宛先
		"loopback2.example": "127.0.0.2",       // 同じ loopback でも許可外
		"metadata.example":  "169.254.169.254", // cloud metadata
		"rfc1918.example":   "10.0.0.1",
	}
	var lookups []string
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		mu.Lock()
		defer mu.Unlock()
		lookups = append(lookups, host)
		ip, ok := resolve[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}
	tr := NewSSRFSafeTransport([]string{"127.0.0.1/32"}, withLookup(lookup))
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	t.Cleanup(tr.CloseIdleConnections)
	// 用途ごとにタイムアウトだけを変えた client が 1 つの Transport を共有する形。
	long := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	short := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	get := func(c *http.Client, host string) error {
		resp, err := c.Get("https://" + net.JoinHostPort(host, port) + "/")
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}

	require.NoError(t, get(long, "example.com"))
	require.NoError(t, get(short, "example.com"))
	assert.Equal(t, int64(1), atomic.LoadInt64(&newConns), "the second request reuses the idle connection")

	for _, host := range []string{"loopback2.example", "metadata.example", "rfc1918.example"} {
		for _, c := range []*http.Client{long, short} {
			err := get(c, host)
			require.Error(t, err, host)
			assert.ErrorIs(t, err, ErrSSRFBlocked, host)
		}
	}
	// 拒否した宛先へは接続を張っていない (サーバーに届いた接続は最初の 1 本だけ)。
	assert.Equal(t, int64(1), atomic.LoadInt64(&newConns))

	// 許可した宛先は、拒否が挟まった後も同じ idle の接続で通る。
	require.NoError(t, get(long, "example.com"))
	assert.Equal(t, int64(1), atomic.LoadInt64(&newConns))

	mu.Lock()
	defer mu.Unlock()
	// example.com の lookup は最初の接続の 1 回だけ。拒否した名前はそれぞれ
	// client ごとに引き直している (結果を覚えて検査を省いていない)。
	assert.Equal(t, []string{
		"example.com",
		"loopback2.example", "loopback2.example",
		"metadata.example", "metadata.example",
		"rfc1918.example", "rfc1918.example",
	}, lookups)
}

// TestSSRFSafeTransport_NewConnectionRechecksAfterRebinding checks that once
// a name starts resolving to a private address, the next connection the
// transport dials for it is rejected.
//
// What it does not cover, by design: an idle connection dialed before the
// change keeps going to the address that passed the check, for up to
// IdleConnTimeout (90 seconds), because reuse skips DialContext. That is the
// same for any keep-alive pool; this test closes the idle connections first
// and pins only the re-check on the next dial.
//
// 使い回しの間は再検査しない (idle の接続が残る最大 90 秒は古い IP へ送る) が、
// 新しく張る接続は必ず解決し直して検査する、のうち後者を固定する。
func TestSSRFSafeTransport_NewConnectionRechecksAfterRebinding(t *testing.T) {
	var newConns int64
	srv, pool := newTLS11Server(t, &newConns)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	var current atomic.Value
	current.Store("127.0.0.1")
	lookup := func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(current.Load().(string))}}, nil
	}
	tr := NewSSRFSafeTransport([]string{"127.0.0.1/32"}, withLookup(lookup))
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	target := (&url.URL{Scheme: "https", Host: net.JoinHostPort("example.com", port), Path: "/"}).String()

	resp, err := client.Get(target)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())

	current.Store("10.0.0.1")
	tr.CloseIdleConnections()
	_, err = client.Get(target)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSRFBlocked)
	assert.Equal(t, int64(1), atomic.LoadInt64(&newConns))
}

// TestSSRFSafeTransport_ProxyReuseStillChecksEachDestination pins the forward
// proxy path: http:// requests to different destinations share one pooled
// connection to the proxy (net/http leaves the target out of the pool key),
// so DialContext never sees the second destination. The per-request check in
// Transport.Proxy must still reject a private destination on that reused
// connection, before anything reaches the proxy.
//
// proxy 経由では宛先が違っても proxy への接続を使い回すので、守っているのは
// DialContext ではなく tr.Proxy の callback (#3591 のレビュー)。
func TestSSRFSafeTransport_ProxyReuseStillChecksEachDestination(t *testing.T) {
	var newConns int64
	var mu sync.Mutex
	var hosts []string
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.URL.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	proxy.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt64(&newConns, 1)
		}
	}
	proxy.Start()
	t.Cleanup(proxy.Close)

	lookup := fakeLookup(map[string][]string{
		"one.example":      {"93.184.215.14"},
		"two.example":      {"93.184.215.15"},
		"internal.example": {"10.0.0.5"},
	})
	tr := NewSSRFSafeTransport(nil, WithProxy(proxy.URL, nil), withLookup(lookup))
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	get := func(u string) error {
		resp, err := client.Get(u)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}

	require.NoError(t, get("http://one.example/"))
	require.NoError(t, get("http://two.example/"))
	require.Equal(t, int64(1), atomic.LoadInt64(&newConns),
		"premise: different http destinations reuse one connection to the proxy")

	for _, u := range []string{"http://internal.example/", "http://169.254.169.254/latest/meta-data/"} {
		err := get(u)
		require.Error(t, err, u)
		assert.ErrorIs(t, err, ErrSSRFBlocked, u)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"one.example", "two.example"}, hosts, "private destinations never reach the proxy")
	assert.Equal(t, int64(1), atomic.LoadInt64(&newConns))
}

// TestNewSSRFSafeTransport_HTTP2HealthCheck pins the HTTP/2 ping settings
// that let the shared transport drop a connection that died silently.
func TestNewSSRFSafeTransport_HTTP2HealthCheck(t *testing.T) {
	tr := NewSSRFSafeTransport(nil)
	require.NotNil(t, tr.HTTP2)
	assert.Equal(t, 15*time.Second, tr.HTTP2.SendPingTimeout)
	assert.Equal(t, 10*time.Second, tr.HTTP2.PingTimeout)
	assert.True(t, tr.ForceAttemptHTTP2)
}
