package notes

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// #2683: 起動時の配線検査が見る述語。**false を返す側を必ず固定する。**
//
// 反転させる変異は e2e が捕まえるが、`return true` のように**弱める**変異は
// 捕まらない (未配線を検出できなくなるのに起動は成功するため)。#2682 の
// レビューで実際に素通りした形なので、同じ型のテストを置く。
func TestHandler_HasPolicyProvider(t *testing.T) {
	assert.False(t, (&Handler{}).HasPolicyProvider(),
		"未配線なら false を返すこと (常に true だと検査が無意味になる)")

	// **配線したら true も固定する。** false 側だけだと、述語が別の field を
	// 読む typo (copy-paste の付け替え漏れ) が単体・e2e とも素通りする
	// (#2683 review LOW-1)。
	h := &Handler{}
	h.SetPolicyProvider(stubWiringPolicyProvider{})
	assert.True(t, h.HasPolicyProvider(), "配線したら true")

}

type stubWiringPolicyProvider struct{}

func (stubWiringPolicyProvider) GetUserPolicies(string) map[string]any { return nil }

// #2708 / #2709 review: notes handler の可視性 gate は 8 つ。**1 つずつ配線して独立性を固定する**
// (まとめて配線すると別の field を読む typo が素通りする)。
func TestHandler_WiringPredicates2708(t *testing.T) {
	empty := &Handler{}
	assert.False(t, empty.HasDriveFileRepo(), "未配線なら false")
	assert.False(t, empty.HasMetaRepo(), "未配線なら false")
	assert.False(t, empty.HasBlockingRepo(), "未配線なら false")
	assert.False(t, empty.HasMutingRepo(), "未配線なら false")
	assert.False(t, empty.HasChannelMutingRepo(), "未配線なら false")
	assert.False(t, empty.HasUserFollowingRepo(), "未配線なら false")
	assert.False(t, empty.HasRenoteMutingRepo(), "未配線なら false")
	assert.False(t, empty.HasUGCVisibility(), "未設定なら false")

	// 述語 -> その 1 つだけを配線する関数。
	wire := map[string]func(*Handler){
		"driveFileRepo":     func(h *Handler) { h.SetDriveFileRepo(testutil.NewMockDriveFileRepository()) },
		"metaRepo":          func(h *Handler) { h.SetMetaRepo(testutil.NewMockMetaRepository()) },
		"blockingRepo":      func(h *Handler) { h.SetBlockingRepo(testutil.NewMockBlockingRepository()) },
		"mutingRepo":        func(h *Handler) { h.SetMutingRepo(testutil.NewMockMutingRepository()) },
		"channelMutingRepo": func(h *Handler) { h.SetChannelMutingRepo(testutil.NewMockChannelMutingRepository()) },
		"userFollowingRepo": func(h *Handler) { h.SetUserFollowingRepo(testutil.NewMockFollowingRepository()) },
		"renoteMutingRepo":  func(h *Handler) { h.SetRenoteMutingRepo(testutil.NewMockRenoteMutingRepository()) },
		"ugcVisibility":     func(h *Handler) { h.SetUGCVisibility("local") },
	}
	check := map[string]func(*Handler) bool{
		"driveFileRepo":     (*Handler).HasDriveFileRepo,
		"metaRepo":          (*Handler).HasMetaRepo,
		"blockingRepo":      (*Handler).HasBlockingRepo,
		"mutingRepo":        (*Handler).HasMutingRepo,
		"channelMutingRepo": (*Handler).HasChannelMutingRepo,
		"userFollowingRepo": (*Handler).HasUserFollowingRepo,
		"renoteMutingRepo":  (*Handler).HasRenoteMutingRepo,
		"ugcVisibility":     (*Handler).HasUGCVisibility,
	}
	for name, set := range wire {
		t.Run(name+" だけ配線", func(t *testing.T) {
			h := &Handler{}
			set(h)
			for other, has := range check {
				if other == name {
					assert.True(t, has(h), "配線したら true")
				} else {
					assert.False(t, has(h), "他の述語は満たされないこと: "+other)
				}
			}
		})
	}
}
