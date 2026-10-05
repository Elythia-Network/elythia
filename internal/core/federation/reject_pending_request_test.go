package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

type rejectMainEvents struct{ events []string }

func (r *rejectMainEvents) PublishMainEvent(userID, eventType string, _ any) {
	r.events = append(r.events, userID+":"+eventType)
}

type rejectWebhooks struct{ unfollows int }

func (r *rejectWebhooks) OnFollow(_, _ *model.User)   {}
func (r *rejectWebhooks) OnUnfollow(_, _ *model.User) { r.unfollows++ }
func (r *rejectWebhooks) OnFollowed(_, _ *model.User) {}

type rejectDelivery struct{ unfollowed int }

func (r *rejectDelivery) OnLocalFollowed(_, _ *model.User)       {}
func (r *rejectDelivery) OnLocalUnfollowed(_, _ *model.User)     { r.unfollowed++ }
func (r *rejectDelivery) OnLocalFollowAccepted(_, _ *model.User) {}

// 承認制のリモートの相手に出した申請が Reject(Follow) で拒否されたとき、本家
// remoteReject と同じく申請を消し、unfollow を main stream と Webhook に 1 回
// 出し、rejecter へ何も送り返さない (#3330)。
func TestProcess_RejectPendingFollowRequest(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	requestRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"), &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, requestRepo, idGen)
	events := &rejectMainEvents{}
	webhooks := &rejectWebhooks{}
	delivery := &rejectDelivery{}
	followingSvc.SetMainStreamPublisher(events)
	followingSvc.SetWebhookHook(webhooks)
	followingSvc.SetFederationHook(delivery)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetLocalBaseURL("https://example.com")
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host, IsLocked: true}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	require.NoError(t, requestRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}))

	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/bob",
			"object": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, requestRepo.Requests, "申請は消える")
	assert.Equal(t, []string{"bob:unfollow"}, events.events)
	assert.Equal(t, 1, webhooks.unfollows, "Webhook の unfollow も出す")
	assert.Zero(t, delivery.unfollowed, "rejecter へ Undo(Follow) を送り返さない")
}

// Reject(Follow) の follower がローカルでなければ何もしない。本家 rejectFollow も
// `skip: follower is not a local user` で止める。
func TestProcess_RejectFollow_RemoteFollowerIsIgnored(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	requestRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"), &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, requestRepo, idGen)
	events := &rejectMainEvents{}
	followingSvc.SetMainStreamPublisher(events)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetLocalBaseURL("https://example.com")
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	carolURI := "https://other.example/users/carol"
	otherHost := "other.example"
	repo.Users["carol1"] = &model.User{ID: "carol1", Username: "carol", UsernameLower: "carol", URI: &carolURI, Host: &otherHost}
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FollowerID: "carol1", FolloweeID: "alice1"}))
	require.NoError(t, requestRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "carol1", FolloweeID: "alice1"}))

	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://other.example/users/carol",
			"object": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, p.Process(body), "ack する")
	assert.Len(t, followingRepo.Followings, 1, "リモートの follower のフォローは消さない")
	assert.Len(t, requestRepo.Requests, 1, "申請も消さない")
	assert.Empty(t, events.events)
}
