package repository

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatRepository_Rooms(t *testing.T) {
	repo := NewChatRepository(testDB)
	user := insertTestUser(t, "u_chat_1", "chatuser1")
	defer cleanupUser(t, user.ID)

	// CreateRoom
	room := &model.ChatRoom{ID: "cr_1", Name: "Test Room", OwnerID: user.ID, Description: "desc"}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	// FindRoomByID
	found, err := repo.FindRoomByID("cr_1")
	require.NoError(t, err)
	assert.Equal(t, "Test Room", found.Name)

	// FindRoomByID - not found
	_, err = repo.FindRoomByID("ghost")
	assert.Error(t, err)

	// UpdateRoom
	found.Name = "Updated"
	require.NoError(t, repo.UpdateRoom(found))

	// ListRoomsByOwner
	rooms, err := repo.ListRoomsByOwner(user.ID, "", "", 30)
	require.NoError(t, err)
	assert.Len(t, rooms, 1)
	assert.Equal(t, "Updated", rooms[0].Name)

	// ListJoinedRooms (empty - no membership yet)
	joined, err := repo.ListJoinedRooms(user.ID, "", "", 30)
	require.NoError(t, err)
	assert.Empty(t, joined)

	// DeleteRoom
	require.NoError(t, repo.DeleteRoom("cr_1"))
	_, err = repo.FindRoomByID("cr_1")
	assert.Error(t, err)
}

func TestChatRepository_Messages(t *testing.T) {
	repo := NewChatRepository(testDB)
	user1 := insertTestUser(t, "u_chat_2", "chatuser2")
	user2 := insertTestUser(t, "u_chat_3", "chatuser3")
	defer cleanupUser(t, user1.ID)
	defer cleanupUser(t, user2.ID)

	room := &model.ChatRoom{ID: "cr_2", Name: "Room", OwnerID: user1.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	// CreateMessage (room message)
	msg := &model.ChatMessage{
		ID: "cm_1", FromUserID: user1.ID, ToRoomID: &room.ID,
		Reads: model.StringArray{}, Reactions: model.StringArray{},
	}
	text := "hello"
	msg.Text = &text
	require.NoError(t, repo.CreateMessage(msg))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msg.ID)

	// FindMessageByID
	found, err := repo.FindMessageByID("cm_1")
	require.NoError(t, err)
	assert.Equal(t, "hello", *found.Text)

	// FindMessageByID - not found
	_, err = repo.FindMessageByID("ghost")
	assert.Error(t, err)

	// ListMessagesByRoom
	msgs, err := repo.ListMessagesByRoom(room.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, msgs, 1)

	// ListMessagesByRoom - default limit
	msgs2, err := repo.ListMessagesByRoom(room.ID, "", "", 0)
	require.NoError(t, err)
	assert.Len(t, msgs2, 1)

	// CreateMessage (DM)
	dm := &model.ChatMessage{
		ID: "cm_2", FromUserID: user1.ID, ToUserID: &user2.ID,
		Reads: model.StringArray{}, Reactions: model.StringArray{},
	}
	dmText := "dm"
	dm.Text = &dmText
	require.NoError(t, repo.CreateMessage(dm))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, dm.ID)

	// ListMessagesByUser
	dms, err := repo.ListMessagesByUser(user1.ID, user2.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, dms, 1)

	// ListMessagesByUser - default limit
	dms2, err := repo.ListMessagesByUser(user1.ID, user2.ID, "", "", 0)
	require.NoError(t, err)
	assert.Len(t, dms2, 1)

	// SearchMessages - default scope (own 1-on-1 + member/owned rooms)
	results, err := repo.SearchMessages(user1.ID, "dm", 10, "", "")
	require.NoError(t, err)
	assert.Len(t, results, 1)

	// SearchMessages - default limit
	results2, err := repo.SearchMessages(user1.ID, "dm", 0, "", "")
	require.NoError(t, err)
	assert.Len(t, results2, 1)

	// SearchMessages - userId scope (1-on-1 with user2)
	byUser, err := repo.SearchMessages(user1.ID, "dm", 10, user2.ID, "")
	require.NoError(t, err)
	assert.Len(t, byUser, 1)

	// SearchMessages - roomId scope (room message "hello")
	byRoom, err := repo.SearchMessages(user1.ID, "hello", 10, "", room.ID)
	require.NoError(t, err)
	assert.Len(t, byRoom, 1)

	// SearchMessages - default scope finds the owned-room message too
	byOwned, err := repo.SearchMessages(user1.ID, "hello", 10, "", "")
	require.NoError(t, err)
	assert.Len(t, byOwned, 1)

	// HasUnreadFromUser: user2 has an unread DM (cm_2) from user1
	hasU, err := repo.HasUnreadFromUser(user2.ID, user1.ID)
	require.NoError(t, err)
	assert.True(t, hasU)

	// HasUnreadInRoom: user2 has an unread room message (cm_1, authored by user1)
	hasR, err := repo.HasUnreadInRoom(user2.ID, room.ID)
	require.NoError(t, err)
	assert.True(t, hasR)

	// the author (user1) never has their own room message counted as unread
	hasROwn, err := repo.HasUnreadInRoom(user1.ID, room.ID)
	require.NoError(t, err)
	assert.False(t, hasROwn)

	// MarkRead
	require.NoError(t, repo.MarkRead(user2.ID, "cm_2"))

	// once read, the DM no longer counts as unread
	hasU2, err := repo.HasUnreadFromUser(user2.ID, user1.ID)
	require.NoError(t, err)
	assert.False(t, hasU2)

	// CountUnread
	count, err := repo.CountUnread(user2.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	// DeleteMessage
	require.NoError(t, repo.DeleteMessage("cm_1"))
}

