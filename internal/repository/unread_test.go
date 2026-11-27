package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteUnreadRepository_UpsertAndQueries(t *testing.T) {
	repo := NewNoteUnreadRepository(testDB)
	author := insertTestUser(t, "nu_author", "nuauthor")
	defer cleanupUser(t, author.ID)
	reader := insertTestUser(t, "nu_reader", "nureader")
	defer cleanupUser(t, reader.ID)

	note := &model.Note{ID: "nu_note_1", UserID: author.ID, Visibility: "specified"}
	require.NoError(t, testDB.Create(note).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID)
	defer testDB.Exec(`DELETE FROM "note_unread" WHERE "userId" = ?`, reader.ID)

	// 初回 insert: isSpecified=true / isMentioned=false
	require.NoError(t, repo.Upsert(&model.NoteUnread{
		ID: "nu_1", UserID: reader.ID, NoteID: note.ID, NoteUserID: author.ID,
		IsSpecified: true, IsMentioned: false,
	}))

	has, err := repo.HasAnySpecified(reader.ID)
	require.NoError(t, err)
	assert.True(t, has)
	hasM, err := repo.HasAnyMentioned(reader.ID)
	require.NoError(t, err)
	assert.False(t, hasM)

	// Upsert: 同一 (userId, noteId) で isMentioned=true → flag が OR で結合
	require.NoError(t, repo.Upsert(&model.NoteUnread{
		ID: "nu_dup", UserID: reader.ID, NoteID: note.ID, NoteUserID: author.ID,
		IsSpecified: false, IsMentioned: true,
	}))

	has, _ = repo.HasAnySpecified(reader.ID)
	assert.True(t, has, "isSpecified は OR 結合で true のまま")
	hasM, _ = repo.HasAnyMentioned(reader.ID)
	assert.True(t, hasM, "isMentioned は true に昇格")

	// delete 後は has=false
	require.NoError(t, repo.DeleteByUserNote(reader.ID, note.ID))
	has, _ = repo.HasAnySpecified(reader.ID)
	assert.False(t, has)
	hasM, _ = repo.HasAnyMentioned(reader.ID)
	assert.False(t, hasM)
}

func TestChannelNoteUnreadRepository_CreateMany_DeduplicatesOnConflict(t *testing.T) {
	// #320: CreateMany が (userId, channelId, noteId) unique 制約違反時に
	// silently skip (ON CONFLICT DO NOTHING) することを実 DB で確認する。
	repo := NewChannelNoteUnreadRepository(testDB)
	userRepo := NewUserRepository(testDB)
	author := insertTestUser(t, "cmu_a", "cmauthor")
	defer cleanupUser(t, author.ID)
	reader := insertTestUser(t, "cmu_r", "cmreader")
	defer cleanupUser(t, reader.ID)
	_ = userRepo

	chRepo := NewChannelRepository(testDB)
	ownerID := author.ID
	ch := newTestChannel("ch_cmany", "cm target", &ownerID)
	require.NoError(t, chRepo.Create(ch))
	defer cleanupChannel(t, ch.ID)

	note := &model.Note{ID: "n_cm_1", UserID: author.ID, Visibility: "public"}
	require.NoError(t, testDB.Create(note).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID)
	defer testDB.Exec(`DELETE FROM "channel_note_unread" WHERE "userId" = ?`, reader.ID)

	rows := []*model.ChannelNoteUnread{
		{ID: "cnu_1", UserID: reader.ID, ChannelID: ch.ID, NoteID: note.ID},
		{ID: "cnu_2", UserID: reader.ID, ChannelID: ch.ID, NoteID: note.ID}, // duplicate
	}
	require.NoError(t, repo.CreateMany(rows))

	has, err := repo.HasAnyByUser(reader.ID)
	require.NoError(t, err)
	assert.True(t, has)

	// 2 回目の CreateMany (完全に同じ内容) でも error にならない。
	require.NoError(t, repo.CreateMany(rows))

	// 空 slice は no-op。
	require.NoError(t, repo.CreateMany(nil))
	require.NoError(t, repo.CreateMany([]*model.ChannelNoteUnread{}))
}

func TestNoteUnreadRepository_HasAny_Empty(t *testing.T) {
	repo := NewNoteUnreadRepository(testDB)
	has, err := repo.HasAnySpecified("no-such-user")
	require.NoError(t, err)
	assert.False(t, has)
	hasM, err := repo.HasAnyMentioned("no-such-user")
	require.NoError(t, err)
	assert.False(t, hasM)
}

// DeleteAllByUser removes every note_unread row of the user and only those
// (#4273: it is now a single raw DELETE, so the WHERE must stay scoped).
func TestNoteUnreadRepository_DeleteAllByUser(t *testing.T) {
	repo := NewNoteUnreadRepository(testDB)
	author := insertTestUser(t, "nu_da_author", "nudaauthor")
	defer cleanupUser(t, author.ID)
	reader := insertTestUser(t, "nu_da_reader", "nudareader")
	defer cleanupUser(t, reader.ID)
	other := insertTestUser(t, "nu_da_other", "nudaother")
	defer cleanupUser(t, other.ID)

	for _, id := range []string{"nu_da_n1", "nu_da_n2"} {
		require.NoError(t, testDB.Create(&model.Note{ID: id, UserID: author.ID, Visibility: "specified"}).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id)
	}
	defer testDB.Exec(`DELETE FROM "note_unread" WHERE "userId" IN (?, ?)`, reader.ID, other.ID)
	for _, row := range []*model.NoteUnread{
		{ID: "nu_da_1", UserID: reader.ID, NoteID: "nu_da_n1", NoteUserID: author.ID, IsSpecified: true},
		{ID: "nu_da_2", UserID: reader.ID, NoteID: "nu_da_n2", NoteUserID: author.ID, IsMentioned: true},
		{ID: "nu_da_3", UserID: other.ID, NoteID: "nu_da_n1", NoteUserID: author.ID, IsSpecified: true},
	} {
		require.NoError(t, repo.Upsert(row))
	}

	require.NoError(t, repo.DeleteAllByUser(reader.ID))
	var left int64
	require.NoError(t, testDB.Raw(`SELECT count(*) FROM "note_unread" WHERE "userId" = ?`, reader.ID).Scan(&left).Error)
	assert.Zero(t, left, "every row of the reader must be deleted")
	has, err := repo.HasAnySpecified(other.ID)
	require.NoError(t, err)
	assert.True(t, has, "another user's rows must be kept")

	// 行が無くてもエラーにしない (i/notifications の暗黙既読は毎回これを呼ぶ)。
	require.NoError(t, repo.DeleteAllByUser(reader.ID))
}
