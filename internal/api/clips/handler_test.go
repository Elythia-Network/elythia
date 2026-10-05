package clips

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreclip "github.com/elythia-network/elythia/internal/core/clip"
	corenote "github.com/elythia-network/elythia/internal/core/note"
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
	*testutil.MockClipRepository,
	*testutil.MockClipNoteRepository,
	*testutil.MockNoteRepository,
) {
	t.Helper()
	h, repo, noteRepo, notes, _ := newHandlerWithFollowing(t)
	return h, repo, noteRepo, notes
}

// newHandlerWithFollowing は followers/specified visibility テスト用に
// MockFollowingRepository も返す helper。followers note を seed して
// follow 関係を Followings に登録できる (#1456 AddNote visibility テスト)。
func newHandlerWithFollowing(t *testing.T) (
	*Handler,
	*testutil.MockClipRepository,
	*testutil.MockClipNoteRepository,
	*testutil.MockNoteRepository,
	*testutil.MockFollowingRepository,
) {
	t.Helper()
	repo := testutil.NewMockClipRepository()
	noteRepo := testutil.NewMockClipNoteRepository()
	notes := testutil.NewMockNoteRepository()
	// ListByClipVisible の visibility push-down 再現のため、clip mock に note の
	// visibility lookup 用 map を共有させる (#1418 review)。
	noteRepo.Notes = notes.Notes
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, noteRepo, notes, idGen)
	h := NewHandler(svc, idGen)
	h.SetQueryService(corenote.NewQueryService(notes, followingRepo))
	return h, repo, noteRepo, notes, followingRepo
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
	c, rec := newReq(t, `{"name":"alpha"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCreate_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_NameRequired(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingClipRepo causes Create to fail.
type failingClipRepo struct {
	*testutil.MockClipRepository
}

func (r *failingClipRepo) Create(_ *model.Clip) error { return errors.New("boom") }

func TestCreate_RepoError(t *testing.T) {
	mock := testutil.NewMockClipRepository()
	repo := &failingClipRepo{MockClipRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, testutil.NewMockClipNoteRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"name":"alpha"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Show ------------------------------------------------------------------

func TestShow_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	h.SetUserRepo(userRepo)
	idGen, _ := id.NewGenerator("aidx")
	clipID := idGen.Generate(time.Now())
	repo.Clips[clipID] = &model.Clip{ID: clipID, UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{"clipId":"`+clipID+`"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "Clip", resp) // L3 (#1320)
}

func TestShow_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"clipId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 非公開 clip の他人閲覧は missing と同じ NO_SUCH_CLIP 404 (存在秘匿、#1562)
func TestShow_PrivateHiddenFromOthers(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
	assert.Contains(t, rec.Body.String(), "c3c5fe33-d62c-44d2-9ea5-d997703f5c20")
}

func TestShow_AnonymousOnPublic(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// IDOR audit follow-up (#1422): 通常 RequireAuth で弾かれるが、URL 設計次第で
// middleware bypass や guest viewer 路を追加した時に regression するため、
// 未認証 viewer (= viewer == nil) が private clip を叩いた時の 403 reject を
// handler 層 negative test で固定する。
func TestShow_AnonymousPrivateHidden(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: false}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
}

// --- Update ----------------------------------------------------------------

func TestUpdate_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{"clipId":"c1","name":"alpha-v2","description":"desc","isPublic":true}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
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
	c, rec := newReq(t, `{"clipId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 非 owner mutation は upstream と同じく missing と区別不能な 404 NO_SUCH_CLIP
// を返す (#1562、存在 enumeration oracle 封じ)。
func TestUpdate_NonOwnerHidden(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	c, rec := newReq(t, `{"clipId":"c1","name":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
}

func TestUpdate_NameEmpty(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	c, rec := newReq(t, `{"clipId":"c1","name":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingUpdateRepo causes UpdateFields to fail.
type failingUpdateRepo struct {
	*testutil.MockClipRepository
}

func (r *failingUpdateRepo) UpdateFields(_ string, _ map[string]any) error {
	return errors.New("boom")
}

func TestUpdate_RepoError(t *testing.T) {
	mock := testutil.NewMockClipRepository()
	mock.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	repo := &failingUpdateRepo{MockClipRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, testutil.NewMockClipNoteRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"clipId":"c1","name":"x"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Delete ----------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestDelete_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"clipId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_NonOwnerHidden(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
}

// failingDeleteRepo causes Delete to fail.
type failingDeleteRepo struct {
	*testutil.MockClipRepository
}

func (r *failingDeleteRepo) Delete(_ *model.Clip) error { return errors.New("boom") }

func TestDelete_RepoError(t *testing.T) {
	mock := testutil.NewMockClipRepository()
	mock.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	repo := &failingDeleteRepo{MockClipRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, testutil.NewMockClipNoteRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- List ------------------------------------------------------------------

func TestList_Success(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", Name: "alpha"}
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alpha")
}

// #1245: clips/list が misskey_dart の Clip.fromJson 互換 shape を返すこと。
// createdAt / user / favoritedCount が非null で含まれること (旧 clipToMap は
// これらを欠いて `Null is not String` で落ちていた)。
func TestList_FullShape(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	h.SetUserRepo(userRepo)
	idGen, _ := id.NewGenerator("aidx")
	clipID := idGen.Generate(time.Now())
	repo.Clips[clipID] = &model.Clip{ID: clipID, UserID: "alice", Name: "alpha", IsPublic: true}

	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	cl := rows[0]
	createdAt, ok := cl["createdAt"].(string)
	assert.True(t, ok, "createdAt must be a non-null string")
	assert.NotEmpty(t, createdAt)
	assert.Equal(t, float64(0), cl["favoritedCount"])
	user, ok := cl["user"].(map[string]any)
	require.True(t, ok, "user must be a non-null object")
	assert.Equal(t, "alice", user["id"])
	shapetest.Assert(t, "Clip", cl) // L3 (#1270)
}

func TestList_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// frontend Paginator (cursor mode) が untilId / sinceId を渡した時に
// 行が絞り込まれること (#487 回帰防止)。bind 漏れだと無限スクロールに
// なるので、untilId 未満 / sinceId 超過の row だけ返ることを assert する。
func TestList_CursorPagination(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", Name: "alpha"}
	repo.Clips["c2"] = &model.Clip{ID: "c2", UserID: "alice", Name: "bravo"}
	repo.Clips["c3"] = &model.Clip{ID: "c3", UserID: "alice", Name: "charlie"}

	// untilId=c3 → id < "c3" を DESC = [c2, c1]
	c, rec := newReq(t, `{"untilId":"c3","limit":10}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"c2"`)
	assert.Contains(t, rec.Body.String(), `"c1"`)
	assert.NotContains(t, rec.Body.String(), `"c3"`)

	// sinceId=c1 → id > "c1" を ASC = [c2, c3]
	c, rec = newReq(t, `{"sinceId":"c1","limit":10}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"c2"`)
	assert.Contains(t, rec.Body.String(), `"c3"`)
	assert.NotContains(t, rec.Body.String(), `"c1"`)
}

// listFailRepo causes ListByUser to fail.
type listFailRepo struct {
	*testutil.MockClipRepository
}

func (r *listFailRepo) ListByUser(_, _, _ string, _, _ int) ([]*model.Clip, error) {
	return nil, errors.New("boom")
}

func TestList_RepoError(t *testing.T) {
	repo := &listFailRepo{MockClipRepository: testutil.NewMockClipRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, testutil.NewMockClipNoteRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- AddNote ---------------------------------------------------------------

func TestAddNote_Success(t *testing.T) {
	h, repo, _, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAddNote_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAddNote_ClipNotFound(t *testing.T) {
	h, _, _, notes := newHandler(t)
	// visibility gate (#1456) を通過させた上で、後続の clip 検索で
	// NO_SUCH_CLIP に到達することを担保する seed。
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	c, rec := newReq(t, `{"clipId":"missing","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
}

func TestAddNote_NonOwnerHidden(t *testing.T) {
	h, repo, _, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	// visibility gate (#1456) を通過させた上で、owner 不一致が 404 NO_SUCH_CLIP
	// に落ちることを担保する seed (#1562)。
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
}

func TestAddNote_NoteNotFound(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
}

func TestAddNote_AlreadyClipped(t *testing.T) {
	h, repo, _, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	c2, rec2 := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c2, "alice")
	require.NoError(t, h.AddNote(c2))
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
	_ = rec
}

// failingClipNoteCreateRepo causes Create to fail (other than already clipped).
type failingClipNoteCreateRepo struct {
	*testutil.MockClipNoteRepository
}

func (r *failingClipNoteCreateRepo) Create(_ *model.ClipNote) error { return errors.New("boom") }

func TestAddNote_RepoError(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes := testutil.NewMockNoteRepository()
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	noteRepo := &failingClipNoteCreateRepo{MockClipNoteRepository: testutil.NewMockClipNoteRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, noteRepo, notes, idGen)
	h := NewHandler(svc, idGen)
	// AddNote の visibility gate (#1456) を通過させるため queryService を wire。
	h.SetQueryService(corenote.NewQueryService(notes, nil))
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- AddNote --------------------------------------------------------------

// upstream ClipService.addNote は note の可視性を検証しない。クリップは
// 「あとで見る」リストで、可視性は読み取り側 (ListByClipVisible, #1418) が
// 評価する。今は見えないノートをクリップしておいて、フォロー後に読む使い方が
// upstream では成立するので、追加時には弾かない。
func TestAddNote_FollowersNote_NonFollower_IsAccepted(t *testing.T) {
	h, repo, clipNoteRepo, notes, _ := newHandlerWithFollowing(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityFollowers}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotEmpty(t, clipNoteRepo.Entries)
}

// フォロワーは followers visibility note を自分の clip に追加できる。
func TestAddNote_FollowersNote_Follower_OK(t *testing.T) {
	h, repo, clipNoteRepo, notes, followingRepo := newHandlerWithFollowing(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityFollowers}
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "author"}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, clipNoteRepo.Entries, 1)
}

// 自分の followers visibility note は自分の clip に追加できる
// (author は visibility に関わらず自分の note を見られる)。
func TestAddNote_FollowersNote_Author_OK(t *testing.T) {
	h, repo, clipNoteRepo, notes, _ := newHandlerWithFollowing(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityFollowers}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, clipNoteRepo.Entries, 1)
}

// specified visibility で宛先に入っていない viewer でもクリップには追加できる
// (upstream 同様。読み取り側で弾かれる)。
func TestAddNote_SpecifiedNote_NotInList_IsAccepted(t *testing.T) {
	h, repo, clipNoteRepo, notes, _ := newHandlerWithFollowing(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{
		ID:             "n1",
		UserID:         "author",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"other"},
	}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotEmpty(t, clipNoteRepo.Entries)
}

// specified visibility で VisibleUserIDs に入っている viewer は追加可。
func TestAddNote_SpecifiedNote_InList_OK(t *testing.T) {
	h, repo, clipNoteRepo, notes, _ := newHandlerWithFollowing(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{
		ID:             "n1",
		UserID:         "author",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"alice"},
	}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, clipNoteRepo.Entries, 1)
}

// queryService が未配線でも追加できる (可視性を見ないので依存しない)。
// 存在しない note は AddNote 側が NO_SUCH_NOTE を返す。
func TestAddNote_WorksWithoutQueryService(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	clipNoteRepo := testutil.NewMockClipNoteRepository()
	notes := testutil.NewMockNoteRepository()
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, clipNoteRepo, notes, idGen)
	h := NewHandler(svc, idGen) // SetQueryService を意図的に呼ばない
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotEmpty(t, clipNoteRepo.Entries)
}

// --- RemoveNote ------------------------------------------------------------

func TestRemoveNote_Success(t *testing.T) {
	h, repo, _, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	c2, rec2 := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c2, "alice")
	require.NoError(t, h.RemoveNote(c2))
	assert.Equal(t, http.StatusNoContent, rec2.Code)
	_ = rec
}

func TestRemoveNote_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRemoveNote_ClipNotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"clipId":"missing","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRemoveNote_NonOwnerHidden(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
}

// #1768: note 不在は 400 NO_SUCH_NOTE (NOT_CLIPPED は廃止)。
func TestRemoveNote_NoSuchNote(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
}

// upstream 2026.10.0 (4682d44cae): note は実在するが clip に無い場合も
// 400 NO_SUCH_NOTE。id は remove-note.ts の noSuchNote と同じ。
func TestRemoveNote_NotInClipIsNoSuchNote(t *testing.T) {
	h, repo, _, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
	assert.Contains(t, rec.Body.String(), "aff017de-190e-434b-893e-33a9ff5049d8")
}

// failingClipNoteDeleteRepo causes DeleteByPair to fail (other than not clipped).
type failingClipNoteDeleteRepo struct {
	*testutil.MockClipNoteRepository
}

func (r *failingClipNoteDeleteRepo) DeleteByPair(string, string) (int64, error) {
	return 0, errors.New("boom")
}

func TestRemoveNote_RepoError(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	mock := testutil.NewMockClipNoteRepository()
	mock.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}
	noteRepo := &failingClipNoteDeleteRepo{MockClipNoteRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	// clip_note の削除が失敗したら、NO_SUCH_NOTE ではなく 500 にする。
	notes := testutil.NewMockNoteRepository()
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	svc := coreclip.NewService(repo, noteRepo, notes, idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.RemoveNote(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Notes -----------------------------------------------------------------

func TestNotes_Success(t *testing.T) {
	h, repo, _, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice"}
	c, rec := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(c, "alice")
	require.NoError(t, h.AddNote(c))
	c2, rec2 := newReq(t, `{"clipId":"c1"}`)
	setUser(c2, "alice")
	require.NoError(t, h.Notes(c2))
	assert.Equal(t, http.StatusOK, rec2.Code)
	_ = rec
}

func TestNotes_BadJSON(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{not`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotes_NotFound(t *testing.T) {
	h, _, _, _ := newHandler(t)
	c, rec := newReq(t, `{"clipId":"missing"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotes_PrivateHiddenFromOthers(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
	assert.Contains(t, rec.Body.String(), "1d7645e6-2b6d-4635-b0fe-fe22b0e72e00")
}

func TestNotes_AnonymousOnPublic(t *testing.T) {
	h, repo, _, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	addCtx, _ := newReq(t, `{"clipId":"c1","noteId":"n1"}`)
	setUser(addCtx, "alice")
	require.NoError(t, h.AddNote(addCtx))

	// 認証なしで public clip の notes を取得できる
	c, rec := newReq(t, `{"clipId":"c1"}`)
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// public clip に含まれる followers / specified visibility のノートは、
// 閲覧権限のない viewer に対して除外されることを guard する。
func TestNotes_AnonymousExcludesNonPublicVisibilityInPublicClip(t *testing.T) {
	h, repo, clipNoteRepo, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	// clip に紐づく ClipNote を直接 seed (AddNote を経由しない = visibility に
	// 関係なく clip に存在する状態を再現する)。
	clipNoteRepo.Entries["cn_pub"] = &model.ClipNote{ID: "cn_pub", ClipID: "c1", NoteID: "n_pub"}
	clipNoteRepo.Entries["cn_fol"] = &model.ClipNote{ID: "cn_fol", ClipID: "c1", NoteID: "n_fol"}
	clipNoteRepo.Entries["cn_spec"] = &model.ClipNote{ID: "cn_spec", ClipID: "c1", NoteID: "n_spec"}
	notes.Notes["n_pub"] = &model.Note{ID: "n_pub", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice"}}
	notes.Notes["n_fol"] = &model.Note{ID: "n_fol", UserID: "alice", Visibility: "followers", User: &model.User{ID: "alice"}}
	notes.Notes["n_spec"] = &model.Note{ID: "n_spec", UserID: "alice", Visibility: "specified", VisibleUserIDs: []string{"other"}, User: &model.User{ID: "alice"}}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := map[string]bool{}
	for _, n := range out {
		ids[n["id"].(string)] = true
	}
	assert.True(t, ids["n_pub"])
	assert.False(t, ids["n_fol"], "followers は anonymous に漏らさない")
	assert.False(t, ids["n_spec"], "specified は対象外 viewer に漏らさない")
}

// listFailNoteRepo causes the clip note listing to fail. clip service Notes は
// ListByClipVisible を呼ぶためそちらを override する。
type listFailNoteRepo struct {
	*testutil.MockClipNoteRepository
}

func (r *listFailNoteRepo) ListByClipVisible(_, _, _, _ string, _ int, _ []string) ([]*model.ClipNote, error) {
	return nil, errors.New("boom")
}

func TestNotes_RepoError(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice"}
	noteRepo := &listFailNoteRepo{MockClipNoteRepository: testutil.NewMockClipNoteRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, noteRepo, testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
