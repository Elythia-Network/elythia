// Package dbhealth reads PostgreSQL's own statistics to show the database's
// health (#3095): indexes that are never used, dead tuples and when tables
// were last vacuumed / analyzed.
//
// **判断材料を出すだけで、何も消さない。** 「使われていないインデックス」は
// 統計が新しいだけのこともあり、UNIQUE / 主キーの制約を支えるものは消すと
// 壊れる。消すかどうかは人が決める。
package dbhealth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

// Thresholds for the warnings. 小さいテーブルは比率が振れやすく、実害も無いので
// 件数の下限を置く。
const (
	// BloatDeadRatio is the dead tuple ratio above which a table is reported as bloated.
	BloatDeadRatio = 0.2
	// MinDeadTuples is the dead tuple count below which a table is never reported.
	MinDeadTuples = 10_000
	// VacuumStaleAfter is how long without any vacuum a table with dead tuples is reported.
	VacuumStaleAfter = 7 * 24 * time.Hour
)

// cacheTTL bounds how often the catalogs are read. pg_class の join は大規模だと
// 重いので、画面を開き直すたびに読み直させない。
const cacheTTL = time.Minute

// readTimeout bounds one reading. 読み取りは呼び出し元の ctx から切り離して
// 行う (下の Report) ので、その上限をここで持つ。
const readTimeout = 30 * time.Second

// IndexStat is one index.
type IndexStat struct {
	Table string `json:"table"`
	Index string `json:"index"`
	// Scans は統計を最後にリセットして (Report.StatsReset) からの index scan の回数。
	Scans int64 `json:"scans"`
	// SizeBytes は作成時か最後の VACUUM / ANALYZE 時点の見積もり (pg_class.relpages)。
	// 行を入れる前に作った index は、測り直されるまで小さく出る。
	SizeBytes int64 `json:"sizeBytes"`
	// Unique は UNIQUE / 主キー。使われていなくても制約を支えているので消せない。
	Unique  bool `json:"unique"`
	Primary bool `json:"primary"`
}

// TableStat is one table.
type TableStat struct {
	Table     string  `json:"table"`
	LiveRows  int64   `json:"liveRows"`
	DeadRows  int64   `json:"deadRows"`
	DeadRatio float64 `json:"deadRatio"`
	// SizeBytes はテーブル + TOAST + インデックスの見積もり (pg_class.relpages)。
	// 本体とインデックスは最後の VACUUM / ANALYZE / CREATE INDEX 時点、TOAST は
	// 最後の VACUUM 時点の値。**一度も VACUUM / ANALYZE されていないテーブルと、
	// TRUNCATE の後まだ測っていないテーブルは nil** (reltuples が -1。relpages が
	// 0 のままなので、0 B と出すと空に見える)。
	SizeBytes *int64 `json:"sizeBytes"`
	// LastVacuum / LastAnalyze は手動と自動の新しい方。一度も無ければ nil。
	LastVacuum  *time.Time `json:"lastVacuum"`
	LastAnalyze *time.Time `json:"lastAnalyze"`
	// ModifiedSinceAnalyze は最後の ANALYZE からの変更行数。
	ModifiedSinceAnalyze int64 `json:"modifiedSinceAnalyze"`
	// Vacuuming は VACUUM の最中 (pg_stat_progress_vacuum に載っている)。**DB の
	// ロールが superuser か pg_read_all_stats を持つときだけ分かる** — それ以外には
	// 他のロール (autovacuum を含む) の VACUUM の relid が NULL で見えるので、
	// false になり、VACUUM 中も警告が出る (危険側には倒れない)。
	Vacuuming bool `json:"vacuuming"`
}

