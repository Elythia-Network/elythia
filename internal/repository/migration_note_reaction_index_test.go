package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration132_NoteReactionUserIDIDIndex checks that 000132 creates a valid
// ("userId", id DESC) index on note_reaction and that down removes it (#3612).
func TestMigration132_NoteReactionUserIDIDIndex(t *testing.T) {
	db, m := openPerfIndexSchema(t, "migrate132")
	const name = "IDX_note_reaction_userId_id"

	require.NoError(t, runMigrate(func() error { return m.Migrate(132) }))
	state := perfIndexState(t, db)
	valid, ok := state[name]
	require.True(t, ok, "%s が無い", name)
	assert.True(t, valid, "%s が INVALID のまま", name)
	var def string
	require.NoError(t, db.Raw(`
		SELECT pg_get_indexdef(c.oid) FROM pg_class c
		WHERE c.relnamespace = to_regnamespace(current_schema()) AND c.relname = ?`, name).Scan(&def).Error)
	// users/reactions の `"userId" = ? ORDER BY id DESC` を索引の順で読めること。
	assert.Contains(t, def, `note_reaction USING btree ("userId", id DESC)`)

	require.NoError(t, runMigrate(func() error { return m.Migrate(131) }))
	assert.NotContains(t, perfIndexState(t, db), name, "down が %s を落としていない", name)
}
