package db_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/db"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/plugin/dbresolver"
)

// testCfgFromEnv builds a minimal Config pointing at the test DB exposed via
// the TEST_DB_* env vars (set in CI; defaulted in OpenTestDB for local dev).
func testCfgFromEnv(t *testing.T) *config.Config {
	t.Helper()
	port, err := strconv.Atoi(testutil.EnvOrDefault("TEST_DB_PORT", "5432"))
	require.NoError(t, err)
	return &config.Config{
		DB: config.DBOptions{
			Host: testutil.EnvOrDefault("TEST_DB_HOST", "localhost"),
			Port: port,
			DB:   testutil.EnvOrDefault("TEST_DB_NAME", "misskey_test"),
			User: testutil.EnvOrDefault("TEST_DB_USER", "mk"),
			Pass: testutil.EnvOrDefault("TEST_DB_PASS", "mk"),
		},
	}
}

// skipIfNoTestDB skips when the test DB is not reachable. Local runs without
// a postgres container should not spuriously fail this package.
func skipIfNoTestDB(t *testing.T) {
	t.Helper()
	// このパッケージは `db.New` の接続処理そのものを試すので、testCfgFromEnv と
	// 同じ共有 schema に対して到達性を見る。OpenTestDB を使うとパッケージ専用の
	// schema を作りに行き、試したい対象と別の場所を確認することになる (#2450)。
	if _, err := testutil.OpenSharedTestDB(); err != nil {
		t.Skipf("test DB not available: %v", err)
	}
}

func TestNew_NoReplicas(t *testing.T) {
	skipIfNoTestDB(t)
	cfg := testCfgFromEnv(t)
	gdb, err := db.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, gdb)

	var one int
	require.NoError(t, gdb.Raw("SELECT 1").Scan(&one).Error)
	assert.Equal(t, 1, one)
}

func TestNew_ReplicationsDisabledIgnoresSlaves(t *testing.T) {
	skipIfNoTestDB(t)
	cfg := testCfgFromEnv(t)
	// dbReplications=false なら dbSlaves が指定されていても無視される (TS互換)
	cfg.DBReplications = false
	cfg.DBSlaves = []config.DBSlaveOptions{
		{Host: "should-not-be-used", Port: 9999, DB: "x", User: "x", Pass: "x"},
	}
	gdb, err := db.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, gdb)

	var one int
	require.NoError(t, gdb.Raw("SELECT 1").Scan(&one).Error)
}

func TestNew_RegistersReplicas(t *testing.T) {
	skipIfNoTestDB(t)
	cfg := testCfgFromEnv(t)
	// CI には専用レプリカが無いので primary 自身を replica DSN として再利用する。
	// dbresolver.Register が成功し、続く SELECT が動作することが目的。
	cfg.DBReplications = true
	cfg.DBSlaves = []config.DBSlaveOptions{
		{
			Host: cfg.DB.Host,
			Port: cfg.DB.Port,
			DB:   cfg.DB.DB,
			User: cfg.DB.User,
			Pass: cfg.DB.Pass,
		},
	}
	gdb, err := db.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, gdb)

	var one int
	require.NoError(t, gdb.Raw("SELECT 1").Scan(&one).Error)
}

func TestNew_BadDSNFails(t *testing.T) {
	cfg := &config.Config{
		DB: config.DBOptions{
			Host: "127.0.0.1",
			Port: 1, // ほぼ確実に到達不可
			DB:   "x",
			User: "x",
			Pass: "x",
		},
	}
	_, err := db.New(cfg)
	assert.Error(t, err)
	// 拒まれたのは時間切れではないので、区切りの設定へ誘導しない。
	assert.NotContains(t, err.Error(), "connectionTimeoutMillis")
}

func TestNew_PoolSettings(t *testing.T) {
	skipIfNoTestDB(t)
	cfg := testCfgFromEnv(t)
	maxOpen, maxIdle := 10, 5
	lifetime, idleTime := 120, 60
	cfg.DB.MaxOpenConns = &maxOpen
	cfg.DB.MaxIdleConns = &maxIdle
	cfg.DB.ConnMaxLifetime = &lifetime
	cfg.DB.ConnMaxIdleTime = &idleTime

	gdb, err := db.New(cfg)
	require.NoError(t, err)

	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	assert.Equal(t, 10, sqlDB.Stats().MaxOpenConnections)
}

func TestNew_PoolDefaults(t *testing.T) {
	skipIfNoTestDB(t)
	cfg := testCfgFromEnv(t)

	gdb, err := db.New(cfg)
	require.NoError(t, err)

	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	// デフォルト値25が適用される
	assert.Equal(t, 25, sqlDB.Stats().MaxOpenConnections)
}

func TestNew_QueryParamLogEnabled(t *testing.T) {
	// Logging.SQL.EnableQueryParamLog のパスを通すためのケース。
	// ログ出力自体は副作用なので接続成功だけ確認する。
	skipIfNoTestDB(t)
	cfg := testCfgFromEnv(t)
	cfg.Logging = &config.LoggingOptions{
		SQL: &config.SQLLoggingOptions{EnableQueryParamLog: true},
	}
	gdb, err := db.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, gdb)
}

