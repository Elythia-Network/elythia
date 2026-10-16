package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eachMeUnreadFlags reads MeUnreadFlags with the per-table queries that
// /api/i used before the one-statement reader (#3608).
func eachMeUnreadFlags(t *testing.T, userID string) MeUnreadFlags {
	t.Helper()
	fr, err := NewFollowRequestRepository(testDB).CountReceived(userID)
	require.NoError(t, err)
	chat, err := NewChatRepository(testDB).CountUnread(userID)
	require.NoError(t, err)
	antenna, err := NewAntennaNoteUnreadRepository(testDB).HasAnyByUser(userID)
	require.NoError(t, err)
	channel, err := NewChannelNoteUnreadRepository(testDB).HasAnyByUser(userID)
	require.NoError(t, err)
	specified, err := NewNoteUnreadRepository(testDB).HasAnySpecified(userID)
	require.NoError(t, err)
	return MeUnreadFlags{
		HasUnreadSpecifiedNotes:         specified,
		HasPendingReceivedFollowRequest: fr > 0,
		HasUnreadChatMessages:           chat > 0,
		HasUnreadAntenna:                antenna,
		HasUnreadChannel:                channel,
	}
}

// Each flag turns on only for rows that the per-table query counts: a
// received (not sent) follow request, an unread (not already read, not sent)
// chat message, and the user's own antenna / channel unread rows.
func TestMeUnreadFlagsRepository_MatchesPerTableQueries(t *testing.T) {
	repo := NewMeUnreadFlagsRepository(testDB)
	me := insertTestUser(t, "mu_me", "mume")
	defer cleanupUser(t, me.ID)
	other := insertTestUser(t, "mu_other", "muother")
	defer cleanupUser(t, other.ID)

	note := &model.Note{ID: "mu_note", UserID: other.ID, Visibility: "public"}
	require.NoError(t, testDB.Create(note).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID)
	antenna := newTestAntenna("mu_ant", me.ID, "a")
	require.NoError(t, testDB.Create(antenna).Error)
	defer cleanupAntenna(t, antenna.ID)
	otherAntenna := newTestAntenna("mu_ant_o", other.ID, "a")
	require.NoError(t, testDB.Create(otherAntenna).Error)
	defer cleanupAntenna(t, otherAntenna.ID)
	ownerID := other.ID
	ch := newTestChannel("mu_ch", "c", &ownerID)
	require.NoError(t, testDB.Create(ch).Error)
	defer cleanupChannel(t, ch.ID)
	defer func() {
		for _, table := range []string{"follow_request", "chat_message", "antenna_note_unread", "channel_note_unread", "note_unread"} {
			testDB.Exec(`DELETE FROM "` + table + `" WHERE id LIKE 'mu\_%'`)
		}
	}()

	check := func(want MeUnreadFlags, msg string) {
		t.Helper()
		got, err := repo.ReadMeUnreadFlags(me.ID)
		require.NoError(t, err)
		assert.Equal(t, want, got, msg)
		assert.Equal(t, eachMeUnreadFlags(t, me.ID), got, msg+" (per-table)")
	}
	var want MeUnreadFlags
	check(want, "nothing")

	// 自分が送ったフォロー申請・自分が送ったチャット・既読のチャット・他人の
	// アンテナとチャンネルの未読は数えない。
	require.NoError(t, testDB.Create(&model.FollowRequest{ID: "mu_fr_sent", FollowerID: me.ID, FolloweeID: other.ID}).Error)
	meID, otherID := me.ID, other.ID
	require.NoError(t, testDB.Create(&model.ChatMessage{ID: "mu_chat_sent", FromUserID: me.ID, ToUserID: &otherID}).Error)
	require.NoError(t, testDB.Create(&model.ChatMessage{ID: "mu_chat_read", FromUserID: other.ID, ToUserID: &meID, Reads: model.StringArray{me.ID}}).Error)
	require.NoError(t, testDB.Create(&model.AntennaNoteUnread{ID: "mu_anu_o", UserID: other.ID, AntennaID: otherAntenna.ID, NoteID: note.ID}).Error)
	require.NoError(t, testDB.Create(&model.ChannelNoteUnread{ID: "mu_cnu_o", UserID: other.ID, ChannelID: ch.ID, NoteID: note.ID}).Error)
	// 言及だけの note_unread と、他人の specified の note_unread も数えない。
	require.NoError(t, testDB.Create(&model.NoteUnread{ID: "mu_nu_mention", UserID: me.ID, NoteID: note.ID, NoteUserID: other.ID, IsMentioned: true}).Error)
	require.NoError(t, testDB.Create(&model.NoteUnread{ID: "mu_nu_o", UserID: other.ID, NoteID: note.ID, NoteUserID: other.ID, IsSpecified: true}).Error)
	check(want, "only rows that are not mine to read")

	require.NoError(t, testDB.Create(&model.FollowRequest{ID: "mu_fr_recv", FollowerID: other.ID, FolloweeID: me.ID}).Error)
	want.HasPendingReceivedFollowRequest = true
	check(want, "received follow request")

	require.NoError(t, testDB.Create(&model.ChatMessage{ID: "mu_chat_unread", FromUserID: other.ID, ToUserID: &meID, Reads: model.StringArray{other.ID}}).Error)
	want.HasUnreadChatMessages = true
	check(want, "unread chat message")

	require.NoError(t, testDB.Create(&model.AntennaNoteUnread{ID: "mu_anu", UserID: me.ID, AntennaID: antenna.ID, NoteID: note.ID}).Error)
	want.HasUnreadAntenna = true
	check(want, "antenna unread")

	require.NoError(t, testDB.Create(&model.ChannelNoteUnread{ID: "mu_cnu", UserID: me.ID, ChannelID: ch.ID, NoteID: note.ID}).Error)
	want.HasUnreadChannel = true
	check(want, "channel unread")

	require.NoError(t, testDB.Exec(`UPDATE "note_unread" SET "isSpecified" = true WHERE id = 'mu_nu_mention'`).Error)
	want.HasUnreadSpecifiedNotes = true
	check(want, "specified note unread")

	// 1 つだけ立っている状態も見る (列の並びを取り違えると別の項目が立つ)。
	testDB.Exec(`DELETE FROM "follow_request" WHERE id = 'mu_fr_recv'`)
	testDB.Exec(`DELETE FROM "antenna_note_unread" WHERE id = 'mu_anu'`)
	testDB.Exec(`DELETE FROM "channel_note_unread" WHERE id = 'mu_cnu'`)
	require.NoError(t, testDB.Exec(`UPDATE "note_unread" SET "isSpecified" = false WHERE id = 'mu_nu_mention'`).Error)
	check(MeUnreadFlags{HasUnreadChatMessages: true}, "chat only")
	testDB.Exec(`DELETE FROM "chat_message" WHERE id = 'mu_chat_unread'`)
	require.NoError(t, testDB.Create(&model.AntennaNoteUnread{ID: "mu_anu", UserID: me.ID, AntennaID: antenna.ID, NoteID: note.ID}).Error)
	check(MeUnreadFlags{HasUnreadAntenna: true}, "antenna only")
	testDB.Exec(`DELETE FROM "antenna_note_unread" WHERE id = 'mu_anu'`)
	require.NoError(t, testDB.Exec(`UPDATE "note_unread" SET "isSpecified" = true WHERE id = 'mu_nu_mention'`).Error)
	require.NoError(t, testDB.Create(&model.ChannelNoteUnread{ID: "mu_cnu", UserID: me.ID, ChannelID: ch.ID, NoteID: note.ID}).Error)
	check(MeUnreadFlags{HasUnreadChannel: true, HasUnreadSpecifiedNotes: true}, "channel and specified")
	testDB.Exec(`DELETE FROM "channel_note_unread" WHERE id = 'mu_cnu'`)
	check(MeUnreadFlags{HasUnreadSpecifiedNotes: true}, "specified only")
}

func TestMeUnreadFlagsRepository_UnstorableAndError(t *testing.T) {
	got, err := NewMeUnreadFlagsRepository(testDB).ReadMeUnreadFlags("a\x00b")
	require.NoError(t, err)
	assert.Equal(t, MeUnreadFlags{}, got)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewMeUnreadFlagsRepository(testDB.WithContext(ctx)).ReadMeUnreadFlags("mu_me")
	assert.Error(t, err)
}
