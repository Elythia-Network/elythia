package following_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errStub is a sentinel error used to simulate repository failures.
var errStub = errors.New("stub repo error")

// failingUserRepo wraps MockUserRepository and lets each method optionally fail.
type failingUserRepo struct {
	*testutil.MockUserRepository
	failIncrementFollowing bool
	failIncrementFollowers bool
}

func (f *failingUserRepo) IncrementFollowingCount(userID string, delta int) error {
	if f.failIncrementFollowing {
		return errStub
	}
	return f.MockUserRepository.IncrementFollowingCount(userID, delta)
}

func (f *failingUserRepo) IncrementFollowersCount(userID string, delta int) error {
	if f.failIncrementFollowers {
		return errStub
	}
	return f.MockUserRepository.IncrementFollowersCount(userID, delta)
}

// failingFollowingRepo wraps MockFollowingRepository and lets each method optionally fail.
type failingFollowingRepo struct {
	*testutil.MockFollowingRepository
	failExists bool
	failCreate bool
	failDelete bool
}

func (f *failingFollowingRepo) Exists(followerID, followeeID string) (bool, error) {
	if f.failExists {
		return false, errStub
	}
	return f.MockFollowingRepository.Exists(followerID, followeeID)
}

func (f *failingFollowingRepo) Create(x *model.Following) error {
	if f.failCreate {
		return errStub
	}
	return f.MockFollowingRepository.Create(x)
}

func (f *failingFollowingRepo) Delete(x *model.Following) error {
	if f.failDelete {
		return errStub
	}
	return f.MockFollowingRepository.Delete(x)
}

// failingFollowRequestRepo wraps MockFollowRequestRepository and lets each method optionally fail.
type failingFollowRequestRepo struct {
	*testutil.MockFollowRequestRepository
	failExists bool
	failCreate bool
	failDelete bool
}

func (f *failingFollowRequestRepo) Exists(followerID, followeeID string) (bool, error) {
	if f.failExists {
		return false, errStub
	}
	return f.MockFollowRequestRepository.Exists(followerID, followeeID)
}

func (f *failingFollowRequestRepo) Create(r *model.FollowRequest) error {
	if f.failCreate {
		return errStub
	}
	return f.MockFollowRequestRepository.Create(r)
}

func (f *failingFollowRequestRepo) Delete(r *model.FollowRequest) error {
	if f.failDelete {
		return errStub
	}
	return f.MockFollowRequestRepository.Delete(r)
}

// newSvcWith builds a Service from explicit repositories.
func newSvcWith(userRepo repository.UserRepository, fRepo repository.FollowingRepository, frRepo repository.FollowRequestRepository) *following.Service {
	idGen, _ := id.NewGenerator("aidx")
	return following.NewService(userRepo, fRepo, frRepo, idGen)
}

func newSvc(t *testing.T) (*following.Service, *testutil.MockUserRepository, *testutil.MockFollowingRepository, *testutil.MockFollowRequestRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := following.NewService(userRepo, fRepo, frRepo, idGen)
	return svc, userRepo, fRepo, frRepo
}

func addUser(t *testing.T, repo *testutil.MockUserRepository, id string, locked bool) *model.User {
	t.Helper()
	u := &model.User{ID: id, Username: id, IsLocked: locked}
	repo.Users[id] = u
	return u
}

func TestFollow_Success(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)

	res, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Following)
	assert.Nil(t, res.Request)
	assert.Len(t, fRepo.Followings, 1)
	assert.Equal(t, 1, userRepo.Users["alice"].FollowingCount)
	assert.Equal(t, 1, userRepo.Users["bob"].FollowersCount)
}

func TestFollow_LockedUser_CreatesRequest(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)

	res, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	assert.Nil(t, res.Following)
	require.NotNil(t, res.Request)
	assert.Empty(t, fRepo.Followings)
	assert.Len(t, frRepo.Requests, 1)
	// counterは更新されない
	assert.Equal(t, 0, userRepo.Users["alice"].FollowingCount)
	assert.Equal(t, 0, userRepo.Users["bob"].FollowersCount)
}

// #1056: Follow に渡した FollowOptions.WithReplies が auto-accept (followee not
// locked) path で新規 Following row に反映されることを検証する。
func TestFollow_WithReplies_True_PersistedOnFollowing(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)

	res, err := svc.Follow("alice", "bob", following.FollowOptions{WithReplies: true})
	require.NoError(t, err)
	require.NotNil(t, res.Following)
	assert.True(t, res.Following.WithReplies, "FollowResult.Following.WithReplies should reflect FollowOptions.WithReplies")

	// fRepo に書き込まれた row も同じ値
	require.Len(t, fRepo.Followings, 1)
	for _, f := range fRepo.Followings {
		assert.True(t, f.WithReplies, "persisted Following row should have WithReplies=true")
	}
}

// #1056: WithReplies=false (default) でも明示的に false で保存される。
func TestFollow_WithReplies_False_PersistedAsFalse(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)

	res, err := svc.Follow("alice", "bob", following.FollowOptions{WithReplies: false})
	require.NoError(t, err)
	require.NotNil(t, res.Following)
	assert.False(t, res.Following.WithReplies)

	require.Len(t, fRepo.Followings, 1)
	for _, f := range fRepo.Followings {
		assert.False(t, f.WithReplies)
	}
}

// #1056: locked followee path では FollowRequest row に WithReplies が保存され、
// AcceptRequest 経由で Following row に propagate される。
func TestFollow_WithReplies_True_LockedFolloweeRequestThenAccept(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true) // locked

	// Step 1: Follow with WithReplies=true → FollowRequest が作られる
	res, err := svc.Follow("alice", "bob", following.FollowOptions{WithReplies: true})
	require.NoError(t, err)
	require.NotNil(t, res.Request)
	assert.True(t, res.Request.WithReplies, "FollowRequest should carry WithReplies=true from Follow options")

	require.Len(t, frRepo.Requests, 1)
	for _, r := range frRepo.Requests {
		assert.True(t, r.WithReplies, "persisted FollowRequest row should have WithReplies=true")
	}

	// Step 2: bob (followee) が AcceptRequest → Following row に propagate
	require.NoError(t, svc.AcceptRequest("bob", "alice"))
	require.Len(t, fRepo.Followings, 1)
	for _, f := range fRepo.Followings {
		assert.True(t, f.WithReplies, "accepted Following row should inherit WithReplies from FollowRequest")
	}
	assert.Empty(t, frRepo.Requests, "FollowRequest should be deleted after accept")
}

// stubMainStreamPublisher captures PublishMainEvent calls for assertion.
type stubMainStreamPublisher struct {
	calls []mainEventCall
}

type mainEventCall struct {
	userID    string
	eventType string
	body      any
}

func (s *stubMainStreamPublisher) PublishMainEvent(userID, eventType string, body any) {
	s.calls = append(s.calls, mainEventCall{userID, eventType, body})
}

func TestFollow_LockedUser_PublishesReceiveFollowRequest(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "bob", pub.calls[0].userID)
	assert.Equal(t, "receiveFollowRequest", pub.calls[0].eventType)
	// body は PackUserLite(follower=alice) で、最低限 id/username が詰まっている
	// ことを JSON round-trip で確認する (UserLite は struct なので直接 map
	// assertion はできない)。
	raw, err := json.Marshal(pub.calls[0].body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "alice", m["id"])
	assert.Equal(t, "alice", m["username"])
}

// stubFederationHook は federationHook の呼び出しを検証用に記録する。
type stubFederationHook struct {
	followed   []string
	unfollowed []string
	accepted   []string
}

func (h *stubFederationHook) OnLocalFollowed(follower, followee *model.User) {
	h.followed = append(h.followed, follower.ID+"->"+followee.ID)
}
func (h *stubFederationHook) OnLocalUnfollowed(follower, followee *model.User) {
	h.unfollowed = append(h.unfollowed, follower.ID+"->"+followee.ID)
}
func (h *stubFederationHook) OnLocalFollowAccepted(follower, followee *model.User) {
	h.accepted = append(h.accepted, follower.ID+"->"+followee.ID)
}

