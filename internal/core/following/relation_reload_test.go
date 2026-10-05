package following_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingReloadPublisher records which users were notified (#2400).
type recordingReloadPublisher struct{ users []string }

func (r *recordingReloadPublisher) PublishFollowingReload(userID string) {
	r.users = append(r.users, userID)
}

// 変わるのは follower 側の snapshot (自分が誰を follow しているか)。followee の
// followingSnapshot は変わらない。
func TestFollow_PublishesReloadForFollower(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	assert.Equal(t, []string{"alice"}, pub.users, "通知先は follower")
}

func TestUnfollow_PublishesReloadForFollower(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)
	require.NoError(t, svc.Unfollow("alice", "bob"))

	assert.Equal(t, []string{"alice"}, pub.users)
}

// **承認制アカウントの経路。** Follow() だけに通知を置くと、AcceptRequest で
// 成立した following が反映されない。設計時のリストから漏れていた箇所。
func TestAcceptRequest_PublishesReloadForFollower(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true) // locked = 承認制
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)
	require.NoError(t, svc.AcceptRequest("bob", "alice"))

	assert.Equal(t, []string{"alice"}, pub.users,
		"承認で following が成立するのは follower 側")
}

// publisher 未配線でも動く。
func TestFollow_WithoutPublisher(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	assert.NoError(t, err)
}

var _ following.RelationReloadPublisher = (*recordingReloadPublisher)(nil)
