package userlists_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/userlists"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHandler(t *testing.T) (*userlists.Handler, *testutil.MockUserListRepository) {
	t.Helper()
	repo := testutil.NewMockUserListRepository()
	idGen, _ := id.NewGenerator("aidx")
	return userlists.NewHandler(repo, idGen), repo
}

func doPost(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = h(c)
	return rec
}

func TestList_Success(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.List, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1550: userId 指定で対象ユーザーの public list のみ返す (private は除外)。
func TestList_ByUserIDReturnsPublicOnly(t *testing.T) {
	h, repo := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"}))
	h.SetUserRepo(userRepo)
	repo.Lists["pub"] = &model.UserList{ID: "pub", UserID: "u2", Name: "public", IsPublic: true}
	repo.Lists["priv"] = &model.UserList{ID: "priv", UserID: "u2", Name: "private", IsPublic: false}

	rec := doPost(h.List, `{"userId":"u2"}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, "public list のみ返る")
	assert.Equal(t, "pub", out[0]["id"])
}

// #1550: userId 指定で対象 user が不在なら NO_SUCH_USER。
func TestList_ByUserIDNoSuchUser(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetUserRepo(testutil.NewMockUserRepository())
	rec := doPost(h.List, `{"userId":"ghost"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "a8af4a82-0980-4cc4-a6af-8b0ffd54465e")
}

// #1550: userId 指定で対象が remote user なら REMOTE_USER_NOT_ALLOWED。
func TestList_ByUserIDRemoteUser(t *testing.T) {
	h, _ := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	require.NoError(t, userRepo.Create(&model.User{ID: "r1", Host: &host}))
	h.SetUserRepo(userRepo)
	rec := doPost(h.List, `{"userId":"r1"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "53858f1b-3315-4a01-81b7-db9b48d4b79a")
}

// #1550: userId 未指定 && 未認証は INVALID_PARAM。
func TestList_NoUserIDUnauthenticated(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.List, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ab36de0e-29e9-48cb-9732-d82f1281620d")
}

func TestCreate_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	rec := doPost(h.Create, `{"name":"My List"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Lists, 1)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "My List", resp["name"])
	shapetest.Assert(t, "UserList", resp) // L3 (#1270)
}

func TestCreate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Create, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "Test"}
	rec := doPost(h.Show, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "UserList", resp) // L3 (#1320)
}

