package channels

import (
	"context"
	"encoding/json"
	"log/slog"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// ReversiGameChannel is the per-game WebSocket channel that forwards game
// state updates (`started`, `log`, `ended`, ...) to connected clients and
// relays player actions (`ready`, `putStone`, ...) to the reversi service.
//
// 観戦モード対応: 認証済みだが player でないユーザーは OnClientMessage を
// 無視するだけで OnRedisEvent は受け取れる。匿名ユーザーも同様。
type ReversiGameChannel struct {
	ctx     stream.ChannelContext
	svc     *corereversi.Service
	gameID  string
	topic   string
	factory *ReversiGameFactory
}

// ReversiGameFactory holds the shared service so multiple channel instances
// can delegate into it. Used by router.go when registering the factory.
type ReversiGameFactory struct {
	svc *corereversi.Service
}

// NewReversiGameFactory constructs a ReversiGameFactory wired to the service.
func NewReversiGameFactory(svc *corereversi.Service) *ReversiGameFactory {
	return &ReversiGameFactory{svc: svc}
}

// New builds a new channel bound to the given context. Usable directly as a
// stream.ChannelFactory.
func (f *ReversiGameFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &ReversiGameChannel{ctx: ctx, svc: f.svc, factory: f}
}

// Init subscribes to `reversiGame:{gameId}` so the channel receives game
// events published by the service. Missing / malformed params silently
// result in a no-op channel.
func (c *ReversiGameChannel) Init(params json.RawMessage) error {
	var p struct {
		GameID string `json:"gameId"`
	}
	// TS本家のreversi-game.tsはgameId欠如時にreturn (init自体は成功) するためerrorは返さない
	if err := json.Unmarshal(params, &p); err != nil || p.GameID == "" {
		return nil
	}
	c.gameID = p.GameID
	c.topic = "reversiGame:" + p.GameID
	c.ctx.Subscribe(c.topic)
	return nil
}

// OnRedisEvent forwards a `reversiGame:{gameId}` pub/sub payload to the
// client. The payload must be a {type, body} envelope.
func (c *ReversiGameChannel) OnRedisEvent(payload []byte) {
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type == "" {
		return
	}
	_ = c.ctx.Send(env.Type, env.Body)
}

// OnClientMessage dispatches player actions to the reversi service. Requires
// authentication; anonymous clients and non-player spectators are ignored.
// 失敗は `error` イベントでクライアントへ通知する。
func (c *ReversiGameChannel) OnClientMessage(msgType string, body json.RawMessage) {
	if c.svc == nil || c.gameID == "" {
		return
	}
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return
	}
	ctx := context.Background()
	var err error
	switch msgType {
	case "ready":
		var ready bool
		if jerr := json.Unmarshal(body, &ready); jerr != nil {
			return
		}
		err = c.svc.UpdateReady(ctx, c.gameID, user.ID, ready)
	case "updateSettings":
		var req struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if jerr := json.Unmarshal(body, &req); jerr != nil || req.Key == "" {
			return
		}
		err = c.svc.UpdateSettings(ctx, c.gameID, user.ID, req.Key, req.Value)
	case "putStone":
		var req struct {
			Pos int    `json:"pos"`
			ID  string `json:"id"`
		}
		if jerr := json.Unmarshal(body, &req); jerr != nil {
			return
		}
		err = c.svc.PutStone(ctx, c.gameID, user.ID, req.Pos, req.ID)
	case "surrender":
		err = c.svc.Surrender(ctx, c.gameID, user.ID)
	case "cancel":
		err = c.svc.CancelGame(ctx, c.gameID, user.ID)
	case "claimTimeIsUp":
		err = c.svc.CheckTimeout(ctx, c.gameID)
	default:
		return
	}
	if err != nil {
		slog.Info("reversi channel: action failed",
			"gameId", c.gameID, "type", msgType, "user", user.ID, "err", err)
		// **内部のエラー文面を返さない。** repository のエラーはそのまま
		// 伝播する設計 (#2799) なので、pgx / gorm の SQLSTATE やリレーション名が
		// クライアントへ届いていた。CLAUDE.md §6 の「内部エラーは slog で記録、
		// ユーザーには汎用メッセージ」に反するうえ、同居する `peerError` /
		// `wrapPluginHandler` は既に「エラー文面を返さない」方針で書かれている。
		// upstream の `reversi-game.ts` はそもそも error を返す経路を持たない。
		_ = c.ctx.Send("error", map[string]any{"message": "operation failed", "type": msgType})
	}
}

// Dispose unsubscribes from the game topic.
func (c *ReversiGameChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
