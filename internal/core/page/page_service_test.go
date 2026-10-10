package page_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/page"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSvc(t *testing.T) (*page.Service, *testutil.MockPageRepository, *testutil.MockPageLikeRepository) {
	t.Helper()
	repo := testutil.NewMockPageRepository()
	likeRepo := testutil.NewMockPageLikeRepository()
	idGen, _ := id.NewGenerator("aidx")
	return page.NewService(repo, likeRepo, idGen), repo, likeRepo
}

// --- Create ----------------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	p, err := svc.Create(page.CreateInput{
		OwnerID: "u1", Title: "t", Name: "alpha",
	})
	require.NoError(t, err)
	assert.Equal(t, "alpha", p.Name)
	assert.Equal(t, model.PageVisibilityPublic, p.Visibility)
	assert.Equal(t, "sans-serif", p.Font)
	assert.Equal(t, []byte("[]"), []byte(p.Content))
	assert.Equal(t, []byte("[]"), []byte(p.Variables))
	assert.Len(t, repo.Pages, 1)
}

func TestCreate_PassesThroughOptionalFields(t *testing.T) {
	svc, _, _ := newSvc(t)
	summary := "summary"
	imgID := "img1"
	p, err := svc.Create(page.CreateInput{
		OwnerID:             "u1",
		Title:               "t",
		Name:                "alpha",
		Summary:             &summary,
		AlignCenter:         true,
		HideTitleWhenPinned: true,
		Font:                "serif",
		EyeCatchingImageID:  &imgID,
		Content:             []byte(`[{"id":"a"}]`),
		Variables:           []byte(`[{"v":1}]`),
		Script:              "x",
		Visibility:          model.PageVisibilityPrivate,
	})
	require.NoError(t, err)
	assert.Equal(t, "summary", *p.Summary)
	assert.True(t, p.AlignCenter)
	assert.True(t, p.HideTitleWhenPinned)
	assert.Equal(t, "serif", p.Font)
	assert.Equal(t, "img1", *p.EyeCatchingImageID)
	assert.Equal(t, "x", p.Script)
	assert.Equal(t, model.PageVisibilityPrivate, p.Visibility)
}

func TestCreate_OwnerRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Create(page.CreateInput{Title: "t", Name: "alpha"})
	assert.Error(t, err)
}

func TestCreate_TitleRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Create(page.CreateInput{OwnerID: "u1", Name: "alpha"})
	assert.ErrorIs(t, err, page.ErrPageTitleRequired)
}

func TestCreate_NameRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Create(page.CreateInput{OwnerID: "u1", Title: "t"})
	assert.ErrorIs(t, err, page.ErrPageNameRequired)
}

// #1548: name が pageNameSchema の禁止文字を含むと ErrPageNameInvalid。
func TestCreate_NameInvalidPattern(t *testing.T) {
	svc, _, _ := newSvc(t)
	for _, bad := range []string{"has space", "slash/x", "colon:x", "hash#x", "at@x", "q?x"} {
		_, err := svc.Create(page.CreateInput{OwnerID: "u1", Title: "t", Name: bad})
		assert.ErrorIs(t, err, page.ErrPageNameInvalid, "name %q must be rejected", bad)
	}
}

// 正常な name (英数 / ハイフン / アンダースコア等) は通る。
func TestCreate_NameValidPattern(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Create(page.CreateInput{OwnerID: "u1", Title: "t", Name: "my-page_01.draft"})
	require.NoError(t, err)
}

// #1548: update でも禁止文字 name は ErrPageNameInvalid。
func TestUpdate_NameInvalidPattern(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Name: "ok"}
	bad := "bad name"
	_, err := svc.Update("u1", "p1", page.UpdateInput{Name: &bad})
	assert.ErrorIs(t, err, page.ErrPageNameInvalid)
}

func TestCreate_NameConflict(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["existing"] = &model.Page{ID: "existing", UserID: "u1", Name: "alpha"}
	_, err := svc.Create(page.CreateInput{OwnerID: "u1", Title: "t", Name: "alpha"})
	assert.ErrorIs(t, err, page.ErrPageNameConflict)
}

