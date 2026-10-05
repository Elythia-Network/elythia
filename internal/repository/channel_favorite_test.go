package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelFavoriteRepository_CRUD(t *testing.T) {
	repo := NewChannelFavoriteRepository(testDB)
	seedUser(t, "chfav_u1")
	seedChannel(t, "chfav_c1", "chfav_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "channel_favorite" WHERE "userId" = ?`, "chfav_u1") })

	fav := &model.ChannelFavorite{ID: "chfav_1", UserID: "chfav_u1", ChannelID: "chfav_c1"}
	require.NoError(t, repo.Create(fav))

	ok, err := repo.Exists("chfav_u1", "chfav_c1")
	require.NoError(t, err)
	assert.True(t, ok)

	list, err := repo.ListByUser("chfav_u1")
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "chfav_1", list[0].ID)

	require.NoError(t, repo.Delete("chfav_u1", "chfav_c1"))
	ok, err = repo.Exists("chfav_u1", "chfav_c1")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestChannelFavoriteRepository_ListEmpty(t *testing.T) {
	repo := NewChannelFavoriteRepository(testDB)
	list, err := repo.ListByUser("chfav_nonexistent")
	require.NoError(t, err)
	assert.Empty(t, list)
}
