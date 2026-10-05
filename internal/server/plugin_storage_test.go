package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/pluginstore"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

// testDBConfig points a Config at the shared test database, so dbBackedStorage
// builds a real DSN through the same path production uses.
//
// **本番と同じ経路を通すのが目的。** 生成した DSN を test だけ別に組み立てると、
// config.DSN() 側の変更で本番だけ壊れる形になる。
func testDBConfig(t *testing.T) *config.Config {
	t.Helper()
	port, err := strconv.Atoi(testutil.EnvOrDefault("TEST_DB_PORT", "5432"))
	require.NoError(t, err)

	return &config.Config{
		URL: "https://example.com",
		DB: config.DBOptions{
			Host: testutil.EnvOrDefault("TEST_DB_HOST", "localhost"),
			Port: port,
			User: testutil.EnvOrDefault("TEST_DB_USER", "mk"),
			Pass: testutil.EnvOrDefault("TEST_DB_PASS", "mk"),
			DB:   testutil.EnvOrDefault("TEST_DB_NAME", "misskey_test"),
		},
	}
}

// dropPluginSchema removes the schema a test created.
func dropPluginSchema(t *testing.T, cfg *config.Config, name string) {
	t.Helper()
	schema, err := pluginstore.SchemaName(name)
	require.NoError(t, err)

	db, err := sql.Open("pgx", cfg.DSN())
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck // 後片付け
	_, _ = db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`)
}

// **プラグインが宣言 → migration → 読み書き までを通しで動かす。** 部品ごとの
// テストは揃っているが、配線 (dbBackedStorage が本物の DSN で開く経路) は
// ここでしか通らない。
func TestPluginStorage_EndToEnd(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const name = "storee2e"
	cfg := testDBConfig(t)
	dropPluginSchema(t, cfg, name)
	t.Cleanup(func() { dropPluginSchema(t, cfg, name) })

	e := echo.New()
	s := &Server{echo: e, config: cfg, role: config.RoleServer}
	api := e.Group("/api")

	def := plugin.Definition{
		Name: name, APIVersion: plugin.APIVersion,
		Routes: func(ctx plugin.Context, r plugin.Router) error {
			if err := ctx.Storage().Migrate(context.Background(), []plugin.Migration{
				{Version: 1, SQL: `CREATE TABLE visits (id serial PRIMARY KEY)`},
			}); err != nil {
				return err
			}
			r.POST("/visit", func(req plugin.Request) (any, error) {
				db := ctx.Storage().DB()
				if _, err := db.ExecContext(req.Context(), `INSERT INTO visits DEFAULT VALUES`); err != nil {
					return nil, err
				}
				var n int
				if err := db.QueryRowContext(req.Context(), `SELECT count(*) FROM visits`).Scan(&n); err != nil {
					return nil, err
				}
				return map[string]any{"visits": n}, nil
			})
			return nil
		},
	}

	require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, s.dbBackedStorage()))

	for i := 1; i <= 2; i++ {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/plugin/"+name+"/visit", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"visits":`+strconv.Itoa(i)+`}`, rec.Body.String())
	}

	// **本体のテーブルが見えないこと。** search_path が自分の schema に固定
	// されているので、修飾なしでは届かない。可視性判定は DB ではなくアプリ側に
	// あるため、ここが破れると非公開ノートが読めてしまう。
	store, err := pluginstore.Open(cfg.DSN(), name, 1)
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck // 後片付け

	var n int
	err = store.DB().QueryRow(`SELECT count(*) FROM "user"`).Scan(&n)
	require.Error(t, err, `修飾なしで本体のテーブルには届かない`)
	assert.Contains(t, err.Error(), "does not exist")
}

// 再起動しても migration が二重に流れないこと (起動のたびに呼ばれる)。
func TestPluginStorage_RestartIsSafe(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const name = "storerestart"
	cfg := testDBConfig(t)
	dropPluginSchema(t, cfg, name)
	t.Cleanup(func() { dropPluginSchema(t, cfg, name) })

	def := plugin.Definition{
		Name: name, APIVersion: plugin.APIVersion,
		Routes: func(ctx plugin.Context, _ plugin.Router) error {
			return ctx.Storage().Migrate(context.Background(), []plugin.Migration{
				{Version: 1, SQL: `CREATE TABLE t (id int)`},
			})
		},
	}

	for i := 0; i < 2; i++ {
		e := echo.New()
		s := &Server{echo: e, config: cfg, role: config.RoleServer}
		require.NoErrorf(t, s.setupPlugins(e.Group("/api"), []plugin.Definition{def}, s.dbBackedStorage()),
			"%d 回目の起動", i+1)
	}
}
