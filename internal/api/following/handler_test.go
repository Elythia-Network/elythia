package following

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/userrelation"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// errStub is a sentinel error for repository failures in tests.
var errStub = errors.New("stub error")

// failingUserRepo wraps MockUserRepository and forces counter operations to fail.
// This produces errors from the service that bubble up to the handler's default
// error branches.
type failingUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingUserRepo) IncrementFollowingCount(_ string, _ int) error { return errStub }
func (f *failingUserRepo) IncrementFollowersCount(_ string, _ int) error { return errStub }

// failingFollowRequestRepo wraps MockFollowRequestRepository and forces Delete
// to fail.
type failingFollowRequestRepo struct {
	*testutil.MockFollowRequestRepository
}

func (f *failingFollowRequestRepo) Delete(_ *model.FollowRequest) error { return errStub }

// failingListReqRepo wraps MockFollowRequestRepository and makes ListReceived fail.
type failingListReqRepo struct {
	*testutil.MockFollowRequestRepository
}

func (f *failingListReqRepo) ListReceived(_ string, _ int, _, _ string) ([]*model.FollowRequest, error) {
	return nil, errStub
}

// newHandlerWithFailingCounters builds a handler whose user-counter increments fail.
// This makes Follow/Unfollow/AcceptRequest return non-domain errors and exercises
// the handler's default error branches.
func newHandlerWithFailingCounters(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockFollowingRepository, *testutil.MockFollowRequestRepository) {
	t.Helper()
	mockUR := testutil.NewMockUserRepository()
	userRepo := &failingUserRepo{MockUserRepository: mockUR}
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	uSvc := coreuser.NewService(userRepo, nil, nil, nil)
	return NewHandler(fSvc, uSvc), mockUR, fRepo, frRepo
}

// newHandlerWithFailingRequestDelete builds a handler whose FollowRequest Delete fails.
func newHandlerWithFailingRequestDelete(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockFollowRequestRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	mockFRR := testutil.NewMockFollowRequestRepository()
	frRepo := &failingFollowRequestRepo{MockFollowRequestRepository: mockFRR}
	fRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	uSvc := coreuser.NewService(userRepo, nil, nil, nil)
	return NewHandler(fSvc, uSvc), userRepo, mockFRR
}

// newHandlerWithFailingListReceived builds a handler whose ListReceived fails.
func newHandlerWithFailingListReceived(t *testing.T) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	mockFRR := testutil.NewMockFollowRequestRepository()
	frRepo := &failingListReqRepo{MockFollowRequestRepository: mockFRR}
	fRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	uSvc := coreuser.NewService(userRepo, nil, nil, nil)
	return NewHandler(fSvc, uSvc), userRepo
}

func newTestHandler(t *testing.T) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	uSvc := coreuser.NewService(userRepo, nil, nil, nil)
	return NewHandler(fSvc, uSvc), userRepo
}

func addUser(repo *testutil.MockUserRepository, id string, locked bool) *model.User {
	u := &model.User{
		ID:                id,
		Username:          id,
		UsernameLower:     id,
		IsLocked:          locked,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users[id] = u
	return u
}

func postJSON(h echo.HandlerFunc, body string, me *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if me != nil {
		c.Set(string(middleware.UserContextKey), me)
	}
	_ = h(c)
	return rec
}

func TestCreate_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)

	rec := postJSON(h.Create, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "bob", resp["id"])
	// upstream following/create は userEntityService.pack を schema 指定なしで呼ぶため
	// 戻り値は UserLite (#1913)。UserDetailed 固有 field は出さない。
	shapetest.Assert(t, "UserLite", resp) // L3 (#1312)
	assertNoDetailedFields(t, resp)
}

// assertNoDetailedFields verifies a UserLite response carries none of the
// UserDetailed-only fields. shapetest.Assert("UserLite") alone does NOT catch
// extra fields (they degrade to SevInfo), so this guard locks the UserLite
// parity for #1913.
func assertNoDetailedFields(t *testing.T, resp map[string]any) {
	t.Helper()
	for _, f := range []string{"followersCount", "followingCount", "notesCount", "createdAt", "roles", "description"} {
		_, has := resp[f]
		assert.False(t, has, "UserLite 応答に UserDetailed 固有 field %q が混ざってはいけない", f)
	}
}