// #1747: id-cursor pagination on the chat list endpoints.
func TestChatRepository_Pagination(t *testing.T) {
	repo := NewChatRepository(testDB)
	user := insertTestUser(t, "u_chat_pg", "chatpg")
	defer cleanupUser(t, user.ID)

	for _, id := range []string{"pg_r1", "pg_r2", "pg_r3"} {
		require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: id, Name: "p", OwnerID: user.ID}))
		defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, id)
	}

	// limit=2 → newest 2 (id 降順 pg_r3, pg_r2)
	page, err := repo.ListRoomsByOwner(user.ID, "", "", 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "pg_r3", page[0].ID)
	assert.Equal(t, "pg_r2", page[1].ID)

	// untilId=pg_r3 → id < pg_r3
	older, err := repo.ListRoomsByOwner(user.ID, "", "pg_r3", 30)
	require.NoError(t, err)
	require.Len(t, older, 2)
	assert.Equal(t, "pg_r2", older[0].ID)

	// sinceId=pg_r1 → id > pg_r1
	newer, err := repo.ListRoomsByOwner(user.ID, "pg_r1", "", 30)
	require.NoError(t, err)
	assert.Len(t, newer, 2)
}

func TestChatRepository_Membership(t *testing.T) {
	repo := NewChatRepository(testDB)
	user := insertTestUser(t, "u_chat_4", "chatuser4")
	defer cleanupUser(t, user.ID)

	room := &model.ChatRoom{ID: "cr_3", Name: "Room", OwnerID: user.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	// CreateMembership
	mem := &model.ChatRoomMembership{ID: "mem_1", UserID: user.ID, RoomID: room.ID}
	require.NoError(t, repo.CreateMembership(mem))
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE id = ?`, mem.ID)

	// FindMembership
	found, err := repo.FindMembership(user.ID, room.ID)
	require.NoError(t, err)
	assert.Equal(t, false, found.IsMuted)

	// FindMembership - not found
	_, err = repo.FindMembership("ghost", room.ID)
	assert.Error(t, err)

	// UpdateMembership
	found.IsMuted = true
	require.NoError(t, repo.UpdateMembership(found))

	// ListMembersByRoom
	members, err := repo.ListMembersByRoom(room.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1)

	// ListJoinedRooms (now has membership)
	joined, err := repo.ListJoinedRooms(user.ID, "", "", 30)
	require.NoError(t, err)
	assert.Len(t, joined, 1)

	// ListMembershipsByUser returns the membership rows with Room (+ Owner) populated.
	memberships, err := repo.ListMembershipsByUser(user.ID, "", "", 30)
	require.NoError(t, err)
	require.Len(t, memberships, 1)
	assert.Equal(t, mem.ID, memberships[0].ID)
	assert.Equal(t, room.ID, memberships[0].RoomID)
	require.NotNil(t, memberships[0].Room, "Room must be eager-loaded")
	assert.Equal(t, room.ID, memberships[0].Room.ID)
	require.NotNil(t, memberships[0].Room.Owner, "Room.Owner must be eager-loaded (required UserLite)")
	assert.Equal(t, user.ID, memberships[0].Room.Owner.ID)

	// 2 件目を別 room に追加し、id 降順 (新しい順) で返ることを確認する。
	room2 := &model.ChatRoom{ID: "cr_3b", Name: "Room2", OwnerID: user.ID}
	require.NoError(t, repo.CreateRoom(room2))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room2.ID)
	mem2 := &model.ChatRoomMembership{ID: "mem_2", UserID: user.ID, RoomID: room2.ID}
	require.NoError(t, repo.CreateMembership(mem2))
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE id = ?`, mem2.ID)
	ordered, err := repo.ListMembershipsByUser(user.ID, "", "", 30)
	require.NoError(t, err)
	require.Len(t, ordered, 2)
	assert.Equal(t, "mem_2", ordered[0].ID, "id 降順で新しい membership が先頭")
	assert.Equal(t, "mem_1", ordered[1].ID)

	// DeleteMembership
	require.NoError(t, repo.DeleteMembership(user.ID, room.ID))
}

func TestChatRepository_Reactions(t *testing.T) {
	repo := NewChatRepository(testDB)
	user1 := insertTestUser(t, "u_chat_rx1", "chatrx1")
	user2 := insertTestUser(t, "u_chat_rx2", "chatrx2")
	defer cleanupUser(t, user1.ID)
	defer cleanupUser(t, user2.ID)

	msg := &model.ChatMessage{
		ID: "cm_rx", FromUserID: user1.ID, ToUserID: &user2.ID,
		Reads: model.StringArray{}, Reactions: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(msg))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msg.ID)

	// AddReaction
	require.NoError(t, repo.AddReaction(msg.ID, user1.ID+"/👍"))
	require.NoError(t, repo.AddReaction(msg.ID, user2.ID+"/❤️"))

	found, _ := repo.FindMessageByID(msg.ID)
	assert.Len(t, found.Reactions, 2)

	// RemoveReaction
	removed, err := repo.RemoveReaction(msg.ID, user1.ID+"/👍")
	require.NoError(t, err)
	assert.True(t, removed, "実在するリアクションを消せていない")
	found, _ = repo.FindMessageByID(msg.ID)
	assert.Len(t, found.Reactions, 1)

	// **持っていないものを消そうとしたら false。** 呼び出し側はこれを見て
	// stream への publish を決めるので (#3037)、常に true を返すと
	// 非参加者によるイベント注入が塞がらない。
	removed, err = repo.RemoveReaction(msg.ID, user1.ID+"/👍")
	require.NoError(t, err)
	assert.False(t, removed, "既に無いリアクションを消したと報告している")

	removed, err = repo.RemoveReaction(msg.ID, "someone-else/🎉")
	require.NoError(t, err)
	assert.False(t, removed, "他人のリアクションを消したと報告している")

	// 存在しないメッセージ。
	removed, err = repo.RemoveReaction("no-such-message", user2.ID+"/❤️")
	require.NoError(t, err)
	assert.False(t, removed)
}

