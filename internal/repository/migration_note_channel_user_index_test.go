package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration140_NoteChannelUserIDIndex checks that 000140 creates a valid
// partial ("channelId", "userId", id) index on note and that down removes it
// (#3369).
func TestMigration140_NoteChannelUserIDIndex(t *testing.T) {
	db, m := openPerfIndexSchema(t, "migrate140")
	const name = "IDX_note_channelId_userId_id"

	require.NoError(t, runMigrate(func() error { return m.Migrate(140) }))
	state := perfIndexState(t, db)
	valid, ok := state[name]
	require.True(t, ok, "%s が無い", name)
	assert.True(t, valid, "%s が INVALID のまま", name)
	var def string
	require.NoError(t, db.Raw(`
		SELECT pg_get_indexdef(c.oid) FROM pg_class c
		WHERE c.relnamespace = to_regnamespace(current_schema()) AND c.relname = ?`, name).Scan(&def).Error)
	// チャンネルの最初の投稿の判定 (`"channelId" = ? AND "userId" = ? AND id < ?`) を
	// 索引の範囲だけで読めること。channelId の無い行は入れない。
	assert.Contains(t, def, `note USING btree ("channelId", "userId", id) WHERE ("channelId" IS NOT NULL)`)

	require.NoError(t, runMigrate(func() error { return m.Migrate(139) }))
	assert.NotContains(t, perfIndexState(t, db), name, "down が %s を落としていない", name)
}
