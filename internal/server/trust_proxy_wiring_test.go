package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a goroutine-safe log sink; the server under test logs
// from background goroutines while the test reads the buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureWarnLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })
	return buf
}

// newServerWithTrustProxy builds a full server through New with the given
// trustProxy so the IPExtractor wiring in newServer is exercised.
func newServerWithTrustProxy(t *testing.T, trustProxy []string) *Server {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	redisClients := &cache.RedisClients{
		Default: redisClient, Pubsub: redisClient, JobQueue: redisClient,
		Timelines: redisClient, Reactions: redisClient,
	}
	redisPort, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	redisOptions := config.RedisOptions{Host: mr.Host(), Port: redisPort}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
		TrustProxy: trustProxy,
	}
	// **`New` はプロセス共有の状態を張り替える** (#2795)。
	restoreProcessGlobals(t)
	srv, err := New(cfg, serverIntegrationDB, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return srv
}

func realIPOf(srv *Server, req *http.Request) string {
	return srv.echo.NewContext(req, httptest.NewRecorder()).RealIP()
}

// trustProxy: false / [] でも IPExtractor は必ず設定する。未設定のまま残すと
// Echo の RealIP は XFF の最左を無条件に信頼するので、誰でも IP を詐称できる
// (以前は len(nets) > 0 のときだけ設定していた)。
func TestNew_EmptyTrustProxyStillSetsIPExtractor(t *testing.T) {
	logs := captureWarnLogs(t)
	srv := newServerWithTrustProxy(t, []string{})
	require.NotNil(t, srv.echo.IPExtractor)
	assert.Equal(t, "10.0.0.5", realIPOf(srv, xffRequest("10.0.0.5:1234", "198.51.100.9")))
	assert.Equal(t, "203.0.113.1", realIPOf(srv, xffRequest("203.0.113.1:1234", "198.51.100.9")))
	// 何も信頼しない構成は前段 proxy が private から繋ぐと壊れるので、起動時に知らせる。
	assert.Contains(t, logs.String(), "trustProxy trusts no loopback or private address")
}

// 既定値 (private + loopback) では起動時の警告を出さない。
func TestNew_DefaultTrustProxyDoesNotWarn(t *testing.T) {
	logs := captureWarnLogs(t)
	srv := newServerWithTrustProxy(t, nil)
	assert.Equal(t, "198.51.100.9", realIPOf(srv, xffRequest("172.21.0.5:1234", "198.51.100.9")))
	assert.NotContains(t, logs.String(), "trustProxy trusts no loopback")
}

func TestTrustsLocalProxyRange(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    bool
	}{
		{name: "default", entries: nil, want: true},
		{name: "trust none", entries: []string{}, want: false},
		{name: "cdn only", entries: []string{"203.0.113.0/24", "2001:db8::/32"}, want: false},
		{name: "link-local only", entries: []string{"linklocal"}, want: false},
		{name: "uniquelocal name", entries: []string{"203.0.113.0/24", "uniquelocal"}, want: true},
		{name: "loopback name", entries: []string{"loopback"}, want: true},
		{name: "single docker bridge address", entries: []string{"172.21.0.5"}, want: true},
		{name: "ipv6 unique-local subnet", entries: []string{"fd12:3456::/48"}, want: true},
		{name: "range covering private space", entries: []string{"0.0.0.0/0"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nets, err := config.ParseTrustProxy(tt.entries)
			require.NoError(t, err)
			assert.Equal(t, tt.want, trustsLocalProxyRange(nets))
		})
	}
}

func TestWarnTrustProxyWithoutLocalRanges(t *testing.T) {
	t.Run("cdn only warns with hint", func(t *testing.T) {
		logs := captureWarnLogs(t)
		warnTrustProxyWithoutLocalRanges(mustParseTrusted(t, "203.0.113.0/24"), "")
		out := logs.String()
		assert.Contains(t, out, "trustProxy trusts no loopback or private address")
		assert.Contains(t, out, "uniquelocal")
		assert.Contains(t, out, "203.0.113.0/24")
	})
	t.Run("private proxy trusted is quiet", func(t *testing.T) {
		logs := captureWarnLogs(t)
		warnTrustProxyWithoutLocalRanges(mustParseTrusted(t, "203.0.113.0/24", "172.16.0.0/12"), "")
		assert.Empty(t, logs.String())
	})
	// UDS では接続元アドレスが無く fallback が XFF を読むので、この構成でも壊れない。
	t.Run("unix socket is quiet", func(t *testing.T) {
		logs := captureWarnLogs(t)
		warnTrustProxyWithoutLocalRanges(mustParseTrusted(t, "203.0.113.0/24"), "/run/mk/mk.sock")
		assert.Empty(t, logs.String())
	})
}

