package charts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestPerUserReactionChart_LocalReactor(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserReaction())
	pc := NewPerUserReactionChart(engine)
	require.Same(t, engine, pc.Chart())

	reactor := &model.User{ID: "alice"}
	note := &model.Note{ID: "n1", UserID: "owner"}
	require.NoError(t, pc.Update(reactor, note))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["owner"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["local.count"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["remote.count"]))
}

func TestPerUserReactionChart_RemoteReactor(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserReaction())
	pc := NewPerUserReactionChart(engine)

	reactor := &model.User{ID: "bob", Host: strPtr("example.com")}
	note := &model.Note{ID: "n2", UserID: "owner2"}
	require.NoError(t, pc.Update(reactor, note))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["owner2"][0]
	assert.Equal(t, int64(0), toInt64(row.Cols["local.count"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["remote.count"]))
}
