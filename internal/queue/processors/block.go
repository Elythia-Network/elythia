package processors

import (
	"context"
	"errors"
	"fmt"

	"github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
)

// Blocker is the narrow subset of core/blocking.Service the block processor
// needs (テスト差し替えのため interface で受け取る)。
type Blocker interface {
	Block(blockerID, blockeeID string) (*model.Blocking, error)
	BlockSilent(blockerID, blockeeID string) (*model.Blocking, error)
}

// BlockProcessor handles per-pair Block queue jobs. Misskey TS の
// relationshipQueue 'block' job (RelationshipProcessorService.processBlock)
// と同じ役割 (#1605)。
type BlockProcessor struct {
	blocker Blocker
}

// NewBlockProcessor wires the processor with the blocking service.
func NewBlockProcessor(blocker Blocker) *BlockProcessor {
	return &BlockProcessor{blocker: blocker}
}

// Handle implements driver.HandlerFunc.
func (p *BlockProcessor) Handle(_ context.Context, t driver.Task) error {
	payload, err := queue.DecodeBlockPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode block payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.BlockerID == "" || payload.BlockeeID == "" {
		return fmt.Errorf("block: blockerId and blockeeId are required: %w", driver.ErrSkipRetry)
	}
	if p.blocker == nil {
		return fmt.Errorf("block: service not wired: %w", driver.ErrSkipRetry)
	}
	// 本家 processBlock は job の silent をそのまま block に渡す (インポートは
	// silent: true)。
	block := p.blocker.Block
	if payload.Silent {
		block = p.blocker.BlockSilent
	}
	_, err = block(payload.BlockerID, payload.BlockeeID)
	switch {
	case err == nil:
		return nil
	// 既に block 済は望む終状態に到達済みとして成功扱い - 冪等吸収。
	case errors.Is(err, blocking.ErrAlreadyBlocking):
		return nil
	// 自己 block / 対象不在は retry しても解消しない恒久エラー。
	case errors.Is(err, blocking.ErrSelfBlock),
		errors.Is(err, blocking.ErrBlockeeNotFound):
		return fmt.Errorf("block %s->%s: %w: %w",
			payload.BlockerID, payload.BlockeeID, err, driver.ErrSkipRetry)
	default:
		return err
	}
}
