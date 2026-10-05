package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupFlash(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "flash_like" WHERE "flashId" = ?`, id)
	testDB.Exec(`DELETE FROM "flash" WHERE id = ?`, id)
}

func newTestFlash(id, ownerID, title string) *model.Flash {
	return &model.Flash{
		ID:          id,
		UpdatedAt:   time.Now(),
		Title:       title,
		Summary:     "summary-" + title,
		UserID:      ownerID,
		Script:      "<:0>",
		Permissions: model.StringArray{},
		Visibility:  "public",
	}
}

func TestFlashRepository_CreateAndFindByID(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_1", "flashuser1")
	defer cleanupUser(t, user.ID)

	f := newTestFlash("fl_cr_1", user.ID, "alpha")
	require.NoError(t, repo.Create(f))
	defer cleanupFlash(t, f.ID)

	got, err := repo.FindByID(f.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", got.Title)
}

func TestFlashRepository_FindByID_NotFound(t *testing.T) {
	repo := NewFlashRepository(testDB)
	_, err := repo.FindByID("missing")
	assert.Error(t, err)
}

func TestFlashRepository_UpdateFields(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_2", "flashuser2")
	defer cleanupUser(t, user.ID)

	f := newTestFlash("fl_cr_2", user.ID, "beta")
	require.NoError(t, repo.Create(f))
	defer cleanupFlash(t, f.ID)

	require.NoError(t, repo.UpdateFields(f.ID, map[string]any{
		"title":   "beta updated",
		"summary": "new summary",
		"script":  "<:1>",
	}))

	got, err := repo.FindByID(f.ID)
	require.NoError(t, err)
	assert.Equal(t, "beta updated", got.Title)
	assert.Equal(t, "new summary", got.Summary)
	assert.Equal(t, "<:1>", got.Script)
}

func TestFlashRepository_UpdateFields_NoOp(t *testing.T) {
	repo := NewFlashRepository(testDB)
	require.NoError(t, repo.UpdateFields("any", nil))
}

// UpdateFields で permissions に空 model.StringArray を渡しても NOT NULL
// 制約違反を起こさないこと (#896)。core/flash.Service.Update が
// model.StringArray() で wrap せずに plain []string を渡していた時、
// GORM が NULL に倒して 23502 を起こしていた regression guard。
func TestFlashRepository_UpdateFields_EmptyPermissions(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fr_perms", "flashpermsuser")
	defer cleanupUser(t, user.ID)

	f := newTestFlash("fl_cr_perms", user.ID, "perms")
	f.Permissions = model.StringArray{"read:account"}
	require.NoError(t, repo.Create(f))
	defer cleanupFlash(t, f.ID)

	// 空 permissions で update — model.StringArray{} なら '{}' に serialize、
	// plain []string{} だと GORM が NULL に倒す drift があったので
	// model.StringArray で wrap する pattern を guard する。
	require.NoError(t, repo.UpdateFields(f.ID, map[string]any{
		"permissions": model.StringArray{},
	}))

	got, err := repo.FindByID(f.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.Permissions, "permissions は NULL にならず空配列で保存される")
	assert.Empty(t, got.Permissions)
}

func TestFlashRepository_Delete(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_3", "flashuser3")
	defer cleanupUser(t, user.ID)

	f := newTestFlash("fl_cr_3", user.ID, "gamma")
	require.NoError(t, repo.Create(f))
	require.NoError(t, repo.Delete(f))
	_, err := repo.FindByID(f.ID)
	assert.Error(t, err)
}

func TestFlashRepository_IncrementCount(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_4", "flashuser4")
	defer cleanupUser(t, user.ID)

	f := newTestFlash("fl_cr_4", user.ID, "delta")
	require.NoError(t, repo.Create(f))
	defer cleanupFlash(t, f.ID)

	require.NoError(t, repo.IncrementCount(f.ID, "likedCount", 4))
	got, err := repo.FindByID(f.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, got.LikedCount)
}

func TestFlashRepository_ListByUser(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_5", "flashuser5")
	defer cleanupUser(t, user.ID)

	for _, id := range []string{"fl_lst_1", "fl_lst_2", "fl_lst_3"} {
		f := newTestFlash(id, user.ID, id)
		require.NoError(t, repo.Create(f))
		defer cleanupFlash(t, f.ID)
	}

	rows, err := repo.ListByUser(user.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 3)
}

func TestFlashRepository_ListByUser_LimitClamp(t *testing.T) {
	repo := NewFlashRepository(testDB)
	rows, err := repo.ListByUser("nobody", "", "", 9999, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
	rows, err = repo.ListByUser("nobody", "", "", -1, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestFlashRepository_ListByUser_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFlashRepository(db)
	_, err := repo.ListByUser("nobody", "", "", 10, 0)
	assert.Error(t, err)
}

func TestFlashRepository_ListFeatured(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_6", "flashuser6")
	defer cleanupUser(t, user.ID)

	flashes := []*model.Flash{
		newTestFlash("fl_ft_1", user.ID, "ft1"),
		newTestFlash("fl_ft_2", user.ID, "ft2"),
	}
	flashes[0].LikedCount = 5
	flashes[1].LikedCount = 10
	// likedCount=0 は featured 対象外。
	zeroLiked := newTestFlash("fl_ft_zero", user.ID, "ftzero")
	zeroLiked.LikedCount = 0
	flashes = append(flashes, zeroLiked)
	// 非公開 flash は likedCount>0 でも featured から除外される。
	private := newTestFlash("fl_ft_priv", user.ID, "ftpriv")
	private.LikedCount = 99
	private.Visibility = "private"
	flashes = append(flashes, private)
	for _, f := range flashes {
		require.NoError(t, repo.Create(f))
		defer cleanupFlash(t, f.ID)
	}
	rows, err := repo.ListFeatured("", "", 10, 0)
	require.NoError(t, err)
	var found []*model.Flash
	for _, f := range rows {
		if f.UserID == user.ID {
			found = append(found, f)
		}
	}
	require.Len(t, found, 2, "likedCount=0 と非公開 flash は除外される")
	assert.Equal(t, "fl_ft_2", found[0].ID)
	assert.Equal(t, "fl_ft_1", found[1].ID)
}

// cursor (untilID) モードでも visibility / likedCount フィルタが効くこと。
func TestFlashRepository_ListFeatured_CursorExcludesPrivate(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_cur", "flashusercur")
	defer cleanupUser(t, user.ID)

	pub := newTestFlash("fl_cur_pub", user.ID, "pub")
	pub.LikedCount = 3
	priv := newTestFlash("fl_cur_priv", user.ID, "priv")
	priv.LikedCount = 99
	priv.Visibility = "private"
	for _, f := range []*model.Flash{pub, priv} {
		require.NoError(t, repo.Create(f))
		defer cleanupFlash(t, f.ID)
	}
	// untilID をテスト対象 ID より大きい値にして両方を candidate に含める。
	rows, err := repo.ListFeatured("", "fl_cur_zzzz", 10, 0)
	require.NoError(t, err)
	for _, f := range rows {
		assert.NotEqual(t, "fl_cur_priv", f.ID, "cursor モードでも非公開は除外される")
	}
}

func TestFlashRepository_ListFeatured_LimitClamp(t *testing.T) {
	repo := NewFlashRepository(testDB)
	_, err := repo.ListFeatured("", "", 9999, 0)
	require.NoError(t, err)
	_, err = repo.ListFeatured("", "", -1, 0)
	require.NoError(t, err)
}

func TestFlashRepository_ListFeatured_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFlashRepository(db)
	_, err := repo.ListFeatured("", "", 10, 0)
	assert.Error(t, err)
}

func TestFlashRepository_Search(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_7", "flashuser7")
	defer cleanupUser(t, user.ID)

	titles := map[string]string{
		"fl_sr_1": "alpha calc tool",
		"fl_sr_2": "beta game",
		"fl_sr_3": "gamma calc",
	}
	for id, title := range titles {
		f := newTestFlash(id, user.ID, title)
		require.NoError(t, repo.Create(f))
		defer cleanupFlash(t, f.ID)
	}
	// 非公開 flash は title が一致しても検索結果に出ない。
	priv := newTestFlash("fl_sr_priv", user.ID, "delta calc secret")
	priv.Visibility = "private"
	require.NoError(t, repo.Create(priv))
	defer cleanupFlash(t, priv.ID)

	rows, err := repo.Search("calc", "", "", 10, 0)
	require.NoError(t, err)
	var found []*model.Flash
	for _, f := range rows {
		if f.UserID == user.ID {
			found = append(found, f)
		}
	}
	assert.Len(t, found, 2, "非公開 flash は検索対象外")
	for _, f := range found {
		assert.NotEqual(t, "fl_sr_priv", f.ID)
	}
}

func TestFlashRepository_Search_LimitClamp(t *testing.T) {
	repo := NewFlashRepository(testDB)
	_, err := repo.Search("anything", "", "", 9999, 0)
	require.NoError(t, err)
	_, err = repo.Search("anything", "", "", -1, 0)
	require.NoError(t, err)
}

func TestFlashRepository_Search_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFlashRepository(db)
	_, err := repo.Search("any", "", "", 10, 0)
	assert.Error(t, err)
}

// #2106 N8: flash/search は query を空白区切りで各語を AND し (全語を含む検索)、
// LIKE メタ文字 (% _) を escape する (wildcard 無効化)。
func TestFlashRepository_Search_WordAndAndLikeEscape(t *testing.T) {
	repo := NewFlashRepository(testDB)
	user := insertTestUser(t, "u_fla_n8", "flashusern8")
	defer cleanupUser(t, user.ID)

	flashes := map[string]string{
		"fl_n8_both": "foo bar baz", // foo と baz 両方含む
		"fl_n8_foo":  "foo only",    // baz を含まない
		"fl_n8_pct":  "100%done",    // literal %
		"fl_n8_oth":  "100Xdone",    // % を wildcard 扱いするとヒットしてしまう対象
	}
	for id, title := range flashes {
		f := newTestFlash(id, user.ID, title)
		require.NoError(t, repo.Create(f))
		defer cleanupFlash(t, f.ID)
	}

	contains := func(rows []*model.Flash, id string) bool {
		for _, f := range rows {
			if f.ID == id {
				return true
			}
		}
		return false
	}

	// word-AND: 'foo baz' は両語を含む flash のみ
	rows, err := repo.Search("foo baz", "", "", 100, 0)
	require.NoError(t, err)
	assert.True(t, contains(rows, "fl_n8_both"), "全語を含む flash はヒット")
	assert.False(t, contains(rows, "fl_n8_foo"), "baz を含まない flash は除外 (word-AND)")

	// LIKE escape: '100%done' の % は literal として扱う (100Xdone を拾わない)
	rows, err = repo.Search("100%done", "", "", 100, 0)
	require.NoError(t, err)
	assert.True(t, contains(rows, "fl_n8_pct"), "literal % にマッチ")
	assert.False(t, contains(rows, "fl_n8_oth"), "% は wildcard でなく literal (100Xdone は除外)")
}
