package following_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingPacker counts DetailedNotMe calls and returns a marker body.
type countingPacker struct{ detailed int }

func (c *countingPacker) Lite(u *model.User) entity.UserLite { return entity.PackUserLite(u) }

func (c *countingPacker) DetailedNotMe(_ context.Context, target, _ *model.User) (entity.UserDetailed, bool) {
	c.detailed++
	d := entity.PackUserDetailed(target, &model.UserProfile{UserID: target.ID})
	d.Description = new("packed")
	return d, true
}

// packedHook implements PackedFollowWebhookHook and builds the body like the
// real webhook hook does (only when a webhook is subscribed).
type packedHook struct {
	recordingWebhookHook
	bodies []entity.UserDetailed
}

func (p *packedHook) OnFollowPacked(_, _ *model.User, packed func() (entity.UserDetailed, bool)) {
	d, ok := packed()
	if ok {
		p.bodies = append(p.bodies, d)
	}
}

func (p *packedHook) OnUnfollowPacked(follower, followee *model.User, packed func() (entity.UserDetailed, bool)) {
	p.OnFollowPacked(follower, followee, packed)
}

// 本家と同じく、main stream と Webhook には 1 回 pack した値を流す (#3330)。
func TestFollowEvents_PackFolloweeOnce(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	packer := &countingPacker{}
	svc.SetUserPacker(packer)
	hook := &packedHook{}
	svc.SetWebhookHook(hook)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	steps := []struct {
		name string
		run  func() error
	}{
		{"follow request accepted", func() error {
			if _, err := svc.Follow("alice", "bob", following.FollowOptions{}); err != nil {
				return err
			}
			return svc.AcceptRequest("bob", "alice")
		}},
		{"unfollow", func() error { return svc.Unfollow("alice", "bob") }},
	}
	for _, st := range steps {
		packer.detailed, hook.bodies, pub.calls = 0, nil, nil
		require.NoError(t, st.run(), st.name)
		assert.Equal(t, 1, packer.detailed, "%s: packed once for stream and webhook", st.name)
		require.Len(t, hook.bodies, 1, st.name)
		assert.Equal(t, "packed", *hook.bodies[0].Description, st.name)
		var streamed int
		for _, c := range pub.calls {
			if c.eventType == "follow" || c.eventType == "unfollow" {
				streamed++
				assert.Equal(t, "packed", *c.body.(entity.UserDetailed).Description)
			}
		}
		assert.Equal(t, 1, streamed, st.name)
	}
}

// 公開アカウントへの follow も同じ。
func TestFollow_PublicPacksOnce(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	packer := &countingPacker{}
	svc.SetUserPacker(packer)
	hook := &packedHook{}
	svc.SetWebhookHook(hook)
	svc.SetMainStreamPublisher(&stubMainStreamPublisher{})

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, packer.detailed)
	assert.Len(t, hook.bodies, 1)
	assert.Equal(t, 1, hook.followedFlag)
}

// packer を配線していなければ、Webhook は従来の OnFollow / OnUnfollow で受け取る。
func TestFollowEvents_UnwiredPackerUsesPlainHook(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	hook := &packedHook{}
	svc.SetWebhookHook(hook)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	require.NoError(t, svc.Unfollow("alice", "bob"))
	assert.Empty(t, hook.bodies)
	assert.Equal(t, 1, hook.follows)
	assert.Equal(t, 1, hook.unfollows)
}

func TestService_HasUserPacker(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	assert.False(t, svc.HasUserPacker())
	svc.SetUserPacker(&countingPacker{})
	assert.True(t, svc.HasUserPacker())
}
