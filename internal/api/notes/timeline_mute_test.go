package notes

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMutedChannelIDs(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockChannelMutingRepository()
	require.NoError(t, repo.Create(&model.ChannelMuting{ID: "m1", UserID: "viewer", ChannelID: "ch-muted-1"}))
	require.NoError(t, repo.Create(&model.ChannelMuting{ID: "m2", UserID: "viewer", ChannelID: "ch-muted-2"}))
	require.NoError(t, repo.Create(&model.ChannelMuting{ID: "m3", UserID: "other", ChannelID: "ch-other"}))
	h.SetChannelMutingRepo(repo)

	viewer := &model.User{ID: "viewer"}
	ids := h.loadMutedChannelIDs(viewer)
	assert.ElementsMatch(t, []string{"ch-muted-1", "ch-muted-2"}, ids)
}

func TestLoadMutedChannelIDs_AnonymousReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetChannelMutingRepo(testutil.NewMockChannelMutingRepository())
	assert.Nil(t, h.loadMutedChannelIDs(nil))
}

func TestLoadMutedChannelIDs_RepoUnsetReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Nil(t, h.loadMutedChannelIDs(&model.User{ID: "viewer"}))
}

func TestLoadMutedChannelIDs_EmptyResultReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockChannelMutingRepository()
	h.SetChannelMutingRepo(repo)
	assert.Nil(t, h.loadMutedChannelIDs(&model.User{ID: "viewer"}))
}

// loadRenoteMutedUserIDs は viewer の renote-mute mutee 一覧を返す (#903)。
// MutedChannelIDs と同 pattern で 4 case (基本 / anonymous / repo nil /
// 空結果) を guard する。internal/api/notes の coverage 90% threshold を
// 維持するために必須。
func TestLoadRenoteMutedUserIDs(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockRenoteMutingRepository()
	require.NoError(t, repo.Create(&model.RenoteMuting{ID: "rm1", MuterID: "viewer", MuteeID: "muted-1"}))
	require.NoError(t, repo.Create(&model.RenoteMuting{ID: "rm2", MuterID: "viewer", MuteeID: "muted-2"}))
	require.NoError(t, repo.Create(&model.RenoteMuting{ID: "rm3", MuterID: "other", MuteeID: "other-mute"}))
	h.SetRenoteMutingRepo(repo)

	viewer := &model.User{ID: "viewer"}
	ids := h.loadRenoteMutedUserIDs(viewer)
	assert.ElementsMatch(t, []string{"muted-1", "muted-2"}, ids)
}

func TestLoadRenoteMutedUserIDs_AnonymousReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetRenoteMutingRepo(testutil.NewMockRenoteMutingRepository())
	assert.Nil(t, h.loadRenoteMutedUserIDs(nil))
}

func TestLoadRenoteMutedUserIDs_RepoUnsetReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Nil(t, h.loadRenoteMutedUserIDs(&model.User{ID: "viewer"}))
}

func TestLoadRenoteMutedUserIDs_EmptyResultReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockRenoteMutingRepository()
	h.SetRenoteMutingRepo(repo)
	assert.Nil(t, h.loadRenoteMutedUserIDs(&model.User{ID: "viewer"}))
}

// loadFollowedChannelIDs は viewer が follow している channel id を返し、mute
// 済 channel を除外する (#1686)。
func TestLoadFollowedChannelIDs(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockChannelFollowingRepository()
	repo.Followings["cf1"] = &model.ChannelFollowing{ID: "cf1", FollowerID: "viewer", FolloweeID: "ch-f1"}
	repo.Followings["cf2"] = &model.ChannelFollowing{ID: "cf2", FollowerID: "viewer", FolloweeID: "ch-f2"}
	repo.Followings["cf3"] = &model.ChannelFollowing{ID: "cf3", FollowerID: "other", FolloweeID: "ch-x"}
	h.SetChannelFollowingRepo(repo)
	// ch-f2 は mute されているので除外される。
	muting := testutil.NewMockChannelMutingRepository()
	require.NoError(t, muting.Create(&model.ChannelMuting{ID: "m1", UserID: "viewer", ChannelID: "ch-f2"}))
	h.SetChannelMutingRepo(muting)

	ids := h.loadFollowedChannelIDs(&model.User{ID: "viewer"})
	assert.ElementsMatch(t, []string{"ch-f1"}, ids)
}

