package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteFavoriteRepository_CRUD(t *testing.T) {
	repo := NewNoteFavoriteRepository(testDB)
	user := insertTestUser(t, "fav_u1", "favuser")
	defer cleanupUser(t, user.ID)

	note := &model.Note{ID: "fav_n1", UserID: user.ID, Visibility: "public"}
	require.NoError(t, testDB.Create(note).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID)

	f := &model.NoteFavorite{ID: "fav_1", UserID: user.ID, NoteID: note.ID}
	require.NoError(t, repo.Create(f))
	defer testDB.Exec(`DELETE FROM "note_favorite" WHERE id = ?`, f.ID)

	exists, err := repo.Exists(user.ID, note.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	favs, err := repo.ListByUser(user.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, favs, 1)

	// default limit
	favs, err = repo.ListByUser(user.ID, "", "", 0)
	require.NoError(t, err)
	assert.NotEmpty(t, favs)

	// limit cap
	favs, err = repo.ListByUser(user.ID, "", "", 999)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(favs), 100)

	// untilID で先頭を含めない範囲を要求すると空が返ること
	favs, err = repo.ListByUser(user.ID, "fav_0", "", 10)
	require.NoError(t, err)
	assert.Empty(t, favs)

	require.NoError(t, repo.Delete(user.ID, note.ID))
	exists, _ = repo.Exists(user.ID, note.ID)
	assert.False(t, exists)
}

// 複数 favorite を作って untilID / sinceID cursor が想定通りの DESC keyset
// 切り出しを返すことを検証する。これが #424 で抜けていた pagination 経路。
func TestNoteFavoriteRepository_ListByUser_KeysetPagination(t *testing.T) {
	repo := NewNoteFavoriteRepository(testDB)
	user := insertTestUser(t, "fav_pg_u", "favpg")
	defer cleanupUser(t, user.ID)

	for i, fid := range []string{"fav_pg_a", "fav_pg_b", "fav_pg_c"} {
		nid := "fav_pg_n_" + string(rune('a'+i))
		note := &model.Note{ID: nid, UserID: user.ID, Visibility: "public"}
		require.NoError(t, testDB.Create(note).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, nid)
		require.NoError(t, repo.Create(&model.NoteFavorite{ID: fid, UserID: user.ID, NoteID: nid}))
		defer testDB.Exec(`DELETE FROM "note_favorite" WHERE id = ?`, fid)
	}

	// no cursor → DESC 全件
	page, err := repo.ListByUser(user.ID, "", "", 10)
	require.NoError(t, err)
	require.Len(t, page, 3)
	assert.Equal(t, "fav_pg_c", page[0].ID)
	assert.Equal(t, "fav_pg_a", page[2].ID)

	// untilID = c → c より小さい id を DESC で返すので [b, a]
	page, err = repo.ListByUser(user.ID, "fav_pg_c", "", 10)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "fav_pg_b", page[0].ID)
	assert.Equal(t, "fav_pg_a", page[1].ID)

	// sinceID = a (untilID 無し) → ASC で [b, c]
	page, err = repo.ListByUser(user.ID, "", "fav_pg_a", 10)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "fav_pg_b", page[0].ID)
	assert.Equal(t, "fav_pg_c", page[1].ID)

	// limit が効くこと
	page, err = repo.ListByUser(user.ID, "", "", 2)
	require.NoError(t, err)
	assert.Len(t, page, 2)
}

func TestNoteFavoriteRepository_Exists_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteFavoriteRepository(testDB.WithContext(ctx))
	_, err := repo.Exists("x", "y")
	assert.Error(t, err)
}

func TestNoteFavoriteRepository_ListByUser_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteFavoriteRepository(testDB.WithContext(ctx))
	_, err := repo.ListByUser("x", "", "", 10)
	assert.Error(t, err)
}
