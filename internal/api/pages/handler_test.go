package pages

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corepage "github.com/elythia-network/elythia/internal/core/page"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newHandler(t *testing.T) (*Handler, *testutil.MockPageRepository, *testutil.MockPageLikeRepository) {
	t.Helper()
	repo := testutil.NewMockPageRepository()
	likeRepo := testutil.NewMockPageLikeRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, likeRepo, idGen)
	return NewHandler(svc, idGen), repo, likeRepo
}

func newReq(t *testing.T, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func setUser(c echo.Context, userID string) {
	c.Set(string(middleware.UserContextKey), &model.User{ID: userID})
}

// --- Create ----------------------------------------------------------------

func TestCreate_Success(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"title":"t","name":"alpha","content":[],"variables":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCreate_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1548: name が禁止文字を含むと INVALID_PARAM (pageNameSchema)。
func TestCreate_NameInvalidPattern(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"title":"t","name":"has space"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1548: eyeCatchingImageId が自分の drive file でなければ NO_SUCH_FILE。
func TestCreate_EyeCatchingImageNoSuchFile(t *testing.T) {
	h, _, _ := newHandler(t)
	h.SetDriveFileRepo(testutil.NewMockDriveFileRepository()) // 空 = 不在
	c, rec := newReq(t, `{"title":"t","name":"alpha","eyeCatchingImageId":"f_ghost","content":[],"variables":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "b7b97489-0f66-4b12-a5ff-b21bd63f6e1c")
}

// 他人の drive file を eyeCatchingImageId に指定しても NO_SUCH_FILE。
func TestCreate_EyeCatchingImageForeignFile(t *testing.T) {
	h, _, _ := newHandler(t)
	dr := testutil.NewMockDriveFileRepository()
	other := "bob"
	dr.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other}
	h.SetDriveFileRepo(dr)
	c, rec := newReq(t, `{"title":"t","name":"alpha","eyeCatchingImageId":"f1","content":[],"variables":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 自分の drive file なら作成成功。
func TestCreate_EyeCatchingImageOwned(t *testing.T) {
	h, _, _ := newHandler(t)
	dr := testutil.NewMockDriveFileRepository()
	owner := "alice"
	dr.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner}
	h.SetDriveFileRepo(dr)
	c, rec := newReq(t, `{"title":"t","name":"alpha","eyeCatchingImageId":"f1","content":[],"variables":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1548: update でも eyeCatchingImageId の所有検証が効く。
func TestUpdate_EyeCatchingImageNoSuchFile(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha"}
	h.SetDriveFileRepo(testutil.NewMockDriveFileRepository())
	c, rec := newReq(t, `{"pageId":"p1","eyeCatchingImageId":"f_ghost"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "cfc23c7c-3887-490e-af30-0ed576703c82")
}

func TestCreate_TitleRequired(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_NameRequired(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"title":"t"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_NameConflict(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["existing"] = &model.Page{ID: "existing", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{"title":"t","name":"alpha","content":[],"variables":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NAME_ALREADY_EXISTS")
}

// failingPageRepo causes Create to fail.
type failingPageRepo struct {
	*testutil.MockPageRepository
}

func (r *failingPageRepo) Create(_ *model.Page) error { return errors.New("boom") }

func TestCreate_RepoError(t *testing.T) {
	mock := testutil.NewMockPageRepository()
	repo := &failingPageRepo{MockPageRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, testutil.NewMockPageLikeRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"title":"t","name":"alpha","content":[],"variables":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Show ------------------------------------------------------------------

func TestShow_ByID(t *testing.T) {
	h, repo, _ := newHandler(t)
	// 実ハンドラでは aidx 由来の createdAt が付与されるため、
	// テストでも有効な aidx ID を使って PackPage の createdAt パスを検証する。
	idGen, _ := id.NewGenerator("aidx")
	pageID := idGen.Generate(time.Now())
	repo.Pages[pageID] = &model.Page{ID: pageID, UserID: "alice", Visibility: model.PageVisibilityPublic}
	c, rec := newReq(t, `{"pageId":"`+pageID+`"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"createdAt":`)
}

func TestShow_ByName(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic}
	c, rec := newReq(t, `{"userId":"alice","name":"alpha"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestShow_IncludesUserAndIsLiked guards #1134 for the single-page path:
// owner が attach され、viewer logged-in 時は isLiked field も含まれる。
func TestShow_IncludesUserAndIsLiked(t *testing.T) {
	h, repo, likeRepo := newHandler(t)
	// L3 (#1270): golden Page は createdAt を必須とするため、実ハンドラと同じく
	// aidx 由来の ID を使って ParseTime 経由の createdAt 付与を成立させる。
	idGen, _ := id.NewGenerator("aidx")
	pageID := idGen.Generate(time.Now())
	repo.Pages[pageID] = &model.Page{ID: pageID, UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic}
	require.NoError(t, likeRepo.Create(&model.PageLike{ID: "l1", UserID: "bob", PageID: pageID}))
	h.SetUserSource(&stubUserSource{
		bundle: &coreuser.UserWithProfile{
			User: &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"},
		},
	})
	c, rec := newReq(t, `{"pageId":"`+pageID+`"}`)
	setUser(c, "bob")
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var row map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &row))
	user, ok := row["user"].(map[string]any)
	require.True(t, ok, "show response must include user field (#1134)")
	assert.Equal(t, "alice", user["username"])
	assert.Equal(t, true, row["isLiked"], "isLiked must reflect viewer's like state (#1134)")
	shapetest.Assert(t, "Page", row) // L3 (#1270)
}

func TestShow_ByUsername(t *testing.T) {
	// #955: pages/show が {username, name} 経路も accept する。upstream
	// frontend (page.vue) はこの shape を投げるので drop-in 互換に必須。
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic}
	h.SetUserSource(&stubUserSource{
		byUsernameBundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice"}},
	})
	c, rec := newReq(t, `{"username":"alice","name":"alpha"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestShow_ByUsername_UserNotFound(t *testing.T) {
	// username が存在しない場合は 404 (= NO_SUCH_PAGE と同じ shape) を
	// 返す。upstream は user 不在も page 不在もまとめて noSuchPage に
	// 集約する設計。
	h, _, _ := newHandler(t)
	h.SetUserSource(&stubUserSource{
		byUsernameErr: errors.New("user not found"),
	})
	c, rec := newReq(t, `{"username":"ghost","name":"alpha"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_ByUsername_NoLookupConfigured(t *testing.T) {
	// userSource 未注入の場合、{username, name} 経路は internal error
	// (= 通常配線では起こらないが defense-in-depth)。
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"username":"alice","name":"alpha"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestShow_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_MissingParams(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"pageId":"missing"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 非 public page を非所有者が show すると、存在ごと隠して 404 NO_SUCH_PAGE を
// 返すこと (#1432)。upstream TS pages/show は可視性ゲートを持たず noSuchPage
// のみ返すため shape が一致し、private page の存在を 403 で露呈しない。
func TestShow_AccessDenied(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Visibility: model.PageVisibilityFollowers}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_PAGE", errObj["code"])
}

// 存在隠蔽は経路非依存であることを固定する (#1432 review)。{userId, name} 経路
// (ShowByName) でも非public・非所有者には 404 NO_SUCH_PAGE を返す。
func TestShow_AccessDenied_ByName(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Name: "alpha", Visibility: model.PageVisibilityFollowers}
	c, rec := newReq(t, `{"userId":"owner","name":"alpha"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_PAGE")
}

// {username, name} 経路 (#955) でも非public・非所有者には 404 NO_SUCH_PAGE。
func TestShow_AccessDenied_ByUsername(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityFollowers}
	h.SetUserSource(&stubUserSource{
		byUsernameBundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice"}},
	})
	c, rec := newReq(t, `{"username":"alice","name":"alpha"}`)
	setUser(c, "bob")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_PAGE")
}

// guest (未認証) viewer が非 public page を引いた場合も 404 NO_SUCH_PAGE で
// 存在隠蔽されること (#1435)。handler は user==nil 時に requesterID="" を
// service へ渡すので、authenticated non-owner と同じ ErrAccessDenied 経路に
// 落ち、404 化フォールスルーで存在を隠す。guest probe は最も悪用されやすい
// 経路なので明示的に固定する。
func TestShow_AccessDenied_Guest(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Visibility: model.PageVisibilityFollowers}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	// setUser is intentionally NOT called — middleware.GetUser returns nil.
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_PAGE")
}

// --- Update ----------------------------------------------------------------

func TestUpdate_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Title: "t"}
	c, rec := newReq(t, `{"pageId":"p1","title":"t2","summary":"s","content":[{"x":1}],"variables":[1],"eyeCatchingImageId":"img"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"pageId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_AccessDenied(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner"}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_TitleEmpty(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice"}
	c, rec := newReq(t, `{"pageId":"p1","title":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_NameConflict(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha"}
	repo.Pages["p2"] = &model.Page{ID: "p2", UserID: "alice", Name: "beta"}
	c, rec := newReq(t, `{"pageId":"p1","name":"beta"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NAME_ALREADY_EXISTS")
}

// failingUpdateRepo causes UpdateFields to fail.
type failingUpdateRepo struct {
	*testutil.MockPageRepository
}

func (r *failingUpdateRepo) UpdateFields(_ string, _ map[string]any) error {
	return errors.New("boom")
}

func TestUpdate_RepoError(t *testing.T) {
	mock := testutil.NewMockPageRepository()
	mock.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice"}
	repo := &failingUpdateRepo{MockPageRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, testutil.NewMockPageLikeRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"pageId":"p1","title":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Delete ----------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice"}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestDelete_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"pageId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_AccessDenied(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner"}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingDeleteRepo causes Delete to fail.
type failingDeleteRepo struct {
	*testutil.MockPageRepository
}

func (r *failingDeleteRepo) Delete(_ *model.Page) error { return errors.New("boom") }

func TestDelete_RepoError(t *testing.T) {
	mock := testutil.NewMockPageRepository()
	mock.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice"}
	repo := &failingDeleteRepo{MockPageRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, testutil.NewMockPageLikeRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- My ---------------------------------------------------------------------

func TestMy_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.My(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha")
}

// TestMy_IncludesUserField guards #1134: frontend MkPagePreview の
// `page.user.username` template が unconditional 参照なので、`user` field が
// 必ず entity に含まれていることを検証する。
func TestMy_IncludesUserField(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic}
	username := "alice"
	c, rec := newReq(t, `{}`)
	c.Set(string(middleware.UserContextKey), &model.User{ID: "alice", Username: username, UsernameLower: username})
	require.NoError(t, h.My(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	user, ok := rows[0]["user"].(map[string]any)
	require.True(t, ok, "page entity must include user field (#1134)")
	assert.Equal(t, "alice", user["username"])
}

func TestMy_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.My(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// listFailRepo causes ListByUser to fail.
type listFailRepo struct {
	*testutil.MockPageRepository
}

func (r *listFailRepo) ListByUser(_, _, _ string, _, _ int) ([]*model.Page, error) {
	return nil, errors.New("boom")
}

func TestMy_RepoError(t *testing.T) {
	repo := &listFailRepo{MockPageRepository: testutil.NewMockPageRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, testutil.NewMockPageLikeRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.My(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Featured --------------------------------------------------------------

func TestFeatured_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic, LikedCount: 1}
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestFeatured_IncludesUserField guards #1134 for the batch-owner path:
// userSource を wire したとき FindManyByIDs で owner が解決され、各 row に
// `user.username` が attach されることを確認。
func TestFeatured_IncludesUserField(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic}
	repo.Pages["p2"] = &model.Page{ID: "p2", UserID: "bob", Name: "beta", Visibility: model.PageVisibilityPublic}
	h.SetUserSource(&stubUserSource{
		manyUsers: []*model.User{
			{ID: "alice", Username: "alice", UsernameLower: "alice"},
			{ID: "bob", Username: "bob", UsernameLower: "bob"},
		},
	})
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	for _, row := range rows {
		user, ok := row["user"].(map[string]any)
		require.True(t, ok, "featured row must include user field (#1134)")
		assert.NotEmpty(t, user["username"])
	}
}

// #1773: pages/featured は upstream packMany(pages, me) に合わせ、認証 viewer に
// 各 row の isLiked を埋める (i/pages・users/pages は me 無しで omit)。
func TestFeatured_IncludesIsLikedForViewer(t *testing.T) {
	h, repo, likeRepo := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "liked", Visibility: model.PageVisibilityPublic}
	repo.Pages["p2"] = &model.Page{ID: "p2", UserID: "alice", Name: "notliked", Visibility: model.PageVisibilityPublic}
	require.NoError(t, likeRepo.Create(&model.PageLike{ID: "l1", UserID: "bob", PageID: "p1"}))
	h.SetUserSource(&stubUserSource{
		manyUsers: []*model.User{{ID: "alice", Username: "alice", UsernameLower: "alice"}},
	})
	c, rec := newReq(t, `{}`)
	setUser(c, "bob")
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	byID := map[string]map[string]any{}
	for _, r := range rows {
		byID[r["id"].(string)] = r
	}
	require.NotNil(t, byID["p1"])
	require.NotNil(t, byID["p2"])
	assert.Equal(t, true, byID["p1"]["isLiked"])
	assert.Equal(t, false, byID["p2"]["isLiked"])
}

// #1773: 未認証 viewer では isLiked を omit する (upstream meId ? ... : undefined)。
func TestFeatured_OmitsIsLikedForAnon(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic}
	h.SetUserSource(&stubUserSource{
		manyUsers: []*model.User{{ID: "alice", Username: "alice", UsernameLower: "alice"}},
	})
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Featured(c)) // no user
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	_, has := rows[0]["isLiked"]
	assert.False(t, has, "anonymous viewer must not get isLiked (#1773)")
}

// TestFeatured_DropsRowsWithoutOwner guards the fail-soft path: when
// userSource 未配線 (or batch fetch error) で owner が解決できない行は
// pagesToList で drop される (= frontend が crash する代わりに silent skip)。
func TestFeatured_DropsRowsWithoutOwner(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "ghost", Name: "alpha", Visibility: model.PageVisibilityPublic}
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Empty(t, rows, "rows without owner must be dropped, not emitted with missing user field")
}

// TestFeatured_IgnoresBadJSON: upstream pages/featured は paramDef 空で body を
// 読まないため、不正な JSON body でも 400 ではなく通常の featured 一覧を返す (#1830)。
func TestFeatured_IgnoresBadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// featuredSpyRepo records the args ListFeatured was called with.
type featuredSpyRepo struct {
	*testutil.MockPageRepository
	gotSince, gotUntil  string
	gotLimit, gotOffset int
}

func (r *featuredSpyRepo) ListFeatured(since, until string, limit, offset int) ([]*model.Page, error) {
	r.gotSince, r.gotUntil, r.gotLimit, r.gotOffset = since, until, limit, offset
	return r.MockPageRepository.ListFeatured(since, until, limit, offset)
}

// TestFeatured_IgnoresRequestParams: limit/offset/sinceId/untilId を body で
// 渡しても無視し、常に固定 10 件 / カーソル無しで問い合わせる (#1830)。
func TestFeatured_IgnoresRequestParams(t *testing.T) {
	repo := &featuredSpyRepo{MockPageRepository: testutil.NewMockPageRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, testutil.NewMockPageLikeRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"limit":3,"offset":5,"sinceId":"aaa","untilId":"zzz"}`)
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "", repo.gotSince, "sinceId は無視される")
	assert.Equal(t, "", repo.gotUntil, "untilId は無視される")
	assert.Equal(t, 10, repo.gotLimit, "limit は固定 10")
	assert.Equal(t, 0, repo.gotOffset, "offset は無視される")
}

// featuredFailRepo causes ListFeatured to fail.
type featuredFailRepo struct {
	*testutil.MockPageRepository
}

func (r *featuredFailRepo) ListFeatured(_, _ string, _, _ int) ([]*model.Page, error) {
	return nil, errors.New("boom")
}

func TestFeatured_RepoError(t *testing.T) {
	repo := &featuredFailRepo{MockPageRepository: testutil.NewMockPageRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, testutil.NewMockPageLikeRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Like ------------------------------------------------------------------

func TestLike_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPublic}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Like(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestLike_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "bob")
	require.NoError(t, h.Like(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestLike_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"pageId":"missing"}`)
	setUser(c, "bob")
	require.NoError(t, h.Like(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 非 public page を非所有者が like しようとすると、存在ごと隠して 404
// NO_SUCH_PAGE を返すこと (#1435)。upstream TS pages/like は accessDenied を
// 宣言しておらず、mk-go の可視性ゲート (drop-in 都合で残置) も外部からは
// noSuchPage に集約して 403 で存在露呈しない。
func TestLike_AccessDenied(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityFollowers}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Like(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_PAGE", errObj["code"])
	assert.Equal(t, "cc98a8a2-0dc3-4123-b198-62c71df18ed3", errObj["id"])
}

func TestLike_AlreadyLiked(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPublic}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Like(c))
	c2, rec2 := newReq(t, `{"pageId":"p1"}`)
	setUser(c2, "bob")
	require.NoError(t, h.Like(c2))
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
	assert.Contains(t, rec2.Body.String(), "ALREADY_LIKED")
	_ = rec
}

// #1438: owner が自分の page を like しようとすると 400 YOUR_PAGE
// (28800466-...) を返す (upstream TS pages/like 準拠)。
func TestLike_YourPage(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPublic}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Like(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "YOUR_PAGE", errObj["code"])
	assert.Equal(t, "28800466-e6db-40f2-8fae-bf9e82aa92b8", errObj["id"])
}

// failingCreateLikeRepo causes Create to fail.
type failingCreateLikeRepo struct {
	*testutil.MockPageLikeRepository
}

func (r *failingCreateLikeRepo) Create(_ *model.PageLike) error { return errors.New("boom") }

func TestLike_RepoError(t *testing.T) {
	repo := testutil.NewMockPageRepository()
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPublic}
	likeRepo := &failingCreateLikeRepo{MockPageLikeRepository: testutil.NewMockPageLikeRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, likeRepo, idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Like(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Unlike ----------------------------------------------------------------

func TestUnlike_Success(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPublic}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Like(c))
	c2, rec2 := newReq(t, `{"pageId":"p1"}`)
	setUser(c2, "bob")
	require.NoError(t, h.Unlike(c2))
	assert.Equal(t, http.StatusNoContent, rec2.Code)
	_ = rec
}

func TestUnlike_BadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "bob")
	require.NoError(t, h.Unlike(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnlike_NotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"pageId":"missing"}`)
	setUser(c, "bob")
	require.NoError(t, h.Unlike(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnlike_NotLiked(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPublic}
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Unlike(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_LIKED")
}

// failingDeleteLikeRepo causes Delete to fail.
type failingDeleteLikeRepo struct {
	*testutil.MockPageLikeRepository
}

func (r *failingDeleteLikeRepo) Delete(_ *model.PageLike) error { return errors.New("boom") }

func TestUnlike_RepoError(t *testing.T) {
	repo := testutil.NewMockPageRepository()
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Visibility: model.PageVisibilityPublic}
	mock := testutil.NewMockPageLikeRepository()
	mock.Likes["pl1"] = &model.PageLike{ID: "pl1", UserID: "bob", PageID: "p1"}
	likeRepo := &failingDeleteLikeRepo{MockPageLikeRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corepage.NewService(repo, likeRepo, idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"pageId":"p1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Unlike(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- PagePush --------------------------------------------------------------

type stubMainStreamPublisher struct {
	calls []mainEventCall
}

type mainEventCall struct {
	userID    string
	eventType string
	body      any
}

func (s *stubMainStreamPublisher) PublishMainEvent(userID, eventType string, body any) {
	s.calls = append(s.calls, mainEventCall{userID, eventType, body})
}

type stubUserSource struct {
	bundle           *coreuser.UserWithProfile
	err              error
	byUsernameBundle *coreuser.UserWithProfile
	byUsernameErr    error
	manyUsers        []*model.User
	manyErr          error
}

func (s *stubUserSource) ShowByID(_ string) (*coreuser.UserWithProfile, error) {
	return s.bundle, s.err
}

func (s *stubUserSource) ShowByUsername(_ string, _ *string) (*coreuser.UserWithProfile, error) {
	// byUsernameBundle / byUsernameErr が個別にセットされていれば優先、
	// 無ければ ShowByID と同じ bundle を返す (= 既存の page-push test での
	// stub 動作を維持)。
	if s.byUsernameBundle != nil || s.byUsernameErr != nil {
		return s.byUsernameBundle, s.byUsernameErr
	}
	return s.bundle, s.err
}

func (s *stubUserSource) FindManyByIDs(_ []string) ([]*model.User, error) {
	return s.manyUsers, s.manyErr
}

func TestPagePush_PublishesPageEvent(t *testing.T) {
	h, repo, _ := newHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	h.SetUserSource(&stubUserSource{
		bundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice"}},
	})
	// public pageでh.svc.FindByIDがsuccess扱い。
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Name: "test", Visibility: model.PageVisibilityPublic}

	c, rec := newReq(t, `{"pageId":"p1","event":"click","var":{"x":1}}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "owner", pub.calls[0].userID)
	assert.Equal(t, "pageEvent", pub.calls[0].eventType)
	body, ok := pub.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "p1", body["pageId"])
	assert.Equal(t, "click", body["event"])
	assert.Equal(t, "alice", body["userId"])
	// varはjson.RawMessageとして透過(non-nilであること確認)
	assert.NotNil(t, body["var"])
}

func TestPagePush_PageNotFound(t *testing.T) {
	h, _, _ := newHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	h.SetUserSource(&stubUserSource{bundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice"}}})

	c, rec := newReq(t, `{"pageId":"ghost","event":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, pub.calls)
}

func TestPagePush_InvalidParam(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"pageId":""}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPagePush_NoPublisher_NoEmit(t *testing.T) {
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Visibility: model.PageVisibilityPublic}
	// publisher / userSource未設定でも204を返すこと
	c, rec := newReq(t, `{"pageId":"p1","event":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestPagePush_UserSourceError_NoEmit(t *testing.T) {
	h, repo, _ := newHandler(t)
	pub := &stubMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	h.SetUserSource(&stubUserSource{err: errors.New("boom")})
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "owner", Visibility: model.PageVisibilityPublic}

	c, rec := newReq(t, `{"pageId":"p1","event":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.PagePush(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, pub.calls)
}

// #1662 page content の image block から attachedFiles を、eyeCatchingImageId から
// eyeCatchingImage を解決する。attachedFiles は page owner-scope。
func TestShow_ResolvesAttachedFilesAndEyeCatchingImage(t *testing.T) {
	h, repo, _ := newHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	pageID := idGen.Generate(time.Now())
	// content: image block (f1, owner alice) + section children に image block
	// (f2, owner alice) + 他人 file (f3, owner bob → 除外) を含む。
	content := `[
		{"type":"image","fileId":"f1"},
		{"type":"text","text":"hi"},
		{"type":"section","children":[{"type":"image","fileId":"f2"},{"type":"image","fileId":"f3"}]}
	]`
	eyeID := "eye1"
	repo.Pages[pageID] = &model.Page{
		ID: pageID, UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic,
		Content: []byte(content), EyeCatchingImageID: &eyeID,
	}

	driveRepo := testutil.NewMockDriveFileRepository()
	alice, bob := "alice", "bob"
	driveRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &alice, Name: "a.png", Type: "image/png"}
	driveRepo.Files["f2"] = &model.DriveFile{ID: "f2", UserID: &alice, Name: "b.png", Type: "image/png"}
	driveRepo.Files["f3"] = &model.DriveFile{ID: "f3", UserID: &bob, Name: "c.png", Type: "image/png"} // owner mismatch
	driveRepo.Files["eye1"] = &model.DriveFile{ID: "eye1", UserID: &alice, Name: "eye.png", Type: "image/png"}
	h.SetDriveFileRepo(driveRepo)
	h.SetUserSource(&stubUserSource{bundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}}})

	c, rec := newReq(t, `{"pageId":"`+pageID+`"}`)
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var row map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &row))

	attached, ok := row["attachedFiles"].([]any)
	require.True(t, ok)
	// f1 + f2 (alice owned)、content 順。f3 (bob) は owner mismatch で除外。
	require.Len(t, attached, 2)
	assert.Equal(t, "f1", attached[0].(map[string]any)["id"])
	assert.Equal(t, "f2", attached[1].(map[string]any)["id"])

	eye, ok := row["eyeCatchingImage"].(map[string]any)
	require.True(t, ok, "eyeCatchingImage が解決される")
	assert.Equal(t, "eye1", eye["id"])
}

// driveFileRepo 未配線なら attachedFiles は空配列 / eyeCatchingImage は null (default 維持)。
func TestShow_NoDriveRepoKeepsDefaults(t *testing.T) {
	h, repo, _ := newHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	pageID := idGen.Generate(time.Now())
	eyeID := "eye1"
	repo.Pages[pageID] = &model.Page{
		ID: pageID, UserID: "alice", Name: "alpha", Visibility: model.PageVisibilityPublic,
		Content: []byte(`[{"type":"image","fileId":"f1"}]`), EyeCatchingImageID: &eyeID,
	}
	h.SetUserSource(&stubUserSource{bundle: &coreuser.UserWithProfile{User: &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}}})

	c, rec := newReq(t, `{"pageId":"`+pageID+`"}`)
	require.NoError(t, h.Show(c))
	var row map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &row))
	attached, ok := row["attachedFiles"].([]any)
	require.True(t, ok)
	assert.Empty(t, attached, "未配線なら空配列")
	assert.Nil(t, row["eyeCatchingImage"], "未配線なら null")
}

// #2027: content/variables 欠落は 400。
func TestCreate_ContentVariablesRequired(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"title":"t","name":"alpha","variables":[]}`)
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "content 欠落は 400 (#2027)")
}

// --- jsonb 列に入らない値 (#3037) ---

// `content` / `variables` は jsonb 列へそのまま入る。PostgreSQL は NUL
// エスケープを SQLSTATE 22P05 でクエリごと落とすので、引く前に弾かないと
// **任意の認証ユーザーが 500 を起こせる**。
func TestCreate_UnstorableJSONBIs400(t *testing.T) {
	esc := `\u0000`
	for name, body := range map[string]string{
		"content の値":   `{"title":"t","name":"alpha","content":[{"x":"a` + esc + `b"}],"variables":[]}`,
		"content のキー":  `{"title":"t","name":"alpha","content":[{"a` + esc + `b":"x"}],"variables":[]}`,
		"variables の値": `{"title":"t","name":"alpha","content":[],"variables":[{"v":"a` + esc + `b"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, repo, _ := newHandler(t)
			c, rec := newReq(t, body)
			setUser(c, "alice")
			require.NoError(t, h.Create(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code, "列に入らない jsonb を受け入れている")
			assert.Empty(t, repo.Pages)
		})
	}
}

func TestUpdate_UnstorableJSONBIs400(t *testing.T) {
	esc := `\u0000`
	h, repo, _ := newHandler(t)
	repo.Pages["p1"] = &model.Page{ID: "p1", UserID: "alice", Name: "alpha"}

	c, rec := newReq(t, `{"pageId":"p1","content":[{"x":"a`+esc+`b"}]}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "列に入らない jsonb を受け入れている")
}

// **普通の JSON は通ったまま。** これが無いと「常に拒否する」実装でも上が通る。
// バックスラッシュ自体がエスケープされた形 (ただの 6 文字) も通ること。
func TestCreate_OrdinaryJSONBStillPasses(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newReq(t, `{"title":"t","name":"alpha","content":[{"x":"a\\u0000b"}],"variables":[{"あ":"絵文字"}]}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}
