package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Decrementing a note counter stops at 0 (#3291). Rows created before a
// counter was maintained sit below their real count, and going negative
// would leak into API responses and the remote-note cleanup condition.
func TestNoteRepository_IncrementCountNeverGoesNegative(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_incfloor", "incflooruser")
	defer cleanupUser(t, user.ID)
	insertTestNote(t, "n_incfloor", user.ID)

	read := func() int16 {
		var v int16
		require.NoError(t, testDB.Raw(`SELECT "clippedCount" FROM "note" WHERE id = ?`, "n_incfloor").Scan(&v).Error)
		return v
	}

	require.NoError(t, repo.IncrementCount("n_incfloor", "clippedCount", -1))
	assert.EqualValues(t, 0, read(), "0 から減らしても 0 のまま")

	require.NoError(t, repo.IncrementCount("n_incfloor", "clippedCount", 2))
	assert.EqualValues(t, 2, read())
	require.NoError(t, repo.IncrementCount("n_incfloor", "clippedCount", -1))
	assert.EqualValues(t, 1, read(), "0 より上では普通に減る")
}
