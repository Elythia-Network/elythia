package processors

import (
	"context"
	"errors"
	"fmt"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// Unfollower is the narrow subset of core/following.Service the processor
// needs. パッケージ間の循環依存を避けるため interface で受け取る。
type Unfollower interface {
	Unfollow(followerID, followeeID string) error
	UnfollowWithoutNotify(followerID, followeeID string) error
}

// ErrNotFollowing is returned by Unfollower implementations when the
// pair was already detached (= no row to delete). The processor treats
// this as success since the desired end-state is "not following".
//
// 既存の core/following.ErrNotFollowing と等価。processor が core 層を
// 直 import すると循環依存になるため、processor 側で同名 sentinel を
// 持たせて errors.Is で照合する。本家 Misskey の queue worker も
// "already not following" を success 扱いにしている。
var ErrNotFollowing = errors.New("not following")

// UnfollowProcessor handles per-pair Unfollow queue jobs spawned by
// admin/federation/remove-all-following. Misskey TS の relationship
// queue 'unfollow' job と同じ役割。
type UnfollowProcessor struct {
	unfollower Unfollower
}

// NewUnfollowProcessor wires the processor with the unfollow service.
func NewUnfollowProcessor(unfollower Unfollower) *UnfollowProcessor {
	return &UnfollowProcessor{unfollower: unfollower}
}

// Handle implements driver.HandlerFunc.
func (p *UnfollowProcessor) Handle(_ context.Context, t driver.Task) error {
	payload, err := queue.DecodeUnfollowPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode unfollow payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.FollowerID == "" || payload.FolloweeID == "" {
		return fmt.Errorf("unfollow: followerId and followeeId are required: %w", driver.ErrSkipRetry)
	}
	if p.unfollower == nil {
		return fmt.Errorf("unfollow: service not wired: %w", driver.ErrSkipRetry)
	}
	// 本家 processUnfollow は job の silent を unfollow に渡す。silent なら main
	// stream の unfollow と Webhook を出さない (配送はする)。
	unfollow := p.unfollower.Unfollow
	if payload.Silent {
		unfollow = p.unfollower.UnfollowWithoutNotify
	}
	if err := unfollow(payload.FollowerID, payload.FolloweeID); err != nil {
		// 既に解消済 (row 無し) は成功扱い - 冪等吸収。
		if isNotFollowingError(err) {
			return nil
		}
		return err
	}
	return nil
}

// isNotFollowingError detects the "already not following" sentinel from
// any Unfollower implementation by error string match (互換用)。実装
// が core/following.ErrNotFollowing を返したケースをここで吸収する。
func isNotFollowingError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFollowing) {
		return true
	}
	return err.Error() == "not following"
}
