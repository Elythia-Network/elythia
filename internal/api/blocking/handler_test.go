package blocking

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/api/userrelation"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newHandler(t *testing.T) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreblocking.NewService(userRepo, blockingRepo, idGen)
	return NewHandler(svc, userRepo, idGen), userRepo
}

func newReq(t *testing.T, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func setUser(c echo.Context, id string) {
	c.Set(string(middleware.UserContextKey), &model.User{ID: id})
}

func addUser(repo *testutil.MockUserRepository, id string) {
	repo.Users[id] = &model.User{ID: id, Username: id}
}

func TestCreate_Success(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "alice")
	addUser(repo, "bob")

	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	// upstream Misskey TS と同じ 200 + UserDetailed (= drop-in 互換、#870)。
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"bob"`)
}

// #1802: create / delete のレスポンスに viewer→blockee の relation block が乗り、
// block 後は isBlocking=true / unblock 後は isBlocking=false になる。
func TestCreateDelete_ReturnsIsBlockingRelation(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreblocking.NewService(userRepo, blockingRepo, idGen)
	h := NewHandler(svc, userRepo, idGen)
	// relation の Blocking は service と同一インスタンスを渡し、block 状態を反映させる。
	h.SetRelationRepos(userrelation.Repos{Blocking: blockingRepo})
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")

	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "bob", resp["id"])
	assert.Equal(t, true, resp["isBlocking"], "block 後は isBlocking=true")
	assert.Equal(t, false, resp["isBlocked"])

	c, rec = newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["isBlocking"], "unblock 後は isBlocking=false")
}

// #1957-a: blocking/list の embed blockee に viewer->blockee の relation block が
// 乗る (isBlocking=true)。upstream BlockingEntityService.packMany が
// UserDetailedNotMe + me で pack するのと一致。mute/list と同様。
func TestList_BlockeeCarriesIsBlocking(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreblocking.NewService(userRepo, blockingRepo, idGen)
	h := NewHandler(svc, userRepo, idGen)
	// relation の Blocking は service と同一インスタンスで block 状態を反映。
	h.SetRelationRepos(userrelation.Repos{Blocking: blockingRepo})
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")

	c1, _ := newReq(t, `{"userId":"bob"}`)
	setUser(c1, "alice")
	require.NoError(t, h.Create(c1))

	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	blockee, ok := resp[0]["blockee"].(map[string]any)
	require.True(t, ok, "blockee が embed される")
	assert.Equal(t, true, blockee["isBlocking"], "embed blockee に isBlocking=true が乗る (#1957-a)")
}

// modStub implements ModeratorChecker for the count-gate tests.
type modStub struct{ mods map[string]bool }

func (m modStub) IsModerator(id string) bool { return m.mods[id] }

// newGatedHandler wires relation + moderator repos and blocks "bob" whose
// followersVisibility=followers and followersCount=7.
func newGatedHandler(t *testing.T, mod ModeratorChecker) (*Handler, *httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreblocking.NewService(userRepo, blockingRepo, idGen)
	h := NewHandler(svc, userRepo, idGen)
	h.SetRelationRepos(userrelation.Repos{
		Following: testutil.NewMockFollowingRepository(),
		Blocking:  blockingRepo,
	})
	if mod != nil {
		h.SetModeratorChecker(mod)
	}
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", FollowersCount: 7}
	userRepo.Profiles["bob"] = &model.UserProfile{
		UserID:              "bob",
		FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}
	c1, _ := newReq(t, `{"userId":"bob"}`)
	setUser(c1, "alice")
	require.NoError(t, h.Create(c1))
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	return h, rec, c
}

// TestCreate_GatesFollowersCount: blocking/create のレスポンス (UserDetailedNotMe)
// でも followers-only count を非フォロワーに leak させない (upstream は
// pack(blockee, blocker) で gate を通す、#1985)。
func TestCreate_GatesFollowersCount(t *testing.T) {
	h, _, _ := newGatedHandler(t, nil)
	// 別途 Create を直接叩いてレスポンスを検証する (newGatedHandler は List 用の
	// context を返すため)。alice は bob を既に block 済みなので unblock してから。
	c, _ := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c)) // 既存 block を解除
	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 0, resp["followersCount"], "create レスポンスでも followers-only count は leak しない (#1985)")
}

// TestCreate_ModeratorSeesFollowersCount: moderator viewer の create レスポンスは
// gate しない。
func TestCreate_ModeratorSeesFollowersCount(t *testing.T) {
	h, _, _ := newGatedHandler(t, modStub{mods: map[string]bool{"alice": true}})
	c, _ := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 7, resp["followersCount"], "moderator viewer には count を見せる (#1985)")
}

// TestList_GatesFollowersCount: followers-only count を非フォロワー viewer に
// leak させない (#1985)。
func TestList_GatesFollowersCount(t *testing.T) {
	h, rec, c := newGatedHandler(t, nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	blockee := resp[0]["blockee"].(map[string]any)
	assert.EqualValues(t, 0, blockee["followersCount"], "followers-only count は非フォロワーに leak しない (#1985)")
}

// TestList_ModeratorSeesFollowersCount: moderator viewer には gate をかけない。
func TestList_ModeratorSeesFollowersCount(t *testing.T) {
	h, rec, c := newGatedHandler(t, modStub{mods: map[string]bool{"alice": true}})
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	blockee := resp[0]["blockee"].(map[string]any)
	assert.EqualValues(t, 7, blockee["followersCount"], "moderator viewer には count を見せる (#1985)")
}

func TestCreate_InvalidParam(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_InvalidJSON(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{invalid`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_Self(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "alice")

	c, rec := newReq(t, `{"userId":"alice"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_NoSuchUser(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{"userId":"ghost"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_AlreadyBlocking(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "alice")
	addUser(repo, "bob")
	c1, _ := newReq(t, `{"userId":"bob"}`)
	setUser(c1, "alice")
	require.NoError(t, h.Create(c1))

	c2, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c2, "alice")
	require.NoError(t, h.Create(c2))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingBlockingRepo causes Create to fail to exercise the internalError path.
type failingBlockingRepo struct {
	*testutil.MockBlockingRepository
}

func (f *failingBlockingRepo) Create(_ *model.Blocking) error {
	return testutil.ErrNotFound
}

func TestCreate_RepoError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	idGen, _ := id.NewGenerator("aidx")
	svc := coreblocking.NewService(userRepo, &failingBlockingRepo{MockBlockingRepository: testutil.NewMockBlockingRepository()}, idGen)
	h := NewHandler(svc, nil, nil)

	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestDelete_Success(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "alice")
	addUser(repo, "bob")
	c1, _ := newReq(t, `{"userId":"bob"}`)
	setUser(c1, "alice")
	require.NoError(t, h.Create(c1))

	c2, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c2, "alice")
	require.NoError(t, h.Delete(c2))
	// upstream Misskey TS と同じ 200 + UserDetailed (#870)。
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"bob"`)
}

