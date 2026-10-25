package stream

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/entity"
)

// 構築子は受け取った依存を持たせる (#3827)。依存を構築子の必須の引数にしたので、
// 起動時の検査 (criticalWiring) は入れ忘れを見なくなった。構築子が引数を捨てても
// build は通るので、ここで固定する。

func TestNewManagerKeepsDependencies(t *testing.T) {
	hardMute := &ctorHardMute{}
	muteBlock := &ctorMuteBlock{}
	policies := &ctorPolicies{}
	m := NewManager(nil, nil, hardMute, muteBlock, policies, func() string { return "ctor" })
	assert.Same(t, hardMute, m.hardMute)
	assert.Same(t, muteBlock, m.muteBlockLookup)
	assert.Same(t, policies, m.policyProvider)
	require.NotNil(t, m.ugcVisibility)
	assert.Equal(t, "ctor", m.ugcVisibility())
}

func TestNewPublishersKeepTheEntityPacker(t *testing.T) {
	pk := entity.NewPacker(nil, entity.PackerDeps{})
	assert.Same(t, pk, NewNotePublisher(nil, nil, pk).entityPacker)
	assert.Same(t, pk, NewNotificationPublisher(nil, pk).entityPacker)
	assert.Same(t, pk, NewReversiGamePublisher(nil, pk).entityPacker)
}

func TestNewServerStatsPublisherKeepsTheGate(t *testing.T) {
	called := false
	p := NewServerStatsPublisher(nil, 0, func() bool { called = true; return true })
	require.NotNil(t, p.enabled)
	assert.True(t, p.isEnabled())
	assert.True(t, called, "isEnabled が構築子で渡した判定を呼んでいない")
}

type ctorHardMute struct{}

func (*ctorHardMute) HardMutedWordsForUser(string) []byte { return nil }

type ctorMuteBlock struct{}

func (*ctorMuteBlock) MuteBlockSnapshotForUser(string) *MuteBlockSnapshot { return nil }

type ctorPolicies struct{}

func (*ctorPolicies) GetUserPolicies(string) map[string]any { return nil }