// ローカルからリモート (鍵でない) へのフォローは、相手の Accept が届くまで申請に
// なる (#3491、本家 UserFollowingService.follow の 3 つ目の OR 条件)。Follow は
// 送り、Accept (AcceptRequest) で成立する。
func TestFollow_LocalToRemote_WaitsForAccept(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	remote := addUser(t, userRepo, "remote_bob", false)
	host := "remote.example"
	remote.Host = &host
	fed := &stubFederationHook{}
	svc.SetFederationHook(fed)

	res, err := svc.Follow("alice", "remote_bob", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Request, "a follow to a remote user did not become a request")
	exists, err := fRepo.Exists("alice", "remote_bob")
	require.NoError(t, err)
	assert.False(t, exists, "following was created before the remote Accept")
	assert.Equal(t, []string{"alice->remote_bob"}, fed.followed, "the Follow activity is still delivered")

	require.NoError(t, svc.AcceptRequest("remote_bob", "alice"))
	exists, err = fRepo.Exists("alice", "remote_bob")
	require.NoError(t, err)
	assert.True(t, exists, "the Accept did not establish the follow")
	pending, err := frRepo.Exists("alice", "remote_bob")
	require.NoError(t, err)
	assert.False(t, pending, "the request was left behind after the Accept")
}

// 本家の backend e2e と同じ切り替え (FORCE_FOLLOW_REMOTE_USER_FOR_TESTING=true) の
// ときは即座に成立する。ローカル同士は今までどおり即座に成立する。
func TestFollow_LocalToRemote_ForceSwitchAndLocalControl(t *testing.T) {
	t.Run("force switch", func(t *testing.T) {
		t.Setenv("FORCE_FOLLOW_REMOTE_USER_FOR_TESTING", "true")
		svc, userRepo, fRepo, _ := newSvc(t)
		addUser(t, userRepo, "alice", false)
		remote := addUser(t, userRepo, "remote_bob", false)
		host := "remote.example"
		remote.Host = &host
		res, err := svc.Follow("alice", "remote_bob", following.FollowOptions{})
		require.NoError(t, err)
		assert.Nil(t, res.Request)
		exists, _ := fRepo.Exists("alice", "remote_bob")
		assert.True(t, exists)
	})
	t.Run("local to local (control)", func(t *testing.T) {
		svc, userRepo, fRepo, _ := newSvc(t)
		addUser(t, userRepo, "alice", false)
		addUser(t, userRepo, "bob", false)
		res, err := svc.Follow("alice", "bob", following.FollowOptions{})
		require.NoError(t, err)
		assert.Nil(t, res.Request)
		exists, _ := fRepo.Exists("alice", "bob")
		assert.True(t, exists)
	})
}

func TestFollow_LockedUser_InvokesFederationHook(t *testing.T) {
	// 承認制の相手に対する follow でも AP Follow activity が飛ぶ必要がある
	// (相手側の承認を待つフロー)。federationHook.OnLocalFollowed が呼ばれ、
	// 実装側の shouldDeliverFollow でリモートかどうか判定される。
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	fed := &stubFederationHook{}
	svc.SetFederationHook(fed)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	assert.Equal(t, []string{"alice->bob"}, fed.followed)
}

func TestFollow_PublicUser_InvokesOutboundFollowOnly(t *testing.T) {
	// non-locked follow では OnLocalFollowed (ローカル→リモートの outbound
	// Follow 用) のみ呼ぶ。インバウンド (remote→local) の Accept 返送は
	// processor 層で original Follow ID を保持した状態で直接行うため、
	// service 層の federationHook では Accept を発行しない。
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	fed := &stubFederationHook{}
	svc.SetFederationHook(fed)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	assert.Equal(t, []string{"alice->bob"}, fed.followed)
	assert.Empty(t, fed.accepted)
}

func TestFollow_PublicUser_DoesNotPublishReceiveFollowRequest(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	// 注: #290 以降 public user への follow でも `follow` / `followed` を
	// emit するようになったため、ここでは receiveFollowRequest が含まれて
	// いないことだけを確認する。
	for _, c := range pub.calls {
		assert.NotEqual(t, "receiveFollowRequest", c.eventType)
	}
}

func TestFollow_PublicUser_PublishesFollowAndFollowed(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	require.Len(t, pub.calls, 2)
	// 1件目: follower (alice) の main に `follow` (body = followee=bob)
	assert.Equal(t, "alice", pub.calls[0].userID)
	assert.Equal(t, "follow", pub.calls[0].eventType)
	assertFollowStreamBody(t, pub.calls[0].body, "bob", true, false)
	// 2件目: followee (bob) の main に `followed` (body = follower=alice)
	assert.Equal(t, "bob", pub.calls[1].userID)
	assert.Equal(t, "followed", pub.calls[1].eventType)
	assertPackedUserLiteID(t, pub.calls[1].body, "alice")
}

func TestUnfollow_PublishesUnfollow(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	// Follow 成功で入った publish を無視するため、ここから publisher を差し替え。
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.Unfollow("alice", "bob"))
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "alice", pub.calls[0].userID)
	assert.Equal(t, "unfollow", pub.calls[0].eventType)
	assertFollowStreamBody(t, pub.calls[0].body, "bob", false, false)
}

// assertPackedUserLiteID は body が UserLite 相当の JSON 表現で、id
// フィールドが期待値と一致することを検証する。
func assertPackedUserLiteID(t *testing.T, body any, expectID string) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, expectID, m["id"])
}

// assertFollowStreamBody は follow/unfollow の body が UserDetailed shape で
// id / isFollowing / hasPendingFollowRequestFromYou を期待値どおりに持つこと
// を検証する。frontend MkFollowButton.onFollowChangeがこれらfieldを直接読む
// ので streaming event body の shape はここに明示的にロックする。
func assertFollowStreamBody(t *testing.T, body any, expectID string, isFollowing, hasPending bool) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, expectID, m["id"])
	assert.Equal(t, isFollowing, m["isFollowing"], "isFollowing mismatch")
	assert.Equal(t, hasPending, m["hasPendingFollowRequestFromYou"], "hasPendingFollowRequestFromYou mismatch")
}

func TestFollow_SelfFollow(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)

	_, err := svc.Follow("alice", "alice", following.FollowOptions{})
	assert.True(t, errors.Is(err, following.ErrSelfFollow))
}

func TestFollow_FolloweeNotFound(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)

	_, err := svc.Follow("alice", "missing", following.FollowOptions{})
	assert.True(t, errors.Is(err, following.ErrFolloweeNotFound))
}

func TestFollow_FollowerNotFound(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "bob", false)

	_, err := svc.Follow("missing", "bob", following.FollowOptions{})
	assert.True(t, errors.Is(err, following.ErrFolloweeNotFound))
}

func TestFollow_AlreadyFollowing(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	_, err = svc.Follow("alice", "bob", following.FollowOptions{})
	assert.True(t, errors.Is(err, following.ErrAlreadyFollowing))
}

func TestFollow_AlreadyRequested(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	_, err = svc.Follow("alice", "bob", following.FollowOptions{})
	assert.True(t, errors.Is(err, following.ErrAlreadyRequested))
}

func TestUnfollow_Success(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	err = svc.Unfollow("alice", "bob")
	require.NoError(t, err)
	assert.Empty(t, fRepo.Followings)
	assert.Equal(t, 0, userRepo.Users["alice"].FollowingCount)
	assert.Equal(t, 0, userRepo.Users["bob"].FollowersCount)
}

func TestUnfollow_NotFollowing(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)

	err := svc.Unfollow("alice", "bob")
	assert.True(t, errors.Is(err, following.ErrNotFollowing))
}

func TestUnfollow_SelfFollow(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.Unfollow("alice", "alice")
	assert.True(t, errors.Is(err, following.ErrSelfFollow))
}

// recordingChartHook captures Follow / Unfollow events forwarded by
// FollowingService so we can verify the chart hook firing path.
type recordingChartHook struct {
	follows   [][2]string
	unfollows [][2]string
}

func (h *recordingChartHook) OnFollow(follower, followee *model.User) {
	h.follows = append(h.follows, [2]string{follower.ID, followee.ID})
}

func (h *recordingChartHook) OnUnfollow(follower, followee *model.User) {
	h.unfollows = append(h.unfollows, [2]string{follower.ID, followee.ID})
}