func TestChatRepository_DeliveryStatus(t *testing.T) {
	repo := NewChatRepository(testDB)
	user1 := insertTestUser(t, "u_chat_ds1", "chatds1")
	user2 := insertTestUser(t, "u_chat_ds2", "chatds2")
	defer cleanupUser(t, user1.ID)
	defer cleanupUser(t, user2.ID)

	msg := &model.ChatMessage{
		ID: "cm_ds", FromUserID: user1.ID, ToUserID: &user2.ID,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(msg))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msg.ID)

	require.NoError(t, repo.UpdateDeliveryStatus(msg.ID, true, false))
	found, _ := repo.FindMessageByID(msg.ID)
	assert.True(t, found.IsDelivering)
	assert.False(t, found.IsDeliverFailed)

	require.NoError(t, repo.UpdateDeliveryStatus(msg.ID, false, true))
	found, _ = repo.FindMessageByID(msg.ID)
	assert.False(t, found.IsDelivering)
	assert.True(t, found.IsDeliverFailed)
}

func TestChatRepository_MarkAllRead(t *testing.T) {
	repo := NewChatRepository(testDB)
	user1 := insertTestUser(t, "u_chat_mr1", "chatmr1")
	user2 := insertTestUser(t, "u_chat_mr2", "chatmr2")
	defer cleanupUser(t, user1.ID)
	defer cleanupUser(t, user2.ID)

	text := "hi"
	for _, id := range []string{"cm_mr1", "cm_mr2"} {
		m := &model.ChatMessage{
			ID: id, FromUserID: user1.ID, ToUserID: &user2.ID, Text: &text,
			Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
		}
		require.NoError(t, repo.CreateMessage(m))
		defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, id)
	}

	count, _ := repo.CountUnread(user2.ID)
	assert.EqualValues(t, 2, count)

	require.NoError(t, repo.MarkAllRead(user2.ID))
	count, _ = repo.CountUnread(user2.ID)
	assert.EqualValues(t, 0, count)
}

func TestChatRepository_ListHistory(t *testing.T) {
	repo := NewChatRepository(testDB)
	me := insertTestUser(t, "u_chat_h1", "chath1")
	other1 := insertTestUser(t, "u_chat_h2", "chath2")
	other2 := insertTestUser(t, "u_chat_h3", "chath3")
	defer cleanupUser(t, me.ID)
	defer cleanupUser(t, other1.ID)
	defer cleanupUser(t, other2.ID)

	// ownerはmembershipレコードなしでも暗黙メンバーとして扱われることを検証する
	// (明示的なCreateMembershipを入れない)。
	room := &model.ChatRoom{ID: "cr_h1", Name: "HistRoom", OwnerID: me.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	text := "msg"
	// DM: me -> other1 (2件、最新はcm_h2)
	for _, id := range []string{"cm_h1", "cm_h2"} {
		m := &model.ChatMessage{
			ID: id, FromUserID: me.ID, ToUserID: &other1.ID, Text: &text,
			Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
		}
		require.NoError(t, repo.CreateMessage(m))
		defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, id)
	}
	// DM: me -> other2
	dm3 := &model.ChatMessage{
		ID: "cm_h3", FromUserID: me.ID, ToUserID: &other2.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(dm3))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, dm3.ID)
	// ルームメッセージ
	rm := &model.ChatMessage{
		ID: "cm_h4", FromUserID: me.ID, ToRoomID: &room.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(rm))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, rm.ID)

	// 3会話あるはず: me-other1, me-other2, room:cr_h1
	hist, err := repo.ListHistory(me.ID, 10)
	require.NoError(t, err)
	assert.Len(t, hist, 3)
	// 最新順: cm_h4 > cm_h3 > cm_h2 (cm_h1はme-other1会話の古い方で除外)
	assert.Equal(t, "cm_h4", hist[0].ID)

	// limit=1で最新1会話のみ
	hist1, err := repo.ListHistory(me.ID, 1)
	require.NoError(t, err)
	assert.Len(t, hist1, 1)

	// limit=0はデフォルト10
	histDef, err := repo.ListHistory(me.ID, 0)
	require.NoError(t, err)
	assert.Len(t, histDef, 3)
}

// #692: 履歴ゼロ件 → 空 slice (nil) を返す経路。limit clamp と異なり ids
// 空の早期 return ブランチをカバー。
func TestChatRepository_ListHistory_Variants_NoMessages(t *testing.T) {
	repo := NewChatRepository(testDB)

	uh, err := repo.ListUserHistory("nope_user_id", 10)
	require.NoError(t, err)
	assert.Empty(t, uh)
	rh, err := repo.ListRoomHistory("nope_user_id", 10)
	require.NoError(t, err)
	assert.Empty(t, rh)
}

// #692: ListUserHistory は DM 限定 (toRoomId NULL) で per-peer 最新を返す。
func TestChatRepository_ListUserHistory(t *testing.T) {
	repo := NewChatRepository(testDB)
	me := insertTestUser(t, "u_chat_uh1", "chatuh1")
	other1 := insertTestUser(t, "u_chat_uh2", "chatuh2")
	other2 := insertTestUser(t, "u_chat_uh3", "chatuh3")
	defer cleanupUser(t, me.ID)
	defer cleanupUser(t, other1.ID)
	defer cleanupUser(t, other2.ID)
	room := &model.ChatRoom{ID: "cr_uh1", Name: "Room", OwnerID: me.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	text := "msg"
	// DM 2 件 (me<->other1)
	for _, id := range []string{"cm_uh1", "cm_uh2"} {
		m := &model.ChatMessage{
			ID: id, FromUserID: me.ID, ToUserID: &other1.ID, Text: &text,
			Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
		}
		require.NoError(t, repo.CreateMessage(m))
		defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, id)
	}
	dm3 := &model.ChatMessage{
		ID: "cm_uh3", FromUserID: me.ID, ToUserID: &other2.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(dm3))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, dm3.ID)
	// room メッセージ → ListUserHistory では除外されるはず
	rm := &model.ChatMessage{
		ID: "cm_uh4", FromUserID: me.ID, ToRoomID: &room.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(rm))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, rm.ID)

	hist, err := repo.ListUserHistory(me.ID, 10)
	require.NoError(t, err)
	// 2 つの DM 会話 (me-other1, me-other2)。room は除外。
	assert.Len(t, hist, 2)
	for _, h := range hist {
		assert.Nil(t, h.ToRoomID, "room メッセージが混入してはいけない")
	}

	// limit clamp: 0 -> 10, 大きすぎる値 -> 100
	defHist, _ := repo.ListUserHistory(me.ID, 0)
	assert.Len(t, defHist, 2)
	clampHist, _ := repo.ListUserHistory(me.ID, 9999)
	assert.Len(t, clampHist, 2)
}

