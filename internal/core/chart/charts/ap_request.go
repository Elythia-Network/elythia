package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
)

// SchemaApRequest returns the schema for the instance-wide
// "apRequest" chart. The chart only tracks raw counts: number of
// successful / failed deliveries and inbox receives.
func SchemaApRequest() chart.Schema {
	return chart.Schema{
		Name: "apRequest",
		Columns: []chart.ColumnDef{
			{Name: "deliverFailed"},
			{Name: "deliverSucceeded"},
			{Name: "inboxReceived"},
		},
	}
}

// ApRequestChart aggregates ActivityPub request counts. Methods
// correspond to the upstream `ApRequestChart.deliverSucc` /
// `deliverFail` / `inbox` helpers.
type ApRequestChart struct {
	c *chart.Chart
}

// NewApRequestChart wraps an engine instance built with
// SchemaApRequest.
func NewApRequestChart(c *chart.Chart) *ApRequestChart {
	return &ApRequestChart{c: c}
}

// Chart returns the underlying engine pointer.
func (a *ApRequestChart) Chart() *chart.Chart { return a.c }

// DeliverSucceeded bumps the deliverSucceeded counter by one.
func (a *ApRequestChart) DeliverSucceeded() error {
	return a.c.Commit(chart.Diff{"deliverSucceeded": int64(1)}, "")
}

// DeliverFailed bumps the deliverFailed counter by one.
func (a *ApRequestChart) DeliverFailed() error {
	return a.c.Commit(chart.Diff{"deliverFailed": int64(1)}, "")
}

// Inbox bumps the inboxReceived counter by one.
func (a *ApRequestChart) Inbox() error {
	return a.c.Commit(chart.Diff{"inboxReceived": int64(1)}, "")
}
