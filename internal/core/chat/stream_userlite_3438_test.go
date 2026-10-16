package chat_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonObject3438 round-trips v through JSON, the way the stream / push body
// reaches the client.
func jsonObject3438(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// streamLiteFiller records each FillLites call and marks every lite so the
// test can see the filled value reach the payload.
type streamLiteFiller struct {
	calls [][]string
}

func (f *streamLiteFiller) FillLites(users []*model.User, lites []*entity.UserLite) {
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
		lites[i].Emojis["filled_"+u.ID] = "https://media.example/" + u.ID + ".png"
	}
	f.calls = append(f.calls, ids)
}

// requireStreamUserLite asserts v satisfies the misskey-js UserLite contract.
func requireStreamUserLite(t *testing.T, v any, wantID string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	require.True(t, ok, "UserLite must be an object, got %T", v)
	assert.Equal(t, wantID, m["id"])
	_, ok = m["avatarDecorations"].([]any)
	assert.True(t, ok, "avatarDecorations must be an array, got %T", m["avatarDecorations"])
	_, ok = m["emojis"].(map[string]any)
	assert.True(t, ok, "emojis must be an object, got %T", m["emojis"])
	assert.Contains(t, []any{"unknown", "online", "active", "offline"}, m["onlineStatus"])
	shapetest.Assert(t, "UserLite", m)
	return m
}

// TestStreamMessage_UsersAreUserLiteFilledInOneBatch pins that the stream
// payload's fromUser / toUser are upstream UserLite and that instance / emojis
// of both are resolved with one FillLites call (#3438).
func TestStreamMessage_UsersAreUserLiteFilledInOneBatch(t *testing.T) {
	svc, repo, pub := newSvc(t)
	filler := &streamLiteFiller{}
	svc.SetLiteFiller(filler)
	require.True(t, svc.HasLiteFiller())

	alice := &model.User{ID: "alice", Username: "alice"}
	bob := &model.User{ID: "bob", Username: "bob"}
	to := "bob"
	old := "old"
	repo.Messages["m1"] = &model.ChatMessage{
		ID: "m1", FromUserID: "alice", FromUser: alice, ToUserID: &to, ToUser: bob, Text: &old,
	}

	_, err := svc.UpdateMessage(context.Background(), "alice", "m1", "new")
	require.NoError(t, err)
	require.Len(t, pub.userCalls, 1)
	body := jsonObject3438(t, pub.userCalls[0].body)

	from := requireStreamUserLite(t, body["fromUser"], "alice")
	toUser := requireStreamUserLite(t, body["toUser"], "bob")
	assert.Contains(t, from["emojis"], "filled_alice", "filled value must reach fromUser")
	assert.Contains(t, toUser["emojis"], "filled_bob", "filled value must reach toUser")
	require.Len(t, filler.calls, 1, "fromUser and toUser must be resolved in one batch")
	assert.ElementsMatch(t, []string{"alice", "bob"}, filler.calls[0])
}

// TestPushNewChatMessage_BodyIsNarrowed pins the Web Push body of a direct
// message: fromUser carries only what the service worker reads (the full
// UserLite does not fit the Web Push size limit), and toUser is dropped.
func TestPushNewChatMessage_BodyIsNarrowed(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetLiteFiller(&streamLiteFiller{})
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(&stubMainPublisher{})
	svc.SetChatPusher(push)

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)
	require.Len(t, push.calls, 1)
	body := jsonObject3438(t, push.calls[0].body)
	assert.Equal(t, map[string]any{
		"id": "alice", "username": "alice", "host": nil, "name": "alice", "avatarUrl": "/identicon/alice",
	}, body["fromUser"])
	assert.NotContains(t, body, "toUser")
	assert.NotContains(t, body, "toRoom")
	assert.Equal(t, "hi", body["text"])
	assert.Equal(t, "bob", body["toUserId"], "the service worker opens the chat by toUserId")
}

// countingUsers counts FindByID calls per user ID.
type countingUsers struct {
	*testutil.MockUserRepository
	mu        sync.Mutex
	finds     map[string]int
	manyCalls int
}

func (r *countingUsers) FindByID(id string) (*model.User, error) {
	r.mu.Lock()
	r.finds[id]++
	r.mu.Unlock()
	return r.MockUserRepository.FindByID(id)
}

