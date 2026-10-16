package repository

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/testutil"
)

const migration068 = "000068_drop_redundant_indexes.up.sql"

// migration068ForSchema returns 000068 with its hard-coded `public` pointed at
// schema.
//
// 000068 は本番の schema (`public`) を名指しで見る。テストの DB では `public` に表が
// 無いので、そのまま流すと何もせずに通ってしまう。名指しの箇所だけを差し替えて、
// 専用の schema に対して判定させる。置き換えが 0 件なら差し替えが空振りしている
// (書式が変わった) ので落とす。
func migration068ForSchema(t *testing.T, schema string) string {
	t.Helper()
	sql := readMigration(t, migration068)
	require.Positive(t, strings.Count(sql, "schemaname = 'public'"), "schema の絞りが見つからない")
	require.Equal(t, 1, strings.Count(sql, "public.%I"), "DROP の schema が見つからない")
	sql = strings.ReplaceAll(sql, "schemaname = 'public'", "schemaname = '"+schema+"'")
	return strings.ReplaceAll(sql, "public.%I", `"`+schema+`".%I`)
}

// migration068Names returns the mk-go index names 000068 may drop.
func migration068Names(t *testing.T) map[string]bool {
	t.Helper()
	sql := readMigration(t, migration068)
	m := regexp.MustCompile(`(?s)mk_index_names text\[\] := ARRAY\[(.*?)\];`).FindStringSubmatch(sql)
	require.NotNil(t, m, "mk_index_names が見つからない")
	out := map[string]bool{}
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
		out[q[1]] = true
	}
	// 2026-10-16 時点で 195 本。抽出が空振りしていないことの下限。
	require.Greater(t, len(out), 150, "mk_index_names の抽出が少なすぎる")
	return out
}

// indexDefsInSchema returns index name -> definition for the handle's schema.
func indexDefsInSchema(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	// 定義は SELECT で読み、絞り込みは schema だけにする (#3365)。
	var rows []struct {
		Indexname string
		Indexdef  string
	}
	require.NoError(t, db.Raw(`
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = current_schema()`).Scan(&rows).Error)
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Indexname] = r.Indexdef
	}
	return out
}

// recreateSchema drops and recreates a dedicated sibling schema.
func recreateSchema(t *testing.T, suffix string) (*gorm.DB, string) {
	t.Helper()
	db, err := testutil.OpenTestDBSchema(suffix)
	require.NoError(t, err, "専用 schema を開けない")
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	// 名前を名指しで要求してから DROP する (public を落とさない)。
	require.Equal(t, "internal_repository_"+suffix, schema)
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	return db, schema
}

