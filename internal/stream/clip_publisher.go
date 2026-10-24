package stream

import (
	"context"
	"encoding/json"
	"log/slog"
)

// ClipStreamPublisher serializes `{type, body}` envelopes to the Redis topic
// `clipStream:{clipID}` which the `clip` WebSocket channel subscribes to
// (upstream 2026.10.1 GlobalEventService.publishClipStream, #3809). The events
// are `updated` and `deleted`, both with a null body.
type ClipStreamPublisher struct {
	pub PubSubPublisher
}

// NewClipStreamPublisher constructs a ClipStreamPublisher backed by the given
// PubSubPublisher.
func NewClipStreamPublisher(pub PubSubPublisher) *ClipStreamPublisher {
	return &ClipStreamPublisher{pub: pub}
}

// PublishClipEvent emits eventType to `clipStream:{clipID}` with a null body.
func (p *ClipStreamPublisher) PublishClipEvent(clipID, eventType string) {
	if p.pub == nil || clipID == "" || eventType == "" {
		return
	}
	// 本家は body を持たない (null)。文字列と nil だけの値なので Marshal は失敗しない。
	raw, _ := json.Marshal(struct {
		Type string `json:"type"`
		Body any    `json:"body"`
	}{Type: eventType})
	topic := "clipStream:" + clipID
	if err := p.pub.Publish(context.Background(), topic, json.RawMessage(raw)); err != nil {
		slog.Warn("clip publisher: publish failed", "topic", topic, "err", err)
	}
}