func (r *countingUsers) FindManyByIDs(ids []string) ([]*model.User, error) {
	r.mu.Lock()
	r.manyCalls++
	r.mu.Unlock()
	return r.MockUserRepository.FindManyByIDs(ids)
}

func newCountingUsers() *countingUsers {
	users := testutil.NewMockUserRepository()
	for _, u := range []string{"alice", "bob", "carol", "dave"} {
		name := u
		users.Users[u] = &model.User{ID: u, Username: u, Name: &name}
	}
	return &countingUsers{MockUserRepository: users, finds: map[string]int{}}
}

// TestRoomMessage_StreamAndNewChatMessageShapes pins, for a room message:
//   - the room stream `message` carries fromUser (upstream
//     ChatMessageLiteForRoom requires it)
//   - main stream `newChatMessage` is upstream packMessageDetailed: ChatMessage
//     with fromUser and toRoom (ChatRoom with owner, and isMuted /
//     invitationExists false as upstream packRoom without a viewer)
//   - the Web Push body narrows toRoom to {id, name}
//   - the sender and owner are loaded once per message and the payload is
//     packed once per message, not per recipient
func TestRoomMessage_StreamAndNewChatMessageShapes(t *testing.T) {
	svc, repo, pub := newSvc(t)
	users := newCountingUsers()
	svc.SetUserRepo(users)
	filler := &streamLiteFiller{}
	svc.SetLiteFiller(filler)
	main := &stubMainPublisher{}
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(push)

	idGen, _ := id.NewGenerator("aidx")
	roomID := idGen.Generate(time.Now())
	seedRoom(t, repo, roomID, "alice", "bob", "carol", "dave")

	_, err := svc.CreateMessageToRoom(context.Background(), "bob", roomID, "hello room", "")
	require.NoError(t, err)

	// room の stream の message。
	require.Len(t, pub.roomCalls, 1)
	streamBody := jsonObject3438(t, pub.roomCalls[0].body)
	requireStreamUserLite(t, streamBody["fromUser"], "bob")

	// main の newChatMessage (alice / carol / dave の 3 人)。
	require.Len(t, main.calls, 3)
	for _, c := range main.calls {
		require.Equal(t, "newChatMessage", c.eventType)
		body := jsonObject3438(t, c.body)
		shapetest.Assert(t, "ChatMessage", body)
		requireStreamUserLite(t, body["fromUser"], "bob")
		assert.NotContains(t, body, "toUser")
		toRoom, ok := body["toRoom"].(map[string]any)
		require.True(t, ok, "newChatMessage must carry toRoom: %v", body)
		shapetest.Assert(t, "ChatRoom", toRoom)
		assert.Equal(t, roomID, toRoom["id"])
		assert.Equal(t, "test-room", toRoom["name"])
		// 本家 packRoom は me 無しだと membership / invitation が null になり、
		// 両方 false を返す。
		assert.Equal(t, false, toRoom["isMuted"])
		assert.Equal(t, false, toRoom["invitationExists"])
		requireStreamUserLite(t, toRoom["owner"], "alice")
	}

	// push の body は main stream と分け、SW が読む項目だけにする (大きさの上限)。
	require.Len(t, push.calls, 3)
	pushBody := jsonObject3438(t, push.calls[0].body)
	assert.Equal(t, map[string]any{"id": roomID, "name": "test-room"}, pushBody["toRoom"])
	assert.NotContains(t, pushBody, "toUser")

	assert.Equal(t, 1, users.finds["bob"], "sender must be loaded once per message")
	assert.Equal(t, 1, users.finds["alice"], "owner must be loaded once per message")
	// stream の message で 1 回、newChatMessage で 1 回。宛先の数に比例しない。
	assert.Len(t, filler.calls, 2)
}

// TestDirectMessage_NewChatMessageIsDetailed pins main stream `newChatMessage`
// of a direct message as upstream packMessageDetailed (fromUser and toUser),
// while the 1:1 stream `message` stays ChatMessageLiteFor1on1 (no fromUser).
func TestDirectMessage_NewChatMessageIsDetailed(t *testing.T) {
	svc, _, pub := newSvc(t)
	filler := &streamLiteFiller{}
	svc.SetLiteFiller(filler)
	main := &stubMainPublisher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(&stubChatPusher{})

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)

	require.Len(t, pub.userCalls, 1)
	assert.NotContains(t, jsonObject3438(t, pub.userCalls[0].body), "fromUser", "1:1 stream message is the lite shape")

	require.Len(t, main.calls, 1)
	body := jsonObject3438(t, main.calls[0].body)
	shapetest.Assert(t, "ChatMessage", body)
	requireStreamUserLite(t, body["fromUser"], "alice")
	requireStreamUserLite(t, body["toUser"], "bob")
	assert.NotContains(t, body, "toRoom")
}