func TestShow_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Show, `{"listId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Show, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPush_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestPush_ListNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Push, `{"listId":"ghost","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPush_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Push, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPull_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	rec := doPost(h.Pull, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestPull_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Pull, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1550: pull は removeMember 前に対象 user 存在を検証し不在なら NO_SUCH_USER。
func TestPull_NoSuchUser(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	h.SetUserRepo(testutil.NewMockUserRepository()) // u2 未登録
	rec := doPost(h.Pull, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "588e7f72-c744-4a61-b180-d354e912bda2")
}

func TestPull_UserExistsSucceeds(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"}))
	h.SetUserRepo(userRepo)
	rec := doPost(h.Pull, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// stubProxyFollow records EnqueueProxyFollow calls for #1704 tests.
type stubProxyFollow struct {
	got  []string
	hits int
}

func (s *stubProxyFollow) EnqueueProxyFollow(ids []string) { s.hits++; s.got = ids }

// #1704: remote user を push したら proxy follow を enqueue する。
func TestPush_RemoteUserProxyFollow(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	require.NoError(t, userRepo.Create(&model.User{ID: "r1", Host: &host}))
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	pf := &stubProxyFollow{}
	h.SetProxyFollow(pf)

	rec := doPost(h.Push, `{"listId":"l1","userId":"r1"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, pf.hits)
	assert.Equal(t, []string{"r1"}, pf.got)
}

// #1704: local user の push では proxy follow を enqueue しない。
func TestPush_LocalUserNoProxyFollow(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"})) // local (Host nil)
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	pf := &stubProxyFollow{}
	h.SetProxyFollow(pf)

	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 0, pf.hits, "local user は proxy follow しない")
}

// stubUserListEventPublisher records PublishUserListEvent calls for #1549 tests.
type stubUserListEventPublisher struct {
	listIDs []string
	types   []string
	bodies  []any
}

func (s *stubUserListEventPublisher) PublishUserListEvent(listID, eventType string, body any) {
	s.listIDs = append(s.listIDs, listID)
	s.types = append(s.types, eventType)
	s.bodies = append(s.bodies, body)
}

// #1549: push が成功したら userAdded membership event を publish する。
func TestPush_PublishesUserAdded(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2", Username: "bob"}))
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	pub := &stubUserListEventPublisher{}
	h.SetUserListEventPublisher(pub)

	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, pub.types, 1)
	assert.Equal(t, "l1", pub.listIDs[0])
	assert.Equal(t, "userAdded", pub.types[0])
}

// #1549: push が AddMember 失敗 (ALREADY_ADDED) で終わるときは userAdded を
// publish しない。
func TestPush_NoPublishOnAlreadyAdded(t *testing.T) {
	repo := &duplicateAddMemberRepo{testutil.NewMockUserListRepository()}
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(repo, idGen)
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"}))
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	pub := &stubUserListEventPublisher{}
	h.SetUserListEventPublisher(pub)

	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, pub.types, "AddMember 失敗時は userAdded を publish しない")
}

// #1549: pull が成功したら userRemoved membership event を publish する。
func TestPull_PublishesUserRemoved(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2", Username: "bob"}))
	h.SetUserRepo(userRepo)
	pub := &stubUserListEventPublisher{}
	h.SetUserListEventPublisher(pub)

	rec := doPost(h.Pull, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, pub.types, 1)
	assert.Equal(t, "l1", pub.listIDs[0])
	assert.Equal(t, "userRemoved", pub.types[0])
}

// #1550: push は membership.userListUserId に list owner を設定する。
func TestPush_SetsUserListUserID(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"}))
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, repo.Members, 1)
	require.NotNil(t, repo.Members[0].UserListUserID)
	assert.Equal(t, "owner", *repo.Members[0].UserListUserID)
}

func TestDelete_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	rec := doPost(h.Delete, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestDelete_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Delete, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Failing repo tests

type failingListRepo struct {
	*testutil.MockUserListRepository
}

func (f *failingListRepo) ListByUser(_ string) ([]*model.UserList, error) { return nil, assert.AnError }

func TestList_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(&failingListRepo{testutil.NewMockUserListRepository()}, idGen)
	rec := doPost(h.List, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// failingMembersRepo は ListMembers だけ error を返す stub。memberIDs
// helper の error fallback path (= repo error 時に slog.Warn して nil を
// 返し、PackUserList が空配列で serialize する) を cover する
// (#871 shape preservation + PR #875 review feedback の error logging)。
type failingMembersRepo struct {
	*testutil.MockUserListRepository
}

func (f *failingMembersRepo) ListMembers(_ string) ([]*model.UserListMembership, error) {
	return nil, assert.AnError
}

func TestList_MembersErrorFallsBackToEmptyUserIds(t *testing.T) {
	repo := &failingMembersRepo{testutil.NewMockUserListRepository()}
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "broken-members"}
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(repo, idGen)
	rec := doPost(h.List, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	// repo error でも shape は保たれ userIds は [] で出る (= upstream parity)。
	assert.Equal(t, []any{}, out[0]["userIds"])
}

// memberIDs の happy path: ListMembers が member を返す場合に userIds が
// 正しく埋まること (#871 shape の core path)。failingMembersRepo を使わない
// 標準 mock 経由で AddMember → Show で round-trip する。命名は List 側の
// TestList_MembersErrorFallsBackToEmptyUserIds と対称になるよう統一。
func TestShow_PopulatedUserIdsAreReturnedFromMembers(t *testing.T) {
	h, repo := newTestHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	listID := idGen.Generate(time.Now())
	memberMembershipID := idGen.Generate(time.Now())
	repo.Lists[listID] = &model.UserList{ID: listID, UserID: "u1", Name: "with-members"}
	require.NoError(t, repo.AddMember(&model.UserListMembership{
		ID: memberMembershipID, UserListID: listID, UserID: "member1",
	}))

	rec := doPost(h.Show, `{"listId":"`+listID+`"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	userIDs, ok := out["userIds"].([]any)
	require.True(t, ok)
	require.Len(t, userIDs, 1)
	assert.Equal(t, "member1", userIDs[0])
}

// Show endpoint も同 helper を経由するので、ListMembers error 時の shape
// 保持を独立 test で守る (= 内部 helper 共有でも response 外形が崩れない
// regression guard、PR #875 review feedback)。
func TestShow_MembersErrorFallsBackToEmptyUserIds(t *testing.T) {
	repo := &failingMembersRepo{testutil.NewMockUserListRepository()}
	repo.Lists["l2"] = &model.UserList{ID: "l2", UserID: "u1", Name: "show-broken"}
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(repo, idGen)
	rec := doPost(h.Show, `{"listId":"l2"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "l2", out["id"])
	assert.Equal(t, []any{}, out["userIds"])
}

type failingCreateRepo struct {
	*testutil.MockUserListRepository
}

func (f *failingCreateRepo) Create(_ *model.UserList) error { return assert.AnError }

func TestCreate_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(&failingCreateRepo{testutil.NewMockUserListRepository()}, idGen)
	rec := doPost(h.Create, `{"name":"x"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingAddMemberRepo struct {
	*testutil.MockUserListRepository
}

func (f *failingAddMemberRepo) AddMember(_ *model.UserListMembership) error { return assert.AnError }

func TestPush_Error(t *testing.T) {
	repo := &failingAddMemberRepo{testutil.NewMockUserListRepository()}
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(repo, idGen)
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// duplicateAddMemberRepo always returns the duplicate sentinel from AddMember
// so we can verify the handler translates it to ALREADY_ADDED (#396).
type duplicateAddMemberRepo struct {
	*testutil.MockUserListRepository
}

func (d *duplicateAddMemberRepo) AddMember(_ *model.UserListMembership) error {
	return repository.ErrUserListDuplicateMember
}

// #1029: userListLimit / userEachUserListsLimit role policy gate test。
type stubRolePolicyProvider struct {
	policies map[string]any
}

func (s *stubRolePolicyProvider) GetUserPolicies(_ string) map[string]any {
	return s.policies
}

func TestCreate_UserListLimitExceeded(t *testing.T) {
	h, repo := newTestHandler(t)
	// 既に 2 list 保有
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	repo.Lists["l2"] = &model.UserList{ID: "l2", UserID: "u1"}
	h.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"userListLimit": 2,
	}})
	rec := doPost(h.Create, `{"name":"third"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_USERLISTS")
	assert.Contains(t, rec.Body.String(), "0cf21a28-7715-4f39-a20d-777bfdb8d138")
}

func TestCreate_UserListLimit_PassesUnderLimit(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"userListLimit": 10,
	}})
	rec := doPost(h.Create, `{"name":"My List"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code, "limit 内なら通常成功")
}

func TestPush_UserEachUserListsLimitExceeded(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	repo.Members = []*model.UserListMembership{
		{ID: "m1", UserListID: "l1", UserID: "u_a"},
		{ID: "m2", UserListID: "l1", UserID: "u_b"},
	}
	h.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"userEachUserListsLimit": 2,
	}})
	rec := doPost(h.Push, `{"listId":"l1","userId":"u_c"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_USERS")
	assert.Contains(t, rec.Body.String(), "2dd9752e-a338-413d-8eec-41814430989b")
}

func TestPush_AlreadyAdded(t *testing.T) {
	repo := &duplicateAddMemberRepo{testutil.NewMockUserListRepository()}
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(repo, idGen)
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"code":"ALREADY_ADDED"`)
	assert.Contains(t, body, `1de7c884-1595-49e9-857e-61f12f4d4fc5`,
		"TS 互換 error UUID を返すこと")
}

// #1550: push が AddMember 前に対象 user の存在 (NO_SUCH_USER) と「対象が
// 自分を block しているか」(YOU_HAVE_BEEN_BLOCKED) を検証することを固定する。

// 対象 user が存在しなければ NO_SUCH_USER で弾き、AddMember を呼ばないこと。
func TestPush_NoSuchUser(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	userRepo := testutil.NewMockUserRepository()
	// u2 は登録されていない (= 不在)
	h.SetUserRepo(userRepo)
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "a89abd3d-f0bc-4cce-beb1-2f446f4f1e6a")
	assert.Empty(t, repo.Members, "不在 user の push は member を追加しないこと")
}

// 対象 user が存在すれば NO_SUCH_USER gate を通過して member を追加できること。
func TestPush_UserExistsSucceeds(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"}))
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, repo.Members, 1)
}

// 対象 user が viewer を block している場合は YOU_HAVE_BEEN_BLOCKED で弾き、
// AddMember を呼ばないこと (blockerId=target, blockeeId=me)。
func TestPush_YouHaveBeenBlocked(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"}))
	blockRepo := testutil.NewMockBlockingRepository()
	require.NoError(t, blockRepo.Create(&model.Blocking{ID: "b1", BlockerID: "u2", BlockeeID: "u1"}))
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(blockRepo)
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "YOU_HAVE_BEEN_BLOCKED")
	assert.Contains(t, rec.Body.String(), "990232c5-3f9d-4d83-9f3f-ef27b6332a4b")
	assert.Empty(t, repo.Members, "block されている user の push は member を追加しないこと")
}

// 逆向きの block (viewer が target を block) では push を妨げないこと。upstream
// は「target が me を block」のみを見るので、me→target の block は無関係。
func TestPush_ViewerBlocksTargetStillSucceeds(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"}))
	blockRepo := testutil.NewMockBlockingRepository()
	// viewer(u1) が target(u2) を block している (向きが逆)
	require.NoError(t, blockRepo.Create(&model.Blocking{ID: "b1", BlockerID: "u1", BlockeeID: "u2"}))
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(blockRepo)
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, repo.Members, 1)
}

