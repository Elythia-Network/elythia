package charts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestPerUserNotesChart_NormalCreate(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserNotes())
	pc := NewPerUserNotesChart(engine)
	require.Same(t, engine, pc.Chart())

	require.NoError(t, pc.Update("u1", &model.Note{ID: "n1"}, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["u1"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["inc"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["diffs.normal"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["diffs.reply"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["diffs.renote"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["diffs.withFile"]))
}

func TestPerUserNotesChart_RenoteWithFileDelete(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserNotes())
	pc := NewPerUserNotesChart(engine)

	note := &model.Note{
		ID:       "n2",
		RenoteID: strPtr("orig"),
		FileIDs:  model.StringArray{"f1"},
	}
	require.NoError(t, pc.Update("u2", note, false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["u2"][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["dec"]))
	assert.Equal(t, int64(-1), toInt64(row.Cols["diffs.renote"]))
	assert.Equal(t, int64(-1), toInt64(row.Cols["diffs.withFile"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["diffs.normal"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["diffs.reply"]))
}

func TestPerUserNotesChart_ReplyCreate(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserNotes())
	pc := NewPerUserNotesChart(engine)

	note := &model.Note{
		ID:      "n3",
		ReplyID: strPtr("p1"),
	}
	require.NoError(t, pc.Update("u3", note, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["u3"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["diffs.reply"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["diffs.normal"]))
}