// #692: ListRoomHistory は room 限定 (toRoomId NOT NULL かつ owner / member) で
// per-room 最新を返す。
func TestChatRepository_ListRoomHistory(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_chat_rh1", "chatrh1")
	other := insertTestUser(t, "u_chat_rh2", "chatrh2")
	defer cleanupUser(t, owner.ID)
	defer cleanupUser(t, other.ID)
	r1 := &model.ChatRoom{ID: "cr_rh1", Name: "R1", OwnerID: owner.ID}
	r2 := &model.ChatRoom{ID: "cr_rh2", Name: "R2", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(r1))
	require.NoError(t, repo.CreateRoom(r2))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id IN (?, ?)`, r1.ID, r2.ID)

	text := "rmsg"
	// r1 に 2 件、r2 に 1 件
	for _, msg := range []*model.ChatMessage{
		{ID: "cm_rh1", FromUserID: owner.ID, ToRoomID: &r1.ID, Text: &text, Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{}},
		{ID: "cm_rh2", FromUserID: owner.ID, ToRoomID: &r1.ID, Text: &text, Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{}},
		{ID: "cm_rh3", FromUserID: owner.ID, ToRoomID: &r2.ID, Text: &text, Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{}},
	} {
		require.NoError(t, repo.CreateMessage(msg))
		defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msg.ID)
	}
	// 1on1 DM は除外される
	dm := &model.ChatMessage{
		ID: "cm_rh4", FromUserID: owner.ID, ToUserID: &other.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(dm))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, dm.ID)

	// owner は r1 / r2 のオーナーなので両方含む
	hist, err := repo.ListRoomHistory(owner.ID, 10)
	require.NoError(t, err)
	assert.Len(t, hist, 2)
	for _, h := range hist {
		require.NotNil(t, h.ToRoomID)
	}

	// other はメンバーシップ無し → 空
	histOther, err := repo.ListRoomHistory(other.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, histOther)

	// limit clamp
	defHist, _ := repo.ListRoomHistory(owner.ID, 0)
	assert.Len(t, defHist, 2)
	clampHist, _ := repo.ListRoomHistory(owner.ID, 9999)
	assert.Len(t, clampHist, 2)
}

// #692: MarkAllReadFromUser は (sender→reader) の DM だけを既読化し、
// 他人発の DM や room メッセージには触らない。
func TestChatRepository_MarkAllReadFromUser(t *testing.T) {
	repo := NewChatRepository(testDB)
	reader := insertTestUser(t, "u_chat_mrf1", "chatmrf1")
	sender := insertTestUser(t, "u_chat_mrf2", "chatmrf2")
	stranger := insertTestUser(t, "u_chat_mrf3", "chatmrf3")
	defer cleanupUser(t, reader.ID)
	defer cleanupUser(t, sender.ID)
	defer cleanupUser(t, stranger.ID)

	text := "x"
	// sender → reader (DM, 2 件)
	for _, id := range []string{"cm_mrf_s1", "cm_mrf_s2"} {
		m := &model.ChatMessage{
			ID: id, FromUserID: sender.ID, ToUserID: &reader.ID, Text: &text,
			Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
		}
		require.NoError(t, repo.CreateMessage(m))
		defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, id)
	}
	// stranger → reader (別人発、対象外)
	mStranger := &model.ChatMessage{
		ID: "cm_mrf_x", FromUserID: stranger.ID, ToUserID: &reader.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(mStranger))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, mStranger.ID)

	require.NoError(t, repo.MarkAllReadFromUser(reader.ID, sender.ID))

	// sender 発のものは reads に reader が入る
	mS1, err := repo.FindMessageByID("cm_mrf_s1")
	require.NoError(t, err)
	assert.Contains(t, []string(mS1.Reads), reader.ID)
	// stranger 発のものは触られない
	mX, err := repo.FindMessageByID("cm_mrf_x")
	require.NoError(t, err)
	assert.NotContains(t, []string(mX.Reads), reader.ID)

	// 冪等性: 2 回呼んでもエラーにならず、reads が膨らまない
	require.NoError(t, repo.MarkAllReadFromUser(reader.ID, sender.ID))
	mAfter, err := repo.FindMessageByID("cm_mrf_s1")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(strings.Join([]string(mAfter.Reads), ","), reader.ID))
}

// #692: MarkAllReadInRoom は同じ部屋の他人発メッセージを既読化、自分発は触らない。
func TestChatRepository_MarkAllReadInRoom(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_chat_mrr1", "chatmrr1")
	other := insertTestUser(t, "u_chat_mrr2", "chatmrr2")
	defer cleanupUser(t, owner.ID)
	defer cleanupUser(t, other.ID)
	room := &model.ChatRoom{ID: "cr_mrr", Name: "R", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	text := "y"
	// other 発 (reader=owner にとって既読対象)
	mOther := &model.ChatMessage{
		ID: "cm_mrr_o1", FromUserID: other.ID, ToRoomID: &room.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(mOther))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, mOther.ID)
	// owner 自身発 (既読化対象外)
	mSelf := &model.ChatMessage{
		ID: "cm_mrr_self", FromUserID: owner.ID, ToRoomID: &room.ID, Text: &text,
		Reads: model.StringArray{}, Reactions: model.StringArray{}, Emojis: model.StringArray{},
	}
	require.NoError(t, repo.CreateMessage(mSelf))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, mSelf.ID)

	require.NoError(t, repo.MarkAllReadInRoom(owner.ID, room.ID))

	mOtherAfter, err := repo.FindMessageByID("cm_mrr_o1")
	require.NoError(t, err)
	assert.Contains(t, []string(mOtherAfter.Reads), owner.ID)

	mSelfAfter, err := repo.FindMessageByID("cm_mrr_self")
	require.NoError(t, err)
	assert.NotContains(t, []string(mSelfAfter.Reads), owner.ID, "自分発メッセージは既読化対象外")
}

func TestChatRepository_ListInvitationsByUserAndRoom(t *testing.T) {
	repo := NewChatRepository(testDB)
	user1 := insertTestUser(t, "u_chat_li1", "chatli1")
	user2 := insertTestUser(t, "u_chat_li2", "chatli2")
	defer cleanupUser(t, user1.ID)
	defer cleanupUser(t, user2.ID)

	room := &model.ChatRoom{ID: "cr_li", Name: "Room", OwnerID: user1.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	inv := &model.ChatRoomInvitation{ID: "inv_li1", UserID: user2.ID, RoomID: room.ID}
	require.NoError(t, repo.CreateInvitation(inv))
	defer testDB.Exec(`DELETE FROM "chat_room_invitation" WHERE id = ?`, inv.ID)

	// ListInvitationsByUser: ignored=false
	rows, err := repo.ListInvitationsByUser(user2.ID, false, "", "", 30)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	// ignored=trueは0件
	rows2, err := repo.ListInvitationsByUser(user2.ID, true, "", "", 30)
	require.NoError(t, err)
	assert.Empty(t, rows2)

	// ListInvitationsByRoom
	roomInvs, err := repo.ListInvitationsByRoom(room.ID, "", "", 30)
	require.NoError(t, err)
	assert.Len(t, roomInvs, 1)
}

func TestChatRepository_Invitation(t *testing.T) {
	repo := NewChatRepository(testDB)
	user := insertTestUser(t, "u_chat_5", "chatuser5")
	defer cleanupUser(t, user.ID)

	room := &model.ChatRoom{ID: "cr_4", Name: "Room", OwnerID: user.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	// CreateInvitation
	inv := &model.ChatRoomInvitation{ID: "inv_1", UserID: user.ID, RoomID: room.ID}
	require.NoError(t, repo.CreateInvitation(inv))
	defer testDB.Exec(`DELETE FROM "chat_room_invitation" WHERE id = ?`, inv.ID)

	// FindInvitation
	found, err := repo.FindInvitation(user.ID, room.ID)
	require.NoError(t, err)
	assert.Equal(t, "inv_1", found.ID)

	// FindInvitation - not found
	_, err = repo.FindInvitation("ghost", room.ID)
	assert.Error(t, err)

	// FindInvitationByID (#1559)
	byID, err := repo.FindInvitationByID("inv_1")
	require.NoError(t, err)
	assert.Equal(t, user.ID, byID.UserID)
	assert.Equal(t, room.ID, byID.RoomID)
	_, err = repo.FindInvitationByID("ghost")
	assert.Error(t, err)

	// DeleteInvitation
	require.NoError(t, repo.DeleteInvitation("inv_1"))
}

func TestChatRepository_ListMessagesByFileID(t *testing.T) {
	repo := NewChatRepository(testDB)
	user := insertTestUser(t, "u_chat_file", "chatfileuser")
	defer cleanupUser(t, user.ID)

	fileX := "file_x"
	other := "file_y"
	mk := func(id, fileID string) *model.ChatMessage {
		f := fileID
		to := user.ID
		return &model.ChatMessage{
			ID: id, FromUserID: user.ID, ToUserID: &to, FileID: &f,
			Reads: model.StringArray{}, Reactions: model.StringArray{},
		}
	}
	for _, m := range []*model.ChatMessage{mk("cmf_1", fileX), mk("cmf_2", fileX), mk("cmf_3", other)} {
		require.NoError(t, repo.CreateMessage(m))
		defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, m.ID)
	}

	// fileX の message だけが newest-first で返る。
	msgs, err := repo.ListMessagesByFileID(fileX, "", "", 10)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "cmf_2", msgs[0].ID)
	assert.Equal(t, "cmf_1", msgs[1].ID)

	// untilID cursor: cmf_2 より前 (= cmf_1) のみ。
	older, err := repo.ListMessagesByFileID(fileX, "cmf_2", "", 10)
	require.NoError(t, err)
	require.Len(t, older, 1)
	assert.Equal(t, "cmf_1", older[0].ID)

	// sinceID cursor: cmf_1 より後 (= cmf_2) のみ。
	newer, err := repo.ListMessagesByFileID(fileX, "", "cmf_1", 10)
	require.NoError(t, err)
	require.Len(t, newer, 1)
	assert.Equal(t, "cmf_2", newer[0].ID)

	// default limit (0 → 10)。
	def, err := repo.ListMessagesByFileID(fileX, "", "", 0)
	require.NoError(t, err)
	assert.Len(t, def, 2)
}

// TestChatRepository_TransferRoomOwnership fixes the invariant that a room's
// owner never holds a chat_room_membership row (#2858).
func TestChatRepository_TransferRoomOwnership(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_xf_a", "chatxfera")
	defer cleanupUser(t, owner.ID)
	heir := insertTestUser(t, "u_xf_b", "chatxferb")
	defer cleanupUser(t, heir.ID)

	room := &model.ChatRoom{ID: "cr_xfer_1", Name: "Room", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE "roomId" = ?`, room.ID)

	// 譲り受ける側は room を mute した状態のメンバー。この行が残ると、
	// API もフロントも「ミュートしていない」と表示するのに行だけが残り、
	// UI から直せない設定になる (通知自体は owner never-muted で届く)。
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "mem_xfer_1", UserID: heir.ID, RoomID: room.ID, IsMuted: true,
	}))

	require.NoError(t, repo.TransferRoomOwnership(room.ID, owner.ID, heir.ID, "mem_xfer_2"))

	got, err := repo.FindRoomByID(room.ID)
	require.NoError(t, err)
	assert.Equal(t, heir.ID, got.OwnerID)

	_, err = repo.FindMembership(heir.ID, room.ID)
	assert.True(t, IsNotFound(err), "新 owner の membership 行は消える")

	old, err := repo.FindMembership(owner.ID, room.ID)
	require.NoError(t, err, "旧 owner は明示メンバーとして残る (譲渡は room を抜ける操作ではない)")
	assert.Equal(t, "mem_xfer_2", old.ID)
	assert.False(t, old.IsMuted)

	// 行数が動かない (削除 1 + 作成 1)。handler は譲渡先をメンバーに限るので
	// (#2858)、この入れ替えが room-full の上限をずらすことは無い。
	members, err := repo.ListMembersByRoom(room.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1)
}

