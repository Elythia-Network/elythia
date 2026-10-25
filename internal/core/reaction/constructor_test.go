package reaction

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"
)

// NewService は受け取った依存を service に持たせる (#3825)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	roles := &constructorUserRoles{}
	silence := &constructorMediaSilence{}
	blocking := &constructorBlocking{}
	svc := NewService(nil, nil, nil, nil, nil, roles, silence, blocking)
	assert.Same(t, roles, svc.userRoles)
	assert.Same(t, silence, svc.mediaSilence)
	assert.Same(t, blocking, svc.blockingChecker)
}

type constructorUserRoles struct{}

func (*constructorUserRoles) GetUserRoles(string) ([]*model.Role, error) { return nil, nil }

type constructorMediaSilence struct{}

func (*constructorMediaSilence) IsMediaSilenced(string) bool { return false }

type constructorBlocking struct{}

func (*constructorBlocking) IsBlocked(string, string) (bool, error) { return false, nil }