func TestCreate_RepoError(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.CreateErr = errors.New("boom")
	_, err := svc.Create(page.CreateInput{OwnerID: "u1", Title: "t", Name: "alpha"})
	assert.Error(t, err)
}

// --- Show ------------------------------------------------------------------

func TestShow_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	got, err := svc.Show("u2", "p1")
	require.NoError(t, err)
	assert.Equal(t, "p1", got.ID)
}

func TestShow_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Show("u1", "missing")
	assert.ErrorIs(t, err, page.ErrPageNotFound)
}

func TestShow_PrivateAccessDenied(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPrivate}
	_, err := svc.Show("u2", "p1")
	assert.ErrorIs(t, err, page.ErrAccessDenied)
}

func TestShow_PrivateOwnerOK(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPrivate}
	got, err := svc.Show("u1", "p1")
	require.NoError(t, err)
	assert.Equal(t, "p1", got.ID)
}

// --- FindByID (visibility チェック無し) -----------------------------------

func TestFindByID_HappyPath_Public(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	got, err := svc.FindByID("p1")
	require.NoError(t, err)
	assert.Equal(t, "p1", got.ID)
}

func TestFindByID_Returns_Private_Regardless_Of_Visibility(t *testing.T) {
	svc, repo, _ := newSvc(t)
	// followers visibilityでも visibility checkはせず返すことを確認
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPrivate}
	got, err := svc.FindByID("p1")
	require.NoError(t, err)
	assert.Equal(t, "p1", got.ID)
}

func TestFindByID_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.FindByID("missing")
	assert.ErrorIs(t, err, page.ErrPageNotFound)
}

// --- ShowByName ------------------------------------------------------------

func TestShowByName_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Name: "alpha", Visibility: model.PageVisibilityPublic}
	got, err := svc.ShowByName("u2", "u1", "alpha")
	require.NoError(t, err)
	assert.Equal(t, "p1", got.ID)
}

func TestShowByName_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.ShowByName("u1", "u1", "missing")
	assert.ErrorIs(t, err, page.ErrPageNotFound)
}

func TestShowByName_PrivateAccessDenied(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Name: "alpha", Visibility: model.PageVisibilityPrivate}
	_, err := svc.ShowByName("u2", "u1", "alpha")
	assert.ErrorIs(t, err, page.ErrAccessDenied)
}

// --- Update ----------------------------------------------------------------

func TestUpdate_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Name: "alpha", Title: "t"}
	newTitle := "t2"
	newName := "beta"
	summary := "s"
	summaryPtr := &summary
	font := "serif"
	imgID := "img1"
	imgPtr := &imgID
	align := true
	hide := true
	script := "x"
	vis := model.PageVisibilityPrivate
	got, err := svc.Update("u1", "p1", page.UpdateInput{
		Title:               &newTitle,
		Name:                &newName,
		Summary:             &summaryPtr,
		AlignCenter:         &align,
		HideTitleWhenPinned: &hide,
		Font:                &font,
		EyeCatchingImageID:  &imgPtr,
		Content:             []byte("[1]"),
		Variables:           []byte("[2]"),
		Script:              &script,
		Visibility:          &vis,
	})
	require.NoError(t, err)
	assert.Equal(t, "t2", got.Title)
	assert.Equal(t, "beta", got.Name)
	require.NotNil(t, got.Summary)
	assert.Equal(t, "s", *got.Summary)
	assert.True(t, got.AlignCenter)
	assert.True(t, got.HideTitleWhenPinned)
	assert.Equal(t, "serif", got.Font)
	require.NotNil(t, got.EyeCatchingImageID)
	assert.Equal(t, "img1", *got.EyeCatchingImageID)
	assert.Equal(t, "x", got.Script)
	assert.Equal(t, model.PageVisibilityPrivate, got.Visibility)
}

func TestUpdate_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Update("u1", "missing", page.UpdateInput{})
	assert.ErrorIs(t, err, page.ErrPageNotFound)
}

func TestUpdate_AccessDenied(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Visibility: model.PageVisibilityPublic}
	_, err := svc.Update("u1", "p1", page.UpdateInput{})
	assert.ErrorIs(t, err, page.ErrAccessDenied)
}

