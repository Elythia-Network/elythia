package stream

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// ReversiGamePublisher serializes {type, body} envelopes to the Redis topic
// `reversiGame:{gameID}` which the reversi WebSocket channel subscribes to.
// 本家 ReversiService の publish パターンと同じ。
type ReversiGamePublisher struct {
	pub PubSubPublisher
}

// NewReversiGamePublisher constructs a ReversiGamePublisher.
func NewReversiGamePublisher(pub PubSubPublisher) *ReversiGamePublisher {
	return &ReversiGamePublisher{pub: pub}
}

// PublishGameEvent implements core/reversi.GamePublisher.
func (p *ReversiGamePublisher) PublishGameEvent(gameID, eventType string, body any) {
	if p.pub == nil || gameID == "" {
		return
	}
	env := map[string]any{"type": eventType, "body": body}
	raw, err := json.Marshal(env)
	if err != nil {
		slog.Warn("reversi publisher: marshal failed", "event", eventType, "err", err)
		return
	}
	topic := "reversiGame:" + gameID
	if err := p.pub.Publish(context.Background(), topic, json.RawMessage(raw)); err != nil {
		slog.Warn("reversi publisher: publish failed", "topic", topic, "err", err)
	}
}

// PublishInvited emits an `invited` event to the `reversi:<userID>` stream
// topic that the Misskey/CherryPick frontend's `reversi` channel subscribes
// to。body は CherryPick 互換で `{ user: UserLite }` shape。これを出さないと
// フロントは polling (/api/reversi/invitations) でしか招待を検知できず
// reload が必要になる (#417 P2)。
func (p *ReversiGamePublisher) PublishInvited(targetUserID string, inviter *model.User) {
	if p.pub == nil || targetUserID == "" || inviter == nil {
		return
	}
	env := map[string]any{
		"type": "invited",
		"body": map[string]any{
			"user": entity.PackUserLite(inviter),
		},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		slog.Warn("reversi publisher: marshal invited failed", "err", err)
		return
	}
	topic := "reversi:" + targetUserID
	if err := p.pub.Publish(context.Background(), topic, json.RawMessage(raw)); err != nil {
		slog.Warn("reversi publisher: publish invited failed", "topic", topic, "err", err)
	}
}
