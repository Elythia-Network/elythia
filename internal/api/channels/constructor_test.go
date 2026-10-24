package channels

import (
	"testing"

	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	hider := notehide.NewHider(nil, nil)
	packer := entity.NewPacker(entity.PackerDeps{})
	meta := testutil.NewMockMetaRepository()

	h := NewHandler(nil, nil, hider, packer, meta, func() string { return "local" })

	assert.Same(t, hider, h.noteHider)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, meta, h.metaRepo)
	require.NotNil(t, h.ugcVisibilityFn)
	assert.Equal(t, "local", h.ugcVisibilityFn())
}
