package admin

import (
	"testing"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
)

// 構築子の引数に渡すだけの値。どれも呼ばれないので、interface を埋め込んで型を満たす。
type (
	ctorDetailExtras     struct{ userpack.DetailExtras }
	ctorListPacker       struct{ userpack.ListPacker }
	ctorSuspensionOrigin struct {
		repository.UserSuspensionOriginRepository
	}
	ctorTokenInvalidator struct{ UserTokenInvalidator }
	ctorStreamRevoker    struct{ UserStreamRevoker }
)

// NewHandler は受け取った依存を handler に持たせる (#3823)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	group := bgwork.New()
	packer := entity.NewPacker(entity.PackerDeps{})
	extras := &ctorDetailExtras{}
	lists := &ctorListPacker{}
	origins := &ctorSuspensionOrigin{}
	tokens := &ctorTokenInvalidator{}
	streams := &ctorStreamRevoker{}
	audit := iplookuplog.NewService(nil, nil)
	fetcher := NewEmojiImageFetcher(nil, nil, "")
	h := NewHandler(nil, nil, nil, nil, nil,
		group, packer, extras, lists, origins, tokens, streams, audit, fetcher)
	assert.Same(t, group, h.background)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, extras, h.detailExtras)
	assert.Same(t, lists, h.listPacker)
	assert.Same(t, origins, h.suspensionOriginRepo)
	assert.Same(t, tokens, h.userTokenInvalidator)
	assert.Same(t, streams, h.userStreamRevoker)
	assert.Same(t, audit, h.ipLookupAudit)
	assert.Same(t, fetcher, h.emojiImageFetcher)
}
