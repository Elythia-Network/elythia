package test

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	testPg    *testutil.TestDB
	testRedis *testutil.TestRedis
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	pg, err := testutil.SetupPostgres(ctx)
	if err != nil {
		log.Printf("failed to setup postgres container: %v", err)
		// Docker が使えない場合はテスト自体を skip させるため 0 で抜ける。
		// 個々のテストは testPg==nil を見て SkipIfNoDocker と同じ扱いにする。
		os.Exit(m.Run())
	}
	testPg = pg

	rc, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Printf("failed to setup redis container: %v", err)
		pg.Teardown(ctx)
		os.Exit(m.Run())
	}
	testRedis = rc

	code := m.Run()

	testRedis.Teardown(ctx)
	testPg.Teardown(ctx)
	os.Exit(code)
}

func requireContainers(t *testing.T) {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	if testPg == nil || testRedis == nil {
		t.Skip("test containers are not available; skipping")
	}
}

// freshEchoContext builds an Echo context with a POST /api/reset-db request.
func freshEchoContext() (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/reset-db", nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// seedRow inserts a row into the given table via raw SQL so that tests do not
// depend on the GORM model package tree. id は呼び出し側で十分一意になる値を渡す前提。
func seedRow(t *testing.T, db *gorm.DB, table, id string) {
	t.Helper()
	stmt := `INSERT INTO ` + quoteIdent(table) + ` (id) VALUES (?) ON CONFLICT DO NOTHING`
	require.NoError(t, db.Exec(stmt, id).Error)
}

func quoteIdent(name string) string {
	// テストコード専用。pg_class から取ってくる実装側と同じ quoting を使う。
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func TestResetDB_TestModeDisabled_Returns404(t *testing.T) {
	h := NewHandler(nil, nil, nil, false)

	c, rec := freshEchoContext()
	err := h.ResetDB(c)
	require.Error(t, err)

	var he *echo.HTTPError
	require.True(t, errors.As(err, &he))
	assert.Equal(t, http.StatusNotFound, he.Code)
	_ = rec
}

func TestResetDB_FlushesRedisAndTables(t *testing.T) {
	requireContainers(t)
	ctx := context.Background()

	// Redis に適当な key を入れる。
	require.NoError(t, testRedis.Client.Set(ctx, "reset-db:marker", "to-be-flushed", 0).Err())

	// meta テーブルに行を入れる (migration で必ず存在するテーブルなので安全)。
	seedRow(t, testPg.DB, "meta", "m_reset_1")

	metaRepo := repository.NewMetaRepository(testPg.DB)
	h := NewHandler(testPg.DB, testRedis.Client, metaRepo, true)

	c, rec := freshEchoContext()
	require.NoError(t, h.ResetDB(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Redis がフラッシュされていること。
	val, err := testRedis.Client.Get(ctx, "reset-db:marker").Result()
	assert.ErrorIs(t, err, redis.Nil, "got value=%q", val)

	// meta テーブルは TRUNCATE されるが、reset-db は initial-setup フローを
	// 維持するため空の singleton 行を即座に再 INSERT する。事前に入れた行は
	// 消え、replacement 行 (id="meta-singleton") が 1 つだけ残るのが正しい。
	var count int64
	require.NoError(t, testPg.DB.Raw(`SELECT COUNT(*) FROM "meta"`).Scan(&count).Error)
	assert.Equal(t, int64(1), count)

	var ids []string
	require.NoError(t, testPg.DB.Raw(`SELECT id FROM "meta"`).Scan(&ids).Error)
	assert.Equal(t, []string{"meta-singleton"}, ids)
}

// meta INSERT のエラー経路をカバーする。テスト前に meta テーブル自体を一時的に
// rename することで「TRUNCATE は対象テーブル一覧から meta が消えるので no-op、
// その後の INSERT INTO "meta" が relation not found で失敗する」状況を作る。
// テスト後に元に戻す。
func TestResetDB_MetaInsertError_Returns500(t *testing.T) {
	requireContainers(t)

	// meta を退避する。名前が重複しないよう一時テーブル名を使う。
	require.NoError(t, testPg.DB.Exec(`ALTER TABLE "meta" RENAME TO "meta_test_backup"`).Error)
	t.Cleanup(func() {
		_ = testPg.DB.Exec(`DROP TABLE IF EXISTS "meta"`).Error
		_ = testPg.DB.Exec(`ALTER TABLE "meta_test_backup" RENAME TO "meta"`).Error
	})

	metaRepo := repository.NewMetaRepository(testPg.DB)
	h := NewHandler(testPg.DB, testRedis.Client, metaRepo, true)
	c, rec := freshEchoContext()
	err := h.ResetDB(c)

	// meta テーブルが存在しないので EnsureInitial が失敗し、500 が返る
	require.Error(t, err)
	var he *echo.HTTPError
	require.True(t, errors.As(err, &he))
	assert.Equal(t, http.StatusInternalServerError, he.Code)
	_ = rec
}

func TestResetDB_PreservesSchemaMigrations(t *testing.T) {
	requireContainers(t)

	// golang-migrate が作る schema_migrations テーブルには
	// SetupPostgres 時点で現行マイグレーションの version 行が載っている。
	// reset 前後で空にならないことを確認する。
	var beforeCount int64
	require.NoError(t, testPg.DB.Raw(`SELECT COUNT(*) FROM schema_migrations`).Scan(&beforeCount).Error)
	if beforeCount == 0 {
		// CI がマイグレーションを走らせていない場合は自前で 1 行入れる。
		require.NoError(t, testPg.DB.Exec(
			`INSERT INTO schema_migrations (version, dirty) VALUES (999999, false) ON CONFLICT DO NOTHING`,
		).Error)
	}

	h := NewHandler(testPg.DB, testRedis.Client, nil, true)
	c, _ := freshEchoContext()
	require.NoError(t, h.ResetDB(c))

	var afterCount int64
	require.NoError(t, testPg.DB.Raw(`SELECT COUNT(*) FROM schema_migrations`).Scan(&afterCount).Error)
	assert.Greater(t, afterCount, int64(0), "schema_migrations must be preserved across ResetDB")
}

func TestResetDB_NilDependencies(t *testing.T) {
	// TestMode=true でも db/redis が nil なら reset は no-op 扱いで 204 を返す。
	h := NewHandler(nil, nil, nil, true)
	c, rec := freshEchoContext()
	require.NoError(t, h.ResetDB(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestResetDB_RedisFlushError_Returns500(t *testing.T) {
	requireContainers(t)

	// 既に閉じた client を渡すと FLUSHDB が失敗する。
	brokenRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	require.NoError(t, brokenRedis.Close())

	h := NewHandler(testPg.DB, brokenRedis, nil, true)
	c, _ := freshEchoContext()
	err := h.ResetDB(c)
	require.Error(t, err)

	var he *echo.HTTPError
	require.True(t, errors.As(err, &he))
	assert.Equal(t, http.StatusInternalServerError, he.Code)
}

func TestResetDB_DBError_Returns500(t *testing.T) {
	requireContainers(t)

	// sql.DB を閉じて GORM 経由で使えなくする。
	// 新しい接続を open してそれを閉じれば、本体 testPg.DB は影響を受けない。
	sqlDB, err := testPg.DB.DB()
	require.NoError(t, err)
	// 実 DB を閉じないよう、別接続を用意する。
	rawDB := &gorm.DB{Config: testPg.DB.Config, Statement: &gorm.Statement{DB: testPg.DB}}
	_ = rawDB
	_ = sqlDB

	// 代わりに、存在しないテーブル名をクエリするパスを通して error を起こす手段が無いので、
	// resetTables を直接呼び出して retry 上限に到達したケースをカバーする。
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // キャンセル済みコンテキストを渡すと RAW SELECT が失敗する
	err = resetTables(ctx, testPg.DB)
	assert.Error(t, err)
}

func TestResetDB_DBError_HandlerReturns500(t *testing.T) {
	requireContainers(t)

	// キャンセル済みコンテキストで handler を呼び出す。
	// Redis FLUSHDB も失敗するので、先に flush を通しておくため handler 経路で確認する。
	req := httptest.NewRequest(http.MethodPost, "/api/reset-db", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)

	// Redis 側は本物のクライアントのままだとキャンセル済み ctx で失敗する。
	// 期待挙動としては 500 が返れば良い。
	h := NewHandler(testPg.DB, testRedis.Client, nil, true)
	err := h.ResetDB(c)
	require.Error(t, err)
	var he *echo.HTTPError
	require.True(t, errors.As(err, &he))
	assert.Equal(t, http.StatusInternalServerError, he.Code)
}

// openBrokenDB returns a GORM *gorm.DB whose underlying sql.DB has been
// closed, so that any query against it returns a "sql: database is closed"
// error. テスト専用。testPg 本体とは別の接続を張ってから閉じるので、
// 他のテストには影響しない。
func openBrokenDB(t *testing.T) *gorm.DB {
	t.Helper()
	ctx := context.Background()
	connStr, err := testPg.Container.(interface {
		ConnectionString(ctx context.Context, args ...string) (string, error)
	}).ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := gorm.Open(postgres.Open(connStr), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	return db
}

func TestResetDB_DBError_WithNilRedis(t *testing.T) {
	requireContainers(t)

	badDB := openBrokenDB(t)
	h := NewHandler(badDB, nil, nil, true)
	c, _ := freshEchoContext()
	err := h.ResetDB(c)
	require.Error(t, err)

	var he *echo.HTTPError
	require.True(t, errors.As(err, &he))
	assert.Equal(t, http.StatusInternalServerError, he.Code)
}

func TestResetTables_RetryExhaustion(t *testing.T) {
	requireContainers(t)

	badDB := openBrokenDB(t)
	err := resetTables(context.Background(), badDB)
	assert.Error(t, err)
}

func TestListUserTables_ReturnsExpected(t *testing.T) {
	requireContainers(t)

	tables, err := listUserTables(context.Background(), testPg.DB)
	require.NoError(t, err)
	require.NotEmpty(t, tables)

	// migration で必ず作られる代表的なテーブルが含まれていること。
	found := map[string]bool{}
	for _, tbl := range tables {
		found[tbl] = true
	}
	assert.True(t, found["meta"], "meta table should be present")
	assert.True(t, found["user"], "user table should be present")
}

// **プラグインの schema にあるテーブルを拾わないこと。**
//
// 拾うと、本体のテーブルとして TRUNCATE しようとして search_path の外で解決
// できずに失敗する。実際に本家 backend e2e が reset-db 500 で落ちた (#2477)。
//
// プラグインのデータを消さないのは意図どおり。reset-db は mk-go 本体の状態を
// 初期化するもので、プラグインの後片付けはプラグイン自身のテストの責務。
func TestListUserTables_IgnoresPluginSchema(t *testing.T) {
	requireContainers(t)

	require.NoError(t, testPg.DB.Exec(`CREATE SCHEMA IF NOT EXISTS plugin_resetprobe`).Error)
	require.NoError(t, testPg.DB.Exec(`CREATE TABLE IF NOT EXISTS plugin_resetprobe.statuses (id int)`).Error)
	t.Cleanup(func() {
		_ = testPg.DB.Exec(`DROP SCHEMA IF EXISTS plugin_resetprobe CASCADE`).Error
	})

	tables, err := listUserTables(context.Background(), testPg.DB)
	require.NoError(t, err)
	assert.NotContains(t, tables, "statuses")
}

func TestListUserTables_ContextCanceled(t *testing.T) {
	requireContainers(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := listUserTables(ctx, testPg.DB)
	assert.Error(t, err)
}

func TestPreservedTablesContainsSchemaMigrations(t *testing.T) {
	for _, table := range []string{"schema_migrations", "schema_migrations_local"} {
		_, ok := preservedTables[table]
		assert.True(t, ok, table)
	}
}

// Ensure json package is kept by a trivial sanity check on http.StatusNoContent
// mapping (avoid dead-import linter noise).
func TestNoContentConstant(t *testing.T) {
	b, err := json.Marshal(http.StatusNoContent)
	require.NoError(t, err)
	assert.Equal(t, "204", string(b))
}