// TestChatRepository_TransferRoomOwnership_SelfIsNoop guards the invariant at
// the repository level: transferring to yourself must not delete and recreate
// your own row. The handler has the same guard, but a second caller would not
// inherit it.
func TestChatRepository_TransferRoomOwnership_SelfIsNoop(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_xf_h", "chatxferh")
	defer cleanupUser(t, owner.ID)

	room := &model.ChatRoom{ID: "cr_xfer_4", Name: "Room", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE "roomId" = ?`, room.ID)

	require.NoError(t, repo.TransferRoomOwnership(room.ID, owner.ID, owner.ID, "mem_xfer_7"))

	members, err := repo.ListMembersByRoom(room.ID)
	require.NoError(t, err)
	assert.Empty(t, members, "owner は membership 行を持たないまま")

	// owner でない者の自己譲渡は成功しない (早期 return を owner 検査より前に
	// 置くとここが通ってしまう)。
	assert.True(t, IsNotFound(repo.TransferRoomOwnership(room.ID, "u_xf_i", "u_xf_i", "mem_xfer_8")))
}

// TestChatRepository_TransferRoomOwnership_ConsumesInvitation covers the other
// half of the invariant: a pending invitation for the incoming owner has to go,
// otherwise they can accept it and recreate the row we just deleted.
func TestChatRepository_TransferRoomOwnership_ConsumesInvitation(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_xf_j", "chatxferj")
	defer cleanupUser(t, owner.ID)
	heir := insertTestUser(t, "u_xf_k", "chatxferk")
	defer cleanupUser(t, heir.ID)
	other := insertTestUser(t, "u_xf_l", "chatxferl")
	defer cleanupUser(t, other.ID)

	room := &model.ChatRoom{ID: "cr_xfer_5", Name: "Room", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)
	defer testDB.Exec(`DELETE FROM "chat_room_invitation" WHERE "roomId" = ?`, room.ID)
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE "roomId" = ?`, room.ID)

	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
		ID: "inv_xfer_1", UserID: heir.ID, RoomID: room.ID,
	}))
	// 第三者宛の招待は残す。
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
		ID: "inv_xfer_2", UserID: other.ID, RoomID: room.ID,
	}))

	require.NoError(t, repo.TransferRoomOwnership(room.ID, owner.ID, heir.ID, "mem_xfer_9"))

	_, err := repo.FindInvitation(heir.ID, room.ID)
	assert.True(t, IsNotFound(err), "新 owner 宛の招待は消える")
	kept, err := repo.FindInvitation(other.ID, room.ID)
	require.NoError(t, err, "第三者宛の招待は残す")
	assert.Equal(t, "inv_xfer_2", kept.ID)
}

