package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// SchemaUsers returns the schema for the instance-wide "users" chart.
// `local.total` / `remote.total` are accumulated; the inc/dec deltas
// fit into smallint.
func SchemaUsers() chart.Schema {
	return chart.Schema{
		Name: "users",
		Columns: []chart.ColumnDef{
			{Name: "local.total", Accumulate: true},
			{Name: "local.inc", Range: chart.RangeSmall},
			{Name: "local.dec", Range: chart.RangeSmall},
			{Name: "remote.total", Accumulate: true},
			{Name: "remote.inc", Range: chart.RangeSmall},
			{Name: "remote.dec", Range: chart.RangeSmall},
		},
	}
}

// UsersChart aggregates user creation / deletion events for the
// instance-wide users chart.
type UsersChart struct {
	c *chart.Chart
}

// NewUsersChart constructs a UsersChart wrapping the supplied engine.
func NewUsersChart(c *chart.Chart) *UsersChart { return &UsersChart{c: c} }

// Chart returns the underlying engine pointer.
func (u *UsersChart) Chart() *chart.Chart { return u.c }

// Update commits the delta from a user create / delete event. Local /
// remote is decided by `user.Host`.
func (u *UsersChart) Update(user *model.User, isAdditional bool) error {
	prefix := "local"
	if user.Host != nil && *user.Host != "" {
		prefix = "remote"
	}
	sign := int64(1)
	if !isAdditional {
		sign = -1
	}
	incVal := int64(0)
	decVal := int64(0)
	if isAdditional {
		incVal = 1
	} else {
		decVal = 1
	}
	diff := chart.Diff{
		prefix + ".total": sign,
		prefix + ".inc":   incVal,
		prefix + ".dec":   decVal,
	}
	return u.c.Commit(diff, "")
}