func TestCreate_LockedReturnsOK(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", true)

	rec := postJSON(h.Create, `{"userId": "bob"}`, alice)
	// 鍵アカウントへのフォローもbobのプロフィールを返す
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCreate_SelfFollow(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.Create, `{"userId": "alice"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// newTestHandlerWithRepos exposes the mock repositories so that tests can
// verify persisted row state (used by #1056 WithReplies tests).
func newTestHandlerWithRepos(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockFollowingRepository, *testutil.MockFollowRequestRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	uSvc := coreuser.NewService(userRepo, nil, nil, nil)
	return NewHandler(fSvc, uSvc), userRepo, fRepo, frRepo
}

// #1056: POST /api/following/create with withReplies=true should persist
// `Following.withReplies=true` on the new row (auto-accept path).
func TestCreate_WithReplies_True_PersistedOnFollowingRow(t *testing.T) {
	h, userRepo, fRepo, _ := newTestHandlerWithRepos(t)
	alice := addUser(userRepo, "alice", false)
	addUser(userRepo, "bob", false)

	rec := postJSON(h.Create, `{"userId": "bob", "withReplies": true}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, fRepo.Followings, 1)
	for _, f := range fRepo.Followings {
		assert.Equal(t, "alice", f.FollowerID)
		assert.Equal(t, "bob", f.FolloweeID)
		assert.True(t, f.WithReplies, "withReplies=true should be persisted on the Following row (#1056)")
	}
}

// #1056: withReplies が省略された場合は default false で row が作られる。
func TestCreate_WithReplies_Omitted_DefaultFalse(t *testing.T) {
	h, userRepo, fRepo, _ := newTestHandlerWithRepos(t)
	alice := addUser(userRepo, "alice", false)
	addUser(userRepo, "bob", false)

	rec := postJSON(h.Create, `{"userId": "bob"}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, fRepo.Followings, 1)
	for _, f := range fRepo.Followings {
		assert.False(t, f.WithReplies, "withReplies omitted should default to false")
	}
}

// #1056: locked followee 経由でも withReplies が FollowRequest row に反映される。
func TestCreate_WithReplies_True_LockedFollowee_PersistedOnFollowRequestRow(t *testing.T) {
	h, userRepo, _, frRepo := newTestHandlerWithRepos(t)
	alice := addUser(userRepo, "alice", false)
	addUser(userRepo, "bob", true) // locked

	rec := postJSON(h.Create, `{"userId": "bob", "withReplies": true}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, frRepo.Requests, 1)
	for _, r := range frRepo.Requests {
		assert.Equal(t, "alice", r.FollowerID)
		assert.Equal(t, "bob", r.FolloweeID)
		assert.True(t, r.WithReplies, "withReplies=true should be persisted on the FollowRequest row (#1056)")
	}
}

func TestCreate_AlreadyFollowing(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)
	postJSON(h.Create, `{"userId": "bob"}`, alice)

	rec := postJSON(h.Create, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_FolloweeNotFound(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.Create, `{"userId": "ghost"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// stubBlockedChecker reports the configured pair as blocked.
type stubBlockedChecker struct{ blockerID, blockeeID string }

func (s *stubBlockedChecker) IsBlocked(blockerID, blockeeID string) (bool, error) {
	return blockerID == s.blockerID && blockeeID == s.blockeeID, nil
}

func TestCreate_Blocked(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)
	// alice -> bob を follow しようとして bob が alice をブロックしている状況
	h.followingService.SetBlockingChecker(&stubBlockedChecker{blockerID: "bob", blockeeID: "alice"})

	rec := postJSON(h.Create, `{"userId": "bob"}`, alice)
	// upstream Misskey TS と同じ 400 BLOCKED (= drop-in 互換、#872)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"BLOCKED"`)
	assert.Contains(t, rec.Body.String(), "c4ab57cc-4e41-45e9-bfd9-584f61e35ce0")
}

