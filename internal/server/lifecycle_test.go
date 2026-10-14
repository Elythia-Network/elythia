package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	mkdb "github.com/elythia-network/elythia/internal/db"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/testutil"
)

// lifecycleEnv is a real PostgreSQL schema plus a real Redis for exercising
// server generations end to end.
type lifecycleEnv struct {
	cfg     *config.Config
	redis   *cache.RedisClients
	rdb     *redis.Client
	schema  string
	appName string
	// observer is a connection outside every generation, used to look at
	// pg_stat_activity and to change rows while a generation is stopped.
	observer *gorm.DB
	opens    atomic.Int32
}

// newLifecycleEnv prepares the package schema, a Redis container and a
// config whose generations connect with a test-unique application_name.
//
// pg_stat_activity は schema で絞れないので、テストごとに一意の
// application_name で絞る (CLAUDE.md Section 4 のシステムカタログの規則の趣旨)。
func newLifecycleEnv(t *testing.T) *lifecycleEnv {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	observer, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(observer)
	t.Cleanup(func() {
		if sqlDB, err := observer.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	var schema string
	require.NoError(t, observer.Raw("SELECT current_schema()").Scan(&schema).Error)

	tr, err := testutil.SetupRedis(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { tr.Teardown(context.Background()) })
	clients := &cache.RedisClients{
		Default: tr.Client, Pubsub: tr.Client, JobQueue: tr.Client,
		Timelines: tr.Client, Reactions: tr.Client,
	}
	redisOptions := config.RedisOptions{Host: tr.Host(), Port: tr.Port()}
	appName := fmt.Sprintf("elythia-test/%s/%d", t.Name(), time.Now().UnixNano())
	if len(appName) > 63 {
		appName = appName[len(appName)-63:]
	}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		JobQueueDriver: "mkq", MediaProxySecret: []byte("test-secret"),
		Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
	}
	// newServer はプロセス共有の状態を張り替える (#2795)。世代を作り直すたびに
	// 張り替わるので、テストの終わりに 1 度戻せば足りる。
	restoreProcessGlobals(t)
	return &lifecycleEnv{
		cfg: cfg, redis: clients, rdb: tr.Client, schema: schema, appName: appName,
		observer: observer,
	}
}

// openDB opens a generation's pool on the package schema, named appName.
func (e *lifecycleEnv) openDB(*config.Config) (*gorm.DB, error) {
	e.opens.Add(1)
	dsn := testutil.TestDSNForSchema(e.schema) + " application_name=" + e.appName
	return gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
}

func (e *lifecycleEnv) deps() SupervisorDeps {
	return SupervisorDeps{
		OpenDB: e.openDB,
		NewServer: func(cfg *config.Config, db *gorm.DB, rc *cache.RedisClients) (*Server, error) {
			return newServer(cfg, db, rc, nil, noopStorage)
		},
		CloseDB:      mkdb.Close,
		DrainTimeout: 5 * time.Second,
	}
}

// backends counts the PostgreSQL backends of this test's generations.
func (e *lifecycleEnv) backends(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.observer.Raw(
		"SELECT count(*) FROM pg_stat_activity WHERE application_name = ?", e.appName).Scan(&n).Error)
	return n
}

// requireNoBackends waits for the backends to go away: PostgreSQL ends a
// backend asynchronously after the client closed the socket.
func (e *lifecycleEnv) requireNoBackends(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return e.backends(t) == 0 }, 5*time.Second, 20*time.Millisecond,
		"generation の DB 接続が残っている")
}

// numPat returns how many distinct pattern subscriptions the Redis holds.
//
// PubSubService は topic ごとの SUBSCRIBE ではなく `<prefix>*` の PSUBSCRIBE を
// 1 本持つので、チャネルの NUMSUB ではなく pattern の数で購読の有無を見る。この
// Redis は環境ごとの専用なので、数はこのサーバーの世代のものだけになる。
func (e *lifecycleEnv) numPat(t *testing.T) int64 {
	t.Helper()
	n, err := e.rdb.PubSubNumPat(context.Background()).Result()
	require.NoError(t, err)
	return n
}

func serveStatus(t *testing.T, h http.Handler, method, path string) int {
	t.Helper()
	return serveRequest(t, h, method, path).Code
}