// TestAPRoomMessage_StreamHasFromUser pins fromUser on the room stream
// `message` and on `newChatMessage` for a federated room message.
func TestAPRoomMessage_StreamHasFromUser(t *testing.T) {
	svc, repo, pub := newSvc(t)
	main := &stubMainPublisher{}
	svc.SetMainStreamPublisher(main)
	host := "remote.example"
	remote := &model.User{ID: "rmt", Username: "rmt", Host: &host}
	room := remoteRoomRow("room1", "General", "rmt")
	require.NoError(t, repo.CreateRoom(room))
	repo.Memberships["alice:"+room.ID] = &model.ChatRoomMembership{UserID: "alice", RoomID: room.ID}

	require.NoError(t, svc.CreateRoomMessageViaAP("https://remote.example/chat/messages/m1", remote, remoteRoomURI("room1"), "<p>hi</p>", ""))
	require.Len(t, pub.roomCalls, 1)
	requireStreamUserLite(t, jsonObject3438(t, pub.roomCalls[0].body)["fromUser"], "rmt")
	require.Len(t, main.calls, 1, "the local member receives newChatMessage")
	body := jsonObject3438(t, main.calls[0].body)
	requireStreamUserLite(t, body["fromUser"], "rmt")
	_, ok := body["toRoom"].(map[string]any)
	assert.True(t, ok, "newChatMessage must carry toRoom")
}

// TestApprovedDirectMessage_NewChatMessageUsers pins newChatMessage of a
// direct message in an already approved conversation (the scope check is
// skipped, so neither user is loaded beforehand): fromUser is the sender,
// toUser is the recipient, and both come from one FindManyByIDs.
func TestApprovedDirectMessage_NewChatMessageUsers(t *testing.T) {
	svc, _, _ := newSvc(t)
	users := newCountingUsers()
	svc.SetUserRepo(users)
	approvals := newFakeApprovalRepo()
	approvals.approvals["bob/alice"] = true // bob が alice へ送ったことがある
	svc.SetApprovalRepo(approvals)
	main := &stubMainPublisher{}
	svc.SetMainStreamPublisher(main)

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)
	require.Len(t, main.calls, 1)
	body := jsonObject3438(t, main.calls[0].body)
	requireStreamUserLite(t, body["fromUser"], "alice")
	requireStreamUserLite(t, body["toUser"], "bob")
	assert.Equal(t, 1, users.manyCalls, "sender and recipient must be loaded with one query")
	assert.Empty(t, users.finds, "no per-user lookup")
}

// TestAPDirectMessage_NewChatMessageAndStream pins a federated direct
// message: newChatMessage carries toUser (the local recipient), and the 1:1
// stream message carries createdAt (required in upstream
// ChatMessageLiteFor1on1) but not fromUser.
func TestAPDirectMessage_NewChatMessageAndStream(t *testing.T) {
	svc, _, pub := newSvc(t)
	main := &stubMainPublisher{}
	svc.SetMainStreamPublisher(main)
	host := "remote.example"
	remote := &model.User{ID: "rmt", Username: "rmt", Host: &host}

	_, err := svc.CreateMessageViaAP(context.Background(), "https://remote.example/m/1", remote, "bob", "<p>hi</p>", "")
	require.NoError(t, err)

	require.Len(t, pub.userCalls, 1)
	stream := jsonObject3438(t, pub.userCalls[0].body)
	createdAt, ok := stream["createdAt"].(string)
	require.True(t, ok, "stream message must carry createdAt: %v", stream)
	_, err = time.Parse(time.RFC3339, createdAt)
	require.NoError(t, err)
	assert.NotContains(t, stream, "fromUser", "1:1 stream message is the lite shape")

	require.Len(t, main.calls, 1)
	body := jsonObject3438(t, main.calls[0].body)
	requireStreamUserLite(t, body["fromUser"], "rmt")
	requireStreamUserLite(t, body["toUser"], "bob")
}
