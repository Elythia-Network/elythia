package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
)

// SchemaFederation returns the schema for the instance-wide
// "federation" chart. The first three columns track unique sets of
// hosts (delivered, inbox, stalled); the rest are accumulated counters
// updated by the periodic Tick.
func SchemaFederation() chart.Schema {
	return chart.Schema{
		Name: "federation",
		Columns: []chart.ColumnDef{
			{Name: "deliveredInstances", UniqueIncrement: true, Range: chart.RangeSmall},
			{Name: "inboxInstances", UniqueIncrement: true, Range: chart.RangeSmall},
			{Name: "stalled", UniqueIncrement: true, Range: chart.RangeSmall},
			{Name: "sub", Accumulate: true, Range: chart.RangeSmall},
			{Name: "pub", Accumulate: true, Range: chart.RangeSmall},
			{Name: "pubsub", Accumulate: true, Range: chart.RangeSmall},
			{Name: "subActive", Accumulate: true, Range: chart.RangeSmall},
			{Name: "pubActive", Accumulate: true, Range: chart.RangeSmall},
		},
	}
}

// FederationChart aggregates federation events into the instance-wide
// "federation" chart. Methods correspond to the upstream
// `FederationChart.deliverd` / `inbox` helpers.
type FederationChart struct {
	c *chart.Chart
}

// NewFederationChart wraps an engine instance constructed with
// SchemaFederation.
func NewFederationChart(c *chart.Chart) *FederationChart {
	return &FederationChart{c: c}
}

// Chart returns the underlying engine pointer.
func (f *FederationChart) Chart() *chart.Chart { return f.c }

// Delivered records a deliver attempt to the given host. succeeded
// determines whether the host is appended to `deliveredInstances`
// (success) or `stalled` (failure).
func (f *FederationChart) Delivered(host string, succeeded bool) error {
	if succeeded {
		return f.c.Commit(chart.Diff{
			"deliveredInstances": []string{host},
		}, "")
	}
	return f.c.Commit(chart.Diff{
		"stalled": []string{host},
	}, "")
}

// Inbox records an inbox receive event from the given host.
func (f *FederationChart) Inbox(host string) error {
	return f.c.Commit(chart.Diff{
		"inboxInstances": []string{host},
	}, "")
}