// TestChatRepository_TransferRoomOwnership_StaleOwnerRejected covers the
// concurrent-transfer guard: the UPDATE carries the expected owner, so the
// loser must change nothing at all.
func TestChatRepository_TransferRoomOwnership_StaleOwnerRejected(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_xf_c", "chatxferc")
	defer cleanupUser(t, owner.ID)
	other := insertTestUser(t, "u_xf_d", "chatxferd")
	defer cleanupUser(t, other.ID)

	room := &model.ChatRoom{ID: "cr_xfer_2", Name: "Room", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE "roomId" = ?`, room.ID)
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "mem_xfer_3", UserID: other.ID, RoomID: room.ID, IsMuted: true,
	}))

	// **stale owner は実在するユーザーにする。** 存在しない ID を渡すと、
	// owner 条件を落とす変異が FK 違反で落ちてしまい、楽観ロックが効いている
	// ことを確かめたことにならない。
	stale := insertTestUser(t, "u_xf_g", "chatxferg")
	defer cleanupUser(t, stale.ID)
	err := repo.TransferRoomOwnership(room.ID, stale.ID, other.ID, "mem_xfer_4")
	require.Error(t, err)
	assert.True(t, IsNotFound(err))

	// **membership 側だけ適用された中途半端な状態にならないこと。**
	got, err := repo.FindRoomByID(room.ID)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, got.OwnerID)
	kept, err := repo.FindMembership(other.ID, room.ID)
	require.NoError(t, err)
	assert.True(t, kept.IsMuted)
	_, err = repo.FindMembership(stale.ID, room.ID)
	assert.True(t, IsNotFound(err))
}

// TestChatRepository_TransferRoomOwnership_KeepsExistingRow guards the
// inconsistent data left by the pre-#2858 implementation: if the outgoing owner
// somehow already has a row, its isMuted must survive.
func TestChatRepository_TransferRoomOwnership_KeepsExistingRow(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_xf_e", "chatxfere")
	defer cleanupUser(t, owner.ID)
	heir := insertTestUser(t, "u_xf_f", "chatxferf")
	defer cleanupUser(t, heir.ID)

	room := &model.ChatRoom{ID: "cr_xfer_3", Name: "Room", OwnerID: owner.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE "roomId" = ?`, room.ID)
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "mem_xfer_5", UserID: owner.ID, RoomID: room.ID, IsMuted: true,
	}))

	require.NoError(t, repo.TransferRoomOwnership(room.ID, owner.ID, heir.ID, "mem_xfer_6"))

	kept, err := repo.FindMembership(owner.ID, room.ID)
	require.NoError(t, err)
	assert.Equal(t, "mem_xfer_5", kept.ID, "既存行を作り直さない")
	assert.True(t, kept.IsMuted, "利用者が設定した mute を勝手に解除しない")
}

