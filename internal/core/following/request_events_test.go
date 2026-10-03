package following_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/following"
	"github.com/shiroha-a/mk/internal/model"
)

type recordingMeUpdated struct{ users []string }

func (r *recordingMeUpdated) PublishMeUpdated(userID string) { r.users = append(r.users, userID) }

// requestEventsSvc wires every event sink so a test can assert what each
// follow request path emits (and what it does not).
type requestEventsSvc struct {
	svc *following.Service
	pub *stubMainStreamPublisher
	me  *recordingMeUpdated
	wh  *recordingWebhookHook
	fed *stubFederationHook
	fr  interface {
		Create(*model.FollowRequest) error
	}
}

func newRequestEventsSvc(t *testing.T, users ...*model.User) requestEventsSvc {
	t.Helper()
	svc, userRepo, _, frRepo := newSvc(t)
	for _, u := range users {
		userRepo.Users[u.ID] = u
	}
	r := requestEventsSvc{
		svc: svc,
		pub: &stubMainStreamPublisher{},
		me:  &recordingMeUpdated{},
		wh:  &recordingWebhookHook{},
		fed: &stubFederationHook{},
		fr:  frRepo,
	}
	svc.SetMainStreamPublisher(r.pub)
	svc.SetMeUpdatedPublisher(r.me)
	svc.SetWebhookHook(r.wh)
	svc.SetFederationHook(r.fed)
	return r
}

func (r requestEventsSvc) reset() {
	r.pub.calls = nil
	r.me.users = nil
	r.wh.unfollows = 0
	r.fed.unfollowed = nil
}

func (r requestEventsSvc) events() []string {
	out := make([]string, 0, len(r.pub.calls))
	for _, c := range r.pub.calls {
		out = append(out, c.userID+":"+c.eventType)
	}
	return out
}

func localUser(id string, locked bool) *model.User {
	return &model.User{ID: id, Username: id, IsLocked: locked}
}

func remoteUser(id string, locked bool) *model.User {
	host := "remote.example"
	return &model.User{ID: id, Username: id, IsLocked: locked, Host: &host}
}

// 本家 createFollowRequest は followee がローカルなら receiveFollowRequest と
// meUpdated を流す。
func TestFollowRequestCreated_PublishesMeUpdatedToLocalFollowee(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), localUser("bob", true))
	res, err := r.svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Request)
	assert.Equal(t, []string{"bob:receiveFollowRequest"}, r.events())
	assert.Equal(t, []string{"bob"}, r.me.users)
}

// リモートの followee には meUpdated を流さない (購読する接続が無い)。
func TestFollowRequestCreated_NoMeUpdatedForRemoteFollowee(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), remoteUser("carol", true))
	res, err := r.svc.Follow("alice", "carol", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Request)
	assert.Empty(t, r.me.users)
}

// 本家 acceptFollowRequest は follow / followed に加えて followee に meUpdated を流す。
func TestAcceptRequest_PublishesMeUpdatedToFollowee(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), localUser("bob", true))
	_, err := r.svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	r.reset()

	require.NoError(t, r.svc.AcceptRequest("bob", "alice"))
	assert.Equal(t, []string{"alice:follow", "bob:followed"}, r.events())
	assert.Equal(t, []string{"bob"}, r.me.users)
}

// 本家 rejectFollowRequest は follower がローカルなら publishUnfollow
// (main stream と Webhook) を出し、meUpdated は流さない。
func TestRejectRequest_PublishesUnfollowAndWebhook(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), localUser("bob", true))
	_, err := r.svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	r.reset()

	require.NoError(t, r.svc.RejectRequest("bob", "alice"))
	assert.Equal(t, []string{"alice:unfollow"}, r.events())
	assert.Equal(t, 1, r.wh.unfollows, "Webhook の unfollow を出す")
	assert.Empty(t, r.me.users, "reject は meUpdated を流さない")
}

// リモートの follower には unfollow も Webhook も出さない。
func TestRejectRequest_RemoteFollowerGetsNoUnfollow(t *testing.T) {
	r := newRequestEventsSvc(t, remoteUser("remote1", false), localUser("bob", true))
	require.NoError(t, r.fr.Create(&model.FollowRequest{ID: "r1", FollowerID: "remote1", FolloweeID: "bob"}))

	require.NoError(t, r.svc.RejectRequest("bob", "remote1"))
	assert.Empty(t, r.events())
	assert.Zero(t, r.wh.unfollows)
	assert.Equal(t, []string{"remote1->bob"}, r.fed.unfollowed, "Reject(Follow) は配送する")
}

// AP の Reject(Follow) で申請だけが残っていた場合も、本家 remoteReject と
// 同じく unfollow を main stream と Webhook に 1 回出し、何も配送しない。
func TestRemoteReject_PendingRequestPublishesOnceWithoutDelivery(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), remoteUser("carol", true))
	_, err := r.svc.Follow("alice", "carol", following.FollowOptions{})
	require.NoError(t, err)
	r.reset()

	require.NoError(t, r.svc.RemoteReject("alice", "carol"))
	assert.Equal(t, []string{"alice:unfollow"}, r.events())
	assert.Equal(t, 1, r.wh.unfollows)
	assert.Empty(t, r.fed.unfollowed, "rejecter へ Undo(Follow) を送り返さない")
	assert.Empty(t, r.me.users)
}

