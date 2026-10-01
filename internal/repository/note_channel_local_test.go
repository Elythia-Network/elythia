package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/shiroha-a/mk/internal/model"
)

// channels/timeline の匿名 visitor + ugcVisibilityForVisitor='local' (upstream
// generateUgcVisibilityQueryForVisitor の `note.userHost IS NULL`) を LIMIT 前に
// 掛けていることを固定する。remote の行を LIMIT の後で落とすとページが
// 過少充填されるので、limit=1 で local の行が返ることまで見る。
func TestNoteRepository_ListLocalByChannelID(t *testing.T) {
	repo := NewNoteRepository(testDB)
	chRepo := NewChannelRepository(testDB)

	local := insertTestUser(t, "u_clo_l", "clolocal")
	t.Cleanup(func() { cleanupUser(t, local.ID) })
	remote := insertRemoteTestUser(t, "u_clo_r", "cloremote", "remote.example")
	t.Cleanup(func() { cleanupUser(t, remote.ID) })

	uid := local.ID
	ch := newTestChannel("ch_clo_1", "channel-local-only", &uid)
	require.NoError(t, chRepo.Create(ch))
	t.Cleanup(func() { cleanupChannel(t, ch.ID) })

	cid := ch.ID
	host := "remote.example"
	// ID 順で remote が新しい側に来るように並べる (newest-first で remote が先頭)。
	notes := []*model.Note{
		{ID: "n_clo_1", UserID: local.ID, ChannelID: &cid, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_clo_2", UserID: remote.ID, UserHost: &host, ChannelID: &cid, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, n.ID) })
	}

	rows, err := repo.ListByChannelID(ch.ID, "", "", "", 50)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "絞らない版は remote の行も返す")

	rows, err = repo.ListLocalByChannelID(ch.ID, "", "", "", 1)
	require.NoError(t, err)
	require.Len(t, rows, 1, "remote の行を LIMIT 前に落とすので、limit=1 でも local の行で埋まる")
	assert.Equal(t, "n_clo_1", rows[0].ID)

	// 可視性の push-down も残っていること。
	fol := &model.Note{ID: "n_clo_3", UserID: local.ID, ChannelID: &cid, Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}"))}
	require.NoError(t, repo.Create(fol))
	t.Cleanup(func() { cleanupNote(t, fol.ID) })
	rows, err = repo.ListLocalByChannelID(ch.ID, "", "", "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 1, "匿名には followers の行を出さない")
	assert.Equal(t, "n_clo_1", rows[0].ID)
}
