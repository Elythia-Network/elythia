package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/stream"
)

// StatsLogProvider returns recent stats snapshots for `requestLog` responses.
// ServerStatsChannel / QueueStatsChannel が共通で使う narrow interface
// (stream.ServerStatsPublisher / QueueStatsPublisher が満たす)。
// 循環依存を避けるため interface で受け取る。
type StatsLogProvider interface {
	Log(maxLen int) []json.RawMessage
}

// ServerStatsChannel broadcasts server statistics.
type ServerStatsChannel struct {
	ctx       stream.ChannelContext
	logger    StatsLogProvider // optional: nil なら requestLog で空配列を返す
	connected bool
}

// NewServerStats returns a channel factory for "serverStats" without a stats
// log provider. requestLog 応答は常に空配列。テスト / 移行用 fallback。
func NewServerStats(ctx stream.ChannelContext) stream.Channel {
	return &ServerStatsChannel{ctx: ctx}
}

// NewServerStatsFactory returns a ChannelFactory that captures a StatsLogProvider
// in a closure so the resulting channel can serve requestLog from real
// historical data (#571 item 2)。
func NewServerStatsFactory(logger StatsLogProvider) stream.ChannelFactory {
	return func(ctx stream.ChannelContext) stream.Channel {
		return &ServerStatsChannel{ctx: ctx, logger: logger}
	}
}

func (c *ServerStatsChannel) Init(_ json.RawMessage) error {
	c.connected = true
	c.ctx.Subscribe("serverStats")
	return nil
}

func (c *ServerStatsChannel) OnRedisEvent(payload []byte) {
	_ = c.ctx.Send("stats", json.RawMessage(payload))
}

// OnClientMessage handles requestLog from the client. body は TS 互換の
// `{ id?: string, length?: int }` 形式。length が指定されていれば最大それまで、
// 未指定なら publisher 側 default (200) で返す。logger が wire されていない
// 場合は空配列を返す (移行期 / テスト用 fallback)。
func (c *ServerStatsChannel) OnClientMessage(msgType string, body json.RawMessage) {
	if msgType != "requestLog" {
		return
	}
	if c.logger == nil {
		_ = c.ctx.Send("statsLog", []any{})
		return
	}
	var req struct {
		Length int `json:"length"`
	}
	_ = json.Unmarshal(body, &req) // 失敗時は length=0 で publisher デフォルト
	_ = c.ctx.Send("statsLog", c.logger.Log(req.Length))
}

// ShouldShare implements stream.ShareableChannel.
func (c *ServerStatsChannel) ShouldShare() bool { return true }

func (c *ServerStatsChannel) Dispose() {
	if c.connected {
		c.ctx.Unsubscribe("serverStats")
	}
}
