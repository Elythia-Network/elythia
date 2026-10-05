package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
)

// SchemaPerUserPv returns the schema for the per-user "perUserPv"
// chart. The `upv.*` columns are uniqueIncrement (cardinality of
// distinct visitors), while `pv.*` are raw counters.
func SchemaPerUserPv() chart.Schema {
	return chart.Schema{
		Name:    "perUserPv",
		Grouped: true,
		Columns: []chart.ColumnDef{
			{Name: "upv.user", UniqueIncrement: true, Range: chart.RangeSmall},
			{Name: "pv.user", Range: chart.RangeSmall},
			{Name: "upv.visitor", UniqueIncrement: true, Range: chart.RangeSmall},
			{Name: "pv.visitor", Range: chart.RangeSmall},
		},
	}
}

// PerUserPvChart aggregates profile pageview events for the owning
// user. Two flavours of commit exist depending on whether the visitor
// is logged in (CommitByUser, keyed on the visitor's user id) or
// anonymous (CommitByVisitor, keyed on a session/cookie).
type PerUserPvChart struct {
	c *chart.Chart
}

// NewPerUserPvChart wraps an engine instance built with SchemaPerUserPv.
func NewPerUserPvChart(c *chart.Chart) *PerUserPvChart {
	return &PerUserPvChart{c: c}
}

// Chart returns the underlying engine pointer.
func (p *PerUserPvChart) Chart() *chart.Chart { return p.c }

// CommitByUser records a pageview by an authenticated visitor. `key`
// is the visitor's user id and is added to the unique upv set;
// `pv.user` is bumped by one regardless.
func (p *PerUserPvChart) CommitByUser(ownerID, key string) error {
	return p.c.Commit(chart.Diff{
		"upv.user": []string{key},
		"pv.user":  int64(1),
	}, ownerID)
}

// CommitByVisitor records a pageview by an anonymous visitor. `key`
// is typically a session/cookie identifier; `pv.visitor` is bumped by
// one regardless of dedup.
func (p *PerUserPvChart) CommitByVisitor(ownerID, key string) error {
	return p.c.Commit(chart.Diff{
		"upv.visitor": []string{key},
		"pv.visitor":  int64(1),
	}, ownerID)
}
