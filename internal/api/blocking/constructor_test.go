package blocking

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/stretchr/testify/assert"
)

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	extras := &manyExtras{}
	user := userpack.New(nil, userpack.Lookups{}, nil)
	h := NewHandler(nil, nil, nil, packer, extras, user)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, extras, h.extras)
	assert.Same(t, user, h.packer)
}