func TestUpdate_TitleEmpty(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1"}
	empty := ""
	_, err := svc.Update("u1", "p1", page.UpdateInput{Title: &empty})
	assert.ErrorIs(t, err, page.ErrPageTitleRequired)
}

func TestUpdate_NameEmpty(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1"}
	empty := ""
	_, err := svc.Update("u1", "p1", page.UpdateInput{Name: &empty})
	assert.ErrorIs(t, err, page.ErrPageNameRequired)
}

func TestUpdate_NameUnchangedNoConflictCheck(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Name: "alpha"}
	name := "alpha"
	_, err := svc.Update("u1", "p1", page.UpdateInput{Name: &name})
	require.NoError(t, err)
}

func TestUpdate_NameConflict(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Name: "alpha"}
	repo.Pages["p2"] = &model.Page{ID: "p2", UserID: "u1", Name: "beta"}
	name := "beta"
	_, err := svc.Update("u1", "p1", page.UpdateInput{Name: &name})
	assert.ErrorIs(t, err, page.ErrPageNameConflict)
}

func TestUpdate_RepoError(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1"}
	repo.UpdateErr = errors.New("boom")
	title := "x"
	_, err := svc.Update("u1", "p1", page.UpdateInput{Title: &title})
	assert.Error(t, err)
}

// --- Delete ----------------------------------------------------------------

func TestDelete_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1"}
	require.NoError(t, svc.Delete("u1", "p1"))
	assert.Empty(t, repo.Pages)
}

func TestDelete_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Delete("u1", "missing")
	assert.ErrorIs(t, err, page.ErrPageNotFound)
}

func TestDelete_AccessDenied(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Visibility: model.PageVisibilityPublic}
	err := svc.Delete("u1", "p1")
	assert.ErrorIs(t, err, page.ErrAccessDenied)
}

// --- ListByUser / Featured ------------------------------------------------

func TestListByUser(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1"}
	repo.Pages["p2"] = &model.Page{ID: "p2", UserID: "u1"}
	rows, err := svc.ListByUser("u1", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestFeatured(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic, LikedCount: 5}
	repo.Pages["p2"] = &model.Page{ID: "p2", UserID: "u1", Visibility: model.PageVisibilityPublic, LikedCount: 10}
	repo.Pages["p3"] = &model.Page{ID: "p3", UserID: "u1", Visibility: model.PageVisibilityPrivate, LikedCount: 100}
	rows, err := svc.Featured("", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "p2", rows[0].ID)
	assert.Equal(t, "p1", rows[1].ID)
}

// --- Like ------------------------------------------------------------------

// TestHasLiked covers the lookup helper added for /api/pages/show
// (#1134). Empty / missing input は false で fail-soft (panic 無し)。
func TestHasLiked(t *testing.T) {
	svc, repo, likeRepo := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	assert.False(t, svc.HasLiked("u2", "p1"))
	require.NoError(t, likeRepo.Create(&model.PageLike{ID: "l1", UserID: "u2", PageID: "p1"}))
	assert.True(t, svc.HasLiked("u2", "p1"))
	assert.False(t, svc.HasLiked("", "p1"))
	assert.False(t, svc.HasLiked("u2", ""))
}

// #1773: LikedPageIDs は viewer が like した pageID の集合を返す
// (pages/featured の batch isLiked 用)。empty userID / pageIDs は query せず空。
func TestLikedPageIDs(t *testing.T) {
	svc, _, likeRepo := newSvc(t)
	require.NoError(t, likeRepo.Create(&model.PageLike{ID: "l1", UserID: "u2", PageID: "p1"}))
	set, err := svc.LikedPageIDs("u2", []string{"p1", "p2"})
	require.NoError(t, err)
	assert.True(t, set["p1"])
	assert.False(t, set["p2"])

	empty, err := svc.LikedPageIDs("", []string{"p1"})
	require.NoError(t, err)
	assert.Empty(t, empty)
	empty, err = svc.LikedPageIDs("u2", nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestLike_HappyPath(t *testing.T) {
	svc, repo, likeRepo := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	require.NoError(t, svc.Like("u2", "p1"))
	assert.Len(t, likeRepo.Likes, 1)
	assert.Equal(t, 1, repo.Pages["p1"].LikedCount)
}

func TestLike_UserRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Like("", "p1")
	assert.Error(t, err)
}

func TestLike_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Like("u1", "missing")
	assert.ErrorIs(t, err, page.ErrPageNotFound)
}

func TestLike_PrivateAccessDenied(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPrivate}
	err := svc.Like("u2", "p1")
	assert.ErrorIs(t, err, page.ErrAccessDenied)
}