// 本家 remoteReject は申請も関係も無くても publishUnfollow を出す。
func TestRemoteReject_NothingToRemoveStillPublishes(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), remoteUser("carol", false))
	require.NoError(t, r.svc.RemoteReject("alice", "carol"))
	assert.Equal(t, []string{"alice:unfollow"}, r.events())
	assert.Equal(t, 1, r.wh.unfollows)
}

// ブロックに伴う申請の取り消しは本家 UserBlockingService.cancelRequest と同じく、
// followee に meUpdated、follower に unfollow (main stream と Webhook) を出す。
func TestCancelFollowRequestsBetween_PublishesBlockEvents(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), localUser("bob", true))
	_, err := r.svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	r.reset()

	require.NoError(t, r.svc.CancelFollowRequestsBetween("bob", "alice", false))
	assert.Equal(t, []string{"bob"}, r.me.users)
	assert.Equal(t, []string{"alice:unfollow"}, r.events())
	assert.Equal(t, 1, r.wh.unfollows)
}

// follower がリモート (Reject 経路) でも、ローカルの followee には meUpdated を流す。
func TestCancelFollowRequestsBetween_RemoteFollowerPublishesMeUpdated(t *testing.T) {
	r := newRequestEventsSvc(t, remoteUser("remote1", false), localUser("bob", true))
	require.NoError(t, r.fr.Create(&model.FollowRequest{ID: "r1", FollowerID: "remote1", FolloweeID: "bob"}))

	require.NoError(t, r.svc.CancelFollowRequestsBetween("bob", "remote1", false))
	assert.Equal(t, []string{"bob"}, r.me.users)
	assert.Empty(t, r.events())
	assert.Zero(t, r.wh.unfollows)
}

// silent (インポートしたブロック) では unfollow を出さず、meUpdated は流す。
func TestCancelFollowRequestsBetween_SilentSkipsUnfollow(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), localUser("bob", true))
	_, err := r.svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	r.reset()

	require.NoError(t, r.svc.CancelFollowRequestsBetween("bob", "alice", true))
	assert.Equal(t, []string{"bob"}, r.me.users)
	assert.Empty(t, r.events())
	assert.Zero(t, r.wh.unfollows)
}

// ブロックで外れたフォローは、follower がローカルなら unfollow を出す。
func TestPublishBlockUnfollow(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), localUser("bob", false), remoteUser("carol", false))
	r.svc.PublishBlockUnfollow("alice", "bob")
	assert.Equal(t, []string{"alice:unfollow"}, r.events())
	assert.Equal(t, 1, r.wh.unfollows)

	r.reset()
	r.svc.PublishBlockUnfollow("carol", "bob")
	assert.Empty(t, r.events(), "リモートの follower には出さない")
	assert.Zero(t, r.wh.unfollows)
}

func TestService_HasMeUpdatedPublisher(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	assert.False(t, svc.HasMeUpdatedPublisher())
	svc.SetUserPacker(&countingPacker{})
	assert.False(t, svc.HasMeUpdatedPublisher(), "別の依存だけを配線しても false")
	svc.SetMeUpdatedPublisher(&recordingMeUpdated{})
	assert.True(t, svc.HasMeUpdatedPublisher())
}

// 本家 follow の silent はフォローした側の follow (main stream と Webhook) だけを
// 止め、followed は出す。
func TestFollow_SilentSuppressesFollowEventOnly(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), localUser("bob", false))
	notifications := &recordingHook{}
	r.svc.SetNotificationHook(notifications)
	_, err := r.svc.Follow("alice", "bob", following.FollowOptions{Silent: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"bob:followed"}, r.events())
	assert.Zero(t, r.wh.follows)
	assert.Equal(t, 1, r.wh.followedFlag)
	assert.Equal(t, []string{"alice->bob"}, notifications.follows, "followee へのフォロー通知は silent でも作る")

	r = newRequestEventsSvc(t, localUser("alice", false), localUser("bob", false))
	_, err = r.svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice:follow", "bob:followed"}, r.events())
	assert.Equal(t, 1, r.wh.follows)
}

// 本家 unfollow の silent は main stream の unfollow と Webhook を止めるが、配送はする。
func TestUnfollowWithoutNotify_DeliversButEmitsNothing(t *testing.T) {
	r := newRequestEventsSvc(t, localUser("alice", false), remoteUser("carol", false))
	_, err := r.svc.Follow("alice", "carol", following.FollowOptions{})
	require.NoError(t, err)
	r.reset()

	require.NoError(t, r.svc.UnfollowWithoutNotify("alice", "carol"))
	assert.Empty(t, r.events())
	assert.Zero(t, r.wh.unfollows)
	assert.Equal(t, []string{"alice->carol"}, r.fed.unfollowed, "Undo(Follow) は配送する")
	ok, err := r.svc.IsFollowing("alice", "carol")
	require.NoError(t, err)
	assert.False(t, ok)
}
