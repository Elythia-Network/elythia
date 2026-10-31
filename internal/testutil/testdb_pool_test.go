package testutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// テストの DB の pool は、接続の数に上限を持ち、使っていない接続を短い時間で閉じる
// (#3369)。閉じない pool がテストごとに idle の接続を持ち続けると、make test の
// 同時接続が max_connections に届く。
func TestOpenTestDB_PoolClosesIdleConnections(t *testing.T) {
	db, err := OpenTestDB()
	if err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	assert.Equal(t, testDBMaxOpenConns, sqlDB.Stats().MaxOpenConnections)
	require.NoError(t, db.Exec("SELECT 1").Error)
	require.Positive(t, sqlDB.Stats().Idle, "the connection was not kept idle after the query")
	assert.Eventually(t, func() bool {
		st := sqlDB.Stats()
		return st.OpenConnections == 0 && st.MaxIdleTimeClosed > 0
	}, testDBConnMaxIdleTime+5*time.Second, 100*time.Millisecond, "the idle connection was not closed")
}
