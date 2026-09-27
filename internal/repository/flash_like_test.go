package repository

import (
	"context"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlashLikeRepository_LifeCycle(t *testing.T) {
	flashRepo := NewFlashRepository(testDB)
	repo := NewFlashLikeRepository(testDB)
	user := insertTestUser(t, "u_flr_1", "fluser1")
	defer cleanupUser(t, user.ID)

	f := newTestFlash("fl_fl_1", user.ID, "alpha")
	require.NoError(t, flashRepo.Create(f))
	defer cleanupFlash(t, f.ID)

	fl := &model.FlashLike{ID: "fll_pair_1", UserID: user.ID, FlashID: f.ID}
	require.NoError(t, repo.Create(fl))
	defer testDB.Exec(`DELETE FROM "flash_like" WHERE id = ?`, fl.ID)

	got, err := repo.FindByPair(user.ID, f.ID)
	require.NoError(t, err)
	assert.Equal(t, fl.ID, got.ID)

	exists, err := repo.Exists(user.ID, f.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	rows, err := repo.ListByUser(user.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	require.NoError(t, repo.Delete(fl))
	_, err = repo.FindByPair(user.ID, f.ID)
	assert.Error(t, err)
}

// #1548: ListByUserSearch joins flash and filters by title/summary ILIKE.
func TestFlashLikeRepository_ListByUserSearch(t *testing.T) {
	flashRepo := NewFlashRepository(testDB)
	repo := NewFlashLikeRepository(testDB)
	user := insertTestUser(t, "u_fls_1", "flsuser1")
	defer cleanupUser(t, user.ID)

	fa := newTestFlash("fl_s_a", user.ID, "golang tips")
	fb := newTestFlash("fl_s_b", user.ID, "ruby notes")
	require.NoError(t, flashRepo.Create(fa))
	require.NoError(t, flashRepo.Create(fb))
	defer cleanupFlash(t, fa.ID)
	defer cleanupFlash(t, fb.ID)

	la := &model.FlashLike{ID: "fll_s_a", UserID: user.ID, FlashID: fa.ID}
	lb := &model.FlashLike{ID: "fll_s_b", UserID: user.ID, FlashID: fb.ID}
	require.NoError(t, repo.Create(la))
	require.NoError(t, repo.Create(lb))
	defer testDB.Exec(`DELETE FROM "flash_like" WHERE id IN (?, ?)`, la.ID, lb.ID)

	// "golang" は title が golang の fa のみにマッチ。
	rows, err := repo.ListByUserSearch(user.ID, "golang", "", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, fa.ID, rows[0].FlashID)

	// summary 由来のマッチ (summary-ruby notes は "ruby" を含む)。
	rows, err = repo.ListByUserSearch(user.ID, "ruby", "", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, lb.ID, rows[0].ID)

	// 空 search は ListByUser 同等 (全件)。
	rows, err = repo.ListByUserSearch(user.ID, "", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	// 複数語は語間 AND (両方の語にマッチするものだけ)。
	rows, err = repo.ListByUserSearch(user.ID, "golang ruby", "", "", 10, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// #1548: ListLikedFlashIDs returns the subset of flashIDs liked by the user.
func TestFlashLikeRepository_ListLikedFlashIDs(t *testing.T) {
	flashRepo := NewFlashRepository(testDB)
	repo := NewFlashLikeRepository(testDB)
	user := insertTestUser(t, "u_fll_2", "flluser2")
	defer cleanupUser(t, user.ID)

	f := newTestFlash("fl_l_a", user.ID, "x")
	require.NoError(t, flashRepo.Create(f))
	defer cleanupFlash(t, f.ID)
	fl := &model.FlashLike{ID: "fll_l_a", UserID: user.ID, FlashID: f.ID}
	require.NoError(t, repo.Create(fl))
	defer testDB.Exec(`DELETE FROM "flash_like" WHERE id = ?`, fl.ID)

	ids, err := repo.ListLikedFlashIDs(user.ID, []string{f.ID, "other"})
	require.NoError(t, err)
	assert.Equal(t, []string{f.ID}, ids)

	// 空入力は空。
	ids, err = repo.ListLikedFlashIDs(user.ID, nil)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestFlashLikeRepository_FindByPair_NotFound(t *testing.T) {
	repo := NewFlashLikeRepository(testDB)
	_, err := repo.FindByPair("nope", "nope")
	assert.Error(t, err)
}

func TestFlashLikeRepository_Exists_False(t *testing.T) {
	repo := NewFlashLikeRepository(testDB)
	exists, err := repo.Exists("nope", "nope")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestFlashLikeRepository_Exists_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFlashLikeRepository(db)
	_, err := repo.Exists("any", "any")
	assert.Error(t, err)
}

func TestFlashLikeRepository_ListByUser_LimitClamp(t *testing.T) {
	repo := NewFlashLikeRepository(testDB)
	_, err := repo.ListByUser("nobody", "", "", 9999, 0)
	require.NoError(t, err)
	_, err = repo.ListByUser("nobody", "", "", -1, 0)
	require.NoError(t, err)
}

func TestFlashLikeRepository_ListByUser_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFlashLikeRepository(db)
	_, err := repo.ListByUser("nobody", "", "", 10, 0)
	assert.Error(t, err)
}

// **like した後に非公開化された Flash を一覧に出さないこと。**
//
// flash/my-likes は Flash を script ごと返すので、ここで絞らないと
// flash/show の可視性判定 (非公開は所有者のみ) を迂回して読める。
func TestFlashLikeRepository_ListByUser_ExcludesPrivateFlashOfOthers(t *testing.T) {
	flashRepo := NewFlashRepository(testDB)
	repo := NewFlashLikeRepository(testDB)
	owner := insertTestUser(t, "u_flp_own", "flpowner")
	defer cleanupUser(t, owner.ID)
	liker := insertTestUser(t, "u_flp_lik", "flpliker")
	defer cleanupUser(t, liker.ID)

	pub := newTestFlash("fl_p_pub", owner.ID, "shared topic public")
	priv := newTestFlash("fl_p_priv", owner.ID, "shared topic private")
	priv.Visibility = "private"
	self := newTestFlash("fl_p_self", liker.ID, "shared topic mine")
	self.Visibility = "private"
	for _, f := range []*model.Flash{pub, priv, self} {
		require.NoError(t, flashRepo.Create(f))
		defer cleanupFlash(t, f.ID)
	}
	likes := []*model.FlashLike{
		{ID: "fll_p_pub", UserID: liker.ID, FlashID: pub.ID},
		{ID: "fll_p_priv", UserID: liker.ID, FlashID: priv.ID},
		// 自分の Flash は like できないが、行があっても所有者には見えてよい。
		{ID: "fll_p_self", UserID: liker.ID, FlashID: self.ID},
	}
	for _, l := range likes {
		require.NoError(t, repo.Create(l))
	}
	defer testDB.Exec(`DELETE FROM "flash_like" WHERE id IN (?, ?, ?)`, likes[0].ID, likes[1].ID, likes[2].ID)

	flashIDs := func(rows []*model.FlashLike) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.FlashID)
		}
		return out
	}

	rows, err := repo.ListByUser(liker.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{pub.ID, self.ID}, flashIDs(rows))

	rows, err = repo.ListByUserSearch(liker.ID, "shared", "", "", 10, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{pub.ID, self.ID}, flashIDs(rows))

	// 所有者自身が like 行を持つ場合は非公開でも出る。
	require.NoError(t, repo.Create(&model.FlashLike{ID: "fll_p_own", UserID: owner.ID, FlashID: priv.ID}))
	defer testDB.Exec(`DELETE FROM "flash_like" WHERE id = ?`, "fll_p_own")
	rows, err = repo.ListByUser(owner.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{priv.ID}, flashIDs(rows))
}
