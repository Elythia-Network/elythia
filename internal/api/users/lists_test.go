package users

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListsGetMemberships_MissingListID(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, postStub(h.ListsGetMemberships, `{}`, &model.User{ID: "u1"}).Code)
}

// seedListWithMember seeds a list + a single member (with User preloaded) for
// get-memberships tests.
func seedListWithMember(repo *testutil.MockUserListRepository, listID, owner, memberID string, isPublic bool) {
	repo.Lists[listID] = &model.UserList{ID: listID, UserID: owner, Name: "list", IsPublic: isPublic}
	repo.Members = append(repo.Members, &model.UserListMembership{
		ID: "mem_" + memberID, UserListID: listID, UserID: memberID, WithReplies: true,
		User: &model.User{ID: memberID, Username: memberID, UsernameLower: memberID},
	})
}

// #1550: get-memberships は listId の membership 配列 [{id,createdAt,userId,user,withReplies}]
// を返す (own private list を owner が取得)。
func TestListsGetMemberships_OwnPrivateList(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockUserListRepository()
	seedListWithMember(repo, "l1", "owner", "u2", false)
	h.SetUserListRepo(repo)

	rec := postStub(h.ListsGetMemberships, `{"listId":"l1"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "u2", rows[0]["userId"])
	assert.Equal(t, true, rows[0]["withReplies"])
	assert.Contains(t, rows[0], "createdAt")
	user := rows[0]["user"].(map[string]any)
	assert.Equal(t, "u2", user["id"], "user は UserLite で埋め込まれる")
}

// 非 owner は forPublic=false では他人の (private/public 問わず) list を取得できない
// (upstream: !forPublic && me!=null → own list only)。
func TestListsGetMemberships_NonOwnerDeniedWithoutForPublic(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockUserListRepository()
	seedListWithMember(repo, "l1", "owner", "u2", true) // public でも forPublic=false なら own-only
	h.SetUserListRepo(repo)
	rec := postStub(h.ListsGetMemberships, `{"listId":"l1"}`, &model.User{ID: "intruder"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// forPublic=true なら public list の membership を非 owner / 未認証でも取得できる。
func TestListsGetMemberships_ForPublic(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockUserListRepository()
	seedListWithMember(repo, "l1", "owner", "u2", true)
	h.SetUserListRepo(repo)
	rec := postStub(h.ListsGetMemberships, `{"listId":"l1","forPublic":true}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 1)
}

