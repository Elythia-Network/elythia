package users

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- S1: users/show bulk が UserDetailed を返す ---

func TestShow_BulkUserIDs_ReturnsUserDetailed(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	rec := post(h.Show, `{"userIds":["u1"]}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	// UserDetailed 固有フィールド (UserLite には無い) が present であること (#1547)。
	_, hasFollowers := out[0]["followersCount"]
	_, hasCreatedAt := out[0]["createdAt"]
	assert.True(t, hasFollowers, "bulk は UserDetailed なので followersCount を持つ")
	assert.True(t, hasCreatedAt, "bulk は UserDetailed なので createdAt を持つ")
}

// --- R1: users/relation が userId 配列を受けて配列を返す ---

func TestRelation_ArrayForm(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.Relation, `{"userId":["u2","u3"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2, "配列入力には配列で応答する (#1547)")
	assert.Equal(t, "u2", out[0]["id"])
	assert.Equal(t, "u3", out[1]["id"])
}

// #1766: upstream relation.ts は単一 userId (string) でも単一要素配列を返す
// (`.then(it => [it])`)。mk-go も配列に揃える。
func TestRelation_SingleFormReturnsArray(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.Relation, `{"userId":"u2"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "単体入力でも単一要素配列で応答する")
	require.Len(t, out, 1)
	assert.Equal(t, "u2", out[0]["id"])
}

// --- N3 / X1 / X4: viewer が target にブロックされている場合の早期 [] ---

// blocked = target(req.UserID) が viewer を block している状態を作る。
func wireBlock(h *Handler, blockerID, blockeeID string) *testutil.MockBlockingRepository {
	br := testutil.NewMockBlockingRepository()
	_ = br.Create(&model.Blocking{ID: "b1", BlockerID: blockerID, BlockeeID: blockeeID})
	h.SetBlockingRepo(br)
	return br
}

func TestNotes_BlockedByTargetReturnsEmpty(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["target"] = &model.User{ID: "target", Username: "t", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	wireBlock(h, "target", "viewer") // target blocks viewer
	rec := postStub(h.Notes, `{"userId":"target"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestReactions_BlockedByTargetReturnsEmpty(t *testing.T) {
	h, _ := newTestHandler(t)
	wireBlock(h, "target", "viewer")
	rec := postStub(h.Reactions, `{"userId":"target"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestFeaturedNotes_BlockedByTargetReturnsEmpty(t *testing.T) {
	h, _ := newTestHandler(t)
	wireBlock(h, "target", "viewer")
	rec := postStub(h.FeaturedNotes, `{"userId":"target"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// --- R3 / R2: search detail param ---

func hasDetailedField(m map[string]any) bool {
	_, ok := m["followersCount"]
	return ok
}

func TestSearch_DetailFalseReturnsUserLite(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	rec := post(h.Search, `{"query":"test","detail":false}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.False(t, hasDetailedField(out[0]), "detail=false は UserLite (followersCount 無し、#1547)")
}

func TestSearchByUsernameAndHost_DefaultIsUserDetailed(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	rec := postStub(h.SearchByUsernameAndHost, `{"username":"testuser"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.True(t, hasDetailedField(out[0]), "default は UserDetailed (followersCount あり、#1547)")
}

func TestSearchByUsernameAndHost_DetailFalseUserLite(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	rec := postStub(h.SearchByUsernameAndHost, `{"username":"testuser","detail":false}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.False(t, hasDetailedField(out[0]), "detail=false は UserLite (#1547)")
}

// #1980: users/search の Detail (default) 経路の embed user に viewer 視点の relation
// block が乗る。viewer が結果 user を follow していれば isFollowing=true、匿名なら省略。
func TestSearch_DetailEmbedsViewerRelation(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 / testuser
	fRepo := testutil.NewMockFollowingRepository()
	require.NoError(t, fRepo.Create(&model.Following{ID: "f1", FollowerID: "viewer1", FolloweeID: "user1"}))
	h.SetFollowingRepo(fRepo)

	rec := postStub(h.Search, `{"query":"test"}`, &model.User{ID: "viewer1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0]["isFollowing"], "search detail の embed user に isFollowing=true (#1980)")

	rec = post(h.Search, `{"query":"test"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var anon []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anon))
	require.Len(t, anon, 1)
	_, has := anon[0]["isFollowing"]
	assert.False(t, has, "匿名 search には relation を出さない (#1980)")
}

// #1980: search-by-username-and-host の Detail (default) 経路にも relation block。
func TestSearchByUsernameAndHost_EmbedsViewerRelation(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	fRepo := testutil.NewMockFollowingRepository()
	require.NoError(t, fRepo.Create(&model.Following{ID: "f1", FollowerID: "viewer1", FolloweeID: "user1"}))
	h.SetFollowingRepo(fRepo)

	rec := postStub(h.SearchByUsernameAndHost, `{"username":"testuser"}`, &model.User{ID: "viewer1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0]["isFollowing"], "search-by-username-and-host の embed user に isFollowing=true (#1980)")

	rec = postStub(h.SearchByUsernameAndHost, `{"username":"testuser"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var anon []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anon))
	require.Len(t, anon, 1)
	_, has := anon[0]["isFollowing"]
	assert.False(t, has, "匿名には relation を出さない (#1980)")
}

// #1980: search-by-username-and-host は followers-visibility の count を非フォロワーに
// leak させない (count gate を users/search と対称に追加した)。
func TestSearchByUsernameAndHost_GatesFollowersCount(t *testing.T) {
	h, repo := newTestHandler(t)
	u := addTestUser(repo) // user1 / testuser
	u.FollowersCount = 7
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:              "user1",
		FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}

	// 非フォロワー viewer: followersVisibility=followers の count は 0 に潰される。
	rec := postStub(h.SearchByUsernameAndHost, `{"username":"testuser"}`, &model.User{ID: "stranger"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.EqualValues(t, 0, out[0]["followersCount"], "followers-only count は非フォロワーに leak しない (#1980)")
}

// --- S5: moderator viewer sees moderationNote ---

type modStub struct{}

func (modStub) IsModerator(string) bool     { return true }
func (modStub) IsAdministrator(string) bool { return true }

func TestShow_ModeratorSeesModerationNote(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	h.SetModeratorChecker(modStub{})
	rec := postStub(h.Show, `{"userId":"user1"}`, &model.User{ID: "mod"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	_, ok := out["moderationNote"]
	assert.True(t, ok, "moderator viewer には moderationNote が present (#1547)")
}

func TestShow_NonModeratorOmitsModerationNote(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	rec := postStub(h.Show, `{"userId":"user1"}`, &model.User{ID: "someone"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	_, ok := out["moderationNote"]
	assert.False(t, ok, "非 moderator には moderationNote を omit (#1547)")
}

// #2106 N2: users/show の userIds バルクモードも viewer relation (isFollowing 等) を emit する。
func TestShow_BulkUserIDs_EmbedsViewerRelation(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 / testuser
	fRepo := testutil.NewMockFollowingRepository()
	require.NoError(t, fRepo.Create(&model.Following{ID: "f1", FollowerID: "viewer1", FolloweeID: "user1"}))
	h.SetFollowingRepo(fRepo)

	rec := postStub(h.Show, `{"userIds":["user1"]}`, &model.User{ID: "viewer1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0]["isFollowing"], "bulk show の user に viewer relation (isFollowing=true)")

	// 匿名 bulk には relation を出さない。
	rec = post(h.Show, `{"userIds":["user1"]}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var anon []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anon))
	require.Len(t, anon, 1)
	_, has := anon[0]["isFollowing"]
	assert.False(t, has, "匿名 bulk show には relation を出さない")
}
