package abuse

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/entity"
)

// NewCreatedNotifier は受け取った依存を持たせる (#3823)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewCreatedNotifierKeepsDependencies(t *testing.T) {
	group := bgwork.New()
	packer := entity.NewPacker(nil, entity.PackerDeps{})
	n := NewCreatedNotifier(nil, nil, nil, group, packer)
	assert.Same(t, group, n.background)
	assert.Same(t, packer, n.entityPacker)
}
