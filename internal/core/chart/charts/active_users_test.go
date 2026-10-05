package charts

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/chart"
)

func newActiveUsersTestEngine(t *testing.T) (*chart.Chart, *fakeRepo, *fakeClock) {
	t.Helper()
	repo := newFakeRepo()
	clk := newFakeClock(time.Date(2026, 4, 9, 12, 30, 0, 0, time.UTC))
	c, err := chart.New(chart.Config{
		Schema: SchemaActiveUsers(),
		Repo:   repo,
		Lock:   chart.NewMemoryLocker(),
		Clock:  clk,
	})
	require.NoError(t, err)
	return c, repo, clk
}

func TestActiveUsersChart_ReadWithinWeek(t *testing.T) {
	engine, repo, clk := newActiveUsersTestEngine(t)
	ac := NewActiveUsersChart(engine, clk)
	require.Same(t, engine, ac.Chart())

	created := clk.Now().Add(-2 * 24 * time.Hour) // 2 日前
	require.NoError(t, ac.Read("u-young", created))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assertUnique(t, row, "read", []string{"u-young"})
	assertUnique(t, row, "registeredWithinWeek", []string{"u-young"})
	assertUnique(t, row, "registeredWithinMonth", []string{"u-young"})
	assertUnique(t, row, "registeredWithinYear", []string{"u-young"})
	assertUniqueEmpty(t, row, "registeredOutsideWeek")
	assertUniqueEmpty(t, row, "registeredOutsideMonth")
	assertUniqueEmpty(t, row, "registeredOutsideYear")
}

func TestActiveUsersChart_ReadOldUser(t *testing.T) {
	engine, repo, clk := newActiveUsersTestEngine(t)
	ac := NewActiveUsersChart(engine, clk)

	created := clk.Now().Add(-400 * 24 * time.Hour) // 1年以上前
	require.NoError(t, ac.Read("u-old", created))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assertUnique(t, row, "read", []string{"u-old"})
	assertUnique(t, row, "registeredOutsideWeek", []string{"u-old"})
	assertUnique(t, row, "registeredOutsideMonth", []string{"u-old"})
	assertUnique(t, row, "registeredOutsideYear", []string{"u-old"})
	assertUniqueEmpty(t, row, "registeredWithinWeek")
}

func TestActiveUsersChart_WriteOnly(t *testing.T) {
	engine, repo, clk := newActiveUsersTestEngine(t)
	ac := NewActiveUsersChart(engine, clk)

	require.NoError(t, ac.Write("u-w", time.Time{}))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assertUnique(t, row, "write", []string{"u-w"})
	assertUniqueEmpty(t, row, "read")
}

func TestActiveUsersChart_ReadWriteIntersection(t *testing.T) {
	engine, repo, clk := newActiveUsersTestEngine(t)
	ac := NewActiveUsersChart(engine, clk)

	created := clk.Now().Add(-3 * 24 * time.Hour)
	require.NoError(t, ac.Read("u-rw", created))
	require.NoError(t, ac.Write("u-rw", created))
	require.NoError(t, ac.Read("u-r-only", created))
	require.NoError(t, ac.Write("u-w-only", created))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	// readWrite は intersection bake により 1
	assert.Equal(t, int64(1), toInt64(row.Cols["readWrite"]))
	// read 集合は 2 種類 (u-rw, u-r-only)
	assert.Equal(t, int64(2), toInt64(row.Cols["read"]))
	// write 集合は 2 種類 (u-rw, u-w-only)
	assert.Equal(t, int64(2), toInt64(row.Cols["write"]))
}

func TestNewActiveUsersChart_NilClockUsesSystem(t *testing.T) {
	repo := newFakeRepo()
	engine, err := chart.New(chart.Config{
		Schema: SchemaActiveUsers(),
		Repo:   repo,
		Lock:   chart.NewMemoryLocker(),
	})
	require.NoError(t, err)
	ac := NewActiveUsersChart(engine, nil)
	// nil clock を渡しても panic せず、SystemClock が使われていれば
	// Read で age 計算ができる。
	require.NoError(t, ac.Read("u", time.Now().Add(-1*time.Hour)))
	require.NoError(t, engine.Save(context.Background()))
}

func assertUnique(t *testing.T, row *chart.Row, name string, want []string) {
	t.Helper()
	got, ok := row.Cols[name+":unique"].([]string)
	require.Truef(t, ok, "column %q is not []string", name)
	assert.Equal(t, want, got)
}

func assertUniqueEmpty(t *testing.T, row *chart.Row, name string) {
	t.Helper()
	got, ok := row.Cols[name+":unique"].([]string)
	if !ok {
		// 列に一度も触れていないので nil でも合格
		return
	}
	assert.Empty(t, got)
}
