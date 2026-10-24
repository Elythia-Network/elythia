package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// NewHandler は受け取った依存を handler に持たせる (#3823)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	files := testutil.NewMockDriveFileRepository()
	lites := &ctorLiteFiller{}
	h := NewHandler(nil, nil, packer, files, lites)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, files, h.fileRepo)
	assert.Same(t, lites, h.lites)
}

type ctorLiteFiller struct{}

func (*ctorLiteFiller) FillLites([]*model.User, []*entity.UserLite) {}
