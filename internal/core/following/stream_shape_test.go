package following_test

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRelations reads the viewer->target relation from the same repositories
// the service writes, so the packed body reflects the row state at publish time.
type repoRelations struct {
	f  *testutil.MockFollowingRepository
	fr *testutil.MockFollowRequestRepository
}

func (r repoRelations) Apply(d *entity.UserDetailed, viewerID string, target *model.User, _ *model.UserProfile) bool {
	isFollowing, _ := r.f.Exists(viewerID, target.ID)
	pending, _ := r.fr.Exists(viewerID, target.ID)
	d.IsFollowing = &isFollowing
	d.HasPendingFollowRequestFromYou = &pending
	d.EnsureRelationFlags()
	return isFollowing
}

const streamRemoteHost = "remote.example"

type streamInstances struct{}

func (streamInstances) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	name := "Remote"
	for _, h := range hosts {
		if h == streamRemoteHost {
			return []*model.Instance{{Host: streamRemoteHost, Name: &name}}, nil
		}
	}
	return nil, nil
}

type streamEmojis struct{}

func (streamEmojis) FindManyByNamesAndHost([]string, *string) ([]*model.Emoji, error) {
	return nil, nil
}

// newStreamSvc wires the service as production does: the main stream body is
// packed by userpack with lookups over the mock repositories. Every user gets
// a profile that shows followers counts to followers only.
func newStreamSvc(t *testing.T) (*following.Service, *testutil.MockUserRepository, *stubMainStreamPublisher) {
	t.Helper()
	svc, userRepo, fRepo, frRepo := newSvc(t)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc.SetUserPacker(userpack.New(userpack.Lookups{
		Instances: streamInstances{},
		Emojis:    streamEmojis{},
		Profiles:  userRepo,
		Relations: repoRelations{f: fRepo, fr: frRepo},
	}, idGen))
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)
	return svc, userRepo, pub
}

func addStreamUser(repo *testutil.MockUserRepository, uid string, locked bool, host *string) *model.User {
	u := &model.User{ID: uid, Username: uid, IsLocked: locked, Host: host, FollowersCount: 7, FollowingCount: 3}
	repo.Users[uid] = u
	repo.Profiles[uid] = &model.UserProfile{UserID: uid,
		FollowersVisibility: model.FollowingVisibilityFollowers, FollowingVisibility: model.FollowingVisibilityPublic}
	return u
}

