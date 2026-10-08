package selfcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/core/dbhealth"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 未配線の依存は skip。サーバー未起動でも config 検査だけ回せるようにするため、
// nil を fail にはしない。
func TestLocalChecks_SkipWhenUnwired(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, StatusSkip, CheckDatabase(ctx, LocalDeps{}).Status)
	assert.Equal(t, StatusSkip, CheckRedis(ctx, LocalDeps{}).Status)
}

func TestCheckRedis_OK(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	assert.Equal(t, StatusOK, CheckRedis(context.Background(), LocalDeps{Redis: rdb}).Status)
}

func TestCheckRedis_Unreachable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })

	got := CheckRedis(context.Background(), LocalDeps{Redis: rdb})
	assert.Equal(t, StatusFail, got.Status)
	assert.NotEmpty(t, got.Hint)
}

func TestCheckDatabase_OK(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	got := CheckDatabase(context.Background(), LocalDeps{DB: db})
	require.Equal(t, StatusOK, got.Status, got.Detail)
	assert.Contains(t, got.Detail, "migration version")
}

// migration の適用漏れは「起動はするが一部機能だけ壊れる」形で出るので、
// 接続できたことをもって正常とみなさない。
func TestCheckDatabase_DetectsMissingMigrations(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	// 同梱本数を実際より多く見せかける = 適用漏れがある状態。
	got := CheckDatabase(context.Background(), LocalDeps{DB: db, MigrationCount: 1_000_000})
	assert.Equal(t, StatusFail, got.Status)
	assert.Contains(t, got.Hint, "migrate-up")
}

// fork の系列 (#3428) は、同梱している最大の version と `schema_migrations_local`
// を突き合わせる。同梱が無い (LocalMigrationLatest == 0) ときは表を見ない。
func TestCheckDatabase_LocalMigrations(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	ctx := context.Background()
	reset := func(t *testing.T, rows string) {
		t.Helper()
		require.NoError(t, db.Exec(`DROP TABLE IF EXISTS schema_migrations_local`).Error)
		if rows == "" {
			return
		}
		require.NoError(t, db.Exec(`CREATE TABLE schema_migrations_local (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`).Error)
		if rows != "none" {
			require.NoError(t, db.Exec(`INSERT INTO schema_migrations_local (version, dirty) VALUES `+rows).Error)
		}
	}
	t.Cleanup(func() { _ = db.Exec(`DROP TABLE IF EXISTS schema_migrations_local`).Error })

	t.Run("no local track does not read the table", func(t *testing.T) {
		reset(t, "")
		got := CheckDatabase(ctx, LocalDeps{DB: db})
		require.Equal(t, StatusOK, got.Status, got.Detail)
		assert.NotContains(t, got.Detail, "local")
	})
	t.Run("up to date", func(t *testing.T) {
		reset(t, "(900003, false)")
		got := CheckDatabase(ctx, LocalDeps{DB: db, LocalMigrationLatest: 900003})
		require.Equal(t, StatusOK, got.Status, got.Detail)
		assert.Contains(t, got.Detail, "local migration version 900003")
	})
	for _, tt := range []struct {
		name, rows string
		latest     int64
		detail     string
	}{
		{"table missing", "", 1, "schema_migrations_local を読めない"},
		{"dirty", "(2, true)", 2, "fork の migration が dirty (version 2)"},
		{"behind", "(1, false)", 2, "適用済み version 1 / 同梱の最新 2"},
		{"all rolled back", "none", 1, "適用済み version 0 / 同梱の最新 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reset(t, tt.rows)
			got := CheckDatabase(ctx, LocalDeps{DB: db, LocalMigrationLatest: tt.latest})
			assert.Equal(t, StatusFail, got.Status)
			assert.Contains(t, got.Detail, tt.detail)
			assert.NotEmpty(t, got.Hint)
		})
	}
}

// Run は最初の失敗で打ち切らない。打ち切ると運用者が直しては走らせ直すことに
// なる。
func TestRun_ContinuesAfterFailure(t *testing.T) {
	srv := httptest.NewServer(nil)
	srv.Close() // 到達不能にする

	report := Run(context.Background(), NewChecker(srv.URL), LocalDeps{})
	assert.False(t, report.OK)
	assert.Len(t, report.Results, 9, "全項目ぶんの結果が返る")
}

// warn だけなら OK は落とさない。「見ておくべき」と「壊れている」を区別する。
func TestReport_WarnDoesNotFailOverall(t *testing.T) {
	r := newReport([]Result{okResult("a", ""), warnResult("b", "", "hint")})
	assert.True(t, r.OK)

	r = newReport([]Result{okResult("a", ""), failResult("b", "", "hint")})
	assert.False(t, r.OK)
}

