package repository

import (
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/testutil"
)

// perfIndexes are the indexes 000123-000131 create on an Elythia-born DB.
var perfIndexes = []string{
	"IDX_note_channelId_id_desc",
	"IDX_NOTE_VISIBLE_USER_IDS",
	"IDX_user_isSuspended_id",
	"IDX_user_avatarId",
	"IDX_user_bannerId",
	"IDX_e637cba4dc4410218c4251260e",
	"IDX_antenna_note_unread_noteId",
	"IDX_channel_note_unread_noteId",
	"IDX_promo_read_noteId",
	"IDX_following_followeeId_notify_normal",
}

// perfIndexState returns index name -> indisvalid for every index in the
// handle's schema.
func perfIndexState(t *testing.T, db *gorm.DB) map[string]bool {
	t.Helper()
	var rows []struct {
		Relname    string
		Indisvalid bool
	}
	require.NoError(t, db.Raw(`
		SELECT c.relname, i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relnamespace = to_regnamespace(current_schema())`).Scan(&rows).Error)
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.Relname] = r.Indisvalid
	}
	return out
}

// indexesLeadingWith counts the indexes on table whose first key column is col.
func indexesLeadingWith(t *testing.T, db *gorm.DB, table, col string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Raw(`
		SELECT count(*) FROM pg_index x
		JOIN pg_class tb ON tb.oid = x.indrelid
		JOIN pg_attribute a ON a.attrelid = tb.oid AND a.attnum = x.indkey[0]
		WHERE tb.relnamespace = to_regnamespace(current_schema())
		  AND tb.relname = ? AND a.attname = ?`, table, col).Scan(&n).Error)
	return n
}

// openPerfIndexSchema recreates a dedicated schema and returns a handle and a
// migrate instance pointed at it.
func openPerfIndexSchema(t *testing.T, suffix string) (*gorm.DB, *migrate.Migrate) {
	t.Helper()
	db, err := testutil.OpenTestDBSchema(suffix)
	require.NoError(t, err, "専用 schema を開けない")
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	// 往復テストと同じく、名前を名指しで要求してから DROP する (public を落とさない)。
	require.Equal(t, "internal_repository_"+suffix, schema)
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	m, err := migrate.New("file://../../migration", migrateURL(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		serr, derr := m.Close()
		assert.NoError(t, serr)
		assert.NoError(t, derr)
	})
	return db, m
}

// TestMigration123to131_PerfIndexes_ElythiaBorn checks that a DB created by
// Elythia gets every index, all valid, and that down removes them again.
func TestMigration123to131_PerfIndexes_ElythiaBorn(t *testing.T) {
	db, m := openPerfIndexSchema(t, "migrate123")

	require.NoError(t, runMigrate(func() error { return m.Migrate(131) }))
	state := perfIndexState(t, db)
	for _, name := range perfIndexes {
		valid, ok := state[name]
		if assert.True(t, ok, "%s が無い", name) {
			assert.True(t, valid, "%s が INVALID のまま", name)
		}
	}

	require.NoError(t, runMigrate(func() error { return m.Migrate(122) }))
	state = perfIndexState(t, db)
	for _, name := range perfIndexes {
		assert.NotContains(t, state, name, "down が %s を落としていない", name)
	}
}

// TestMigration123to131_PerfIndexes_TSBorn checks a DB carried over from
// Misskey TS: indexes upstream already has (same name or, for avatarId /
// bannerId, the OneToOne unique constraint) must not be duplicated.
//
// 本物の TS 版の DB の代わりに、122 まで当てた schema に本家の index を本家の名前で
// 足して再現する (本家の定義は testdata/golden_upstream_indexes.json と同じ)。
func TestMigration123to131_PerfIndexes_TSBorn(t *testing.T) {
	db, m := openPerfIndexSchema(t, "migrate123ts")

	require.NoError(t, runMigrate(func() error { return m.Migrate(122) }))
	for _, stmt := range []string{
		`CREATE UNIQUE INDEX "REL_58f5c71eaab331645112cf8cfa" ON "user" ("avatarId")`,
		`CREATE UNIQUE INDEX "REL_afc64b53f8db3707ceb34eb28e" ON "user" ("bannerId")`,
		`CREATE INDEX "IDX_NOTE_VISIBLE_USER_IDS" ON "note" USING gin ("visibleUserIds")`,
		`CREATE INDEX "IDX_e637cba4dc4410218c4251260e" ON "note_unread" ("noteId")`,
	} {
		require.NoError(t, db.Exec(stmt).Error, stmt)
	}

	require.NoError(t, runMigrate(func() error { return m.Migrate(131) }))
	state := perfIndexState(t, db)
	assert.NotContains(t, state, "IDX_user_avatarId", "本家の一意制約があるときは作らない")
	assert.NotContains(t, state, "IDX_user_bannerId", "本家の一意制約があるときは作らない")
	assert.EqualValues(t, 1, indexesLeadingWith(t, db, "user", "avatarId"))
	assert.EqualValues(t, 1, indexesLeadingWith(t, db, "user", "bannerId"))
	assert.EqualValues(t, 1, indexesLeadingWith(t, db, "note", "visibleUserIds"))
	assert.EqualValues(t, 1, indexesLeadingWith(t, db, "note_unread", "noteId"))
	for _, name := range []string{"IDX_note_channelId_id_desc", "IDX_user_isSuspended_id", "IDX_following_followeeId_notify_normal"} {
		assert.True(t, state[name], "%s が無いか INVALID", name)
	}

	// 000126 の down は本家の一意制約に触れない。
	require.NoError(t, runMigrate(func() error { return m.Migrate(125) }))
	state = perfIndexState(t, db)
	assert.Contains(t, state, "REL_58f5c71eaab331645112cf8cfa")
	assert.Contains(t, state, "REL_afc64b53f8db3707ceb34eb28e")
}
