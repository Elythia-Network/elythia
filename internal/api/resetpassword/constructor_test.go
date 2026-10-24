package resetpassword

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/testutil"
)

// NewHandler は受け取った依存を handler に持たせる (#3823)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	group := bgwork.New()
	meta := testutil.NewMockMetaRepository()
	h := NewHandler(nil, nil, nil, group, meta)
	assert.Same(t, group, h.background)
	assert.Same(t, meta, h.metaRepo)
}
