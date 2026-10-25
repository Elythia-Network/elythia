package drive

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/testutil"
)

// NewService は受け取った依存を service に持たせる (#3825)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	roles := &constructorRoleChecker{}
	logs := &constructorModLogger{}
	users := testutil.NewMockUserRepository()
	svc := NewService(nil, nil, nil, nil, packer, roles, logs, users)
	assert.Same(t, packer, svc.entityPacker)
	assert.Same(t, roles, svc.roleChecker)
	assert.Same(t, logs, svc.modLog)
	assert.Same(t, users, svc.modLogUsers)
}

type constructorRoleChecker struct{}

func (*constructorRoleChecker) IsModerator(string) bool               { return false }
func (*constructorRoleChecker) GetUserPolicies(string) map[string]any { return nil }

type constructorModLogger struct{}

func (*constructorModLogger) Log(context.Context, string, moderationlog.LogType, map[string]any) {}
