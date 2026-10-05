package clips

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

// #2709 review: mute/block の 2 repo をまとめて見る述語。**conjunct を両方固定する。**
//
// channelMuting は `LoadMuteBlockSets` へ意図的に nil を渡しているので対象外。
func TestHandler_HasMuteBlockRepos(t *testing.T) {
	assert.False(t, (&Handler{}).HasMuteBlockRepos(), "未配線なら false")

	both := &Handler{}
	both.SetMuteBlockRepos(testutil.NewMockMutingRepository(), testutil.NewMockBlockingRepository())
	assert.True(t, both.HasMuteBlockRepos(), "2 つとも配線したら true")

	onlyMuting := &Handler{}
	onlyMuting.SetMuteBlockRepos(testutil.NewMockMutingRepository(), nil)
	assert.False(t, onlyMuting.HasMuteBlockRepos(), "blocking が欠けたら false")

	onlyBlocking := &Handler{}
	onlyBlocking.SetMuteBlockRepos(nil, testutil.NewMockBlockingRepository())
	assert.False(t, onlyBlocking.HasMuteBlockRepos(), "muting が欠けたら false")

	// 述語どうしの独立性 (#2709 review L-5)。
	other := &Handler{}
	other.SetMetaRepo(testutil.NewMockMetaRepository())
	assert.False(t, other.HasMuteBlockRepos())
	assert.False(t, both.HasMetaRepo(), "他の述語は満たされないこと")
}
