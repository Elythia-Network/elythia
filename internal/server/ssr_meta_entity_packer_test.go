package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/entity"
)

// newSSRMetaHandler は受け取った依存を handler に持たせる (#3823)。依存を構築子の
// 必須の引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewSSRMetaHandlerKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	h := newSSRMetaHandler(&config.Config{URL: "https://local.example"}, nil, nil, nil, nil, ssrMetaDeps{}, packer)
	assert.Same(t, packer, h.entityPacker)
}
