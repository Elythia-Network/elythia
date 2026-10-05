package channels

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corechannel "github.com/elythia-network/elythia/internal/core/channel"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newHandler(t *testing.T) (
	*Handler,
	*testutil.MockChannelRepository,
	*testutil.MockChannelFollowingRepository,
	*testutil.MockNoteRepository,
) {
	t.Helper()
	repo := testutil.NewMockChannelRepository()
	followRepo := testutil.NewMockChannelFollowingRepository()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, followRepo, noteRepo, idGen)
	return NewHandler(svc, idGen), repo, followRepo, noteRepo
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
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha","color":"#abcdef"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "Channel", resp) // L3 (#1280)
}

func TestCreate_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_BannerIDPersisted(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	dr := testutil.NewMockDriveFileRepository()
	owner := "alice"
	require.NoError(t, dr.Create(&model.DriveFile{ID: "b1", UserID: &owner, Type: "image/png", URL: "https://x/b.png"}))
	h.SetDriveFileRepo(dr)

	c, rec := newReq(t, `{"name":"ch","color":"#abcdef","bannerId":"b1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code)
	// 永続化された channel に bannerId が入る。
	var found *model.Channel
	for _, ch := range repo.Channels {
		found = ch
	}
	require.NotNil(t, found)
	require.NotNil(t, found.BannerID)
	assert.Equal(t, "b1", *found.BannerID)
}

func TestCreate_ForeignBannerRejected(t *testing.T) {
	h, _, _, _ := newHandler(t)
	dr := testutil.NewMockDriveFileRepository()
	other := "bob"
	require.NoError(t, dr.Create(&model.DriveFile{ID: "b1", UserID: &other, Type: "image/png", URL: "https://x/b.png"}))
	h.SetDriveFileRepo(dr)

	c, rec := newReq(t, `{"name":"ch","color":"#abcdef","bannerId":"b1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
}

func TestCreate_NameRequired(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// canCreateChannel role policy gate は #1020 で middleware.RequireRolePolicy に
// 昇格したため、本 handler test は policy gate を持たない (= 旧 stubRoleChecker /
// TestCreate_RolePolicyAllowed / Denied / NoRoleCheckerSkipsGate は middleware
// 側の TestRequireRolePolicy_* に移管した)。

// failingChannelRepo causes Create to fail.
type failingChannelRepo struct {
	*testutil.MockChannelRepository
}

func (r *failingChannelRepo) Create(_ *model.Channel) error { return errors.New("boom") }

func TestCreate_RepoError(t *testing.T) {
	mock := testutil.NewMockChannelRepository()
	repo := &failingChannelRepo{MockChannelRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, testutil.NewMockChannelFollowingRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)

	c, rec := newReq(t, `{"name":"alpha"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Show ------------------------------------------------------------------

func TestShow_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha"}
	c, rec := newReq(t, `{"channelId":"c1"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestShow_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_EmptyID(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"channelId":"missing"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1540: show は detailed=true 相当で pinnedNotes (展開済 Note[]) を pinnedNoteIds 順で返す。
func TestShow_DetailedPinnedNotes(t *testing.T) {
	h, repo, _, noteRepo := newHandler(t)
	h.SetPinnedNoteRepo(noteRepo)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	// shapetest の createdAt (aidx 派生) のため channel id は valid aidx を使う。
	idGen, _ := id.NewGenerator("aidx")
	cid := idGen.Generate(time.Now())
	repo.Channels[cid] = &model.Channel{ID: cid, Name: "alpha", PinnedNoteIDs: []string{"n2", "n1"}}
	c, rec := newReq(t, `{"channelId":"`+cid+`"}`)
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	pn, ok := resp["pinnedNotes"].([]any)
	require.True(t, ok, "pinnedNotes must be present")
	require.Len(t, pn, 2)
	// pinnedNoteIds の順 (n2, n1) を保つ
	assert.Equal(t, "n2", pn[0].(map[string]any)["id"])
	assert.Equal(t, "n1", pn[1].(map[string]any)["id"])
	shapetest.Assert(t, "Channel", resp) // L3: detailed Channel (pinnedNotes 込み)
}

// pinnedNotes は detailed では常に存在し、ピン無しでも空配列を返す。
func TestShow_PinnedNotesEmptyArray(t *testing.T) {
	h, repo, _, noteRepo := newHandler(t)
	h.SetPinnedNoteRepo(noteRepo)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha"}
	c, rec := newReq(t, `{"channelId":"c1"}`)
	require.NoError(t, h.Show(c))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	pn, ok := resp["pinnedNotes"].([]any)
	require.True(t, ok)
	assert.Empty(t, pn)
}

// #1540: viewer がある時 hasUnreadNote:false を必ず付ける (後方互換)。
func TestShow_HasUnreadNoteWhenViewer(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha"}
	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	val, ok := resp["hasUnreadNote"]
	require.True(t, ok, "hasUnreadNote must be present when viewer exists")
	assert.Equal(t, false, val)
}

// viewer が無い時は hasUnreadNote を出さない (upstream は me 無しで省略)。
func TestShow_NoHasUnreadNoteWithoutViewer(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha"}
	c, rec := newReq(t, `{"channelId":"c1"}`)
	require.NoError(t, h.Show(c))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, ok := resp["hasUnreadNote"]
	assert.False(t, ok, "hasUnreadNote must be absent without viewer")
}

// --- Update ----------------------------------------------------------------

func TestUpdate_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner}
	c, rec := newReq(t, `{"channelId":"c1","name":"alpha-v2"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1948-15: description の absent/null/value を区別する。
//
//	absent → 維持 / null → NULL clear / "x" → 設定 (upstream は !== undefined で spread)。
func TestUpdate_DescriptionNullAbsentValue(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	old := "old desc"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner, Description: &old}

	// absent → 維持。
	c, rec := newReq(t, `{"channelId":"c1","name":"a2"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, repo.Channels["c1"].Description, "description 省略は維持 (#1948-15)")
	assert.Equal(t, "old desc", *repo.Channels["c1"].Description)

	// null → NULL clear。
	c, rec = newReq(t, `{"channelId":"c1","description":null}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, repo.Channels["c1"].Description, "description:null は NULL clear (#1948-15)")

	// "new" → 設定。
	c, rec = newReq(t, `{"channelId":"c1","description":"new"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, repo.Channels["c1"].Description)
	assert.Equal(t, "new", *repo.Channels["c1"].Description)
}

func TestUpdate_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"channelId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_AccessDenied(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	other := "bob"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &other}
	c, rec := newReq(t, `{"channelId":"c1","name":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_NameEmpty(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	c, rec := newReq(t, `{"channelId":"c1","name":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_DescriptionUpdated(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner}
	c, rec := newReq(t, `{"channelId":"c1","description":"new"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCreate_AllowRenoteToExternalFalse(t *testing.T) {
	// channels/create で allowRenoteToExternal:false を渡すと保存され、
	// response にも false で出る (#1540)。
	h, repo, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha","allowRenoteToExternal":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["allowRenoteToExternal"])
	for _, ch := range repo.Channels {
		assert.False(t, ch.AllowRenoteToExternal)
	}
}

func TestCreate_AllowRenoteToExternalDefaultsTrue(t *testing.T) {
	// 未指定なら upstream `?? true` 既定で true。
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"alpha"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["allowRenoteToExternal"])
}

func TestUpdate_AllowRenoteToExternal(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner, AllowRenoteToExternal: true}
	c, rec := newReq(t, `{"channelId":"c1","allowRenoteToExternal":false}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, repo.Channels["c1"].AllowRenoteToExternal)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["allowRenoteToExternal"])
}

// stubModerator is a corechannel.ModeratorChecker test double.
type stubModerator struct {
	moderators map[string]bool
}

func (s stubModerator) IsModerator(userID string) bool { return s.moderators[userID] }

// newModeratorHandler builds a handler whose service treats the given user IDs
// as moderators (channels/update bypass, #1540).
func newModeratorHandler(t *testing.T, moderators ...string) (*Handler, *testutil.MockChannelRepository) {
	t.Helper()
	repo := testutil.NewMockChannelRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, testutil.NewMockChannelFollowingRepository(), testutil.NewMockNoteRepository(), idGen)
	mods := make(map[string]bool, len(moderators))
	for _, m := range moderators {
		mods[m] = true
	}
	svc.SetModeratorChecker(stubModerator{moderators: mods})
	return NewHandler(svc, idGen), repo
}

func TestUpdate_ModeratorBypass(t *testing.T) {
	// owner でない moderator が他人の channel を編集できる (#1540)。
	h, repo := newModeratorHandler(t, "mod")
	owner := "bob"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner}
	c, rec := newReq(t, `{"channelId":"c1","name":"edited"}`)
	setUser(c, "mod")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "edited", repo.Channels["c1"].Name)
}

func TestUpdate_NonModeratorNonOwnerForbidden(t *testing.T) {
	// fail-closed: moderator でも owner でもなければ 403。
	h, repo := newModeratorHandler(t, "mod")
	owner := "bob"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner}
	c, rec := newReq(t, `{"channelId":"c1","name":"x"}`)
	setUser(c, "intruder")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingUpdateRepo causes UpdateFields to fail to exercise the internalError
// branch.
type failingUpdateRepo struct {
	*testutil.MockChannelRepository
}

func (r *failingUpdateRepo) UpdateFields(_ string, _ map[string]any) error {
	return errors.New("boom")
}

func TestUpdate_RepoError(t *testing.T) {
	mock := testutil.NewMockChannelRepository()
	owner := "alice"
	mock.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	repo := &failingUpdateRepo{MockChannelRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, testutil.NewMockChannelFollowingRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"channelId":"c1","name":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Follow / Unfollow -----------------------------------------------------

func TestFollow_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Follow(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFollow_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Follow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFollow_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"channelId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Follow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFollow_AlreadyFollowing(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Follow(c))
	c2, rec2 := newReq(t, `{"channelId":"c1"}`)
	setUser(c2, "alice")
	require.NoError(t, h.Follow(c2))
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
	assert.Contains(t, rec2.Body.String(), "ALREADY_FOLLOWING")
	// upstream 2026.7.0 (#17802) の canonical id。mk-go は先行実装していたが
	// 独自 id だったため揃えた。
	assert.Contains(t, rec2.Body.String(), "7db31665-651e-40c1-8e6e-28e9ad829a2d")
	_ = rec
}

// failingFollowRepo causes Follow.Create to fail (other than already
// following) to exercise internalError branch.
type failingFollowRepo struct {
	*testutil.MockChannelFollowingRepository
}

func (r *failingFollowRepo) Create(_ *model.ChannelFollowing) error { return errors.New("boom") }

func TestFollow_InternalError(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	followRepo := &failingFollowRepo{MockChannelFollowingRepository: testutil.NewMockChannelFollowingRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)

	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Follow(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUnfollow_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Follow(c))
	_ = rec
	c2, rec2 := newReq(t, `{"channelId":"c1"}`)
	setUser(c2, "alice")
	require.NoError(t, h.Unfollow(c2))
	assert.Equal(t, http.StatusNoContent, rec2.Code)
}

func TestUnfollow_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Unfollow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnfollow_NotFollowing(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Unfollow(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingUnfollowRepo causes Delete to fail (other than not following).
type failingUnfollowRepo struct {
	*testutil.MockChannelFollowingRepository
}

func (r *failingUnfollowRepo) Delete(_ *model.ChannelFollowing) error { return errors.New("boom") }

func TestUnfollow_InternalError(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	mock := testutil.NewMockChannelFollowingRepository()
	mock.Followings["f1"] = &model.ChannelFollowing{ID: "f1", FollowerID: "alice", FolloweeID: "c1"}
	followRepo := &failingUnfollowRepo{MockChannelFollowingRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)

	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Unfollow(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Listing ---------------------------------------------------------------

func TestFollowed_Success(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Followed(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowed_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Followed(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// listFailFollowRepo: ListFollowed returns an error.
type listFailFollowRepo struct {
	*testutil.MockChannelFollowingRepository
}

func (r *listFailFollowRepo) ListFollowed(_, _, _ string, _, _ int) ([]*model.ChannelFollowing, error) {
	return nil, errors.New("boom")
}

func TestFollowed_RepoError(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	followRepo := &listFailFollowRepo{MockChannelFollowingRepository: testutil.NewMockChannelFollowingRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Followed(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestOwned_Success(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Owned(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestOwned_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Owned(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// listFailChannelRepo: List returns an error.
type listFailChannelRepo struct {
	*testutil.MockChannelRepository
}

func (r *listFailChannelRepo) List(_ model.ChannelListFilter) ([]*model.Channel, error) {
	return nil, errors.New("boom")
}

func TestOwned_RepoError(t *testing.T) {
	mock := testutil.NewMockChannelRepository()
	repo := &listFailChannelRepo{MockChannelRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, testutil.NewMockChannelFollowingRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Owned(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestFeatured_Success(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFeatured_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFeatured_RepoError(t *testing.T) {
	mock := testutil.NewMockChannelRepository()
	repo := &listFailChannelRepo{MockChannelRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, testutil.NewMockChannelFollowingRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{}`)
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestSearch_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha channel"}
	c, rec := newReq(t, `{"query":"alpha"}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha channel")
}

func TestSearch_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// type 省略時 (= nameAndDescription) は description 一致でもヒットする。
func TestSearch_DefaultMatchesDescription(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	desc := "a channel about gophers"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", Description: &desc}
	c, rec := newReq(t, `{"query":"gophers"}`)
	require.NoError(t, h.Search(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha", "description 一致で hit")
}

// type=nameOnly は description 一致を除外する。
func TestSearch_NameOnlyExcludesDescription(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	desc := "a channel about gophers"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", Description: &desc}
	c, rec := newReq(t, `{"query":"gophers","type":"nameOnly"}`)
	require.NoError(t, h.Search(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "alpha", "nameOnly は description を見ない")
}

// enum 外の type は 400 (upstream は ajv enum validation で弾く)。
func TestSearch_InvalidTypeRejected(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"query":"x","type":"bogus"}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// search は archived channel を除外する (upstream は常に isArchived=FALSE)。
func TestSearch_ExcludesArchived(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha-live"}
	repo.Channels["c2"] = &model.Channel{ID: "c2", Name: "alpha-archived", IsArchived: true}
	c, rec := newReq(t, `{"query":"alpha"}`)
	require.NoError(t, h.Search(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha-live")
	assert.NotContains(t, rec.Body.String(), "alpha-archived")
}

// isMuting は list 経路 (search/featured/owned) にも乗る。
func TestSearch_ListIncludesIsMuting(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "muted-ch"}
	muting := testutil.NewMockChannelMutingRepository()
	require.NoError(t, muting.Create(&model.ChannelMuting{ID: "m1", UserID: "alice", ChannelID: "c1"}))
	h.SetMutingRepo(muting)

	c, rec := newReq(t, `{"query":"muted"}`)
	setUser(c, "alice")
	require.NoError(t, h.Search(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, true, resp[0]["isMuting"])
}

// 解除: bannerId:"" で既存 banner が null 化される。
func TestUpdate_BannerClear(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	existing := "b0"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "ch", UserID: &owner, BannerID: &existing}
	c, rec := newReq(t, `{"channelId":"c1","bannerId":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, repo.Channels["c1"].BannerID, `bannerId:"" は banner を解除する`)
}

// Update で他人所有 banner は NO_SUCH_FILE (update 固有 error id)。
func TestUpdate_ForeignBannerRejected(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "ch", UserID: &owner}
	dr := testutil.NewMockDriveFileRepository()
	other := "bob"
	require.NoError(t, dr.Create(&model.DriveFile{ID: "b1", UserID: &other, Type: "image/png", URL: "https://x/b.png"}))
	h.SetDriveFileRepo(dr)
	c, rec := newReq(t, `{"channelId":"c1","bannerId":"b1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "e86c14a4-0da2-4032-8df3-e737a04c7f3b")
}

// bannerResolver 未配線 + bannerId 指定は fail-closed (NO_SUCH_FILE)。
func TestCreate_BannerNoResolverFailsClosed(t *testing.T) {
	h, _, _, _ := newHandler(t) // SetDriveFileRepo を呼ばない
	c, rec := newReq(t, `{"name":"ch","color":"#abcdef","bannerId":"b1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
}

// pinnedNoteIds:[] は空配列として永続化 (nil でない)。
func TestUpdate_PinnedNotesEmpty(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "ch", UserID: &owner, PinnedNoteIDs: []string{"n1"}}
	c, rec := newReq(t, `{"channelId":"c1","pinnedNoteIds":[]}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	got := repo.Channels["c1"]
	assert.NotNil(t, got.PinnedNoteIDs, "[] は nil でなく空配列")
	assert.Len(t, []string(got.PinnedNoteIDs), 0)
}

// isMuting=false ケース (muting 配線済だが対象を mute していない)。
func TestShow_IsMutingFalse(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "ch"}
	h.SetMutingRepo(testutil.NewMockChannelMutingRepository())
	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["isMuting"])
}

// channels/update が bannerId / pinnedNoteIds を反映する。
func TestUpdate_BannerAndPinnedNotes(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	owner := "alice"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "ch", UserID: &owner}
	dr := testutil.NewMockDriveFileRepository()
	require.NoError(t, dr.Create(&model.DriveFile{ID: "b1", UserID: &owner, Type: "image/png", URL: "https://x/b.png"}))
	h.SetDriveFileRepo(dr)

	c, rec := newReq(t, `{"channelId":"c1","bannerId":"b1","pinnedNoteIds":["n1","n2"]}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	got := repo.Channels["c1"]
	require.NotNil(t, got.BannerID)
	assert.Equal(t, "b1", *got.BannerID)
	assert.Equal(t, []string{"n1", "n2"}, []string(got.PinnedNoteIDs))
}

// viewer 視点の isMuting が pack に含まれる。
func TestShow_IncludesIsMuting(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "ch"}
	muting := testutil.NewMockChannelMutingRepository()
	require.NoError(t, muting.Create(&model.ChannelMuting{ID: "m1", UserID: "alice", ChannelID: "c1"}))
	h.SetMutingRepo(muting)

	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isMuting"])
}

func TestSearch_RepoError(t *testing.T) {
	mock := testutil.NewMockChannelRepository()
	repo := &listFailChannelRepo{MockChannelRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, testutil.NewMockChannelFollowingRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"query":"x"}`)
	require.NoError(t, h.Search(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Timeline --------------------------------------------------------------

func TestTimeline_Success(t *testing.T) {
	h, repo, _, noteRepo := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	cid := "c1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", ChannelID: &cid, UserID: "u1"}
	c, rec := newReq(t, `{"channelId":"c1"}`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1790: viewer が muted にしている channel を renote/quote した note は channel TL
// から除外する (当該 channel 自身は muted set から除く)。
func TestTimeline_ExcludesMutedChannelRenote(t *testing.T) {
	h, repo, _, noteRepo := newHandler(t)
	mutRepo := testutil.NewMockChannelMutingRepository()
	h.SetMutingRepo(mutRepo)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	cid := "c1"
	mutedCh := "c_muted"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", ChannelID: &cid, UserID: "u1", Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", ChannelID: &cid, UserID: "u1", Visibility: model.NoteVisibilityPublic, RenoteChannelID: &mutedCh}
	require.NoError(t, mutRepo.Create(&model.ChannelMuting{ID: "m1", UserID: "alice", ChannelID: mutedCh}))

	c, rec := newReq(t, `{"channelId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Timeline(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := make([]string, 0, len(out))
	for _, n := range out {
		ids = append(ids, n["id"].(string))
	}
	assert.ElementsMatch(t, []string{"n1"}, ids, "muted channel を renote した n2 は除外")
}

func TestTimeline_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTimeline_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"channelId":"missing"}`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTimeline_LimitOutOfRangeRejected(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	c, rec := newReq(t, `{"channelId":"c1","limit":9999}`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestTimeline_HidesNonPublicFromOutsiders は public channel に投稿された
// followers / specified note が非フォロワー / non-target viewer / 匿名に
// 露出しないことを handler 層で固定する (#1440 IDOR)。
func TestTimeline_HidesNonPublicFromOutsiders(t *testing.T) {
	h, repo, _, noteRepo := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	cid := "c1"
	noteRepo.Notes["n_pub"] = &model.Note{
		ID: "n_pub", ChannelID: &cid, UserID: "author", Visibility: model.NoteVisibilityPublic,
	}
	noteRepo.Notes["n_fol"] = &model.Note{
		ID: "n_fol", ChannelID: &cid, UserID: "author", Visibility: model.NoteVisibilityFollowers,
	}
	noteRepo.Notes["n_spec"] = &model.Note{
		ID: "n_spec", ChannelID: &cid, UserID: "author",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"allowed"},
	}
	noteRepo.Following["follower"] = []string{"author"}

	cases := []struct {
		name    string
		viewer  string // 空文字 = 匿名
		visible map[string]bool
	}{
		{"anonymous", "", map[string]bool{"n_pub": true}},
		{"non-follower", "stranger", map[string]bool{"n_pub": true}},
		{"non-target specified viewer", "stranger", map[string]bool{"n_pub": true}},
		{"follower", "follower", map[string]bool{"n_pub": true, "n_fol": true}},
		{"specified target", "allowed", map[string]bool{"n_pub": true, "n_spec": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newReq(t, `{"channelId":"c1","limit":50}`)
			if tc.viewer != "" {
				setUser(c, tc.viewer)
			}
			require.NoError(t, h.Timeline(c))
			require.Equal(t, http.StatusOK, rec.Code)
			var rows []map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
			got := make(map[string]bool, len(rows))
			for _, r := range rows {
				got[r["id"].(string)] = true
			}
			assert.Equal(t, tc.visible, got)
		})
	}
}

// failingChannelNoteRepo: ListByChannelID returns an error so Timeline hits the
// internal-error branch (#1440 で service が visibility push-down 版に切替)。
type failingChannelNoteRepo struct {
	*testutil.MockNoteRepository
}

func (r *failingChannelNoteRepo) ListByChannelID(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, errors.New("boom")
}

func TestTimeline_RepoError(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	noteRepo := &failingChannelNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, testutil.NewMockChannelFollowingRepository(), noteRepo, idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"channelId":"c1"}`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- isFollowing / isFavorited embedding (#522) ---------------------------

// countingFollowingRepo wraps MockChannelFollowingRepository to count
// per-row vs batch lookups.
type countingFollowingRepo struct {
	*testutil.MockChannelFollowingRepository
	existsCalls     int
	existsManyCalls int
	existsManySize  int
}

func (c *countingFollowingRepo) Exists(followerID, channelID string) (bool, error) {
	c.existsCalls++
	return c.MockChannelFollowingRepository.Exists(followerID, channelID)
}

func (c *countingFollowingRepo) ExistsMany(followerID string, channelIDs []string) (map[string]bool, error) {
	c.existsManyCalls++
	c.existsManySize += len(channelIDs)
	return c.MockChannelFollowingRepository.ExistsMany(followerID, channelIDs)
}

type countingFavoriteRepo struct {
	*testutil.MockChannelFavoriteRepository
	existsCalls     int
	existsManyCalls int
}

func (c *countingFavoriteRepo) Exists(userID, channelID string) (bool, error) {
	c.existsCalls++
	return c.MockChannelFavoriteRepository.Exists(userID, channelID)
}

func (c *countingFavoriteRepo) ExistsMany(userID string, channelIDs []string) (map[string]bool, error) {
	c.existsManyCalls++
	return c.MockChannelFavoriteRepository.ExistsMany(userID, channelIDs)
}

// Show が viewer の following / favorite 状態に応じて isFollowing /
// isFavorited を embed することを確認する (#522)。frontend の
// channel.vue がフォローボタンの状態を切り替える前提となるフィールド。
func TestShow_EmbedsViewerFollowState(t *testing.T) {
	h, repo, followRepo, _ := newHandler(t)
	favRepo := testutil.NewMockChannelFavoriteRepository()
	repo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "ch1"}
	_ = followRepo.Create(&model.ChannelFollowing{ID: "f1", FollowerID: "alice", FolloweeID: "ch1"})
	_ = favRepo.Create(&model.ChannelFavorite{ID: "fv1", UserID: "alice", ChannelID: "ch1"})

	h.SetFollowingRepo(followRepo)
	h.SetFavoriteRepo(favRepo)

	c, rec := newReq(t, `{"channelId":"ch1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"isFollowing":true`)
	assert.Contains(t, body, `"isFavorited":true`)
}

// 認証されていない viewer (anonymous) の Show では isFollowing /
// isFavorited を返さないこと。Misskey TS と同じく viewer 不在時は
// undefined 扱い。
func TestShow_NoViewerOmitsFollowState(t *testing.T) {
	h, repo, followRepo, _ := newHandler(t)
	favRepo := testutil.NewMockChannelFavoriteRepository()
	repo.Channels["ch1"] = &model.Channel{ID: "ch1"}
	h.SetFollowingRepo(followRepo)
	h.SetFavoriteRepo(favRepo)

	c, rec := newReq(t, `{"channelId":"ch1"}`)
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "isFollowing")
	assert.NotContains(t, body, "isFavorited")
}

// list endpoint (Featured) で per-row Exists が呼ばれず batch ExistsMany
// が 1 回だけ呼ばれること。N+1 解消の担保 (#522)。
func TestFeatured_BatchEmbedsFollowState(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	follow := &countingFollowingRepo{MockChannelFollowingRepository: testutil.NewMockChannelFollowingRepository()}
	fav := &countingFavoriteRepo{MockChannelFavoriteRepository: testutil.NewMockChannelFavoriteRepository()}
	for i := 1; i <= 5; i++ {
		id := "feat" + string(rune('0'+i))
		repo.Channels[id] = &model.Channel{
			ID:         id,
			IsArchived: false,
			NotesCount: 1,
			LastNotedAt: func() *time.Time {
				t := time.Now()
				return &t
			}(),
		}
	}
	_ = follow.Create(&model.ChannelFollowing{ID: "f1", FollowerID: "alice", FolloweeID: "feat1"})
	h.SetFollowingRepo(follow)
	h.SetFavoriteRepo(fav)

	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, 0, follow.existsCalls,
		"per-row Exists must not be called from list endpoint (N+1 must be eliminated)")
	assert.Equal(t, 1, follow.existsManyCalls,
		"ExistsMany should be called exactly once per request")
	assert.Equal(t, 5, follow.existsManySize,
		"all 5 channel IDs should be coalesced into a single batch")
	assert.Equal(t, 1, fav.existsManyCalls)

	body := rec.Body.String()
	// #1770: lastNotedAt は upstream の toISOString() に合わせ 3桁 ms (.000Z) 形式。
	assert.Regexp(t, `"lastNotedAt":"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z"`, body,
		"lastNotedAt must be normalized to 3-digit ms ISO format")
	assert.Contains(t, body, `"isFollowing":true`,
		"alice's followed channel must report isFollowing=true")
	assert.Contains(t, body, `"isFollowing":false`,
		"unfollowed channels must still report the field for the frontend")
}

// --- bannerUrl / createdAt resolution (#1280) -----------------------------

// stubBannerResolver is a ChannelBannerResolver double for banner lookups.
type stubBannerResolver struct {
	files    map[string]*model.DriveFile
	batchErr error
}

func (s *stubBannerResolver) FindByID(id string) (*model.DriveFile, error) {
	if f, ok := s.files[id]; ok {
		return f, nil
	}
	return nil, errors.New("not found")
}

func (s *stubBannerResolver) FindByIDs(ids []string) ([]*model.DriveFile, error) {
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	out := make([]*model.DriveFile, 0, len(ids))
	for _, id := range ids {
		if f, ok := s.files[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

func aidxID(t *testing.T) string {
	t.Helper()
	gen, _ := id.NewGenerator("aidx")
	return gen.Generate(time.Now())
}

func TestChannelToMap_BannerURL(t *testing.T) {
	h, _, _, _ := newHandler(t)
	web := "https://media.example/web.png"
	h.SetDriveFileRepo(&stubBannerResolver{files: map[string]*model.DriveFile{
		"b1": {ID: "b1", URL: "https://media.example/orig.png", WebpublicURL: &web},
		"b2": {ID: "b2", URL: "https://media.example/orig2.png"},
	}})
	chID := aidxID(t)

	bid := "b1"
	out := h.channelToMapForViewer(&model.Channel{ID: chID, BannerID: &bid}, nil)
	assert.Equal(t, web, out["bannerUrl"], "webpublicUrl is preferred over url")
	assert.NotEmpty(t, out["createdAt"], "createdAt is derived from the aidx id")

	// webpublicUrl が無ければ url にフォールバック。
	bid2 := "b2"
	out = h.channelToMapForViewer(&model.Channel{ID: chID, BannerID: &bid2}, nil)
	assert.Equal(t, "https://media.example/orig2.png", out["bannerUrl"])
}

func TestChannelToMap_BannerURL_NullCases(t *testing.T) {
	h, _, _, _ := newHandler(t)
	chID := aidxID(t)

	// resolver 未配線 -> null。
	out := h.channelToMapForViewer(&model.Channel{ID: chID}, nil)
	assert.Contains(t, out, "bannerUrl")
	assert.Nil(t, out["bannerUrl"])

	h.SetDriveFileRepo(&stubBannerResolver{files: map[string]*model.DriveFile{}})
	// bannerId nil -> null。
	out = h.channelToMapForViewer(&model.Channel{ID: chID}, nil)
	assert.Nil(t, out["bannerUrl"])
	// bannerId set でも lookup miss -> null。
	bid := "missing"
	out = h.channelToMapForViewer(&model.Channel{ID: chID, BannerID: &bid}, nil)
	assert.Nil(t, out["bannerUrl"])
}

func TestResolveBannerURLs_Batch(t *testing.T) {
	h, _, _, _ := newHandler(t)
	web := "https://media.example/web.png"
	h.SetDriveFileRepo(&stubBannerResolver{files: map[string]*model.DriveFile{
		"b1": {ID: "b1", URL: "u1", WebpublicURL: &web},
	}})
	b1 := "b1"
	rows := []*model.Channel{
		{ID: "c1", BannerID: &b1},
		{ID: "c2"},
	}
	res := h.resolveBannerURLs(rows)
	assert.Equal(t, web, res["c1"])
	_, ok := res["c2"]
	assert.False(t, ok, "channel without banner has no map entry")

	// FindByIDs error -> empty map (best-effort)。
	h.SetDriveFileRepo(&stubBannerResolver{batchErr: errors.New("boom")})
	assert.Empty(t, h.resolveBannerURLs(rows))

	// resolver 未配線 -> empty map。
	h2, _, _, _ := newHandler(t)
	assert.Empty(t, h2.resolveBannerURLs(rows))
	// 全 channel banner なし -> FindByIDs を呼ばず empty。
	assert.Empty(t, h.resolveBannerURLs([]*model.Channel{{ID: "c3"}}))
}

// #2106 L12: name が 128 文字超だと DB 500 でなく 400 INVALID_PARAM を返す。
func TestCreate_NameTooLong(t *testing.T) {
	h, _, _, _ := newHandler(t)
	long := strings.Repeat("a", 129)
	c, rec := newReq(t, `{"name":"`+long+`"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #2106 L13: explicit に color:"" を送ると minLength:1 違反で 400 (absent は default 適用)。
func TestCreate_EmptyColorRejected(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"x","color":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// **列に入らないカーソルは 400 (#3025)。** NUL を含む値は `id < ?` の bind
// parameter に載せた時点で PostgreSQL が落とすので、そのまま repository へ渡すと
// 500 になる。**JSON では NUL をエスケープで送る** — 生バイトを body に載せると
// JSON として不正になり、`Bind` の 400 で guard まで届かない。
func TestCursorGuardRejectsUnstorableCursor(t *testing.T) {
	const body = `{"untilId":"a\u0000b"}`
	for _, tt := range []struct {
		name string
		fn   func(h *Handler) func(echo.Context) error
	}{
		{"Followed", func(h *Handler) func(echo.Context) error { return h.Followed }},
		{"Owned", func(h *Handler) func(echo.Context) error { return h.Owned }},
		{"Search", func(h *Handler) func(echo.Context) error { return h.Search }},
		{"Timeline", func(h *Handler) func(echo.Context) error { return h.Timeline }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _, _ := newHandler(t)
			c, rec := newReq(t, body)
			setUser(c, "alice")
			require.NoError(t, tt.fn(h)(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"列に入らないカーソルを repository へ渡している (SELECT がそこで落ちる)")
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
		})
	}
}

// `channels/show` のピン留めノートに可視性ゲートが掛かること。
//
// **`channels/update` は `pinnedNoteIds` を検証しない** (所有者もチャンネル所属も
// 見ない) ので、誰でも自分のチャンネルに他人のノート ID を書ける。`channels/show`
// は未認証で叩けるため、ゲートが無いと **followers 限定ノートが全文で公開される**。
//
// 最も現実的な経路は「フォロワーが、見られるようになった followers 限定ノートの
// ID を自分のチャンネルにピン留めする」形で、ID の入手手段を考える必要が無い。
//
// **`HideEmbeds` では止まらない。** top-level 判定 (`HideNoteByPrefsDecision`) は
// 著者設定による降格しか見ず、元から followers の note は「intrinsic ゲート任せ」
// と明記して素通しする。
func TestShow_PinnedNotesAreVisibilityGated(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")

	setup := func(t *testing.T) (*Handler, *testutil.MockChannelRepository, *testutil.MockFollowingRepository, string) {
		t.Helper()
		h, repo, _, noteRepo := newHandler(t)
		h.SetPinnedNoteRepo(noteRepo)
		follows := testutil.NewMockFollowingRepository()
		h.SetUserFollowingRepo(follows)
		noteRepo.Notes["pub"] = &model.Note{ID: "pub", UserID: "alice", Visibility: model.NoteVisibilityPublic}
		noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "alice", Visibility: model.NoteVisibilityFollowers}
		cid := idGen.Generate(time.Now())
		repo.Channels[cid] = &model.Channel{ID: cid, Name: "alpha", PinnedNoteIDs: []string{"pub", "fol"}}
		return h, repo, follows, cid
	}

	pinnedIDs := func(t *testing.T, rec *httptest.ResponseRecorder) []string {
		t.Helper()
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		raw, ok := resp["pinnedNotes"].([]any)
		require.True(t, ok, "pinnedNotes must be present")
		out := make([]string, 0, len(raw))
		for _, e := range raw {
			out = append(out, e.(map[string]any)["id"].(string))
		}
		return out
	}

	t.Run("未認証には followers 限定を出さない", func(t *testing.T) {
		h, _, _, cid := setup(t)
		c, rec := newReq(t, `{"channelId":"`+cid+`"}`)
		require.NoError(t, h.Show(c))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"pub"}, pinnedIDs(t, rec))
	})

	t.Run("非フォロワーにも出さない", func(t *testing.T) {
		h, _, _, cid := setup(t)
		c, rec := newReq(t, `{"channelId":"`+cid+`"}`)
		setUser(c, "bob")
		require.NoError(t, h.Show(c))
		assert.Equal(t, []string{"pub"}, pinnedIDs(t, rec))
	})

	// **これが無いと follow 判定そのものが空虚になる。** repo を nil に差し替える
	// 変異が検出できず、「チャンネルのフォロー repo と取り違える」類の誤りを
	// 通してしまう (変異検証で実測)。
	t.Run("フォロワーには出す", func(t *testing.T) {
		h, _, follows, cid := setup(t)
		require.NoError(t, follows.Create(&model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice"}))
		c, rec := newReq(t, `{"channelId":"`+cid+`"}`)
		setUser(c, "bob")
		require.NoError(t, h.Show(c))
		assert.Equal(t, []string{"pub", "fol"}, pinnedIDs(t, rec))
	})

	// **弾きすぎていないことを見る。** 全部落とす実装でも上の 2 つは緑になる。
	t.Run("投稿者本人には出す", func(t *testing.T) {
		h, _, _, cid := setup(t)
		c, rec := newReq(t, `{"channelId":"`+cid+`"}`)
		setUser(c, "alice")
		require.NoError(t, h.Show(c))
		assert.Equal(t, []string{"pub", "fol"}, pinnedIDs(t, rec))
	})

	// 未配線は fail-closed (`CanSeeNote` が follow 判定不能を「見せない」に倒す)。
	t.Run("未配線でも public は出る", func(t *testing.T) {
		h, repo, _, noteRepo := newHandler(t)
		h.SetPinnedNoteRepo(noteRepo)
		// SetUserFollowingRepo を呼ばない
		noteRepo.Notes["pub"] = &model.Note{ID: "pub", UserID: "alice", Visibility: model.NoteVisibilityPublic}
		noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "alice", Visibility: model.NoteVisibilityFollowers}
		cid := idGen.Generate(time.Now())
		repo.Channels[cid] = &model.Channel{ID: cid, Name: "a", PinnedNoteIDs: []string{"pub", "fol"}}
		c, rec := newReq(t, `{"channelId":"`+cid+`"}`)
		setUser(c, "bob")
		require.NoError(t, h.Show(c))
		assert.Equal(t, []string{"pub"}, pinnedIDs(t, rec))
	})
}
