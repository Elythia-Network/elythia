package processors

import (
	"context"
	"log/slog"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// ModeratorsActivityChecker is the narrow interface the cron job delegates to.
// Matches core/moderatoractivity.Service.Check.
type ModeratorsActivityChecker interface {
	Check() error
}

// CheckModeratorsActivityProcessor delegates to the moderator-activity service
// on each scheduled fire (cron `30 * * * *`, #1563). 失敗はログだけ残して
// success 扱いにする (MaxRetry(0))。
type CheckModeratorsActivityProcessor struct {
	svc ModeratorsActivityChecker
}

// NewCheckModeratorsActivityProcessor constructs a processor. nil svc makes the
// handler a no-op so callers can wire it before the service is ready.
func NewCheckModeratorsActivityProcessor(svc ModeratorsActivityChecker) *CheckModeratorsActivityProcessor {
	return &CheckModeratorsActivityProcessor{svc: svc}
}

// Handle implements the driver handler contract.
func (p *CheckModeratorsActivityProcessor) Handle(_ context.Context, _ driver.Task) error {
	if p.svc == nil {
		return nil
	}
	if err := p.svc.Check(); err != nil {
		slog.Warn("checkModeratorsActivity: failed", "err", err)
		return nil
	}
	return nil
}
