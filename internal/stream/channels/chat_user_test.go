package channels

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatUserChannel_Init_SubscribesMyDirection(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	assert.Equal(t, []string{"chatUserStream:alice-bob"}, ctx.subs)
}

func TestChatUserChannel_Init_MissingOtherID(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	err := ch.Init(json.RawMessage(`{}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestChatUserChannel_Init_BadJSON(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	err := ch.Init(json.RawMessage(`not-json`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestChatUserChannel_Init_Unauthenticated(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(nil)
	ch := NewChatUserFactory(svc).New(ctx)
	err := ch.Init(json.RawMessage(`{"otherId":"bob"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestChatUserChannel_Init_SelfIsInvalid(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	err := ch.Init(json.RawMessage(`{"otherId":"alice"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestChatUserChannel_OnRedisEvent_Forwards(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	ch.OnRedisEvent([]byte(`{"type":"message","body":{"id":"m1"}}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "message", ctx.sentType[0])
}

func TestChatUserChannel_OnRedisEvent_Invalid(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	ch.OnRedisEvent([]byte(`not-json`))
	assert.Empty(t, ctx.sentType)
}

func TestChatUserChannel_OnRedisEvent_NoType(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	ch.OnRedisEvent([]byte(`{"body":{}}`))
	assert.Empty(t, ctx.sentType)
}

func TestChatUserChannel_OnClientMessage_Read(t *testing.T) {
	svc, repo := newChatSvc(t)
	text := "hi"
	toID := "alice"
	repo.Messages["m1"] = &model.ChatMessage{
		ID: "m1", FromUserID: "bob", ToUserID: &toID, Text: &text,
	}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	ch.OnClientMessage("read", json.RawMessage(`{"id":"m1"}`))
}

func TestChatUserChannel_OnClientMessage_UnknownType(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	ch.OnClientMessage("bogus", json.RawMessage(`{}`))
}

func TestChatUserChannel_OnClientMessage_ReadEmptyID(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	ch.OnClientMessage("read", json.RawMessage(`{"id":""}`))
}

func TestChatUserChannel_OnClientMessage_ReadBadJSON(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	ch.OnClientMessage("read", json.RawMessage(`not-json`))
}

func TestChatUserChannel_OnClientMessage_NoOther(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := &ChatUserChannel{ctx: ctx, svc: svc} // no Init → otherID empty
	ch.OnClientMessage("read", json.RawMessage(`{"id":"m1"}`))
}

func TestChatUserChannel_OnClientMessage_Unauthenticated(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(nil)
	ch := &ChatUserChannel{ctx: ctx, svc: svc, otherID: "bob"}
	ch.OnClientMessage("read", json.RawMessage(`{"id":"m1"}`))
}

func TestChatUserChannel_OnClientMessage_NilService(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := &ChatUserChannel{ctx: ctx, svc: nil, otherID: "bob"}
	ch.OnClientMessage("read", json.RawMessage(`{"id":"m1"}`))
}

func TestChatUserChannel_OnClientMessage_ReadServiceError(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := &ChatUserChannel{ctx: ctx, svc: svc, otherID: "bob"}
	ch.OnClientMessage("read", json.RawMessage(`{"id":"ghost"}`))
}

func TestChatUserChannel_Dispose(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatUserFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
	ch.Dispose()
	assert.Equal(t, []string{"chatUserStream:alice-bob"}, ctx.unsubs)
}

func TestChatUserChannel_Dispose_WithoutInit(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(nil)
	ch := NewChatUserFactory(svc).New(ctx)
	ch.Dispose()
	assert.Empty(t, ctx.unsubs)
}
