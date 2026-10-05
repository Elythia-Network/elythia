package processors

import (
	"context"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
)

// InstanceRefreshFetcher is the narrow interface the InstanceRefreshProcessor
// needs from the instance metadata fetcher. Matches
// coreinstance.FetchMetadataService.Fetch signature so the processor stays
// decoupled from the full instance service.
type InstanceRefreshFetcher interface {
	Fetch(host string) error
}

// InstanceRefreshConfig controls how aggressively the periodic refresh walks
// the instance table.
type InstanceRefreshConfig struct {
	// StaleAfter is how old `infoUpdatedAt` must be before a row becomes a
	// refresh candidate. Zero falls back to 24h.
	StaleAfter time.Duration
	// BatchLimit caps the number of hosts walked in a single cron firing so
	// that a large federation graph does not monopolise the maintenance
	// queue. Zero falls back to 100.
	BatchLimit int
	// FetchDelay throttles successive Fetch calls so we do not spike
	// outbound HTTP connections. Zero falls back to 200ms.
	FetchDelay time.Duration
}

// InstanceRefreshProcessor walks stale instance rows and calls the metadata
// fetcher once per host. Failures are logged but do not fail the job, so a
// single unreachable host cannot stall refreshes for the rest.
type InstanceRefreshProcessor struct {
	repo    repository.InstanceRepository
	fetcher InstanceRefreshFetcher
	cfg     InstanceRefreshConfig
	now     func() time.Time
}

// NewInstanceRefreshProcessor constructs a processor. A nil fetcher turns the
// processor into a no-op so callers can wire it before the fetcher is ready.
func NewInstanceRefreshProcessor(
	repo repository.InstanceRepository,
	fetcher InstanceRefreshFetcher,
	cfg InstanceRefreshConfig,
) *InstanceRefreshProcessor {
	return &InstanceRefreshProcessor{
		repo:    repo,
		fetcher: fetcher,
		cfg:     cfg,
		now:     time.Now,
	}
}

// SetClock overrides the clock source. Intended for tests.
func (p *InstanceRefreshProcessor) SetClock(now func() time.Time) {
	if now != nil {
		p.now = now
	}
}

// Handle implements the driver handler contract.
func (p *InstanceRefreshProcessor) Handle(ctx context.Context, _ driver.Task) error {
	if p.repo == nil || p.fetcher == nil {
		return nil
	}
	staleAfter := p.cfg.StaleAfter
	if staleAfter <= 0 {
		staleAfter = 24 * time.Hour
	}
	batch := p.cfg.BatchLimit
	if batch <= 0 {
		batch = 100
	}
	delay := p.cfg.FetchDelay
	if delay < 0 {
		delay = 0
	}
	if delay == 0 {
		delay = 200 * time.Millisecond
	}

	cutoff := p.now().Add(-staleAfter)
	rows, err := p.repo.ListForRefresh(cutoff, batch)
	if err != nil {
		return err
	}

	var refreshed, failed int
	for _, inst := range rows {
		if err := ctx.Err(); err != nil {
			// ctx cancellation (shutdown / deadline) は呼び出し側 driver が
			// リトライするので、ここで中断しても safe。これまでの成果を log。
			slog.Info("instance refresh: interrupted",
				"processed", refreshed+failed, "total", len(rows))
			return err
		}
		if err := p.fetcher.Fetch(inst.Host); err != nil {
			slog.Warn("instance refresh: fetch failed",
				"host", inst.Host, "err", err)
			failed++
		} else {
			refreshed++
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	slog.Info("instance refresh: done",
		"refreshed", refreshed, "failed", failed, "total", len(rows))
	return nil
}
