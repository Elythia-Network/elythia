package blocking_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

type recordingUnfollowPublisher struct{ calls [][2]string }

func (r *recordingUnfollowPublisher) PublishBlockUnfollow(followerID, followeeID string) {
	r.calls = append(r.calls, [2]string{followerID, followeeID})
}

func seedFollow(t *testing.T, fr *testutil.MockFollowingRepository, followerID, followeeID string) {
	t.Helper()
	require.NoError(t, fr.Create(&model.Following{ID: followerID + "-" + followeeID, FollowerID: followerID, FolloweeID: followeeID}))
}

// 本家 UserBlockingService.block は UserFollowingService.unfollow を双方向で
// 呼ぶので、外れたフォローごとに unfollow を出す。
func TestBlock_PublishesUnfollowForRemovedFollowings(t *testing.T) {
	svc, ur, _, fr := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	seedFollow(t, fr, "a", "b")
	seedFollow(t, fr, "b", "a")
	pub := &recordingUnfollowPublisher{}
	svc.SetUnfollowPublisher(pub)

	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"a", "b"}, {"b", "a"}}, pub.calls)
}

// フォローが無い向きには出さない (本家の unfollow も行が無ければ何もしない)。
func TestBlock_NoUnfollowWithoutFollowing(t *testing.T) {
	svc, ur, _, fr := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	seedFollow(t, fr, "b", "a")
	pub := &recordingUnfollowPublisher{}
	svc.SetUnfollowPublisher(pub)

	_, err := svc.Block("a", "b")
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"b", "a"}}, pub.calls)
}

// インポートしたブロック (silent) は unfollow を出さず、申請の取り消しにも
// silent を渡す (本家 ImportBlockingProcessorService は silent: true)。
func TestBlockSilent_SuppressesUnfollow(t *testing.T) {
	svc, ur, _, fr := newSvc(t)
	addUser(ur, "a")
	addUser(ur, "b")
	seedFollow(t, fr, "a", "b")
	pub := &recordingUnfollowPublisher{}
	svc.SetUnfollowPublisher(pub)
	canceller := &recordingFollowRequestCanceller{}
	svc.SetFollowRequestCanceller(canceller)

	_, err := svc.BlockSilent("a", "b")
	require.NoError(t, err)
	assert.Empty(t, pub.calls)
	assert.Equal(t, []bool{true}, canceller.silent)
	blocked, err := svc.IsBlocked("a", "b")
	require.NoError(t, err)
	assert.True(t, blocked)
	exists, err := fr.Exists("a", "b")
	require.NoError(t, err)
	assert.False(t, exists, "silent でもフォローは外す")
}

func TestHasUnfollowPublisher(t *testing.T) {
	var empty blocking.Service
	assert.False(t, empty.HasUnfollowPublisher())
	svc, _, _, _ := newSvc(t)
	svc.SetUnfollowPublisher(&recordingUnfollowPublisher{})
	assert.True(t, svc.HasUnfollowPublisher())
	other, _, _, _ := newSvc(t)
	other.SetFollowRequestCanceller(&recordingFollowRequestCanceller{})
	assert.False(t, other.HasUnfollowPublisher(), "別の依存だけを配線しても false")
}
