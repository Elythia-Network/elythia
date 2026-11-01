package pgstat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/testutil"
)

// testDB is the shared test PostgreSQL, which does not preload
// pg_stat_statements.
var testDB = testutil.MustOpenTestDB()

// preloadedServer starts a PostgreSQL that preloads pg_stat_statements and
// returns a superuser connection and the connection string format for other
// users.
//
// 共有のテストの DB は shared_preload_libraries を持たず、再起動もできないので、
// この試験のためだけに起動の引数で読み込ませたサーバーを立てる。
func preloadedServer(t *testing.T) (*gorm.DB, func(user, password string) string) {
	t.Helper()
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("elythia"),
		tcpostgres.WithUsername("admin"),
		tcpostgres.WithPassword("admin"),
		testcontainers.WithCmdArgs("-c", "shared_preload_libraries=pg_stat_statements"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start a PostgreSQL that preloads pg_stat_statements")
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	dsn := func(user, password string) string {
		return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=elythia sslmode=disable", host, port.Port(), user, password)
	}
	return open(t, dsn("admin", "admin")), dsn
}

func open(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestEnsure_PreloadedServer(t *testing.T) {
	admin, dsn := preloadedServer(t)
	ctx := context.Background()

	// superuser でも pg_read_all_settings の一員でもない利用者 (DB の持ち主) でも、
	// 読み込まれていることは分かる。作れないので、起動を止めないよう ErrCreate で
	// 返し、何も作らない。
	require.NoError(t, admin.Exec(`CREATE ROLE app LOGIN PASSWORD 'app'`).Error)
	require.NoError(t, admin.Exec(`ALTER DATABASE elythia OWNER TO app`).Error)
	app := open(t, dsn("app", "app"))
	st, err := Inspect(ctx, app)
	require.NoError(t, err)
	assert.Equal(t, State{Preloaded: true, Installed: false}, st)
	require.Error(t, app.Raw("SHOW shared_preload_libraries").Scan(new(string)).Error,
		"the user cannot read shared_preload_libraries itself (the test's premise)")
	outcome, err := Ensure(ctx, app)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCreate), "%v", err)
	assert.Empty(t, outcome)
	st, err = Inspect(ctx, admin)
	require.NoError(t, err)
	assert.Equal(t, State{Preloaded: true, Installed: false}, st)

	// superuser なら作る。search_path の先頭が別の schema でも、作る先は public
	// (verify は public の拡張だけを先に作る)。
	require.NoError(t, admin.Exec(`CREATE SCHEMA elsewhere`).Error)
	elsewhere := open(t, dsn("admin", "admin")+" search_path=elsewhere,public")
	var first string
	require.NoError(t, elsewhere.Raw(`SELECT current_schema()`).Scan(&first).Error)
	require.Equal(t, "elsewhere", first, "the test's premise")
	outcome, err = Ensure(ctx, elsewhere)
	require.NoError(t, err)
	assert.Equal(t, OutcomeCreated, outcome)
	st, err = Inspect(ctx, admin)
	require.NoError(t, err)
	assert.Equal(t, State{Preloaded: true, Installed: true}, st)
	var schema string
	require.NoError(t, admin.Raw(`SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = ?`, Extension).Scan(&schema).Error)
	assert.Equal(t, "public", schema)
	// 作った view が読める (読み込まれているので)。
	var n int64
	require.NoError(t, admin.Raw(`SELECT count(*) FROM pg_stat_statements`).Scan(&n).Error)

	// 2 回目は何もしない。superuser でない利用者でも失敗しない。
	outcome, err = Ensure(ctx, app)
	require.NoError(t, err)
	assert.Equal(t, OutcomeInstalled, outcome)
}

// Processes started at the same time (web and queue nodes) race to create the
// extension. The losers see it created and do not warn.
func TestEnsure_ConcurrentCreate(t *testing.T) {
	admin, dsn := preloadedServer(t)
	const n = 8
	conns := make([]*gorm.DB, n)
	for i := range conns {
		conns[i] = open(t, dsn("admin", "admin"))
	}
	require.NoError(t, admin.Exec("SELECT 1").Error)
	var wg sync.WaitGroup
	outcomes := make([]Outcome, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outcomes[i], errs[i] = Ensure(context.Background(), conns[i])
		}()
	}
	close(start)
	wg.Wait()
	created := 0
	for i := range conns {
		require.NoError(t, errs[i])
		if outcomes[i] == OutcomeCreated {
			created++
		} else {
			assert.Equal(t, OutcomeInstalled, outcomes[i])
		}
	}
	assert.Equal(t, 1, created)
}

// Another process may finish creating the extension after Ensure read that it
// was missing and before Ensure creates it (#3933). Ensure reports that it
// found the extension, not that it created it.
//
// TestEnsure_ConcurrentCreate がこの順になるのは負荷の高いときだけなので、
// ここでは確かめた直後に別の接続で作り終えて、順を決めて起こす。
func TestEnsure_CreatedByAnotherAfterInspect(t *testing.T) {
	admin, dsn := preloadedServer(t)
	other := open(t, dsn("admin", "admin"))
	outcome, err := ensure(context.Background(), admin, func() {
		require.NoError(t, other.Exec(CreateSQL).Error)
	})
	require.NoError(t, err)
	assert.Equal(t, OutcomeInstalled, outcome)
}

// A server that does not preload the library is left alone.
func TestEnsure_NotPreloaded(t *testing.T) {
	db := testDB
	ctx := context.Background()
	st, err := Inspect(ctx, db)
	require.NoError(t, err)
	require.False(t, st.Preloaded, "the shared test PostgreSQL must not preload pg_stat_statements (the test's premise)")
	require.False(t, st.Installed, "the shared test database must not have the extension (the test's premise)")
	outcome, err := Ensure(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, OutcomeNotPreloaded, outcome)
	after, err := Inspect(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, st.Installed, after.Installed, "nothing is created")
}

func TestInspect_Error(t *testing.T) {
	db := testDB
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Inspect(ctx, db)
	assert.Error(t, err)
	_, err = Ensure(ctx, db)
	assert.Error(t, err)
	assert.False(t, errors.Is(err, ErrCreate))
}
