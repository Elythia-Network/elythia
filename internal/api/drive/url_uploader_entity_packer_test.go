package drive

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/entity"
)

// NewURLUploader は受け取った依存を持たせる (#3823)。依存を構築子の必須の引数に
// したので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewURLUploaderKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	assert.Same(t, packer, NewURLUploader(nil, nil, nil, nil, "", 0, packer).entityPacker)
}
