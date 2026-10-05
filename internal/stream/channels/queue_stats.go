package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/stream"
)

// QueueStatsChannel broadcasts job queue statistics.
type QueueStatsChannel struct {
	ctx       stream.ChannelContext
	logger    StatsLogProvider // optional: nil なら requestLog で空配列を返す
	connected bool
}

// NewQueueStats returns a channel factory for "queueStats" without a stats
// log provider. requestLog 応答は常に空配列。テスト / 移行用 fallback。
func NewQueueStats(ctx stream.ChannelContext) stream.Channel {
	return &QueueStatsChannel{ctx: ctx}
}

// NewQueueStatsFactory returns a ChannelFactory that captures a StatsLogProvider
// (#571 item 2)。詳細は server_stats.go の同名 factory を参照。
func NewQueueStatsFactory(logger StatsLogProvider) stream.ChannelFactory {
	return func(ctx stream.ChannelContext) stream.Channel {
		return &QueueStatsChannel{ctx: ctx, logger: logger}
	}
}

func (c *QueueStatsChannel) Init(_ json.RawMessage) error {
	c.connected = true
	c.ctx.Subscribe("queueStats")
	return nil
}

func (c *QueueStatsChannel) OnRedisEvent(payload []byte) {
	_ = c.ctx.Send("stats", json.RawMessage(payload))
}

// OnClientMessage handles requestLog from the client. server_stats と同形式。
func (c *QueueStatsChannel) OnClientMessage(msgType string, body json.RawMessage) {
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
	_ = json.Unmarshal(body, &req)
	_ = c.ctx.Send("statsLog", c.logger.Log(req.Length))
}

// ShouldShare implements stream.ShareableChannel.
func (c *QueueStatsChannel) ShouldShare() bool { return true }

func (c *QueueStatsChannel) Dispose() {
	if c.connected {
		c.ctx.Unsubscribe("queueStats")
	}
}
