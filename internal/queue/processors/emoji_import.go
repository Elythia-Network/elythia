package processors

import (
	"context"
	"errors"
	"fmt"

	"github.com/elythia-network/elythia/internal/core/emojiimport"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// ImportCustomEmojisProcessor handles `importCustomEmojis` tasks by delegating
// to emojiimport.Importer. 本家 Misskey の
// ImportCustomEmojisProcessorService 相当。
type ImportCustomEmojisProcessor struct {
	importer *emojiimport.Importer
}

// NewImportCustomEmojisProcessor constructs the processor.
func NewImportCustomEmojisProcessor(importer *emojiimport.Importer) *ImportCustomEmojisProcessor {
	return &ImportCustomEmojisProcessor{importer: importer}
}

// Handle dispatches a single emoji-zip import task.
func (p *ImportCustomEmojisProcessor) Handle(ctx context.Context, t driver.Task) error {
	if p.importer == nil {
		return fmt.Errorf("import custom emojis processor not configured: %w", driver.ErrSkipRetry)
	}
	payload, err := queue.DecodeImportCustomEmojisPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode import custom emojis payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.UserID == "" || payload.FileID == "" {
		return fmt.Errorf("import custom emojis payload missing fields: %w", driver.ErrSkipRetry)
	}
	if _, err := p.importer.Run(ctx, payload.UserID, payload.FileID); err != nil {
		// ZIP 破損・meta.json 不在 など恒久的エラーは再試行してもムダ。
		if errors.Is(err, emojiimport.ErrInvalidZip) ||
			errors.Is(err, emojiimport.ErrMissingMeta) ||
			errors.Is(err, emojiimport.ErrMalformedMeta) ||
			errors.Is(err, emojiimport.ErrUserNotFound) ||
			errors.Is(err, emojiimport.ErrDriveFileNotFound) ||
			errors.Is(err, emojiimport.ErrZipEntryTooLarge) {
			return fmt.Errorf("%w: %w", err, driver.ErrSkipRetry)
		}
		return err
	}
	return nil
}
