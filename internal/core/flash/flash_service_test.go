package flash_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/flash"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSvc(t *testing.T) (*flash.Service, *testutil.MockFlashRepository, *testutil.MockFlashLikeRepository) {
	t.Helper()
	repo := testutil.NewMockFlashRepository()
	likeRepo := testutil.NewMockFlashLikeRepository()
	idGen, _ := id.NewGenerator("aidx")
	return flash.NewService(repo, likeRepo, idGen), repo, likeRepo
}

// --- Create ----------------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	f, err := svc.Create(flash.CreateInput{
		OwnerID: "u1", Title: "t", Script: "<:0>",
	})
	require.NoError(t, err)
	assert.Equal(t, "t", f.Title)
	assert.Equal(t, "public", f.Visibility)
	assert.Len(t, repo.Flashes, 1)
}

func TestCreate_PassesThroughOptionalFields(t *testing.T) {
	svc, _, _ := newSvc(t)
	f, err := svc.Create(flash.CreateInput{
		OwnerID:     "u1",
		Title:       "t",
		Summary:     "summary",
		Script:      "<:0>",
		Permissions: []string{"read:account"},
		Visibility:  "private",
	})
	require.NoError(t, err)
	assert.Equal(t, "summary", f.Summary)
	assert.Equal(t, "private", f.Visibility)
	assert.Len(t, f.Permissions, 1)
}

func TestCreate_OwnerRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Create(flash.CreateInput{Title: "t", Script: "x"})
	assert.Error(t, err)
}

func TestCreate_TitleRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Create(flash.CreateInput{OwnerID: "u1", Script: "x"})
	assert.ErrorIs(t, err, flash.ErrFlashTitleRequired)
}

func TestCreate_ScriptRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Create(flash.CreateInput{OwnerID: "u1", Title: "t"})
	assert.ErrorIs(t, err, flash.ErrFlashScriptRequired)
}

func TestCreate_RepoError(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.CreateErr = errors.New("boom")
	_, err := svc.Create(flash.CreateInput{OwnerID: "u1", Title: "t", Script: "x"})
	assert.Error(t, err)
}

// --- Show ------------------------------------------------------------------

func TestShow_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1", Title: "t"}
	got, err := svc.Show("u2", "f1")
	require.NoError(t, err)
	assert.Equal(t, "t", got.Title)
}

func TestShow_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Show("u1", "missing")
	assert.ErrorIs(t, err, flash.ErrFlashNotFound)
}

// --- Update ----------------------------------------------------------------

func TestUpdate_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1", Title: "t"}
	newTitle := "t2"
	newSummary := "s"
	newScript := "<:1>"
	newPerms := []string{"a"}
	newVis := "private"
	got, err := svc.Update("u1", "f1", flash.UpdateInput{
		Title:       &newTitle,
		Summary:     &newSummary,
		Script:      &newScript,
		Permissions: &newPerms,
		Visibility:  &newVis,
	})
	require.NoError(t, err)
	assert.Equal(t, "t2", got.Title)
	assert.Equal(t, "s", got.Summary)
	assert.Equal(t, "<:1>", got.Script)
	assert.Equal(t, "private", got.Visibility)
}

func TestUpdate_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Update("u1", "missing", flash.UpdateInput{})
	assert.ErrorIs(t, err, flash.ErrFlashNotFound)
}

func TestUpdate_AccessDenied(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "owner"}
	_, err := svc.Update("u1", "f1", flash.UpdateInput{})
	assert.ErrorIs(t, err, flash.ErrAccessDenied)
}

func TestUpdate_TitleEmpty(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	empty := ""
	_, err := svc.Update("u1", "f1", flash.UpdateInput{Title: &empty})
	assert.ErrorIs(t, err, flash.ErrFlashTitleRequired)
}

// Update で empty Permissions を渡しても、UpdateFields に model.StringArray
// として渡されることを担保する (#896)。plain []string で渡すと
// production の GORM 経由で NULL 化して NOT NULL 制約違反になる。
// ここでは mock repo の UpdateFields capture で wrap 型を検査する。
func TestUpdate_EmptyPermissionsWrappedAsStringArray(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1", Title: "t"}
	emptyPerms := []string{}
	_, err := svc.Update("u1", "f1", flash.UpdateInput{Permissions: &emptyPerms})
	require.NoError(t, err)
	require.Contains(t, repo.LastUpdates, "permissions")
	v, ok := repo.LastUpdates["permissions"].(model.StringArray)
	require.True(t, ok, "permissions should be model.StringArray, got %T", repo.LastUpdates["permissions"])
	assert.Empty(t, v)
}