// serveRequest serves one request; POST carries an empty JSON object, which
// the API requires.
func serveRequest(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// dialStreaming opens an anonymous /streaming WebSocket through ts.
func dialStreaming(t *testing.T, ts *httptest.Server, origin string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/streaming"
	header := http.Header{"Origin": []string{origin}}
	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).Dial(url, header)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func closeCodeOf(t *testing.T, conn *websocket.Conn) int {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if errors.As(err, &ce) {
			return ce.Code
		}
		t.Fatalf("connection ended without a close frame: %v", err)
		return 0
	}
}

// TestSupervisor_RecyclesGenerationsWithoutLeaking stops and rebuilds the
// server generation twice in one process (#3463 (6a)).
//
// 各周で確かめること: 止めると DB の接続が 0 になり、古い世代の Redis の購読と
// WebSocket が閉じる (WebSocket には 1012 が届く)。作り直すと別の Server が
// 応答する。周を重ねても goroutine が増え続けない。
func TestSupervisor_RecyclesGenerationsWithoutLeaking(t *testing.T) {
	env := newLifecycleEnv(t)
	sup, err := NewSupervisor(env.cfg, env.redis, env.deps())
	require.NoError(t, err)
	t.Cleanup(func() { _ = sup.Shutdown(context.Background()) })
	sup.genMu.Lock()
	require.NoError(t, sup.gen.srv.StartBackground())
	sup.genMu.Unlock()
	ts := httptest.NewServer(sup)
	t.Cleanup(ts.Close)

	var goroutines []int
	for cycle := 0; cycle < 3; cycle++ {
		require.Equal(t, http.StatusOK, serveStatus(t, sup, http.MethodGet, "/healthz"), "cycle %d", cycle)
		// /api/meta は DB を読む。世代が実際に DB に繋がっていることの確認。
		require.Equal(t, http.StatusOK, serveStatus(t, sup, http.MethodPost, "/api/meta"), "cycle %d", cycle)
		require.Positive(t, env.backends(t), "cycle %d: 世代が DB に繋がっていない (前提)", cycle)
		require.Positive(t, env.numPat(t), "cycle %d: 世代が Redis を購読していない (前提)", cycle)
		ws := dialStreaming(t, ts, env.cfg.URL)

		sup.genMu.Lock()
		oldSrv := sup.gen.srv
		sup.genMu.Unlock()
		require.Eventually(t, func() bool { return oldSrv.streamManager.Count() == 1 },
			5*time.Second, 10*time.Millisecond)

		rep := sup.stopGeneration(http.HandlerFunc(unavailable))
		require.NoError(t, rep.Err, "cycle %d", cycle)
		assert.Zero(t, rep.InFlight)
		assert.Zero(t, rep.OpenConns)
		assert.Equal(t, stream.ServiceRestartCloseCode, closeCodeOf(t, ws), "cycle %d", cycle)
		env.requireNoBackends(t)
		assert.Zero(t, env.numPat(t), "cycle %d: 古い世代の購読が残っている", cycle)
		assert.Equal(t, http.StatusServiceUnavailable, serveStatus(t, sup, http.MethodGet, "/healthz"))

		require.NoError(t, sup.startGeneration(), "cycle %d", cycle)
		sup.genMu.Lock()
		newSrv := sup.gen.srv
		sup.genMu.Unlock()
		require.NotSame(t, oldSrv, newSrv, "cycle %d: 世代が作り直されていない", cycle)

		// goroutine の数は、終了待ちの残りが落ち着いてから測る。
		time.Sleep(500 * time.Millisecond)
		runtime.GC()
		goroutines = append(goroutines, runtime.NumGoroutine())
	}
	require.Equal(t, http.StatusOK, serveStatus(t, sup, http.MethodGet, "/healthz"))
	assert.EqualValues(t, 4, env.opens.Load())
	// 1 周ごとに増える分があれば、3 周目は 2 周目より多くなる。shutdown hook を
	// 1 つも呼ばない変異では 1 周に 10 本ずつ増えた。揺らぎ (終了処理中の
	// goroutine) を見込んで、ごく少しの差だけ許す。1 周目は初回だけ作られる
	// ものを含むので比べない。
	t.Logf("goroutines after each cycle: %v", goroutines)
	assert.LessOrEqual(t, goroutines[2], goroutines[1]+3,
		"世代を作り直すたびに goroutine が増えている: %v", goroutines)
}
