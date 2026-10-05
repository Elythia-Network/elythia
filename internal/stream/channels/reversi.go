package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// ReversiChannel forwards global reversi game events (match invitations, etc.).
// reversiGameチャンネル（特定ゲームのイベント）とは異なり、ゲーム発見用。
type ReversiChannel struct {
	ctx       stream.ChannelContext
	connected bool
}

// NewReversi returns a channel factory for "reversi".
func NewReversi(ctx stream.ChannelContext) stream.Channel {
	return &ReversiChannel{ctx: ctx}
}

func (c *ReversiChannel) Init(_ json.RawMessage) error {
	// 認証必須
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	c.connected = true
	c.ctx.Subscribe("reversi:" + user.ID)
	return nil
}

func (c *ReversiChannel) OnRedisEvent(payload []byte) {
	// {type, body}エンベロープの展開
	var envelope struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Type != "" {
		_ = c.ctx.Send(envelope.Type, envelope.Body)
		return
	}
	_ = c.ctx.Send("reversiEvent", json.RawMessage(payload))
}

func (c *ReversiChannel) OnClientMessage(string, json.RawMessage) {}

// ShouldShare implements stream.ShareableChannel.
func (c *ReversiChannel) ShouldShare() bool { return true }

// RequiredPermission implements stream.PermittedChannel.
func (c *ReversiChannel) RequiredPermission() string { return "read:account" }

func (c *ReversiChannel) Dispose() {
	if c.connected {
		user, ok := c.ctx.User().(*model.User)
		if ok && user != nil {
			c.ctx.Unsubscribe("reversi:" + user.ID)
		}
	}
}