// Report is one reading of the statistics.
type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// StatsReset は pg_stat_reset などで統計を最後にリセットした記録。無ければ nil。
	// **統計の起点としては保証されない** — 異常終了からの復旧やフェイルオーバーでも
	// 統計は消えるが、この時刻は進まない (#3095 の敵対的レビューで実測)。
	// Scans = 0 を「使われていない」と読む前に確かめる材料の 1 つでしかない。
	StatsReset *time.Time `json:"statsReset"`
	// ReplicasConfigured は読み取りレプリカが有効か。統計はプライマリから読むので、
	// レプリカでの index の利用は Scans に入らない。
	ReplicasConfigured bool `json:"replicasConfigured"`
	// AutovacuumThreshold / AutovacuumScaleFactor はサーバーの autovacuum の起動条件
	// (dead tuple が threshold + scale_factor * 行数 を超えると走る)。テーブルごとの
	// 設定 (reloptions) は見ない。
	AutovacuumThreshold   int64   `json:"autovacuumThreshold"`
	AutovacuumScaleFactor float64 `json:"autovacuumScaleFactor"`
	// AutovacuumMaxThreshold は起動条件の上限 (PG18 の autovacuum_vacuum_max_threshold)。
	// 無い版や無効 (-1) なら 0 以下。
	AutovacuumMaxThreshold int64       `json:"autovacuumMaxThreshold"`
	UnusedIndexes          []IndexStat `json:"unusedIndexes"`
	Tables                 []TableStat `json:"tables"`
	// Problems は肥大・VACUUM の遅れの判定結果 (GeneratedAt 時点)。**判定は
	// ここだけで行う** — 画面と self-check がそれぞれ閾値を持つと食い違う。
	Problems []Problem `json:"problems"`
}

// Problem is a finding worth a warning.
type Problem struct {
	Table  string `json:"table"`
	Kind   string `json:"kind"` // "bloat" / "vacuum"
	Detail string `json:"detail"`
}

// findProblems returns the tables that look bloated or unvacuumed at now.
func (r Report) findProblems(now time.Time) []Problem {
	out := []Problem{}
	for _, t := range r.Tables {
		// VACUUM の最中は dead tuple が片付く途中なので判定しない (比率も経過も
		// 一時的なもの)。
		if t.DeadRows < MinDeadTuples || t.Vacuuming {
			continue
		}
		if t.DeadRatio >= BloatDeadRatio {
			out = append(out, Problem{Table: t.Table, Kind: "bloat",
				Detail: fmt.Sprintf("%s: dead tuple %d 行 (%.0f%%)", t.Table, t.DeadRows, t.DeadRatio*100)})
		}
		// **autovacuum の起動条件を超えているのに走っていない**ときだけにする。
		// 大きいテーブルは起動条件そのものが大きく (既定は 50 + 20%)、条件に届く
		// までは VACUUM されないのが正常なので、経過日数だけで見ると警告し続ける。
		trigger := float64(r.AutovacuumThreshold) + r.AutovacuumScaleFactor*float64(t.LiveRows)
		// PG18 からは起動条件に上限がある (autovacuum_vacuum_max_threshold。-1 で無効)。
		if r.AutovacuumMaxThreshold > 0 && trigger > float64(r.AutovacuumMaxThreshold) {
			trigger = float64(r.AutovacuumMaxThreshold)
		}
		if float64(t.DeadRows) > trigger && (t.LastVacuum == nil || now.Sub(*t.LastVacuum) >= VacuumStaleAfter) {
			since := "一度も無い"
			if t.LastVacuum != nil {
				since = fmt.Sprintf("%d 日前", int(now.Sub(*t.LastVacuum).Hours()/24))
			}
			out = append(out, Problem{Table: t.Table, Kind: "vacuum",
				Detail: fmt.Sprintf("%s: 最後の VACUUM が%s (dead tuple %d 行)", t.Table, since, t.DeadRows)})
		}
	}
	return out
}

// Service reads the statistics with a short cache.
type Service struct {
	db       *gorm.DB
	replicas bool
	clock    func() time.Time

	mu     sync.Mutex
	cached *Report
	group  singleflight.Group
	// readFn は統計の読み取り (テストで遅い読み取りを差し込むため)。
	readFn func(ctx context.Context, now time.Time) (Report, error)
}

