package chart

import (
	"context"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// testDB / testRedis are set up once for all integration tests.
var (
	testDB    *gorm.DB
	testRedis *testutil.TestRedis
)

func TestMain(m *testing.M) {
	// PostgreSQL (テストDB)
	if db, err := testutil.OpenTestDB(); err == nil {
		testutil.ApplyMigrations(db)
		testDB = db
	}

	// Redis (ローカル)
	redisHost := testutil.EnvOrDefault("TEST_REDIS_HOST", "localhost")
	redisPort := testutil.EnvOrDefault("TEST_REDIS_PORT", "6379")
	addr := redisHost + ":" + redisPort
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err == nil {
		testRedis = &testutil.TestRedis{Client: client, Addr: addr}
	}

	code := m.Run()

	if testRedis != nil {
		_ = testRedis.Client.Close()
	}
	os.Exit(code)
}

// requirePostgres skips the test if no test database is available.
func requirePostgres(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("test database not available")
	}
}

// requireRedis skips the test if no test Redis is available.
func requireRedis(t *testing.T) {
	t.Helper()
	if testRedis == nil {
		t.Skip("test Redis not available")
	}
}

// notesIntegrationSchema は実際の `__chart__notes` テーブルとぴったり一致
// する必要がある。migration の DDL と同じ列順 / 型を使う。
var notesIntegrationSchema = Schema{
	Name: "notes",
	Columns: []ColumnDef{
		{Name: "local.total", Accumulate: true},
		{Name: "local.inc"},
		{Name: "local.dec"},
		{Name: "local.diffs.normal"},
		{Name: "local.diffs.reply"},
		{Name: "local.diffs.renote"},
		{Name: "local.diffs.withFile"},
		{Name: "remote.total", Accumulate: true},
		{Name: "remote.inc"},
		{Name: "remote.dec"},
		{Name: "remote.diffs.normal"},
		{Name: "remote.diffs.reply"},
		{Name: "remote.diffs.renote"},
		{Name: "remote.diffs.withFile"},
	},
}

var perUserIntegrationSchema = Schema{
	Name:    "perUserNotes",
	Grouped: true,
	Columns: []ColumnDef{
		{Name: "total", Accumulate: true},
		{Name: "inc", Range: RangeSmall},
		{Name: "dec", Range: RangeSmall},
		{Name: "diffs.normal", Range: RangeSmall},
		{Name: "diffs.reply", Range: RangeSmall},
		{Name: "diffs.renote", Range: RangeSmall},
		{Name: "diffs.withFile", Range: RangeSmall},
	},
}

var activeUsersIntegrationSchema = Schema{
	Name: "activeUsers",
	Columns: []ColumnDef{
		{Name: "readWrite", IntersectionOf: []string{"read", "write"}},
		{Name: "read", UniqueIncrement: true},
		{Name: "write", UniqueIncrement: true},
		{Name: "registeredWithinWeek", UniqueIncrement: true},
		{Name: "registeredWithinMonth", UniqueIncrement: true},
		{Name: "registeredWithinYear", UniqueIncrement: true},
		{Name: "registeredOutsideWeek", UniqueIncrement: true},
		{Name: "registeredOutsideMonth", UniqueIncrement: true},
		{Name: "registeredOutsideYear", UniqueIncrement: true},
	},
}

// truncateChartTable wipes the hour and day tables for the given chart
// so each test starts from a clean slate. The helper also enforces
// that postgres is available — every caller is an integration test.
func truncateChartTable(t *testing.T, chartName string) {
	t.Helper()
	requirePostgres(t)
	// 戻り値を検査する。黙って失敗すると前のテストの行が残り、原因から遠い
	// 症状に化ける (CLAUDE.md Section 4)。
	require.NoError(t, testDB.Exec(`TRUNCATE TABLE "`+HourTableName(chartName)+`"`).Error)
	require.NoError(t, testDB.Exec(`TRUNCATE TABLE "`+DayTableName(chartName)+`"`).Error)
}

func TestIntegration_GormRepository_InsertAndFindCurrent(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()

	row, err := repo.Insert(ctx, SpanHour, "", 1700000000, map[string]any{
		"local.total":        int64(5),
		"local.inc":          int64(1),
		"local.diffs.normal": int64(1),
	})
	require.NoError(t, err)
	require.NotZero(t, row.ID)

	got, err := repo.FindCurrent(ctx, SpanHour, "", 1700000000)
	require.NoError(t, err)
	assert.Equal(t, int64(5), toInt64(got.Cols["local.total"]))
	assert.Equal(t, int64(1), toInt64(got.Cols["local.inc"]))
}

