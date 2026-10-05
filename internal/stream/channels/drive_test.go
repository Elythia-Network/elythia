package channels

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ stream.Channel = (*DriveChannel)(nil)

func TestDrive_AuthenticatedSubscribesPerUser(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewDrive(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"drive:alice"}, ctx.subs)

	// envelope あり
	ch.OnRedisEvent([]byte(`{"type":"fileCreated","body":{"id":"f1"}}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "fileCreated", ctx.sentType[0])

	// envelope なし → fileCreated にフォールバック
	ch.OnRedisEvent([]byte(`{"id":"f2"}`))
	require.Len(t, ctx.sentType, 2)
	assert.Equal(t, "fileCreated", ctx.sentType[1])

	ch.OnClientMessage("ignored", json.RawMessage(`{}`))
	ch.Dispose()
	assert.Equal(t, []string{"drive:alice"}, ctx.unsubs)
}

func TestDrive_AnonymousIsNoOp(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewDrive(ctx)
	err := ch.Init(nil)
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
	ch.Dispose()
	assert.Empty(t, ctx.unsubs)
}

func TestDrive_NilUserPointer(t *testing.T) {
	ctx := newCtx((*model.User)(nil))
	ch := NewDrive(ctx)
	err := ch.Init(nil)
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}
