package userrelation

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullRepos wires every relation repo with the given (viewer, target) state and
// returns a Repos plus the target user/profile.
func fullRepos(t *testing.T) (Repos, *model.User, *model.UserProfile) {
	t.Helper()
	return Repos{
		Following:     testutil.NewMockFollowingRepository(),
		Blocking:      testutil.NewMockBlockingRepository(),
		Muting:        testutil.NewMockMutingRepository(),
		RenoteMuting:  testutil.NewMockRenoteMutingRepository(),
		FollowRequest: testutil.NewMockFollowRequestRepository(),
		Memo:          testutil.NewMockUserMemoRepository(),
	}, &model.User{ID: "target"}, &model.UserProfile{UserID: "target"}
}

func TestApply_NoOpConditions(t *testing.T) {
	r, target, profile := fullRepos(t)
	d := entity.UserDetailed{}

	// nil detailed
	assert.False(t, r.Apply(nil, "viewer", target, profile))
	// nil target
	assert.False(t, r.Apply(&d, "viewer", nil, profile))
	// empty viewerID (anonymous)
	assert.False(t, r.Apply(&d, "", target, profile))
	// self view
	assert.False(t, r.Apply(&d, "target", target, profile))
	// 何も書き込まれていないこと。
	assert.Nil(t, d.IsFollowing)
	assert.Nil(t, d.IsBlocking)
}

func TestApply_FullRelationBlock(t *testing.T) {
	r, target, profile := fullRepos(t)
	following := r.Following.(*testutil.MockFollowingRepository)
	blocking := r.Blocking.(*testutil.MockBlockingRepository)
	muting := r.Muting.(*testutil.MockMutingRepository)
	renoteMuting := r.RenoteMuting.(*testutil.MockRenoteMutingRepository)
	followReq := r.FollowRequest.(*testutil.MockFollowRequestRepository)
	memo := r.Memo.(*testutil.MockUserMemoRepository)

	// viewer follows target (notify=normal, withReplies=true), target blocks back,
	// viewer mutes + renote-mutes target, mutual pending follow requests, memo.
	notify := "normal"
	require.NoError(t, following.Create(&model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "target", Notify: &notify, WithReplies: true}))
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "target", BlockeeID: "viewer"}))
	require.NoError(t, muting.Create(&model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "target"}))
	require.NoError(t, renoteMuting.Create(&model.RenoteMuting{ID: "rm1", MuterID: "viewer", MuteeID: "target"}))
	require.NoError(t, followReq.Create(&model.FollowRequest{ID: "fr1", FollowerID: "viewer", FolloweeID: "target"}))
	require.NoError(t, followReq.Create(&model.FollowRequest{ID: "fr2", FollowerID: "target", FolloweeID: "viewer"}))
	require.NoError(t, memo.CreateOrUpdate(&model.UserMemo{ID: "memo1", UserID: "viewer", TargetUserID: "target", Memo: "a note"}))
	msg := "thanks for following"
	profile.FollowedMessage = &msg

	d := entity.UserDetailed{}
	got := r.Apply(&d, "viewer", target, profile)

	assert.True(t, got, "Apply returns whether viewer follows target")
	require.NotNil(t, d.IsFollowing)
	assert.True(t, *d.IsFollowing)
	require.NotNil(t, d.IsFollowed)
	assert.False(t, *d.IsFollowed, "target does not follow viewer")
	require.NotNil(t, d.Notify)
	assert.Equal(t, "normal", *d.Notify)
	require.NotNil(t, d.WithReplies)
	assert.True(t, *d.WithReplies)
	require.NotNil(t, d.FollowedMessage)
	assert.JSONEq(t, `"thanks for following"`, string(d.FollowedMessage))
	require.NotNil(t, d.IsBlocked)
	assert.True(t, *d.IsBlocked, "target blocks viewer")
	require.NotNil(t, d.IsBlocking)
	assert.False(t, *d.IsBlocking)
	require.NotNil(t, d.IsMuted)
	assert.True(t, *d.IsMuted)
	require.NotNil(t, d.IsRenoteMuted)
	assert.True(t, *d.IsRenoteMuted)
	require.NotNil(t, d.HasPendingFollowRequestFromYou)
	assert.True(t, *d.HasPendingFollowRequestFromYou)
	require.NotNil(t, d.HasPendingFollowRequestToYou)
	assert.True(t, *d.HasPendingFollowRequestToYou)
	require.NotNil(t, d.Memo)
	assert.Equal(t, "a note", *d.Memo)
}

// 関係が無いときのデフォルト: notify='none' / withReplies=false / followedMessage 無し。
func TestApply_NoRelationDefaults(t *testing.T) {
	r, target, profile := fullRepos(t)
	msg := "secret"
	profile.FollowedMessage = &msg

	d := entity.UserDetailed{}
	got := r.Apply(&d, "viewer", target, profile)

	assert.False(t, got)
	require.NotNil(t, d.IsFollowing)
	assert.False(t, *d.IsFollowing)
	require.NotNil(t, d.Notify)
	assert.Equal(t, "none", *d.Notify, "follow row が無くても notify は 'none' を出す")
	require.NotNil(t, d.WithReplies)
	assert.False(t, *d.WithReplies)
	assert.Nil(t, d.FollowedMessage, "非 follower には followedMessage を出さない")
	require.NotNil(t, d.IsBlocking)
	assert.False(t, *d.IsBlocking)
	assert.Nil(t, d.Memo)
}

// follow row はあるが notify 未設定なら notify='none' に default する。
func TestApply_FollowWithoutNotify(t *testing.T) {
	r, target, profile := fullRepos(t)
	following := r.Following.(*testutil.MockFollowingRepository)
	require.NoError(t, following.Create(&model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "target"}))

	d := entity.UserDetailed{}
	r.Apply(&d, "viewer", target, profile)
	require.NotNil(t, d.Notify)
	assert.Equal(t, "none", *d.Notify)
	// follower なので followedMessage を出す。profile に無くても upstream 互換で
	// null を emit する (#2097)。
	assert.Equal(t, json.RawMessage("null"), d.FollowedMessage)
}

// nil repo は対応する flag を skip する (= omit)。
func TestApply_PartialReposSkipFlags(t *testing.T) {
	r := Repos{Blocking: testutil.NewMockBlockingRepository()} // blocking のみ wire
	blocking := r.Blocking.(*testutil.MockBlockingRepository)
	require.NoError(t, blocking.Create(&model.Blocking{ID: "b1", BlockerID: "viewer", BlockeeID: "target"}))

	d := entity.UserDetailed{}
	got := r.Apply(&d, "viewer", &model.User{ID: "target"}, nil)

	assert.False(t, got, "following repo 未配線なので false")
	assert.Nil(t, d.IsFollowing, "following 未配線なら isFollowing は omit")
	assert.Nil(t, d.IsMuted, "muting 未配線なら isMuted は omit")
	require.NotNil(t, d.IsBlocking, "blocking は wire 済みなので emit")
	assert.True(t, *d.IsBlocking)
}