func TestFollow_ChartHookInvoked(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	hook := &recordingChartHook{}
	svc.SetChartHook(hook)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	require.Len(t, hook.follows, 1)
	assert.Equal(t, [2]string{"alice", "bob"}, hook.follows[0])
}

func TestUnfollow_ChartHookInvoked(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	hook := &recordingChartHook{}
	svc.SetChartHook(hook)

	require.NoError(t, svc.Unfollow("alice", "bob"))
	require.Len(t, hook.unfollows, 1)
	assert.Equal(t, [2]string{"alice", "bob"}, hook.unfollows[0])
}

func TestAcceptRequest_ChartHookInvoked(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	hook := &recordingChartHook{}
	svc.SetChartHook(hook)

	require.NoError(t, svc.AcceptRequest("bob", "alice"))
	require.Len(t, hook.follows, 1)
	assert.Equal(t, [2]string{"alice", "bob"}, hook.follows[0])
}

func TestAcceptRequest_Success(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	err = svc.AcceptRequest("bob", "alice")
	require.NoError(t, err)
	assert.Empty(t, frRepo.Requests)
	assert.Len(t, fRepo.Followings, 1)
	assert.Equal(t, 1, userRepo.Users["alice"].FollowingCount)
	assert.Equal(t, 1, userRepo.Users["bob"].FollowersCount)
}

func TestAcceptRequest_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.AcceptRequest("bob", "alice")
	assert.True(t, errors.Is(err, following.ErrRequestNotFound))
}

// followee (bob, 承認する側=self) が follower (alice) を block している方向は
// Follow() の blockingChecker 検査と対称に ErrBlocking (#1562 の 'blocking' 相当)。
// 申請行も削除されずに残ること (承認せず何も変えない) まで見る。
func TestAcceptRequest_BlockingFollower(t *testing.T) {
	svc, userRepo, _, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	svc.SetBlockingChecker(&stubBlockingChecker{blockedPairs: map[string]bool{"bob->alice": true}})

	err = svc.AcceptRequest("bob", "alice")
	require.ErrorIs(t, err, following.ErrBlocking)
	assert.Len(t, frRepo.Requests, 1, "block 中は申請を消さずに残す")
}

// follower (alice) が followee (bob, self) を block している方向は ErrBlocked。
func TestAcceptRequest_BlockedByFollower(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	svc.SetBlockingChecker(&stubBlockingChecker{blockedPairs: map[string]bool{"alice->bob": true}})

	err = svc.AcceptRequest("bob", "alice")
	require.ErrorIs(t, err, following.ErrBlocked)
}

// 相互 block 時は Follow() と同じく blocking 判定が先勝ちする。
func TestAcceptRequest_MutualBlockPrefersBlocking(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	svc.SetBlockingChecker(&stubBlockingChecker{blockedPairs: map[string]bool{
		"bob->alice": true,
		"alice->bob": true,
	}})

	err = svc.AcceptRequest("bob", "alice")
	require.ErrorIs(t, err, following.ErrBlocking)
}

func TestAcceptRequest_BlockingCheckerFirstCheckError(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	svc.SetBlockingChecker(&stubBlockingChecker{err: errStub})

	err = svc.AcceptRequest("bob", "alice")
	assert.ErrorIs(t, err, errStub)
}

func TestAcceptRequest_BlockingCheckerSecondCheckError(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	svc.SetBlockingChecker(&stubBlockingCheckerForwardError{})

	err = svc.AcceptRequest("bob", "alice")
	assert.ErrorIs(t, err, errStub)
}

// blockingChecker が配線されていてもブロック関係が無ければ通常どおり承認できる
// (nil の場合は TestAcceptRequest_Success で確認済み)。
func TestAcceptRequest_BlockingCheckerWiredNoBlock(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	svc.SetBlockingChecker(&stubBlockingChecker{})

	err = svc.AcceptRequest("bob", "alice")
	require.NoError(t, err)
	assert.Len(t, fRepo.Followings, 1)
}

func TestAcceptRequest_PublishesFollowAndFollowed(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	// Follow() でキャプチャされた receiveFollowRequest を除外するため
	// publisher を差し替える。
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.AcceptRequest("bob", "alice"))

	require.Len(t, pub.calls, 2)
	// 1件目: follower (alice) の main に `follow` (body = followee=bob)
	assert.Equal(t, "alice", pub.calls[0].userID)
	assert.Equal(t, "follow", pub.calls[0].eventType)
	assertFollowStreamBody(t, pub.calls[0].body, "bob", true, false)
	// 2件目: followee (bob) の main に `followed` (body = follower=alice)
	assert.Equal(t, "bob", pub.calls[1].userID)
	assert.Equal(t, "followed", pub.calls[1].eventType)
	assertPackedUserLiteID(t, pub.calls[1].body, "alice")
}

func TestRejectRequest_Success(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	err = svc.RejectRequest("bob", "alice")
	require.NoError(t, err)
	assert.Empty(t, frRepo.Requests)
	assert.Empty(t, fRepo.Followings)
}

func TestRejectRequest_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.RejectRequest("bob", "alice")
	assert.True(t, errors.Is(err, following.ErrRequestNotFound))
}

func TestCancelRequest_Success(t *testing.T) {
	svc, userRepo, _, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	err = svc.CancelRequest("alice", "bob")
	require.NoError(t, err)
	assert.Empty(t, frRepo.Requests)
}

func TestCancelRequest_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.CancelRequest("alice", "bob")
	assert.True(t, errors.Is(err, following.ErrRequestNotFound))
}

func TestCancelRequest_InvokesUnfollowHook(t *testing.T) {
	// pending request を cancel したら、リモート locked followee に
	// Undo Follow を送るために OnLocalUnfollowed を呼ぶ必要がある。
	// hook 内の shouldDeliverFollow で local followee は自動的に no-op に
	// なるのでここでは呼び出し有無だけ検証する。
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	fed := &stubFederationHook{}
	svc.SetFederationHook(fed)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	// Follow() の中で OnLocalFollowed が呼ばれているはずなのでリセットして
	// cancel 分だけを観察する。
	fed.unfollowed = nil

	require.NoError(t, svc.CancelRequest("alice", "bob"))
	assert.Equal(t, []string{"alice->bob"}, fed.unfollowed)
}

func TestCancelRequest_PublishesOnlyMeUpdated(t *testing.T) {
	// 本家 cancelFollowRequest は followee に meUpdated を流すだけで、follower に
	// unfollow も Webhook も出さない (フォローボタンは API の応答で戻す)。
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)
	me := &recordingMeUpdated{}
	svc.SetMeUpdatedPublisher(me)
	wh := &recordingWebhookHook{}
	svc.SetWebhookHook(wh)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	pub.calls = nil
	me.users = nil

	require.NoError(t, svc.CancelRequest("alice", "bob"))
	assert.Empty(t, pub.calls, "follower に unfollow を流さない")
	assert.Equal(t, []string{"bob"}, me.users, "followee に meUpdated を流す")
	assert.Zero(t, wh.unfollows, "Webhook の unfollow を出さない")
}

// selectiveFindFailRepo makes FindByPair fail for a single pair so error
// propagation and "continue with the other direction" can be asserted.
type selectiveFindFailRepo struct {
	*testutil.MockFollowRequestRepository
	failPair [2]string
}

func (r *selectiveFindFailRepo) FindByPair(followerID, followeeID string) (*model.FollowRequest, error) {
	if followerID == r.failPair[0] && followeeID == r.failPair[1] {
		return nil, errStub
	}
	return r.MockFollowRequestRepository.FindByPair(followerID, followeeID)
}

// followerLookupFailRepo fails FindByID for a single user so the
// follower-lookup fallback in cancelOneFollowRequest can be exercised.
type followerLookupFailRepo struct {
	*testutil.MockUserRepository
	failID string
}

func (r *followerLookupFailRepo) FindByID(id string) (*model.User, error) {
	if id == r.failID {
		return nil, errStub
	}
	return r.MockUserRepository.FindByID(id)
}

func TestCancelFollowRequestsBetween_NoRequestsIsNoop(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	require.NoError(t, svc.CancelFollowRequestsBetween("alice", "bob", false))
}