// droppedIndexes returns the names in before that are missing from after.
func droppedIndexes(before, after map[string]string) []string {
	var out []string
	for name := range before {
		if _, ok := after[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// TestMigration068_DropsOnlyUpstreamTwins pins which indexes 000068 drops for
// each kind of input: an exact upstream twin, a listed partial index whose
// upstream twin is full, and the cases it must leave alone (#2246, #3369).
//
// #3369 で、定義を EXISTS の中の WHERE で比べる形から、配列に読み切ってから比べる
// 形に書き換えた。落とす対象が変わっていないことをここで固定する (書き換える前の
// SQL でも同じ結果になることを確かめてある)。
func TestMigration068_DropsOnlyUpstreamTwins(t *testing.T) {
	db, schema := recreateSchema(t, "migrate068")
	decoy, decoySchema := recreateSchema(t, "migrate068d")

	// 同じ形の表を 2 つの schema に作る。別の schema の index は判定にも DROP にも
	// 入らないこと (schema の絞り) を、decoy の側で見る。
	tables := []string{
		`CREATE TABLE following ("followerId" text, "followeeId" text)`,
		`CREATE TABLE note (uri text, "userId" text)`,
		`CREATE TABLE note_unread ("userId" text, "isMentioned" boolean)`,
		`CREATE TABLE drive_file (uri text)`,
		`CREATE TABLE "user" ("usernameLower" text, host text)`,
		`CREATE TABLE muting ("muterId" text)`,
	}
	for _, h := range []*gorm.DB{db, decoy} {
		for _, stmt := range tables {
			require.NoError(t, h.Exec(stmt).Error)
		}
	}
	for _, stmt := range []string{
		// (A) 定義が完全一致 → mk-go 側を落とす
		`CREATE INDEX "IDX_following_followerId" ON following ("followerId")`,
		`CREATE INDEX "IDX_up_following_follower" ON following ("followerId")`,
		// 対になる upstream の index が無い → 残す
		`CREATE INDEX "IDX_following_followeeId" ON following ("followeeId")`,
		// UNIQUE かどうかが違う → 残す
		`CREATE UNIQUE INDEX "IDX_following_followerId_followeeId" ON following ("followerId", "followeeId")`,
		`CREATE INDEX "IDX_up_following_pair" ON following ("followerId", "followeeId")`,
		// (B) 明示列挙した partial で、WHERE を外すと upstream の full と一致 → 落とす
		`CREATE UNIQUE INDEX "IDX_note_uri" ON note (uri) WHERE uri IS NOT NULL`,
		`CREATE UNIQUE INDEX "IDX_up_note_uri" ON note (uri)`,
		// mk-go 同士で定義が一致 → どちらも残す
		`CREATE INDEX "IDX_note_userId" ON note ("userId")`,
		`CREATE INDEX "IDX_note_userHost" ON note ("userId")`,
		// 明示列挙していない partial は、upstream の full があっても残す
		`CREATE INDEX "IDX_note_unread_userId_isMentioned" ON note_unread ("userId") WHERE "isMentioned"`,
		`CREATE INDEX "IDX_up_note_unread_user" ON note_unread ("userId")`,
		// (A) partial 同士で WHERE まで一致 → 落とす
		`CREATE INDEX "IDX_drive_file_uri" ON drive_file (uri) WHERE uri IS NOT NULL`,
		`CREATE INDEX "IDX_up_drive_file_uri" ON drive_file (uri) WHERE uri IS NOT NULL`,
		// 明示列挙した partial でも、upstream の側が別の WHERE を持つ → 残す
		`CREATE UNIQUE INDEX "IDX_user_usernameLower_host_unique" ON "user" ("usernameLower", host) WHERE host IS NOT NULL`,
		`CREATE UNIQUE INDEX "IDX_up_user_lower_host" ON "user" ("usernameLower", host) WHERE "usernameLower" IS NOT NULL`,
		// upstream 同士の重複 → upstream の index は落とさない
		`CREATE INDEX "IDX_up_muting_a" ON muting ("muterId")`,
		`CREATE INDEX "IDX_up_muting_b" ON muting ("muterId")`,
	} {
		require.NoError(t, db.Exec(stmt).Error)
		require.NoError(t, decoy.Exec(stmt).Error)
	}
	// decoy にだけ対になる upstream の index を置く。schema で絞らずに判定すると、
	// 同じ名前の index を対象の schema から落としてしまう。
	require.NoError(t, decoy.Exec(`CREATE INDEX "IDX_up_following_followee" ON following ("followeeId")`).Error)

	before := indexDefsInSchema(t, db)
	decoyBefore := indexDefsInSchema(t, decoy)
	require.Len(t, before, 17)

	require.NoError(t, db.Exec(migration068ForSchema(t, schema)).Error)

	assert.Equal(t, []string{"IDX_drive_file_uri", "IDX_following_followerId", "IDX_note_uri"},
		droppedIndexes(before, indexDefsInSchema(t, db)))
	assert.Equal(t, decoyBefore, indexDefsInSchema(t, decoy),
		"%s の index を見て %s を判定・DROP している", decoySchema, schema)

	// 2 回目は何も落とさない (golang-migrate が再実行することは無いが、
	// testutil.ApplyMigrations は書き換えると流し直す)。
	again := indexDefsInSchema(t, db)
	require.NoError(t, db.Exec(migration068ForSchema(t, schema)).Error)
	assert.Equal(t, again, indexDefsInSchema(t, db))
}

// TestMigration068_TSBornSchema runs 000068 on a schema migrated to 000067 by
// golang-migrate, after adding an upstream-named twin for every mk-go index
// 000068 lists, as a Misskey TS-born DB would have. Every listed index that
// exists must be dropped, and no upstream twin.
func TestMigration068_TSBornSchema(t *testing.T) {
	db, m := openPerfIndexSchema(t, "migrate068ts")
	require.NoError(t, runMigrate(func() error { return m.Migrate(67) }))
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)

	names := migration068Names(t)
	nameRe := regexp.MustCompile(`^CREATE (UNIQUE )?INDEX \S+ ON `)
	want := []string{}
	twins := map[string]bool{}
	for name, def := range indexDefsInSchema(t, db) {
		if !names[name] {
			continue
		}
		want = append(want, name)
		// upstream (TypeORM) の hash 名に相当する、mk-go の一覧に無い名前で同じ定義を作る。
		twin := fmt.Sprintf("IDX_twin_%03d", len(twins))
		twins[twin] = true
		stmt := nameRe.ReplaceAllString(def, "CREATE ${1}INDEX \""+twin+"\" ON ")
		require.NoError(t, db.Exec(stmt).Error, stmt)
	}
	sort.Strings(want)
	// Elythia の 000067 までが作る、一覧に載った index の本数。抽出の下限。
	require.Greater(t, len(want), 150)

	before := indexDefsInSchema(t, db)
	require.NoError(t, db.Exec(migration068ForSchema(t, schema)).Error)
	after := indexDefsInSchema(t, db)

	assert.Equal(t, want, droppedIndexes(before, after))
	for twin := range twins {
		assert.Contains(t, after, twin, "upstream 側の %s を落としている", twin)
	}
}
