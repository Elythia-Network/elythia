package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clip_favoriteはclip行へのFKを持つため、最小clipを先に投入する。
func seedClip(t *testing.T, clipID, ownerUserID string) {
	t.Helper()
	require.NoError(t, testDB.Exec(
		`INSERT INTO "clip" (id, "userId", name, "isPublic") VALUES (?, ?, ?, true) ON CONFLICT (id) DO NOTHING`,
		clipID, ownerUserID, "cl_"+clipID,
	).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "clip" WHERE id = ?`, clipID) })
}

func TestClipFavoriteRepository_CRUD(t *testing.T) {
	repo := NewClipFavoriteRepository(testDB)
	seedUser(t, "clfav_u1")
	seedClip(t, "clfav_cl1", "clfav_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "clip_favorite" WHERE "userId" = ?`, "clfav_u1") })

	fav := &model.ClipFavorite{ID: "clfav_1", UserID: "clfav_u1", ClipID: "clfav_cl1"}
	require.NoError(t, repo.Create(fav))

	ok, err := repo.Exists("clfav_u1", "clfav_cl1")
	require.NoError(t, err)
	assert.True(t, ok)

	list, err := repo.ListByUser("clfav_u1")
	require.NoError(t, err)
	assert.Len(t, list, 1)

	require.NoError(t, repo.Delete("clfav_u1", "clfav_cl1"))
	ok, err = repo.Exists("clfav_u1", "clfav_cl1")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestClipFavoriteRepository_ListEmpty(t *testing.T) {
	repo := NewClipFavoriteRepository(testDB)
	list, err := repo.ListByUser("clfav_nonexistent")
	require.NoError(t, err)
	assert.Empty(t, list)
}
