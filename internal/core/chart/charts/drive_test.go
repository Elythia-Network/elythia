package charts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestDriveChart_LocalUpload(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaDrive())
	dc := NewDriveChart(engine)
	require.Same(t, engine, dc.Chart())

	file := &model.DriveFile{ID: "f1", Size: 5500} // 5500 / 1000 = 5 KB
	require.NoError(t, dc.Update(file, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["local.incCount"]))
	assert.Equal(t, int64(5), toInt64(row.Cols["local.incSize"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.decCount"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["remote.incCount"]))
}

func TestDriveChart_RemoteDelete(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaDrive())
	dc := NewDriveChart(engine)

	file := &model.DriveFile{
		ID:       "f2",
		UserHost: strPtr("example.com"),
		Size:     12345, // 12 KB after kb truncation
	}
	require.NoError(t, dc.Update(file, false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour[""][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["remote.decCount"]))
	assert.Equal(t, int64(12), toInt64(row.Cols["remote.decSize"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["remote.incCount"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["local.incCount"]))
}
