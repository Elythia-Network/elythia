package repository

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// migration 000094 の統合ロジックを実 PostgreSQL で固定する。
//
// **通常の migration 適用では一度も実行されない。** `ApplyMigrations` が流すのは
// 行が 1 件も無い fresh schema なので、`DO $$ ... $$` のループ本体に入らない。
// つまり**この変更で最もリスクの高い部分 (行の DELETE と値の書き換え) に、回帰を
// 止めるものが何も無い**状態になる。ここで migration 前の形を自分で作って流す。
//
// **`ApplyMigrations` は呼ばない。** 呼ぶと 000094 まで適用済みの形になり、
// migration 前の状態を作れない。必要なのは `user_ip` 1 つだけ。
//
// **列を DROP して作り直さない (#2756)。** PostgreSQL は `DROP COLUMN` した列も
// 1 テーブル 1600 列の上限に数えるので、実行のたびに枠が減る。テーブルごと
// 作り直せば枠は戻る。
var (
	ipMigDB     *gorm.DB
	ipMigOnce   sync.Once
	ipMigDBErr  error
	ipMigUpOnce sync.Once
	ipMigUpSQL  string
	ipMigUpErr  error
)

func ipMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	ipMigOnce.Do(func() {
		ipMigDB, ipMigDBErr = testutil.OpenTestDBSchema("ipmig")
	})
	require.NoError(t, ipMigDBErr)
	require.NotNil(t, ipMigDB)
	return ipMigDB
}

// userIPMigrationSQL reads the up migration once.
func userIPMigrationSQL(t *testing.T) string {
	t.Helper()
	ipMigUpOnce.Do(func() {
		b, err := os.ReadFile(filepath.Join("..", "..", "migration", "000094_user_ip_observation.up.sql"))
		ipMigUpSQL, ipMigUpErr = string(b), err
	})
	require.NoError(t, ipMigUpErr)
	// **空振りを許さない。** ファイル名を変えたり中身を空にしたとき、テストが
	// 「何も流さずに緑」になるのを防ぐ。
	require.Contains(t, ipMigUpSQL, "DO $$", "000094 の統合ブロックが読めていない")
	return ipMigUpSQL
}