// follower 自身が followee を block している方向は BLOCKED ではなく BLOCKING
// (upstream create.ts の blocking エラー、#1562)。
func TestCreate_Blocking(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)
	// alice -> bob を follow しようとして alice が bob をブロックしている状況
	h.followingService.SetBlockingChecker(&stubBlockedChecker{blockerID: "alice", blockeeID: "bob"})

	rec := postJSON(h.Create, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"BLOCKING"`)
	assert.Contains(t, rec.Body.String(), "4e2206ec-aa4f-4960-b865-6c23ac38e2d9")
}

func TestCreate_InvalidParam(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.Create, `{}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_InvalidJSON(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.Create, `{invalid`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)
	postJSON(h.Create, `{"userId": "bob"}`, alice)

	rec := postJSON(h.Delete, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// upstream following/delete も UserLite を返す (#1913)。
	shapetest.Assert(t, "UserLite", resp) // L3 (#1312)
	assertNoDetailedFields(t, resp)
}

func TestDelete_UserNotFound(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.Delete, `{"userId": "ghost"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_NotFollowing(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)

	rec := postJSON(h.Delete, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_SelfFollow(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.Delete, `{"userId": "alice"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_InvalidParam(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.Delete, `{}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestList_EmbedsRelationFlags: following/list の埋め込み followee に
// viewer-relation flag が付与される (#1912)。自分の following 一覧なので
// isFollowing=true、block/mute 系は false で present。
func TestList_EmbedsRelationFlags(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	uSvc := coreuser.NewService(userRepo, nil, nil, nil)
	h := NewHandler(fSvc, uSvc)
	h.SetIDGen(idGen)
	h.SetListPacker(userpack.New(userpack.Lookups{Relations: userrelation.Repos{
		Following:     fRepo,
		Blocking:      testutil.NewMockBlockingRepository(),
		Muting:        testutil.NewMockMutingRepository(),
		RenoteMuting:  testutil.NewMockRenoteMutingRepository(),
		FollowRequest: frRepo,
	}}, idGen))

	addUser(userRepo, "alice", false)
	bob := addUser(userRepo, "bob", false)
	// bob -> alice (alice not locked → 即 follow)。
	require.Equal(t, http.StatusOK, postJSON(h.Create, `{"userId":"alice"}`, bob).Code)

	rec := postJSON(h.List, `{}`, bob)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	followee, ok := out[0]["followee"].(map[string]any)
	require.True(t, ok, "following row must embed followee")
	assert.Equal(t, true, followee["isFollowing"], "following 一覧の followee は isFollowing=true")
	// UserDetailedNotMe の relation block は block/mute flag も present (false)。
	assert.Equal(t, false, followee["isMuted"])
	assert.Equal(t, false, followee["isBlocking"])
	assert.Equal(t, false, followee["hasPendingFollowRequestFromYou"])
}

func TestRequests_AcceptRejectCancel(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	bob := addUser(repo, "bob", true)

	// alice -> bob (locked) creates request
	postJSON(h.Create, `{"userId": "bob"}`, alice)

	// list received by bob
	rec := postJSON(h.ListRequests, `{}`, bob)
	assert.Equal(t, http.StatusOK, rec.Code)
	var listResp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	assert.Len(t, listResp, 1)

	// bob accepts
	rec = postJSON(h.AcceptRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// alice が存在しない → upstream accept.ts は getUser 先行で NO_SUCH_USER (66ce1645)。
func TestAcceptRequest_NoSuchUser(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := addUser(repo, "bob", false)

	rec := postJSON(h.AcceptRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "66ce1645-d66c-46bb-8b79-96739af885bd")
}

// alice は存在するが pending request 無し → NO_FOLLOW_REQUEST (bcde4f8b)。
func TestAcceptRequest_NoFollowRequest(t *testing.T) {
	h, repo := newTestHandler(t)
	addUser(repo, "alice", false)
	bob := addUser(repo, "bob", false)

	rec := postJSON(h.AcceptRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_FOLLOW_REQUEST")
	assert.Contains(t, rec.Body.String(), "bcde4f8b-0913-4614-8881-614e522fb041")
}

func TestAcceptRequest_InvalidParam(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := addUser(repo, "bob", false)

	rec := postJSON(h.AcceptRequest, `{}`, bob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// bob (承認する側) が alice (申請者) を block している状況で承認しようとすると
// BLOCKING (Create の #1562 と同じ code、occurrence が別なので id は別)。block
// 時の申請取り消しが best-effort で失敗し申請が残っているケースの多層防御。
func TestAcceptRequest_Blocking(t *testing.T) {
	h, repo, _, frRepo := newTestHandlerWithRepos(t)
	addUser(repo, "alice", false)
	bob := addUser(repo, "bob", true)
	frRepo.Requests["r"] = &model.FollowRequest{ID: "r", FollowerID: "alice", FolloweeID: "bob"}
	h.followingService.SetBlockingChecker(&stubBlockedChecker{blockerID: "bob", blockeeID: "alice"})

	rec := postJSON(h.AcceptRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"BLOCKING"`)
	assert.Contains(t, rec.Body.String(), "b155c6b3-83e9-400a-89c1-4a521da65240")
}

// alice (申請者) が bob (承認する側) を block している状況では BLOCKED。
func TestAcceptRequest_Blocked(t *testing.T) {
	h, repo, _, frRepo := newTestHandlerWithRepos(t)
	addUser(repo, "alice", false)
	bob := addUser(repo, "bob", true)
	frRepo.Requests["r"] = &model.FollowRequest{ID: "r", FollowerID: "alice", FolloweeID: "bob"}
	h.followingService.SetBlockingChecker(&stubBlockedChecker{blockerID: "alice", blockeeID: "bob"})

	rec := postJSON(h.AcceptRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"BLOCKED"`)
	assert.Contains(t, rec.Body.String(), "25d1906c-9e73-49c0-85d1-f48d2520cd60")
}

func TestRejectRequest_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	bob := addUser(repo, "bob", true)
	postJSON(h.Create, `{"userId": "bob"}`, alice)

	rec := postJSON(h.RejectRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// alice が存在しない → upstream reject.ts は getUser 先行で NO_SUCH_USER (abc2ffa6)。
func TestRejectRequest_NoSuchUser(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := addUser(repo, "bob", false)

	rec := postJSON(h.RejectRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "abc2ffa6-25b2-4380-ba99-321ff3a94555")
}

// alice は存在するが pending request 無し → upstream removeFollowRequest は silent
// return なので 204 No Content (NO_FOLLOW_REQUEST 系 error は reject.ts に無い、#1911)。
func TestRejectRequest_NoRequestSilent204(t *testing.T) {
	h, repo := newTestHandler(t)
	addUser(repo, "alice", false)
	bob := addUser(repo, "bob", false)

	rec := postJSON(h.RejectRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRejectRequest_InvalidParam(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := addUser(repo, "bob", false)

	rec := postJSON(h.RejectRequest, `{}`, bob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCancelRequest_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", true)
	postJSON(h.Create, `{"userId": "bob"}`, alice)

	rec := postJSON(h.CancelRequest, `{"userId": "bob"}`, alice)
	// upstream requests/cancel.ts は packed followee (UserLite) を 200 で返す (#1562)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "bob", body["id"])
}

func TestCancelRequest_NotFound(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", true)

	// request を送っていない → upstream の公開エラー FOLLOW_REQUEST_NOT_FOUND
	// (service 内部 id 流用の旧 NO_FOLLOW_REQUEST から変更、#1562)
	rec := postJSON(h.CancelRequest, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "FOLLOW_REQUEST_NOT_FOUND")
	assert.Contains(t, rec.Body.String(), "089b125b-d338-482a-9a09-e2622ac9f8d4")
}

func TestCancelRequest_NoSuchUser(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.CancelRequest, `{"userId": "ghost"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "4e68c551-fc4c-4e46-bb41-7d4a37bf9dab")
}

func TestCancelRequest_InvalidParam(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)

	rec := postJSON(h.CancelRequest, `{}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListRequests_Empty(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := addUser(repo, "bob", false)

	rec := postJSON(h.ListRequests, `{}`, bob)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// alice が bob (鍵付) に Follow すると FollowRequest 行が作られる。
// me=bob で ListRequests を呼ぶと follower フィールドに alice の情報が入ること、
// limit / sinceId / untilId の各 pagination param が反映されることを検証。
func TestListRequests_PopulatesFollowerAndPaginates(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := addUser(repo, "bob", true) // locked
	addUser(repo, "alice", false)
	addUser(repo, "carol", false)
	addUser(repo, "dave", false)
	_, _ = h.followingService.Follow("alice", bob.ID, corefollowing.FollowOptions{})
	_, _ = h.followingService.Follow("carol", bob.ID, corefollowing.FollowOptions{})
	_, _ = h.followingService.Follow("dave", bob.ID, corefollowing.FollowOptions{})

	rec := postJSON(h.ListRequests, `{}`, bob)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"alice"`)
	assert.Contains(t, rec.Body.String(), `"carol"`)
	assert.Contains(t, rec.Body.String(), `"dave"`)

	// limit=1 で件数が絞られること
	rec = postJSON(h.ListRequests, `{"limit":1}`, bob)
	require.Equal(t, http.StatusOK, rec.Code)
	var items []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	assert.Len(t, items, 1)
}

// --- failing-repo tests for handler default branches ---

func TestCreate_InternalError(t *testing.T) {
	h, repo, _, _ := newHandlerWithFailingCounters(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)

	rec := postJSON(h.Create, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestDelete_InternalError(t *testing.T) {
	h, repo, fRepo, _ := newHandlerWithFailingCounters(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)
	// 既存のfollowingを直接挿入してUnfollowに到達させる
	fRepo.Followings["x"] = &model.Following{ID: "x", FollowerID: "alice", FolloweeID: "bob"}

	rec := postJSON(h.Delete, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestAcceptRequest_InternalError(t *testing.T) {
	h, repo, _, frRepo := newHandlerWithFailingCounters(t)
	addUser(repo, "alice", false)
	bob := addUser(repo, "bob", true)
	frRepo.Requests["r"] = &model.FollowRequest{ID: "r", FollowerID: "alice", FolloweeID: "bob"}

	rec := postJSON(h.AcceptRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRejectRequest_InternalError(t *testing.T) {
	h, repo, frRepo := newHandlerWithFailingRequestDelete(t)
	addUser(repo, "alice", false)
	bob := addUser(repo, "bob", true)
	frRepo.Requests["r"] = &model.FollowRequest{ID: "r", FollowerID: "alice", FolloweeID: "bob"}

	rec := postJSON(h.RejectRequest, `{"userId": "alice"}`, bob)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestCancelRequest_InternalError(t *testing.T) {
	h, repo, frRepo := newHandlerWithFailingRequestDelete(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", true)
	frRepo.Requests["r"] = &model.FollowRequest{ID: "r", FollowerID: "alice", FolloweeID: "bob"}

	rec := postJSON(h.CancelRequest, `{"userId": "bob"}`, alice)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestListRequests_InternalError(t *testing.T) {
	h, repo := newHandlerWithFailingListReceived(t)
	bob := addUser(repo, "bob", false)

	rec := postJSON(h.ListRequests, `{}`, bob)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- /api/following/list (upstream 2026.5.2 #17385 + #17416) ---

// newTestHandlerWithFollowings は List テスト向けに followee user を seed して
// MockFollowingRepository に Following 行を入れた状態の Handler を返す。
func newTestHandlerWithFollowings(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockFollowingRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	uSvc := coreuser.NewService(userRepo, nil, nil, nil)
	h := NewHandler(fSvc, uSvc)
	h.SetIDGen(idGen)
	return h, userRepo, fRepo
}

func TestList_ReturnsFollowingsWithFollowee(t *testing.T) {
	h, repo, fRepo := newTestHandlerWithFollowings(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)
	addUser(repo, "carol", false)
	fRepo.Followings["f1"] = &model.Following{
		ID:         "f1",
		FollowerID: alice.ID,
		FolloweeID: "bob",
	}
	fRepo.Followings["f2"] = &model.Following{
		ID:         "f2",
		FollowerID: alice.ID,
		FolloweeID: "carol",
	}

	rec := postJSON(h.List, `{}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
	// followee が embed されること、id / followeeId / followerId が一致
	for _, item := range out {
		assert.Equal(t, alice.ID, item["followerId"])
		assert.NotNil(t, item["followee"])
		_, hasFollower := item["follower"]
		assert.False(t, hasFollower, "populateFollower=false なので follower は省略")
	}
}

func TestList_NotificationFilter(t *testing.T) {
	h, repo, fRepo := newTestHandlerWithFollowings(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)
	addUser(repo, "carol", false)
	notify := "play"
	fRepo.Followings["f1"] = &model.Following{
		ID:         "f1",
		FollowerID: alice.ID,
		FolloweeID: "bob",
		Notify:     &notify,
	}
	fRepo.Followings["f2"] = &model.Following{
		ID:         "f2",
		FollowerID: alice.ID,
		FolloweeID: "carol",
		// Notify == nil → notification=true で除外される
	}

	rec := postJSON(h.List, `{"notification": true}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "bob", out[0]["followeeId"])
}

func TestList_LimitOutOfRangeRejected(t *testing.T) {
	h, repo, fRepo := newTestHandlerWithFollowings(t)
	alice := addUser(repo, "alice", false)
	// 200 件積んで limit=100 にクランプされること
	for i := 0; i < 200; i++ {
		fid := "fu" + string(rune('a'+i%26))
		addUser(repo, fid, false)
		// id は 200 件 unique にしたいので i を含める (= mock では順序に
		// 影響するが、limit クランプ確認には十分)
		fRepo.Followings[fid] = &model.Following{
			ID:         fid,
			FollowerID: alice.ID,
			FolloweeID: fid,
		}
	}

	// upstream は ajv で maximum を強制するので範囲外は 400。
	rec := postJSON(h.List, `{"limit": 9999}`, alice)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 上限ちょうどは通り、最大 100 件まで返る。
	rec = postJSON(h.List, `{"limit": 100}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.LessOrEqual(t, len(out), 100)
}

func TestList_DefaultLimit(t *testing.T) {
	h, repo, fRepo := newTestHandlerWithFollowings(t)
	alice := addUser(repo, "alice", false)
	for i := 0; i < 30; i++ {
		fid := "fu" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		addUser(repo, fid, false)
		fRepo.Followings[fid] = &model.Following{
			ID:         fid,
			FollowerID: alice.ID,
			FolloweeID: fid,
		}
	}

	rec := postJSON(h.List, `{}`, alice)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 10, len(out), "limit 未指定 → default 10")
}