func TestListsGetMemberships_PublicUnauthenticatedAllowed(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockUserListRepository()
	seedListWithMember(repo, "l1", "owner", "u2", true)
	h.SetUserListRepo(repo)
	rec := postStub(h.ListsGetMemberships, `{"listId":"l1"}`, nil) // 未認証
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestListsGetMemberships_PrivateUnauthenticatedDenied(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockUserListRepository()
	seedListWithMember(repo, "l1", "owner", "u2", false)
	h.SetUserListRepo(repo)
	rec := postStub(h.ListsGetMemberships, `{"listId":"l1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsGetMemberships_NoSuchList(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetUserListRepo(testutil.NewMockUserListRepository())
	rec := postStub(h.ListsGetMemberships, `{"listId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- ListsCreateFromPublic ---

func TestListsCreateFromPublic_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.ListsCreateFromPublic, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- ListsFavorite ---

func TestListsFavorite(t *testing.T) {
	h, _ := newTestHandler(t)
	// listId未指定 → 400
	rec := postStub(h.ListsFavorite, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsFavorite_WithRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	h.SetUserListFavoriteRepo(favRepo)
	rec := postStub(h.ListsFavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := favRepo.Exists("u1", "l1")
	assert.True(t, exists)
}

// 既 fav は upstream `users/lists/favorite` 互換で ALREADY_FAVORITED
// (HTTP 400) を返す (#1550)。旧 mk-go は 204 を返していて shape が乖離して
// いた。UUID は favorite.ts の alreadyFavorited と一致。
func TestListsFavorite_AlreadyFavorited(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	favRepo.Favorites["u1:l1"] = &model.UserListFavorite{ID: "f1", UserID: "u1", UserListID: "l1"}
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "pub", IsPublic: true}
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)
	rec := postStub(h.ListsFavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ALREADY_FAVORITED")
	assert.Contains(t, rec.Body.String(), "6425bba0-985b-461e-af1b-518070e72081")
}

func TestListsFavorite_MissingListID(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	h.SetUserListFavoriteRepo(favRepo)
	rec := postStub(h.ListsFavorite, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 非公開 list を非所有者が favorite しようとしたら 404 NO_SUCH_USER_LIST で
// 弾き、fav row も作らない (#1423)。これが無いと `i/favorites` 経由で
// 他人の private list の existence fingerprint が成立する。
func TestListsFavorite_PrivateNonOwner(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "other", Name: "secret", IsPublic: false}
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsFavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER_LIST")
	// Misskey TS `users/lists/favorite` 固有の UUID (show の NO_SUCH_LIST とは別)。
	assert.Contains(t, rec.Body.String(), "7dbaf3cf-7b42-4b8f-b431-b3919e580dbe")
	exists, _ := favRepo.Exists("u1", "l1")
	assert.False(t, exists, "fav row should not be created for private non-owner list")
}

// TS favorite は `exists({ id, isPublic: true })` ゲートのため、所有者
// 本人でも private list は favorite 不可 (#1423)。owner 分岐を入れると
// TS と挙動が乖離するためここで 404 を期待値として固定する。
func TestListsFavorite_PrivateOwner(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "my-secret", IsPublic: false}
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsFavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER_LIST")
	assert.Contains(t, rec.Body.String(), "7dbaf3cf-7b42-4b8f-b431-b3919e580dbe")
	exists, _ := favRepo.Exists("u1", "l1")
	assert.False(t, exists, "fav row should not be created for private list even when caller is owner")
}

// 公開 list は所有者でなくても favorite 可 (= 既存の正常系を IsPublic
// gate 導入後も維持する)。
func TestListsFavorite_PublicNonOwner(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "other", Name: "public-list", IsPublic: true}
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsFavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := favRepo.Exists("u1", "l1")
	assert.True(t, exists)
}

// list が存在しない場合は 404 NO_SUCH_USER_LIST (#1423)。存在しない listId
// に対する favorite 試行を probing 経路にしない。private/不存在を同じ
// UUID で返すことで両者の区別ができないようにする。
func TestListsFavorite_NoSuchList(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	listRepo := testutil.NewMockUserListRepository()
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsFavorite, `{"listId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER_LIST")
	assert.Contains(t, rec.Body.String(), "7dbaf3cf-7b42-4b8f-b431-b3919e580dbe")
	exists, _ := favRepo.Exists("u1", "ghost")
	assert.False(t, exists)
}

// --- ListsUnfavorite ---

func TestListsUnfavorite(t *testing.T) {
	h, _ := newTestHandler(t)
	// listId未指定 → 400
	rec := postStub(h.ListsUnfavorite, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUnfavorite_WithRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	favRepo.Favorites["u1:l1"] = &model.UserListFavorite{ID: "f1", UserID: "u1", UserListID: "l1"}
	h.SetUserListFavoriteRepo(favRepo)
	rec := postStub(h.ListsUnfavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := favRepo.Exists("u1", "l1")
	assert.False(t, exists)
}

// public list + fav row が揃った正常系。NO_SUCH_USER_LIST / ALREADY_FAVORITED
// gate を導入後も unfavorite が成功して fav row を消すことを固定する (#1550)。
func TestListsUnfavorite_PublicWithFav(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	favRepo.Favorites["u1:l1"] = &model.UserListFavorite{ID: "f1", UserID: "u1", UserListID: "l1"}
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "other", Name: "pub", IsPublic: true}
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)
	rec := postStub(h.ListsUnfavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	exists, _ := favRepo.Exists("u1", "l1")
	assert.False(t, exists)
}

// upstream `users/lists/unfavorite` は `exists({ id, isPublic: true })` を
// 満たさない list を NO_SUCH_USER_LIST で弾く。private list は所有者本人でも
// unfavorite 経路では存在を隠す (UUID は favorite のそれとは別、#1550)。
func TestListsUnfavorite_PrivateNoSuchList(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	favRepo.Favorites["u1:l1"] = &model.UserListFavorite{ID: "f1", UserID: "u1", UserListID: "l1"}
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "secret", IsPublic: false}
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)
	rec := postStub(h.ListsUnfavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER_LIST")
	assert.Contains(t, rec.Body.String(), "baedb33e-76b8-4b0c-86a8-9375c0a7b94b")
	// list gate で弾かれるので fav row は残ったまま。
	exists, _ := favRepo.Exists("u1", "l1")
	assert.True(t, exists)
}

// 存在しない list への unfavorite も NO_SUCH_USER_LIST (private と同 UUID で
// 区別不能にする、#1550)。
func TestListsUnfavorite_NoSuchList(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	listRepo := testutil.NewMockUserListRepository()
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)
	rec := postStub(h.ListsUnfavorite, `{"listId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER_LIST")
	assert.Contains(t, rec.Body.String(), "baedb33e-76b8-4b0c-86a8-9375c0a7b94b")
}

// public list だが fav row が無い場合は upstream notFavorited (code
// ALREADY_FAVORITED, UUID は favorite のそれとは別) を HTTP 400 で返す (#1550)。
func TestListsUnfavorite_NotFavorited(t *testing.T) {
	h, _ := newTestHandler(t)
	favRepo := testutil.NewMockUserListFavoriteRepository()
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "other", Name: "pub", IsPublic: true}
	h.SetUserListFavoriteRepo(favRepo)
	h.SetUserListRepo(listRepo)
	rec := postStub(h.ListsUnfavorite, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ALREADY_FAVORITED")
	assert.Contains(t, rec.Body.String(), "835c4b27-463d-4cfa-969b-a9058678d465")
}

// --- ListsUpdate ---

// stubListsPolicy is a RolePolicyProvider stub for create-from-public limit tests.
type stubListsPolicy struct{ policies map[string]any }

func (s *stubListsPolicy) GetUserPolicies(_ string) map[string]any { return s.policies }

// newListsHandlerFull wires userListRepo / userRepo / blockingRepo /
// rolePolicyProvider so create-from-public の各 gate を test できる。
func newListsHandlerFull(t *testing.T) (*Handler, *testutil.MockUserListRepository, *testutil.MockUserRepository, *testutil.MockBlockingRepository, *stubListsPolicy) {
	t.Helper()
	h, userRepo := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	blockRepo := testutil.NewMockBlockingRepository()
	policy := &stubListsPolicy{policies: map[string]any{}}
	h.SetUserListRepo(listRepo)
	h.SetUserRepo(userRepo)
	h.SetBlockingRepo(blockRepo)
	h.SetRolePolicyProvider(policy)
	return h, listRepo, userRepo, blockRepo, policy
}

// seedPublicSrc seeds a public source list with the given member user IDs (and
// registers those users so NO_SUCH_USER passes).
func seedPublicSrc(t *testing.T, listRepo *testutil.MockUserListRepository, userRepo *testutil.MockUserRepository, listID string, memberIDs ...string) {
	t.Helper()
	listRepo.Lists[listID] = &model.UserList{ID: listID, UserID: "srcowner", Name: "src", IsPublic: true}
	for i, mid := range memberIDs {
		require.NoError(t, listRepo.AddMember(&model.UserListMembership{ID: "srcm_" + string(rune('a'+i)), UserListID: listID, UserID: mid}))
		require.NoError(t, userRepo.Create(&model.User{ID: mid}))
	}
}

// #1550: create-from-public は member を copy して packer shape を返す (全 gate 通過)。
func TestListsCreateFromPublic_FullCopiesMembers(t *testing.T) {
	h, listRepo, userRepo, _, _ := newListsHandlerFull(t)
	seedPublicSrc(t, listRepo, userRepo, "src", "m1", "m2")
	rec := postStub(h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []any{"m1", "m2"}, resp["userIds"])
	assert.NotContains(t, resp, "userId")
}

// #1550: create-from-public は copy した membership の userListUserId に新 list の
// owner を設定する。
func TestListsCreateFromPublic_SetsUserListUserID(t *testing.T) {
	h, listRepo, userRepo, _, _ := newListsHandlerFull(t)
	seedPublicSrc(t, listRepo, userRepo, "src", "m1")
	rec := postStub(h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	require.Equal(t, http.StatusOK, rec.Code)
	// copy された membership (UserID=m1) の userListUserId が owner "me"。
	var found *model.UserListMembership
	for _, m := range listRepo.Members {
		if m.UserID == "m1" && m.UserListID != "src" {
			found = m
		}
	}
	require.NotNil(t, found, "copy された m1 membership が存在する")
	require.NotNil(t, found.UserListUserID)
	assert.Equal(t, "me", *found.UserListUserID)
}

// stubUsersProxyFollow records EnqueueProxyFollow calls for #1704.
type stubUsersProxyFollow struct {
	got  []string
	hits int
}

func (s *stubUsersProxyFollow) EnqueueProxyFollow(ids []string) { s.hits++; s.got = ids }

// #1704: create-from-public で copy した remote member だけ proxy follow を enqueue。
func TestListsCreateFromPublic_RemoteMemberProxyFollow(t *testing.T) {
	h, listRepo, userRepo, _, _ := newListsHandlerFull(t)
	listRepo.Lists["src"] = &model.UserList{ID: "src", UserID: "srcowner", Name: "src", IsPublic: true}
	require.NoError(t, listRepo.AddMember(&model.UserListMembership{ID: "s1", UserListID: "src", UserID: "m1"}))
	require.NoError(t, listRepo.AddMember(&model.UserListMembership{ID: "s2", UserListID: "src", UserID: "r1"}))
	require.NoError(t, userRepo.Create(&model.User{ID: "m1"})) // local
	host := "remote.example"
	require.NoError(t, userRepo.Create(&model.User{ID: "r1", Host: &host})) // remote
	pf := &stubUsersProxyFollow{}
	h.SetProxyFollow(pf)

	rec := postStub(h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"r1"}, pf.got, "remote member のみ proxy follow")
}

func TestListsCreateFromPublic_TooManyUserLists(t *testing.T) {
	h, listRepo, userRepo, _, policy := newListsHandlerFull(t)
	seedPublicSrc(t, listRepo, userRepo, "src")
	// viewer は既に 1 list 所有、userListLimit=1 → TOO_MANY_USERLISTS。
	listRepo.Lists["own1"] = &model.UserList{ID: "own1", UserID: "me", Name: "owned"}
	policy.policies["userListLimit"] = 1
	rec := postStub(h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "e9c105b2-c595-47de-97fb-7f7c2c33e92f")
}

func TestListsCreateFromPublic_NoSuchUserMember(t *testing.T) {
	h, listRepo, _, _, _ := newListsHandlerFull(t)
	// member u_missing を memberships に入れるが userRepo には登録しない。
	listRepo.Lists["src"] = &model.UserList{ID: "src", UserID: "srcowner", Name: "src", IsPublic: true}
	require.NoError(t, listRepo.AddMember(&model.UserListMembership{ID: "sm1", UserListID: "src", UserID: "u_missing"}))
	rec := postStub(h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "13c457db-a8cb-4d88-b70a-211ceeeabb5f")
}

func TestListsCreateFromPublic_BlockedMember(t *testing.T) {
	h, listRepo, userRepo, blockRepo, _ := newListsHandlerFull(t)
	seedPublicSrc(t, listRepo, userRepo, "src", "blocker")
	require.NoError(t, blockRepo.Create(&model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "me"}))
	rec := postStub(h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "a2497f2a-2389-439c-8626-5298540530f4")
}

func TestListsCreateFromPublic_TooManyUsers(t *testing.T) {
	h, listRepo, userRepo, _, policy := newListsHandlerFull(t)
	seedPublicSrc(t, listRepo, userRepo, "src", "m1", "m2")
	policy.policies["userEachUserListsLimit"] = 1 // 1 件追加後に次で TOO_MANY_USERS
	rec := postStub(h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "1845ea77-38d1-426e-8e4e-8b83b24f5bd7")
}

func TestListsCreateFromPublic_NameTooLong(t *testing.T) {
	h, listRepo, userRepo, _, _ := newListsHandlerFull(t)
	seedPublicSrc(t, listRepo, userRepo, "src")
	body := `{"listId":"src","name":"` + strings.Repeat("a", 101) + `"}`
	rec := postStub(h.ListsCreateFromPublic, body, &model.User{ID: "me"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdate_MissingListID(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.ListsUpdate, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdate_Success(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "old"}
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsUpdate, `{"listId":"l1","name":"new"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "new", listRepo.Lists["l1"].Name)
}

// #1550: update は UserList packer shape ({id,createdAt,name,userIds,isPublic}) を
// 返し userId を露出しない。
func TestListsUpdate_ReturnsPackedShape(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "old"}
	listRepo.Members = append(listRepo.Members, &model.UserListMembership{ID: "m1", UserListID: "l1", UserID: "member1"})
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsUpdate, `{"listId":"l1","name":"new"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "new", resp["name"])
	assert.NotContains(t, resp, "userId", "userId は露出しない")
	assert.Contains(t, resp, "createdAt", "createdAt field が shape に含まれる")
	assert.ElementsMatch(t, []any{"member1"}, resp["userIds"])
}

// #1550: name は minLength:1/maxLength:100 で validate (渡された場合)。
func TestListsUpdate_NameTooLong(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetUserListRepo(testutil.NewMockUserListRepository())
	body := `{"listId":"l1","name":"` + strings.Repeat("a", 101) + `"}`
	rec := postStub(h.ListsUpdate, body, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdate_NameEmptyRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetUserListRepo(testutil.NewMockUserListRepository())
	// 空文字 name が渡されたら minLength 違反で 400 (絞り込み前に検証)。
	rec := postStub(h.ListsUpdate, `{"listId":"l1","name":""}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdate_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsUpdate, `{"listId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdate_IsPublic(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "test"}
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsUpdate, `{"listId":"l1","isPublic":true}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, listRepo.Lists["l1"].IsPublic)
}

func TestListsUpdate_NotOwner(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "other", Name: "test"}
	h.SetUserListRepo(listRepo)

	// u1は所有者ではないので404
	rec := postStub(h.ListsUpdate, `{"listId":"l1","name":"hacked"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdate_NilRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	// userListRepo未注入時はgraceful NoContent
	rec := postStub(h.ListsUpdate, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// --- ListsUpdateMembership ---

func TestListsUpdateMembership_MissingParams(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.ListsUpdateMembership, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdateMembership_Success(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "test"}
	listRepo.Members = append(listRepo.Members, &model.UserListMembership{ID: "m1", UserListID: "l1", UserID: "u2"})
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"u2","withReplies":true}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// #1948-15: withReplies を省略すると既存値を維持する (upstream は undefined カラムを
// skip)。明示値は更新する。
func TestListsUpdateMembership_WithRepliesOmittedPreserves(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "test"}
	mem := &model.UserListMembership{ID: "m1", UserListID: "l1", UserID: "u2", WithReplies: true}
	listRepo.Members = append(listRepo.Members, mem)
	h.SetUserListRepo(listRepo)

	// withReplies 省略 → 既存 true を維持。
	rec := postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"u2"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, mem.WithReplies, "withReplies 省略時は既存値を維持 (#1948-15)")

	// withReplies:false → 明示更新。
	rec = postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"u2","withReplies":false}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.False(t, mem.WithReplies, "明示 false は更新する (#1948-15)")
}

func TestListsUpdateMembership_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsUpdateMembership, `{"listId":"ghost","userId":"u2","withReplies":false}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdateMembership_NotOwner(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "other", Name: "test"}
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"u2","withReplies":true}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsUpdateMembership_NilRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"u2","withReplies":true}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestListsUpdateMembership_MemberNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "test"}
	// メンバーが存在しないためUpdateMembershipがErrNotFoundを返す→404
	h.SetUserListRepo(listRepo)

	rec := postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"ghost","withReplies":true}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #2005: update-membership の意図的挙動を pin する。(1) withReplies 省略 (nil) は既存値を
// 維持して 204 (upstream は TypeORM 空 SET で 500 だが mk-go は堅牢挙動を採る)。
// (2) member でない user 指定は NO_SUCH_USER 404 (upstream は user 存在時 500 だが mk-go は 404)。
func TestListsUpdateMembership_DeliberateDeviations(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockUserListRepository()
	h.SetUserListRepo(repo)
	seedListWithMember(repo, "l1", "owner", "member1", false) // WithReplies: true で seed
	owner := &model.User{ID: "owner"}

	// (1) withReplies 省略 → 既存 true を維持、204。
	rec := postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"member1"}`, owner)
	assert.Equal(t, http.StatusNoContent, rec.Code, "省略は 204 (#2005)")
	assert.Equal(t, true, repo.Members[0].WithReplies, "省略時は既存値維持 (#2005)")

	// withReplies=false を明示 → 更新される。
	rec = postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"member1","withReplies":false}`, owner)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, false, repo.Members[0].WithReplies, "明示値は更新 (#2005)")

	// (2) member でない user → NO_SUCH_USER 404。
	rec = postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"notmember"}`, owner)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER", "非 member は NO_SUCH_USER 404 (#2005)")
}

// stubUserListEventPub records PublishUserListEvent calls (#2051)。
type stubUserListEventPub struct {
	calls []struct {
		listID, eventType string
		body              any
	}
}

func (s *stubUserListEventPub) PublishUserListEvent(listID, eventType string, body any) {
	s.calls = append(s.calls, struct {
		listID, eventType string
		body              any
	}{listID, eventType, body})
}

// #2051: update-membership で withReplies を明示変更したときだけ userUpdated を流す。
func TestListsUpdateMembership_EmitsUserUpdated(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockUserListRepository()
	h.SetUserListRepo(repo)
	pub := &stubUserListEventPub{}
	h.SetUserListEventPublisher(pub)
	seedListWithMember(repo, "l1", "owner", "member1", false)
	owner := &model.User{ID: "owner"}

	// withReplies 省略 → event 無し。
	postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"member1"}`, owner)
	assert.Empty(t, pub.calls, "withReplies 省略時は userUpdated を流さない (#2051)")

	// withReplies=true 明示 → userUpdated {id, withReplies:true}。
	postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"member1","withReplies":true}`, owner)
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "l1", pub.calls[0].listID)
	assert.Equal(t, "userUpdated", pub.calls[0].eventType)
	body, ok := pub.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "member1", body["id"])
	assert.Equal(t, true, body["withReplies"])
}
