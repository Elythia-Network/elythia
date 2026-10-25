package note

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/testutil"
)

// NewCreateService は受け取った依存を service に持たせる (#3825)。依存を構築子の
// 必須の引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewCreateServiceKeepsDependencies(t *testing.T) {
	g := bgwork.New()
	packer := entity.NewPacker(entity.PackerDeps{})
	blocking := testutil.NewMockBlockingRepository()
	meta := testutil.NewMockMetaRepository()
	silencing := &constructorSilencing{}
	files := testutil.NewMockDriveFileRepository()
	svc := NewCreateService(nil, nil, nil, nil, g, packer, blocking, meta, silencing, files)
	assert.Same(t, g, svc.background)
	assert.Same(t, packer, svc.entityPacker)
	assert.Same(t, blocking, svc.blockingRepo)
	assert.Same(t, meta, svc.metaRepo)
	assert.Same(t, silencing, svc.silencingProvider)
	assert.Same(t, files, svc.driveFileRepo)
}

type constructorSilencing struct{}

func (*constructorSilencing) IsSilenced(string) bool { return false }
