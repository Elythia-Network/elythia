package webhook_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/core/webhook"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeExtras は users の handler の代わりにピン留めと移行先を埋める。
type fakeExtras struct{ viewerID string }

func (f *fakeExtras) FillDetailedExtras(_ context.Context, viewer, _ *model.User, _ *model.UserProfile, d *entity.UserDetailed) {
	f.viewerID = viewer.ID
	d.PinnedNoteIDs = []string{"pinned-note"}
	moved := "destination"
	d.MovedTo = &moved
}

// main stream と共有する packer を渡すと、ピン留めと移行先も Webhook に乗る (#3330)。
func TestFollowingHook_SharedPackerFillsPinnedAndMoveTargets(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	follower, followee := localUser(), remoteUser()
	userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: follower.ID, Active: true, On: model.StringArray{webhook.EventFollow}}
	profiles := &fakeProfileLookup{profiles: map[string]*model.UserProfile{followee.ID: {UserID: followee.ID}}}
	extras := &fakeExtras{}
	l := followLookups(profiles, nil, nil)
	l.Extras = extras
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h := webhook.NewFollowingHook(svc)
	h.SetUserPacker(userpack.New(l, idGen))

	h.OnFollow(follower, followee)

	u := userOf(t, enq)
	assert.Equal(t, []any{"pinned-note"}, u["pinnedNoteIds"])
	assert.Equal(t, "destination", u["movedTo"])
	assert.Equal(t, follower.ID, extras.viewerID, "pinned notes are gated by the follower")
}

// following service が main stream と共有する値を渡したら、それをそのまま送る
// (hook 自身の packer では組み直さない)。購読が無ければ組み立てない。
func TestFollowingHook_PackedUsesSharedValue(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: localUser().ID, Active: true,
		On: model.StringArray{webhook.EventFollow, webhook.EventUnfollow}}
	profiles := &fakeProfileLookup{profiles: map[string]*model.UserProfile{}}
	h := webhook.NewFollowingHook(svc)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetUserLookups(followLookups(profiles, nil, nil), idGen)

	calls := 0
	packed := func() (entity.UserDetailed, bool) {
		calls++
		d := entity.PackUserDetailed(remoteUser(), &model.UserProfile{}, idGen)
		d.Description = new("shared")
		return d, true
	}
	h.OnFollowPacked(localUser(), remoteUser(), packed)
	assert.Equal(t, "shared", userOf(t, enq)["description"])
	enq.userCalls = nil
	h.OnUnfollowPacked(localUser(), remoteUser(), packed)
	assert.Equal(t, "shared", userOf(t, enq)["description"])
	assert.Zero(t, profiles.calls, "the hook's own packer is not used")
	assert.Equal(t, 2, calls)

	// 購読していない利用者では呼ばない。
	h.OnFollowPacked(remoteUser(), localUser(), packed)
	assert.Equal(t, 2, calls)
	// packed が無い (nil) ときは送らない。
	enq.userCalls = nil
	h.OnFollowPacked(localUser(), remoteUser(), nil)
	assert.Empty(t, enq.userCalls)
	// 組めなかったときも送らない。
	h.OnFollowPacked(localUser(), remoteUser(), func() (entity.UserDetailed, bool) { return entity.UserDetailed{}, false })
	assert.Empty(t, enq.userCalls)
}

func TestFollowingHook_HasUserPacker(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	h := webhook.NewFollowingHook(svc)
	assert.False(t, h.HasUserPacker())
	h.SetUserPacker(userpack.New(userpack.Lookups{}, nil))
	assert.True(t, h.HasUserPacker())
}

// packer を配線していなければ、UserDetailedNotMe は組めないので送らない。
// UserLite の followed は送る。
func TestFollowingHook_UnwiredPacker(t *testing.T) {
	svc, enq, userRepo, _ := newTestService(t)
	userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: localUser().ID, Active: true,
		On: model.StringArray{webhook.EventFollow, webhook.EventFollowed}}
	h := webhook.NewFollowingHook(svc)

	h.OnFollow(localUser(), remoteUser())
	assert.Empty(t, enq.userCalls)
	h.OnFollowed(remoteUser(), localUser())
	assert.Equal(t, remoteUser().ID, userOf(t, enq)["id"])
}
