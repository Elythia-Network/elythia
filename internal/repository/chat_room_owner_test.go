package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChatRepository_RoomListsLoadOwner pins that every query whose rooms are
// packed as ChatRoom (owner is required in upstream packedChatRoomSchema) loads
// the room owner (#3438).
//
// packRoom は Owner が無いと owner を省く。どの一覧で読み込みが抜けても応答から
// owner が落ちるので、経路ごとに確かめる。
func TestChatRepository_RoomListsLoadOwner(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_chat_ow1", "chatow1")
	member := insertTestUser(t, "u_chat_ow2", "chatow2")
	defer cleanupUser(t, owner.ID)
	defer cleanupUser(t, member.ID)

	room := &model.ChatRoom{ID: "cr_ow1", Name: "OwnerRoom", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	mem := &model.ChatRoomMembership{ID: "crm_ow1", UserID: member.ID, RoomID: room.ID}
	require.NoError(t, repo.CreateMembership(mem))
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE id = ?`, mem.ID)

	inv := &model.ChatRoomInvitation{ID: "inv_ow1", UserID: member.ID, RoomID: room.ID}
	require.NoError(t, repo.CreateInvitation(inv))
	defer testDB.Exec(`DELETE FROM "chat_room_invitation" WHERE id = ?`, inv.ID)

	text := "hi"
	fileID := "file_ow1"
	msg := &model.ChatMessage{
		ID: "cm_ow1", FromUserID: member.ID, ToRoomID: &room.ID, Text: &text, FileID: &fileID,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(msg))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msg.ID)

	requireOwner := func(t *testing.T, r *model.ChatRoom) {
		t.Helper()
		require.NotNil(t, r, "room must be loaded")
		require.NotNil(t, r.Owner, "room owner must be preloaded")
		assert.Equal(t, owner.ID, r.Owner.ID)
	}

	t.Run("ListRoomsByOwner", func(t *testing.T) {
		rooms, err := repo.ListRoomsByOwner(owner.ID, "", "", 30)
		require.NoError(t, err)
		require.Len(t, rooms, 1)
		requireOwner(t, rooms[0])
	})
	t.Run("ListJoinedRooms", func(t *testing.T) {
		rooms, err := repo.ListJoinedRooms(member.ID, "", "", 30)
		require.NoError(t, err)
		require.Len(t, rooms, 1)
		requireOwner(t, rooms[0])
	})
	t.Run("ListInvitationsByUser", func(t *testing.T) {
		rows, err := repo.ListInvitationsByUser(member.ID, false, "", "", 30)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		requireOwner(t, rows[0].Room)
	})
	t.Run("ListMessagesByFileID", func(t *testing.T) {
		msgs, err := repo.ListMessagesByFileID(fileID, "", "", 10)
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		requireOwner(t, msgs[0].ToRoom)
	})
	t.Run("ListHistory", func(t *testing.T) {
		msgs, err := repo.ListHistory(member.ID, 10)
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		requireOwner(t, msgs[0].ToRoom)
	})
	t.Run("ListRoomHistory", func(t *testing.T) {
		msgs, err := repo.ListRoomHistory(member.ID, 10)
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		requireOwner(t, msgs[0].ToRoom)
	})
}

// TestChatRepository_FindMessageDetailedByID pins that the chat/messages/show
// lookup loads the sender, the attached file and the recipient of a direct
// message, and the room (with owner) of a room message (#3438).
func TestChatRepository_FindMessageDetailedByID(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_chat_md1", "chatmd1")
	other := insertTestUser(t, "u_chat_md2", "chatmd2")
	defer cleanupUser(t, owner.ID)
	defer cleanupUser(t, other.ID)

	room := &model.ChatRoom{ID: "cr_md1", Name: "R", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	file := newTestDriveFile("df_md1", owner.ID, "md5_md1", nil)
	require.NoError(t, testDB.Create(file).Error)
	defer testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, file.ID)

	text := "hi"
	dm := &model.ChatMessage{ID: "cm_md1", FromUserID: owner.ID, ToUserID: &other.ID, Text: &text, FileID: &file.ID,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{}}
	rm := &model.ChatMessage{ID: "cm_md2", FromUserID: other.ID, ToRoomID: &room.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{}}
	for _, m := range []*model.ChatMessage{dm, rm} {
		require.NoError(t, repo.CreateMessage(m))
		defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, m.ID)
	}

	got, err := repo.FindMessageDetailedByID(dm.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FromUser, "the sender must be loaded")
	assert.Equal(t, owner.ID, got.FromUser.ID)
	require.NotNil(t, got.File, "the attached file must be loaded (show returns it as file)")
	assert.Equal(t, file.ID, got.File.ID)
	require.NotNil(t, got.ToUser, "the recipient must be loaded")
	assert.Equal(t, other.ID, got.ToUser.ID)

	got, err = repo.FindMessageDetailedByID(rm.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ToRoom, "the room must be loaded")
	require.NotNil(t, got.ToRoom.Owner, "the room owner must be loaded")
	assert.Equal(t, owner.ID, got.ToRoom.Owner.ID)

	_, err = repo.FindMessageDetailedByID("ghost")
	assert.Error(t, err)
	_, err = repo.FindMessageDetailedByID(string([]byte{0}))
	assert.ErrorIs(t, err, ErrNotFound)
}
