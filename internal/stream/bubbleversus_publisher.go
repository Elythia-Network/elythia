package stream

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/core/bubbleversus"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// BubbleVersusPublisher publishes bubble-game versus events (#3230) as
// {type, body} envelopes. 招待など対局の外の知らせは `bubbleVersus:<userId>`、
// 対局中のやりとりは `bubbleVersusMatch:<matchId>` へ出す。
type BubbleVersusPublisher struct {
	pub PubSubPublisher
}

// NewBubbleVersusPublisher constructs a BubbleVersusPublisher.
func NewBubbleVersusPublisher(pub PubSubPublisher) *BubbleVersusPublisher {
	return &BubbleVersusPublisher{pub: pub}
}

var _ bubbleversus.Publisher = (*BubbleVersusPublisher)(nil)

// BubbleVersusUserTopic is the topic of userID's invitation channel.
func BubbleVersusUserTopic(userID string) string { return "bubbleVersus:" + userID }

// BubbleVersusMatchTopic is the topic of a match channel.
func BubbleVersusMatchTopic(matchID string) string { return "bubbleVersusMatch:" + matchID }

// PublishInvited implements bubbleversus.Publisher.
func (p *BubbleVersusPublisher) PublishInvited(targetUserID string, inviter *model.User, m *bubbleversus.Match) {
	if inviter == nil || m == nil {
		return
	}
	p.publish(BubbleVersusUserTopic(targetUserID), "invited", map[string]any{
		"matchId":  m.ID,
		"gameMode": m.GameMode,
		"user":     entity.PackUserLite(inviter),
	})
}

// PublishUser implements bubbleversus.Publisher.
func (p *BubbleVersusPublisher) PublishUser(userID, eventType string, body any) {
	p.publish(BubbleVersusUserTopic(userID), eventType, body)
}

// PublishMatch implements bubbleversus.Publisher.
func (p *BubbleVersusPublisher) PublishMatch(matchID, eventType string, body any) {
	p.publish(BubbleVersusMatchTopic(matchID), eventType, body)
}

func (p *BubbleVersusPublisher) publish(topic, eventType string, body any) {
	if p == nil || p.pub == nil {
		return
	}
	raw, err := json.Marshal(map[string]any{"type": eventType, "body": body})
	if err != nil {
		slog.Warn("bubble versus publisher: marshal failed", "event", eventType, "err", err)
		return
	}
	if err := p.pub.Publish(context.Background(), topic, json.RawMessage(raw)); err != nil {
		slog.Warn("bubble versus publisher: publish failed", "topic", topic, "err", err)
	}
}
