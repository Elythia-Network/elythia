package antenna

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// NewService は受け取った依存を service に持たせる (#3825)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	policies := &constructorPolicyProvider{}
	svc := NewService(nil, nil, nil, nil, policies)
	assert.Same(t, policies, svc.rolePolicyProvider)
}

type constructorPolicyProvider struct{}

func (*constructorPolicyProvider) GetUserPolicies(string) map[string]any { return nil }