// countBackends counts the server-side backends carrying appName, observed
// from a separate connection.
//
// pg_stat_activity は schema で絞れないので、テストごとに一意の
// application_name で絞る (CLAUDE.md Section 4 のシステムカタログの規則と同じ趣旨)。
func countBackends(t *testing.T, appName string) int {
	t.Helper()
	observer, err := testutil.OpenSharedTestDB()
	require.NoError(t, err)
	sqlDB, err := observer.DB()
	require.NoError(t, err)
	defer sqlDB.Close()
	var n int
	require.NoError(t, observer.Raw(
		"SELECT count(*) FROM pg_stat_activity WHERE application_name = ?", appName).Scan(&n).Error)
	return n
}

// TestClose_ClosesPrimaryAndReplicaPools proves Close leaves no backend
// behind: gdb.DB().Close() alone would keep the replica pool connected
// (#3463).
func TestClose_ClosesPrimaryAndReplicaPools(t *testing.T) {
	skipIfNoTestDB(t)
	cfg := testCfgFromEnv(t)
	cfg.DBApplicationName = fmt.Sprintf("elythia-test/dbclose/%d", time.Now().UnixNano())
	cfg.DBReplications = true
	cfg.DBSlaves = []config.DBSlaveOptions{{
		Host: cfg.DB.Host, Port: cfg.DB.Port, DB: cfg.DB.DB, User: cfg.DB.User, Pass: cfg.DB.Pass,
	}}
	gdb, err := db.New(cfg)
	require.NoError(t, err)

	// replica (SELECT の既定の行き先) と primary (Write を明示) の両方に接続を張る。
	var one int
	require.NoError(t, gdb.Raw("SELECT 1").Scan(&one).Error)
	require.NoError(t, gdb.Clauses(dbresolver.Write).Raw("SELECT 1").Scan(&one).Error)
	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	require.Positive(t, sqlDB.Stats().OpenConnections, "primary に接続が張られていない (前提)")
	require.GreaterOrEqual(t, countBackends(t, cfg.DBApplicationName), 2,
		"primary と replica の両方に接続が張られていない (前提)")

	require.NoError(t, db.Close(gdb))
	// backend の終了はクライアントが切った後に非同期で起きるので、少し待つ。
	assert.Eventually(t, func() bool { return countBackends(t, cfg.DBApplicationName) == 0 },
		5*time.Second, 50*time.Millisecond)
	// 2 回目は何もしない。
	require.NoError(t, db.Close(gdb))
	require.NoError(t, db.Close(nil))
}

// silentListener accepts connections and never answers, like a stalled
// PostgreSQL or a path that swallows packets after the handshake. It returns
// the address to dial; the connections are closed when the test ends.
func silentListener(t *testing.T, network, address string) net.Addr {
	t.Helper()
	l, err := net.Listen(network, address)
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return l.Addr()
}

// newWithin runs db.New and fails the test if it does not return within
// limit, so a missing connect timeout fails instead of hanging the package.
func newWithin(t *testing.T, cfg *config.Config, limit time.Duration) (time.Duration, error) {
	t.Helper()
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := db.New(cfg)
		done <- result{err: err, elapsed: time.Since(start)}
	}()
	select {
	case r := <-done:
		return r.elapsed, r.err
	case <-time.After(limit):
		t.Fatalf("db.New did not return within %s against a server that never answers", limit)
		return 0, nil
	}
}

func TestNew_TimesOutAgainstSilentServer(t *testing.T) {
	addr := silentListener(t, "tcp", "127.0.0.1:0").(*net.TCPAddr)
	cfg := &config.Config{DB: config.DBOptions{
		Host: "127.0.0.1", Port: addr.Port, DB: "x", User: "x", Pass: "x",
		Extra: map[string]any{"connectionTimeoutMillis": 1000},
	}}
	elapsed, err := newWithin(t, cfg, 15*time.Second)
	require.Error(t, err)
	// 1 秒で区切る。早すぎるなら別の理由で落ちている。
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	assert.Less(t, elapsed, 5*time.Second)
	msg := err.Error()
	assert.Contains(t, msg, fmt.Sprintf("PostgreSQL at 127.0.0.1:%d did not answer within 1s", addr.Port))
	assert.Contains(t, msg, "db.extra.connectionTimeoutMillis")
}

func TestNew_TimesOutAgainstSilentUnixSocket(t *testing.T) {
	// ソケットのパスは 108 バイトまでなので、短い一時ディレクトリを使う。
	dir, err := os.MkdirTemp("", "mk3632")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	silentListener(t, "unix", filepath.Join(dir, ".s.PGSQL.5432"))
	cfg := &config.Config{DB: config.DBOptions{
		Host: dir, Port: 5432, DB: "x", User: "x", Pass: "x",
		Extra: map[string]any{"connectionTimeoutMillis": 1000},
	}}
	elapsed, err := newWithin(t, cfg, 15*time.Second)
	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second)
	assert.Contains(t, err.Error(), "PostgreSQL at "+filepath.Join(dir, ".s.PGSQL.5432")+" did not answer within 1s")
}

func TestNew_ReplicaTimesOutAgainstSilentServer(t *testing.T) {
	skipIfNoTestDB(t)
	addr := silentListener(t, "tcp", "127.0.0.1:0").(*net.TCPAddr)
	cfg := testCfgFromEnv(t)
	cfg.DB.Extra = map[string]any{"connectionTimeoutMillis": 1000}
	cfg.DBReplications = true
	cfg.DBSlaves = []config.DBSlaveOptions{{Host: "127.0.0.1", Port: addr.Port, DB: "x", User: "x", Pass: "x"}}
	elapsed, err := newWithin(t, cfg, 15*time.Second)
	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second)
	assert.Contains(t, err.Error(), "failed to register db replicas (a PostgreSQL read replica (dbSlaves) did not answer within 1s")
}
