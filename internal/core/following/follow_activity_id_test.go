package following_test

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家 2026.11.1 (#18163): リモートから受けた Follow の id を申請・following に残し、
// 承認の Accept と、外す・断るときの Reject の中の Follow に入れる。

const remoteFollowID = "https://remote.example/follows/1"

func addRemoteFollower(t *testing.T, repo *testutil.MockUserRepository, id string) *model.User {
	t.Helper()
	host := "remote.example"
	uri := "https://remote.example/users/" + id
	u := &model.User{ID: id, Username: id, Host: &host, URI: &uri}
	repo.Users[id] = u
	return u
}

func followingPair(t *testing.T, repo *testutil.MockFollowingRepository, followerID, followeeID string) *model.Following {
	t.Helper()
	f, err := repo.FindByPair(followerID, followeeID)
	require.NoError(t, err)
	return f
}

func TestFollow_KeepsTheRemoteFollowID(t *testing.T) {
	t.Run("direct follow keeps the id and Reject on removal wraps it", func(t *testing.T) {
		svc, userRepo, fRepo, _ := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addUser(t, userRepo, "bob", false)
		fed := &stubFederationHook{}
		svc.SetFederationHook(fed)

		res, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID})
		require.NoError(t, err)
		require.NotNil(t, res.Following)
		require.NotNil(t, followingPair(t, fRepo, "remote1", "bob").RequestID)
		assert.Equal(t, remoteFollowID, *followingPair(t, fRepo, "remote1", "bob").RequestID)

		// ローカルの人がリモートのフォロワーを外す (following/invalidate)
		require.NoError(t, svc.Unfollow("remote1", "bob"))
		assert.Equal(t, []string{remoteFollowID}, fed.unfollowedIDs)
	})

	t.Run("a new Follow for an existing following replaces the id", func(t *testing.T) {
		svc, userRepo, fRepo, _ := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addUser(t, userRepo, "bob", false)
		_, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID})
		require.NoError(t, err)

		_, err = svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID + "/again"})
		require.ErrorIs(t, err, following.ErrAlreadyFollowing)
		assert.Equal(t, remoteFollowID+"/again", *followingPair(t, fRepo, "remote1", "bob").RequestID)
	})

	t.Run("request keeps the id, Accept wraps it and the following inherits it", func(t *testing.T) {
		svc, userRepo, fRepo, frRepo := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addUser(t, userRepo, "bob", true)
		fed := &stubFederationHook{}
		svc.SetFederationHook(fed)

		res, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID})
		require.NoError(t, err)
		require.NotNil(t, res.Request)
		req, err := frRepo.FindByPair("remote1", "bob")
		require.NoError(t, err)
		require.NotNil(t, req.RequestID)
		assert.Equal(t, remoteFollowID, *req.RequestID)

		require.NoError(t, svc.AcceptRequest("bob", "remote1"))
		assert.Equal(t, []string{remoteFollowID}, fed.acceptedIDs)
		f := followingPair(t, fRepo, "remote1", "bob")
		require.NotNil(t, f.RequestID)
		assert.Equal(t, remoteFollowID, *f.RequestID)
	})

	// 申請中に新しい id の Follow が来たら、承認の Accept は新しい id を包む (本家
	// createFollowRequest は申請を作り直すので、最新の id になる)。
	t.Run("a new Follow for a pending request replaces the id", func(t *testing.T) {
		svc, userRepo, _, frRepo := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addUser(t, userRepo, "bob", true)
		fed := &stubFederationHook{}
		svc.SetFederationHook(fed)
		_, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID})
		require.NoError(t, err)

		_, err = svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID + "/again"})
		require.ErrorIs(t, err, following.ErrAlreadyRequested)
		req, err := frRepo.FindByPair("remote1", "bob")
		require.NoError(t, err)
		require.NotNil(t, req.RequestID)
		assert.Equal(t, remoteFollowID+"/again", *req.RequestID)

		require.NoError(t, svc.AcceptRequest("bob", "remote1"))
		assert.Equal(t, []string{remoteFollowID + "/again"}, fed.acceptedIDs)
	})

	t.Run("rejecting a request wraps the request's id", func(t *testing.T) {
		svc, userRepo, _, _ := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addUser(t, userRepo, "bob", true)
		fed := &stubFederationHook{}
		svc.SetFederationHook(fed)
		_, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID})
		require.NoError(t, err)

		require.NoError(t, svc.RejectRequest("bob", "remote1"))
		assert.Equal(t, []string{remoteFollowID}, fed.unfollowedIDs)
	})

	// 申請とフォローが両方残った過去の行では、申請に id が無ければフォローの id を使う
	// (本家 deliverReject の request?.requestId ?? following?.requestId)。
	t.Run("rejecting a request without an id falls back to the following's", func(t *testing.T) {
		svc, userRepo, fRepo, frRepo := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addUser(t, userRepo, "bob", false)
		_, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID})
		require.NoError(t, err)
		require.NotNil(t, followingPair(t, fRepo, "remote1", "bob"))
		require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "r1", FollowerID: "remote1", FolloweeID: "bob"}))
		fed := &stubFederationHook{}
		svc.SetFederationHook(fed)

		require.NoError(t, svc.RejectRequest("bob", "remote1"))
		assert.Equal(t, []string{remoteFollowID}, fed.unfollowedIDs)
	})

	// 列に入らない長さの id は残さず、フォローそのものは受ける。
	t.Run("an id longer than the column is not kept", func(t *testing.T) {
		svc, userRepo, fRepo, frRepo := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addRemoteFollower(t, userRepo, "remote2")
		addUser(t, userRepo, "bob", false)
		addUser(t, userRepo, "carol", true)
		long := "https://remote.example/" + strings.Repeat("a", 600)

		_, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: long})
		require.NoError(t, err)
		assert.Nil(t, followingPair(t, fRepo, "remote1", "bob").RequestID)

		_, err = svc.Follow("remote2", "carol", following.FollowOptions{RequestID: long[:200]})
		require.NoError(t, err)
		req, err := frRepo.FindByPair("remote2", "carol")
		require.NoError(t, err)
		assert.Nil(t, req.RequestID, "follow_request.requestId は varchar(128)")
	})

	// NUL を含む id は列に入らない (22021 で落ちる) ので残さず、フォローそのものは受ける。
	// mock の repository は NUL を弾かないので、残さないこと (RequestID が nil) を見る。
	t.Run("an id with NUL is not kept and the follow is still accepted", func(t *testing.T) {
		svc, userRepo, fRepo, frRepo := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addRemoteFollower(t, userRepo, "remote2")
		addUser(t, userRepo, "bob", false)
		addUser(t, userRepo, "carol", true)
		withNUL := remoteFollowID + "#\x00"

		res, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: withNUL})
		require.NoError(t, err)
		require.NotNil(t, res.Following)
		assert.Nil(t, followingPair(t, fRepo, "remote1", "bob").RequestID)

		res, err = svc.Follow("remote2", "carol", following.FollowOptions{RequestID: withNUL})
		require.NoError(t, err)
		require.NotNil(t, res.Request)
		req, err := frRepo.FindByPair("remote2", "carol")
		require.NoError(t, err)
		assert.Nil(t, req.RequestID)
	})

	// 申請中に来た新しい Follow の id が列に入らないときは、本家 (申請を作り直す) と同じく
	// 申請の id を NULL にする。
	t.Run("a new Follow whose id cannot be stored clears the pending request's id", func(t *testing.T) {
		svc, userRepo, _, frRepo := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addUser(t, userRepo, "bob", true)
		_, err := svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID})
		require.NoError(t, err)

		_, err = svc.Follow("remote1", "bob", following.FollowOptions{RequestID: remoteFollowID + "#\x00"})
		require.ErrorIs(t, err, following.ErrAlreadyRequested)
		req, err := frRepo.FindByPair("remote1", "bob")
		require.NoError(t, err)
		assert.Nil(t, req.RequestID)
	})

	// varchar(n) は文字数で数えるので、バイト数が上限を超えても文字数が収まれば残す。
	t.Run("the length is counted in characters", func(t *testing.T) {
		svc, userRepo, _, frRepo := newSvc(t)
		addRemoteFollower(t, userRepo, "remote1")
		addUser(t, userRepo, "carol", true)
		// 128 文字で 128 バイトを超える (「あ」は 3 バイト)
		id := "https://remote.example/" + strings.Repeat("あ", 128-len("https://remote.example/"))
		require.Greater(t, len(id), 128)

		_, err := svc.Follow("remote1", "carol", following.FollowOptions{RequestID: id})
		require.NoError(t, err)
		req, err := frRepo.FindByPair("remote1", "carol")
		require.NoError(t, err)
		require.NotNil(t, req.RequestID)
		assert.Equal(t, id, *req.RequestID)
	})
}