func TestCancelFollowRequestsBetween_LocalFollowerUsesCancelPath(t *testing.T) {
	svc, userRepo, _, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "alice", FolloweeID: "bob"}))

	hook := &recordingHook{}
	svc.SetNotificationHook(hook)
	fed := &stubFederationHook{}
	svc.SetFederationHook(fed)

	require.NoError(t, svc.CancelFollowRequestsBetween("bob", "alice", false))
	assert.Empty(t, frRepo.Requests)
	assert.Empty(t, hook.rejects, "local follower の取り消しは Reject 通知の掃除を伴わない")
	assert.Equal(t, []string{"alice->bob"}, fed.unfollowed, "CancelRequest 経路は unfollow hook を呼ぶ")
}

func TestCancelFollowRequestsBetween_RemoteFollowerUsesRejectPath(t *testing.T) {
	svc, userRepo, _, frRepo := newSvc(t)
	addUser(t, userRepo, "bob", false)
	host := "remote.example"
	userRepo.Users["remote1"] = &model.User{ID: "remote1", Username: "remote1", Host: &host}
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "remote1", FolloweeID: "bob"}))

	hook := &recordingHook{}
	svc.SetNotificationHook(hook)

	require.NoError(t, svc.CancelFollowRequestsBetween("bob", "remote1", false))
	assert.Empty(t, frRepo.Requests)
	assert.Equal(t, []string{"remote1->bob"}, hook.rejects, "remote follower の取り消しは followee 側の通知を掃除する")
}

func TestCancelFollowRequestsBetween_BothDirections(t *testing.T) {
	svc, userRepo, _, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "alice", FolloweeID: "bob"}))
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r2", FollowerID: "bob", FolloweeID: "alice"}))

	require.NoError(t, svc.CancelFollowRequestsBetween("alice", "bob", false))
	assert.Empty(t, frRepo.Requests, "双方向の申請が消える")
}

func TestCancelFollowRequestsBetween_ContinuesAfterError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	frRepo := &selectiveFindFailRepo{
		MockFollowRequestRepository: testutil.NewMockFollowRequestRepository(),
		failPair:                    [2]string{"alice", "bob"},
	}
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r2", FollowerID: "bob", FolloweeID: "alice"}))
	svc := newSvcWith(userRepo, testutil.NewMockFollowingRepository(), frRepo)

	err := svc.CancelFollowRequestsBetween("alice", "bob", false)
	assert.ErrorIs(t, err, errStub, "失敗した direction の error を返す")
	assert.Empty(t, frRepo.Requests, "1 方向が失敗してももう片方は処理する")
}

// follower の lookup が一時障害でも、行の削除は RejectRequest 経路で進む。
func TestCancelFollowRequestsBetween_FollowerLookupFailureStillDeletes(t *testing.T) {
	base := testutil.NewMockUserRepository()
	addUser(t, base, "alice", false)
	addUser(t, base, "bob", false)
	userRepo := &followerLookupFailRepo{MockUserRepository: base, failID: "alice"}
	frRepo := testutil.NewMockFollowRequestRepository()
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "alice", FolloweeID: "bob"}))
	svc := newSvcWith(userRepo, testutil.NewMockFollowingRepository(), frRepo)

	require.NoError(t, svc.CancelFollowRequestsBetween("bob", "alice", false))
	assert.Empty(t, frRepo.Requests, "lookup 失敗でも申請行は消える")
}

// local follower 側の CancelRequest が失敗したら error を返す。
func TestCancelFollowRequestsBetween_CancelRequestErrorPropagates(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	frRepo := &failingFollowRequestRepo{
		MockFollowRequestRepository: testutil.NewMockFollowRequestRepository(),
		failDelete:                  true,
	}
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "alice", FolloweeID: "bob"}))
	svc := newSvcWith(userRepo, testutil.NewMockFollowingRepository(), frRepo)

	err := svc.CancelFollowRequestsBetween("alice", "bob", false)
	assert.ErrorIs(t, err, errStub, "削除失敗は握り潰さない")
}

// remote follower 側の RejectRequest が失敗したら error を返す。
func TestCancelFollowRequestsBetween_RejectRequestErrorPropagates(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	addUser(t, userRepo, "bob", false)
	host := "remote.example"
	userRepo.Users["remote1"] = &model.User{ID: "remote1", Username: "remote1", Host: &host}
	frRepo := &failingFollowRequestRepo{
		MockFollowRequestRepository: testutil.NewMockFollowRequestRepository(),
		failDelete:                  true,
	}
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "remote1", FolloweeID: "bob"}))
	svc := newSvcWith(userRepo, testutil.NewMockFollowingRepository(), frRepo)

	err := svc.CancelFollowRequestsBetween("bob", "remote1", false)
	assert.ErrorIs(t, err, errStub, "削除失敗は握り潰さない")
}

// リモートfollowerからのリクエストをrejectしたときにfederation hookの
// OnLocalUnfollowedが呼ばれてReject(Follow)配信がトリガーされること (#349 PR コメント対応)。
func TestRejectRequest_InvokesFederationHookForRemoteFollower(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	fhook := &recordingFederationHook{}
	svc.SetFederationHook(fhook)

	require.NoError(t, svc.RejectRequest("bob", "alice"))
	// OnLocalUnfollowed が alice(follower) / bob(followee) で1回呼ばれる。
	// 配信自体は federation hook 実装側で shouldDeliverFollow / 受動側
	// follower=remote の分岐で制御される (別testで検証済み)。
	assert.Equal(t, []string{"alice->bob"}, fhook.unfollows)
}

func TestRejectRequest_PublishesUnfollowEvent(t *testing.T) {
	// 拒否された follower (alice) の frontend が「承認待ち」→「フォロー」
	// にリロードなしで戻るよう、main channel に unfollow event を publish する。
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.RejectRequest("bob", "alice"))
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "alice", pub.calls[0].userID)
	assert.Equal(t, "unfollow", pub.calls[0].eventType)
	assertFollowStreamBody(t, pub.calls[0].body, "bob", false, false)
}

func TestListReceivedRequests(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "carol", false)
	addUser(t, userRepo, "bob", true)
	_, _ = svc.Follow("alice", "bob", following.FollowOptions{})
	_, _ = svc.Follow("carol", "bob", following.FollowOptions{})

	rows, err := svc.ListReceivedRequests("bob", 100, "", "")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestListReceivedFollowing(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	addUser(t, userRepo, "carol", false)
	_, _ = svc.Follow("bob", "alice", following.FollowOptions{})
	_, _ = svc.Follow("carol", "alice", following.FollowOptions{})

	rows, err := svc.ListReceivedFollowing("alice", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestListSentFollowing(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	addUser(t, userRepo, "carol", false)
	_, _ = svc.Follow("alice", "bob", following.FollowOptions{})
	_, _ = svc.Follow("alice", "carol", following.FollowOptions{})

	rows, err := svc.ListSentFollowing("alice", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestListSentRequests(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	addUser(t, userRepo, "dave", true)
	_, _ = svc.Follow("alice", "bob", following.FollowOptions{})
	_, _ = svc.Follow("alice", "dave", following.FollowOptions{})

	rows, err := svc.ListSentRequests("alice", 100, "", "")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

// --- failing-repo error path tests ---

func setupUsers(repo *testutil.MockUserRepository, ids ...string) {
	for _, i := range ids {
		repo.Users[i] = &model.User{ID: i, Username: i}
	}
}

func setupLockedUser(repo *testutil.MockUserRepository, id string) {
	repo.Users[id] = &model.User{ID: id, Username: id, IsLocked: true}
}

func TestFollow_ExistsError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	setupUsers(userRepo, "alice", "bob")
	fRepo := &failingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository(), failExists: true}
	frRepo := testutil.NewMockFollowRequestRepository()
	svc := newSvcWith(userRepo, fRepo, frRepo)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	assert.ErrorIs(t, err, errStub)
}

func TestFollow_CreateError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	setupUsers(userRepo, "alice", "bob")
	fRepo := &failingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository(), failCreate: true}
	frRepo := testutil.NewMockFollowRequestRepository()
	svc := newSvcWith(userRepo, fRepo, frRepo)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	assert.ErrorIs(t, err, errStub)
}

func TestFollow_RequestExistsError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	setupUsers(userRepo, "alice")
	setupLockedUser(userRepo, "bob")
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := &failingFollowRequestRepo{MockFollowRequestRepository: testutil.NewMockFollowRequestRepository(), failExists: true}
	svc := newSvcWith(userRepo, fRepo, frRepo)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	assert.ErrorIs(t, err, errStub)
}

