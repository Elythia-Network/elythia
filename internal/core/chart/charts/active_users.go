package charts

import (
	"time"

	"github.com/elythia-network/elythia/internal/core/chart"
)

// Window thresholds for the registeredWithin* / registeredOutside*
// columns. The upstream uses 7 / 30 / 365 day spans counted in
// milliseconds; we use the same span values via time.Duration.
const (
	activeUsersWeek  = 7 * 24 * time.Hour
	activeUsersMonth = 30 * 24 * time.Hour
	activeUsersYear  = 365 * 24 * time.Hour
)

// SchemaActiveUsers returns the schema for the instance-wide
// "activeUsers" chart. `readWrite` is the intersection of `read` and
// `write`; the rest are uniqueIncrement sets sliced by the user's
// registration age.
func SchemaActiveUsers() chart.Schema {
	return chart.Schema{
		Name: "activeUsers",
		Columns: []chart.ColumnDef{
			{Name: "readWrite", IntersectionOf: []string{"read", "write"}},
			{Name: "read", UniqueIncrement: true},
			{Name: "write", UniqueIncrement: true},
			{Name: "registeredWithinWeek", UniqueIncrement: true},
			{Name: "registeredWithinMonth", UniqueIncrement: true},
			{Name: "registeredWithinYear", UniqueIncrement: true},
			{Name: "registeredOutsideWeek", UniqueIncrement: true},
			{Name: "registeredOutsideMonth", UniqueIncrement: true},
			{Name: "registeredOutsideYear", UniqueIncrement: true},
		},
	}
}

// ActiveUsersChart aggregates active-user events. The wrapper exposes
// Read and Write helpers; the chart engine takes care of merging the
// unique sets and computing readWrite via intersection bake.
type ActiveUsersChart struct {
	c     *chart.Chart
	clock chart.Clock
}

// NewActiveUsersChart wraps an engine built with SchemaActiveUsers.
// The clock is needed independently of the engine because the
// registeredWithin* age computation must use the same "now" for all
// columns; we read it via the chart.Clock interface so tests can
// substitute a deterministic value.
func NewActiveUsersChart(c *chart.Chart, clock chart.Clock) *ActiveUsersChart {
	if clock == nil {
		clock = chart.SystemClock{}
	}
	return &ActiveUsersChart{c: c, clock: clock}
}

// Chart returns the underlying engine pointer.
func (a *ActiveUsersChart) Chart() *chart.Chart { return a.c }

// Read records that the given local user accessed the API. The user
// id is added to the unique `read` set and to whichever
// registered{Within,Outside}{Week,Month,Year} buckets apply.
//
// `createdAt` is the user's signup timestamp (parsed from the user id
// upstream by `IdService.parse`).
func (a *ActiveUsersChart) Read(userID string, createdAt time.Time) error {
	return a.c.Commit(a.partition(userID, createdAt, "read"), "")
}

// Write records that the given local user wrote (created a note,
// changed a setting etc.). Only the `write` set is updated; readWrite
// is computed via the intersection bake on Save.
func (a *ActiveUsersChart) Write(userID string, _ time.Time) error {
	return a.c.Commit(chart.Diff{
		"write": []string{userID},
	}, "")
}

// partition builds the diff for a Read or Write event. The slice keys
// are populated only when the user falls in the corresponding age
// bucket; chart.Chart.Commit drops empty slices, so the unused keys do
// not consume bandwidth.
func (a *ActiveUsersChart) partition(userID string, createdAt time.Time, kind string) chart.Diff {
	now := a.clock.Now()
	age := now.Sub(createdAt)
	yes := []string{userID}
	pick := func(cond bool) []string {
		if cond {
			return yes
		}
		return nil
	}
	diff := chart.Diff{
		kind: yes,
	}
	addIfPresent := func(k string, v []string) {
		if v != nil {
			diff[k] = v
		}
	}
	addIfPresent("registeredWithinWeek", pick(age < activeUsersWeek))
	addIfPresent("registeredWithinMonth", pick(age < activeUsersMonth))
	addIfPresent("registeredWithinYear", pick(age < activeUsersYear))
	addIfPresent("registeredOutsideWeek", pick(age > activeUsersWeek))
	addIfPresent("registeredOutsideMonth", pick(age > activeUsersMonth))
	addIfPresent("registeredOutsideYear", pick(age > activeUsersYear))
	return diff
}