func TestDelete_InvalidParam(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_Self(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{"userId":"alice"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_NotBlocking(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingDeleteBlockingRepo lets us trigger Delete errors.
type failingDeleteBlockingRepo struct {
	*testutil.MockBlockingRepository
}

func (f *failingDeleteBlockingRepo) Delete(_ *model.Blocking) error {
	return testutil.ErrNotFound
}

func TestDelete_RepoError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	mock := testutil.NewMockBlockingRepository()
	mock.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "alice", BlockeeID: "bob"}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreblocking.NewService(userRepo, &failingDeleteBlockingRepo{MockBlockingRepository: mock}, idGen)
	h := NewHandler(svc, nil, nil)

	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestList_OK(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "alice")
	addUser(repo, "bob")
	c1, _ := newReq(t, `{"userId":"bob"}`)
	setUser(c1, "alice")
	require.NoError(t, h.Create(c1))

	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	shapetest.Assert(t, "Blocking", resp[0]) // L3 (#1286)
}

func TestList_LimitOutOfRangeRejected(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{"limit":1000}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestList_InvalidJSON(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{invalid`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingListBlockingRepo lets us trigger List errors.
type failingListBlockingRepo struct {
	*testutil.MockBlockingRepository
}

func (f *failingListBlockingRepo) ListByBlocker(_, _, _ string, _, _ int) ([]*model.Blocking, error) {
	return nil, testutil.ErrNotFound
}

func TestList_RepoError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreblocking.NewService(userRepo, &failingListBlockingRepo{MockBlockingRepository: testutil.NewMockBlockingRepository()}, idGen)
	h := NewHandler(svc, nil, nil)

	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