// NewService constructs a Service. replicas は dbReplications が有効で
// dbSlaves があるとき true。
func NewService(db *gorm.DB, replicas bool) *Service {
	s := &Service{db: db, replicas: replicas, clock: time.Now}
	s.readFn = s.read
	return s
}

// SetClockForTest overrides the time source.
func (s *Service) SetClockForTest(fn func() time.Time) { s.clock = fn }

// Report returns the statistics, read at most once per cacheTTL.
//
// **読み取りは 1 本にまとめ、待つのは呼び出し元の ctx の間だけにする。** lock を
// 持ったまま読むと、遅い読み取りの後ろに並んだ呼び出しは自分の ctx が切れても
// 待たされ続ける (画面・self-check・doctor が連鎖して止まる。#3095 の敵対的
// レビューで実測)。読み取り自体は呼び出し元の ctx から切り離す — 最初の呼び出し元が
// 諦めると、相乗りしている他の呼び出しまで巻き込んで失敗するため。
func (s *Service) Report(ctx context.Context) (Report, error) {
	s.mu.Lock()
	cached := s.cached
	s.mu.Unlock()
	if cached != nil && s.clock().Sub(cached.GeneratedAt) < cacheTTL {
		return *cached, nil
	}
	ch := s.group.DoChan("report", func() (res any, err error) {
		// **panic をここで拾う。** DoChan は関数内の panic を別の goroutine で
		// 投げ直すので、echo の Recover では拾えずプロセスごと落ちる。
		defer func() {
			if p := recover(); p != nil {
				res, err = nil, fmt.Errorf("dbhealth: read panicked: %v", p)
			}
		}()
		rctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		r, err := s.readFn(rctx, s.clock())
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.cached = &r
		s.mu.Unlock()
		return r, nil
	})
	select {
	case <-ctx.Done():
		return Report{}, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return Report{}, res.Err
		}
		return res.Val.(Report), nil
	}
}

