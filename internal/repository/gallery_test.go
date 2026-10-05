package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGalleryRepository_ListByUser(t *testing.T) {
	repo := NewGalleryRepository(testDB)
	seedUser(t, "gp_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "gallery_post" WHERE "userId" = ?`, "gp_u1") })

	for _, id := range []string{"gp_1", "gp_2", "gp_3"} {
		require.NoError(t, testDB.Create(&model.GalleryPost{
			ID: id, UserID: "gp_u1", Title: "t_" + id, UpdatedAt: time.Now(),
		}).Error)
	}

	posts, err := repo.ListByUser("gp_u1", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, posts, 3)

	// pagination: offset > 0
	posts, err = repo.ListByUser("gp_u1", "", "", 10, 1)
	require.NoError(t, err)
	assert.Len(t, posts, 2)

	// clampLimit: 0 -> 10 (but only 3 rows so limit is not the limiting factor)
	posts, err = repo.ListByUser("gp_u1", "", "", 0, 0)
	require.NoError(t, err)
	assert.Len(t, posts, 3)

	// clampLimit: >100 -> 100
	posts, err = repo.ListByUser("gp_u1", "", "", 9999, 0)
	require.NoError(t, err)
	assert.Len(t, posts, 3)
}

func TestGalleryRepository_ListLikesByUser(t *testing.T) {
	repo := NewGalleryRepository(testDB)
	seedUser(t, "gl_u1")
	// postはlikeのFKなので先に作る
	require.NoError(t, testDB.Create(&model.GalleryPost{ID: "gl_p1", UserID: "gl_u1", Title: "t", UpdatedAt: time.Now()}).Error)
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "gallery_like" WHERE "userId" = ?`, "gl_u1")
		testDB.Exec(`DELETE FROM "gallery_post" WHERE id = ?`, "gl_p1")
	})

	require.NoError(t, testDB.Create(&model.GalleryLike{ID: "gl_l1", UserID: "gl_u1", PostID: "gl_p1"}).Error)

	likes, err := repo.ListLikesByUser("gl_u1", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, likes, 1)

	// offset > 0
	likes, err = repo.ListLikesByUser("gl_u1", "", "", 10, 1)
	require.NoError(t, err)
	assert.Empty(t, likes)
}

func TestGalleryRepository_EmptyList(t *testing.T) {
	repo := NewGalleryRepository(testDB)
	posts, err := repo.ListByUser("gp_nonexistent", "", "", 10, 0)
	require.NoError(t, err)
	assert.Empty(t, posts)

	likes, err := repo.ListLikesByUser("gp_nonexistent", "", "", 10, 0)
	require.NoError(t, err)
	assert.Empty(t, likes)
}

func TestGalleryRepository_ExistsLike(t *testing.T) {
	repo := NewGalleryRepository(testDB)
	seedUser(t, "gex_u1")
	require.NoError(t, testDB.Create(&model.GalleryPost{ID: "gex_p1", UserID: "gex_u1", Title: "t", UpdatedAt: time.Now()}).Error)
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "gallery_like" WHERE "userId" = ?`, "gex_u1")
		testDB.Exec(`DELETE FROM "gallery_post" WHERE id = ?`, "gex_p1")
	})

	// like 未作成 → false
	ok, err := repo.ExistsLike("gex_u1", "gex_p1")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, testDB.Create(&model.GalleryLike{ID: "gex_l1", UserID: "gex_u1", PostID: "gex_p1"}).Error)

	// like 済 → true
	ok, err = repo.ExistsLike("gex_u1", "gex_p1")
	require.NoError(t, err)
	assert.True(t, ok)

	// 別 user / 別 post は false
	ok, err = repo.ExistsLike("gex_other", "gex_p1")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGalleryRepository_FindPostsByIDs(t *testing.T) {
	repo := NewGalleryRepository(testDB)
	seedUser(t, "gf_u1")
	require.NoError(t, testDB.Create(&model.GalleryPost{ID: "gf_p1", UserID: "gf_u1", Title: "t1", UpdatedAt: time.Now()}).Error)
	require.NoError(t, testDB.Create(&model.GalleryPost{ID: "gf_p2", UserID: "gf_u1", Title: "t2", UpdatedAt: time.Now()}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "gallery_post" WHERE "userId" = ?`, "gf_u1") })

	// 空 ids は nil を返す (早期 return)。
	posts, err := repo.FindPostsByIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, posts)

	posts, err = repo.FindPostsByIDs([]string{"gf_p1", "gf_p2", "gf_missing"})
	require.NoError(t, err)
	assert.Len(t, posts, 2)
}
