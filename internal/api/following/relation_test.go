package following

import (
	"encoding/json"
	"net/http"
	"testing"

	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvalidate_MissingUserIDRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, postJSON(h.Invalidate, `{}`, &model.User{ID: "admin"}).Code)
}

func TestUpdateFollow_MissingUserIDRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, postJSON(h.UpdateFollow, `{}`, &model.User{ID: "u1"}).Code)
}

func TestUpdateFollowAll_NoFieldsNoOp(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, postJSON(h.UpdateFollowAll, `{}`, &model.User{ID: "u1"}).Code)
}

func TestInvalidate_Success(t *testing.T) {
	h, userRepo := newTestHandler(t)
	// admin に follower を follow させた状態を作る
	admin := &model.User{ID: "admin", Username: "admin", UsernameLower: "admin"}
	follower := &model.User{ID: "follower", Username: "f", UsernameLower: "f"}
	userRepo.Users[admin.ID] = admin
	userRepo.Users[follower.ID] = follower
	_, _ = h.followingService.Follow(follower.ID, admin.ID, corefollowing.FollowOptions{})

	rec := postJSON(h.Invalidate, `{"userId":"follower"}`, admin)
	// upstream invalidate.ts は packed follower (UserLite) を 200 で返す (#1562)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "follower", body["id"])
}

func TestInvalidate_NotFollowing(t *testing.T) {
	h, userRepo := newTestHandler(t)
	admin := &model.User{ID: "admin", Username: "admin", UsernameLower: "admin"}
	other := &model.User{ID: "other", Username: "o", UsernameLower: "o"}
	userRepo.Users[admin.ID] = admin
	userRepo.Users[other.ID] = other
	// other は admin をフォローしていない → NOT_FOLLOWING
	rec := postJSON(h.Invalidate, `{"userId":"other"}`, admin)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdateFollow_Success(t *testing.T) {
	h, repo, fRepo, _ := newTestHandlerWithRepos(t)
	alice := addUser(repo, "alice", false)
	bob := addUser(repo, "bob", false)
	fRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: alice.ID, FolloweeID: bob.ID}
	rec := postJSON(h.UpdateFollow, `{"userId":"bob","notify":"normal","withReplies":true}`, alice)
	// upstream update.ts は packed follower (= 自分) を 200 で返す (#1562)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, alice.ID, body["id"])
}

// upstream update.ts は self / 不在 user / 未フォローをそれぞれ
// FOLLOWEE_IS_YOURSELF / NO_SUCH_USER / NOT_FOLLOWING で拒否する (#1562)。
func TestUpdateFollow_SelfRejected(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	rec := postJSON(h.UpdateFollow, `{"userId":"alice"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "FOLLOWEE_IS_YOURSELF")
	assert.Contains(t, rec.Body.String(), "4c4cbaf9-962a-463b-8418-a5e365dbf2eb")
}

func TestUpdateFollow_NoSuchUser(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	rec := postJSON(h.UpdateFollow, `{"userId":"ghost"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "14318698-f67e-492a-99da-5353a5ac52be")
}

func TestUpdateFollow_NotFollowing(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := addUser(repo, "alice", false)
	addUser(repo, "bob", false)
	rec := postJSON(h.UpdateFollow, `{"userId":"bob"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_FOLLOWING")
	assert.Contains(t, rec.Body.String(), "b8dc75cf-1cb5-46c9-b14b-5f1ffbd782c9")
}

// invalidate も self / 不在 user を upstream と同じエラーで拒否する (#1562)。
func TestInvalidate_SelfRejected(t *testing.T) {
	h, repo := newTestHandler(t)
	admin := addUser(repo, "admin", false)
	rec := postJSON(h.Invalidate, `{"userId":"admin"}`, admin)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "FOLLOWER_IS_YOURSELF")
	assert.Contains(t, rec.Body.String(), "07dc03b9-03da-422d-885b-438313707662")
}

func TestInvalidate_NoSuchUser(t *testing.T) {
	h, repo := newTestHandler(t)
	admin := addUser(repo, "admin", false)
	rec := postJSON(h.Invalidate, `{"userId":"ghost"}`, admin)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "b77e6ae6-a3e5-40da-9cc8-c240115479cc")
}

func TestUpdateFollowAll_WithFields(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent,
		postJSON(h.UpdateFollowAll, `{"notify":"none"}`, &model.User{ID: "u1"}).Code)
}

// upstream Misskey TS 2026.5.2 #17385 互換: notify="none" は SQL NULL に
// 変換されて DB に書かれる。これがないと /api/following/list の
// notification=true filter (= notify IS NOT NULL) が機能しない。
func TestUpdateFollow_NotifyNone_NormalizedToNull(t *testing.T) {
	h, repo, fRepo, _ := newTestHandlerWithRepos(t)
	alice := addUser(repo, "alice", false)
	bob := addUser(repo, "bob", false)
	// 既存 follow + notify=normal 状態を pre-seed
	notify := "normal"
	fRepo.Followings["f1"] = &model.Following{
		ID:         "f1",
		FollowerID: alice.ID,
		FolloweeID: bob.ID,
		Notify:     &notify,
	}

	rec := postJSON(h.UpdateFollow, `{"userId":"bob","notify":"none"}`, alice)
	assert.Equal(t, http.StatusOK, rec.Code)
	// notify="none" → DB の Notify 列が nil (= SQL NULL) になる
	assert.Nil(t, fRepo.Followings["f1"].Notify, "notify=none must normalize to nil for following/list notification=true filter")
}

// notify="normal" は文字列のまま保存される (= notify ON 状態を表す)。
func TestUpdateFollow_NotifyNormal_StoredAsString(t *testing.T) {
	h, repo, fRepo, _ := newTestHandlerWithRepos(t)
	alice := addUser(repo, "alice", false)
	bob := addUser(repo, "bob", false)
	fRepo.Followings["f1"] = &model.Following{
		ID:         "f1",
		FollowerID: alice.ID,
		FolloweeID: bob.ID,
	}

	rec := postJSON(h.UpdateFollow, `{"userId":"bob","notify":"normal"}`, alice)
	assert.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, fRepo.Followings["f1"].Notify)
	assert.Equal(t, "normal", *fRepo.Followings["f1"].Notify)
}

// #2027: notify enum (normal/none) 外は 400 で弾く (verbatim 保存しない)。
func TestUpdateFollow_InvalidNotifyEnum(t *testing.T) {
	h, repo, fRepo, _ := newTestHandlerWithRepos(t)
	alice := addUser(repo, "alice", false)
	bob := addUser(repo, "bob", false)
	fRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: alice.ID, FolloweeID: bob.ID}
	rec := postJSON(h.UpdateFollow, `{"userId":"bob","notify":"bogus"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "notify enum 外は 400 (#2027)")
}

// #2027: update-all も notify enum 外を 400 で弾く。
func TestUpdateFollowAll_InvalidNotifyEnum(t *testing.T) {
	h, repo, _, _ := newTestHandlerWithRepos(t)
	alice := addUser(repo, "alice", false)
	rec := postJSON(h.UpdateFollowAll, `{"notify":"bogus"}`, alice)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "update-all notify enum 外は 400 (#2027)")
}
