package blocking_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// eventLog records the order in which Block's collaborators are called.
type eventLog struct{ entries []string }

type loggingUnfollower struct{ log *eventLog }

func (u loggingUnfollower) UnfollowForBlock(followerID, followeeID string, silent bool) error {
	u.log.entries = append(u.log.entries, fmt.Sprintf("unfollow %s->%s silent=%t", followerID, followeeID, silent))
	return nil
}

type loggingBlockFederation struct{ log *eventLog }

func (h loggingBlockFederation) OnBlocked(blockerID, blockeeID string) {
	h.log.entries = append(h.log.entries, "block "+blockerID+"->"+blockeeID)
}

func (h loggingBlockFederation) OnUnblocked(string, string) {}

func seedFollow(t *testing.T, fr *testutil.MockFollowingRepository, follower, followee *model.User) {
	t.Helper()
	require.NoError(t, fr.Create(&model.Following{
		ID: follower.ID + "-" + followee.ID, FollowerID: follower.ID, FolloweeID: followee.ID,
		FollowerHost: follower.Host, FolloweeHost: followee.Host,
	}))
	follower.FollowingCount++
	followee.FollowersCount++
}

// 本家 UserBlockingService.block は unfollow(blocker, blockee) と
// unfollow(blockee, blocker) を待ってから Block を配送する。Undo(Follow) /
// Reject(Follow) が Block より後に届くと、相手のサーバーはブロックされた後に
// フォローの解除を受け取ることになる。
func TestBlock_UnfollowsBothDirectionsBeforeBlockDelivery(t *testing.T) {
	cases := []struct {
		name   string
		silent bool
	}{
		{"block", false},
		{"silent block", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ur, _, _ := newSvc(t)
			addUser(ur, "a")
			addUser(ur, "b")
			log := &eventLog{}
			svc.SetUnfollower(loggingUnfollower{log})
			svc.SetFederationHook(loggingBlockFederation{log})
			svc.SetUserListRepo(&loggingUserListRepo{MockUserListRepository: testutil.NewMockUserListRepository(), log: log})

			block := svc.Block
			if tc.silent {
				block = svc.BlockSilent
			}
			_, err := block("a", "b")
			require.NoError(t, err)
			assert.Equal(t, []string{
				fmt.Sprintf("unfollow a->b silent=%t", tc.silent),
				fmt.Sprintf("unfollow b->a silent=%t", tc.silent),
				"lists of b containing a",
				"block a->b",
			}, log.entries)
		})
	}
}

type recordingFollowFederation struct{ unfollowed []string }

func (h *recordingFollowFederation) OnLocalFollowed(*model.User, *model.User) {}
func (h *recordingFollowFederation) OnLocalUnfollowed(follower, followee *model.User) {
	h.unfollowed = append(h.unfollowed, follower.ID+"->"+followee.ID)
}
func (h *recordingFollowFederation) OnLocalFollowAccepted(*model.User, *model.User) {}

type recordingFollowChart struct{ unfollows []string }

func (h *recordingFollowChart) OnFollow(*model.User, *model.User) {}
func (h *recordingFollowChart) OnUnfollow(follower, followee *model.User) {
	h.unfollows = append(h.unfollows, follower.ID+"->"+followee.ID)
}

type recordingMainStream struct{ events []string }

func (p *recordingMainStream) PublishMainEvent(userID, eventType string, _ any) {
	p.events = append(p.events, userID+":"+eventType)
}

type recordingFollowWebhook struct{ unfollows []string }

func (h *recordingFollowWebhook) OnFollow(*model.User, *model.User)   {}
func (h *recordingFollowWebhook) OnFollowed(*model.User, *model.User) {}
func (h *recordingFollowWebhook) OnUnfollow(follower, followee *model.User) {
	h.unfollows = append(h.unfollows, follower.ID+"->"+followee.ID)
}