func TestFollow_RequestCreateError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	setupUsers(userRepo, "alice")
	setupLockedUser(userRepo, "bob")
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := &failingFollowRequestRepo{MockFollowRequestRepository: testutil.NewMockFollowRequestRepository(), failCreate: true}
	svc := newSvcWith(userRepo, fRepo, frRepo)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	assert.ErrorIs(t, err, errStub)
}

func TestFollow_IncrementFollowingError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	userRepo := &failingUserRepo{MockUserRepository: mockUR, failIncrementFollowing: true}
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	svc := newSvcWith(userRepo, fRepo, frRepo)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	assert.ErrorIs(t, err, errStub)
}

func TestFollow_IncrementFollowersError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	userRepo := &failingUserRepo{MockUserRepository: mockUR, failIncrementFollowers: true}
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	svc := newSvcWith(userRepo, fRepo, frRepo)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	assert.ErrorIs(t, err, errStub)
}

func TestUnfollow_DeleteError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	mockFR := testutil.NewMockFollowingRepository()
	// 既存のfollowingを直接挿入
	mockFR.Followings["existing"] = &model.Following{ID: "existing", FollowerID: "alice", FolloweeID: "bob"}
	fRepo := &failingFollowingRepo{MockFollowingRepository: mockFR, failDelete: true}
	frRepo := testutil.NewMockFollowRequestRepository()
	svc := newSvcWith(mockUR, fRepo, frRepo)

	err := svc.Unfollow("alice", "bob")
	assert.ErrorIs(t, err, errStub)
}

func TestUnfollow_IncrementFollowingError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	userRepo := &failingUserRepo{MockUserRepository: mockUR, failIncrementFollowing: true}
	mockFR := testutil.NewMockFollowingRepository()
	mockFR.Followings["existing"] = &model.Following{ID: "existing", FollowerID: "alice", FolloweeID: "bob"}
	frRepo := testutil.NewMockFollowRequestRepository()
	svc := newSvcWith(userRepo, mockFR, frRepo)

	err := svc.Unfollow("alice", "bob")
	assert.ErrorIs(t, err, errStub)
}

func TestUnfollow_IncrementFollowersError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	userRepo := &failingUserRepo{MockUserRepository: mockUR, failIncrementFollowers: true}
	mockFR := testutil.NewMockFollowingRepository()
	mockFR.Followings["existing"] = &model.Following{ID: "existing", FollowerID: "alice", FolloweeID: "bob"}
	frRepo := testutil.NewMockFollowRequestRepository()
	svc := newSvcWith(userRepo, mockFR, frRepo)

	err := svc.Unfollow("alice", "bob")
	assert.ErrorIs(t, err, errStub)
}

func TestAcceptRequest_DeleteError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	mockFRR := testutil.NewMockFollowRequestRepository()
	mockFRR.Requests["req"] = &model.FollowRequest{ID: "req", FollowerID: "alice", FolloweeID: "bob"}
	frRepo := &failingFollowRequestRepo{MockFollowRequestRepository: mockFRR, failDelete: true}
	svc := newSvcWith(mockUR, testutil.NewMockFollowingRepository(), frRepo)

	err := svc.AcceptRequest("bob", "alice")
	assert.ErrorIs(t, err, errStub)
}

func TestAcceptRequest_FollowingCreateError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	mockFRR := testutil.NewMockFollowRequestRepository()
	mockFRR.Requests["req"] = &model.FollowRequest{ID: "req", FollowerID: "alice", FolloweeID: "bob"}
	mockFR := testutil.NewMockFollowingRepository()
	fRepo := &failingFollowingRepo{MockFollowingRepository: mockFR, failCreate: true}
	svc := newSvcWith(mockUR, fRepo, mockFRR)

	err := svc.AcceptRequest("bob", "alice")
	assert.ErrorIs(t, err, errStub)
}

func TestAcceptRequest_IncrementFollowingError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	userRepo := &failingUserRepo{MockUserRepository: mockUR, failIncrementFollowing: true}
	mockFRR := testutil.NewMockFollowRequestRepository()
	mockFRR.Requests["req"] = &model.FollowRequest{ID: "req", FollowerID: "alice", FolloweeID: "bob"}
	svc := newSvcWith(userRepo, testutil.NewMockFollowingRepository(), mockFRR)

	err := svc.AcceptRequest("bob", "alice")
	assert.ErrorIs(t, err, errStub)
}

func TestAcceptRequest_IncrementFollowersError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	setupUsers(mockUR, "alice", "bob")
	userRepo := &failingUserRepo{MockUserRepository: mockUR, failIncrementFollowers: true}
	mockFRR := testutil.NewMockFollowRequestRepository()
	mockFRR.Requests["req"] = &model.FollowRequest{ID: "req", FollowerID: "alice", FolloweeID: "bob"}
	svc := newSvcWith(userRepo, testutil.NewMockFollowingRepository(), mockFRR)

	err := svc.AcceptRequest("bob", "alice")
	assert.ErrorIs(t, err, errStub)
}

// recordingHook captures notification hook calls.
type recordingHook struct {
	follows  []string
	requests []string
	accepts  []string
	rejects  []string
}

func (h *recordingHook) OnFollowed(followerID, followeeID string) {
	h.follows = append(h.follows, followerID+"->"+followeeID)
}
func (h *recordingHook) OnFollowRequested(followerID, followeeID string) {
	h.requests = append(h.requests, followerID+"->"+followeeID)
}
func (h *recordingHook) OnFollowAccepted(followerID, followeeID string) {
	h.accepts = append(h.accepts, followerID+"->"+followeeID)
}
func (h *recordingHook) OnFollowRejected(followerID, followeeID string) {
	h.rejects = append(h.rejects, followerID+"->"+followeeID)
}

func TestService_NotificationHook_OnFollow(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	hook := &recordingHook{}
	svc.SetNotificationHook(hook)

	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"bob->alice"}, hook.follows)
}

func TestService_NotificationHook_OnFollowRequest(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", true)
	addUser(t, ur, "bob", false)
	hook := &recordingHook{}
	svc.SetNotificationHook(hook)

	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"bob->alice"}, hook.requests)
}

// stubBlockingChecker is a configurable BlockingChecker for tests.
type stubBlockingChecker struct {
	blockedPairs map[string]bool // "blockerID->blockeeID"
	err          error
}

func (s *stubBlockingChecker) IsBlocked(blockerID, blockeeID string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.blockedPairs[blockerID+"->"+blockeeID], nil
}

func TestFollow_BlockedByFollowee(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	svc.SetBlockingChecker(&stubBlockingChecker{blockedPairs: map[string]bool{"alice->bob": true}})

	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	require.ErrorIs(t, err, following.ErrBlocked)
}

// follower (bob) が followee (alice) を block している方向は ErrBlocking
// (upstream の 'blocking' に対応、#1562)。
func TestFollow_BlockingFollowee(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	svc.SetBlockingChecker(&stubBlockingChecker{blockedPairs: map[string]bool{"bob->alice": true}})

	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	require.ErrorIs(t, err, following.ErrBlocking)
}

// 相互 block 時は upstream と同じく blocking 判定が先勝ちする (#1562)。
func TestFollow_MutualBlockPrefersBlocking(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	svc.SetBlockingChecker(&stubBlockingChecker{blockedPairs: map[string]bool{
		"bob->alice": true,
		"alice->bob": true,
	}})

	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	require.ErrorIs(t, err, following.ErrBlocking)
}

func TestFollow_BlockingCheckerReverseError(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	svc.SetBlockingChecker(&stubBlockingChecker{err: errStub})
	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	assert.ErrorIs(t, err, errStub)
}

// stubBlockingCheckerForwardError errors only on forward (follower->followee) check.
type stubBlockingCheckerForwardError struct {
	calls int
}

