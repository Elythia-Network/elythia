package stream

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// #2683: 起動時の配線検査が見る述語。**false を返す側を必ず固定する。**
//
// 反転させる変異は e2e が捕まえるが、`return true` のように**弱める**変異は
// 捕まらない (未配線を検出できなくなるのに起動は成功するため)。#2682 の
// レビューで実際に素通りした形なので、同じ型のテストを置く。
//
// **4 つは独立に消せる**ので、それぞれ別に固定する。
// `SetFollowingSnapshotLookup` / `SetNoteVisibilityChecker` は未配線で
// fail-closed なので検査対象外 (述語も持たせていない)。
func TestManager_WiringPredicates(t *testing.T) {
	m := &Manager{}
	assert.False(t, m.HasHardMuteLookup(), "未配線なら false")
	assert.False(t, m.HasMuteBlockSnapshotLookup(), "未配線なら false")
	assert.False(t, m.HasPolicyProvider(), "未配線なら false")
	assert.False(t, m.HasUGCVisibilityLookup(), "未配線なら false")

	// **配線したら true も固定する。** false 側だけだと、述語が別の field を
	// 読む typo (copy-paste の付け替え漏れ) が単体・e2e とも素通りする
	// (#2683 review LOW-1)。
	// **1 つずつ配線して独立性を固定する** (#2683 review LOW-1)。
	t.Run("hardMute だけ配線", func(t *testing.T) {
		w := &Manager{}
		w.SetHardMuteLookup(stubWiringHardMute{})
		assert.True(t, w.HasHardMuteLookup(), "配線したら true")
		assert.False(t, w.HasMuteBlockSnapshotLookup(), "他の述語は満たされないこと")
		assert.False(t, w.HasPolicyProvider(), "他の述語は満たされないこと")
		assert.False(t, w.HasUGCVisibilityLookup(), "他の述語は満たされないこと")
	})
	t.Run("muteBlock だけ配線", func(t *testing.T) {
		w := &Manager{}
		w.SetMuteBlockSnapshotLookup(stubWiringMuteBlock{})
		assert.True(t, w.HasMuteBlockSnapshotLookup(), "配線したら true")
		assert.False(t, w.HasHardMuteLookup(), "他の述語は満たされないこと")
		assert.False(t, w.HasPolicyProvider(), "他の述語は満たされないこと")
		assert.False(t, w.HasUGCVisibilityLookup(), "他の述語は満たされないこと")
	})
	t.Run("policyProvider だけ配線", func(t *testing.T) {
		w := &Manager{}
		w.SetPolicyProvider(stubWiringStreamPolicy{})
		assert.True(t, w.HasPolicyProvider(), "配線したら true")
		assert.False(t, w.HasHardMuteLookup(), "他の述語は満たされないこと")
		assert.False(t, w.HasMuteBlockSnapshotLookup(), "他の述語は満たされないこと")
		assert.False(t, w.HasUGCVisibilityLookup(), "他の述語は満たされないこと")
	})
	t.Run("ugcVisibility だけ配線", func(t *testing.T) {
		w := &Manager{}
		w.SetUGCVisibilityLookup(func() string { return "all" })
		assert.True(t, w.HasUGCVisibilityLookup(), "配線したら true")
		assert.False(t, w.HasHardMuteLookup(), "他の述語は満たされないこと")
		assert.False(t, w.HasMuteBlockSnapshotLookup(), "他の述語は満たされないこと")
		assert.False(t, w.HasPolicyProvider(), "他の述語は満たされないこと")
	})

}

type stubWiringHardMute struct{}

func (stubWiringHardMute) HardMutedWordsForUser(string) []byte { return nil }

type stubWiringMuteBlock struct{}

func (stubWiringMuteBlock) MuteBlockSnapshotForUser(string) *MuteBlockSnapshot { return nil }

type stubWiringStreamPolicy struct{}

func (stubWiringStreamPolicy) GetUserPolicies(string) map[string]any { return nil }
