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
	// entityPacker は利用者を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3794)。構築子の必須の引数で受ける (#3827)。
	entityPacker *entity.Packer
}

// NewReversiGamePublisher constructs a ReversiGamePublisher. entityPacker is
// the Packer it packs users with (#3794). 無いと avatarUrl などを生のまま返し、
// 閲覧者の IP がリモートへ漏れるので、構築子の必須の引数で受ける (#3827)。
func NewReversiGamePublisher(pub PubSubPublisher, entityPacker *entity.Packer) *ReversiGamePublisher {
	return &ReversiGamePublisher{pub: pub, entityPacker: entityPacker}
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
			"user": p.entityPacker.PackUserLite(inviter),
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
