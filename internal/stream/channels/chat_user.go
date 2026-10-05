package channels

import (
	"context"
	"encoding/json"
	"log/slog"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// ChatUserChannel forwards direct-message events for a specific conversation
// pair (me ↔ other). Param: `otherId`.
// 本家 Misskey ChatUserChannel 相当。subscribe するトピックは
// `chatUserStream:{myId}-{otherId}` (自分のビュー)。
type ChatUserChannel struct {
	ctx     stream.ChannelContext
	svc     *corechat.Service
	otherID string
	topic   string
}

// ChatUserFactory holds the shared service for per-connection factory calls.
type ChatUserFactory struct {
	svc *corechat.Service
}

// NewChatUserFactory constructs a ChatUserFactory.
func NewChatUserFactory(svc *corechat.Service) *ChatUserFactory {
	return &ChatUserFactory{svc: svc}
}

// New builds a new channel bound to ctx, usable directly as stream.ChannelFactory.
func (f *ChatUserFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &ChatUserChannel{ctx: ctx, svc: f.svc}
}

// Init parses `otherId` and subscribes to the user's own conversation view.
func (c *ChatUserChannel) Init(params json.RawMessage) error {
	var p struct {
		OtherID string `json:"otherId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.OtherID == "" {
		return stream.ErrInvalidParams
	}
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	// 自分と同じ id を other に指定するのは無効。
	if p.OtherID == user.ID {
		return stream.ErrInvalidParams
	}
	c.otherID = p.OtherID
	c.topic = "chatUserStream:" + user.ID + "-" + p.OtherID
	c.ctx.Subscribe(c.topic)
	return nil
}

// OnRedisEvent decodes a {type, body} envelope and forwards to the client.
func (c *ChatUserChannel) OnRedisEvent(payload []byte) {
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type == "" {
		return
	}
	_ = c.ctx.Send(env.Type, env.Body)
}

// OnClientMessage handles "read" to mark a DM as read. Other types ignored.
func (c *ChatUserChannel) OnClientMessage(msgType string, body json.RawMessage) {
	if c.svc == nil || c.otherID == "" {
		return
	}
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return
	}
	switch msgType {
	case "read":
		// upstream chat-user.ts onMessage('read') は body を見ず会話全体を既読化
		// する (#1549)。message id 必須だった旧実装は id 無し read を no-op にして
		// しまい unread が消えなかった。
		if err := c.svc.ReadUserChat(context.Background(), user.ID, c.otherID); err != nil {
			slog.Info("chat user channel: read failed",
				"user", user.ID, "other", c.otherID, "err", err)
		}
	}
}

// RequiredPermission implements stream.PermittedChannel.
func (c *ChatUserChannel) RequiredPermission() string { return "read:chat" }

// Dispose unsubscribes from the user's conversation topic.
func (c *ChatUserChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