// TestMigration000082_DropsOnlyOwnerMemberships checks the cleanup for rows the
// pre-#2858 transfer-ownership left behind: a room's owner must not hold a
// chat_room_membership row, while every other member's row survives.
func TestMigration000082_DropsOnlyOwnerMemberships(t *testing.T) {
	repo := NewChatRepository(testDB)
	a := insertTestUser(t, "u_m82a", "chatmig82a")
	defer cleanupUser(t, a.ID)
	b := insertTestUser(t, "u_m82b", "chatmig82b")
	defer cleanupUser(t, b.ID)

	roomA := &model.ChatRoom{ID: "cr_m82_1", Name: "A", OwnerID: a.ID}
	roomB := &model.ChatRoom{ID: "cr_m82_2", Name: "B", OwnerID: b.ID}
	require.NoError(t, repo.CreateRoom(roomA))
	require.NoError(t, repo.CreateRoom(roomB))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id IN (?, ?)`, roomA.ID, roomB.ID)
	defer testDB.Exec(`DELETE FROM "chat_room_membership" WHERE "roomId" IN (?, ?)`, roomA.ID, roomB.ID)
	defer testDB.Exec(`DELETE FROM "chat_room_invitation" WHERE "roomId" IN (?, ?)`, roomA.ID, roomB.ID)

	// owner 自身の行 (譲渡で残ったもの)。mute されている点が症状の核心で、
	// この行があると API もフロントも「ミュートしていない」と表示するのに、
	// 利用者からは見えない設定として残り続ける。
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "mem_m82_1", UserID: a.ID, RoomID: roomA.ID, IsMuted: true,
	}))
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "mem_m82_2", UserID: b.ID, RoomID: roomB.ID,
	}))
	// 通常のメンバー行。消してはいけない。
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "mem_m82_3", UserID: b.ID, RoomID: roomA.ID, IsMuted: true,
	}))
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "mem_m82_4", UserID: a.ID, RoomID: roomB.ID,
	}))

	// 招待も同じ形で残る (owner 宛の招待)。片方だけ消しても、accept で
	// membership が作り直されるので意味が無い。
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
		ID: "inv_m82_1", UserID: a.ID, RoomID: roomA.ID,
	}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
		ID: "inv_m82_2", UserID: a.ID, RoomID: roomB.ID,
	}))

	require.NoError(t, testDB.Exec(migrationSQL(t, "000082_drop_owner_chat_room_memberships.up.sql")).Error)

	exists := func(id string) bool {
		var n int64
		require.NoError(t, testDB.Raw(`SELECT count(*) FROM "chat_room_membership" WHERE id = ?`, id).Scan(&n).Error)
		return n > 0
	}
	invExists := func(id string) bool {
		var n int64
		require.NoError(t, testDB.Raw(`SELECT count(*) FROM "chat_room_invitation" WHERE id = ?`, id).Scan(&n).Error)
		return n > 0
	}
	assert.False(t, invExists("inv_m82_1"), "owner 宛の招待は削除される")
	assert.True(t, invExists("inv_m82_2"), "他 room 宛の招待は残す")
	assert.False(t, exists("mem_m82_1"), "owner 自身の行は削除される")
	assert.False(t, exists("mem_m82_2"), "mute していない owner の行も削除される")
	assert.True(t, exists("mem_m82_3"), "他 room のメンバー行は mute でも残す")
	assert.True(t, exists("mem_m82_4"), "通常のメンバー行は残す")

	// 冪等であること (migration は再適用されうる)。
	require.NoError(t, testDB.Exec(migrationSQL(t, "000082_drop_owner_chat_room_memberships.up.sql")).Error)
	assert.True(t, exists("mem_m82_3"))
	assert.True(t, exists("mem_m82_4"))

	// down は no-op で成功すること (ここで落ちると 000081 まで戻せなくなる)。
	require.NoError(t, testDB.Exec(migrationSQL(t, "000082_drop_owner_chat_room_memberships.down.sql")).Error)
}

// --- #2994: room は URI で引く ---

// **ローカル room を URI で掴まない。** `uri` は NULL なので、空文字や他ホストの
// URI で引いたときにローカル room が返ってはいけない (それが #2994 の取り違え)。
func TestChat_FindRoomByURI(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_findbyuri", "findbyuri")
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "chat_room" WHERE "ownerId" = ?`, owner.ID)
		cleanupUser(t, owner.ID)
	})

	const uri = "https://remote.example/chat/rooms/room1"
	host := "remote.example"
	u := uri
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{
		ID: "rm_remote_1", Name: "R", OwnerID: owner.ID, Host: &host, URI: &u,
	}))
	// 同じ room id のローカル room。`uri` は NULL。
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "L", OwnerID: owner.ID}))

	got, err := repo.FindRoomByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, "rm_remote_1", got.ID)
	require.NotNil(t, got.Host)
	assert.Equal(t, host, *got.Host)

	_, err = repo.FindRoomByURI("")
	assert.Error(t, err, "空文字でローカル room を掴んでいる")
	_, err = repo.FindRoomByURI("https://other.example/chat/rooms/room1")
	assert.Error(t, err)
}

// `IDX_chat_room_uri` が**部分 UNIQUE** であることを schema から固定する (#2994)。
//
// **既に migration を流した schema では、index の定義を変える変異を入れても
// `CREATE INDEX IF NOT EXISTS` が skip されるので検出できない。** CI は毎回
// クリーンな DB を立てるのでそちらでは効く。手元で確かめるなら schema を作り直す。
func TestChat_RoomURIIndexIsPartialUnique(t *testing.T) {
	var defs []string
	require.NoError(t, testDB.Raw(`SELECT indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'chat_room' AND indexname = 'IDX_chat_room_uri'`).
		Scan(&defs).Error)
	require.Len(t, defs, 1, "IDX_chat_room_uri が無い")
	assert.Contains(t, defs[0], "UNIQUE", "二重取り込みを DB で止められない")
	assert.Contains(t, defs[0], "WHERE (uri IS NOT NULL)",
		"部分 index でないとローカル room (uri が NULL) を巻き込む")
}