func (s *stubBlockingCheckerForwardError) IsBlocked(_, _ string) (bool, error) {
	s.calls++
	if s.calls == 2 {
		return false, errStub
	}
	return false, nil
}

func TestFollow_BlockingCheckerForwardError(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	svc.SetBlockingChecker(&stubBlockingCheckerForwardError{})
	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	assert.ErrorIs(t, err, errStub)
}

func TestService_NotificationHook_OnAccept(t *testing.T) {
	svc, ur, _, frRepo := newSvc(t)
	addUser(t, ur, "alice", true)
	addUser(t, ur, "bob", false)
	frRepo.Requests["req"] = &model.FollowRequest{ID: "req", FollowerID: "bob", FolloweeID: "alice"}
	hook := &recordingHook{}
	svc.SetNotificationHook(hook)

	require.NoError(t, svc.AcceptRequest("alice", "bob"))
	// follower (bob) に「follow request accepted」通知
	assert.Equal(t, []string{"bob->alice"}, hook.accepts)
	// followee (alice) に「follow」通知 (#349 コメント対応、本家 insertFollowingDoc 互換)
	assert.Equal(t, []string{"bob->alice"}, hook.follows)
}

func TestService_NotificationHook_OnReject(t *testing.T) {
	svc, ur, _, frRepo := newSvc(t)
	addUser(t, ur, "alice", true)
	addUser(t, ur, "bob", false)
	frRepo.Requests["req"] = &model.FollowRequest{ID: "req", FollowerID: "bob", FolloweeID: "alice"}
	hook := &recordingHook{}
	svc.SetNotificationHook(hook)

	require.NoError(t, svc.RejectRequest("alice", "bob"))
	// reject後、followee (alice) 側の receiveFollowRequest 通知を purge するため
	// OnFollowRejected が呼ばれる。
	assert.Equal(t, []string{"bob->alice"}, hook.rejects)
}

// recordingFederationHook captures federation hook invocations for assertion.
type recordingFederationHook struct {
	follows   []string
	unfollows []string
	accepts   []string
}

func (h *recordingFederationHook) OnLocalFollowed(follower, followee *model.User) {
	h.follows = append(h.follows, follower.ID+"->"+followee.ID)
}

func (h *recordingFederationHook) OnLocalUnfollowed(follower, followee *model.User) {
	h.unfollows = append(h.unfollows, follower.ID+"->"+followee.ID)
}

func (h *recordingFederationHook) OnLocalFollowAccepted(follower, followee *model.User) {
	h.accepts = append(h.accepts, follower.ID+"->"+followee.ID)
}

func TestService_FederationHook_OnFollow(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice->bob"}, hook.follows)
}

func TestService_FederationHook_OnUnfollow(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	require.NoError(t, svc.Unfollow("alice", "bob"))
	assert.Equal(t, []string{"alice->bob"}, hook.unfollows)
}

func TestService_FederationHook_OnUnfollow_UserLookupFailure(t *testing.T) {
	// follower の repo から FindByID が失敗する経路: alice をフォロー後に
	// repo から削除する。
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	delete(ur.Users, "alice")
	require.NoError(t, svc.Unfollow("alice", "bob"))
	assert.Empty(t, hook.unfollows)
}

func TestService_FederationHook_OnAccept(t *testing.T) {
	svc, ur, _, frRepo := newSvc(t)
	addUser(t, ur, "alice", true)
	addUser(t, ur, "bob", false)
	frRepo.Requests["req"] = &model.FollowRequest{ID: "req", FollowerID: "bob", FolloweeID: "alice"}
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	require.NoError(t, svc.AcceptRequest("alice", "bob"))
	assert.Equal(t, []string{"bob->alice"}, hook.accepts)
}

func TestService_FederationHook_OnAccept_UserLookupFailure(t *testing.T) {
	svc, ur, _, frRepo := newSvc(t)
	addUser(t, ur, "alice", true)
	addUser(t, ur, "bob", false)
	frRepo.Requests["req"] = &model.FollowRequest{ID: "req", FollowerID: "bob", FolloweeID: "alice"}
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	// follower (bob) を消してから accept する → hook 呼ばれない
	delete(ur.Users, "bob")
	require.NoError(t, svc.AcceptRequest("alice", "bob"))
	assert.Empty(t, hook.accepts)
}

// --- Instance counter incremental hook (#596) ---

// remote follower → local followee: instance(remote).followingCount += 1
// (本家 insertFollowingDoc の isRemoteUser(follower) 分岐、#3330)。
func TestFollow_RemoteFollower_BumpsInstanceFollowing(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	instanceRepo := testutil.NewMockInstanceRepository()
	host := "remote.example"
	instanceRepo.Instances[host] = &model.Instance{Host: host}
	svc.SetInstanceRepo(instanceRepo)

	addUser(t, userRepo, "alice_local", false)
	remote := addUser(t, userRepo, "remote_user", false)
	remote.Host = &host

	_, err := svc.Follow("remote_user", "alice_local", following.FollowOptions{})
	require.NoError(t, err)

	assert.Equal(t, 0, instanceRepo.Instances[host].FollowersCount)
	assert.Equal(t, 1, instanceRepo.Instances[host].FollowingCount)
}

// local follower → remote followee: instance(remote).followersCount += 1
// (本家 insertFollowingDoc の isRemoteUser(followee) 分岐、#3330)。
func TestFollow_LocalFollowsRemote_BumpsInstanceFollowers(t *testing.T) {
	// ローカルからリモートへのフォローは Accept まで申請になる (#3491)。このテストは
	// 成立した後を見るので、本家の backend e2e と同じ切り替えで即座に成立させる。
	t.Setenv("FORCE_FOLLOW_REMOTE_USER_FOR_TESTING", "true")
	svc, userRepo, _, _ := newSvc(t)
	instanceRepo := testutil.NewMockInstanceRepository()
	host := "remote.example"
	instanceRepo.Instances[host] = &model.Instance{Host: host}
	svc.SetInstanceRepo(instanceRepo)

	addUser(t, userRepo, "alice_local", false)
	remote := addUser(t, userRepo, "remote_user", false)
	remote.Host = &host

	_, err := svc.Follow("alice_local", "remote_user", following.FollowOptions{})
	require.NoError(t, err)

	assert.Equal(t, 1, instanceRepo.Instances[host].FollowersCount)
	assert.Equal(t, 0, instanceRepo.Instances[host].FollowingCount)
}

// Unfollow で counter -1
func TestUnfollow_DecrementsInstanceCounters(t *testing.T) {
	// ローカルからリモートへのフォローは Accept まで申請になる (#3491)。このテストは
	// 成立した後を見るので、本家の backend e2e と同じ切り替えで即座に成立させる。
	t.Setenv("FORCE_FOLLOW_REMOTE_USER_FOR_TESTING", "true")
	svc, userRepo, _, _ := newSvc(t)
	instanceRepo := testutil.NewMockInstanceRepository()
	host := "remote.example"
	instanceRepo.Instances[host] = &model.Instance{Host: host, FollowersCount: 5, FollowingCount: 3}
	svc.SetInstanceRepo(instanceRepo)

	addUser(t, userRepo, "alice_local", false)
	remote := addUser(t, userRepo, "remote_user", false)
	remote.Host = &host

	_, err := svc.Follow("remote_user", "alice_local", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, 4, instanceRepo.Instances[host].FollowingCount)

	require.NoError(t, svc.Unfollow("remote_user", "alice_local"))
	assert.Equal(t, 3, instanceRepo.Instances[host].FollowingCount)
	assert.Equal(t, 5, instanceRepo.Instances[host].FollowersCount, "remote → local は followersCount を触らない")

	_, err = svc.Follow("alice_local", "remote_user", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, 6, instanceRepo.Instances[host].FollowersCount)
	require.NoError(t, svc.Unfollow("alice_local", "remote_user"))
	assert.Equal(t, 5, instanceRepo.Instances[host].FollowersCount)
	assert.Equal(t, 3, instanceRepo.Instances[host].FollowingCount, "local → remote は followingCount を触らない")
}

// Local→local follow ではどの instance counter も動かない (host=nil)
func TestFollow_LocalLocal_DoesNotTouchInstance(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	instanceRepo := testutil.NewMockInstanceRepository()
	svc.SetInstanceRepo(instanceRepo)

	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	assert.Empty(t, instanceRepo.Instances, "local-only follow は instance row を触らない")
}