// #1438: owner は自分の page を like できない (upstream TS の yourPage)。可視性に
// 依らず YOUR_PAGE で弾く。owner は可視性ゲートを素通りするため、private page でも
// ErrYourPage が返る (ErrAccessDenied ではない)。
func TestLike_OwnPrivate_YourPage(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPrivate}
	err := svc.Like("u1", "p1")
	assert.ErrorIs(t, err, page.ErrYourPage)
}

// #1438: public page でも owner 自身の like は YOUR_PAGE で弾き、like 行も
// likedCount も増やさない。
func TestLike_OwnPublic_YourPage(t *testing.T) {
	svc, repo, likeRepo := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	err := svc.Like("u1", "p1")
	assert.ErrorIs(t, err, page.ErrYourPage)
	assert.Empty(t, likeRepo.Likes)
	assert.Equal(t, 0, repo.Pages["p1"].LikedCount)
}

func TestLike_AlreadyLiked(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	require.NoError(t, svc.Like("u2", "p1"))
	err := svc.Like("u2", "p1")
	assert.ErrorIs(t, err, page.ErrAlreadyLiked)
}

// failingExistsLikeRepo causes Exists to fail.
type failingExistsLikeRepo struct {
	*testutil.MockPageLikeRepository
}

func (r *failingExistsLikeRepo) Exists(_, _ string) (bool, error) {
	return false, errors.New("boom")
}