func (s *Service) read(ctx context.Context, now time.Time) (Report, error) {
	// **プライマリから読む。** レプリカが有効だと dbresolver が SELECT を
	// レプリカへ振るが、VACUUM が走るのはプライマリで、レプリカの
	// n_dead_tup / last_*vacuum は意味を持たない。
	db := s.db.WithContext(ctx).Clauses(dbresolver.Write)
	r := Report{GeneratedAt: now, ReplicasConfigured: s.replicas, UnusedIndexes: []IndexStat{}, Tables: []TableStat{}}

	var reset struct{ StatsReset *time.Time }
	if err := db.Raw(`SELECT stats_reset FROM pg_stat_database WHERE datname = current_database()`).
		Scan(&reset).Error; err != nil {
		return Report{}, fmt.Errorf("dbhealth: stats_reset: %w", err)
	}
	r.StatsReset = reset.StatsReset

	var av struct {
		Threshold    int64
		ScaleFactor  float64
		MaxThreshold *int64
	}
	// max_threshold は PG18 から。第 2 引数の true で、無い版では NULL になる。
	if err := db.Raw(`SELECT current_setting('autovacuum_vacuum_threshold')::bigint AS threshold,
		current_setting('autovacuum_vacuum_scale_factor')::float8 AS scale_factor,
		current_setting('autovacuum_vacuum_max_threshold', true)::bigint AS max_threshold`).Scan(&av).Error; err != nil {
		return Report{}, fmt.Errorf("dbhealth: autovacuum settings: %w", err)
	}
	r.AutovacuumThreshold, r.AutovacuumScaleFactor = av.Threshold, av.ScaleFactor
	if av.MaxThreshold != nil {
		r.AutovacuumMaxThreshold = *av.MaxThreshold
	}

	// **自分の schema に絞る** (#2777)。システムカタログは search_path に従わず
	// 全 schema 分を返す。本番は public、テストはパッケージ専用の schema になる。
	// プラグイン専用の schema (pluginstore) は対象外。
	//
	// **サイズは pg_class.relpages から出す。** pg_relation_size /
	// pg_total_relation_size は対象に AccessShareLock を取りに行くので、VACUUM FULL や
	// REINDEX の最中 (= 肥大を直している時間帯) に読み取りごと待たされる。relpages は
	// 最後の VACUUM / ANALYZE 時点の見積もりだが、ロックを取らない。
	if err := db.Raw(`
		SELECT s.relname AS "table", s.indexrelname AS "index", s.idx_scan AS scans,
		       ic.relpages::bigint * current_setting('block_size')::bigint AS size_bytes,
		       i.indisunique AS "unique", i.indisprimary AS "primary"
		FROM pg_stat_user_indexes s
		JOIN pg_index i ON i.indexrelid = s.indexrelid
		JOIN pg_class ic ON ic.oid = s.indexrelid
		WHERE s.schemaname = current_schema() AND s.idx_scan = 0
		ORDER BY ic.relpages DESC, s.relname, s.indexrelname
	`).Scan(&r.UnusedIndexes).Error; err != nil {
		return Report{}, fmt.Errorf("dbhealth: indexes: %w", err)
	}

	var tables []struct {
		Table                string
		LiveRows             int64
		DeadRows             int64
		SizeBytes            int64
		SizeKnown            bool
		Vacuuming            bool
		LastVacuum           *time.Time
		LastAutovacuum       *time.Time
		LastAnalyze          *time.Time
		LastAutoanalyze      *time.Time
		ModifiedSinceAnalyze int64
	}
	if err := db.Raw(`
		SELECT s.relname AS "table", s.n_live_tup AS live_rows, s.n_dead_tup AS dead_rows,
		       (c.relpages::bigint + COALESCE(t.relpages, 0)
		        + COALESCE((SELECT sum(ic.relpages) FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		                    WHERE i.indrelid = s.relid), 0)
		       )::bigint * current_setting('block_size')::bigint AS size_bytes,
		       -- reltuples が -1 なら、まだ一度も VACUUM / ANALYZE されていない (PG14 以降)。
		       c.reltuples >= 0 AS size_known,
		       EXISTS (SELECT 1 FROM pg_stat_progress_vacuum p WHERE p.relid = s.relid) AS vacuuming,
		       s.last_vacuum, s.last_autovacuum, s.last_analyze, s.last_autoanalyze,
		       s.n_mod_since_analyze AS modified_since_analyze
		FROM pg_stat_user_tables s
		JOIN pg_class c ON c.oid = s.relid
		LEFT JOIN pg_class t ON t.oid = c.reltoastrelid
		WHERE s.schemaname = current_schema()
		ORDER BY s.n_dead_tup DESC, s.relname
	`).Scan(&tables).Error; err != nil {
		return Report{}, fmt.Errorf("dbhealth: tables: %w", err)
	}
	for _, t := range tables {
		ts := TableStat{Table: t.Table, LiveRows: t.LiveRows, DeadRows: t.DeadRows, Vacuuming: t.Vacuuming,
			LastVacuum: latest(t.LastVacuum, t.LastAutovacuum), LastAnalyze: latest(t.LastAnalyze, t.LastAutoanalyze),
			ModifiedSinceAnalyze: t.ModifiedSinceAnalyze}
		if t.SizeKnown {
			size := t.SizeBytes
			ts.SizeBytes = &size
		}
		if total := t.LiveRows + t.DeadRows; total > 0 {
			ts.DeadRatio = float64(t.DeadRows) / float64(total)
		}
		r.Tables = append(r.Tables, ts)
	}
	r.Problems = r.findProblems(now)
	return r, nil
}

func latest(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	}
	return a
}