// 自分自身を push する場合は block 判定を skip する (= TS の user.id !== me.id
// ガード)。自己 block row が存在しても無視されること。
func TestPush_SelfSkipsBlockCheck(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1"}))
	blockRepo := testutil.NewMockBlockingRepository()
	require.NoError(t, blockRepo.Create(&model.Blocking{ID: "b1", BlockerID: "u1", BlockeeID: "u1"}))
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(blockRepo)
	rec := doPost(h.Push, `{"listId":"l1","userId":"u1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, repo.Members, 1)
}

// blocking repo が error を返したら fail-closed で YOU_HAVE_BEEN_BLOCKED を
// 返し、AddMember を呼ばないこと (block 判定不能時に membership を作らない)。
func TestPush_BlockRepoErrorFailsClosed(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2"}))
	blockRepo := testutil.NewMockBlockingRepository()
	blockRepo.ExistsErr = assert.AnError
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(blockRepo)
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "YOU_HAVE_BEEN_BLOCKED")
	assert.Empty(t, repo.Members, "block 判定不能時は member を追加しないこと (fail-closed)")
}

type failingRemoveMemberRepo struct {
	*testutil.MockUserListRepository
}

func (f *failingRemoveMemberRepo) RemoveMember(_, _ string) error { return assert.AnError }

func TestPull_Error(t *testing.T) {
	repo := &failingRemoveMemberRepo{testutil.NewMockUserListRepository()}
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(repo, idGen)
	rec := doPost(h.Pull, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingDeleteRepo struct {
	*testutil.MockUserListRepository
}

func (f *failingDeleteRepo) Delete(_ string) error { return assert.AnError }

func TestDelete_Error(t *testing.T) {
	repo := &failingDeleteRepo{testutil.NewMockUserListRepository()}
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	h := userlists.NewHandler(repo, idGen)
	rec := doPost(h.Delete, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 非所有者は他人の私的リストを閲覧できない (privacy: 任意の認証ユーザーが
// 他人の private list を読めていた drift の回帰防止)。
func TestShow_PrivateListNonOwnerHidden(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "Private", IsPublic: false}
	rec := doPost(h.Show, `{"listId":"l1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_LIST")
}

// #1948-15: 非所有者が他人の public list を閲覧するには forPublic=true が必要。
// default (forPublic=false) は {id, userId:me} selection なので 404 (upstream 一致)。
func TestShow_PublicListNonOwnerNeedsForPublic(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "Public", IsPublic: true}
	// default forPublic=false → 404。
	rec := doPost(h.Show, `{"listId":"l1"}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// forPublic=true → 200。
	rec = doPost(h.Show, `{"listId":"l1","forPublic":true}`, &model.User{ID: "u2"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1550: forPublic && public のとき likedCount/isLiked を付与する。
func TestShow_ForPublicLikedCountAndIsLiked(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner", Name: "pub", IsPublic: true}
	favRepo := testutil.NewMockUserListFavoriteRepository()
	require.NoError(t, favRepo.Create(&model.UserListFavorite{ID: "f1", UserID: "viewer", UserListID: "l1"}))
	require.NoError(t, favRepo.Create(&model.UserListFavorite{ID: "f2", UserID: "other", UserListID: "l1"}))
	h.SetFavoriteRepo(favRepo)

	rec := doPost(h.Show, `{"listId":"l1","forPublic":true}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, float64(2), resp["likedCount"])
	assert.Equal(t, true, resp["isLiked"])
}

