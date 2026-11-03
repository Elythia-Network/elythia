package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/db"
)

// newSingleConnDB opens db.New against the test DB with one connection, so
// every statement of the test runs in the same PostgreSQL session.
func newSingleConnDB(t *testing.T) *gorm.DB {
	t.Helper()
	skipIfNoTestDB(t)
	cfg := testCfgFromEnv(t)
	one := 1
	cfg.DB.MaxOpenConns = &one
	cfg.DB.MaxIdleConns = &one
	gdb, err := db.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close(gdb) })
	return gdb
}

// markerStatements returns the prepare_time of every statement prepared in
// this session whose text carries the marker.
//
// 照会の文自身に marker が続けて現れないよう、LIKE の型は連結で組む。引数を
// 持たない文は pgx が simple protocol で送るので、照会の文は準備されない。
func markerStatements(t *testing.T, gdb *gorm.DB) []time.Time {
	t.Helper()
	var times []time.Time
	require.NoError(t, gdb.Raw(`SELECT prepare_time FROM pg_prepared_statements WHERE statement LIKE '%' || 'stmt' || 'cache3964' || '%' ORDER BY prepare_time`).Scan(&times).Error)
	return times
}

// TestNew_TransactionsReusePreparedStatements pins that a statement run inside
// transactions is prepared once per connection and reused, instead of being
// prepared again for every transaction (#3964).
//
// GORM の PrepareStmt はトランザクションの中では文をそのトランザクションに
// 結び付け直すので、トランザクションごとに Parse し、終わりに閉じていた。
// pgx の文のキャッシュは接続ごとに持ち、トランザクションをまたいで使い回す。
func TestNew_TransactionsReusePreparedStatements(t *testing.T) {
	gdb := newSingleConnDB(t)
	run := func() {
		require.NoError(t, gdb.Transaction(func(tx *gorm.DB) error {
			var v int
			if err := tx.Raw(`SELECT ?::int + 1 AS stmtcache3964`, 1).Scan(&v).Error; err != nil {
				return err
			}
			assert.Equal(t, 2, v)
			return nil
		}))
	}

	run()
	first := markerStatements(t, gdb)
	require.Len(t, first, 1, "the statement must stay prepared after the transaction ends")

	run()
	run()
	again := markerStatements(t, gdb)
	require.Len(t, again, 1)
	assert.Equal(t, first[0], again[0], "later transactions must reuse the statement, not prepare it again")
}

// TestNew_RecoversWhenCachedResultTypeChanges pins how a cached statement
// behaves when a migration changes the shape of what it returns: PostgreSQL
// rejects the cached plan once (`cached plan must not change result type`),
// pgx drops the statement, and the next run prepares it again and succeeds.
//
// 稼働中のプロセスがある間に、別のプロセスが migration で列を足す (`SELECT *`
// の結果の型が変わる) ときの形。失敗は文と接続ごとに 1 回で、続かない。
func TestNew_RecoversWhenCachedResultTypeChanges(t *testing.T) {
	gdb := newSingleConnDB(t)
	// 一時表はこの session だけのもので、共有の schema を汚さない。
	require.NoError(t, gdb.Exec(`CREATE TEMP TABLE stmtcache3964_t (a int)`).Error)
	require.NoError(t, gdb.Exec(`INSERT INTO stmtcache3964_t VALUES (1)`).Error)

	selectAll := func() error {
		var rows []map[string]any
		return gdb.Raw(`SELECT * FROM stmtcache3964_t WHERE a = ?`, 1).Scan(&rows).Error
	}
	require.NoError(t, selectAll())
	require.NoError(t, selectAll())

	require.NoError(t, gdb.Exec(`ALTER TABLE stmtcache3964_t ADD COLUMN b int`).Error)

	if err := selectAll(); err != nil {
		assert.ErrorContains(t, err, "cached plan must not change result type")
	}
	require.NoError(t, selectAll(), "the statement must be prepared again after the cached plan was rejected")
}