// meta.rootUserId が未設定なら fail を返す。
//
// **未設定だと `admin/accounts/create` の初回セットアップ判定が、ローカル利用者数の
// ガードだけに依存する状態になる。** 運用者がそれに気付ける手段が他に無い。
func TestCheckRootUser(t *testing.T) {
	t.Run("DB 未配線は skip", func(t *testing.T) {
		got := CheckRootUser(context.Background(), LocalDeps{})
		assert.Equal(t, StatusSkip, got.Status)
	})

	// **クエリ本体まで実 DB で踏む。** skip 枝しか通らないテストだと、
	// 壊れても誰も気付かない部分が未実行のまま残る (同パッケージの
	// `TestCheckDatabase_OK` は既に実 DB を使っている)。
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	// **meta 行が無いと全部 fail になってしまう。** migration は行を作らず、
	// 本番では `EnsureInitial` が起動時に作る。
	require.NoError(t, db.Exec(`INSERT INTO meta (id) VALUES ('x') ON CONFLICT DO NOTHING`).Error)

	t.Run("rootUserId 未設定なら fail", func(t *testing.T) {
		require.NoError(t, db.Exec(`UPDATE meta SET "rootUserId" = NULL`).Error)
		got := CheckRootUser(context.Background(), LocalDeps{DB: db})
		assert.Equal(t, StatusFail, got.Status)
		assert.Contains(t, got.Hint, "update-meta")
	})

	t.Run("空文字も fail", func(t *testing.T) {
		require.NoError(t, db.Exec(`UPDATE meta SET "rootUserId" = ''`).Error)
		assert.Equal(t, StatusFail, CheckRootUser(context.Background(), LocalDeps{DB: db}).Status)
	})

	t.Run("設定済みなら ok", func(t *testing.T) {
		require.NoError(t, db.Exec(`UPDATE meta SET "rootUserId" = 'someroot'`).Error)
		t.Cleanup(func() { db.Exec(`UPDATE meta SET "rootUserId" = NULL`) })
		got := CheckRootUser(context.Background(), LocalDeps{DB: db})
		assert.Equal(t, StatusOK, got.Status)
		assert.Contains(t, got.Detail, "rootUserId")
	})

	// **読めないときは fail。** 「未設定」と区別が付かないので、判定できない
	// ことを ok に倒さない。
	//
	// **DDL で再現しない (#3037 レビュー)。** `ALTER TABLE meta RENAME` は
	// このパッケージの schema を書き換えるので、テストが途中で死ぬと
	// `meta` の無い schema が残る。`ApplyMigrations` は台帳を見て作り直さない
	// ため、以後このパッケージは永久に落ちる (docs/testing.md「DB を使うテストの分離」/ #2756)。
	// 閉じた接続を渡せば同じ枝を踏めて、共有状態に触らない。
	t.Run("meta を読めないなら fail", func(t *testing.T) {
		closed := testutil.MustOpenTestDB()
		sqlDB, err := closed.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())

		got := CheckRootUser(context.Background(), LocalDeps{DB: closed})
		assert.Equal(t, StatusFail, got.Status)
	})
}

func TestCheckDatabaseHealth(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, StatusSkip, CheckDatabaseHealth(ctx, LocalDeps{}).Status)

	res := CheckDatabaseHealth(ctx, LocalDeps{DBHealth: func(context.Context) (dbhealth.Report, error) {
		return dbhealth.Report{}, errors.New("permission denied")
	}})
	assert.Equal(t, StatusWarn, res.Status, "unreadable statistics are a warning, not a failure")
	assert.Contains(t, res.Detail, "permission denied")
	assert.NotEmpty(t, res.Hint)

	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	healthy := dbhealth.Report{GeneratedAt: now, Tables: []dbhealth.TableStat{{Table: "note", DeadRows: 5, LastVacuum: &recent}}, Problems: []dbhealth.Problem{}}
	res = CheckDatabaseHealth(ctx, LocalDeps{DBHealth: func(context.Context) (dbhealth.Report, error) { return healthy, nil }})
	assert.Equal(t, StatusOK, res.Status)
	assert.Contains(t, res.Detail, "1 テーブル")

	// 問題は warn (fail ではない)。名前を出すのは 5 件までで、残りは件数。
	var problems []dbhealth.Problem
	for i := 0; i < 7; i++ {
		problems = append(problems, dbhealth.Problem{Table: fmt.Sprintf("t%d", i), Kind: "bloat", Detail: fmt.Sprintf("t%d: dead", i)})
	}
	bloated := dbhealth.Report{GeneratedAt: now, Problems: problems}
	res = CheckDatabaseHealth(ctx, LocalDeps{DBHealth: func(context.Context) (dbhealth.Report, error) { return bloated, nil }})
	assert.Equal(t, StatusWarn, res.Status)
	assert.Contains(t, res.Detail, "t0")
	assert.Contains(t, res.Detail, "t4")
	assert.NotContains(t, res.Detail, "t5")
	assert.Contains(t, res.Detail, "ほか 2 件")
	assert.Contains(t, res.Hint, "VACUUM")

	// Run にも入っている。
	rep := Run(ctx, NewChecker("https://example.invalid"), LocalDeps{DBHealth: func(context.Context) (dbhealth.Report, error) { return bloated, nil }})
	found := false
	for _, r := range rep.Results {
		if r.Name == "database-health" {
			found = true
			assert.Equal(t, StatusWarn, r.Status)
		}
	}
	assert.True(t, found)
}

// 統計の読み取りが待たされても、この検査だけで全体の期限を食い潰さない。
func TestCheckDatabaseHealth_HasItsOwnTimeout(t *testing.T) {
	old := healthTimeout
	healthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { healthTimeout = old })
	start := time.Now()
	res := CheckDatabaseHealth(context.Background(), LocalDeps{DBHealth: func(ctx context.Context) (dbhealth.Report, error) {
		<-ctx.Done()
		return dbhealth.Report{}, ctx.Err()
	}})
	assert.Equal(t, StatusWarn, res.Status)
	assert.Less(t, time.Since(start), time.Second)
	assert.Contains(t, res.Detail, "以内に終わらない", "a timeout is not reported as a permission problem")
}
