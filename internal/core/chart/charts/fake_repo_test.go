package charts

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/core/chart"
)

// fakeRepo is an in-memory chart.Repository tailored for the wrappers
// in this package. It deliberately mirrors the engine-internal fake
// (internal/core/chart/fake_repo_test.go) so the assertions in tests
// look identical, but lives in its own _test.go because the engine
// fake is unexported.
type fakeRepo struct {
	mu     sync.Mutex
	nextID int64
	hour   map[string][]*chart.Row
	day    map[string][]*chart.Row
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		hour: make(map[string][]*chart.Row),
		day:  make(map[string][]*chart.Row),
	}
}

func (r *fakeRepo) tableFor(span chart.Span) map[string][]*chart.Row {
	if span == chart.SpanDay {
		return r.day
	}
	return r.hour
}

func (r *fakeRepo) FindCurrent(_ context.Context, span chart.Span, group string, ts int64) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.tableFor(span)[group] {
		if row.Date == ts {
			return row, nil
		}
	}
	return nil, chart.ErrRowNotFound
}

func (r *fakeRepo) FindLatest(_ context.Context, span chart.Span, group string) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.tableFor(span)[group]
	if len(rows) == 0 {
		return nil, chart.ErrRowNotFound
	}
	latest := rows[0]
	for _, row := range rows[1:] {
		if row.Date > latest.Date {
			latest = row
		}
	}
	return latest, nil
}

func (r *fakeRepo) FindBefore(_ context.Context, span chart.Span, group string, ts int64) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *chart.Row
	for _, row := range r.tableFor(span)[group] {
		if row.Date < ts {
			if best == nil || row.Date > best.Date {
				best = row
			}
		}
	}
	if best == nil {
		return nil, chart.ErrRowNotFound
	}
	return best, nil
}

func (r *fakeRepo) FindRange(_ context.Context, span chart.Span, group string, gt, lt int64) ([]*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*chart.Row
	for _, row := range r.tableFor(span)[group] {
		if row.Date >= gt && row.Date <= lt {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

func (r *fakeRepo) Insert(_ context.Context, span chart.Span, group string, ts int64, cols map[string]any) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	row := &chart.Row{
		ID:   r.nextID,
		Date: ts,
		Cols: make(map[string]any, len(cols)),
	}
	if group != "" {
		row.Group = sql.NullString{Valid: true, String: group}
	}
	maps.Copy(row.Cols, cols)
	r.tableFor(span)[group] = append(r.tableFor(span)[group], row)
	return row, nil
}

func (r *fakeRepo) ApplyDeltas(_ context.Context, span chart.Span, id int64, deltas map[string]int64, uniqueAppends map[string][]string, setInts map[string]int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.findByID(span, id)
	if row == nil {
		return fmt.Errorf("fakeRepo: row %d not found", id)
	}
	for k, v := range deltas {
		row.Cols[k] = toInt64(row.Cols[k]) + v
	}
	for k, items := range uniqueAppends {
		key := k + ":unique"
		cur, _ := row.Cols[key].([]string)
		row.Cols[key] = append(cur, items...)
	}
	for k, v := range setInts {
		row.Cols[k] = v
	}
	return nil
}

func (r *fakeRepo) SetColumns(_ context.Context, span chart.Span, id int64, cols map[string]int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.findByID(span, id)
	if row == nil {
		return fmt.Errorf("fakeRepo: row %d not found", id)
	}
	for k, v := range cols {
		row.Cols[k] = v
	}
	return nil
}

func (r *fakeRepo) ResetUniqueTempColumns(_ context.Context, span chart.Span, gt, lt int64, columns []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rows := range r.tableFor(span) {
		for _, row := range rows {
			if row.Date > gt && row.Date < lt {
				for _, c := range columns {
					row.Cols[c+":unique"] = []string{}
				}
			}
		}
	}
	return nil
}

func (r *fakeRepo) findByID(span chart.Span, id int64) *chart.Row {
	for _, rows := range r.tableFor(span) {
		for _, row := range rows {
			if row.ID == id {
				return row
			}
		}
	}
	return nil
}

// toInt64 mirrors the helper in the chart package; it coerces a
// driver-backed value into int64 for in-memory tests.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	default:
		return 0
	}
}

// fakeClock returns a fixed UTC time and lets tests advance it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t.UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
