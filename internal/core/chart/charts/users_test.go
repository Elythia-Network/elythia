package charts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestUsersChart_LocalCreate(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaUsers())
	uc := NewUsersChart(engine)
	require.Same(t, engine, uc.Chart())

	require.NoError(t, uc.Update(&model.User{ID: "u1"}, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["local.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["local.inc"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.dec"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["remote.total"]))
}

func TestUsersChart_RemoteDelete(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaUsers())
	uc := NewUsersChart(engine)

	user := &model.User{ID: "u2", Host: strPtr("example.com")}
	require.NoError(t, uc.Update(user, false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["remote.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["remote.dec"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["remote.inc"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.total"]))
}

func TestUsersChart_EmptyHostIsLocal(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaUsers())
	uc := NewUsersChart(engine)

	user := &model.User{ID: "u3", Host: strPtr("")}
	require.NoError(t, uc.Update(user, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["local.total"]))
}
