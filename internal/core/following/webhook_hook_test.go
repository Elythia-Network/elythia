package following_test

import (
	"sync"
	"testing"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingWebhookHook captures webhook hook calls for assertions.
type recordingWebhookHook struct {
	mu           sync.Mutex
	follows      int
	unfollows    int
	followedFlag int
}

func (r *recordingWebhookHook) OnFollow(_, _ *model.User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.follows++
}

func (r *recordingWebhookHook) OnUnfollow(_, _ *model.User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unfollows++
}

func (r *recordingWebhookHook) OnFollowed(_, _ *model.User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.followedFlag++
}

// Follow / Unfollow のパス上で WebhookHook が呼ばれるか確認する。
func TestService_WebhookHook_FollowAndUnfollow(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)

	hook := &recordingWebhookHook{}
	svc.SetWebhookHook(hook)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, hook.follows)
	assert.Equal(t, 1, hook.followedFlag)

	require.NoError(t, svc.Unfollow("alice", "bob"))
	assert.Equal(t, 1, hook.unfollows)
}

// AcceptRequest 後にも WebhookHook.OnFollow / OnFollowed が呼ばれる。
func TestService_WebhookHook_AcceptRequest(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true) // 鍵アカ → Follow は FollowRequest に

	hook := &recordingWebhookHook{}
	svc.SetWebhookHook(hook)

	// FollowRequest を作る (この時点では WebhookHook.OnFollow は呼ばれない)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, hook.follows)

	// bob が承認 → OnFollow / OnFollowed が呼ばれる
	require.NoError(t, svc.AcceptRequest("bob", "alice"))
	assert.Equal(t, 1, hook.follows)
	assert.Equal(t, 1, hook.followedFlag)
}
