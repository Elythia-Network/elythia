package dbhealth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"

	"github.com/shiroha-a/mk/internal/testutil"
)

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skip("PostgreSQL unavailable:", err)
	}
	return db
}

// fixture: 主キー / UNIQUE / 普通の index を持ち、dead tuple を抱えたテーブル。
// autovacuum は止めておく (走ると dead tuple が消えて結果が揺れる)。
func setupFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, q := range []string{
		`DROP TABLE IF EXISTS dbh_bloated, dbh_small`,
		// DELETE は index を使わない条件にする (使うと idx_scan が進み、使われて
		// いない index の検査にならない)。
		`CREATE TABLE dbh_bloated (id int PRIMARY KEY, u int UNIQUE, w int) WITH (autovacuum_enabled = false)`,
		`CREATE INDEX dbh_bloated_w ON dbh_bloated (w)`,
		`INSERT INTO dbh_bloated SELECT g, g, g FROM generate_series(1, 20000) g`,
	} {
		require.NoError(t, db.Exec(q).Error, q)
	}
	// サイズは relpages (見積もり) から出すので、消す前に ANALYZE して埋める
	// (index を行より先に作っているので、CREATE INDEX では埋まらない)。**先に INSERT の
	// 件数が統計に反映されるのを待つ** — ANALYZE は行数を絶対値で上書きするので、
	// INSERT 側の未反映の加算が後から載ると数がずれる。
	require.Eventually(t, func() bool {
		var live int64
		db.Raw(`SELECT n_live_tup FROM pg_stat_user_tables WHERE schemaname = current_schema() AND relname = 'dbh_bloated'`).Scan(&live)
		return live == 20000
	}, 20*time.Second, 100*time.Millisecond)
	for _, q := range []string{
		`ANALYZE dbh_bloated`,
		`DELETE FROM dbh_bloated WHERE id % 4 <> 1`,
		`CREATE TABLE dbh_small (id int PRIMARY KEY) WITH (autovacuum_enabled = false)`,
		`INSERT INTO dbh_small SELECT g FROM generate_series(1, 100) g`,
		`DELETE FROM dbh_small WHERE id % 10 <> 1`,
	} {
		require.NoError(t, db.Exec(q).Error, q)
	}
	t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS dbh_bloated, dbh_small`) })
}

func tableOf(r Report, name string) *TableStat {
	for i := range r.Tables {
		if r.Tables[i].Table == name {
			return &r.Tables[i]
		}
	}
	return nil
}

func TestService_Report(t *testing.T) {
	db := openDB(t)
	setupFixture(t, db)

	// 統計は非同期に反映されるので、dead tuple が見えるまで待つ (キャッシュは
	// 毎回作り直して読み直させる)。
	var r Report
	require.Eventually(t, func() bool {
		var err error
		r, err = NewService(db, false).Report(context.Background())
		require.NoError(t, err)
		b := tableOf(r, "dbh_bloated")
		return b != nil && b.DeadRows == 15000
	}, 20*time.Second, 200*time.Millisecond)

	b := tableOf(r, "dbh_bloated")
	assert.EqualValues(t, 5000, b.LiveRows)
	assert.InDelta(t, 0.75, b.DeadRatio, 0.001)
	require.NotNil(t, b.SizeBytes)
	assert.Positive(t, *b.SizeBytes)
	// 一度も VACUUM / ANALYZE されていない (index も行を入れる前に作った)
	// テーブルはサイズ不明 (relpages が 0 のままで、0 B と出すと空に見える)。
	small := tableOf(r, "dbh_small")
	require.NotNil(t, small)
	assert.Nil(t, small.SizeBytes)
	assert.False(t, b.Vacuuming)
	assert.Nil(t, b.LastVacuum, "never vacuumed")
	assert.False(t, r.ReplicasConfigured)
	var av struct {
		Threshold    int64
		ScaleFactor  float64
		MaxThreshold *int64
	}
	require.NoError(t, db.Raw(`SELECT current_setting('autovacuum_vacuum_threshold')::bigint AS threshold,
		current_setting('autovacuum_vacuum_scale_factor')::float8 AS scale_factor,
		current_setting('autovacuum_vacuum_max_threshold', true)::bigint AS max_threshold`).Scan(&av).Error)
	assert.Equal(t, av.Threshold, r.AutovacuumThreshold)
	if av.MaxThreshold != nil {
		assert.Equal(t, *av.MaxThreshold, r.AutovacuumMaxThreshold)
	} else {
		assert.Zero(t, r.AutovacuumMaxThreshold)
	}
	assert.Equal(t, av.ScaleFactor, r.AutovacuumScaleFactor)
	assert.Positive(t, r.AutovacuumScaleFactor)
	// 一度もリセットしていないデータベースでは NULL (= initdb 以来)。値そのものは
	// 環境次第なので、pg_stat_database と一致することを見る。
	var want struct{ StatsReset *time.Time }
	require.NoError(t, db.Raw(`SELECT stats_reset FROM pg_stat_database WHERE datname = current_database()`).Scan(&want).Error)
	if want.StatsReset == nil {
		assert.Nil(t, r.StatsReset)
	} else {
		require.NotNil(t, r.StatsReset)
		assert.True(t, want.StatsReset.Equal(*r.StatsReset))
	}

	// 使われていない index: 主キー / UNIQUE は印を付けて出す (消せない)。
	byName := map[string]IndexStat{}
	for _, ix := range r.UnusedIndexes {
		byName[ix.Index] = ix
		assert.Zero(t, ix.Scans)
	}
	require.Contains(t, byName, "dbh_bloated_w")
	assert.False(t, byName["dbh_bloated_w"].Unique)
	require.Contains(t, byName, "dbh_bloated_pkey")
	assert.True(t, byName["dbh_bloated_pkey"].Primary)
	assert.True(t, byName["dbh_bloated_pkey"].Unique)
	require.Contains(t, byName, "dbh_bloated_u_key")
	assert.True(t, byName["dbh_bloated_u_key"].Unique)
	assert.False(t, byName["dbh_bloated_u_key"].Primary)
	assert.Equal(t, "dbh_bloated", byName["dbh_bloated_w"].Table)

	// 小さいテーブルは比率が高くても問題にしない。
	problems := r.Problems
	kinds := map[string][]string{}
	for _, p := range problems {
		kinds[p.Table] = append(kinds[p.Table], p.Kind)
	}
	assert.ElementsMatch(t, []string{"bloat", "vacuum"}, kinds["dbh_bloated"])
	assert.NotContains(t, kinds, "dbh_small")

	// VACUUM すれば「VACUUM の遅れ」は消える。**dead tuple が 0 になることは
	// 待たない** — CI は PostgreSQL を他パッケージと共有しており、そちらが古い
	// snapshot を持ったトランザクションを開いていると、VACUUM は行を回収できず
	// dead tuple が残る (#3095 の敵対的レビュー 3 周目で再現)。
	require.NoError(t, db.Exec(`VACUUM dbh_bloated`).Error)
	require.Eventually(t, func() bool {
		r, _ = NewService(db, false).Report(context.Background())
		b := tableOf(r, "dbh_bloated")
		return b != nil && b.LastVacuum != nil
	}, 20*time.Second, 200*time.Millisecond)
	for _, p := range r.Problems {
		if p.Table == "dbh_bloated" {
			assert.NotEqual(t, "vacuum", p.Kind, "a recent VACUUM clears the vacuum warning")
		}
	}

	// 使われた index は一覧から外れる。SET LOCAL はトランザクションの中でだけ
	// 効く (プールの別の接続に設定を残さない)。
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SET LOCAL enable_seqscan = off`).Error; err != nil {
			return err
		}
		var n int64
		return tx.Raw(`SELECT count(*) FROM dbh_bloated WHERE w = 10`).Scan(&n).Error
	}))
	require.Eventually(t, func() bool {
		r, _ = NewService(db, false).Report(context.Background())
		for _, ix := range r.UnusedIndexes {
			if ix.Index == "dbh_bloated_w" {
				return false
			}
		}
		return true
	}, 20*time.Second, 200*time.Millisecond)
}

