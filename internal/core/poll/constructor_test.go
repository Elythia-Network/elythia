package poll

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// NewService は受け取った依存を service に持たせる (#3825)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	blocking := &constructorBlocking{}
	svc := NewService(nil, nil, nil, nil, nil, blocking)
	assert.Same(t, blocking, svc.blockingChecker)
}

type constructorBlocking struct{}

func (*constructorBlocking) IsBlocked(string, string) (bool, error) { return false, nil }
