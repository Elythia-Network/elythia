package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteThreadMutingRepository_CRUD(t *testing.T) {
	repo := NewNoteThreadMutingRepository(testDB)
	seedUser(t, "ntm_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note_thread_muting" WHERE "userId" = ?`, "ntm_u1") })

	m := &model.NoteThreadMuting{ID: "ntm_1", UserID: "ntm_u1", ThreadID: "thread_1"}
	require.NoError(t, repo.Create(m))

	ok, err := repo.Exists("ntm_u1", "thread_1")
	require.NoError(t, err)
	assert.True(t, ok)

	require.NoError(t, repo.Delete("ntm_u1", "thread_1"))
	ok, err = repo.Exists("ntm_u1", "thread_1")
	require.NoError(t, err)
	assert.False(t, ok)
}

// #1554 ListMutedThreadIDs は user の muted threadId のみ返す。
func TestNoteThreadMutingRepository_ListMutedThreadIDs(t *testing.T) {
	repo := NewNoteThreadMutingRepository(testDB)
	seedUser(t, "ntml_u1")
	seedUser(t, "ntml_u2")
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "note_thread_muting" WHERE "userId" IN (?, ?)`, "ntml_u1", "ntml_u2")
	})
	require.NoError(t, repo.Create(&model.NoteThreadMuting{ID: "ntml_1", UserID: "ntml_u1", ThreadID: "t1"}))
	require.NoError(t, repo.Create(&model.NoteThreadMuting{ID: "ntml_2", UserID: "ntml_u1", ThreadID: "t2"}))
	require.NoError(t, repo.Create(&model.NoteThreadMuting{ID: "ntml_3", UserID: "ntml_u2", ThreadID: "t3"}))

	ids, err := repo.ListMutedThreadIDs("ntml_u1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"t1", "t2"}, ids)

	// 空 userID は nil。
	ids, err = repo.ListMutedThreadIDs("")
	require.NoError(t, err)
	assert.Nil(t, ids)
}
