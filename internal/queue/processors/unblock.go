package processors

import (
	"context"
	"errors"
	"fmt"

	"github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// Unblocker is the narrow subset of core/blocking.Service the unblock
// processor needs (テスト差し替えのため interface で受け取る)。
type Unblocker interface {
	Unblock(blockerID, blockeeID string) error
}

// UnblockProcessor handles per-pair Unblock queue jobs. Misskey TS の
// relationshipQueue 'unblock' job (RelationshipProcessorService.processUnblock)
// と同じ役割 (#1605)。
type UnblockProcessor struct {
	unblocker Unblocker
}

// NewUnblockProcessor wires the processor with the blocking service.
func NewUnblockProcessor(unblocker Unblocker) *UnblockProcessor {
	return &UnblockProcessor{unblocker: unblocker}
}

// Handle implements driver.HandlerFunc.
func (p *UnblockProcessor) Handle(_ context.Context, t driver.Task) error {
	payload, err := queue.DecodeUnblockPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode unblock payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.BlockerID == "" || payload.BlockeeID == "" {
		return fmt.Errorf("unblock: blockerId and blockeeId are required: %w", driver.ErrSkipRetry)
	}
	if p.unblocker == nil {
		return fmt.Errorf("unblock: service not wired: %w", driver.ErrSkipRetry)
	}
	err = p.unblocker.Unblock(payload.BlockerID, payload.BlockeeID)
	switch {
	case err == nil:
		return nil
	// 既に解消済 (row 無し) は望む終状態に到達済みとして成功扱い - 冪等吸収。
	case errors.Is(err, blocking.ErrNotBlocking):
		return nil
	// 自己 unblock は retry しても解消しない恒久エラー。
	case errors.Is(err, blocking.ErrSelfBlock):
		return fmt.Errorf("unblock %s->%s: %w: %w",
			payload.BlockerID, payload.BlockeeID, err, driver.ErrSkipRetry)
	default:
		return err
	}
}
