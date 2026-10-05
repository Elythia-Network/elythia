package repository

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// noTablesDB は**テーブルが 1 つも無い兄弟 schema**につないだ接続。
//
// SQL が壊れたときに err が返ることを試すための足場。以前は
// `db.Begin()` + `SET LOCAL search_path TO pg_temp` で作っていたが、
// **照会を `withStatementTimeout` で包んだ時点でその形は成立しなくなった** —
// 既にトランザクションの `*gorm.DB` に対して `Begin()` は
// `ErrInvalidTransaction` を返すので、**SQL に一度も到達しないまま err になり、
// テストは緑のまま別物を検査していた** (敵対的レビュー 2 周目で実測)。
//
// search_path は DSN の runtime parameter なので、この接続はプールのどの接続でも
// 空の schema を見る。migration を流さないのでテーブルは存在しない。
var (
	noTablesOnce sync.Once
	noTablesDB   *gorm.DB
	noTablesErr  error
)

func emptySchemaDB(t *testing.T) *gorm.DB {
	t.Helper()
	noTablesOnce.Do(func() {
		noTablesDB, noTablesErr = testutil.OpenTestDBSchema("notables")
	})
	require.NoError(t, noTablesErr)
	require.NotNil(t, noTablesDB)
	return noTablesDB
}

// **この repository はトランザクション配下の `*gorm.DB` を受け取れない。**
// 照会ごとに自分でトランザクションを開いて `SET LOCAL statement_timeout` を
// 掛けるため。router は `s.db` を渡すので本番では起きないが、契約として固定する
// (黙って上限が外れるより、全照会が 500 で落ちるほうが気付ける)。
func TestLookupRepositories_RejectTransactionScopedDB(t *testing.T) {
	db := ipSearchTestDB(t)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()

	_, err := NewUserIPSearchRepository(tx).HasAnyHistory()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "begin",
		"トランザクション配下で構築したら、上限を掛けられないことが分かる形で落ちること")

	_, err = NewIPLookupLogRepository(tx).List(10, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "begin")
}

// lockTable takes an ACCESS EXCLUSIVE lock on one table until the test ends.
//
// **待たせる側を作るのが要点。** `statement_timeout` は**ロック待ちにも効く**ので、
// ロックを取った状態で照会を呼べば「上限が掛かっているか」を**分布や実行速度に
// 依らず決定的に**試せる。掛かっていなければロックが外れるまで永久に待つ。
//
// 専用の接続を 1 本取るのが必須 — プール越しに `BEGIN` を投げると、ロックを取った
// 接続と `ROLLBACK` を投げる接続が別になりうる。`search_path` は DSN の runtime
// parameter なので、プールから取る接続はどれも同じ schema を見る。
func lockTable(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	conn, err := sqlDB.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	})
	_, err = conn.ExecContext(context.Background(), "BEGIN")
	require.NoError(t, err)
	// **ロック待ちで固まらないよう、ロックを取る側にも上限を置く。** 取れないまま
	// 待つと、この後の照会は「ロックされていない」状態を測ってしまい空振りする。
	_, err = conn.ExecContext(context.Background(), "SET LOCAL lock_timeout TO 5000")
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `LOCK TABLE "`+table+`" IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err, "ロックが取れていないと、この後の測定が空振りする")
}

// mustTimeOut runs fn and requires it to fail with a statement timeout quickly.
//
// **待ち続ける形で落とさない。** 上限が掛かっていないと fn はロックが外れるまで
// 戻らないので、そのまま待つと `go test` の 10 分で落ちて診断が「timeout」になる。
// 別の goroutine で走らせて、待たされた事実そのものを失敗として報告する。
func mustTimeOut(t *testing.T, name string, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		require.Error(t, err, "%s: ロック中なのに成功した (測定が空振りしている)", name)
		assert.True(t, strings.Contains(err.Error(), "57014"),
			"%s: 取り消し以外の理由で失敗している: %v", name, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: statement_timeout を通っていない (ロック待ちで戻らない)", name)
	}
}

// **モデレーションの照会は 1 本残らず DB 側の上限を通る** (#3106)。
//
// **ラッパーを直接呼ぶテストだけでは足りない。** 実際にこの形の変異
// (`ListAccountsByIP` を `r.db.Raw(...)` に戻す) が、ラッパー自身のテストが
// 3 本ある状態で**緑のまま通った** (敵対的レビュー 1 周目で実測)。
// 見るべきは「ラッパーが上限を掛けるか」ではなく「**照会がラッパーを通るか**」。
func TestModerationLookups_AreBoundedByTimeout(t *testing.T) {
	db := ipSearchTestDB(t)
	lockTable(t, db, "user_ip")

	repo := NewUserIPSearchRepository(db).(*userIPSearchRepository)
	repo.lookupTimeout = 300 * time.Millisecond

	since := time.Now().Add(-24 * time.Hour)
	mustTimeOut(t, "ListAccountsByIP", func() error {
		_, err := repo.ListAccountsByIP("203.0.113.1", since, 10, 0)
		return err
	})
	mustTimeOut(t, "HasAnyHistory", func() error {
		_, err := repo.HasAnyHistory()
		return err
	})
	mustTimeOut(t, "ListIPsByUser", func() error {
		_, err := repo.ListIPsByUser("u_1", since, 10)
		return err
	})
	mustTimeOut(t, "ListSharedIPAccounts", func() error {
		_, err := repo.ListSharedIPAccounts([]string{"203.0.113.1"}, since, 10)
		return err
	})
}

// 監査の一覧も同じ扱い (#3106)。**ここだけ無制限だと doc の「負荷の上限」が
// endpoint ごとに嘘になる。**
func TestIPLookupLogList_IsBoundedByTimeout(t *testing.T) {
	db := ipSearchTestDB(t)
	lockTable(t, db, "ip_lookup_log")

	repo := NewIPLookupLogRepository(db).(*ipLookupLogRepository)
	repo.lookupTimeout = 300 * time.Millisecond

	mustTimeOut(t, "List", func() error {
		_, err := repo.List(10, 0)
		return err
	})
}
