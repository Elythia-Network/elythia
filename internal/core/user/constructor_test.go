package user

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/entity"
)

// NewService は受け取った Packer を service に持たせる (#3825)。依存を構築子の
// 必須の引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	svc := NewService(nil, nil, nil, nil, packer)
	assert.Same(t, packer, svc.entityPacker)
}
