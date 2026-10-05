package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// SchemaPerUserReaction returns the schema for the per-user
// "perUserReaction" chart. Note the singular `Reaction` in the upstream
// entity name (and therefore the SQL table is `__chart__per_user_reaction`).
func SchemaPerUserReaction() chart.Schema {
	return chart.Schema{
		Name:    "perUserReaction",
		Grouped: true,
		Columns: []chart.ColumnDef{
			{Name: "local.count", Range: chart.RangeSmall},
			{Name: "remote.count", Range: chart.RangeSmall},
		},
	}
}

// PerUserReactionChart aggregates reactions received by a user. The
// chart is grouped by the *note owner's* id (the recipient of the
// reaction); the local/remote split is decided by the reactor's host.
type PerUserReactionChart struct {
	c *chart.Chart
}

// NewPerUserReactionChart wraps an engine instance built with
// SchemaPerUserReaction.
func NewPerUserReactionChart(c *chart.Chart) *PerUserReactionChart {
	return &PerUserReactionChart{c: c}
}

// Chart returns the underlying engine pointer.
func (p *PerUserReactionChart) Chart() *chart.Chart { return p.c }

// Update commits one received-reaction event. `reactor` is the user
// who placed the reaction (its host decides the local/remote split);
// `note` is the reacted note and provides the recipient (note owner)
// id used as the chart group.
func (p *PerUserReactionChart) Update(reactor *model.User, note *model.Note) error {
	prefix := userPrefix(reactor)
	return p.c.Commit(chart.Diff{
		prefix + ".count": int64(1),
	}, note.UserID)
}
