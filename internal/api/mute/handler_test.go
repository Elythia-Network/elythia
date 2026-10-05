package mute

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/api/userrelation"
	coremuting "github.com/elythia-network/elythia/internal/core/muting"
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
	mutingRepo := testutil.NewMockMutingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coremuting.NewService(userRepo, mutingRepo, idGen)
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
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestCreate_WithExpiry(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "alice")
	addUser(repo, "bob")

	c, rec := newReq(t, `{"userId":"bob","expiresAt":2000000000000}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
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
	h, _ := newHandler(t)
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

func TestCreate_AlreadyMuting(t *testing.T) {
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

// failingMutingRepo causes Create to fail.
type failingMutingRepo struct {
	*testutil.MockMutingRepository
}

func (f *failingMutingRepo) Create(_ *model.Muting) error {
	return testutil.ErrNotFound
}

func TestCreate_RepoError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	idGen, _ := id.NewGenerator("aidx")
	svc := coremuting.NewService(userRepo, &failingMutingRepo{MockMutingRepository: testutil.NewMockMutingRepository()}, idGen)
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
	assert.Equal(t, http.StatusNoContent, rec.Code)
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

func TestDelete_NotMuting(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Delete(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingDeleteMutingRepo lets us trigger Delete error.
type failingDeleteMutingRepo struct {
	*testutil.MockMutingRepository
}

func (f *failingDeleteMutingRepo) Delete(_ *model.Muting) error {
	return testutil.ErrNotFound
}

func TestDelete_RepoError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	mock := testutil.NewMockMutingRepository()
	mock.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "alice", MuteeID: "bob"}
	idGen, _ := id.NewGenerator("aidx")
	svc := coremuting.NewService(userRepo, &failingDeleteMutingRepo{MockMutingRepository: mock}, idGen)
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
	c1, _ := newReq(t, `{"userId":"bob","expiresAt":2000000000000}`)
	setUser(c1, "alice")
	require.NoError(t, h.Create(c1))

	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	shapetest.Assert(t, "Muting", resp[0]) // L3 (#1286)
}

// TestList_EmbedsRelationFlags: mute/list の埋め込み mutee に viewer-relation
// flag が付与される (#1912)。mute 一覧なので isMuted=true。
func TestList_EmbedsRelationFlags(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	mutingRepo := testutil.NewMockMutingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coremuting.NewService(userRepo, mutingRepo, idGen)
	h := NewHandler(svc, userRepo, idGen)
	h.SetRelationRepos(userrelation.Repos{
		Following:     testutil.NewMockFollowingRepository(),
		Blocking:      testutil.NewMockBlockingRepository(),
		Muting:        mutingRepo,
		RenoteMuting:  testutil.NewMockRenoteMutingRepository(),
		FollowRequest: testutil.NewMockFollowRequestRepository(),
	})
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
	mutee, ok := resp[0]["mutee"].(map[string]any)
	require.True(t, ok, "muting row must embed mutee")
	assert.Equal(t, true, mutee["isMuted"], "mute 一覧の mutee は isMuted=true")
	assert.Equal(t, false, mutee["isFollowing"])
	assert.Equal(t, false, mutee["isBlocking"])
}

// modStub implements ModeratorChecker for the count-gate tests.
type modStub struct{ mods map[string]bool }

func (m modStub) IsModerator(id string) bool { return m.mods[id] }

// newGatedHandler wires relation + moderator repos and returns a handler with a
// muted "bob" whose followersVisibility=followers and followersCount=7.
func newGatedHandler(t *testing.T, mod ModeratorChecker) (*Handler, *httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	mutingRepo := testutil.NewMockMutingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coremuting.NewService(userRepo, mutingRepo, idGen)
	h := NewHandler(svc, userRepo, idGen)
	h.SetRelationRepos(userrelation.Repos{
		Following: testutil.NewMockFollowingRepository(),
		Muting:    mutingRepo,
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

// TestList_GatesFollowersCount: followers-only count を非フォロワー viewer に
// leak させない (#1985)。
func TestList_GatesFollowersCount(t *testing.T) {
	h, rec, c := newGatedHandler(t, nil)
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	mutee := resp[0]["mutee"].(map[string]any)
	assert.EqualValues(t, 0, mutee["followersCount"], "followers-only count は非フォロワーに leak しない (#1985)")
}

// TestList_ModeratorSeesFollowersCount: moderator viewer には gate をかけず
// count をそのまま見せる (upstream iAmModerator、#1985)。
func TestList_ModeratorSeesFollowersCount(t *testing.T) {
	h, rec, c := newGatedHandler(t, modStub{mods: map[string]bool{"alice": true}})
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	mutee := resp[0]["mutee"].(map[string]any)
	assert.EqualValues(t, 7, mutee["followersCount"], "moderator viewer には count を見せる (#1985)")
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

// failingListMutingRepo causes List to fail.
type failingListMutingRepo struct {
	*testutil.MockMutingRepository
}

func (f *failingListMutingRepo) ListByMuter(_, _, _ string, _, _ int) ([]*model.Muting, error) {
	return nil, testutil.ErrNotFound
}

func TestList_RepoError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coremuting.NewService(userRepo, &failingListMutingRepo{MockMutingRepository: testutil.NewMockMutingRepository()}, idGen)
	h := NewHandler(svc, nil, nil)

	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// **列に入らないカーソルは 400 (#3025)。** NUL を含む値は `id < ?` の bind
// parameter に載せた時点で PostgreSQL が落とすので (手元の simple protocol で
// SQLSTATE 08P01、本番の pgx extended protocol で 22021)、そのまま repository へ
// 渡すと**認証済みの一般利用者がパラメータ 1 文字で 500 を起こせる**。
//
// **JSON では NUL をエスケープで送る。** 生バイトを body に載せると JSON として
// 不正になり、`Bind` の 400 で guard まで届かない (= 何も検証しないまま緑になる)。
func TestList_RejectsUnstorableCursor(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{"untilId に NUL", `{"untilId":"a\u0000b"}`},
		{"sinceId に NUL", `{"sinceId":"a\u0000b"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, repo := newHandler(t)
			addUser(repo, "alice")
			c, rec := newReq(t, tt.body)
			setUser(c, "alice")
			require.NoError(t, h.List(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"列に入らないカーソルを repository へ渡している (SELECT がそこで落ちる)")
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
		})
	}
}

// 正常なカーソルは通る (guard を広げすぎると全ページングが 400 になる)。
func TestList_AcceptsOrdinaryCursor(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "alice")
	c, rec := newReq(t, `{"untilId":"9abcdef0123456"}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}
