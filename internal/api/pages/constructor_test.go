package pages

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(nil, entity.PackerDeps{})
	files := testutil.NewMockDriveFileRepository()
	users := &recordingPacker{}
	h := NewHandler(nil, nil, packer, files, users)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, files, h.driveFileRepo)
	assert.Same(t, users, h.userPacker)
}