// 他の schema の同名テーブルを拾わない (#2777)。
func TestService_StaysInItsSchema(t *testing.T) {
	db := openDB(t)
	setupFixture(t, db)
	other, err := testutil.OpenTestDBSchema("other")
	if err != nil {
		t.Skip(err)
	}
	require.NoError(t, other.Exec(`DROP TABLE IF EXISTS dbh_elsewhere`).Error)
	require.NoError(t, other.Exec(`CREATE TABLE dbh_elsewhere (id int PRIMARY KEY)`).Error)
	t.Cleanup(func() { other.Exec(`DROP TABLE IF EXISTS dbh_elsewhere`) })

	r, err := NewService(db, true).Report(context.Background())
	require.NoError(t, err)
	assert.True(t, r.ReplicasConfigured)
	assert.Nil(t, tableOf(r, "dbh_elsewhere"))
	for _, ix := range r.UnusedIndexes {
		assert.NotEqual(t, "dbh_elsewhere_pkey", ix.Index)
	}
}

// 読み直すのは cacheTTL に 1 回。
func TestService_Caches(t *testing.T) {
	db := openDB(t)
	now := time.Now()
	s := NewService(db, false)
	s.SetClockForTest(func() time.Time { return now })
	first, err := s.Report(context.Background())
	require.NoError(t, err)
	now = now.Add(cacheTTL - time.Second)
	again, err := s.Report(context.Background())
	require.NoError(t, err)
	assert.Equal(t, first.GeneratedAt, again.GeneratedAt, "served from the cache")
	now = now.Add(time.Second)
	fresh, err := s.Report(context.Background())
	require.NoError(t, err)
	assert.True(t, fresh.GeneratedAt.After(first.GeneratedAt))

	// 読めなければエラー (キャッシュを汚さない)。
	failing := NewService(db, false)
	failing.readFn = func(context.Context, time.Time) (Report, error) { return Report{}, assert.AnError }
	_, err = failing.Report(context.Background())
	assert.ErrorIs(t, err, assert.AnError)
	assert.Nil(t, failing.cached)
}

