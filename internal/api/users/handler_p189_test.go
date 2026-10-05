package users_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/users"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// p189Kit bundles the handler and every mock the P4-7 endpoints touch so a
// single test can populate just what it needs.
type p189Kit struct {
	h        *users.Handler
	userRepo *testutil.MockUserRepository
	noteRepo *testutil.MockNoteRepository
	fRepo    *testutil.MockFollowingRepository
	listRepo *testutil.MockUserListRepository
}

func newP189Kit(t *testing.T) *p189Kit {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	listRepo := testutil.NewMockUserListRepository()
	idGen, _ := id.NewGenerator("aidx")
	userSvc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	h := users.NewHandler(userSvc, fSvc, noteRepo, idGen)
	h.SetFollowingRepo(fRepo)
	h.SetUserListRepo(listRepo)
	return &p189Kit{h: h, userRepo: userRepo, noteRepo: noteRepo, fRepo: fRepo, listRepo: listRepo}
}

func postP189(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = handler(c)
	return rec
}

// --- GetFrequentlyRepliedUsers ---

func TestGetFrequentlyRepliedUsers_AggregatesAndWeights(t *testing.T) {
	k := newP189Kit(t)
	k.userRepo.Users["me"] = &model.User{ID: "me", Username: "me"}
	k.userRepo.Users["t1"] = &model.User{ID: "t1", Username: "t1"}
	k.userRepo.Users["t2"] = &model.User{ID: "t2", Username: "t2"}
	// me → t1 を 3 件, me → t2 を 1 件の reply を作成する
	ridDummy := "anyReply"
	for i, target := range []string{"t1", "t1", "t1", "t2"} {
		uid := target
		nid := "n" + string(rune('a'+i))
		k.noteRepo.Notes[nid] = &model.Note{
			ID:          nid,
			UserID:      "me",
			ReplyID:     &ridDummy,
			ReplyUserID: &uid,
			// visibility push-down (#1486) によって viewer 不一致時は集計から
			// 落ちるため、本テストは public reply での集計挙動を確認する。
			Visibility: model.NoteVisibilityPublic,
		}
	}
	rec := postP189(k.h.GetFrequentlyRepliedUsers, `{"userId":"me","limit":10}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
	// 先頭は t1 (count=3, weight=1.0)、次に t2 (count=1, weight=1/3)。
	assert.Equal(t, 1.0, out[0]["weight"])
	assert.InDelta(t, 1.0/3.0, out[1]["weight"], 1e-9)
}

func TestGetFrequentlyRepliedUsers_MissingUserId(t *testing.T) {
	k := newP189Kit(t)
	rec := postP189(k.h.GetFrequentlyRepliedUsers, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetFrequentlyRepliedUsers_UserNotFound(t *testing.T) {
	k := newP189Kit(t)
	rec := postP189(k.h.GetFrequentlyRepliedUsers, `{"userId":"ghost"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// 第三者 viewer に author の followers/specified reply 対人関係が leak しないこと
// を確認する (#1486)。mock の noteVisibleToViewer が CanSeeNote と同じ条件で
// 絞るため、anonymous は public のみ集計される。
func TestGetFrequentlyRepliedUsers_VisibilityIDOR(t *testing.T) {
	k := newP189Kit(t)
	author := &model.User{ID: "auth", Username: "auth"}
	target1 := &model.User{ID: "tg1", Username: "tg1"}
	target2 := &model.User{ID: "tg2", Username: "tg2"}
	stranger := &model.User{ID: "stg", Username: "stg"}
	follower := &model.User{ID: "flw", Username: "flw"}
	k.userRepo.Users[author.ID] = author
	k.userRepo.Users[target1.ID] = target1
	k.userRepo.Users[target2.ID] = target2
	k.userRepo.Users[stranger.ID] = stranger
	k.userRepo.Users[follower.ID] = follower
	// follower → author (Mock の noteVisibleToViewer は followerID -> followeeIDs map を参照)
	k.noteRepo.Following[follower.ID] = []string{author.ID}

	replyDummy := "src"
	tg1ID, tg2ID := target1.ID, target2.ID
	// author → target1 (public + followers + specified target2)
	k.noteRepo.Notes["np"] = &model.Note{ID: "np", UserID: author.ID, ReplyID: &replyDummy, ReplyUserID: &tg1ID, Visibility: model.NoteVisibilityPublic}
	k.noteRepo.Notes["nf"] = &model.Note{ID: "nf", UserID: author.ID, ReplyID: &replyDummy, ReplyUserID: &tg1ID, Visibility: model.NoteVisibilityFollowers}
	k.noteRepo.Notes["ns"] = &model.Note{ID: "ns", UserID: author.ID, ReplyID: &replyDummy, ReplyUserID: &tg2ID, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{"some-other"}}

	// stranger (匿名相当 = follower でも specified 対象でもない) は public のみ。
	rec := postP189(k.h.GetFrequentlyRepliedUsers, `{"userId":"auth","limit":10}`, stranger)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	u := out[0]["user"].(map[string]any)
	assert.Equal(t, target1.ID, u["id"])

	// anonymous も同じ shape。
	rec = postP189(k.h.GetFrequentlyRepliedUsers, `{"userId":"auth","limit":10}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	out = out[:0]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)

	// follower は public + followers で target1 が count=2、specified は対象外。
	rec = postP189(k.h.GetFrequentlyRepliedUsers, `{"userId":"auth","limit":10}`, follower)
	require.Equal(t, http.StatusOK, rec.Code)
	out = out[:0]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	u = out[0]["user"].(map[string]any)
	assert.Equal(t, target1.ID, u["id"])
}

// p189ModStub implements users.ModeratorChecker for the count-gate tests.
type p189ModStub struct{ mods map[string]bool }

func (m p189ModStub) IsModerator(id string) bool     { return m.mods[id] }
func (m p189ModStub) IsAdministrator(id string) bool { return m.mods[id] }

// seedFollowersOnly registers a user with followersVisibility=followers and
// followersCount=7 so the count-gate behavior can be observed.
func seedFollowersOnly(k *p189Kit, id string) {
	k.userRepo.Users[id] = &model.User{ID: id, Username: id, FollowersCount: 7}
	k.userRepo.Profiles[id] = &model.UserProfile{
		UserID:              id,
		FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}
}

// #1985: get-frequently-replied-users の embed user の followers-only count は
// 非フォロワー viewer に leak しない。
func TestGetFrequentlyRepliedUsers_GatesFollowersCount(t *testing.T) {
	k := newP189Kit(t)
	k.userRepo.Users["auth"] = &model.User{ID: "auth", Username: "auth"}
	seedFollowersOnly(k, "bob")
	replyDummy, bobID := "src", "bob"
	k.noteRepo.Notes["np"] = &model.Note{ID: "np", UserID: "auth", ReplyID: &replyDummy, ReplyUserID: &bobID, Visibility: model.NoteVisibilityPublic}

	// 非フォロワー viewer: count は 0 に潰される。
	rec := postP189(k.h.GetFrequentlyRepliedUsers, `{"userId":"auth","limit":10}`, &model.User{ID: "stranger"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	u := out[0]["user"].(map[string]any)
	assert.EqualValues(t, 0, u["followersCount"], "followers-only count は非フォロワーに leak しない (#1985)")
}

// #1985: moderator viewer には count をそのまま見せる。
func TestGetFrequentlyRepliedUsers_ModeratorSeesFollowersCount(t *testing.T) {
	k := newP189Kit(t)
	k.h.SetModeratorChecker(p189ModStub{mods: map[string]bool{"mod": true}})
	k.userRepo.Users["auth"] = &model.User{ID: "auth", Username: "auth"}
	seedFollowersOnly(k, "bob")
	replyDummy, bobID := "src", "bob"
	k.noteRepo.Notes["np"] = &model.Note{ID: "np", UserID: "auth", ReplyID: &replyDummy, ReplyUserID: &bobID, Visibility: model.NoteVisibilityPublic}

	rec := postP189(k.h.GetFrequentlyRepliedUsers, `{"userId":"auth","limit":10}`, &model.User{ID: "mod"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	u := out[0]["user"].(map[string]any)
	assert.EqualValues(t, 7, u["followersCount"], "moderator viewer には count を見せる (#1985)")
}

// #1985: users/recommendation の embed user の followers-only count も gate する。
func TestUserRecommendation_GatesFollowersCount(t *testing.T) {
	k := newP189Kit(t)
	now := time.Now()
	k.userRepo.Users["me"] = &model.User{ID: "me"}
	// recommend 対象 r1 は followers-only の count。me は r1 を follow していない。
	k.userRepo.Users["r1"] = &model.User{ID: "r1", Username: "r1", IsExplorable: true, UpdatedAt: &now, FollowersCount: 7}
	k.userRepo.Profiles["r1"] = &model.UserProfile{
		UserID:              "r1",
		FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}

	rec := postP189(k.h.UserRecommendation, `{"limit":10}`, &model.User{ID: "me"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.EqualValues(t, 0, out[0]["followersCount"], "followers-only count は非フォロワーに leak しない (#1985)")

	// moderator viewer には count を見せる。
	k.h.SetModeratorChecker(p189ModStub{mods: map[string]bool{"me": true}})
	rec = postP189(k.h.UserRecommendation, `{"limit":10}`, &model.User{ID: "me"})
	require.Equal(t, http.StatusOK, rec.Code)
	out = out[:0]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.EqualValues(t, 7, out[0]["followersCount"], "moderator viewer には count を見せる (#1985)")
}

// --- GetFollowingUsersByBirthday ---

func TestGetFollowingUsersByBirthday_SingleDayMatch(t *testing.T) {
	k := newP189Kit(t)
	k.userRepo.Users["me"] = &model.User{ID: "me"}
	k.userRepo.Users["fe1"] = &model.User{ID: "fe1", Username: "fe1"}
	k.userRepo.Users["fe2"] = &model.User{ID: "fe2", Username: "fe2"}
	k.fRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "me", FolloweeID: "fe1"}
	k.fRepo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "me", FolloweeID: "fe2"}
	k.fRepo.Birthdays["fe1"] = "1990-05-10"
	k.fRepo.Birthdays["fe2"] = "1991-07-01"

	rec := postP189(k.h.GetFollowingUsersByBirthday, `{"birthday":{"month":5,"day":10}}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "fe1", out[0]["id"])
}

func TestGetFollowingUsersByBirthday_RangeWrap(t *testing.T) {
	k := newP189Kit(t)
	k.userRepo.Users["me"] = &model.User{ID: "me"}
	k.userRepo.Users["fe1"] = &model.User{ID: "fe1", Username: "fe1"}
	k.userRepo.Users["fe2"] = &model.User{ID: "fe2", Username: "fe2"}
	k.userRepo.Users["fe3"] = &model.User{ID: "fe3", Username: "fe3"}
	k.fRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "me", FolloweeID: "fe1"}
	k.fRepo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "me", FolloweeID: "fe2"}
	k.fRepo.Followings["f3"] = &model.Following{ID: "f3", FollowerID: "me", FolloweeID: "fe3"}
	k.fRepo.Birthdays["fe1"] = "1990-12-30"
	k.fRepo.Birthdays["fe2"] = "1991-01-03"
	k.fRepo.Birthdays["fe3"] = "1992-06-15"

	rec := postP189(k.h.GetFollowingUsersByBirthday,
		`{"birthday":{"begin":{"month":12,"day":25},"end":{"month":1,"day":5}}}`,
		&model.User{ID: "me"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2) // fe1 と fe2 のみ
}

func TestGetFollowingUsersByBirthday_BadParams(t *testing.T) {
	k := newP189Kit(t)
	// birthday 欠落
	assert.Equal(t, http.StatusBadRequest, postP189(k.h.GetFollowingUsersByBirthday, `{}`, &model.User{ID: "me"}).Code)
	// month/day のみで month が範囲外
	assert.Equal(t, http.StatusBadRequest, postP189(k.h.GetFollowingUsersByBirthday, `{"birthday":{"month":0,"day":1}}`, &model.User{ID: "me"}).Code)
	// begin は有効だが end の月が範囲外
	assert.Equal(t, http.StatusBadRequest, postP189(k.h.GetFollowingUsersByBirthday, `{"birthday":{"begin":{"month":1,"day":1},"end":{"month":13,"day":1}}}`, &model.User{ID: "me"}).Code)
}

// --- UserRecommendation ---

func TestUserRecommendation_SortedByFollowers(t *testing.T) {
	k := newP189Kit(t)
	now := time.Now()
	// me (自分), r1/r2 (local active, 候補), r3 (remote, 除外),
	// r4 (locked, 除外), r5 (non-explorable, 除外)
	k.userRepo.Users["me"] = &model.User{ID: "me"}
	k.userRepo.Users["r1"] = &model.User{ID: "r1", IsExplorable: true, UpdatedAt: &now, FollowersCount: 5}
	k.userRepo.Users["r2"] = &model.User{ID: "r2", IsExplorable: true, UpdatedAt: &now, FollowersCount: 20}
	remote := "remote.example"
	k.userRepo.Users["r3"] = &model.User{ID: "r3", IsExplorable: true, UpdatedAt: &now, FollowersCount: 999, Host: &remote}
	k.userRepo.Users["r4"] = &model.User{ID: "r4", IsExplorable: true, UpdatedAt: &now, FollowersCount: 999, IsLocked: true}
	k.userRepo.Users["r5"] = &model.User{ID: "r5", IsExplorable: false, UpdatedAt: &now, FollowersCount: 999}

	rec := postP189(k.h.UserRecommendation, `{"limit":10}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
	// followersCount DESC
	assert.Equal(t, "r2", out[0]["id"])
	assert.Equal(t, "r1", out[1]["id"])
}

func TestUserRecommendation_ExcludesAlreadyFollowed(t *testing.T) {
	k := newP189Kit(t)
	now := time.Now()
	k.userRepo.Users["me"] = &model.User{ID: "me"}
	k.userRepo.Users["r1"] = &model.User{ID: "r1", IsExplorable: true, UpdatedAt: &now, FollowersCount: 5}
	k.userRepo.Users["r2"] = &model.User{ID: "r2", IsExplorable: true, UpdatedAt: &now, FollowersCount: 20}
	k.userRepo.RecommendationFollowing["me"] = []string{"r2"}

	rec := postP189(k.h.UserRecommendation, `{}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "r1", out[0]["id"])
}

// --- ListsCreateFromPublic ---

func TestListsCreateFromPublic_CopiesMembers(t *testing.T) {
	k := newP189Kit(t)
	src := &model.UserList{ID: "src", UserID: "author", Name: "orig", IsPublic: true}
	k.listRepo.Lists[src.ID] = src
	k.listRepo.Members = append(k.listRepo.Members,
		&model.UserListMembership{ID: "m1", UserListID: src.ID, UserID: "u1"},
		&model.UserListMembership{ID: "m2", UserListID: src.ID, UserID: "u2"},
	)

	rec := postP189(k.h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// #1550: res は UserList packer shape ({id,createdAt,name,userIds,isPublic})。
	// model.UserList 生 JSON の userId は露出しない。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "mine", resp["name"])
	assert.Equal(t, false, resp["isPublic"])
	assert.NotContains(t, resp, "userId", "userId は露出しない")
	assert.NotEmpty(t, resp["createdAt"], "createdAt が含まれる")
	assert.ElementsMatch(t, []any{"u1", "u2"}, resp["userIds"], "コピーされた member が userIds に反映される")

	// 新 list に u1, u2 が追加されているはず。
	newID := resp["id"].(string)
	copied := 0
	for _, m := range k.listRepo.Members {
		if m.UserListID == newID {
			copied++
		}
	}
	assert.Equal(t, 2, copied)
}

func TestListsCreateFromPublic_NonPublicRejected(t *testing.T) {
	k := newP189Kit(t)
	src := &model.UserList{ID: "src", UserID: "author", Name: "orig", IsPublic: false}
	k.listRepo.Lists[src.ID] = src
	rec := postP189(k.h.ListsCreateFromPublic, `{"listId":"src","name":"mine"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestListsCreateFromPublic_InvalidParam(t *testing.T) {
	k := newP189Kit(t)
	assert.Equal(t, http.StatusBadRequest, postP189(k.h.ListsCreateFromPublic, `{}`, &model.User{ID: "me"}).Code)
}
