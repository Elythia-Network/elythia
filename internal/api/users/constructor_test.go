package users

import (
	"testing"

	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) と wiring-check は入れ忘れを
// 見なくなった。構築子が引数を捨てても build は通るので、ここで固定する。
// 関数の依存は比べられないので、呼んで渡したものが返ることを見る。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	hider := notehide.NewHider(nil, nil)
	packer := entity.NewPacker(nil, entity.PackerDeps{})
	policies := &stubListsPolicy{}
	meta := testutil.NewMockMetaRepository()
	channelMuting := testutil.NewMockChannelMutingRepository()
	muting := testutil.NewMockMutingRepository()
	blocking := testutil.NewMockBlockingRepository()
	reader := repository.NewUserRelationRepository(nil)

	h := NewHandler(nil, nil, nil, nil, hider, packer, policies, meta,
		func() string { return "local" }, channelMuting, muting, blocking, reader)

	assert.Same(t, hider, h.noteHider)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, policies, h.rolePolicyProvider)
	assert.Same(t, meta, h.metaRepo)
	require.NotNil(t, h.ugcVisibilityFn)
	assert.Equal(t, "local", h.ugcVisibilityFn())
	assert.Same(t, channelMuting, h.channelMutingRepo)
	assert.Same(t, muting, h.mutingRepo)
	assert.Same(t, blocking, h.blockingRepo)
	assert.Equal(t, reader, h.relationReader)
}