func TestIntegration_GormRepository_FindCurrentNotFound(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	_, err := repo.FindCurrent(context.Background(), SpanHour, "", 1)
	if !errors.Is(err, ErrRowNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_GormRepository_FindLatestAndBefore(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()

	for i, ts := range []int64{1000, 2000, 3000} {
		_, err := repo.Insert(ctx, SpanHour, "", ts, map[string]any{
			"local.total": int64(i + 1),
		})
		require.NoError(t, err)
	}

	latest, err := repo.FindLatest(ctx, SpanHour, "")
	require.NoError(t, err)
	assert.Equal(t, int64(3000), latest.Date)

	before, err := repo.FindBefore(ctx, SpanHour, "", 2500)
	require.NoError(t, err)
	assert.Equal(t, int64(2000), before.Date)

	_, err = repo.FindBefore(ctx, SpanHour, "", 500)
	if !errors.Is(err, ErrRowNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_GormRepository_FindRange(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()

	for _, ts := range []int64{1000, 2000, 3000, 4000} {
		_, err := repo.Insert(ctx, SpanHour, "", ts, map[string]any{
			"local.inc": int64(ts / 1000),
		})
		require.NoError(t, err)
	}

	rows, err := repo.FindRange(ctx, SpanHour, "", 1500, 3500)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// 結果は date DESC
	assert.Equal(t, int64(3000), rows[0].Date)
	assert.Equal(t, int64(2000), rows[1].Date)
}

func TestIntegration_GormRepository_ApplyDeltas(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()

	row, err := repo.Insert(ctx, SpanHour, "", 1000, map[string]any{
		"local.inc": int64(1),
	})
	require.NoError(t, err)

	require.NoError(t, repo.ApplyDeltas(ctx, SpanHour, row.ID,
		map[string]int64{"local.inc": 4, "local.dec": 2}, nil, nil))

	got, err := repo.FindCurrent(ctx, SpanHour, "", 1000)
	require.NoError(t, err)
	assert.Equal(t, int64(5), toInt64(got.Cols["local.inc"]))
	assert.Equal(t, int64(2), toInt64(got.Cols["local.dec"]))
}

func TestIntegration_GormRepository_ApplyDeltasNoOpWhenEmpty(t *testing.T) {
	requirePostgres(t)
	repo := NewRepository(testDB, notesIntegrationSchema)
	require.NoError(t, repo.ApplyDeltas(context.Background(), SpanHour, 1, nil, nil, nil))
}

func TestIntegration_GormRepository_SetColumns(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()
	row, err := repo.Insert(ctx, SpanHour, "", 1000, map[string]any{
		"local.total": int64(5),
	})
	require.NoError(t, err)
	require.NoError(t, repo.SetColumns(ctx, SpanHour, row.ID, map[string]int64{
		"local.total": 99,
	}))
	got, err := repo.FindCurrent(ctx, SpanHour, "", 1000)
	require.NoError(t, err)
	assert.Equal(t, int64(99), toInt64(got.Cols["local.total"]))
}

func TestIntegration_GormRepository_SetColumnsEmpty(t *testing.T) {
	requirePostgres(t)
	repo := NewRepository(testDB, notesIntegrationSchema)
	require.NoError(t, repo.SetColumns(context.Background(), SpanHour, 0, nil))
}

func TestIntegration_GormRepository_GroupedTable(t *testing.T) {
	truncateChartTable(t, perUserIntegrationSchema.Name)
	repo := NewRepository(testDB, perUserIntegrationSchema)
	ctx := context.Background()

	_, err := repo.Insert(ctx, SpanHour, "user1", 1000, map[string]any{
		"total": int64(3),
		"inc":   int64(1),
	})
	require.NoError(t, err)
	_, err = repo.Insert(ctx, SpanHour, "user2", 1000, map[string]any{
		"total": int64(7),
		"inc":   int64(2),
	})
	require.NoError(t, err)

	r1, err := repo.FindCurrent(ctx, SpanHour, "user1", 1000)
	require.NoError(t, err)
	assert.Equal(t, int64(3), toInt64(r1.Cols["total"]))
	assert.Equal(t, "user1", r1.Group.String)

	r2, err := repo.FindLatest(ctx, SpanHour, "user2")
	require.NoError(t, err)
	assert.Equal(t, int64(7), toInt64(r2.Cols["total"]))
}

func TestIntegration_GormRepository_UniqueIncrementApplyDeltas(t *testing.T) {
	truncateChartTable(t, activeUsersIntegrationSchema.Name)
	repo := NewRepository(testDB, activeUsersIntegrationSchema)
	ctx := context.Background()

	row, err := repo.Insert(ctx, SpanHour, "", 1000, map[string]any{
		"read": int64(0),
	})
	require.NoError(t, err)

	require.NoError(t, repo.ApplyDeltas(ctx, SpanHour, row.ID, nil,
		map[string][]string{"read": {"u1", "u2"}},
		map[string]int64{"read": 2},
	))

	got, err := repo.FindCurrent(ctx, SpanHour, "", 1000)
	require.NoError(t, err)
	assert.Equal(t, int64(2), toInt64(got.Cols["read"]))
	// unique-temp 配列も更新されている。**型まで見ること。** assert.Contains は
	// string に対する部分文字列判定としても成立するので、`[]string` を要求しないと
	// driver が string を返していても素通りする (#2652)。
	uniques, ok := got.Cols["read:unique"].([]string)
	require.True(t, ok, "unique-temp は []string に正規化されること")
	assert.ElementsMatch(t, []string{"u1", "u2"}, uniques)
}

// TestIntegration_GormRepository_UniqueAppendBindsHostileValues pins that
// unique-temp values survive a round trip through PostgreSQL unchanged.
//
// unique 配列の要素は行の値そのもの (外部由来の文字列を含みうる) なので、
// 構造上意味を持つ文字を含みうる。`ApplyDeltas` は配列リテラルを SQL テキストへ
// 入れず `?::varchar[]` で bind するので、以下はすべてただの文字列として往復する。
//
// **既存の unique テストは値が `u1` / `u2` だけで、この性質を見ていない** —
// 配列リテラルを書式へ差し込む形に変えても緑のまま通る (実測)。
//
// **このテストが受け持つのは静的ゲートには見えない側**。書式そのものの退行は
// `make sqlbind-check` が静的に落とす (あちらは `ApplyDeltas` が組む書式集合を
// pin している) ので、ここは静的には見えない側を受け持つ — `pgArrayLiteral` の
// `\` / `"` のエスケープを片方でも外すと落ちる (実測。この 2 つは
// `TestPgArrayLiteral_EscapesQuotesAndBackslashes` が書き側の出力を golden で
// pin しているので、そちらでも落ちる)。**Go 内で完結する
// `TestPgArrayLiteral_RoundTrip` では代わりにならない** — 書き側と読み側を対称に
// 壊すとあちらは通るが、ここは PostgreSQL の入力パーサが権威なので落ちる (実測)。
//
// **クォートを含む値は load-bearing。** 「整理」して落とすと、素の補間に対する
// 振る舞い側の検出が消える。
//
// **NUL は意図的に入れていない。** PostgreSQL がプロトコル層で弾くので値の
// 往復として成立しない (#3025 と同型)。ここへ届く値はいずれも正規化済みか
// ID 形式で、NUL を含みえない。
func TestIntegration_GormRepository_UniqueAppendBindsHostileValues(t *testing.T) {
	truncateChartTable(t, activeUsersIntegrationSchema.Name)
	repo := NewRepository(testDB, activeUsersIntegrationSchema)
	ctx := context.Background()

	row, err := repo.Insert(ctx, SpanHour, "", 4000, map[string]any{
		"read": int64(0),
	})
	require.NoError(t, err)

	hostile := []string{
		`plain`,
		`quote'inside`,
		`dquote"inside`,
		`back\slash`,
		`brace{}pair`,
		`comma,separated`,
		"tab\tand\nnewline",
		`日本語とマルチバイト`,
	}
	require.NoError(t, repo.ApplyDeltas(ctx, SpanHour, row.ID, nil,
		map[string][]string{"read": hostile},
		map[string]int64{"read": int64(len(hostile))},
	))

	got, err := repo.FindCurrent(ctx, SpanHour, "", 4000)
	require.NoError(t, err)
	uniques, ok := got.Cols["read:unique"].([]string)
	require.True(t, ok, "unique-temp は []string に正規化されること")
	// 値がそのまま戻ること。エスケープが片道だけ壊れると件数は合うので、
	// 濃度ではなく要素そのものを突き合わせる。
	assert.ElementsMatch(t, hostile, uniques)
	assert.Equal(t, int64(len(hostile)), toInt64(got.Cols["read"]))
}

func TestIntegration_GormRepository_ResetUniqueTempColumns(t *testing.T) {
	truncateChartTable(t, activeUsersIntegrationSchema.Name)
	repo := NewRepository(testDB, activeUsersIntegrationSchema)
	ctx := context.Background()

	row, err := repo.Insert(ctx, SpanHour, "", 2000, map[string]any{
		"read": int64(0),
	})
	require.NoError(t, err)
	require.NoError(t, repo.ApplyDeltas(ctx, SpanHour, row.ID, nil,
		map[string][]string{"read": {"u1"}}, nil))
	require.NoError(t, repo.ResetUniqueTempColumns(ctx, SpanHour, 1000, 3000, []string{"read"}))

	got, err := repo.FindCurrent(ctx, SpanHour, "", 2000)
	require.NoError(t, err)
	// 空配列にリセットされている。型が違えば分岐に入らず素通りしていたので、
	// ここも型を要求する (#2652)。
	uniques, ok := got.Cols["read:unique"].([]string)
	require.True(t, ok, "unique-temp は []string に正規化されること")
	assert.Empty(t, uniques)
}

func TestIntegration_GormRepository_ResetUniqueTempEmptyNoOp(t *testing.T) {
	requirePostgres(t)
	repo := NewRepository(testDB, activeUsersIntegrationSchema)
	require.NoError(t, repo.ResetUniqueTempColumns(context.Background(), SpanHour, 0, 0, nil))
}

// unique 列を実 DB の Chart.Save に通す。**ここが #2652 の再現テスト。**
// fakeRepo は unique-temp 配列を []string で持つのでこの経路を試せず、pgx が
// varchar[] を string で返すことに気付けなかった。
func TestIntegration_ChartEngine_UniqueAccumulatesAcrossSaves(t *testing.T) {
	truncateChartTable(t, activeUsersIntegrationSchema.Name)
	repo := NewRepository(testDB, activeUsersIntegrationSchema)
	clk := newFakeClock(time.Date(2026, 4, 9, 12, 30, 0, 0, time.UTC))
	c, err := New(Config{Schema: activeUsersIntegrationSchema, Repo: repo, Lock: NewMemoryLocker(), Clock: clk})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, c.Commit(Diff{"read": []string{"u1", "u2"}, "write": []string{"u1"}}, ""))
	require.NoError(t, c.Save(ctx))

	row, err := repo.FindCurrent(ctx, SpanHour, "", truncateToHour(clk.Now()).Unix())
	require.NoError(t, err)
	// scanRow が driver の値を []string に正規化していること。
	uniques, ok := row.Cols["read:unique"].([]string)
	require.True(t, ok, "unique-temp は []string で読めること")
	assert.ElementsMatch(t, []string{"u1", "u2"}, uniques)
	assert.Equal(t, int64(2), toInt64(row.Cols["read"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["readWrite"]))

	// 同じ bucket に別のキーを積む。行側の集合が読めていれば濃度は累積する。
	require.NoError(t, c.Commit(Diff{"read": []string{"u2", "u3"}}, ""))
	require.NoError(t, c.Save(ctx))

	row, err = repo.FindCurrent(ctx, SpanHour, "", truncateToHour(clk.Now()).Unix())
	require.NoError(t, err)
	assert.Equal(t, int64(3), toInt64(row.Cols["read"]), "bucket をまたいで濃度が累積する")
	// 既に行にある u2 は積み直さない。
	uniques, _ = row.Cols["read:unique"].([]string)
	assert.ElementsMatch(t, []string{"u1", "u2", "u3"}, uniques, "既存キーを重複して積まない")
	// write は今回の差分に無いので据え置き。intersection も再計算されて維持される。
	assert.Equal(t, int64(1), toInt64(row.Cols["write"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["readWrite"]))
}

// hour と day で行の unique-temp 配列の中身が違う状態を作る。**これが無いと
// hour 行と day 行を取り違える実装ミスをテストが一切検出できない** (固定 clock の
// テストだけだと両者の配列が常に同じになるため)。
//
// day は 1 日ぶんを積むので hour より大きい集合を持つ。append の filter も
// bake も span ごとに行を見る必要がある。
func TestIntegration_ChartEngine_HourAndDayDivergeWithinSameDay(t *testing.T) {
	truncateChartTable(t, activeUsersIntegrationSchema.Name)
	repo := NewRepository(testDB, activeUsersIntegrationSchema)
	clk := newFakeClock(time.Date(2026, 4, 9, 12, 30, 0, 0, time.UTC))
	c, err := New(Config{Schema: activeUsersIntegrationSchema, Repo: repo, Lock: NewMemoryLocker(), Clock: clk})
	require.NoError(t, err)
	ctx := context.Background()

	// 12 時台に u1 を積む。hour(12) と day(04-09) の両方に入る。
	require.NoError(t, c.Commit(Diff{"read": []string{"u1"}}, ""))
	require.NoError(t, c.Save(ctx))

	// 13 時台に u2 / u3。hour は新しいバケットなので u1 を持たないが、
	// day は同じバケットのまま u1 を持っている。
	clk.set(time.Date(2026, 4, 9, 13, 15, 0, 0, time.UTC))
	require.NoError(t, c.Commit(Diff{"read": []string{"u2", "u3"}}, ""))
	require.NoError(t, c.Save(ctx))

	hour, err := repo.FindCurrent(ctx, SpanHour, "", truncateToHour(clk.Now()).Unix())
	require.NoError(t, err)
	day, err := repo.FindCurrent(ctx, SpanDay, "", truncateToSpan(clk.Now(), SpanDay).Unix())
	require.NoError(t, err)

	hourUniques, ok := hour.Cols["read:unique"].([]string)
	require.True(t, ok)
	dayUniques, ok := day.Cols["read:unique"].([]string)
	require.True(t, ok)

	// 13 時のバケットは 13 時台のぶんだけ。
	assert.ElementsMatch(t, []string{"u2", "u3"}, hourUniques)
	assert.Equal(t, int64(2), toInt64(hour.Cols["read"]))
	// day は 1 日ぶん。
	assert.ElementsMatch(t, []string{"u1", "u2", "u3"}, dayUniques)
	assert.Equal(t, int64(3), toInt64(day.Cols["read"]))

	// もう一度 u1 を積む。day は既に持っているので配列は伸びず、hour には入る。
	require.NoError(t, c.Commit(Diff{"read": []string{"u1"}}, ""))
	require.NoError(t, c.Save(ctx))

	hour, err = repo.FindCurrent(ctx, SpanHour, "", truncateToHour(clk.Now()).Unix())
	require.NoError(t, err)
	day, err = repo.FindCurrent(ctx, SpanDay, "", truncateToSpan(clk.Now(), SpanDay).Unix())
	require.NoError(t, err)
	hourUniques, _ = hour.Cols["read:unique"].([]string)
	dayUniques, _ = day.Cols["read:unique"].([]string)
	assert.ElementsMatch(t, []string{"u1", "u2", "u3"}, hourUniques, "hour には u1 が入る")
	assert.ElementsMatch(t, []string{"u1", "u2", "u3"}, dayUniques, "day は既に持っているので伸びない")
	assert.Equal(t, int64(3), toInt64(hour.Cols["read"]))
	assert.Equal(t, int64(3), toInt64(day.Cols["read"]))
}

// 差分の無い列を 0 で上書きしないこと。**#2652 の 2 つ目の症状**で、
// uniqueIncrement 列は毎回 setInts に載っていたため、その列に何も起きていない
// Save が既存の濃度を潰していた。
//
// このテストが捕まえるのは **scanRow のパース**の方。パースが直れば毎回 bake し
// 直しても同じ値が再計算されるので、bake の範囲を絞る変更単独の回帰は
// TestBakeUniqueAndIntersection_OnlyBakesColumnsWithDiff /
// _DoesNotZeroClearedColumn が見る。
func TestIntegration_ChartEngine_SaveDoesNotClobberUntouchedColumns(t *testing.T) {
	truncateChartTable(t, activeUsersIntegrationSchema.Name)
	repo := NewRepository(testDB, activeUsersIntegrationSchema)
	clk := newFakeClock(time.Date(2026, 4, 9, 12, 30, 0, 0, time.UTC))
	c, err := New(Config{Schema: activeUsersIntegrationSchema, Repo: repo, Lock: NewMemoryLocker(), Clock: clk})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, c.Commit(Diff{"read": []string{"u1", "u2"}, "write": []string{"u1"}}, ""))
	require.NoError(t, c.Save(ctx))

	// まったく無関係な列だけを動かす。
	require.NoError(t, c.Commit(Diff{"registeredWithinWeek": []string{"u9"}}, ""))
	require.NoError(t, c.Save(ctx))

	row, err := repo.FindCurrent(ctx, SpanHour, "", truncateToHour(clk.Now()).Unix())
	require.NoError(t, err)
	assert.Equal(t, int64(2), toInt64(row.Cols["read"]), "触っていない unique 列が 0 に落ちない")
	assert.Equal(t, int64(1), toInt64(row.Cols["write"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["readWrite"]), "intersection も維持される")
	assert.Equal(t, int64(1), toInt64(row.Cols["registeredWithinWeek"]))
}

func TestIntegration_ChartEngine_EndToEnd(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	clk := newFakeClock(time.Date(2026, 4, 9, 12, 30, 0, 0, time.UTC))
	c, err := New(Config{
		Schema: notesIntegrationSchema,
		Repo:   repo,
		Lock:   NewMemoryLocker(),
		Clock:  clk,
	})
	require.NoError(t, err)

	require.NoError(t, c.Commit(Diff{"local.inc": 1, "local.total": 1}, ""))
	require.NoError(t, c.Commit(Diff{"local.inc": 2, "local.total": 2}, ""))
	require.NoError(t, c.Save(context.Background()))

	out, err := c.GetChart(context.Background(), SpanHour, 1, nil, "")
	require.NoError(t, err)
	assert.Equal(t, []int64{3}, out["local.inc"])
	assert.Equal(t, []int64{3}, out["local.total"])
}

func TestIntegration_RedisLocker_AcquireRelease(t *testing.T) {
	requireRedis(t)
	testRedis.FlushAll(context.Background())
	locker := NewRedisLocker(testRedis.Client, "")
	ctx := context.Background()

	release1, err := locker.Acquire(ctx, "key1", time.Second)
	require.NoError(t, err)

	// 別 key は競合しない
	release2, err := locker.Acquire(ctx, "key2", time.Second)
	require.NoError(t, err)
	release2()

	// 同 key は ctx タイムアウトで弾かれる
	deadline, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := locker.Acquire(deadline, "key1", time.Second); err == nil {
		t.Fatal("expected deadline error on contended lock")
	}

	// 解放後は再取得できる
	release1()
	release3, err := locker.Acquire(ctx, "key1", time.Second)
	require.NoError(t, err)
	release3()
}

func TestIntegration_RedisLocker_ReleaseScriptDoesNotErase(t *testing.T) {
	requireRedis(t)
	testRedis.FlushAll(context.Background())
	locker := NewRedisLocker(testRedis.Client, "")

	// 自分の token じゃないキーを置いておく
	require.NoError(t, testRedis.Client.Set(context.Background(), "chartInsertLock:k", "other", time.Minute).Err())

	// 別のキーを取得 → release しても元の "k" は触らない
	release, err := locker.Acquire(context.Background(), "different", time.Second)
	require.NoError(t, err)
	release()

	val, err := testRedis.Client.Get(context.Background(), "chartInsertLock:k").Result()
	require.NoError(t, err)
	assert.Equal(t, "other", val)
}

func TestIntegration_RedisLocker_NilClient(t *testing.T) {
	var l *RedisLocker
	if _, err := l.Acquire(context.Background(), "k", time.Second); err == nil {
		t.Fatal("expected error from nil locker")
	}
}

func TestIntegration_RedisLocker_DefaultPrefix(t *testing.T) {
	requireRedis(t)
	l := NewRedisLocker(testRedis.Client, "")
	assert.Equal(t, "chartInsertLock:", l.prefix)
}

func TestIntegration_RedisLocker_DefaultTTL(t *testing.T) {
	requireRedis(t)
	testRedis.FlushAll(context.Background())
	l := NewRedisLocker(testRedis.Client, "")
	// 0 ttl → 30s default。Test 完了で release が走るので副作用なし。
	release, err := l.Acquire(context.Background(), "ttl-default", 0)
	require.NoError(t, err)
	release()
}

func TestIntegration_RedisLocker_RedisError(t *testing.T) {
	bad := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer bad.Close()
	l := NewRedisLocker(bad, "")
	if _, err := l.Acquire(context.Background(), "k", time.Second); err == nil {
		t.Fatal("expected error from unreachable redis")
	}
}

func TestIntegration_GormRepository_FindBeforeUngrouped(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()

	// FindBefore on the ungrouped notes table — covers the non-grouped branch.
	_, err := repo.Insert(ctx, SpanHour, "", 5000, map[string]any{
		"local.inc": int64(1),
	})
	require.NoError(t, err)
	got, err := repo.FindBefore(ctx, SpanHour, "", 6000)
	require.NoError(t, err)
	assert.Equal(t, int64(5000), got.Date)
}

func TestIntegration_GormRepository_FindRangeUngrouped(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()

	for _, ts := range []int64{1000, 2000, 3000} {
		_, err := repo.Insert(ctx, SpanHour, "", ts, map[string]any{"local.inc": int64(ts)})
		require.NoError(t, err)
	}
	rows, err := repo.FindRange(ctx, SpanHour, "", 0, 4000)
	require.NoError(t, err)
	assert.Len(t, rows, 3)
}

func TestIntegration_GormRepository_DayTableInsertAndFind(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()

	_, err := repo.Insert(ctx, SpanDay, "", 9000, map[string]any{
		"local.inc": int64(11),
	})
	require.NoError(t, err)
	got, err := repo.FindCurrent(ctx, SpanDay, "", 9000)
	require.NoError(t, err)
	assert.Equal(t, int64(11), toInt64(got.Cols["local.inc"]))
}

func TestIntegration_GormRepository_InsertSkipsUnknownColumns(t *testing.T) {
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()
	row, err := repo.Insert(ctx, SpanHour, "", 1234, map[string]any{
		"local.inc":     int64(1),
		"unknown.field": int64(99),
	})
	require.NoError(t, err)
	require.NotZero(t, row.ID)
}

func TestIntegration_GormRepository_FindBeforeGrouped(t *testing.T) {
	truncateChartTable(t, perUserIntegrationSchema.Name)
	repo := NewRepository(testDB, perUserIntegrationSchema)
	ctx := context.Background()

	for _, ts := range []int64{1000, 2000, 3000} {
		_, err := repo.Insert(ctx, SpanHour, "u1", ts, map[string]any{"total": int64(1)})
		require.NoError(t, err)
	}
	got, err := repo.FindBefore(ctx, SpanHour, "u1", 2500)
	require.NoError(t, err)
	assert.Equal(t, int64(2000), got.Date)
}

func TestIntegration_GormRepository_FindRangeGrouped(t *testing.T) {
	truncateChartTable(t, perUserIntegrationSchema.Name)
	repo := NewRepository(testDB, perUserIntegrationSchema)
	ctx := context.Background()

	for _, ts := range []int64{1000, 2000, 3000} {
		_, err := repo.Insert(ctx, SpanHour, "u1", ts, map[string]any{"total": int64(1)})
		require.NoError(t, err)
		_, err = repo.Insert(ctx, SpanHour, "u2", ts, map[string]any{"total": int64(1)})
		require.NoError(t, err)
	}
	got, err := repo.FindRange(ctx, SpanHour, "u1", 1500, 4000)
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func TestIntegration_GormRepository_InsertGroupedSchemaSkipsUnknown(t *testing.T) {
	// 既存テーブルへ unknown 列を渡しても黙って無視される (列順安定の
	// ループ内 default 分岐) のを踏む。
	truncateChartTable(t, perUserIntegrationSchema.Name)
	repo := NewRepository(testDB, perUserIntegrationSchema)
	row, err := repo.Insert(context.Background(), SpanHour, "u1", 4242, map[string]any{
		"total":   int64(1),
		"missing": int64(99),
	})
	require.NoError(t, err)
	require.NotZero(t, row.ID)
}

func TestScanRow_HandlesUniqueTempAndNullGroup(t *testing.T) {
	repo := &gormRepository{schema: notesIntegrationSchema}
	row := repo.scanRow(map[string]any{
		"id":                  int64(7),
		"date":                int64(1234),
		"group":               nil,
		"___local_inc":        int64(2),
		"unique_temp___local": []any{"a"},
	})
	if row.ID != 7 || row.Date != 1234 {
		t.Fatalf("got %+v", row)
	}
	if row.Group.Valid {
		t.Fatal("expected null group")
	}
	if _, ok := row.Cols["local.inc"]; !ok {
		t.Fatal("missing local.inc")
	}
	if _, ok := row.Cols["local:unique"]; !ok {
		t.Fatal("missing local:unique")
	}
}

func TestScanRow_NonNullGroup(t *testing.T) {
	repo := &gormRepository{schema: perUserIntegrationSchema}
	row := repo.scanRow(map[string]any{
		"id":    int64(1),
		"date":  int64(0),
		"group": "u1",
	})
	if !row.Group.Valid || row.Group.String != "u1" {
		t.Fatalf("got %+v", row.Group)
	}
}

// TestIntegration_GormRepository_InsertEmptyReturning forces the
// `len(rows) == 0` defensive branch by attaching a BEFORE INSERT
// trigger that returns NULL — PostgreSQL then skips the insert and
// the RETURNING clause yields zero rows.
func TestIntegration_GormRepository_InsertEmptyReturning(t *testing.T) {
	requirePostgres(t)
	// Use a dedicated chart name so the trigger does not interfere with
	// other tests. We piggy-back on the existing migration table for
	// federation since it has the right shape; instead create a tiny
	// throwaway table that matches the schema we declare.
	const tbl = "__chart__skip_insert"
	const tblDay = "__chart_day__skip_insert"
	const fnName = "chart_skip_insert_fn"
	const trgName = "chart_skip_insert_trg"

	// Drop and recreate the support objects so reruns are clean.
	testDB.Exec(`DROP TRIGGER IF EXISTS ` + trgName + ` ON "` + tbl + `"`)
	testDB.Exec(`DROP FUNCTION IF EXISTS ` + fnName + `()`)
	testDB.Exec(`DROP TABLE IF EXISTS "` + tbl + `"`)
	testDB.Exec(`DROP TABLE IF EXISTS "` + tblDay + `"`)

	testDB.Exec(`CREATE TABLE "` + tbl + `" (
		"id" SERIAL PRIMARY KEY,
		"date" integer NOT NULL,
		"___v" integer NOT NULL DEFAULT 0
	)`)
	testDB.Exec(`CREATE TABLE "` + tblDay + `" (
		"id" SERIAL PRIMARY KEY,
		"date" integer NOT NULL,
		"___v" integer NOT NULL DEFAULT 0
	)`)
	testDB.Exec(`CREATE OR REPLACE FUNCTION ` + fnName + `() RETURNS trigger AS $$ BEGIN RETURN NULL; END; $$ LANGUAGE plpgsql`)
	testDB.Exec(`CREATE TRIGGER ` + trgName + ` BEFORE INSERT ON "` + tbl + `" FOR EACH ROW EXECUTE FUNCTION ` + fnName + `()`)

	defer func() {
		testDB.Exec(`DROP TRIGGER IF EXISTS ` + trgName + ` ON "` + tbl + `"`)
		testDB.Exec(`DROP FUNCTION IF EXISTS ` + fnName + `()`)
		testDB.Exec(`DROP TABLE IF EXISTS "` + tbl + `"`)
		testDB.Exec(`DROP TABLE IF EXISTS "` + tblDay + `"`)
	}()

	repo := NewRepository(testDB, Schema{
		Name:    "skipInsert",
		Columns: []ColumnDef{{Name: "v"}},
	})
	_, err := repo.Insert(context.Background(), SpanHour, "", 1, map[string]any{"v": int64(1)})
	if err == nil {
		t.Fatal("expected error from empty RETURNING")
	}
}

func TestIntegration_GormRepository_ErrorOnClosedDB(t *testing.T) {
	requirePostgres(t)
	// Use the testDB but issue a query against a non-existent column to
	// force a SQL error from the underlying driver.
	repo := NewRepository(testDB, Schema{
		Name:    "doesnotexist", // tableName -> __chart__doesnotexist
		Columns: []ColumnDef{{Name: "x"}},
	})
	ctx := context.Background()
	if _, err := repo.FindCurrent(ctx, SpanHour, "", 0); err == nil {
		t.Fatal("expected error querying nonexistent table")
	}
	if _, err := repo.FindLatest(ctx, SpanHour, ""); err == nil {
		t.Fatal("expected error querying nonexistent table")
	}
	if _, err := repo.FindBefore(ctx, SpanHour, "", 0); err == nil {
		t.Fatal("expected error querying nonexistent table")
	}
	if _, err := repo.FindRange(ctx, SpanHour, "", 0, 100); err == nil {
		t.Fatal("expected error querying nonexistent table")
	}
	if _, err := repo.Insert(ctx, SpanHour, "", 0, map[string]any{"x": int64(1)}); err == nil {
		t.Fatal("expected error inserting into nonexistent table")
	}
	if err := repo.ApplyDeltas(ctx, SpanHour, 1, map[string]int64{"x": 1}, nil, nil); err == nil {
		t.Fatal("expected error from ApplyDeltas")
	}
	if err := repo.SetColumns(ctx, SpanHour, 1, map[string]int64{"x": 1}); err == nil {
		t.Fatal("expected error from SetColumns")
	}
	if err := repo.ResetUniqueTempColumns(ctx, SpanHour, 0, 100, []string{"x"}); err == nil {
		t.Fatal("expected error from ResetUniqueTempColumns")
	}
}

func TestIntegration_ChartEngine_GetChart_FindBeforeError(t *testing.T) {
	// Tests the error propagation path in GetChart by issuing a query
	// against a table that has rows in range — exercising the
	// "non-empty rows but earliest row != start" branch which calls
	// FindBefore.
	truncateChartTable(t, notesIntegrationSchema.Name)
	repo := NewRepository(testDB, notesIntegrationSchema)
	ctx := context.Background()
	clk := newFakeClock(time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC))
	c, err := New(Config{Schema: notesIntegrationSchema, Repo: repo, Lock: NewMemoryLocker(), Clock: clk})
	require.NoError(t, err)

	// Insert one row at the end of the requested window so that the
	// "earliest row != start" branch fires.
	bucket := truncateToHour(clk.Now())
	_, err = repo.Insert(ctx, SpanHour, "", bucket.Unix(), map[string]any{"local.inc": int64(1)})
	require.NoError(t, err)

	out, err := c.GetChart(ctx, SpanHour, 5, nil, "")
	require.NoError(t, err)
	assert.Len(t, out["local.inc"], 5)
}

// **列の型の範囲で頭打ちにする (upstream 2026.9.1 #17931)。** 範囲を超える
// 加算は UPDATE 全体を失敗させ、同じ行の他の列の記録まで落とす。smallint /
// integer は上限・下限で止まり、代入 (cardinality や引き継ぎ) も丸める。
func TestIntegration_GormRepository_ApplyDeltasClampsToColumnRange(t *testing.T) {
	truncateChartTable(t, perUserIntegrationSchema.Name)
	repo := NewRepository(testDB, perUserIntegrationSchema)
	ctx := context.Background()

	row, err := repo.Insert(ctx, SpanHour, "u1", 1000, map[string]any{"total": int64(0), "inc": int64(0)})
	require.NoError(t, err)
	get := func(name string) int64 {
		t.Helper()
		got, err := repo.FindCurrent(ctx, SpanHour, "u1", 1000)
		require.NoError(t, err)
		return toInt64(got.Cols[name])
	}

	// smallint: 上限で止まり、他の列の加算も失われない。
	require.NoError(t, repo.SetColumns(ctx, SpanHour, row.ID, map[string]int64{"inc": 32760}))
	require.NoError(t, repo.ApplyDeltas(ctx, SpanHour, row.ID, map[string]int64{"inc": 100, "dec": 3}, nil, nil))
	assert.Equal(t, int64(math.MaxInt16), get("inc"))
	assert.Equal(t, int64(3), get("dec"), "同じ UPDATE の他の列も書かれる")

	// smallint: 下限で止まる。
	require.NoError(t, repo.SetColumns(ctx, SpanHour, row.ID, map[string]int64{"inc": -32760}))
	require.NoError(t, repo.ApplyDeltas(ctx, SpanHour, row.ID, map[string]int64{"inc": -100}, nil, nil))
	assert.Equal(t, int64(math.MinInt16), get("inc"))

	// integer: 上限で止まる。
	require.NoError(t, repo.SetColumns(ctx, SpanHour, row.ID, map[string]int64{"total": math.MaxInt32 - 5}))
	require.NoError(t, repo.ApplyDeltas(ctx, SpanHour, row.ID, map[string]int64{"total": 100}, nil, nil))
	assert.Equal(t, int64(math.MaxInt32), get("total"))

	// 代入も範囲に丸める (unique の cardinality / 引き継ぎのコピー)。
	require.NoError(t, repo.ApplyDeltas(ctx, SpanHour, row.ID, nil, nil, map[string]int64{"inc": 40000}))
	assert.Equal(t, int64(math.MaxInt16), get("inc"))
	require.NoError(t, repo.SetColumns(ctx, SpanHour, row.ID, map[string]int64{"total": math.MaxInt32 + 10}))
	assert.Equal(t, int64(math.MaxInt32), get("total"))
}
