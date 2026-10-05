package processors

import (
	"context"
	"log/slog"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// RetentionAggregator is the narrow interface the daily retention job needs.
// Matches `core/retention.Service.Aggregate`.
type RetentionAggregator interface {
	Aggregate(ctx context.Context) error
}

// RetentionAggregateProcessor delegates to the retention aggregation
// service on each scheduled fire (#421)。失敗はログだけ残してジョブとしては
// success 扱いにする (driver の retry に任せて雪崩らせる利点が無い)。
type RetentionAggregateProcessor struct {
	svc RetentionAggregator
}

// NewRetentionAggregateProcessor constructs a processor. nil svc turns the
// handler into a no-op so callers can wire it before the service is ready.
func NewRetentionAggregateProcessor(svc RetentionAggregator) *RetentionAggregateProcessor {
	return &RetentionAggregateProcessor{svc: svc}
}

// Handle implements the driver handler contract.
func (p *RetentionAggregateProcessor) Handle(ctx context.Context, _ driver.Task) error {
	if p.svc == nil {
		return nil
	}
	if err := p.svc.Aggregate(ctx); err != nil {
		// 失敗時も nil を返してジョブを success 扱いにする。MaxRetry(0) で
		// driver に retry させない方針なので err を返すと dead queue が肥大
		// するだけで利点が無い。次回 cron (翌日 0:00) を待てば自然に
		// 再アグリゲートされる。
		slog.Warn("retention aggregate: failed", "err", err)
		return nil
	}
	slog.Info("retention aggregate: done")
	return nil
}
