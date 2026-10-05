package processors_test

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memRepo is a minimal in-memory chart.Repository sufficient for
// ChartProcessor unit tests. Only the methods exercised by Tick and
// Clean are non-trivial; the rest return ErrRowNotFound or nil.
type memRepo struct {
	mu      sync.Mutex
	nextID  int64
	rows    map[chart.Span]map[string][]*chart.Row
	resetN  atomic.Int64
	setColN atomic.Int64
}

func newMemRepo() *memRepo {
	return &memRepo{
		rows: map[chart.Span]map[string][]*chart.Row{
			chart.SpanHour: {},
			chart.SpanDay:  {},
		},
	}
}

func (r *memRepo) FindCurrent(_ context.Context, span chart.Span, group string, ts int64) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.rows[span][group] {
		if row.Date == ts {
			return row, nil
		}
	}
	return nil, chart.ErrRowNotFound
}

func (r *memRepo) FindLatest(_ context.Context, span chart.Span, group string) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.rows[span][group]
	if len(rows) == 0 {
		return nil, chart.ErrRowNotFound
	}
	return rows[len(rows)-1], nil
}

func (r *memRepo) FindBefore(_ context.Context, span chart.Span, group string, ts int64) (*chart.Row, error) {
	return nil, chart.ErrRowNotFound
}

func (r *memRepo) FindRange(_ context.Context, span chart.Span, group string, gt, lt int64) ([]*chart.Row, error) {
	return nil, nil
}

func (r *memRepo) Insert(_ context.Context, span chart.Span, group string, ts int64, cols map[string]any) (*chart.Row, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	row := &chart.Row{
		ID:   r.nextID,
		Date: ts,
		Cols: map[string]any{},
	}
	if group != "" {
		row.Group = sql.NullString{Valid: true, String: group}
	}
	for k, v := range cols {
		row.Cols[k] = v
	}
	r.rows[span][group] = append(r.rows[span][group], row)
	return row, nil
}

func (r *memRepo) ApplyDeltas(_ context.Context, _ chart.Span, _ int64, _ map[string]int64, _ map[string][]string, _ map[string]int64) error {
	return nil
}

func (r *memRepo) SetColumns(_ context.Context, _ chart.Span, _ int64, _ map[string]int64) error {
	r.setColN.Add(1)
	return nil
}

func (r *memRepo) ResetUniqueTempColumns(_ context.Context, _ chart.Span, _, _ int64, _ []string) error {
	r.resetN.Add(1)
	return nil
}

// makeChart constructs a chart.Chart wired to a memRepo for testing.
func makeChart(t *testing.T, schema chart.Schema, tick chart.TickFunc) (*chart.Chart, *memRepo) {
	t.Helper()
	repo := newMemRepo()
	c, err := chart.New(chart.Config{
		Schema: schema,
		Repo:   repo,
		Lock:   chart.NewMemoryLocker(),
		Tick:   tick,
		Clock:  fixedClock{t: time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)},
	})
	require.NoError(t, err)
	return c, repo
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// schemaWithUnique returns a tiny schema with one unique-increment column,
// so Clean has work to do.
func schemaWithUnique() chart.Schema {
	return chart.Schema{
		Name: "test_chart",
		Columns: []chart.ColumnDef{
			{Name: "u", UniqueIncrement: true},
		},
	}
}

// schemaPlain returns a schema with no unique columns; Clean is a no-op.
func schemaPlain() chart.Schema {
	return chart.Schema{
		Name: "plain",
		Columns: []chart.ColumnDef{
			{Name: "x"},
		},
	}
}

// schemaGrouped returns a grouped schema; ChartProcessor must skip Tick.
func schemaGrouped() chart.Schema {
	return chart.Schema{
		Name:    "grouped",
		Grouped: true,
		Columns: []chart.ColumnDef{
			{Name: "x"},
		},
	}
}

func TestChartProcessor_HandleClean(t *testing.T) {
	withUnique, repo1 := makeChart(t, schemaWithUnique(), nil)
	plain, repo2 := makeChart(t, schemaPlain(), nil)
	p := processors.NewChartProcessor([]*chart.Chart{withUnique, plain})

	require.NoError(t, p.HandleClean(context.Background(), driver.RawTask{TypeName: "x"}))
	// withUnique chart は span hour + day で 2 回 ResetUniqueTempColumns が呼ばれる
	assert.Equal(t, int64(2), repo1.resetN.Load())
	// plain chart は uniqueColumn が無いので ResetUniqueTempColumns は呼ばれない
	assert.Equal(t, int64(0), repo2.resetN.Load())
}

func TestChartProcessor_HandleTick_SkipsGrouped(t *testing.T) {
	called := atomic.Int64{}
	tickFn := chart.TickFunc(func(_ context.Context, _ string, _ bool) (map[string]int64, error) {
		called.Add(1)
		return map[string]int64{"x": 1}, nil
	})
	plain, _ := makeChart(t, schemaPlain(), tickFn)
	grouped, _ := makeChart(t, schemaGrouped(), tickFn)
	p := processors.NewChartProcessor([]*chart.Chart{plain, grouped})

	require.NoError(t, p.HandleTick(context.Background(), driver.RawTask{TypeName: "x"}))
	// grouped chart は skip される
	assert.Equal(t, int64(1), called.Load())
}

func TestChartProcessor_HandleResync_PassesMajorTrue(t *testing.T) {
	gotMajor := atomic.Bool{}
	tickFn := chart.TickFunc(func(_ context.Context, _ string, major bool) (map[string]int64, error) {
		gotMajor.Store(major)
		return map[string]int64{"x": 1}, nil
	})
	plain, _ := makeChart(t, schemaPlain(), tickFn)
	p := processors.NewChartProcessor([]*chart.Chart{plain})

	require.NoError(t, p.HandleResync(context.Background(), driver.RawTask{TypeName: "x"}))
	assert.True(t, gotMajor.Load())
}

func TestChartProcessor_HandleTick_PassesMajorFalse(t *testing.T) {
	gotMajor := atomic.Bool{}
	gotMajor.Store(true) // 初期値 true でデフォルトを区別できるようにする
	tickFn := chart.TickFunc(func(_ context.Context, _ string, major bool) (map[string]int64, error) {
		gotMajor.Store(major)
		return map[string]int64{"x": 1}, nil
	})
	plain, _ := makeChart(t, schemaPlain(), tickFn)
	p := processors.NewChartProcessor([]*chart.Chart{plain})

	require.NoError(t, p.HandleTick(context.Background(), driver.RawTask{TypeName: "x"}))
	assert.False(t, gotMajor.Load())
}

func TestChartProcessor_HandleTick_NilTickFuncIsNoop(t *testing.T) {
	plain, _ := makeChart(t, schemaPlain(), nil)
	p := processors.NewChartProcessor([]*chart.Chart{plain})
	require.NoError(t, p.HandleTick(context.Background(), driver.RawTask{TypeName: "x"}))
	require.NoError(t, p.HandleResync(context.Background(), driver.RawTask{TypeName: "x"}))
}