// AcceptRequest 経由でも counter += 1
func TestAcceptRequest_BumpsInstanceCounters(t *testing.T) {
	svc, userRepo, _, frRepo := newSvc(t)
	instanceRepo := testutil.NewMockInstanceRepository()
	host := "remote.example"
	instanceRepo.Instances[host] = &model.Instance{Host: host}
	svc.SetInstanceRepo(instanceRepo)

	addUser(t, userRepo, "alice_local", true)
	remote := addUser(t, userRepo, "remote_user", false)
	remote.Host = &host
	frRepo.Requests["req1"] = &model.FollowRequest{
		ID: "req1", FollowerID: "remote_user", FolloweeID: "alice_local", FollowerHost: &host,
	}

	require.NoError(t, svc.AcceptRequest("alice_local", "remote_user"))
	// follower=remote → followingCount on remote host += 1
	assert.Equal(t, 1, instanceRepo.Instances[host].FollowingCount)
	assert.Equal(t, 0, instanceRepo.Instances[host].FollowersCount)
}

// TestFollow_InstanceStatsGateOff_LeavesCounters pins the upstream
// meta.enableStatsForFederatedInstances gate: when it is false, follow and
// unfollow in either direction do not touch the instance counters.
func TestFollow_InstanceStatsGateOff_LeavesCounters(t *testing.T) {
	// ローカルからリモートへのフォローは Accept まで申請になる (#3491)。このテストは
	// 成立した後を見るので、本家の backend e2e と同じ切り替えで即座に成立させる。
	t.Setenv("FORCE_FOLLOW_REMOTE_USER_FOR_TESTING", "true")
	svc, userRepo, _, _ := newSvc(t)
	instanceRepo := testutil.NewMockInstanceRepository()
	host := "remote.example"
	instanceRepo.Instances[host] = &model.Instance{Host: host, FollowersCount: 5, FollowingCount: 3}
	svc.SetInstanceRepo(instanceRepo)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{EnableStatsForFederatedInstances: false}
	svc.SetInstanceStatsGate(following.MetaInstanceStatsGate(metaRepo))

	addUser(t, userRepo, "alice_local", false)
	remote := addUser(t, userRepo, "remote_user", false)
	remote.Host = &host

	_, err := svc.Follow("remote_user", "alice_local", following.FollowOptions{})
	require.NoError(t, err)
	_, err = svc.Follow("alice_local", "remote_user", following.FollowOptions{})
	require.NoError(t, err)
	assert.Equal(t, 5, instanceRepo.Instances[host].FollowersCount)
	assert.Equal(t, 3, instanceRepo.Instances[host].FollowingCount)

	require.NoError(t, svc.Unfollow("remote_user", "alice_local"))
	assert.Equal(t, 3, instanceRepo.Instances[host].FollowingCount)

	// 運営者が有効に戻したら次の操作から集計する (meta を毎回読む)。
	metaRepo.Meta.EnableStatsForFederatedInstances = true
	require.NoError(t, svc.Unfollow("alice_local", "remote_user"))
	assert.Equal(t, 4, instanceRepo.Instances[host].FollowersCount)
}

// TestFollow_RemoteRemote_DoesNotTouchInstance pins that only follows with a
// local side are counted, matching upstream's if / else if branches.
func TestFollow_RemoteRemote_DoesNotTouchInstance(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	instanceRepo := testutil.NewMockInstanceRepository()
	h1, h2 := "one.example", "two.example"
	instanceRepo.Instances[h1] = &model.Instance{Host: h1}
	instanceRepo.Instances[h2] = &model.Instance{Host: h2}
	svc.SetInstanceRepo(instanceRepo)

	a := addUser(t, userRepo, "remote_a", false)
	a.Host = &h1
	b := addUser(t, userRepo, "remote_b", false)
	b.Host = &h2

	_, err := svc.Follow("remote_a", "remote_b", following.FollowOptions{})
	require.NoError(t, err)
	for _, h := range []string{h1, h2} {
		assert.Zero(t, instanceRepo.Instances[h].FollowersCount, h)
		assert.Zero(t, instanceRepo.Instances[h].FollowingCount, h)
	}
}

// TestMetaInstanceStatsGate covers the gate's fallbacks: a missing repository
// or an unreadable meta reports enabled (the upstream default).
func TestMetaInstanceStatsGate(t *testing.T) {
	assert.True(t, following.MetaInstanceStatsGate(nil)())

	metaRepo := testutil.NewMockMetaRepository()
	assert.True(t, following.MetaInstanceStatsGate(metaRepo)(), "no meta row")
	metaRepo.Meta = &model.Meta{EnableStatsForFederatedInstances: true}
	assert.True(t, following.MetaInstanceStatsGate(metaRepo)())
	metaRepo.Meta.EnableStatsForFederatedInstances = false
	assert.False(t, following.MetaInstanceStatsGate(metaRepo)())

	metaRepo.FetchErr = errors.New("db down")
	assert.True(t, following.MetaInstanceStatsGate(metaRepo)())
}

// SetInstanceRepo 未配線でも従来 path は動く (regression 防止)
func TestFollow_NoInstanceRepoStillWorks(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "a", false)
	addUser(t, userRepo, "b", false)
	_, err := svc.Follow("a", "b", following.FollowOptions{})
	require.NoError(t, err)
	assert.Len(t, fRepo.Followings, 1)
}

// #2106 N11: RemoteReject は Undo(Follow) を逆配送しない (federationHook 不発火) が、
// main stream の unfollow event は upstream remoteReject 同様に publish する。
func TestRemoteReject_DoesNotFederateButPublishes(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	// Follow 時の hook 呼び出しを無視するため fresh な hook / pub に差し替える。
	fed := &stubFederationHook{}
	svc.SetFederationHook(fed)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.RemoteReject("alice", "bob"))
	assert.Empty(t, fed.unfollowed, "RemoteReject は Undo(Follow) を逆配送しない")
	assert.Empty(t, fRepo.Followings)
	require.Len(t, pub.calls, 1, "main stream の unfollow event は publish する")
	assert.Equal(t, "unfollow", pub.calls[0].eventType)
}

// #2106 N11 regression guard: 通常 Unfollow は従来通り federationHook (Undo 配送) を発火する。
func TestUnfollow_StillFederates(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	fed := &stubFederationHook{}
	svc.SetFederationHook(fed)

	require.NoError(t, svc.Unfollow("alice", "bob"))
	assert.Equal(t, []string{"alice->bob"}, fed.unfollowed, "通常 Unfollow は Undo を配送する")
}

// #2106 N21: locked + autoAcceptFollowed の followee に相互フォロー相手から follow すると
// follow request でなく即 Following が成立する (upstream UserFollowingService の autoAccept)。
func TestFollow_LockedAutoAcceptFollowed_AutoAccepts(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true) // bob is locked
	userRepo.Profiles["bob"] = &model.UserProfile{UserID: "bob", AutoAcceptFollowed: true}
	// bob → alice (相互フォローの片側)。alice は非 locked なので即 Following。
	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	require.NoError(t, err)

	// alice → bob (locked + autoAcceptFollowed + 相互) → 即 Following。
	res, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Following, "autoAcceptFollowed + 相互で即 Following")
	assert.Nil(t, res.Request)
	assert.Empty(t, frRepo.Requests, "follow request は作られない")
	// alice→bob の Following が存在する。
	ex, _ := fRepo.Exists("alice", "bob")
	assert.True(t, ex)
}

// #2106 N21: autoAcceptFollowed でも相互フォローでなければ通常通り follow request 止まり。
func TestFollow_LockedAutoAcceptFollowed_NoMutual_CreatesRequest(t *testing.T) {
	svc, userRepo, _, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", true)
	userRepo.Profiles["bob"] = &model.UserProfile{UserID: "bob", AutoAcceptFollowed: true}
	// bob は alice を follow していない (非相互)。
	res, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Request, "相互でなければ request 止まり")
	assert.Nil(t, res.Following)
	assert.Len(t, frRepo.Requests, 1)
}

