package following

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// NewService は受け取った依存を service に持たせる (#3825)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	silenced := &constructorSilenced{}
	silent := &constructorSilentFollowRepo{}
	users := &constructorUserPacker{}
	svc := NewService(nil, nil, nil, nil, packer, silenced, silent, users)
	assert.Same(t, packer, svc.entityPacker)
	assert.Same(t, silenced, svc.silencedChecker)
	assert.Same(t, silent, svc.silentFollowRepo)
	assert.Same(t, users, svc.userPacker)
}

type constructorSilenced struct{}

func (*constructorSilenced) IsSilenced(string) bool { return false }

type constructorSilentFollowRepo struct {
	repository.SilentFollowRepository
}

type constructorUserPacker struct{}

func (*constructorUserPacker) Lite(*model.User) entity.UserLite { return entity.UserLite{} }
func (*constructorUserPacker) DetailedNotMe(context.Context, *model.User, *model.User) (entity.UserDetailed, bool) {
	return entity.UserDetailed{}, false
}
