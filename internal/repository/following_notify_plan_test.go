package repository

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// sqlRecorder is a GORM logger that records the last statement with its bind
// placeholders intact ($1, $2, ...), before GORM interpolates the values.
type sqlRecorder struct {
	logger.Interface
	mu   sync.Mutex
	sql  string
	vars []interface{}
}

// ParamsFilter is called by GORM with the un-interpolated SQL and its vars.
func (r *sqlRecorder) ParamsFilter(_ context.Context, sql string, params ...interface{}) (string, []interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sql, r.vars = sql, params
	return sql, params
}

// Trace calls fc because GORM invokes ParamsFilter only from inside it.
func (r *sqlRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	fc()
}

// TestListFollowersToNotify_GenericPlanUsesPartialIndex pins the literal
// 'normal' in ListFollowersToNotify (#3590).
//
// 本番は PrepareStmt: true なので、同じ文が繰り返されると generic plan に切り替わる。
// notify を bind 変数にすると、generic plan では部分 index (000131,
// WHERE "notify" = 'normal') の述語を満たすと判定できず、使われない。GORM が実際に
// 出す SQL を PREPARE し、generic plan を強制して確かめる。
func TestListFollowersToNotify_GenericPlanUsesPartialIndex(t *testing.T) {
	rec := &sqlRecorder{Interface: logger.Discard}
	repo := NewFollowingRepository(testDB.Session(&gorm.Session{Logger: rec}))
	_, err := repo.ListFollowersToNotify("u_gp_none")
	require.NoError(t, err)

	rec.mu.Lock()
	sql, vars := rec.sql, rec.vars
	rec.mu.Unlock()
	require.Contains(t, sql, `FROM "following"`, "ListFollowersToNotify の SQL を拾えていない")
	require.NotEmpty(t, vars)

	placeholders := make([]string, len(vars))
	for i := range vars {
		placeholders[i] = "?"
	}
	var lines []string
	// PREPARE は transaction の外 (セッション) に残るので、接続を 1 本に固定して、
	// transaction を閉じた後に同じ接続で DEALLOCATE する。transaction の中で
	// DEALLOCATE すると、途中の文が失敗したときに abort 済みの transaction では
	// 効かず、プールに戻った接続に gp_notify が残って -count>1 の次の PREPARE が
	// already exists で落ちる。
	require.NoError(t, testDB.Connection(func(conn *gorm.DB) error {
		defer conn.Exec(`DEALLOCATE gp_notify`) // PREPARE 前に落ちたときのエラーは無視する
		tx := conn.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		// 表が小さいと seq scan が選ばれ、index の選び方を見られない。
		for _, s := range []string{
			`SET LOCAL plan_cache_mode = force_generic_plan`,
			`SET LOCAL enable_seqscan = off`,
			`PREPARE gp_notify AS ` + sql,
		} {
			require.NoError(t, tx.Exec(s).Error, s)
		}
		return tx.Raw(`EXPLAIN EXECUTE gp_notify(`+strings.Join(placeholders, ", ")+`)`, vars...).Scan(&lines).Error
	}))
	plan := strings.Join(lines, "\n")
	assert.Contains(t, plan, "IDX_following_followeeId_notify_normal",
		"generic plan で部分 index が使われない (notify を bind 変数に戻していないか):\n%s", plan)
}
