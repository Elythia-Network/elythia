package channels

import (
	"encoding/json"
	"testing"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chat 用 fake repo は testutil.MockChatRepository に集約された (#709)。

func newChatSvc(t *testing.T) (*corechat.Service, *testutil.MockChatRepository) {
	t.Helper()
	repo := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	return corechat.NewService(repo, idGen), repo
}

// --- conformance ---

var _ stream.Channel = (*ChatRoomChannel)(nil)
var _ stream.Channel = (*ChatUserChannel)(nil)

// --- ChatRoom channel tests ---

func TestChatRoomChannel_Init_Member(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	repo.Memberships["bob:r1"] = &model.ChatRoomMembership{UserID: "bob", RoomID: "r1"}

	ctx := newCtx(&model.User{ID: "bob"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))

	assert.Equal(t, []string{"chatRoomStream:r1"}, ctx.subs)
}

func TestChatRoomChannel_Init_OwnerImplicitMember(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}

	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))

	assert.Equal(t, []string{"chatRoomStream:r1"}, ctx.subs)
}

func TestChatRoomChannel_Init_NotMember(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}

	ctx := newCtx(&model.User{ID: "carol"})
	ch := NewChatRoomFactory(svc).New(ctx)
	err := ch.Init(json.RawMessage(`{"roomId":"r1"}`))

	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs, "non-member should not subscribe")
}

func TestChatRoomChannel_Init_Unauthenticated(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(nil)
	ch := NewChatRoomFactory(svc).New(ctx)
	err := ch.Init(json.RawMessage(`{"roomId":"r1"}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestChatRoomChannel_Init_MissingRoomID(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	err := ch.Init(json.RawMessage(`{}`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestChatRoomChannel_Init_BadJSON(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	err := ch.Init(json.RawMessage(`not-json`))
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestChatRoomChannel_OnRedisEvent_Forwards(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
	ch.OnRedisEvent([]byte(`{"type":"message","body":{"id":"m1"}}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "message", ctx.sentType[0])
}

func TestChatRoomChannel_OnRedisEvent_Invalid(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
	ch.OnRedisEvent([]byte(`not-json`))
	assert.Empty(t, ctx.sentType)
}

func TestChatRoomChannel_OnRedisEvent_NoType(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
	ch.OnRedisEvent([]byte(`{"body":{}}`))
	assert.Empty(t, ctx.sentType)
}

func TestChatRoomChannel_OnClientMessage_Read(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	textStr := "hi"
	repo.Messages["m1"] = &model.ChatMessage{
		ID: "m1", FromUserID: "alice", ToRoomID: strPtr("r1"), Text: &textStr,
	}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
	ch.OnClientMessage("read", json.RawMessage(`{"id":"m1"}`))
	// should not panic; we don't verify the side effect here (covered in service tests)
}

func TestChatRoomChannel_OnClientMessage_UnknownType(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
	ch.OnClientMessage("bogus", json.RawMessage(`{}`))
	// silently ignored
}

func TestChatRoomChannel_OnClientMessage_ReadEmptyID(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
	ch.OnClientMessage("read", json.RawMessage(`{"id":""}`))
}

func TestChatRoomChannel_OnClientMessage_ReadBadJSON(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
	ch.OnClientMessage("read", json.RawMessage(`not-json`))
}

func TestChatRoomChannel_OnClientMessage_Unauthenticated(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(nil)
	ch := &ChatRoomChannel{ctx: ctx, svc: svc, roomID: "r1"}
	ch.OnClientMessage("read", json.RawMessage(`{"id":"m1"}`))
}

func TestChatRoomChannel_OnClientMessage_NilService(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := &ChatRoomChannel{ctx: ctx, svc: nil, roomID: "r1"}
	ch.OnClientMessage("read", json.RawMessage(`{"id":"m1"}`))
}

func TestChatRoomChannel_OnClientMessage_ReadServiceError(t *testing.T) {
	svc, _ := newChatSvc(t)
	// message doesn't exist in repo → MarkReadByMessageID returns ErrNotFound
	ctx := newCtx(&model.User{ID: "alice"})
	ch := &ChatRoomChannel{ctx: ctx, svc: svc, roomID: "r1"}
	ch.OnClientMessage("read", json.RawMessage(`{"id":"ghost"}`))
}

func TestChatRoomChannel_Dispose(t *testing.T) {
	svc, repo := newChatSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewChatRoomFactory(svc).New(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
	ch.Dispose()
	assert.Equal(t, []string{"chatRoomStream:r1"}, ctx.unsubs)
}

func TestChatRoomChannel_Dispose_WithoutInit(t *testing.T) {
	svc, _ := newChatSvc(t)
	ctx := newCtx(nil)
	ch := NewChatRoomFactory(svc).New(ctx)
	ch.Dispose() // no panic
	assert.Empty(t, ctx.unsubs)
}

// helper
func strPtr(s string) *string { return &s }
