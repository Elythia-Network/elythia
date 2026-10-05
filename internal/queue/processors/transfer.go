package processors

import (
	"context"
	"errors"
	"fmt"

	"github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// ExportProcessor handles `export` tasks by delegating to transfer.Exporter.
type ExportProcessor struct {
	exporter *transfer.Exporter
}

// NewExportProcessor constructs an ExportProcessor.
func NewExportProcessor(exporter *transfer.Exporter) *ExportProcessor {
	return &ExportProcessor{exporter: exporter}
}

// Handle dispatches a single export task.
func (p *ExportProcessor) Handle(ctx context.Context, t driver.Task) error {
	if p.exporter == nil {
		return fmt.Errorf("export processor not configured: %w", driver.ErrSkipRetry)
	}
	payload, err := queue.DecodeExportPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode export payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.UserID == "" || payload.Type == "" {
		return fmt.Errorf("export payload missing fields: %w", driver.ErrSkipRetry)
	}
	if _, err := p.exporter.Export(ctx, payload.UserID, payload.Type,
		transfer.WithExcludeMuting(payload.ExcludeMuting),
		transfer.WithExcludeInactive(payload.ExcludeInactive)); err != nil {
		// 未対応typeはリトライしても通らないので ErrSkipRetry。
		if errors.Is(err, transfer.ErrUnsupportedType) {
			return fmt.Errorf("%w: %w", err, driver.ErrSkipRetry)
		}
		return err
	}
	return nil
}

// ImportProcessor handles `import` tasks by delegating to transfer.Importer.
type ImportProcessor struct {
	importer *transfer.Importer
}

// NewImportProcessor constructs an ImportProcessor.
func NewImportProcessor(importer *transfer.Importer) *ImportProcessor {
	return &ImportProcessor{importer: importer}
}

// Handle dispatches a single import task.
func (p *ImportProcessor) Handle(ctx context.Context, t driver.Task) error {
	if p.importer == nil {
		return fmt.Errorf("import processor not configured: %w", driver.ErrSkipRetry)
	}
	payload, err := queue.DecodeImportPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode import payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.UserID == "" || payload.Type == "" || payload.FileID == "" {
		return fmt.Errorf("import payload missing fields: %w", driver.ErrSkipRetry)
	}
	if _, err := p.importer.Import(ctx, payload.UserID, payload.Type, payload.FileID, transfer.WithFollowReplies(payload.WithReplies)); err != nil {
		if errors.Is(err, transfer.ErrUnsupportedType) {
			return fmt.Errorf("%w: %w", err, driver.ErrSkipRetry)
		}
		return err
	}
	return nil
}
