package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **モデレーター向けの応答にはカウントを出すこと。**
//
// packer は `followersVisibility` / `followingVisibility` が public でない
// カウントを既定で伏せる (呼び忘れても漏れないようにするため)。admin 経路で
// ゲートを通さないと、モデレーション画面で全員のフォロー数が 0 になる。
// upstream `UserEntityService.pack` は `isMe || iAmModerator` に実数を返す。
func TestShowUsers_ShowsCountsToModerator(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{
		ID: "u1", Username: "a", FollowersCount: 42, FollowingCount: 7,
	}
	userRepo.Profiles["u1"] = &model.UserProfile{
		UserID: "u1", FollowersVisibility: "followers", FollowingVisibility: "private",
	}

	rec := doPost(h.ShowUsers, `{"limit":10}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	// 期待値はリテラル。非公開の設定でもモデレーターには実数が出る。
	assert.EqualValues(t, 42, resp[0]["followersCount"])
	assert.EqualValues(t, 7, resp[0]["followingCount"])
}

// find-by-email は本家が pack(user, null) で組むので、呼んだ管理者にも
// 非公開のカウントを見せない (匿名の閲覧者と同じ、#3330)。public は見える。
func TestAccountsFindByEmail_GatesCountsAsAnonymous(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{
		ID: "u1", Username: "a", FollowersCount: 42, FollowingCount: 7,
	}
	userRepo.Profiles["u1"] = &model.UserProfile{
		UserID: "u1", Email: strptr("x@example.test"),
		FollowersVisibility: "followers", FollowingVisibility: "private",
	}

	rec := doPost(h.AccountsFindByEmail, `{"email":"x@example.test"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 0, resp["followersCount"])
	assert.EqualValues(t, 0, resp["followingCount"])
	_, hasNote := resp["moderationNote"]
	assert.False(t, hasNote, "閲覧者 null なので moderationNote は出さない")

	userRepo.Profiles["u1"].FollowersVisibility = "public"
	rec = doPost(h.AccountsFindByEmail, `{"email":"x@example.test"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	resp = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 42, resp["followersCount"])
}

func strptr(s string) *string { return &s }