// resetPreMigrationUserIP rebuilds `user_ip` in its pre-000094 (= migration
// 000029) shape.
func resetPreMigrationUserIP(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`DROP TABLE IF EXISTS "user_ip"`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE "user_ip" (
		"id" bigserial PRIMARY KEY,
		"createdAt" timestamp with time zone NOT NULL,
		"userId" varchar(32) NOT NULL,
		"ip" varchar(128) NOT NULL
	)`).Error)
	require.NoError(t, db.Exec(
		`CREATE UNIQUE INDEX "IDX_user_ip_userId_ip" ON "user_ip" ("userId", "ip")`).Error)
}

type preMigRow struct {
	userID string
	ip     string
	// createdAt は `YYYY-MM-DD`。**UTC として入れる** — オフセットを付けないと
	// セッションの timezone で解釈され、JST の手元では前日として保存される
	// (読み出しを UTC に揃えているので、比較がその分ずれる)。
	createdAt string
}

func seedPreMigrationRows(t *testing.T, db *gorm.DB, rows []preMigRow) {
	t.Helper()
	for _, r := range rows {
		require.NoError(t, db.Exec(
			`INSERT INTO "user_ip" ("createdAt", "userId", ip) VALUES (?, ?, ?)`,
			r.createdAt+"T00:00:00Z", r.userID, r.ip).Error)
	}
}

type postMigRow struct {
	UserID    string `gorm:"column:userId"`
	IP        string `gorm:"column:ip"`
	FirstSeen string `gorm:"column:first_seen"`
	LastSeen  string `gorm:"column:last_seen"`
	Count     int    `gorm:"column:observationCount"`
}

func readPostMigration(t *testing.T, db *gorm.DB) []postMigRow {
	t.Helper()
	var out []postMigRow
	require.NoError(t, db.Raw(`
		SELECT "userId", ip,
		       to_char("createdAt" AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS first_seen,
		       to_char("lastSeenAt" AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS last_seen,
		       "observationCount"
		FROM "user_ip" ORDER BY "userId", ip`).Scan(&out).Error)
	return out
}

func applyUserIPMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(userIPMigrationSQL(t)).Error)
}

// 表記揺れで分かれていた行が 1 行に畳まれ、初回は最古・最終は最新・回数は合算に
// なること。**3 行以上**と、正規形の行が最初から無い場合 (rename してから後続が
// 合流する経路) の両方を見る。
func TestUserIPMigration_MergesNormalizedDuplicates(t *testing.T) {
	db := ipMigrationDB(t)
	resetPreMigrationUserIP(t, db)
	seedPreMigrationRows(t, db, []preMigRow{
		// 正規形が最初からある (keeper 有の経路)。
		{"u1", "192.0.2.1", "2026-01-01"},
		{"u1", "::ffff:192.0.2.1", "2026-02-01"},
		{"u1", "::FFFF:192.0.2.1", "2026-03-01"},
		// **id は後ろだが観測は最古。** これが無いと「最終観測を GREATEST で
		// 取らずに上書きする」変異が検出できない (id 昇順に処理するので、
		// 揃っていると常に新しい方が最後に来る)。
		{"u1", "::ffff:c000:201", "2025-06-01"},
		// 正規形が無い (rename → 後続が合流する経路)。
		{"u2", "::ffff:203.0.113.9", "2026-01-10"},
		{"u2", "::FFFF:203.0.113.9", "2026-02-10"},
		{"u2", "::ffff:cb00:7109", "2026-03-10"},
		// **別の利用者は統合しない。**
		{"u3", "::ffff:192.0.2.1", "2026-01-20"},
		// IPv6 の表記揺れ。
		{"u4", "2001:DB8::0001", "2026-01-05"},
		{"u4", "2001:db8:0:0:0:0:0:1", "2026-02-05"},
	})

	applyUserIPMigration(t, db)

	assert.Equal(t, []postMigRow{
		{"u1", "192.0.2.1", "2025-06-01", "2026-03-01", 4},
		{"u2", "203.0.113.9", "2026-01-10", "2026-03-10", 3},
		{"u3", "192.0.2.1", "2026-01-20", "2026-01-20", 1},
		{"u4", "2001:db8::1", "2026-01-05", "2026-02-05", 2},
	}, readPostMigration(t, db))
}

// **正規化できない値は触らない (消しもしない)。** `inet` が読めない形と、
// 読めてしまうが IP ではない形 (CIDR) の両方。
func TestUserIPMigration_LeavesUnparsableValuesAlone(t *testing.T) {
	db := ipMigrationDB(t)
	resetPreMigrationUserIP(t, db)
	seedPreMigrationRows(t, db, []preMigRow{
		{"u1", "not-an-ip", "2026-01-01"},
		// **CIDR を畳むと実在しない観測になる** (`host()` が 192.0.2.0 を返す)。
		{"u1", "192.0.2.0/24", "2026-01-02"},
		// port / zone 付きは `inet` が読めないので残る (ipnorm は畳むという非対称。
		// migration の doc コメント参照)。
		{"u1", "1.2.3.4:5678", "2026-01-03"},
		{"u1", "fe80::1%eth0", "2026-01-04"},
		// 前後の空白は落とす。
		{"u2", "  192.0.2.7  ", "2026-01-05"},
	})

	applyUserIPMigration(t, db)

	assert.Equal(t, []postMigRow{
		{"u1", "1.2.3.4:5678", "2026-01-03", "2026-01-03", 1},
		{"u1", "192.0.2.0/24", "2026-01-02", "2026-01-02", 1},
		{"u1", "fe80::1%eth0", "2026-01-04", "2026-01-04", 1},
		{"u1", "not-an-ip", "2026-01-01", "2026-01-01", 1},
		{"u2", "192.0.2.7", "2026-01-05", "2026-01-05", 1},
	}, readPostMigration(t, db))
}

// 既存行の `lastSeenAt` は `createdAt` から埋まり、NOT NULL と DEFAULT が付く。
// **DEFAULT が無いと TS へ戻したときに INSERT が落ちる** — 純正はこの 2 列を知らないので
// `(createdAt, userId, ip)` だけを INSERT する。復路は保証しない (#3191) が、今戻せる
// 範囲として測っている。
func TestUserIPMigration_BackfillsAndKeepsTSInsertsWorking(t *testing.T) {
	db := ipMigrationDB(t)
	resetPreMigrationUserIP(t, db)
	seedPreMigrationRows(t, db, []preMigRow{{"u1", "192.0.2.1", "2026-01-01"}})

	applyUserIPMigration(t, db)

	rows := readPostMigration(t, db)
	require.Len(t, rows, 1)
	assert.Equal(t, "2026-01-01", rows[0].LastSeen, "lastSeenAt が createdAt から埋まっていない")
	assert.Equal(t, 1, rows[0].Count)

	// 純正が書く形の INSERT が通ること。
	require.NoError(t, db.Exec(
		`INSERT INTO "user_ip" ("createdAt", "userId", ip) VALUES (now(), 'ts', '198.51.100.1')`).Error)
	var nulls int64
	require.NoError(t, db.Raw(
		`SELECT count(*) FROM "user_ip" WHERE "lastSeenAt" IS NULL`).Scan(&nulls).Error)
	assert.Zero(t, nulls)

	var notNull bool
	require.NoError(t, db.Raw(`
		SELECT is_nullable = 'NO' FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'user_ip'
		  AND column_name = 'lastSeenAt'`).Scan(&notNull).Error)
	assert.True(t, notNull, "lastSeenAt が NOT NULL になっていない")
}

// **生き残るのは正規形の側 (= keeper)。** 統合の順序が非決定になると、どの行が
// 残るかが変わり、`ListByUser` の `id DESC` 上の位置 = `admin/get-user-ips` の並びが
// 実行ごとに変わる。
func TestUserIPMigration_KeepsTheCanonicalRow(t *testing.T) {
	db := ipMigrationDB(t)
	resetPreMigrationUserIP(t, db)
	seedPreMigrationRows(t, db, []preMigRow{
		{"u1", "::ffff:192.0.2.1", "2026-01-01"}, // id 1 (正規形ではない)
		{"u1", "192.0.2.1", "2026-02-01"},        // id 2 (正規形)
		{"u1", "::FFFF:192.0.2.1", "2026-03-01"}, // id 3
	})

	applyUserIPMigration(t, db)

	var ids []int64
	require.NoError(t, db.Raw(`SELECT id FROM "user_ip" ORDER BY id`).Scan(&ids).Error)
	require.Len(t, ids, 1)
	// **既に正規形だった行 (id 2) が残る。** id 1 は自分を rename する前に keeper
	// (id 2) を見つけるので、そちらへ合流して消える。id 3 も同じ keeper へ合流する。
	assert.EqualValues(t, 2, ids[0], "統合で生き残る行が変わっている")
}

// #3066 の中核クエリ用の index を作ること。
func TestUserIPMigration_CreatesLookupIndex(t *testing.T) {
	db := ipMigrationDB(t)
	resetPreMigrationUserIP(t, db)

	applyUserIPMigration(t, db)

	var n int64
	require.NoError(t, db.Raw(`
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'user_ip'
		  AND indexname = 'IDX_user_ip_ip_lastSeenAt'`).Scan(&n).Error)
	assert.EqualValues(t, 1, n)
}

// 2 回流しても結果が変わらない。golang-migrate は 1 度しか流さないが、
// 手で流し直す運用 (テストの兄弟 schema や復旧手順) で壊れないこと。
func TestUserIPMigration_IsIdempotent(t *testing.T) {
	db := ipMigrationDB(t)
	resetPreMigrationUserIP(t, db)
	seedPreMigrationRows(t, db, []preMigRow{
		{"u1", "192.0.2.1", "2026-01-01"},
		{"u1", "::ffff:192.0.2.1", "2026-02-01"},
	})

	applyUserIPMigration(t, db)
	once := readPostMigration(t, db)
	applyUserIPMigration(t, db)

	assert.Equal(t, once, readPostMigration(t, db))
}
