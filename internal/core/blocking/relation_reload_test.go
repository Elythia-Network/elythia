package blocking_test

import (
	"testing"

	"github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingReloadPublisher records notifications per scope (#2400).
type recordingReloadPublisher struct {
	muteBlock []string
	following []string
}

func (r *recordingReloadPublisher) PublishMuteBlockReload(userID string) {
	r.muteBlock = append(r.muteBlock, userID)
}

func (r *recordingReloadPublisher) PublishFollowingReload(userID string) {
	r.following = append(r.following, userID)
}

// **本 issue で一番間違えやすい点。** MuteBlockSnapshot が持つのは BlockingMe
// (= 自分を block している人) なので、block で mute-block snapshot が変わるのは
// **blockee 側**。ここを blocker にすると「block したのに相手の画面に流れ続ける」
// 形で残る。
func TestService_Block_NotifiesBlockeeNotBlocker(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)

	_, err := svc.Block("alice", "bob")
	require.NoError(t, err)

	assert.Equal(t, []string{"bob"}, pub.muteBlock,
		"mute-block の通知先は blockee (bob)。blocker ではない")
	assert.NotContains(t, pub.muteBlock, "alice")
}

// follow 関係が無い相手を block したときは following の通知を出さない。
// 無条件に出すと接続中の全 connection で無駄な DB 往復が起きる。
func TestService_Block_NoFollowingNotifyWhenNotFollowing(t *testing.T) {
	svc, userRepo, _, _, followingSvc := newSvcWithFollowing(t)
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)
	followingSvc.SetRelationReloadPublisher(pub)

	_, err := svc.Block("alice", "bob")
	require.NoError(t, err)

	assert.Equal(t, []string{"bob"}, pub.muteBlock)
	assert.Empty(t, pub.following, "解除する follow が無ければ通知しない")
}

// 実際に解除された側だけ following の通知を出す。通知は解除を担う
// following.Service が出し、blocking 側では重ねない (重ねると同じ接続へ 2 回飛ぶ)。
func TestService_Block_NotifiesUnfollowedSideOnly(t *testing.T) {
	svc, userRepo, _, fRepo, followingSvc := newSvcWithFollowing(t)
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	// alice → bob の片方向 follow だけを張る。
	require.NoError(t, fRepo.Create(&model.Following{
		ID: "f1", FollowerID: "alice", FolloweeID: "bob",
	}))
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)
	followingSvc.SetRelationReloadPublisher(pub)

	_, err := svc.Block("alice", "bob")
	require.NoError(t, err)

	assert.Equal(t, []string{"alice"}, pub.following,
		"follow を持っていた alice 側だけ、1 回だけ通知する")
}

// unblock は follow を復元しないので following の通知は出さない。
func TestService_Unblock_NotifiesBlockeeMuteBlockOnly(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	_, err := svc.Block("alice", "bob")
	require.NoError(t, err)

	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)
	require.NoError(t, svc.Unblock("alice", "bob"))

	assert.Equal(t, []string{"bob"}, pub.muteBlock)
	assert.Empty(t, pub.following, "unblock は follow を復元しないので通知しない")
}

// 失敗時は通知しない。
func TestService_Unblock_NoPublishOnFailure(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)

	assert.Error(t, svc.Unblock("alice", "bob"))
	assert.Empty(t, pub.muteBlock)
}

// publisher 未配線でも動く。
func TestService_Block_WithoutPublisher(t *testing.T) {
	svc, userRepo, _, _ := newSvc(t)
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")

	_, err := svc.Block("alice", "bob")
	assert.NoError(t, err)
}

var _ blocking.RelationReloadPublisher = (*recordingReloadPublisher)(nil)
