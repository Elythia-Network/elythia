package processors

import (
	"context"
	"fmt"

	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// ChartProcessor implements the periodic tick / resync / clean handlers
// that mirror Misskey upstream's tickCharts / resyncCharts / cleanCharts
// queue processors. それぞれ driver.Scheduler の cron pattern で起動される。
type ChartProcessor struct {
	// charts is the ordered list of every chart engine that should be
	// ticked / resynced / cleaned. Caller passes them in the same order
	// upstream lists them; ordering only matters for debug output since
	// the chart job never runs concurrently with itself.
	charts []*chart.Chart
}

// NewChartProcessor wraps the given chart engines.
func NewChartProcessor(charts []*chart.Chart) *ChartProcessor {
	return &ChartProcessor{charts: charts}
}

// HandleTick runs a minor tick on every registered chart. ungrouped
// charts only — grouped charts (per-user / per-host) cannot be enumerated
// without an external driver and are skipped, matching the upstream
// TickChartsProcessorService.process behaviour.
func (p *ChartProcessor) HandleTick(ctx context.Context, _ driver.Task) error {
	return p.runTick(ctx, false)
}

// HandleResync runs a major tick on every registered chart. Charts whose
// TickFunc returns nil for major are no-ops; per-group charts (per-user /
// per-host) require enumeration and are skipped — this is intentional and
// matches Misskey TS upstream's TickChartsProcessorService.process which
// also excludes per-group charts due to the same enumeration cost. Past
// audits (#572 item 3) flagged this as TODO but the design decision is
// to preserve TS-equivalent behaviour rather than diverge; implementing
// resync would require an opt-in admin flag + admin UI which is out of
// scope while frontend remains drop-in.
func (p *ChartProcessor) HandleResync(ctx context.Context, _ driver.Task) error {
	return p.runTick(ctx, true)
}

// HandleClean clears expired uniqueIncrement temp arrays on every
// registered chart.
func (p *ChartProcessor) HandleClean(ctx context.Context, _ driver.Task) error {
	for _, c := range p.charts {
		if err := c.Clean(ctx); err != nil {
			return fmt.Errorf("chart clean %s: %w", c.Name(), err)
		}
	}
	return nil
}

// runTick walks every registered chart and calls Tick. ungrouped charts
// pass an empty group; grouped charts are skipped here because the cron
// driver has no enumeration source for them.
func (p *ChartProcessor) runTick(ctx context.Context, major bool) error {
	for _, c := range p.charts {
		if c.IsGrouped() {
			// upstream も grouped chart (instance / per-user-*) は cron で
			// tick せず、関連サービス側で resync をトリガする設計。
			continue
		}
		if err := c.Tick(ctx, major, ""); err != nil {
			return fmt.Errorf("chart tick %s (major=%v): %w", c.Name(), major, err)
		}
	}
	return nil
}