func TestUpdate_RepoError(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	repo.UpdateErr = errors.New("boom")
	title := "x"
	_, err := svc.Update("u1", "f1", flash.UpdateInput{Title: &title})
	assert.Error(t, err)
}

// --- Delete ----------------------------------------------------------------

func TestDelete_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	require.NoError(t, svc.Delete("u1", "f1"))
	assert.Empty(t, repo.Flashes)
}

func TestDelete_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Delete("u1", "missing")
	assert.ErrorIs(t, err, flash.ErrFlashNotFound)
}

func TestDelete_AccessDenied(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "owner"}
	err := svc.Delete("u1", "f1")
	assert.ErrorIs(t, err, flash.ErrAccessDenied)
}

// --- My / Featured / Search -----------------------------------------------

func TestMy(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	repo.Flashes["f2"] = &model.Flash{ID: "f2", UserID: "u1"}
	rows, err := svc.My("u1", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestFeatured(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1", LikedCount: 5}
	repo.Flashes["f2"] = &model.Flash{ID: "f2", UserID: "u1", LikedCount: 10}
	rows, err := svc.Featured("", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "f2", rows[0].ID)
	assert.Equal(t, "f1", rows[1].ID)
}

func TestSearch(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1", Title: "alpha calc"}
	repo.Flashes["f2"] = &model.Flash{ID: "f2", UserID: "u1", Title: "beta game"}
	rows, err := svc.Search("calc", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// --- Like ------------------------------------------------------------------

func TestLike_HappyPath(t *testing.T) {
	svc, repo, likeRepo := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	require.NoError(t, svc.Like("u2", "f1"))
	assert.Len(t, likeRepo.Likes, 1)
	assert.Equal(t, 1, repo.Flashes["f1"].LikedCount)
}

func TestLike_UserRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Like("", "f1")
	assert.Error(t, err)
}

func TestLike_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Like("u1", "missing")
	assert.ErrorIs(t, err, flash.ErrFlashNotFound)
}

func TestLike_AlreadyLiked(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	require.NoError(t, svc.Like("u2", "f1"))
	err := svc.Like("u2", "f1")
	assert.ErrorIs(t, err, flash.ErrAlreadyLiked)
}

// failingExistsLikeRepo causes Exists to fail.
type failingExistsLikeRepo struct {
	*testutil.MockFlashLikeRepository
}

func (r *failingExistsLikeRepo) Exists(_, _ string) (bool, error) {
	return false, errors.New("boom")
}

func TestLike_ExistsError(t *testing.T) {
	repo := testutil.NewMockFlashRepository()
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	likeRepo := &failingExistsLikeRepo{MockFlashLikeRepository: testutil.NewMockFlashLikeRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := flash.NewService(repo, likeRepo, idGen)
	err := svc.Like("u2", "f1")
	assert.Error(t, err)
}

// failingCreateLikeRepo causes Create to fail.
type failingCreateLikeRepo struct {
	*testutil.MockFlashLikeRepository
}

func (r *failingCreateLikeRepo) Create(_ *model.FlashLike) error { return errors.New("boom") }

func TestLike_CreateError(t *testing.T) {
	repo := testutil.NewMockFlashRepository()
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	likeRepo := &failingCreateLikeRepo{MockFlashLikeRepository: testutil.NewMockFlashLikeRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := flash.NewService(repo, likeRepo, idGen)
	err := svc.Like("u2", "f1")
	assert.Error(t, err)
}

// --- Unlike ----------------------------------------------------------------

func TestUnlike_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	require.NoError(t, svc.Like("u2", "f1"))
	require.NoError(t, svc.Unlike("u2", "f1"))
	assert.Equal(t, 0, repo.Flashes["f1"].LikedCount)
}

func TestUnlike_UserRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Unlike("", "f1")
	assert.Error(t, err)
}

func TestUnlike_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Unlike("u1", "missing")
	assert.ErrorIs(t, err, flash.ErrFlashNotFound)
}

func TestUnlike_NotLiked(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	err := svc.Unlike("u2", "f1")
	assert.ErrorIs(t, err, flash.ErrNotLiked)
}

// failingDeleteLikeRepo causes Delete to fail.
type failingDeleteLikeRepo struct {
	*testutil.MockFlashLikeRepository
}

func (r *failingDeleteLikeRepo) Delete(_ *model.FlashLike) error { return errors.New("boom") }

func TestUnlike_DeleteError(t *testing.T) {
	repo := testutil.NewMockFlashRepository()
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	mock := testutil.NewMockFlashLikeRepository()
	mock.Likes["fl1"] = &model.FlashLike{ID: "fl1", UserID: "u2", FlashID: "f1"}
	likeRepo := &failingDeleteLikeRepo{MockFlashLikeRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := flash.NewService(repo, likeRepo, idGen)
	err := svc.Unlike("u2", "f1")
	assert.Error(t, err)
}

// --- MyLikes ---------------------------------------------------------------

func TestMyLikes_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	repo.Flashes["f2"] = &model.Flash{ID: "f2", UserID: "u1"}
	require.NoError(t, svc.Like("u2", "f1"))
	require.NoError(t, svc.Like("u2", "f2"))
	rows, err := svc.MyLikes("u2", "", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestMyLikes_SkipsMissingFlashes(t *testing.T) {
	svc, repo, likeRepo := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	require.NoError(t, svc.Like("u2", "f1"))
	// Insert a stale like pointing to a non-existent flash.
	likeRepo.Likes["stale"] = &model.FlashLike{ID: "stale", UserID: "u2", FlashID: "missing"}
	rows, err := svc.MyLikes("u2", "", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// failingListLikeRepo causes ListByUserSearch (used by MyLikes) to fail.
type failingListLikeRepo struct {
	*testutil.MockFlashLikeRepository
}

func (r *failingListLikeRepo) ListByUserSearch(_, _, _, _ string, _, _ int) ([]*model.FlashLike, error) {
	return nil, errors.New("boom")
}

func TestMyLikes_RepoError(t *testing.T) {
	repo := testutil.NewMockFlashRepository()
	likeRepo := &failingListLikeRepo{MockFlashLikeRepository: testutil.NewMockFlashLikeRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := flash.NewService(repo, likeRepo, idGen)
	_, err := svc.MyLikes("u2", "", "", "", 10, 0)
	assert.Error(t, err)
}

// --- SetClock --------------------------------------------------------------

func TestSetClock(t *testing.T) {
	svc, _, _ := newSvc(t)
	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	svc.SetClock(nil)
	f, err := svc.Create(flash.CreateInput{OwnerID: "u1", Title: "t", Script: "x"})
	require.NoError(t, err)
	assert.NotEmpty(t, f.ID)
}

// #1548: 自分の flash を like すると ErrYourFlash。
func TestLike_YourFlash(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "u1"}
	err := svc.Like("u1", "f1")
	assert.ErrorIs(t, err, flash.ErrYourFlash)
}

// #1548: DeleteByID は owner チェックなしで削除する (moderator 経路)。
func TestDeleteByID(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "owner"}
	require.NoError(t, svc.DeleteByID("f1"))
	_, err := repo.FindByID("f1")
	assert.Error(t, err)
}

func TestDeleteByID_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	assert.ErrorIs(t, svc.DeleteByID("ghost"), flash.ErrFlashNotFound)
}

// #1548: LikedFlashIDs は viewer が like した flashId の集合を返す。
func TestLikedFlashIDs(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "owner"}
	repo.Flashes["f2"] = &model.Flash{ID: "f2", UserID: "owner"}
	require.NoError(t, svc.Like("u2", "f1"))

	set, err := svc.LikedFlashIDs("u2", []string{"f1", "f2"})
	require.NoError(t, err)
	assert.True(t, set["f1"])
	assert.False(t, set["f2"])

	// 空 userID / flashIDs は空集合。
	empty, err := svc.LikedFlashIDs("", []string{"f1"})
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// IsLikedBy reports the viewer's like state for a single flash.
func TestIsLikedBy(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Flashes["f1"] = &model.Flash{ID: "f1", UserID: "owner"}
	require.NoError(t, svc.Like("u2", "f1"))
	liked, err := svc.IsLikedBy("u2", "f1")
	require.NoError(t, err)
	assert.True(t, liked)
	notLiked, err := svc.IsLikedBy("u3", "f1")
	require.NoError(t, err)
	assert.False(t, notLiked)
}