// 遅い読み取りの後ろに並んだ呼び出しも、自分の ctx が切れたら返る。読み取りは
// 1 本にまとめ、最初の呼び出し元が諦めても他の呼び出しは結果を受け取る。
func TestService_WaitsOnlyForTheCallersContext(t *testing.T) {
	s := NewService(nil, false)
	release := make(chan struct{})
	var reads int
	var mu sync.Mutex
	s.readFn = func(ctx context.Context, now time.Time) (Report, error) {
		mu.Lock()
		reads++
		mu.Unlock()
		<-release
		return Report{GeneratedAt: now}, nil
	}

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Report(context.Background())
		done <- err
	}()
	start := time.Now()
	_, err := s.Report(short)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second, "the caller's context is honoured")

	close(release)
	require.NoError(t, <-done, "the other caller still gets the result")
	mu.Lock()
	assert.Equal(t, 1, reads, "one reading is shared")
	mu.Unlock()
	require.NotNil(t, s.cached)
}

// レプリカを登録していても、統計はプライマリから読む。レプリカの schema には
// 無いテーブルが見えることで確かめる (素の SELECT はレプリカへ行く)。
func TestService_ReadsThePrimary(t *testing.T) {
	primarySQL := openDB(t)
	replica, err := testutil.OpenTestDBSchema("replica")
	if err != nil {
		t.Skip(err)
	}
	require.NoError(t, primarySQL.Exec(`DROP TABLE IF EXISTS dbh_primary_only`).Error)
	require.NoError(t, primarySQL.Exec(`CREATE TABLE dbh_primary_only (id int PRIMARY KEY)`).Error)
	t.Cleanup(func() { primarySQL.Exec(`DROP TABLE IF EXISTS dbh_primary_only`) })

	primaryConn, err := primarySQL.DB()
	require.NoError(t, err)
	replicaConn, err := replica.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: primaryConn}), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Use(dbresolver.Register(dbresolver.Config{
		Replicas: []gorm.Dialector{postgres.New(postgres.Config{Conn: replicaConn})},
	})))

	var plain int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM pg_stat_user_tables WHERE schemaname = current_schema() AND relname = 'dbh_primary_only'`).Scan(&plain).Error)
	require.Zero(t, plain, "a plain SELECT goes to the replica (the resolver is active)")

	r, err := NewService(db, true).Report(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, tableOf(r, "dbh_primary_only"), "the statistics come from the primary")
}

func TestProblems(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	old := now.Add(-VacuumStaleAfter)
	r := Report{Tables: []TableStat{
		{Table: "bloated_recent", DeadRows: MinDeadTuples, DeadRatio: BloatDeadRatio, LastVacuum: &recent},
		{Table: "stale", DeadRows: MinDeadTuples, DeadRatio: 0.01, LastVacuum: &old},
		{Table: "fine", DeadRows: MinDeadTuples, DeadRatio: BloatDeadRatio - 0.01, LastVacuum: &recent},
		{Table: "small", DeadRows: MinDeadTuples - 1, DeadRatio: 0.9},
		// autovacuum の起動条件 (50 + 20% = 200050) に届いていないので、VACUUM が
		// 古くても正常。比率も低い。
		{Table: "large_below_trigger", LiveRows: 1_000_000, DeadRows: 200_000, DeadRatio: 0.166, LastVacuum: &old},
		{Table: "large_over_trigger", LiveRows: 1_000_000, DeadRows: 200_051, DeadRatio: 0.166, LastVacuum: &old},
		// VACUUM の最中は判定しない。
		{Table: "vacuuming", DeadRows: MinDeadTuples, DeadRatio: 0.9, Vacuuming: true},
	}, AutovacuumThreshold: 50, AutovacuumScaleFactor: 0.2}
	got := map[string]string{}
	for _, p := range r.findProblems(now) {
		got[p.Table] += p.Kind
		assert.NotEmpty(t, p.Detail)
	}
	assert.Equal(t, map[string]string{"bloated_recent": "bloat", "stale": "vacuum", "large_over_trigger": "vacuum"}, got)
	assert.Contains(t, r.findProblems(now)[1].Detail, "7 日前")
	assert.NotNil(t, Report{}.findProblems(now), "empty, not null, in JSON")

	a, b := now, now.Add(time.Second)
	assert.Equal(t, &b, latest(&a, &b))
	assert.Equal(t, &b, latest(&b, &a))
	assert.Equal(t, &a, latest(&a, nil))
	assert.Equal(t, &a, latest(nil, &a))
	assert.Nil(t, latest(nil, nil))
}

// 統計をリセットした時刻を返す (Scans = 0 が「統計が新しいだけ」かを見分ける
// 材料)。**pg_stat_reset はデータベース全体の統計を消す** — `pg_stat_*` を読む
// テストはこのパッケージにしか無いことを確かめたうえで使う。
func TestService_ReportsStatsReset(t *testing.T) {
	db := openDB(t)
	if err := db.Exec(`SELECT pg_stat_reset()`).Error; err != nil {
		t.Skip("pg_stat_reset is not permitted:", err)
	}
	before := time.Now().Add(-time.Minute)
	r, err := NewService(db, false).Report(context.Background())
	require.NoError(t, err)
	require.NotNil(t, r.StatsReset)
	assert.True(t, r.StatsReset.After(before))
}

// テーブルに ACCESS EXCLUSIVE ロックがあっても (VACUUM FULL / REINDEX の最中)
// 読み取りが待たされない。サイズを pg_relation_size で出すと、対象のロックを
// 取りに行って止まる。
func TestService_DoesNotWaitForTableLocks(t *testing.T) {
	db := openDB(t)
	setupFixture(t, db)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { tx.Rollback() })
	require.NoError(t, tx.Exec(`LOCK TABLE dbh_bloated IN ACCESS EXCLUSIVE MODE`).Error)

	s := NewService(db, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.Report(ctx)
	require.NoError(t, err, "a table lock does not block the reading")
	assert.NotNil(t, tableOf(r, "dbh_bloated"))
}

// PG18 の autovacuum_vacuum_max_threshold: 起動条件はこれを超えない。
func TestProblems_MaxThreshold(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	old := now.Add(-VacuumStaleAfter)
	huge := TableStat{Table: "huge", LiveRows: 1_000_000_000, DeadRows: 150_000_000, DeadRatio: 0.13, LastVacuum: &old}
	r := Report{Tables: []TableStat{huge}, AutovacuumThreshold: 50, AutovacuumScaleFactor: 0.2}
	assert.Empty(t, r.findProblems(now), "below 50 + 20% without a cap")
	r.AutovacuumMaxThreshold = 100_000_000
	require.Len(t, r.findProblems(now), 1, "the cap lowers the trigger")
	assert.Equal(t, "vacuum", r.findProblems(now)[0].Kind)
	r.AutovacuumMaxThreshold = -1
	assert.Empty(t, r.findProblems(now), "-1 disables the cap")
}

// 読み取り中の panic はエラーとして返す (プロセスを落とさない)。
func TestService_RecoversFromPanic(t *testing.T) {
	s := NewService(nil, false)
	s.readFn = func(context.Context, time.Time) (Report, error) { panic("boom") }
	_, err := s.Report(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}
