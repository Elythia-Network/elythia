package repository

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// DeleteInvitationsToOwnedRooms は、ownerID が持つ全てのルームへの userID の
// 招待だけを消す。他の人のルームへの招待と、同じルームへの第三者の招待は残す。
func TestChatRepository_DeleteInvitationsToOwnedRooms(t *testing.T) {
	repo := NewChatRepository(testDB)
	owner := insertTestUser(t, "u_chat_dio_o", "chatdioowner")
	target := insertTestUser(t, "u_chat_dio_t", "chatdiotarget")
	other := insertTestUser(t, "u_chat_dio_x", "chatdioother")
	defer cleanupUser(t, owner.ID)
	defer cleanupUser(t, target.ID)
	defer cleanupUser(t, other.ID)

	rooms := []*model.ChatRoom{
		{ID: "cr_dio_o1", Name: "o1", OwnerID: owner.ID},
		{ID: "cr_dio_o2", Name: "o2", OwnerID: owner.ID},
		{ID: "cr_dio_x", Name: "x", OwnerID: other.ID},
	}
	for _, r := range rooms {
		require.NoError(t, repo.CreateRoom(r))
		defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, r.ID)
	}
	invs := []*model.ChatRoomInvitation{
		{ID: "inv_dio_1", UserID: target.ID, RoomID: "cr_dio_o1"},
		{ID: "inv_dio_2", UserID: target.ID, RoomID: "cr_dio_o2"},
		{ID: "inv_dio_3", UserID: other.ID, RoomID: "cr_dio_o1"},
		{ID: "inv_dio_4", UserID: target.ID, RoomID: "cr_dio_x"},
	}
	for _, inv := range invs {
		require.NoError(t, repo.CreateInvitation(inv))
		defer testDB.Exec(`DELETE FROM "chat_room_invitation" WHERE id = ?`, inv.ID)
	}

	require.NoError(t, repo.DeleteInvitationsToOwnedRooms(owner.ID, target.ID))

	var left []string
	require.NoError(t, testDB.Raw(`SELECT "id" FROM "chat_room_invitation" WHERE "id" LIKE 'inv_dio_%'`).Scan(&left).Error)
	sort.Strings(left)
	assert.Equal(t, []string{"inv_dio_3", "inv_dio_4"}, left)

	// NUL を含む id は行に一致しないので、クエリを投げずに何もしない
	require.NoError(t, repo.DeleteInvitationsToOwnedRooms("bad\x00id", target.ID))
}
