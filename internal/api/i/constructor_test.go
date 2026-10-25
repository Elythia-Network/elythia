package i

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/testutil"
)

// NewHandler は受け取った依存を handler に持たせる (#3827)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewHandlerKeepsDependencies(t *testing.T) {
	hider := notehide.NewHider(nil, func() string { return "local" })
	packer := entity.NewPacker(entity.PackerDeps{})
	roles := &stubRoleProvider{}
	replay := &countingReplayGuard{used: map[string]bool{}}
	pending := &ctorPendingGuard{}
	inv := &stubTokenInvalidator{}
	tokens := testutil.NewMockAccessTokenRepository()
	revoker := &ctorStreamRevoker{}
	accepter := &recordingBulkAccepter{}
	bg := bgwork.New()
	t.Cleanup(func() { _ = bg.Stop(context.Background()) })

	h := NewHandler(nil, nil, hider, packer, roles, replay, pending, inv, tokens, revoker, accepter, bg)
	assert.Same(t, hider, h.noteHider)
	assert.Same(t, packer, h.entityPacker)
	assert.Same(t, roles, h.roleProvider)
	assert.Same(t, replay, h.totpReplayGuard)
	assert.Same(t, pending, h.twoFAPending)
	assert.Same(t, inv, h.authInvalidator)
	assert.Same(t, tokens, h.accessTokenRepo)
	assert.Same(t, revoker, h.streamRevoker)
	assert.Same(t, accepter, h.followRequestAccepter)
	assert.Same(t, bg, h.background)
}

type ctorPendingGuard struct{}

func (*ctorPendingGuard) Start(context.Context, string, string) error   { return nil }
func (*ctorPendingGuard) Attempt(context.Context, string, string) error { return nil }
func (*ctorPendingGuard) Finish(context.Context, string) error          { return nil }

type ctorStreamRevoker struct{}

func (*ctorStreamRevoker) RevokeUserStreams(string)                {}
func (*ctorStreamRevoker) RevokeNativeTokenStreams(string, string) {}
func (*ctorStreamRevoker) RevokeAccessTokenStreams(string, string) {}
