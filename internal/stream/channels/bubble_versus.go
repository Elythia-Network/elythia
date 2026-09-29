package channels

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/shiroha-a/mk/internal/core/bubbleversus"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/stream"
)

// bubbleVersusMaxState bounds the board summary a player relays to the
// opponent. 中身は表示用でサーバーは解釈しないので、大きさだけ抑える。
const bubbleVersusMaxState = 8 * 1024

// unwrapEnvelope forwards a {type, body} pub/sub payload to the client.
func unwrapEnvelope(ctx stream.ChannelContext, payload []byte) {
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type == "" {
		return
	}
	_ = ctx.Send(env.Type, env.Body)
}

// BubbleVersusChannel delivers versus invitations and their answers to the
// signed-in user (#3230)。対局そのものは BubbleVersusMatchChannel が受け持つ。
type BubbleVersusChannel struct {
	ctx   stream.ChannelContext
	topic string
}

// NewBubbleVersus returns a channel for "bubbleVersus".
func NewBubbleVersus(ctx stream.ChannelContext) stream.Channel {
	return &BubbleVersusChannel{ctx: ctx}
}

// Init subscribes to the user's topic.
func (c *BubbleVersusChannel) Init(_ json.RawMessage) error {
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	c.topic = stream.BubbleVersusUserTopic(user.ID)
	c.ctx.Subscribe(c.topic)
	return nil
}

// OnRedisEvent forwards an event to the client.
func (c *BubbleVersusChannel) OnRedisEvent(payload []byte) { unwrapEnvelope(c.ctx, payload) }

// OnClientMessage ignores client messages (招待への返事は API で行う)。
func (c *BubbleVersusChannel) OnClientMessage(string, json.RawMessage) {}

// ShouldShare implements stream.ShareableChannel.
func (c *BubbleVersusChannel) ShouldShare() bool { return true }

// RequiredPermission implements stream.PermittedChannel.
func (c *BubbleVersusChannel) RequiredPermission() string { return "read:account" }

// Dispose unsubscribes from the user's topic.
func (c *BubbleVersusChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}

// BubbleVersusMatchChannel is the channel of one versus match. 参加者だけが
// つなげる (観戦は無い)。準備・攻撃・盤面の知らせ・切断の申告をここで受ける。
// 終局の報告は記録が大きいので API (`bubble-game/versus/report`) で送る。
type BubbleVersusMatchChannel struct {
	ctx     stream.ChannelContext
	svc     *bubbleversus.Service
	userID  string
	matchID string
	topic   string
}

// BubbleVersusMatchFactory builds BubbleVersusMatchChannel instances.
type BubbleVersusMatchFactory struct {
	svc *bubbleversus.Service
}

// NewBubbleVersusMatchFactory constructs a factory wired to the service.
func NewBubbleVersusMatchFactory(svc *bubbleversus.Service) *BubbleVersusMatchFactory {
	return &BubbleVersusMatchFactory{svc: svc}
}

// New builds a channel. Usable as a stream.ChannelFactory.
func (f *BubbleVersusMatchFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &BubbleVersusMatchChannel{ctx: ctx, svc: f.svc}
}

// Init subscribes to the match when the user is one of its players.
func (c *BubbleVersusMatchChannel) Init(params json.RawMessage) error {
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil || c.svc == nil {
		return stream.ErrInvalidParams
	}
	var p struct {
		MatchID string `json:"matchId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.MatchID == "" {
		return stream.ErrInvalidParams
	}
	m, err := c.svc.Get(context.Background(), p.MatchID)
	if err != nil {
		if !errors.Is(err, bubbleversus.ErrNoSuchMatch) {
			slog.Warn("bubble versus channel: lookup failed", "matchId", p.MatchID, "err", err)
		}
		return stream.ErrInvalidParams
	}
	// 相手の盤面と攻撃が流れるので、参加者以外には見せない。
	if m.Side(user.ID) < 0 {
		return stream.ErrInvalidParams
	}
	c.userID = user.ID
	c.matchID = m.ID
	c.topic = stream.BubbleVersusMatchTopic(m.ID)
	c.ctx.Subscribe(c.topic)
	return nil
}

// OnRedisEvent forwards a match event to the client.
func (c *BubbleVersusMatchChannel) OnRedisEvent(payload []byte) { unwrapEnvelope(c.ctx, payload) }

// OnClientMessage dispatches a player's action to the service. 失敗は `error`
// で知らせる (内部のエラー文面は返さない。reversiGame と同じ)。
func (c *BubbleVersusMatchChannel) OnClientMessage(msgType string, body json.RawMessage) {
	if c.matchID == "" {
		return
	}
	ctx := context.Background()
	var err error
	switch msgType {
	case "ready":
		var ready bool
		if json.Unmarshal(body, &ready) != nil {
			return
		}
		_, err = c.svc.Ready(ctx, c.userID, c.matchID, ready)
	case "attack":
		var req struct {
			Count int64 `json:"count"`
		}
		if json.Unmarshal(body, &req) != nil {
			return
		}
		err = c.svc.Attack(ctx, c.userID, c.matchID, req.Count)
	case "state":
		if len(body) == 0 || len(body) > bubbleVersusMaxState || !json.Valid(body) {
			return
		}
		c.svc.State(ctx, c.userID, c.matchID, body)
	case "claimDisconnected":
		_, err = c.svc.ClaimDisconnected(ctx, c.userID, c.matchID)
	default:
		return
	}
	if err != nil {
		slog.Info("bubble versus channel: action failed", "matchId", c.matchID, "type", msgType, "user", c.userID, "err", err)
		_ = c.ctx.Send("error", map[string]any{"type": msgType, "code": bubbleVersusErrorCode(err)})
	}
}

// bubbleVersusErrorCode maps a service error to a code the client can act on.
// 既知でないものは内部の文面を出さずに一括りにする。
func bubbleVersusErrorCode(err error) string {
	switch {
	case errors.Is(err, bubbleversus.ErrNotYet):
		return "NOT_YET"
	case errors.Is(err, bubbleversus.ErrInvalidState):
		return "INVALID_STATE"
	case errors.Is(err, bubbleversus.ErrInvalidReport):
		return "INVALID_PARAM"
	case errors.Is(err, bubbleversus.ErrNoSuchMatch):
		return "NO_SUCH_MATCH"
	default:
		return "INTERNAL_ERROR"
	}
}

// RequiredPermission implements stream.PermittedChannel.
func (c *BubbleVersusMatchChannel) RequiredPermission() string { return "read:account" }

// Dispose unsubscribes from the match.
func (c *BubbleVersusMatchChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