func TestLike_ExistsError(t *testing.T) {
	repo := testutil.NewMockPageRepository()
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	likeRepo := &failingExistsLikeRepo{MockPageLikeRepository: testutil.NewMockPageLikeRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := page.NewService(repo, likeRepo, idGen)
	err := svc.Like("u2", "p1")
	assert.Error(t, err)
}

// failingListLikedPageIDsRepo causes ListLikedPageIDs to fail (#1773)。
type failingListLikedPageIDsRepo struct {
	*testutil.MockPageLikeRepository
}

func (r *failingListLikedPageIDsRepo) ListLikedPageIDs(_ string, _ []string) ([]string, error) {
	return nil, errors.New("boom")
}

func TestLikedPageIDs_RepoError(t *testing.T) {
	repo := testutil.NewMockPageRepository()
	likeRepo := &failingListLikedPageIDsRepo{MockPageLikeRepository: testutil.NewMockPageLikeRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := page.NewService(repo, likeRepo, idGen)
	set, err := svc.LikedPageIDs("u2", []string{"p1"})
	assert.Error(t, err)
	assert.Empty(t, set)
}

// failingCreateLikeRepo causes Create to fail.
type failingCreateLikeRepo struct {
	*testutil.MockPageLikeRepository
}

func (r *failingCreateLikeRepo) Create(_ *model.PageLike) error { return errors.New("boom") }

func TestLike_CreateError(t *testing.T) {
	repo := testutil.NewMockPageRepository()
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	likeRepo := &failingCreateLikeRepo{MockPageLikeRepository: testutil.NewMockPageLikeRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := page.NewService(repo, likeRepo, idGen)
	err := svc.Like("u2", "p1")
	assert.Error(t, err)
}

// --- Unlike ----------------------------------------------------------------

func TestUnlike_HappyPath(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	require.NoError(t, svc.Like("u2", "p1"))
	require.NoError(t, svc.Unlike("u2", "p1"))
	assert.Equal(t, 0, repo.Pages["p1"].LikedCount)
}

func TestUnlike_UserRequired(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Unlike("", "p1")
	assert.Error(t, err)
}

func TestUnlike_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Unlike("u1", "missing")
	assert.ErrorIs(t, err, page.ErrPageNotFound)
}

func TestUnlike_NotLiked(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	err := svc.Unlike("u2", "p1")
	assert.ErrorIs(t, err, page.ErrNotLiked)
}

// TestUnlike_PrivatePageHidesFromStrangers checks that a stranger who has not
// liked a private page cannot tell it exists (#3479).
func TestUnlike_PrivatePageHidesFromStrangers(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPrivate}
	assert.ErrorIs(t, svc.Unlike("u2", "p1"), page.ErrPageNotFound)
	assert.ErrorIs(t, svc.Unlike("u1", "p1"), page.ErrNotLiked)
}

// TestUpdateDelete_PrivatePageHidesFromStrangers checks that a non-owner
// cannot tell a private page exists through update or delete (#3479).
func TestUpdateDelete_PrivatePageHidesFromStrangers(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Pages["priv"] = &model.Page{ID: "priv", UserID: "u1", Visibility: model.PageVisibilityPrivate}
	repo.Pages["pub"] = &model.Page{ID: "pub", UserID: "u1", Visibility: model.PageVisibilityPublic}
	title := "x"

	_, err := svc.Update("u2", "priv", page.UpdateInput{Title: &title})
	assert.ErrorIs(t, err, page.ErrPageNotFound)
	assert.ErrorIs(t, svc.Delete("u2", "priv"), page.ErrPageNotFound)
	require.Contains(t, repo.Pages, "priv")

	// 公開の Page は本家と同じ ACCESS_DENIED のまま。
	_, err = svc.Update("u2", "pub", page.UpdateInput{Title: &title})
	assert.ErrorIs(t, err, page.ErrAccessDenied)
	assert.ErrorIs(t, svc.Delete("u2", "pub"), page.ErrAccessDenied)
}

// failingDeleteLikeRepo causes Delete to fail.
type failingDeleteLikeRepo struct {
	*testutil.MockPageLikeRepository
}

func (r *failingDeleteLikeRepo) Delete(_ *model.PageLike) error { return errors.New("boom") }

func TestUnlike_DeleteError(t *testing.T) {
	repo := testutil.NewMockPageRepository()
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "u1", Visibility: model.PageVisibilityPublic}
	mock := testutil.NewMockPageLikeRepository()
	mock.Likes["pl1"] = &model.PageLike{ID: "pl1", UserID: "u2", PageID: "p1"}
	likeRepo := &failingDeleteLikeRepo{MockPageLikeRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := page.NewService(repo, likeRepo, idGen)
	err := svc.Unlike("u2", "p1")
	assert.Error(t, err)
}

// --- SetClock --------------------------------------------------------------

func TestSetClock(t *testing.T) {
	svc, _, _ := newSvc(t)
	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	svc.SetClock(nil)
	p, err := svc.Create(page.CreateInput{OwnerID: "u1", Title: "t", Name: "alpha"})
	require.NoError(t, err)
	assert.NotEmpty(t, p.ID)
}

// failingPageRepo makes every page lookup look like a database failure.
type failingPageRepo struct {
	*testutil.MockPageRepository
	err error
}

func (r *failingPageRepo) FindByID(string) (*model.Page, error) { return nil, r.err }

func (r *failingPageRepo) FindByUserAndName(string, string) (*model.Page, error) {
	return nil, r.err
}

// failingLikeRepo makes every like lookup look like a database failure.
type failingLikeRepo struct {
	*testutil.MockPageLikeRepository
	err error
}

func (r *failingLikeRepo) FindByPair(string, string) (*model.PageLike, error) { return nil, r.err }

// **DB 障害を ErrPageNotFound に丸めないこと** (#2792)。
//
// 全部 ErrPageNotFound にすると、呼び出し側は 4xx を返すしかなくなり、接続断が
// 「そんなページは無い」として返る。監視でも 5xx が立たない。
func TestFindByID_DBFailureIsNotPageNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")
	svc := page.NewService(&failingPageRepo{
		MockPageRepository: testutil.NewMockPageRepository(),
		err:                dbErr,
	}, testutil.NewMockPageLikeRepository(), idGen)

	_, err := svc.FindByID("p1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, page.ErrPageNotFound),
		"DB 障害が not-found に丸められている")
	assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
}

