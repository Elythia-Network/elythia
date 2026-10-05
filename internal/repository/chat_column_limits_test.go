package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// chatRemoteColumns は core/chat の ...ViaAP が AP から受けた値を書く列と、その上限。
// migration/000022_chat.up.sql と 000090_chat_room_host.up.sql に一致させる。
var chatRemoteColumns = []struct {
	table  string
	column string
	max    int
}{
	{"chat_room", "id", 32},
	{"chat_room", "host", 128},
	{"chat_room", "uri", 512},
	{"chat_room", "name", 256},
	{"chat_room", "description", 2048},
	{"chat_message", "text", 4096},
	{"chat_message", "uri", 512},
}

// 列の上限そのものを schema から固定する (#2726)。
//
// core/chat 側の定数 (chatRoomIDMaxRunes 等) と独立に同じ数値が書かれているだけ
// だと、揃って動かせば全部緑になる。列が変わったらここが落ちる。
func TestChat_RemoteColumnLimits(t *testing.T) {
	for _, tc := range chatRemoteColumns {
		var n int
		require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
			tc.table, tc.column).Scan(&n).Error)
		assert.Equal(t, tc.max, n,
			"%s.%s の列長が変わっている (internal/core/chat/service.go の定数も直すこと)", tc.table, tc.column)
	}
}

// core/chat が「収まる」と判断した長さで実際に書けること (#2726)。
//
// mock repository は列制約を持たないので、service のテストだけでは「本当に入る
// 長さか」を確かめられない。**全角で埋める** — byte で数える実装だと 3 倍になって
// 入らない (列はコードポイント単位)。
func TestChat_RemoteColumnLimitsAcceptMaxLengthValues(t *testing.T) {
	repo := NewChatRepository(testDB)
	user := insertTestUser(t, "u_chatcol_1", "chatcol1")
	defer cleanupUser(t, user.ID)

	roomID := strings.Repeat("r", 32)
	room := &model.ChatRoom{
		ID:          roomID,
		Name:        strings.Repeat("あ", 256),
		Description: strings.Repeat("い", 2048),
		OwnerID:     user.ID,
	}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, roomID)

	text := strings.Repeat("う", 4096)
	uri := "https://remote.example/" + strings.Repeat("え", 489)
	require.Equal(t, 512, len([]rune(uri)))
	msg := &model.ChatMessage{
		ID:         "cm_collimit_1",
		FromUserID: user.ID,
		ToRoomID:   &roomID,
		Text:       &text,
		URI:        &uri,
	}
	require.NoError(t, repo.CreateMessage(msg))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msg.ID)

	storedRoom, err := repo.FindRoomByID(roomID)
	require.NoError(t, err)
	assert.Equal(t, 256, len([]rune(storedRoom.Name)))
	assert.Equal(t, 2048, len([]rune(storedRoom.Description)))

	storedMsg, err := repo.FindMessageByID(msg.ID)
	require.NoError(t, err)
	require.NotNil(t, storedMsg.Text)
	require.NotNil(t, storedMsg.URI)
	assert.Equal(t, 4096, len([]rune(*storedMsg.Text)))
	assert.Equal(t, 512, len([]rune(*storedMsg.URI)))
}
