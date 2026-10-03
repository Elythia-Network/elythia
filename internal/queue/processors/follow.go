package processors

import (
	"context"
	"errors"
	"fmt"

	"github.com/shiroha-a/mk/internal/core/following"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
)

// Follower is the narrow subset of core/following.Service the processor
// needs. core/following は queue を import しないため直接参照しても循環
// しない (processors/transfer.go と同じ前例) が、テスト差し替えのため
// interface で受け取る。
type Follower interface {
	Follow(followerID, followeeID string, opts following.FollowOptions) (*following.FollowResult, error)
}

// FollowProcessor handles per-pair Follow queue jobs. Misskey TS の
// relationshipQueue 'follow' job (RelationshipProcessorService.processFollow)
// と同じ役割 (#1605)。
type FollowProcessor struct {
	follower Follower
}

// NewFollowProcessor wires the processor with the follow service.
func NewFollowProcessor(follower Follower) *FollowProcessor {
	return &FollowProcessor{follower: follower}
}

// Handle implements driver.HandlerFunc.
func (p *FollowProcessor) Handle(_ context.Context, t driver.Task) error {
	payload, err := queue.DecodeFollowPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode follow payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.FollowerID == "" || payload.FolloweeID == "" {
		return fmt.Errorf("follow: followerId and followeeId are required: %w", driver.ErrSkipRetry)
	}
	if p.follower == nil {
		return fmt.Errorf("follow: service not wired: %w", driver.ErrSkipRetry)
	}
	_, err = p.follower.Follow(payload.FollowerID, payload.FolloweeID,
		following.FollowOptions{WithReplies: payload.WithReplies, Silent: payload.Silent})
	switch {
	case err == nil:
		return nil
	// 既に follow 済 / follow request 送信済は望む終状態に到達済みとして
	// 成功扱い - 冪等吸収。
	case errors.Is(err, following.ErrAlreadyFollowing),
		errors.Is(err, following.ErrAlreadyRequested):
		return nil
	// block 関係 / 自己 follow / 対象不在は retry しても解消しない恒久
	// エラーなので即終了する (本家は job fail のまま放置される)。
	case errors.Is(err, following.ErrBlocked),
		errors.Is(err, following.ErrBlocking),
		errors.Is(err, following.ErrSelfFollow),
		errors.Is(err, following.ErrFolloweeNotFound):
		return fmt.Errorf("follow %s->%s: %w: %w",
			payload.FollowerID, payload.FolloweeID, err, driver.ErrSkipRetry)
	default:
		return err
	}
}
