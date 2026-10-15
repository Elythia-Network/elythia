package channels

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// Client messages that change state require the kind of the matching REST
// endpoint, not just the kind that let the token subscribe to the channel.

func TestChatUserChannel_ReadRequiresWriteChat(t *testing.T) {
	for _, tt := range []struct {
		name  string
		perms []string
		want  []string
	}{
		{"read:chat only does not mark read", []string{"read:chat"}, nil},
		{"write:chat marks read", []string{"read:chat", "write:chat"}, []string{"user:alice:bob"}},
		{"native token marks read", nil, []string{"user:alice:bob"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo := newChatSvc(t)
			ctx := newCtx(&model.User{ID: "alice"})
			ctx.permissions = tt.perms
			ch := NewChatUserFactory(svc).New(ctx)
			require.NoError(t, ch.Init(json.RawMessage(`{"otherId":"bob"}`)))
			ch.OnClientMessage("read", nil)
			assert.Equal(t, tt.want, repo.ReadAllCalls)
		})
	}
}

func TestChatRoomChannel_ReadRequiresWriteChat(t *testing.T) {
	for _, tt := range []struct {
		name  string
		perms []string
		want  []string
	}{
		{"read:chat only does not mark read", []string{"read:chat"}, nil},
		{"write:chat marks read", []string{"read:chat", "write:chat"}, []string{"room:alice:r1"}},
		{"native token marks read", nil, []string{"room:alice:r1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo := newChatSvc(t)
			repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
			ctx := newCtx(&model.User{ID: "alice"})
			ctx.permissions = tt.perms
			ch := NewChatRoomFactory(svc).New(ctx)
			require.NoError(t, ch.Init(json.RawMessage(`{"roomId":"r1"}`)))
			ch.OnClientMessage("read", nil)
			assert.Equal(t, tt.want, repo.ReadAllCalls)
		})
	}
}

func TestReversiGameChannel_ActionsRequireWriteAccount(t *testing.T) {
	for _, tt := range []struct {
		name      string
		perms     []string
		wantReady bool
	}{
		{"read:account only cannot act", []string{"read:account"}, false},
		{"write:account can act", []string{"read:account", "write:account"}, true},
		{"native token can act", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ch, ctx, repo, game := newChannel(t, &model.User{ID: "alice"})
			ctx.permissions = tt.perms
			require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
			ch.OnClientMessage("ready", json.RawMessage(`true`))
			got, err := repo.FindByID(game.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantReady, got.User1Ready)
		})
	}
}

func TestBubbleVersusMatchChannel_ActionsRequireWriteAccount(t *testing.T) {
	for _, tt := range []struct {
		name      string
		perms     []string
		wantReady bool
	}{
		{"read:account only cannot act", []string{"read:account"}, false},
		{"write:account can act", []string{"read:account", "write:account"}, true},
		{"native token can act", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, m, _ := newVersusMatch(t)
			params, _ := json.Marshal(map[string]string{"matchId": m.ID})
			actx := newCtx(&model.User{ID: "alice"})
			actx.permissions = tt.perms
			a := NewBubbleVersusMatchFactory(svc).New(actx)
			require.NoError(t, a.Init(params))
			a.OnClientMessage("ready", json.RawMessage(`true`))
			got, err := svc.Get(context.Background(), m.ID)
			require.NoError(t, err)
			side := got.Side("alice")
			require.GreaterOrEqual(t, side, 0)
			assert.Equal(t, tt.wantReady, got.Players[side].Ready)
		})
	}
}
