package clip

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// NewService は受け取った依存を service に持たせる (#3825)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	policies := &constructorPolicyProvider{}
	pub := &recordingClipPublisher{}
	svc := NewService(nil, nil, nil, nil, policies, pub)
	assert.Same(t, policies, svc.rolePolicyProvider)
	assert.Same(t, pub, svc.streamPublisher)
}

type constructorPolicyProvider struct{}

func (*constructorPolicyProvider) GetUserPolicies(string) map[string]any { return nil }
