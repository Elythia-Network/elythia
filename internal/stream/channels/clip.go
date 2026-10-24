package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// ClipLookup resolves a clip by id for the clip channel's visibility gate.
// repository.ClipRepository satisfies it (FindByID).
type ClipLookup interface {
	FindByID(id string) (*model.Clip, error)
}

// ClipFactory carries the clip lookup the clip channel checks at Init.
type ClipFactory struct {
	clips ClipLookup
}

// NewClipFactory constructs a ClipFactory. A nil lookup fails closed (every
// subscription rejected).
func NewClipFactory(clips ClipLookup) *ClipFactory {
	return &ClipFactory{clips: clips}
}

// New builds a new ClipChannel. Usable as a stream.ChannelFactory.
func (f *ClipFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &ClipChannel{ctx: ctx, clips: f.clips}
}

// ClipChannel tells the client that a clip changed (`updated`) or was deleted
// (`deleted`), mirroring upstream 2026.10.1 channels/clip.ts (#3809). Both
// events carry a null body; the client reloads the clip itself.
//
// 本家と同じく未ログインでも公開クリップは購読でき、非公開のクリップは持ち主だけが
// 購読できる。判定は購読の時点だけで、後から非公開にしても購読は切らない (本家と同じ)。
// 流れるのは本文を持たない合図だけなので、漏れるのは「変わった」という事実に限られる。
type ClipChannel struct {
	ctx   stream.ChannelContext
	clips ClipLookup
	topic string
}

// Init subscribes to `clipStream:{clipId}` when the clip exists and the viewer
// may see it.
func (c *ClipChannel) Init(params json.RawMessage) error {
	var p struct {
		ClipID *string `json:"clipId"`
	}
	// 本家は clipId が文字列でなければ断る。型が違えば Unmarshal が失敗する。
	if len(params) == 0 || json.Unmarshal(params, &p) != nil || p.ClipID == nil {
		return stream.ErrInvalidParams
	}
	if c.clips == nil {
		return stream.ErrInvalidParams
	}
	clip, err := c.clips.FindByID(*p.ClipID)
	if err != nil || clip == nil {
		return stream.ErrInvalidParams
	}
	if !clip.IsPublic {
		user, ok := c.ctx.User().(*model.User)
		if !ok || user == nil || user.ID != clip.UserID {
			return stream.ErrInvalidParams
		}
	}
	c.topic = "clipStream:" + clip.ID
	c.ctx.Subscribe(c.topic)
	return nil
}

// OnRedisEvent forwards `updated` / `deleted` with a null body.
func (c *ClipChannel) OnRedisEvent(payload []byte) {
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &env) != nil {
		return
	}
	// 本家と同じく、内部の body は client へ流さず null にする。
	if env.Type == "updated" || env.Type == "deleted" {
		_ = c.ctx.Send(env.Type, nil)
	}
}

// OnClientMessage ignores client messages; the channel only receives.
func (c *ClipChannel) OnClientMessage(string, json.RawMessage) {}

// Dispose unsubscribes from the clip topic.
func (c *ClipChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
