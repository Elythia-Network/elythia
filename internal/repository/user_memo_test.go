package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupMemo(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "user_memo" WHERE id = ?`, id)
}

func TestUserMemoRepository_CreateOrUpdate(t *testing.T) {
	repo := NewUserMemoRepository(testDB)
	u1 := insertTestUser(t, "memo_u1", "mu1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "memo_u2", "mu2")
	defer cleanupUser(t, u2.ID)

	memo := &model.UserMemo{ID: "m1", UserID: u1.ID, TargetUserID: u2.ID, Memo: "hello"}
	require.NoError(t, repo.CreateOrUpdate(memo))
	defer cleanupMemo(t, memo.ID)

	got, err := repo.FindByPair(u1.ID, u2.ID)
	require.NoError(t, err)
	assert.Equal(t, "hello", got.Memo)

	// upsert (same pair, different memo)
	memo2 := &model.UserMemo{ID: "m2", UserID: u1.ID, TargetUserID: u2.ID, Memo: "updated"}
	require.NoError(t, repo.CreateOrUpdate(memo2))
	defer cleanupMemo(t, memo2.ID)

	got, _ = repo.FindByPair(u1.ID, u2.ID)
	assert.Equal(t, "updated", got.Memo)
}

func TestUserMemoRepository_FindByPair_NotFound(t *testing.T) {
	repo := NewUserMemoRepository(testDB)
	_, err := repo.FindByPair("ghost", "ghost2")
	assert.Error(t, err)
}

func TestUserMemoRepository_Delete(t *testing.T) {
	repo := NewUserMemoRepository(testDB)
	u1 := insertTestUser(t, "memod_u1", "md1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "memod_u2", "md2")
	defer cleanupUser(t, u2.ID)

	memo := &model.UserMemo{ID: "md1", UserID: u1.ID, TargetUserID: u2.ID, Memo: "bye"}
	require.NoError(t, repo.CreateOrUpdate(memo))
	defer cleanupMemo(t, memo.ID)

	require.NoError(t, repo.Delete(u1.ID, u2.ID))
	_, err := repo.FindByPair(u1.ID, u2.ID)
	assert.Error(t, err)
}
