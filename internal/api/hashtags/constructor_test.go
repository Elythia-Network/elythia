package hashtags

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// NewHandler は受け取った依存を handler に持たせる (#3817)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(nil, entity.PackerDeps{})
	extras := &ctorExtras{}
	moderators := &ctorModerators{}
	meta := testutil.NewMockMetaRepository()
	h := NewHandler(nil, packer, extras, moderators, meta)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, extras, h.extras)
	assert.Same(t, moderators, h.moderators)
	assert.Same(t, meta, h.meta)
}

type ctorExtras struct{}

func (*ctorExtras) FillDetailedExtrasMany(context.Context, *model.User, []userpack.DetailTarget) {}

type ctorModerators struct{}

func (*ctorModerators) IsModerator(string) bool { return false }