// 信頼しない private / loopback から XFF 付きで届いたら、前段 proxy を
// trustProxy に書き忘れている可能性が高いので知らせる。ただし 1 回だけ。
func TestBuildIPExtractor_WarnsOnceForUntrustedLocalProxy(t *testing.T) {
	logs := captureWarnLogs(t)
	extractor := buildIPExtractor(mustParseTrusted(t, "203.0.113.0/24"))

	// 対象外: XFF 無し / 公開アドレス / 信頼済みの proxy。
	assert.Equal(t, "10.0.0.5", extractor(xffRequest("10.0.0.5:1234", "")))
	assert.Equal(t, "198.51.100.1", extractor(xffRequest("198.51.100.1:1234", "192.0.2.9")))
	assert.Equal(t, "192.0.2.9", extractor(xffRequest("203.0.113.10:1234", "192.0.2.9")))
	assert.Empty(t, logs.String())

	assert.Equal(t, "172.21.0.5", extractor(xffRequest("172.21.0.5:1234", "192.0.2.9")))
	assert.Equal(t, "127.0.0.1", extractor(xffRequest("127.0.0.1:1234", "192.0.2.9")))
	assert.Equal(t, "fd00::1", extractor(xffRequest("[fd00::1]:1234", "192.0.2.9")))
	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, "received X-Forwarded-For from a loopback or private address"), out)
	assert.Contains(t, out, "remoteAddr=172.21.0.5")
}

// TCP 経路で XFF を右から走査する途中に解析できない値があると、echo は
// その右隣ではなく接続元アドレスを返す。docs/divergence.md の記述を固定する。
func TestBuildIPExtractor_UnparsableXFFEntryYieldsDirectIP(t *testing.T) {
	extractor := buildIPExtractor(mustParseTrusted(t, "10.0.0.0/8"))
	assert.Equal(t, "10.0.0.2", extractor(xffRequest("10.0.0.2:1234", "203.0.113.7, garbage, 10.0.0.9")))
	assert.Equal(t, "10.0.0.2", extractor(xffRequest("10.0.0.2:1234", "garbage")))
	// 解析できない値より右に untrusted があれば、そちらが先に採られる。
	assert.Equal(t, "203.0.113.8", extractor(xffRequest("10.0.0.2:1234", "garbage, 203.0.113.8")))
}

// UDS では trustProxy: false / [] でも XFF (無ければ X-Real-IP) を読む。
// docs/configuration.md の表が注記している例外を固定する。
func TestBuildIPExtractor_UDSReadsHeadersEvenWhenTrustingNone(t *testing.T) {
	extractor := buildIPExtractor(mustParseTrusted(t))
	assert.Equal(t, "192.0.2.9", extractor(xffRequest("", "198.51.100.1, 192.0.2.9")))
	req := xffRequest("", "")
	req.Header.Set("X-Real-IP", "192.0.2.10")
	assert.Equal(t, "192.0.2.10", extractor(req))
}

// 信頼している private proxy からの XFF では警告しない (既定構成)。
// UDS (RemoteAddr が空) も対象外。
func TestBuildIPExtractor_NoWarnForTrustedLocalProxy(t *testing.T) {
	logs := captureWarnLogs(t)
	extractor := buildIPExtractor(mustParseTrusted(t, "172.16.0.0/12"))
	assert.Equal(t, "192.0.2.9", extractor(xffRequest("172.21.0.5:1234", "192.0.2.9")))
	assert.Equal(t, "192.0.2.9", extractor(xffRequest("", "192.0.2.9")))
	assert.Empty(t, logs.String())
}
