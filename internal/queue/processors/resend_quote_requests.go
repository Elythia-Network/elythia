package processors

import (
	"context"
	"log/slog"
	"time"

	"github.com/shiroha-a/mk/internal/queue/driver"
)

// QuoteRequestResender sends again the QuoteRequests that are still pending
// and due (#3238). core/federation.QuoteOutbox satisfies it.
type QuoteRequestResender interface {
	ResendPending(now time.Time) (int, error)
}

// ResendQuoteRequestsProcessor runs the per-minute resend of pending
// QuoteRequests (FEP-044f、#3238)。
type ResendQuoteRequestsProcessor struct {
	resender QuoteRequestResender
	now      func() time.Time
}

// NewResendQuoteRequestsProcessor constructs a processor.
func NewResendQuoteRequestsProcessor(r QuoteRequestResender) *ResendQuoteRequestsProcessor {
	return &ResendQuoteRequestsProcessor{resender: r, now: time.Now}
}

// Handle implements the driver handler contract.
func (p *ResendQuoteRequestsProcessor) Handle(_ context.Context, _ driver.Task) error {
	// 失敗しても nil を返して success 扱い (MaxRetry(0))。次の回 (1 分後) が
	// 残りを拾う。
	n, err := p.resender.ResendPending(p.now())
	if err != nil {
		slog.Warn("resendQuoteRequests: failed", "err", err)
		return nil
	}
	if n > 0 {
		slog.Info("resendQuoteRequests: sent", "count", n)
	}
	return nil
}