func bodyMap(t *testing.T, body any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func callsOf(pub *stubMainStreamPublisher, event string) []mainEventCall {
	var out []mainEventCall
	for _, c := range pub.calls {
		if c.eventType == event {
			out = append(out, c)
		}
	}
	return out
}

// follow の相手は、Webhook と同じく閲覧者=フォローした側の UserDetailedNotMe (#3330)。
// 本家 UserFollowingService は profile を読み、関係とフォロワー限定のカウントを載せる。
func TestStream_FollowCarriesDetailedNotMe(t *testing.T) {
	// ローカルからリモートへのフォローは Accept まで申請になる (#3491)。このテストは
	// 成立した後を見るので、本家の backend e2e と同じ切り替えで即座に成立させる。
	t.Setenv("FORCE_FOLLOW_REMOTE_USER_FOR_TESTING", "true")
	svc, userRepo, pub := newStreamSvc(t)
	addStreamUser(userRepo, "alice", false, nil)
	host := streamRemoteHost
	addStreamUser(userRepo, "bob", false, &host)
	desc := "hello"
	userRepo.Profiles["bob"].Description = &desc

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	follow := callsOf(pub, "follow")
	require.Len(t, follow, 1)
	assert.Equal(t, "alice", follow[0].userID)
	m := bodyMap(t, follow[0].body)
	assert.Equal(t, "bob", m["id"])
	assert.Equal(t, "hello", m["description"], "profile is read")
	assert.Equal(t, true, m["isFollowing"])
	assert.Equal(t, false, m["hasPendingFollowRequestFromYou"])
	// 7 + このフォローの 1。
	assert.EqualValues(t, 8, m["followersCount"], "followers-only count is visible to the new follower")
	assert.Equal(t, "Remote", m["instance"].(map[string]any)["name"])
}

// unfollow の後は関係の行が無いので、フォロワー限定のカウントを伏せる (本家と同じ)。
func TestStream_UnfollowHidesFollowersOnlyCounts(t *testing.T) {
	svc, userRepo, pub := newStreamSvc(t)
	addStreamUser(userRepo, "alice", false, nil)
	addStreamUser(userRepo, "bob", false, nil)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	pub.calls = nil

	require.NoError(t, svc.Unfollow("alice", "bob"))

	unfollow := callsOf(pub, "unfollow")
	require.Len(t, unfollow, 1)
	m := bodyMap(t, unfollow[0].body)
	assert.Equal(t, "bob", m["id"])
	assert.Equal(t, false, m["isFollowing"])
	assert.EqualValues(t, 0, m["followersCount"], "followers-only count must be hidden after unfollow")
	assert.EqualValues(t, 3, m["followingCount"])
	assert.Contains(t, m, "description", "UserDetailedNotMe shape")
}

// followed と receiveFollowRequest は本家と同じく UserLite (pack の既定)。
// リモートの相手には instance が付く。
func TestStream_FollowedAndReceiveFollowRequestCarryUserLite(t *testing.T) {
	host := streamRemoteHost
	t.Run("followed", func(t *testing.T) {
		svc, userRepo, pub := newStreamSvc(t)
		addStreamUser(userRepo, "carol", false, &host)
		addStreamUser(userRepo, "bob", false, nil)
		_, err := svc.Follow("carol", "bob", following.FollowOptions{})
		require.NoError(t, err)
		followed := callsOf(pub, "followed")
		require.Len(t, followed, 1)
		assert.Equal(t, "bob", followed[0].userID)
		m := bodyMap(t, followed[0].body)
		assert.Equal(t, "Remote", m["instance"].(map[string]any)["name"])
		assert.NotContains(t, m, "description", "UserLite")
		// フォローした側がリモートなら、そちらの main stream には流さない (本家と同じ)。
		assert.Empty(t, callsOf(pub, "follow"))
	})
	t.Run("receiveFollowRequest", func(t *testing.T) {
		svc, userRepo, pub := newStreamSvc(t)
		addStreamUser(userRepo, "carol", false, &host)
		addStreamUser(userRepo, "bob", true, nil)
		_, err := svc.Follow("carol", "bob", following.FollowOptions{})
		require.NoError(t, err)
		req := callsOf(pub, "receiveFollowRequest")
		require.Len(t, req, 1)
		m := bodyMap(t, req[0].body)
		assert.Equal(t, "carol", m["id"])
		assert.Equal(t, "Remote", m["instance"].(map[string]any)["name"])
	})
}

// フォローされた側がリモートなら followed / receiveFollowRequest を流さない (本家と同じ)。
func TestStream_RemoteFolloweeGetsNoEvent(t *testing.T) {
	host := streamRemoteHost
	for _, locked := range []bool{false, true} {
		svc, userRepo, pub := newStreamSvc(t)
		addStreamUser(userRepo, "alice", false, nil)
		addStreamUser(userRepo, "dave", locked, &host)
		_, err := svc.Follow("alice", "dave", following.FollowOptions{})
		require.NoError(t, err)
		for _, c := range pub.calls {
			assert.NotEqual(t, "dave", c.userID, "locked=%v: no event to a remote followee", locked)
		}
	}
}

// 承認で成立した follow も同じ形。
func TestStream_AcceptRequestCarriesDetailedNotMe(t *testing.T) {
	svc, userRepo, pub := newStreamSvc(t)
	addStreamUser(userRepo, "alice", false, nil)
	addStreamUser(userRepo, "bob", true, nil)
	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	pub.calls = nil

	require.NoError(t, svc.AcceptRequest("bob", "alice"))
	follow := callsOf(pub, "follow")
	require.Len(t, follow, 1)
	m := bodyMap(t, follow[0].body)
	assert.Equal(t, true, m["isFollowing"])
	assert.EqualValues(t, 8, m["followersCount"])
	require.Len(t, callsOf(pub, "followed"), 1)
}

// 申請の拒否・拒否された申請 (Reject(Follow)) で流す unfollow も同じ形で、申請中の
// 印は消えている。取り消し (cancel) は本家と同じく unfollow を流さない。
func TestStream_RequestResolvedCarriesDetailedNotMe(t *testing.T) {
	for name, resolve := range map[string]func(*following.Service) error{
		"reject":       func(s *following.Service) error { return s.RejectRequest("bob", "alice") },
		"remoteReject": func(s *following.Service) error { return s.RemoteReject("alice", "bob") },
	} {
		t.Run(name, func(t *testing.T) {
			svc, userRepo, pub := newStreamSvc(t)
			addStreamUser(userRepo, "alice", false, nil)
			addStreamUser(userRepo, "bob", true, nil)
			_, err := svc.Follow("alice", "bob", following.FollowOptions{})
			require.NoError(t, err)
			pub.calls = nil

			require.NoError(t, resolve(svc))
			unfollow := callsOf(pub, "unfollow")
			require.Len(t, unfollow, 1)
			m := bodyMap(t, unfollow[0].body)
			assert.Equal(t, "bob", m["id"])
			assert.Equal(t, false, m["hasPendingFollowRequestFromYou"])
			assert.EqualValues(t, 0, m["followersCount"])
		})
	}
}

// profile を読めないときは流さない (本家は findOneByOrFail で例外になる)。
// profile 無しで組むとフォロワー限定のカウントが出てしまう。
func TestStream_ProfileMissingDropsDetailedEvent(t *testing.T) {
	svc, userRepo, pub := newStreamSvc(t)
	addStreamUser(userRepo, "alice", false, nil)
	addStreamUser(userRepo, "bob", false, nil)
	delete(userRepo.Profiles, "bob")

	_, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	assert.Empty(t, callsOf(pub, "follow"))
	assert.Len(t, callsOf(pub, "followed"), 1, "followed (UserLite) does not need the profile")
}
