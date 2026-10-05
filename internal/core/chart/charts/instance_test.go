package charts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestInstanceChart_RequestReceivedAndSent(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)
	require.Same(t, engine, ic.Chart())

	require.NoError(t, ic.RequestReceived("a.example"))
	require.NoError(t, ic.RequestSent("a.example", true))
	require.NoError(t, ic.RequestSent("a.example", false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["a.example"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["requests.received"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["requests.succeeded"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["requests.failed"]))
}

func TestInstanceChart_NewUser(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	require.NoError(t, ic.NewUser("b.example"))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["b.example"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["users.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["users.inc"]))
}

func TestInstanceChart_UpdateNoteAndFollows(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	note := &model.Note{
		ID:       "rn1",
		ReplyID:  strPtr("orig"),
		FileIDs:  model.StringArray{"f1", "f2"},
		UserHost: strPtr("c.example"),
	}
	require.NoError(t, ic.UpdateNote("c.example", note, true))
	require.NoError(t, ic.UpdateFollowing("c.example", true))
	require.NoError(t, ic.UpdateFollowers("c.example", false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["c.example"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["notes.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["notes.diffs.reply"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["notes.diffs.withFile"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["notes.diffs.normal"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["notes.diffs.renote"]))

	assert.Equal(t, int64(1), toInt64(row.Cols["following.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["following.inc"]))
	assert.Equal(t, int64(-1), toInt64(row.Cols["followers.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["followers.dec"]))
}

func TestInstanceChart_UpdateNoteRenoteDelete(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	note := &model.Note{
		ID:       "rn2",
		RenoteID: strPtr("o"),
		UserHost: strPtr("d.example"),
	}
	require.NoError(t, ic.UpdateNote("d.example", note, false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["d.example"][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["notes.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["notes.dec"]))
	assert.Equal(t, int64(-1), toInt64(row.Cols["notes.diffs.renote"]))
}

func TestInstanceChart_UpdateDriveRemote(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	file := &model.DriveFile{
		ID:       "rf1",
		Size:     8000, // 8 KB
		UserHost: strPtr("e.example"),
	}
	require.NoError(t, ic.UpdateDrive(file, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["e.example"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["drive.totalFiles"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["drive.incFiles"]))
	assert.Equal(t, int64(8), toInt64(row.Cols["drive.incUsage"]))
}

func TestInstanceChart_UpdateDriveDelete(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	file := &model.DriveFile{
		ID:       "rf2",
		Size:     2500, // 2 KB
		UserHost: strPtr("f.example"),
	}
	require.NoError(t, ic.UpdateDrive(file, false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["f.example"][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["drive.totalFiles"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["drive.decFiles"]))
	assert.Equal(t, int64(2), toInt64(row.Cols["drive.decUsage"]))
}

func TestInstanceChart_UpdateNoteNormalCreate(t *testing.T) {
	// reply / renote / file いずれも持たない通常ノートで diffs.normal を踏ませる
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	note := &model.Note{
		ID:       "rn3",
		UserHost: strPtr("g.example"),
	}
	require.NoError(t, ic.UpdateNote("g.example", note, true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["g.example"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["notes.diffs.normal"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["notes.diffs.reply"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["notes.diffs.renote"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["notes.diffs.withFile"]))
}

func TestInstanceChart_UpdateFollowingUnfollow(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	require.NoError(t, ic.UpdateFollowing("h.example", false))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["h.example"][0]
	assert.Equal(t, int64(-1), toInt64(row.Cols["following.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["following.dec"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["following.inc"]))
}

func TestInstanceChart_UpdateFollowersFollow(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	require.NoError(t, ic.UpdateFollowers("i.example", true))
	require.NoError(t, engine.Save(context.Background()))

	row := repo.hour["i.example"][0]
	assert.Equal(t, int64(1), toInt64(row.Cols["followers.total"]))
	assert.Equal(t, int64(1), toInt64(row.Cols["followers.inc"]))
	assert.Equal(t, int64(0), toInt64(row.Cols["followers.dec"]))
}

func TestInstanceChart_UpdateDriveLocalSkipped(t *testing.T) {
	engine, repo, _ := newTestEngine(t, SchemaInstance())
	ic := NewInstanceChart(engine)

	require.NoError(t, ic.UpdateDrive(&model.DriveFile{ID: "lf1", Size: 1000}, true))
	require.NoError(t, ic.UpdateDrive(&model.DriveFile{ID: "lf2", Size: 1000, UserHost: strPtr("")}, true))
	require.NoError(t, engine.Save(context.Background()))

	// local file は instance chart の対象外なので row が一切作られない
	assert.Empty(t, repo.hour)
	assert.Empty(t, repo.day)
}