// block で外れるフォローは通常の unfollow と同じ後始末を通る (#3330)。
// 以前は blocking 側で行とカウントだけを消していたので、相互フォローの
// リモートの相手をブロックしても Undo(Follow) / Reject(Follow) が届かず、相手の
// サーバーにはフォロー関係が残り、チャートも動かなかった。
func TestBlock_RemoteMutualFollowGoesThroughUnfollow(t *testing.T) {
	cases := []struct {
		name       string
		silent     bool
		wantEvents []string
	}{
		{"block", false, []string{"alice:unfollow"}},
		{"silent block", true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ur, _, fr, followingSvc := newSvcWithFollowing(t)
			host := "remote.example"
			alice := &model.User{ID: "alice", Username: "alice"}
			carol := &model.User{ID: "carol", Username: "carol", Host: &host}
			ur.Users[alice.ID] = alice
			ur.Users[carol.ID] = carol
			seedFollow(t, fr, alice, carol)
			seedFollow(t, fr, carol, alice)
			instanceRepo := testutil.NewMockInstanceRepository()
			instanceRepo.Instances[host] = &model.Instance{Host: host, FollowersCount: 3, FollowingCount: 3}
			followingSvc.SetInstanceRepo(instanceRepo)
			fed := &recordingFollowFederation{}
			followingSvc.SetFederationHook(fed)
			chart := &recordingFollowChart{}
			followingSvc.SetChartHook(chart)
			stream := &recordingMainStream{}
			followingSvc.SetMainStreamPublisher(stream)
			webhook := &recordingFollowWebhook{}
			followingSvc.SetWebhookHook(webhook)

			block := svc.Block
			if tc.silent {
				block = svc.BlockSilent
			}
			_, err := block("alice", "carol")
			require.NoError(t, err)

			assert.Empty(t, fr.Followings, "both followings are removed")
			// 向きごとに 1 回ずつ。alice->carol は Undo(Follow)、carol->alice は
			// Reject(Follow) になる (実際の組み立ては federation の hook)。
			assert.Equal(t, []string{"alice->carol", "carol->alice"}, fed.unfollowed,
				"AP delivery is requested once per direction, even when silent")
			assert.Equal(t, []string{"alice->carol", "carol->alice"}, chart.unfollows)
			assert.Equal(t, 0, alice.FollowingCount)
			assert.Equal(t, 0, alice.FollowersCount)
			assert.Equal(t, 0, carol.FollowingCount)
			assert.Equal(t, 0, carol.FollowersCount)
			assert.Equal(t, 2, instanceRepo.Instances[host].FollowersCount)
			assert.Equal(t, 2, instanceRepo.Instances[host].FollowingCount)
			// unfollow はローカルの follower (alice) にだけ出る。silent では出さない。
			assert.Equal(t, tc.wantEvents, stream.events)
			if tc.silent {
				assert.Empty(t, webhook.unfollows)
			} else {
				assert.Equal(t, []string{"alice->carol"}, webhook.unfollows)
			}
		})
	}
}

// 移行済みの相手をブロックしても、カウントとチャートは触らない (本家
// decrementFollowing の `!movedToUri` の分岐)。移行の処理がカウントを先に
// 落としているので、ここで減らすと二重に減る。
func TestBlock_MovedAccountKeepsCounts(t *testing.T) {
	svc, ur, _, fr, followingSvc := newSvcWithFollowing(t)
	alice := &model.User{ID: "alice", Username: "alice"}
	moved := "https://elsewhere.example/users/bob"
	bob := &model.User{ID: "bob", Username: "bob", MovedToURI: &moved}
	ur.Users[alice.ID] = alice
	ur.Users[bob.ID] = bob
	seedFollow(t, fr, alice, bob)
	chart := &recordingFollowChart{}
	followingSvc.SetChartHook(chart)

	_, err := svc.Block("alice", "bob")
	require.NoError(t, err)

	assert.Empty(t, fr.Followings, "the following row is still removed")
	assert.Equal(t, 1, alice.FollowingCount)
	assert.Equal(t, 1, bob.FollowersCount)
	assert.Empty(t, chart.unfollows)
}

func TestHasUnfollower(t *testing.T) {
	var empty blocking.Service
	assert.False(t, empty.HasUnfollower())
	svc, _, _, _ := newSvc(t)
	assert.True(t, svc.HasUnfollower())
	other := blocking.NewService(testutil.NewMockUserRepository(), testutil.NewMockBlockingRepository(), nil)
	other.SetFollowRequestCanceller(&recordingFollowRequestCanceller{})
	assert.False(t, other.HasUnfollower(), "別の依存だけを配線しても false")
}
