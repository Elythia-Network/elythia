package charts

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

func newTestEngine(t *testing.T, schema chart.Schema) (*chart.Chart, *fakeRepo, *fakeClock) {
	t.Helper()
	repo := newFakeRepo()
	clk := newFakeClock(time.Date(2026, 4, 9, 12, 30, 0, 0, time.UTC))
	c, err := chart.New(chart.Config{
		Schema: schema,
		Repo:   repo,
		Lock:   chart.NewMemoryLocker(),
		Clock:  clk,
	})
	require.NoError(t, err)
	return c, repo, clk
}

// strPtr returns a pointer to its argument; used for *string fields.
func strPtr(s string) *string { return &s }

func TestNotesChart_LocalAdditionalNormal(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaNotes())
	nc := NewNotesChart(engine)
	require.Same(t, engine, nc.Chart())

	require.NoError(t, nc.Update(&model.Note{ID: "n1"}, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["local.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["local.inc"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["local.diffs.normal"]))
	// dec/reply/renote/withFile はゼロのまま (engine が seed する)
	assert.Equal(t, int64(0), toInt64(row.Cols["local.dec"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.diffs.reply"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.diffs.renote"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.diffs.withFile"]))
}

func TestNotesChart_LocalDeleteReplyWithFile(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaNotes())
	nc := NewNotesChart(engine)

	note := &model.Note{
		ID:      "n2",
		ReplyID: strPtr("p1"),
		FileIDs: model.StringArray{"f1"},
	}
	require.NoError(t, nc.Update(note, false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["local.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["local.dec"]))
	assert.Equal(t, int64(-1), toInt64(row.Cols["local.diffs.reply"]))
	assert.Equal(t, int64(-1), toInt64(row.Cols["local.diffs.withFile"]))
	// reply 持ちのノートは normal にカウントしない (engine seed の 0 のまま)
	assert.Equal(t, int64(0), toInt64(row.Cols["local.diffs.normal"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.diffs.renote"]))
}

func TestNotesChart_RemoteRenote(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaNotes())
	nc := NewNotesChart(engine)

	note := &model.Note{
		ID:       "n3",
		RenoteID: strPtr("orig"),
		UserHost: strPtr("example.com"),
	}
	require.NoError(t, nc.Update(note, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["remote.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["remote.inc"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["remote.diffs.renote"]))
	// remote ルートなので local 列はすべて 0 のまま
	assert.Equal(t, int64(0), toInt64(row.Cols["local.total"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.inc"]))
}
