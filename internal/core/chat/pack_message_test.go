package chat

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// packMessage は exported ではないので同一パッケージのテストで exercise する。
// 全ポインタ列 (Text / ToUserID / ToRoomID / FileID / URI) と配列列
// (Reads / Reactions) を埋めて 100% カバレッジを狙う。
func TestPackMessage_AllFieldsFilled(t *testing.T) {
	text := "hello"
	toUser := "bob"
	toRoom := "room1"
	fileID := "file1"
	uri := "https://example.com/messages/m1"
	msg := &model.ChatMessage{
		ID:         "m1",
		FromUserID: "alice",
		Text:       &text,
		ToUserID:   &toUser,
		ToRoomID:   &toRoom,
		FileID:     &fileID,
		URI:        &uri,
		Reads:      model.StringArray{"bob"},
		// 保存形式は "<userId>/<reaction>" (handler.go の ReactionsCreate)。
		Reactions: model.StringArray{"bob/👍"},
	}
	out := packMessage(msg)

	assert.Equal(t, "m1", out["id"])
	assert.Equal(t, "alice", out["fromUserId"])
	assert.Equal(t, "hello", out["text"])
	assert.Equal(t, "bob", out["toUserId"])
	assert.Equal(t, "room1", out["toRoomId"])
	assert.Equal(t, "file1", out["fileId"])
	assert.Equal(t, "https://example.com/messages/m1", out["uri"])
	assert.Equal(t, []string{"bob"}, out["reads"])
	// reactions は misskey_dart 互換の {reaction} object 配列 + userId prefix 除去 (#1246)。
	assert.Equal(t, []map[string]any{{"reaction": "👍"}}, out["reactions"])
}