func TestLoadFollowedChannelIDs_NoMute(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockChannelFollowingRepository()
	repo.Followings["cf1"] = &model.ChannelFollowing{ID: "cf1", FollowerID: "viewer", FolloweeID: "ch-f1"}
	repo.Followings["cf2"] = &model.ChannelFollowing{ID: "cf2", FollowerID: "viewer", FolloweeID: "ch-f2"}
	h.SetChannelFollowingRepo(repo)
	ids := h.loadFollowedChannelIDs(&model.User{ID: "viewer"})
	assert.ElementsMatch(t, []string{"ch-f1", "ch-f2"}, ids)
}

func TestLoadFollowedChannelIDs_AllMutedReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockChannelFollowingRepository()
	repo.Followings["cf1"] = &model.ChannelFollowing{ID: "cf1", FollowerID: "viewer", FolloweeID: "ch-f1"}
	h.SetChannelFollowingRepo(repo)
	muting := testutil.NewMockChannelMutingRepository()
	require.NoError(t, muting.Create(&model.ChannelMuting{ID: "m1", UserID: "viewer", ChannelID: "ch-f1"}))
	h.SetChannelMutingRepo(muting)
	assert.Nil(t, h.loadFollowedChannelIDs(&model.User{ID: "viewer"}))
}

func TestLoadFollowedChannelIDs_AnonymousReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetChannelFollowingRepo(testutil.NewMockChannelFollowingRepository())
	assert.Nil(t, h.loadFollowedChannelIDs(nil))
}

func TestLoadFollowedChannelIDs_RepoUnsetReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Nil(t, h.loadFollowedChannelIDs(&model.User{ID: "viewer"}))
}

func TestLoadFollowedChannelIDs_EmptyResultReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetChannelFollowingRepo(testutil.NewMockChannelFollowingRepository())
	assert.Nil(t, h.loadFollowedChannelIDs(&model.User{ID: "viewer"}))
}

// loadFollowingIDs は viewer のフォロイー集合を返す。HTL / STL の
// 「返信先が followers 限定の投稿」ガードで使う (upstream timeline.ts の
// noteFilter が followings を引くのと同じ)。
func TestLoadFollowingIDs(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockFollowingRepository()
	require.NoError(t, repo.Create(&model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "bob"}))
	require.NoError(t, repo.Create(&model.Following{ID: "f2", FollowerID: "viewer", FolloweeID: "carol"}))
	require.NoError(t, repo.Create(&model.Following{ID: "f3", FollowerID: "other", FolloweeID: "dave"}))
	h.SetUserFollowingRepo(repo)

	got := h.loadFollowingIDs(&model.User{ID: "viewer"})
	require.NotNil(t, got)
	_, hasBob := got["bob"]
	_, hasCarol := got["carol"]
	_, hasDave := got["dave"]
	assert.True(t, hasBob)
	assert.True(t, hasCarol)
	assert.False(t, hasDave, "他人のフォローは含めない")
}

func TestLoadFollowingIDs_AnonymousReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetUserFollowingRepo(testutil.NewMockFollowingRepository())
	assert.Nil(t, h.loadFollowingIDs(nil))
}

func TestLoadFollowingIDs_RepoUnsetReturnsNil(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.Nil(t, h.loadFollowingIDs(&model.User{ID: "viewer"}))
}

// フォローが 0 件でも非 nil を返す。nil にすると「未配線」と区別が付かず、
// ガードが丸ごと無効になってしまう。
func TestLoadFollowingIDs_EmptyIsNotNil(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetUserFollowingRepo(testutil.NewMockFollowingRepository())
	got := h.loadFollowingIDs(&model.User{ID: "viewer"})
	require.NotNil(t, got)
	assert.Empty(t, got)
}
