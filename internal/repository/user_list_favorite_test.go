package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedUserList は user_list_favorite の FK 制約を満たすためのヘルパー。
func seedUserList(t *testing.T, listID, ownerUserID string) {
	t.Helper()
	require.NoError(t, testDB.Exec(
		`INSERT INTO "user_list" (id, "userId", name, "isPublic") VALUES (?, ?, ?, true) ON CONFLICT (id) DO NOTHING`,
		listID, ownerUserID, "ul_"+listID,
	).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listID) })
}

func TestUserListFavoriteRepository_CRUD(t *testing.T) {
	repo := NewUserListFavoriteRepository(testDB)
	seedUser(t, "ulfav_u1")
	seedUserList(t, "ulfav_l1", "ulfav_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user_list_favorite" WHERE "userId" = ?`, "ulfav_u1") })

	fav := &model.UserListFavorite{ID: "ulfav_1", UserID: "ulfav_u1", UserListID: "ulfav_l1"}
	require.NoError(t, repo.Create(fav))

	ok, err := repo.Exists("ulfav_u1", "ulfav_l1")
	require.NoError(t, err)
	assert.True(t, ok)

	list, err := repo.ListByUser("ulfav_u1")
	require.NoError(t, err)
	assert.Len(t, list, 1)

	require.NoError(t, repo.Delete("ulfav_u1", "ulfav_l1"))
	ok, err = repo.Exists("ulfav_u1", "ulfav_l1")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestUserListFavoriteRepository_ListEmpty(t *testing.T) {
	repo := NewUserListFavoriteRepository(testDB)
	list, err := repo.ListByUser("ulfav_nonexistent")
	require.NoError(t, err)
	assert.Empty(t, list)
}

// #1550: CountByList は list の favorite 数を返す (show forPublic の likedCount)。
func TestUserListFavoriteRepository_CountByList(t *testing.T) {
	repo := NewUserListFavoriteRepository(testDB)
	seedUser(t, "ulc_u1")
	seedUser(t, "ulc_u2")
	seedUserList(t, "ulc_l1", "ulc_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user_list_favorite" WHERE "userListId" = ?`, "ulc_l1") })

	n, err := repo.CountByList("ulc_l1")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	require.NoError(t, repo.Create(&model.UserListFavorite{ID: "ulc_f1", UserID: "ulc_u1", UserListID: "ulc_l1"}))
	require.NoError(t, repo.Create(&model.UserListFavorite{ID: "ulc_f2", UserID: "ulc_u2", UserListID: "ulc_l1"}))
	n, err = repo.CountByList("ulc_l1")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}
