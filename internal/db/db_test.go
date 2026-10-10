package db_test

import (
	"fmt"
	"strconv"
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
