package antennas

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// #2708: 起動時の配線検査が見る述語。**両側を固定する。**
//
// `assert.False` だけだと述語を `return true` に弱める変異が e2e でも捕まらず、
// `assert.True` を足しても**まとめて配線すると別の field を読む typo が
// 素通りする**。1 つずつ配線して他の述語が false のままであることも見る
// (#2683 review)。
func TestHandler_HasMetaRepo(t *testing.T) {
	assert.False(t, (&Handler{}).HasMetaRepo(), "未配線なら false")

	h := &Handler{}
	h.SetMetaRepo(testutil.NewMockMetaRepository())
	assert.True(t, h.HasMetaRepo(), "配線したら true")
}

// #2709 review: mute/block の 3 repo をまとめて見る述語。**conjunct を全て固定する。**
//
// `&&` を 1 つ落としたり 1 つを `|| true` にしたりする変異は、「全部未配線で
// false / 全部配線で true」だけでは捕まらない。1 つずつ欠かして見る。
func TestHandler_HasMuteBlockRepos(t *testing.T) {
	all := func() *Handler {
		h := &Handler{}
		h.SetMuteBlockRepos(
			testutil.NewMockMutingRepository(),
			testutil.NewMockBlockingRepository(),
			testutil.NewMockChannelMutingRepository(),
		)
		return h
	}
	assert.False(t, (&Handler{}).HasMuteBlockRepos(), "未配線なら false")
	assert.True(t, all().HasMuteBlockRepos(), "3 つとも配線したら true")

	t.Run("muting だけ欠ける", func(t *testing.T) {
		h := all()
		h.SetMuteBlockRepos(nil, testutil.NewMockBlockingRepository(), testutil.NewMockChannelMutingRepository())
		assert.False(t, h.HasMuteBlockRepos())
	})
	t.Run("blocking だけ欠ける", func(t *testing.T) {
		h := all()
		h.SetMuteBlockRepos(testutil.NewMockMutingRepository(), nil, testutil.NewMockChannelMutingRepository())
		assert.False(t, h.HasMuteBlockRepos())
	})
	t.Run("channelMuting だけ欠ける", func(t *testing.T) {
		h := all()
		h.SetMuteBlockRepos(testutil.NewMockMutingRepository(), testutil.NewMockBlockingRepository(), nil)
		assert.False(t, h.HasMuteBlockRepos())
	})

	// 別の依存だけを配線しても false のままであること (述語を広げる変異、
	// #2709 review L-5)。
	other := &Handler{}
	other.SetMetaRepo(testutil.NewMockMetaRepository())
	assert.False(t, other.HasMuteBlockRepos())
	assert.False(t, all().HasMetaRepo(), "他の述語は満たされないこと")
}
