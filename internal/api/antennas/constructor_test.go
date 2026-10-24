package antennas

import (
	"testing"

	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	hider := notehide.NewHider(nil, nil)
	packer := entity.NewPacker(entity.PackerDeps{})
	meta := testutil.NewMockMetaRepository()
	muting := testutil.NewMockMutingRepository()
	blocking := testutil.NewMockBlockingRepository()
	channelMuting := testutil.NewMockChannelMutingRepository()

	h := NewHandler(nil, nil, nil, hider, packer, meta, muting, blocking, channelMuting)

	assert.Same(t, hider, h.noteHider)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, meta, h.metaRepo)
	assert.Same(t, muting, h.mutingRepo)
	assert.Same(t, blocking, h.blockingRepo)
	assert.Same(t, channelMuting, h.channelMutingRepo)
}
