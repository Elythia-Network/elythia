package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// DriveChannel forwards drive file life-cycle events scoped to the
// authenticated user. Anonymous connections subscribe to nothing.
type DriveChannel struct {
	ctx   stream.ChannelContext
	topic string
}

// NewDrive returns a channel factory for "drive".
func NewDrive(ctx stream.ChannelContext) stream.Channel {
	return &DriveChannel{ctx: ctx}
}

func (c *DriveChannel) Init(_ json.RawMessage) error {
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	c.topic = "drive:" + user.ID
	c.ctx.Subscribe(c.topic)
	return nil
}

// OnRedisEvent reads the embedded `type` (e.g. "fileCreated") and forwards
// the body. payload は {"type":"...","body":{...}} を想定する。
func (c *DriveChannel) OnRedisEvent(payload []byte) {
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err == nil && env.Type != "" {
		_ = c.ctx.Send(env.Type, env.Body)
		return
	}
	// envelope が無い payload はそのまま fileCreated 扱いで転送する
	_ = c.ctx.Send("fileCreated", json.RawMessage(payload))
}

func (c *DriveChannel) OnClientMessage(string, json.RawMessage) {}

// ShouldShare implements stream.ShareableChannel.
func (c *DriveChannel) ShouldShare() bool { return true }

// RequiredPermission implements stream.PermittedChannel.
func (c *DriveChannel) RequiredPermission() string { return "read:account" }

func (c *DriveChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