// 未認証 forPublic は likedCount を返すが isLiked は false。
func TestShow_ForPublicUnauthenticatedIsLikedFalse(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner", Name: "pub", IsPublic: true}
	favRepo := testutil.NewMockUserListFavoriteRepository()
	require.NoError(t, favRepo.Create(&model.UserListFavorite{ID: "f1", UserID: "x", UserListID: "l1"}))
	h.SetFavoriteRepo(favRepo)

	rec := doPost(h.Show, `{"listId":"l1","forPublic":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, float64(1), resp["likedCount"])
	assert.Equal(t, false, resp["isLiked"])
}

// forPublic 無しなら likedCount/isLiked は付かない (通常 UserList shape)。
func TestShow_NoForPublicNoLikedFields(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner", Name: "pub", IsPublic: true}
	h.SetFavoriteRepo(testutil.NewMockUserListFavoriteRepository())
	rec := doPost(h.Show, `{"listId":"l1"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotContains(t, resp, "likedCount")
	assert.NotContains(t, resp, "isLiked")
}

// #1948-15: forPublic=true は {id, isPublic:true} selection なので、自分の private
// list でも 404 (NO_SUCH_LIST) になる (upstream show.ts と一致)。
func TestShow_ForPublicOwnPrivateNotFound(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner", Name: "priv", IsPublic: false}
	h.SetFavoriteRepo(testutil.NewMockUserListFavoriteRepository())
	rec := doPost(h.Show, `{"listId":"l1","forPublic":true}`, &model.User{ID: "owner"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_LIST")
}

// #1948-15: default (forPublic=false) で認証非所有者は他人の public list を読めない
// ({id, userId:me} selection)。
func TestShow_NonOwnerPublicListNotFound(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner", Name: "pub", IsPublic: true}
	rec := doPost(h.Show, `{"listId":"l1"}`, &model.User{ID: "other"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_LIST")
}

// #1948-15: default で所有者は自分の private list を読める ({id, userId:me})。
func TestShow_OwnerPrivateListVisible(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner", Name: "priv", IsPublic: false}
	rec := doPost(h.Show, `{"listId":"l1"}`, &model.User{ID: "owner"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1948-15: forPublic=true で他人の public list は読める ({id, isPublic:true})。
func TestShow_ForPublicOtherPublicVisible(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner", Name: "pub", IsPublic: true}
	h.SetFavoriteRepo(testutil.NewMockUserListFavoriteRepository())
	rec := doPost(h.Show, `{"listId":"l1","forPublic":true}`, &model.User{ID: "other"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp, "likedCount", "forPublic && public は likedCount を付与")
}

// 他人 list への push が「存在しない」扱いで弾かれること。viewer が list owner
// 以外なら NO_SUCH_LIST を返し、AddMember が呼ばれてはならない (= owner gate
// 抜けの IDOR 回帰防止)。
func TestPush_NotOwner(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	rec := doPost(h.Push, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "intruder"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_LIST")
	assert.Empty(t, repo.Members, "owner 以外の push は member を追加しないこと")
}

// 他人 list からの pull が「存在しない」扱いで弾かれること。
func TestPull_NotOwner(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	repo.Members = []*model.UserListMembership{
		{ID: "m1", UserListID: "l1", UserID: "u2"},
	}
	rec := doPost(h.Pull, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "intruder"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_LIST")
	assert.Len(t, repo.Members, 1, "owner 以外の pull は member を削除しないこと")
}

// Pull が存在しない listId に対し 404 を返すこと (= 修正前は list を引かず
// 204 を返していた箇所の挙動変更を test で固定)。
func TestPull_ListNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Pull, `{"listId":"ghost","userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_LIST")
}

// 他人 list の delete が「存在しない」扱いで弾かれること。
func TestDelete_NotOwner(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner"}
	rec := doPost(h.Delete, `{"listId":"l1"}`, &model.User{ID: "intruder"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_LIST")
	assert.Contains(t, repo.Lists, "l1", "owner 以外の delete は list を削除しないこと")
}

// Delete が存在しない listId に対し 404 を返すこと。
func TestDelete_ListNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Delete, `{"listId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_LIST")
}