// **同じ room URI は 1 行しか持てない。** 並行する Invite が同時に通っても
// 二重取り込みにならないよう、DB 側でも止める (部分 UNIQUE index)。
func TestChat_RoomURIIsUnique(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_uriunique", "uriunique")
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "chat_room" WHERE "ownerId" = ?`, owner.ID)
		cleanupUser(t, owner.ID)
	})

	const uri = "https://remote.example/chat/rooms/dup"
	host := "remote.example"
	a, b := uri, uri
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "rm_dup_a", Name: "A", OwnerID: owner.ID, Host: &host, URI: &a}))
	require.Error(t, repo.CreateRoom(&model.ChatRoom{ID: "rm_dup_b", Name: "B", OwnerID: owner.ID, Host: &host, URI: &b}),
		"同じ uri の room が 2 行作れている")

	// ローカル room (uri が NULL) は何行でも作れる (部分 index なので NULL は対象外)。
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "rm_local_a", Name: "A", OwnerID: owner.ID}))
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "rm_local_b", Name: "B", OwnerID: owner.ID}))
}

// **backfill そのものを流して確かめる (#2994)。** migration の SQL は冪等
// (`ADD COLUMN IF NOT EXISTS` / `WHERE "uri" IS NULL` / `CREATE INDEX IF NOT EXISTS`)
// なので、ファイルをそのまま再実行できる。SQL を書き写して確かめると、写し間違い
// ごと緑になる。
func TestChat_MigrationBackfillsRemoteRoomHostAndURI(t *testing.T) {
	repo := NewChatRepository(testDB)
	remoteHost := "remote.example"
	// **scheme は `user.uri` から、authority は `user.host` から。** 片方だけを
	// 見る実装と区別が付かないと、この assert は空虚になる。`user.host` は
	// 非既定ポートを保つ (`hostFromURI` / `punyHostPort`)。
	ownerURI := "http://remote.example:8080/users/bob"
	remoteHostWithPort := "remote.example:8080"
	remote := insertTestUser(t, "u_bf_remote", "bfremote")
	require.NoError(t, testDB.Exec(`UPDATE "user" SET host = ?, uri = ? WHERE id = ?`,
		remoteHostWithPort, ownerURI, remote.ID).Error)
	// owner の uri が無いリモート (fallback 経路)。
	noURI := insertTestUser(t, "u_bf_nouri", "bfnouri")
	require.NoError(t, testDB.Exec(`UPDATE "user" SET host = ? WHERE id = ?`, remoteHost, noURI.ID).Error)
	local := insertTestUser(t, "u_bf_local", "bflocal")
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "chat_room" WHERE id IN (?, ?, ?)`, "bf_remote", "bf_nouri", "bf_local")
		cleanupUser(t, remote.ID)
		cleanupUser(t, noURI.ID)
		cleanupUser(t, local.ID)
	})

	// migration より前の形: host / uri は NULL、id が origin の room id そのもの。
	for id, owner := range map[string]string{
		"bf_remote": remote.ID, "bf_nouri": noURI.ID, "bf_local": local.ID,
	} {
		require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: id, Name: "R", OwnerID: owner}))
	}

	require.NoError(t, testDB.Exec(migrationSQL(t, "000090_chat_room_host.up.sql")).Error)

	got, err := repo.FindRoomByID("bf_remote")
	require.NoError(t, err)
	require.NotNil(t, got.Host)
	assert.Equal(t, remoteHostWithPort, *got.Host)
	require.NotNil(t, got.URI)
	assert.Equal(t, "http://remote.example:8080/chat/rooms/bf_remote", *got.URI,
		"owner の actor URI の scheme+authority から復元していない")

	// owner の uri が無ければ host から https で組み立てる。**埋め損ねると、次の
	// Invite が同じ room を別の行として作り直し、membership と invitation が宙に浮く。**
	got, err = repo.FindRoomByID("bf_nouri")
	require.NoError(t, err)
	require.NotNil(t, got.URI)
	assert.Equal(t, "https://remote.example/chat/rooms/bf_nouri", *got.URI)

	// ローカル room は NULL のまま。
	got, err = repo.FindRoomByID("bf_local")
	require.NoError(t, err)
	assert.Nil(t, got.Host, "ローカル room に host が付いている")
	assert.Nil(t, got.URI, "ローカル room に uri が付いている")
}

// **backfill した `uri` は実行時の引き当てと同じ正規形であること (#2994 レビュー H1)。**
// 実行時は `idnhost.CanonicalURI` を通した形で引くので、相手が名乗った生の actor id
// (`https://Mixed.Example/...` / `:443` 付き / IDN) をそのまま刻むと、**その room 宛の
// Accept も本文も引き当てに失敗して恒久的に落ちる**。しかも配送側は古い綴りを
// 名乗り続けるので誰も気付かない。
func TestChat_MigrationBackfillUsesCanonicalURI(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_bf_canon", "bfcanon")
	// `user.host` は #2706 以降 punycode + 小文字 + 既定ポート除去で保存される。
	// `user.uri` は相手が名乗った生の値。
	require.NoError(t, testDB.Exec(`UPDATE "user" SET host = ?, uri = ? WHERE id = ?`,
		"xn--eckve.example", "https://パイ.example:443/users/bob", owner.ID).Error)
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, "bf_canon")
		cleanupUser(t, owner.ID)
	})
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "bf_canon", Name: "R", OwnerID: owner.ID}))

	require.NoError(t, testDB.Exec(migrationSQL(t, "000090_chat_room_host.up.sql")).Error)

	got, err := repo.FindRoomByID("bf_canon")
	require.NoError(t, err)
	require.NotNil(t, got.URI)
	assert.Equal(t, "https://xn--eckve.example/chat/rooms/bf_canon", *got.URI,
		"生の actor id の綴りをそのまま刻んでいる (実行時の引き当てと食い違う)")
	assert.Equal(t, idnhost.CanonicalURI(*got.URI), *got.URI, "実行時の正規形と一致しない")
}
