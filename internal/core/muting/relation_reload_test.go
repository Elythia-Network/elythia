package muting_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/muting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingReloadPublisher records which users were notified (#2400).
type recordingReloadPublisher struct{ users []string }

func (r *recordingReloadPublisher) PublishMuteBlockReload(userID string) {
	r.users = append(r.users, userID)
}

// mute した本人 (muter) の snapshot が変わる。mutee 側は変わらない。
func TestService_Mute_PublishesReloadForMuter(t *testing.T) {
	svc, userRepo, _ := newMuteService(t)
	addUser(userRepo, "bob")
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)

	_, err := svc.Mute("alice", "bob", nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"alice"}, pub.users, "通知先は muter")
}

func TestService_Unmute_PublishesReloadForMuter(t *testing.T) {
	svc, userRepo, _ := newMuteService(t)
	addUser(userRepo, "bob")
	_, err := svc.Mute("alice", "bob", nil)
	require.NoError(t, err)

	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)
	require.NoError(t, svc.Unmute("alice", "bob"))

	assert.Equal(t, []string{"alice"}, pub.users)
}

// 失敗時は通知しない。存在しない mute の解除で reload を撒くと無駄な DB 往復になる。
func TestService_Unmute_NoPublishOnFailure(t *testing.T) {
	svc, userRepo, _ := newMuteService(t)
	addUser(userRepo, "bob")
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)

	assert.Error(t, svc.Unmute("alice", "bob"))
	assert.Empty(t, pub.users)
}

// publisher 未配線でも動く (既存構成が壊れない)。
func TestService_Mute_WithoutPublisher(t *testing.T) {
	svc, userRepo, _ := newMuteService(t)
	addUser(userRepo, "bob")
	_, err := svc.Mute("alice", "bob", nil)
	assert.NoError(t, err)
}

func TestRenoteService_Mute_PublishesReload(t *testing.T) {
	svc, userRepo, _ := newRenoteMuteService(t)
	addUser(userRepo, "bob")
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)

	_, err := svc.Mute("alice", "bob")
	require.NoError(t, err)
	assert.Equal(t, []string{"alice"}, pub.users)
}

func TestRenoteService_Unmute_PublishesReload(t *testing.T) {
	svc, userRepo, _ := newRenoteMuteService(t)
	addUser(userRepo, "bob")
	_, err := svc.Mute("alice", "bob")
	require.NoError(t, err)

	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)
	require.NoError(t, svc.Unmute("alice", "bob"))
	assert.Equal(t, []string{"alice"}, pub.users)
}

func TestRenoteService_Unmute_NoPublishOnFailure(t *testing.T) {
	svc, userRepo, _ := newRenoteMuteService(t)
	addUser(userRepo, "bob")
	pub := &recordingReloadPublisher{}
	svc.SetRelationReloadPublisher(pub)

	assert.Error(t, svc.Unmute("alice", "bob"))
	assert.Empty(t, pub.users)
}

var _ muting.RelationReloadPublisher = (*recordingReloadPublisher)(nil)
