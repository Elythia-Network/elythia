package notes

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// #2708 / #2709 review: notes handler の可視性 gate は 7 つ。**1 つずつ配線して独立性を固定する**
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

	// 述語 -> その 1 つだけを配線する関数。
	wire := map[string]func(*Handler){
		"driveFileRepo":     func(h *Handler) { h.SetDriveFileRepo(testutil.NewMockDriveFileRepository()) },
		"metaRepo":          func(h *Handler) { h.SetMetaRepo(testutil.NewMockMetaRepository()) },
		"blockingRepo":      func(h *Handler) { h.SetBlockingRepo(testutil.NewMockBlockingRepository()) },
		"mutingRepo":        func(h *Handler) { h.SetMutingRepo(testutil.NewMockMutingRepository()) },
		"channelMutingRepo": func(h *Handler) { h.SetChannelMutingRepo(testutil.NewMockChannelMutingRepository()) },
		"userFollowingRepo": func(h *Handler) { h.SetUserFollowingRepo(testutil.NewMockFollowingRepository()) },
		"renoteMutingRepo":  func(h *Handler) { h.SetRenoteMutingRepo(testutil.NewMockRenoteMutingRepository()) },
	}
	check := map[string]func(*Handler) bool{
		"driveFileRepo":     (*Handler).HasDriveFileRepo,
		"metaRepo":          (*Handler).HasMetaRepo,
		"blockingRepo":      (*Handler).HasBlockingRepo,
		"mutingRepo":        (*Handler).HasMutingRepo,
		"channelMutingRepo": (*Handler).HasChannelMutingRepo,
		"userFollowingRepo": (*Handler).HasUserFollowingRepo,
		"renoteMutingRepo":  (*Handler).HasRenoteMutingRepo,
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
