package charts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestPerUserDriveChart_Upload(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserDrive())
	pc := NewPerUserDriveChart(engine)
	require.Same(t, engine, pc.Chart())

	file := &model.DriveFile{
		ID:     "f1",
		UserID: strPtr("u1"),
		Size:   4500, // 4 KB after kb truncation
	}
	require.NoError(t, pc.Update(file, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["u1"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["totalCount"]))
	assert.Equal(t, int64(4), toInt64(row.Cols["totalSize"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["incCount"]))
	assert.Equal(t, int64(4), toInt64(row.Cols["incSize"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["decCount"]))
}

func TestPerUserDriveChart_Delete(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaPerUserDrive())
	pc := NewPerUserDriveChart(engine)

	file := &model.DriveFile{
		ID:     "f2",
		UserID: strPtr("u2"),
		Size:   7000, // 7 KB
	}
	require.NoError(t, pc.Update(file, false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["u2"][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["totalCount"]))
	assert.Equal(t, int64(-7), toInt64(row.Cols["totalSize"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["decCount"]))
	assert.Equal(t, int64(7), toInt64(row.Cols["decSize"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["incCount"]))
}

func TestPerUserDriveChart_NoOwnerError(t *testing.T) {
	engine, _, _ := newTestEngine(t, SchemaPerUserDrive())
	pc := NewPerUserDriveChart(engine)

	// nil UserID
	err := pc.Update(&model.DriveFile{ID: "f3", Size: 1000}, true)
	require.ErrorIs(t, err, ErrPerUserDriveNoOwner)

	// 空文字列 UserID
	err = pc.Update(&model.DriveFile{ID: "f4", UserID: strPtr(""), Size: 1000}, true)
	require.ErrorIs(t, err, ErrPerUserDriveNoOwner)
}