// not-found は従来どおり ErrPageNotFound。
func TestFindByID_MissingRowIsPageNotFound(t *testing.T) {
	svc, _, _ := newSvc(t)

	_, err := svc.FindByID("ghost")
	assert.ErrorIs(t, err, page.ErrPageNotFound)
}

// **兄弟メソッドも同じ扱いにすること** (#2792)。
//
// `FindByID` だけ直して他を放置していた期間があり、DB 障害中は全ページが
// 「そんなページは無い」の 400 で返っていた。**gate は `internal/core` を
// walk しないので、ここが唯一の回帰検知**。
func TestPageService_DBFailureIsNotPageNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")

	newFailing := func() *page.Service {
		return page.NewService(
			&failingPageRepo{MockPageRepository: testutil.NewMockPageRepository(), err: dbErr},
			testutil.NewMockPageLikeRepository(), idGen)
	}

	for _, tt := range []struct {
		name string
		run  func(*page.Service) error
	}{
		{"Show", func(s *page.Service) error { _, err := s.Show("u1", "p1"); return err }},
		{"ShowByName", func(s *page.Service) error { _, err := s.ShowByName("u1", "o1", "n"); return err }},
		{"Update", func(s *page.Service) error { _, err := s.Update("u1", "p1", page.UpdateInput{}); return err }},
		{"Delete", func(s *page.Service) error { return s.Delete("u1", "p1") }},
		{"Like", func(s *page.Service) error { return s.Like("u1", "p1") }},
		{"Unlike", func(s *page.Service) error { return s.Unlike("u1", "p1") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(newFailing())
			require.Error(t, err)
			assert.False(t, errors.Is(err, page.ErrPageNotFound),
				"DB 障害が not-found に丸められている")
			assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
		})
	}
}

// Unlike の like lookup も同じ (#2792)。ここを潰すと「like していない」に化ける。
func TestUnlike_LikeLookupDBFailureIsNotNotLiked(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")
	repo := testutil.NewMockPageRepository()
	require.NoError(t, repo.Create(&model.Page{
		ID: "p1", UserID: "other", Name: "n", Visibility: model.PageVisibilityPublic,
	}))
	svc := page.NewService(repo, &failingLikeRepo{
		MockPageLikeRepository: testutil.NewMockPageLikeRepository(), err: dbErr,
	}, idGen)

	err := svc.Unlike("u1", "p1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, page.ErrNotLiked), "DB 障害が「like していない」に化けている")
}

// **jsonb 列は `string` で渡すこと (#3037)。**
//
// `[]byte` のまま `Updates` に載せると driver が bytea として送り、jsonb 列への
// 代入が SQLSTATE 22P02 で落ちる。つまり `pages/update` の `content` /
// `variables` は**どんな値でも 500** だった。mock repository では値をそのまま
// 受けてしまうので、**何を渡したか自体**を検査する。
func TestUpdate_PassesJSONBColumnsAsString(t *testing.T) {
	repo := testutil.NewMockPageRepository()
	likeRepo := testutil.NewMockPageLikeRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := page.NewService(repo, likeRepo, idGen)

	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "n", Title: "t"}

	content := json.RawMessage(`[{"x":1}]`)
	variables := json.RawMessage(`[{"v":"a"}]`)
	_, err := svc.Update("alice", "p1", page.UpdateInput{Content: content, Variables: variables})
	require.NoError(t, err)

	require.Len(t, repo.UpdateFieldsCalls, 1)
	fields := repo.UpdateFieldsCalls[0]
	assert.IsType(t, "", fields["content"], "content を []byte のまま渡している (bytea になる)")
	assert.IsType(t, "", fields["variables"], "variables を []byte のまま渡している (bytea になる)")
	assert.Equal(t, `[{"x":1}]`, fields["content"])
	assert.Equal(t, `[{"v":"a"}]`, fields["variables"])
}
