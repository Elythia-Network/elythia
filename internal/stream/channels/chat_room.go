package channels

import (
	"context"
	"encoding/json"
	"log/slog"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// ChatRoomChannel forwards chat room events (message / edited / deleted /
// read) to any connected client that is a member of the target room.
// Init に失敗 (未参加、room 不明) した場合は topic を subscribe せず沈黙する
// — 本家 Misskey ChatRoomChannel と同じ挙動。
type ChatRoomChannel struct {
	ctx    stream.ChannelContext
	svc    *corechat.Service
	roomID string
	topic  string
}

// ChatRoomFactory holds the shared chat service so the registry can build
// channel instances per connection.
type ChatRoomFactory struct {
	svc *corechat.Service
}

// NewChatRoomFactory constructs a ChatRoomFactory.
func NewChatRoomFactory(svc *corechat.Service) *ChatRoomFactory {
	return &ChatRoomFactory{svc: svc}
}

// New builds a new channel bound to ctx, usable directly as stream.ChannelFactory.
func (f *ChatRoomFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &ChatRoomChannel{ctx: ctx, svc: f.svc}
}

// Init parses `roomId` from params, verifies the connected user is a member
// of the room, then subscribes to the shared Redis topic.
func (c *ChatRoomChannel) Init(params json.RawMessage) error {
	var p struct {
		RoomID string `json:"roomId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.RoomID == "" {
		return stream.ErrInvalidParams
	}
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	if c.svc != nil {
		// #2106 L57: room メンバーに加え moderator も購読を許可する
		// (upstream hasPermissionToViewRoomTimeline)。
		ok, err := c.svc.CanViewRoomTimeline(user.ID, p.RoomID)
		if err != nil || !ok {
			return stream.ErrInvalidParams
		}
	}
	c.roomID = p.RoomID
	c.topic = "chatRoomStream:" + p.RoomID
	c.ctx.Subscribe(c.topic)
	return nil
}

// OnRedisEvent decodes a {type, body} envelope and forwards the body to the
// client with the matching event type.
func (c *ChatRoomChannel) OnRedisEvent(payload []byte) {
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type == "" {
		return
	}
	_ = c.ctx.Send(env.Type, env.Body)
}

// OnClientMessage handles the single supported client-side action: "read".
// Clients send `{type: "read", body: {id: messageID}}` to mark a message as
// read, which propagates to the service layer. Other message types are
// silently ignored.
func (c *ChatRoomChannel) OnClientMessage(msgType string, body json.RawMessage) {
	if c.svc == nil || c.roomID == "" {
		return
	}
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return
	}
	switch msgType {
	case "read":
		// upstream chat-room.ts onMessage('read') は body を見ず部屋全体を既読化
		// する (#1549)。
		if err := c.svc.ReadRoomChat(context.Background(), user.ID, c.roomID); err != nil {
			slog.Info("chat room channel: read failed",
				"user", user.ID, "room", c.roomID, "err", err)
		}
	}
}

// RequiredPermission implements stream.PermittedChannel.
func (c *ChatRoomChannel) RequiredPermission() string { return "read:chat" }

// Dispose unsubscribes from the room topic.
func (c *ChatRoomChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}

// HasService reports whether the chat service was wired into the factory.
//
// 未配線だと `chatRoom` channel の購読でメンバーシップ検査 (moderator を含む
// `CanViewRoomTimeline`) が丸ごと skip され、**誰でも任意の room の timeline を
// 購読できる**。起動時検査に使う (#2683)。
func (f *ChatRoomFactory) HasService() bool { return f.svc != nil }
