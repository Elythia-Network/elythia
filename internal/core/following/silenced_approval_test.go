package following_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubSilenced struct{ hosts map[string]bool }

func (s stubSilenced) IsSilenced(host string) bool { return s.hosts[host] }

// **サイレンスしたホストからのフォローは承認待ちになること。**
//
// これが無いと、サイレンス指定したインスタンスの利用者が未施錠のローカル
// アカウントを承認なしで即フォローでき、以後 followers 限定ノートが配送される。
// upstream `UserFollowingService.follow` の 4 つ目の OR 条件。
func TestFollow_SilencedRemoteFollowerNeedsApproval(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	host := "silenced.example"
	userRepo.Users["remote"] = &model.User{ID: "remote", Username: "r", Host: &host}
	addUser(t, userRepo, "local", false) // 未施錠のローカル
	svc.SetSilencedHostChecker(stubSilenced{hosts: map[string]bool{host: true}})

	res, err := svc.Follow("remote", "local", following.FollowOptions{})
	require.NoError(t, err)
	assert.Nil(t, res.Following, "即フォローを成立させないこと")
	require.NotNil(t, res.Request, "承認待ちになること")
	assert.Empty(t, fRepo.Followings)
	assert.Len(t, frRepo.Requests, 1)
}

// サイレンスしていないホストは従来どおり即フォローできること。
func TestFollow_NonSilencedRemoteFollowerStillAutoAccepts(t *testing.T) {
	svc, userRepo, fRepo, frRepo := newSvc(t)
	host := "ok.example"
	userRepo.Users["remote"] = &model.User{ID: "remote", Username: "r", Host: &host}
	addUser(t, userRepo, "local", false)
	svc.SetSilencedHostChecker(stubSilenced{hosts: map[string]bool{"other.example": true}})

	res, err := svc.Follow("remote", "local", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Following, "サイレンスしていない相手は従来どおり")
	assert.Len(t, fRepo.Followings, 1)
	assert.Empty(t, frRepo.Requests)
}

// ローカル同士は影響を受けないこと (follower.Host が nil)。
func TestFollow_LocalFollowerUnaffectedBySilence(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)
	svc.SetSilencedHostChecker(stubSilenced{hosts: map[string]bool{"": true}})

	res, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Following)
	assert.Len(t, fRepo.Followings, 1)
}
