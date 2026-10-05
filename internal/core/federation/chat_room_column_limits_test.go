package federation_test

import (
	"strings"
	"testing"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roomURIPrefix / maxLengthRoomURI / oversizedRoomURI build room URIs at the
// `chat_room.uri` column boundary.
const roomURIPrefix = "https://remote.example/chat/rooms/"

func maxLengthRoomURI() string { return roomURIPrefix + strings.Repeat("a", 512-len(roomURIPrefix)) }
func oversizedRoomURI() string { return roomURIPrefix + strings.Repeat("a", 513-len(roomURIPrefix)) }

// `chat_room.uri` は varchar(512)。**room の身元は URI であって room id ではない**
// (#2994) — 行の `id` は取り込み時にこちらで採番するので相手の値は入らない。
// 収まらない URI は room として認識しない (#2726 と同じ判断)。
func TestProcess_ChatRoomInvite_OversizedRoomURINotRouted(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/users/bob",
		"object": {"type": "Group", "id": "` + oversizedRoomURI() + `", "name": "G"}
	}`)
	// Group と認識されないので reversi Invite 経路へ落ち、ack される
	// (retry storm にしない)。
	err := p.Process(body)
	require.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.ensureCalls)
}

// 上限ちょうどの room URI は通ること (境界を締めすぎていないこと)。
func TestProcess_ChatRoomInvite_MaxLengthRoomURIAccepted(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	roomURI := maxLengthRoomURI()
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/users/bob",
		"object": {"type": "Group", "id": "` + roomURI + `", "name": "G"}
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, recv.ensureCalls, 1)
	assert.Equal(t, roomURI, recv.ensureCalls[0][0])
}

// `@context` が room URI なのに列に収まらない group message は、**1-on-1 DM
// 経路へ落とさず** ここで drop する。混ぜると別の理由 (recipient 解決失敗) で
// retry を使い切ることになる (#2726)。
func TestProcess_ChatRoomMessage_OversizedRoomURIDropped(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	chatMsg := &fakeChatMessageReceiver{}
	p.SetChatService(chatMsg)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI, ChatScope: "everyone"}

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat/messages/m1",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "hello room",
			"to": ["https://example.com/users/bob"],
			"_misskey_talk": true,
			"@context": "` + oversizedRoomURI() + `"
		}
	}`)
	err := p.Process(body)
	require.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.msgCalls)
	assert.Zero(t, chatMsg.calls, "room message must not fall through to the 1-on-1 path")
}

// 列に収まらない uri を chat service が拒んだら (ErrInvalidTarget)、inbox job は
// retry せず ack する。retry しても同じ値が来るだけ (#2726)。
func TestProcess_ChatRoomMessage_UnstorableURINotRetried(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{msgErr: corechat.ErrInvalidTarget}
	p.SetChatRoomReceiver(recv)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat/messages/m1",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "hello room",
			"_misskey_talk": true,
			"@context": "https://remote.example/chat/rooms/room1"
		}
	}`)
	err := p.Process(body)
	require.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}
