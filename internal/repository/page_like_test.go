package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPageLikeRepository_LifeCycle(t *testing.T) {
	pageRepo := NewPageRepository(testDB)
	repo := NewPageLikeRepository(testDB)
	user := insertTestUser(t, "u_plr_1", "pluser1")
	defer cleanupUser(t, user.ID)

	p := newTestPage("pg_pl_1", user.ID, "alpha")
	require.NoError(t, pageRepo.Create(p))
	defer cleanupPage(t, p.ID)

	pl := &model.PageLike{ID: "pl_pair_1", UserID: user.ID, PageID: p.ID}
	require.NoError(t, repo.Create(pl))
	defer testDB.Exec(`DELETE FROM "page_like" WHERE id = ?`, pl.ID)

	got, err := repo.FindByPair(user.ID, p.ID)
	require.NoError(t, err)
	assert.Equal(t, pl.ID, got.ID)

	exists, err := repo.Exists(user.ID, p.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	require.NoError(t, repo.Delete(pl))
	_, err = repo.FindByPair(user.ID, p.ID)
	assert.Error(t, err)
}

func TestPageLikeRepository_FindByPair_NotFound(t *testing.T) {
	repo := NewPageLikeRepository(testDB)
	_, err := repo.FindByPair("nope", "nope")
	assert.Error(t, err)
}

func TestPageLikeRepository_Exists_False(t *testing.T) {
	repo := NewPageLikeRepository(testDB)
	exists, err := repo.Exists("nope", "nope")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestPageLikeRepository_Exists_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewPageLikeRepository(db)
	_, err := repo.Exists("any", "any")
	assert.Error(t, err)
}

// #1773: ListLikedPageIDs は userID が like した pageID の部分集合を返す。
func TestPageLikeRepository_ListLikedPageIDs(t *testing.T) {
	pageRepo := NewPageRepository(testDB)
	repo := NewPageLikeRepository(testDB)
	user := insertTestUser(t, "u_pllp_1", "pllpuser1")
	defer cleanupUser(t, user.ID)

	p1 := newTestPage("pg_llp_1", user.ID, "alpha")
	p2 := newTestPage("pg_llp_2", user.ID, "beta")
	require.NoError(t, pageRepo.Create(p1))
	require.NoError(t, pageRepo.Create(p2))
	defer cleanupPage(t, p1.ID)
	defer cleanupPage(t, p2.ID)

	pl := &model.PageLike{ID: "pl_llp_1", UserID: user.ID, PageID: p1.ID}
	require.NoError(t, repo.Create(pl))
	defer testDB.Exec(`DELETE FROM "page_like" WHERE id = ?`, pl.ID)

	ids, err := repo.ListLikedPageIDs(user.ID, []string{p1.ID, p2.ID})
	require.NoError(t, err)
	assert.Equal(t, []string{p1.ID}, ids)

	// empty pageIDs は query せず nil を返す。
	ids, err = repo.ListLikedPageIDs(user.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, ids)
}

func TestPageLikeRepository_ListLikedPageIDs_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewPageLikeRepository(db)
	_, err := repo.ListLikedPageIDs("any", []string{"p1"})
	assert.Error(t, err)
}