// #2106 N22: carefulBot=true の非 locked followee を bot が follow すると follow request になる。
func TestFollow_CarefulBot_BotFollowerCreatesRequest(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	addUser(t, userRepo, "alice", false) // alice は非 locked
	userRepo.Profiles["alice"] = &model.UserProfile{UserID: "alice", CarefulBot: true}
	// bob は bot。
	addUser(t, userRepo, "bob", false)
	userRepo.Users["bob"].IsBot = true

	res, err := svc.Follow("bob", "alice", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Request, "carefulBot + bot follower は follow request 化")
	assert.Nil(t, res.Following)
	assert.Empty(t, fRepo.Followings)
	assert.Len(t, frRepo.Requests, 1)
}

// #2106 N22: carefulBot でも非 bot follower は通常通り即フォロー成立。
func TestFollow_CarefulBot_NonBotFollowerFollowsDirectly(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	userRepo.Profiles["alice"] = &model.UserProfile{UserID: "alice", CarefulBot: true}
	addUser(t, userRepo, "carol", false) // 非 bot

	res, err := svc.Follow("carol", "alice", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Following, "非 bot follower は carefulBot の影響を受けない")
	assert.Nil(t, res.Request)
	ex, _ := fRepo.Exists("carol", "alice")
	assert.True(t, ex)
}

// 移行済みアカウントが絡む unfollow ではカウントを触らない (#2418)。upstream
// UserFollowingService.decrementFollowing の
// `if (!follower.movedToUri && !followee.movedToUri)` と同じガード。
//
// アカウント移行時に PostMoveProcess が「unfollow せずカウントだけ落とす」調整を
// 先に済ませているので、ここで無条件に減らすと二重に減る。
//
// チャートも同じ分岐の中でだけ更新する (本家は移行済みの側を "TODO: adjust
// charts" のまま触らない)。
func TestUnfollow_SkipsCountAdjustmentWhenMoved(t *testing.T) {
	cases := []struct {
		name         string
		movedUser    string // movedToUri を立てるユーザー ("" ならどちらも未移行)
		wantFollow   int    // unfollow 後の alice.FollowingCount
		wantFollower int    // unfollow 後の bob.FollowersCount
		wantCharts   int    // chartHook.OnUnfollow の呼び出し回数
	}{
		{"未移行なら通常どおり減らす", "", 0, 0, 1},
		{"follower が移行済みなら触らない", "alice", 1, 1, 0},
		{"followee が移行済みなら触らない", "bob", 1, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, userRepo, fRepo, _ := newSvc(t)
			addUser(t, userRepo, "alice", false)
			addUser(t, userRepo, "bob", false)
			_, err := svc.Follow("alice", "bob", following.FollowOptions{})
			require.NoError(t, err)
			require.Equal(t, 1, userRepo.Users["alice"].FollowingCount)
			require.Equal(t, 1, userRepo.Users["bob"].FollowersCount)
			chart := &recordingChartHook{}
			svc.SetChartHook(chart)

			if tc.movedUser != "" {
				moved := "https://elsewhere.example/users/x"
				userRepo.Users[tc.movedUser].MovedToURI = &moved
			}

			require.NoError(t, svc.Unfollow("alice", "bob"))

			// 行の削除自体はガードに関係なく必ず起きる。
			assert.Empty(t, fRepo.Followings, "the following row is always removed")
			assert.Equal(t, tc.wantFollow, userRepo.Users["alice"].FollowingCount)
			assert.Equal(t, tc.wantFollower, userRepo.Users["bob"].FollowersCount)
			assert.Len(t, chart.unfollows, tc.wantCharts)
		})
	}
}

// **述語が配線を見ていること。** 起動時の critical wiring 検査はこれを読むので、
// 常に true を返すようになると未配線を検出できなくなる。
func TestHasSilencedHostChecker(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	svc := following.NewService(testutil.NewMockUserRepository(), testutil.NewMockFollowingRepository(),
		testutil.NewMockFollowRequestRepository(), idGen)
	assert.False(t, svc.HasSilencedHostChecker(), "未配線では false")
	svc.SetSilencedHostChecker(silencedHostFunc(func(string) bool { return false }))
	assert.True(t, svc.HasSilencedHostChecker(), "配線後は true")
}

type silencedHostFunc func(string) bool

func (f silencedHostFunc) IsSilenced(host string) bool { return f(host) }

// #3067: UnfollowQuiet は upstream の silent と同じく、main stream と利用者の
// webhook を出さず、Undo(Follow) も配送しない。数と集計は通常どおり減らす。
func TestUnfollowQuiet_NoDeliveryNoNotification(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	fed := &stubFederationHook{}
	svc.SetFederationHook(fed)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)
	web := &recordingWebhookHook{}
	svc.SetWebhookHook(web)
	chart := &recordingChartHook{}
	svc.SetChartHook(chart)

	require.NoError(t, svc.UnfollowQuiet("alice", "bob"))
	assert.Empty(t, fed.unfollowed, "no Undo(Follow)")
	assert.Empty(t, pub.calls, "no main-stream event")
	assert.Zero(t, web.unfollows, "no user webhook")
	assert.Len(t, chart.unfollows, 1, "charts are still updated")
	assert.Equal(t, 0, userRepo.Users["alice"].FollowingCount)
	assert.Equal(t, 0, userRepo.Users["bob"].FollowersCount)
	assert.ErrorIs(t, svc.UnfollowQuiet("alice", "bob"), following.ErrNotFollowing)
}

// 移行済みアカウントが絡むフォローでは、行は作るがカウント・instance の集計列・
// chart を動かさない (#3330)。本家 insertFollowingDoc の
// `if (!followeeUser.movedToUri && !followerUser.movedToUri)` と同じガードで、
// Follow も AcceptRequest (承認) も insertFollowingDoc を通る。unfollow 側の
// ガード (TestUnfollow_SkipsCountAdjustmentWhenMoved) と対になる。
func TestFollow_SkipsCountsWhenMoved(t *testing.T) {
	host := "remote.example"
	const movedURI = "https://elsewhere.example/users/x"
	cases := []struct {
		name      string
		movedUser string // movedToUri を立てるユーザー ("" ならどちらも未移行)
		movedTo   string // 立てる movedToUri の値 (空文字は未移行とみなす)
		accept    bool   // true なら承認制アカウントへの申請を承認する経路
		want      int    // remote.FollowingCount / alice.FollowersCount / instance.FollowingCount
	}{
		{name: "follow: neither moved", want: 1},
		{name: "follow: empty movedToUri is not moved", movedUser: "alice", movedTo: "", want: 1},
		{name: "follow: follower moved", movedUser: "remote", movedTo: movedURI, want: 0},
		{name: "follow: followee moved", movedUser: "alice", movedTo: movedURI, want: 0},
		{name: "accept: neither moved", accept: true, want: 1},
		{name: "accept: follower moved", movedUser: "remote", movedTo: movedURI, accept: true, want: 0},
		{name: "accept: followee moved", movedUser: "alice", movedTo: movedURI, accept: true, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, userRepo, fRepo, _ := newSvc(t)
			instanceRepo := testutil.NewMockInstanceRepository()
			instanceRepo.Instances[host] = &model.Instance{Host: host}
			svc.SetInstanceRepo(instanceRepo)
			chart := &recordingChartHook{}
			svc.SetChartHook(chart)

			addUser(t, userRepo, "alice", tc.accept)
			remote := addUser(t, userRepo, "remote", false)
			remote.Host = &host
			if tc.movedUser != "" {
				moved := tc.movedTo
				userRepo.Users[tc.movedUser].MovedToURI = &moved
			}

			res, err := svc.Follow("remote", "alice", following.FollowOptions{})
			require.NoError(t, err)
			if tc.accept {
				require.NotNil(t, res.Request)
				require.NoError(t, svc.AcceptRequest("alice", "remote"))
			}

			// 行はガードに関係なく作る。
			assert.Len(t, fRepo.Followings, 1, "the following row is always created")
			assert.Equal(t, tc.want, userRepo.Users["remote"].FollowingCount)
			assert.Equal(t, tc.want, userRepo.Users["alice"].FollowersCount)
			assert.Equal(t, tc.want, instanceRepo.Instances[host].FollowingCount)
			assert.Len(t, chart.follows, tc.want)
		})
	}
}
