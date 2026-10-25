package signup

import (
	"testing"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/stretchr/testify/assert"
)

type ctorPolicyProvider struct{}

func (*ctorPolicyProvider) GetUserPolicies(string) map[string]any { return nil }

// NewHandler は受け取った依存を handler に持たせる (#3823)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	group := bgwork.New()
	packer := entity.NewPacker(nil, entity.PackerDeps{})
	policies := &ctorPolicyProvider{}
	h := NewHandler(nil, nil, nil, group, packer, policies)
	assert.Same(t, group, h.background)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, policies, h.userPolicies)
}
