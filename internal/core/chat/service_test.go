package chat_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	corechat "github.com/shiroha-a/mk/internal/core/chat"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeChatRepo / newFakeRepo は testutil.MockChatRepository に集約された
// (#709)。本ファイル / ap_delivery_test.go の呼び出し点を保つために alias
// を残しているが、新規 test は testutil.NewMockChatRepository() を直接
// 呼ぶこと。既存呼び出しも段階的に置換していく方針。
//
// Deprecated: use testutil.NewMockChatRepository directly.
// --- #2994: room は URI で指定する ---

// testRemoteRoomHost is the origin host used by the remote-room fixtures.
const testRemoteRoomHost = "remote.example"

// remoteRoomURI builds the canonical AP URI a remote instance would use for
// roomID. AP 経路の引数はすべてこれ (room id ではない)。
func remoteRoomURI(roomID string) string {
	return "https://" + testRemoteRoomHost + "/chat/rooms/" + roomID
}

func newFakeRepo() *testutil.MockChatRepository {
	return testutil.NewMockChatRepository()
}

// --- capture publisher ---

type capturePublisher struct {
	mu        sync.Mutex
	userCalls []userCall
	roomCalls []roomCall
}

type userCall struct {
	from, to, eventType string
	body                any
}

type roomCall struct {
	roomID, eventType string
	body              any
}

func (p *capturePublisher) PublishUserMessage(_ context.Context, from, to, t string, body any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.userCalls = append(p.userCalls, userCall{from, to, t, body})
}

func (p *capturePublisher) PublishRoomMessage(_ context.Context, roomID, t string, body any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roomCalls = append(p.roomCalls, roomCall{roomID, t, body})
}

// stubMainPublisher captures PublishMainEvent calls for newChatMessage tests.
type stubMainPublisher struct {
	mu    sync.Mutex
	calls []mainEventCall
}

type mainEventCall struct {
	userID, eventType string
	body              any
}

func (p *stubMainPublisher) PublishMainEvent(userID, eventType string, body any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, mainEventCall{userID, eventType, body})
}

// --- helper ---

func newSvc(t *testing.T) (*corechat.Service, *testutil.MockChatRepository, *capturePublisher) {
	t.Helper()
	repo := newFakeRepo()
	pub := &capturePublisher{}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(repo, idGen)
	svc.SetStreamingPublisher(pub)
	// push の body に fromUser を載せるため (#2840)。production は router が
	// 同じものを配線する。
	users := testutil.NewMockUserRepository()
	for _, u := range []string{"alice", "bob", "carol"} {
		name := u
		users.Users[u] = &model.User{ID: u, Username: u, Name: &name}
	}
	svc.SetUserRepo(users)
	return svc, repo, pub
}

// --- CreateMessageToUser ---

func TestCreateMessageToUser_Success(t *testing.T) {
	svc, _, pub := newSvc(t)
	msg, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hello", "")
	require.NoError(t, err)
	assert.NotEmpty(t, msg.ID)
	require.NotNil(t, msg.Text)
	assert.Equal(t, "hello", *msg.Text)
	require.Len(t, pub.userCalls, 1)
	assert.Equal(t, "alice", pub.userCalls[0].from)
	assert.Equal(t, "bob", pub.userCalls[0].to)
	assert.Equal(t, corechat.EventMessage, pub.userCalls[0].eventType)
}

func TestCreateMessageToUser_WithFile(t *testing.T) {
	svc, _, _ := newSvc(t)
	msg, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "", "file_x")
	require.NoError(t, err)
	require.NotNil(t, msg.FileID)
	assert.Equal(t, "file_x", *msg.FileID)
}

func TestCreateMessageToUser_EmptySender(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.CreateMessageToUser(context.Background(), "", "bob", "hi", "")
	assert.ErrorIs(t, err, corechat.ErrInvalidTarget)
}

func TestCreateMessageToUser_EmptyRecipient(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "", "hi", "")
	assert.ErrorIs(t, err, corechat.ErrInvalidTarget)
}

func TestCreateMessageToUser_RepoError(t *testing.T) {
	svc, repo, pub := newSvc(t)
	repo.CreateErr = errors.New("db boom")
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	assert.Error(t, err)
	assert.Empty(t, pub.userCalls)
}

// recipient (bob) が sender (alice) を block していると DM は ErrChatBlocked。
func TestCreateMessageToUser_BlockedByRecipient(t *testing.T) {
	svc, _, pub := newSvc(t)
	blocks := testutil.NewMockBlockingRepository()
	// blocker=bob, blockee=alice。
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "bob", BlockeeID: "alice"}))
	svc.SetBlockingRepo(blocks)

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	assert.ErrorIs(t, err, corechat.ErrChatBlocked)
	assert.Empty(t, pub.userCalls, "blocked DM must not be persisted/published")
}

// block 関係が無ければ通常どおり送信できる (block check は逆方向を見ない)。
func TestCreateMessageToUser_NotBlockedPasses(t *testing.T) {
	svc, _, _ := newSvc(t)
	blocks := testutil.NewMockBlockingRepository()
	// alice が bob を block していても、bob→alice は送れる(逆方向 block は無関係)。
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "alice", BlockeeID: "bob"}))
	svc.SetBlockingRepo(blocks)

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)
}

// block check の DB error は fail-closed (送信せず error を返す)。
func TestCreateMessageToUser_BlockCheckFailClosed(t *testing.T) {
	svc, _, pub := newSvc(t)
	blocks := testutil.NewMockBlockingRepository()
	blocks.ExistsErr = errors.New("db down")
	svc.SetBlockingRepo(blocks)

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.Error(t, err)
	assert.Empty(t, pub.userCalls, "fail-closed: message must not be sent on block-check error")
}

// 連合 inbound DM (CreateMessageViaAP) でも recipient が sender を block して
// いれば拒否される (#parity review chat-block-1)。
func TestCreateMessageViaAP_BlockedByRecipient(t *testing.T) {
	svc, _, pub := newSvc(t)
	blocks := testutil.NewMockBlockingRepository()
	// blocker=bob (local recipient), blockee=alice (remote sender)。
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "bob", BlockeeID: "alice"}))
	svc.SetBlockingRepo(blocks)

	remoteSender := &model.User{ID: "alice"}
	_, err := svc.CreateMessageViaAP(context.Background(), "https://remote/notes/1", remoteSender, "bob", "hi", "")
	assert.ErrorIs(t, err, corechat.ErrChatBlocked)
	assert.Empty(t, pub.userCalls, "blocked inbound DM must not be persisted/published")
}

// --- CreateMessageToRoom ---

func seedRoom(t *testing.T, repo *testutil.MockChatRepository, id string, owner string, members ...string) {
	t.Helper()
	repo.Rooms[id] = &model.ChatRoom{ID: id, Name: "test-room", OwnerID: owner}
	for _, u := range members {
		repo.Memberships[u+":"+id] = &model.ChatRoomMembership{UserID: u, RoomID: id}
	}
}

func TestCreateMessageToRoom_Success(t *testing.T) {
	svc, repo, pub := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "bob")

	msg, err := svc.CreateMessageToRoom(context.Background(), "bob", "r1", "hello room", "")
	require.NoError(t, err)
	require.NotNil(t, msg.ToRoomID)
	assert.Equal(t, "r1", *msg.ToRoomID)
	require.Len(t, pub.roomCalls, 1)
	assert.Equal(t, "r1", pub.roomCalls[0].roomID)
	assert.Equal(t, corechat.EventMessage, pub.roomCalls[0].eventType)
}

// block は 1-on-1 DM のみに効き、room メッセージには影響しない (upstream の
// createMessageToRoom も checkBlocked を呼ばない、#parity review chat-block-3)。
func TestCreateMessageToRoom_BlockDoesNotAffectRoom(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "bob")
	blocks := testutil.NewMockBlockingRepository()
	// alice と bob が相互 block していても room メッセージは通る。
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "alice", BlockeeID: "bob"}))
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b2", BlockerID: "bob", BlockeeID: "alice"}))
	svc.SetBlockingRepo(blocks)

	_, err := svc.CreateMessageToRoom(context.Background(), "bob", "r1", "hi room", "")
	require.NoError(t, err, "room message must not be blocked by 1-on-1 block")
}

func TestCreateMessageToRoom_OwnerIsImplicitMember(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice") // bob not a member

	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "r1", "hi", "")
	require.NoError(t, err)
}

func TestCreateMessageToRoom_NotMember(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice")

	_, err := svc.CreateMessageToRoom(context.Background(), "carol", "r1", "hi", "")
	assert.ErrorIs(t, err, corechat.ErrForbidden)
}

// --- newChatMessage emit (Phase 7-4b-4 / #298) ---

func TestCreateMessageToUser_PublishesNewChatMessage(t *testing.T) {
	svc, _, _ := newSvc(t)
	main := &stubMainPublisher{}
	svc.SetMainStreamPublisher(main)

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)

	main.mu.Lock()
	defer main.mu.Unlock()
	require.Len(t, main.calls, 1)
	// sender (alice) には送らず recipient (bob) にだけ emit。
	assert.Equal(t, "bob", main.calls[0].userID)
	assert.Equal(t, "newChatMessage", main.calls[0].eventType)
	body, ok := main.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", body["fromUserId"])
	assert.Equal(t, "bob", body["toUserId"])
	assert.Equal(t, "hi", body["text"])
}

func TestCreateMessageToUser_NoMainPublisher_NoEmit(t *testing.T) {
	svc, _, _ := newSvc(t)
	// SetMainStreamPublisher を呼ばない状態で正常終了することを確認。
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)
}

func TestCreateMessageViaAP_PublishesNewChatMessageToLocalRecipient(t *testing.T) {
	svc, _, _ := newSvc(t)
	main := &stubMainPublisher{}
	svc.SetMainStreamPublisher(main)

	// Remote → local DM: fromUser は remote (host set)、toUserID はローカル。
	remoteHost := "remote.example"
	fromUser := &model.User{ID: "remote_user", Host: &remoteHost}
	_, err := svc.CreateMessageViaAP(context.Background(), "https://remote.example/n/1", fromUser, "bob", "hello from remote", "")
	require.NoError(t, err)

	main.mu.Lock()
	defer main.mu.Unlock()
	require.Len(t, main.calls, 1)
	assert.Equal(t, "bob", main.calls[0].userID)
	assert.Equal(t, "newChatMessage", main.calls[0].eventType)
	body, ok := main.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "remote_user", body["fromUserId"])
	assert.Equal(t, "bob", body["toUserId"])
}

func TestCreateMessageToRoom_PublishesNewChatMessageToMembersExceptSender(t *testing.T) {
	svc, repo, _ := newSvc(t)
	// owner=alice, members=bob,carol。sender=bob の場合 alice と carol に emit。
	seedRoom(t, repo, "r1", "alice", "bob", "carol")
	main := &stubMainPublisher{}
	svc.SetMainStreamPublisher(main)

	_, err := svc.CreateMessageToRoom(context.Background(), "bob", "r1", "room hi", "")
	require.NoError(t, err)

	main.mu.Lock()
	defer main.mu.Unlock()
	// emit 対象 userID を set で検証 (順序は非決定的)。
	gotUserIDs := make(map[string]bool, len(main.calls))
	for _, c := range main.calls {
		assert.Equal(t, "newChatMessage", c.eventType)
		gotUserIDs[c.userID] = true
	}
	assert.Equal(t, map[string]bool{"alice": true, "carol": true}, gotUserIDs)
}

func TestCreateMessageToRoom_OwnerAsSender_EmitsToMembersOnly(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "bob")
	main := &stubMainPublisher{}
	svc.SetMainStreamPublisher(main)

	// owner=alice が送信 → bob にだけ emit (alice は sender 自身なので除外)。
	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "r1", "hi", "")
	require.NoError(t, err)

	main.mu.Lock()
	defer main.mu.Unlock()
	require.Len(t, main.calls, 1)
	assert.Equal(t, "bob", main.calls[0].userID)
}

func TestCreateMessageToRoom_RoomNotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "ghost", "hi", "")
	assert.ErrorIs(t, err, corechat.ErrNotFound)
}

func TestCreateMessageToRoom_EmptyTarget(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "", "hi", "")
	assert.ErrorIs(t, err, corechat.ErrInvalidTarget)
}

func TestCreateMessageToRoom_WithFile(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice")
	msg, err := svc.CreateMessageToRoom(context.Background(), "alice", "r1", "", "file_y")
	require.NoError(t, err)
	require.NotNil(t, msg.FileID)
}

func TestCreateMessageToRoom_RepoError(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice")
	repo.CreateErr = errors.New("db boom")
	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "r1", "hi", "")
	assert.Error(t, err)
}

// --- DeleteMessage ---

func TestDeleteMessage_UserDM(t *testing.T) {
	svc, _, pub := newSvc(t)
	msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	pub.userCalls = nil // reset

	require.NoError(t, svc.DeleteMessage(context.Background(), "alice", msg.ID))
	require.Len(t, pub.userCalls, 1)
	assert.Equal(t, corechat.EventDeleted, pub.userCalls[0].eventType)
}

func TestDeleteMessage_Room(t *testing.T) {
	svc, repo, pub := newSvc(t)
	seedRoom(t, repo, "r1", "alice")
	msg, _ := svc.CreateMessageToRoom(context.Background(), "alice", "r1", "hi", "")
	pub.roomCalls = nil

	require.NoError(t, svc.DeleteMessage(context.Background(), "alice", msg.ID))
	require.Len(t, pub.roomCalls, 1)
	assert.Equal(t, corechat.EventDeleted, pub.roomCalls[0].eventType)
}

func TestDeleteMessage_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.DeleteMessage(context.Background(), "alice", "ghost")
	assert.ErrorIs(t, err, corechat.ErrNotFound)
}

func TestDeleteMessage_NotAuthor(t *testing.T) {
	svc, _, _ := newSvc(t)
	msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	err := svc.DeleteMessage(context.Background(), "carol", msg.ID)
	assert.ErrorIs(t, err, corechat.ErrForbidden)
}

func TestDeleteMessage_RepoError(t *testing.T) {
	svc, repo, _ := newSvc(t)
	msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	repo.DeleteErr = errors.New("db boom")
	err := svc.DeleteMessage(context.Background(), "alice", msg.ID)
	assert.Error(t, err)
}

// --- UpdateMessage ---

func TestUpdateMessage_Success(t *testing.T) {
	svc, _, pub := newSvc(t)
	msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "original", "")
	pub.userCalls = nil

	updated, err := svc.UpdateMessage(context.Background(), "alice", msg.ID, "edited")
	require.NoError(t, err)
	require.NotNil(t, updated.Text)
	assert.Equal(t, "edited", *updated.Text)
	require.Len(t, pub.userCalls, 1)
	assert.Equal(t, corechat.EventEdited, pub.userCalls[0].eventType)
}

func TestUpdateMessage_Room(t *testing.T) {
	svc, repo, pub := newSvc(t)
	seedRoom(t, repo, "r1", "alice")
	msg, _ := svc.CreateMessageToRoom(context.Background(), "alice", "r1", "original", "")
	pub.roomCalls = nil

	_, err := svc.UpdateMessage(context.Background(), "alice", msg.ID, "edited")
	require.NoError(t, err)
	require.Len(t, pub.roomCalls, 1)
	assert.Equal(t, corechat.EventEdited, pub.roomCalls[0].eventType)
}

func TestUpdateMessage_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.UpdateMessage(context.Background(), "alice", "ghost", "x")
	assert.ErrorIs(t, err, corechat.ErrNotFound)
}

func TestUpdateMessage_NotAuthor(t *testing.T) {
	svc, _, _ := newSvc(t)
	msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	_, err := svc.UpdateMessage(context.Background(), "carol", msg.ID, "edited")
	assert.ErrorIs(t, err, corechat.ErrForbidden)
}

func TestUpdateMessage_RepoError(t *testing.T) {
	svc, repo, _ := newSvc(t)
	msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	repo.UpdateErr = errors.New("db boom")
	_, err := svc.UpdateMessage(context.Background(), "alice", msg.ID, "edited")
	assert.Error(t, err)
}

// --- MarkReadByMessageID ---

func TestMarkReadByMessageID_DM(t *testing.T) {
	svc, _, pub := newSvc(t)
	msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	pub.userCalls = nil

	require.NoError(t, svc.MarkReadByMessageID(context.Background(), "bob", msg.ID))
	require.Len(t, pub.userCalls, 1)
	assert.Equal(t, corechat.EventRead, pub.userCalls[0].eventType)
}

func TestMarkReadByMessageID_Room(t *testing.T) {
	svc, repo, pub := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "bob")
	msg, _ := svc.CreateMessageToRoom(context.Background(), "alice", "r1", "hi", "")
	pub.roomCalls = nil

	require.NoError(t, svc.MarkReadByMessageID(context.Background(), "bob", msg.ID))
	require.Len(t, pub.roomCalls, 1)
	assert.Equal(t, corechat.EventRead, pub.roomCalls[0].eventType)
}

// **当事者でなければ既読にできない。** 検査が無いと、非参加者が他人の reads を
// 汚したうえ、購読側が閉じている topic に read イベントを注入できる。
func TestMarkReadByMessageID_RequiresParticipation(t *testing.T) {
	t.Run("DM の第三者", func(t *testing.T) {
		svc, _, pub := newSvc(t)
		msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
		pub.userCalls = nil

		err := svc.MarkReadByMessageID(context.Background(), "carol", msg.ID)
		assert.ErrorIs(t, err, corechat.ErrNotFound, "存在を秘匿するため not-found に倒す")
		assert.Empty(t, pub.userCalls, "イベントを注入させない")
	})
	t.Run("room の非メンバー", func(t *testing.T) {
		svc, repo, pub := newSvc(t)
		seedRoom(t, repo, "r1", "alice", "bob")
		msg, _ := svc.CreateMessageToRoom(context.Background(), "alice", "r1", "hi", "")
		pub.roomCalls = nil

		err := svc.MarkReadByMessageID(context.Background(), "carol", msg.ID)
		assert.ErrorIs(t, err, corechat.ErrNotFound)
		assert.Empty(t, pub.roomCalls)
	})
	t.Run("room の owner は membership 行が無くても既読にできる", func(t *testing.T) {
		svc, repo, pub := newSvc(t)
		seedRoom(t, repo, "r1", "alice", "bob")
		msg, _ := svc.CreateMessageToRoom(context.Background(), "bob", "r1", "hi", "")
		pub.roomCalls = nil

		require.NoError(t, svc.MarkReadByMessageID(context.Background(), "alice", msg.ID))
		assert.Len(t, pub.roomCalls, 1)
	})
}

func TestLeaveRoom_MemberLeaves(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "bob")

	require.NoError(t, svc.LeaveRoom(context.Background(), "bob", "r1"))

	_, err := repo.FindMembership("bob", "r1")
	assert.Error(t, err, "自分の membership は消える")
}

func TestMarkReadByMessageID_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.MarkReadByMessageID(context.Background(), "bob", "ghost")
	assert.ErrorIs(t, err, corechat.ErrNotFound)
}

// --- IsRoomMember ---

func TestIsRoomMember_Member(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "bob")
	ok, err := svc.IsRoomMember("bob", "r1")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestIsRoomMember_Owner(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice") // alice is owner, no explicit membership
	ok, err := svc.IsRoomMember("alice", "r1")
	require.NoError(t, err)
	assert.True(t, ok, "owner should implicitly be a member")
}

func TestIsRoomMember_NonMember(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice")
	ok, err := svc.IsRoomMember("carol", "r1")
	require.NoError(t, err)
	assert.False(t, ok)
}

// --- nil publisher fallback ---

func TestService_NilPublisherStillWorks(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(newFakeRepo(), idGen)
	// No publisher set; operations should still succeed (no panic)
	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	assert.NoError(t, err)
}

// --- sanity: reads array mutation round-trip ---

func TestMarkRead_AppendsToReads(t *testing.T) {
	svc, repo, _ := newSvc(t)
	msg, _ := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, svc.MarkReadByMessageID(context.Background(), "bob", msg.ID))
	stored := repo.Messages[msg.ID]
	require.NotNil(t, stored)
	assert.Equal(t, model.StringArray{"bob"}, stored.Reads)
}

// --- HasPermissionToViewRoomInfo (upstream 2026.5.4 / #1164 Phase C) ---

// stubModeratorChecker satisfies corechat.ModeratorChecker for tests where we
// need to flip the "is moderator" answer per case without dragging in the full
// role service.
type stubModeratorChecker struct {
	moderators map[string]bool
}

func (s *stubModeratorChecker) IsModerator(userID string) bool {
	return s.moderators[userID]
}

func TestHasPermissionToViewRoomInfo_NilRoom(t *testing.T) {
	svc, _, _ := newSvc(t)
	ok, err := svc.HasPermissionToViewRoomInfo("alice", nil)
	require.NoError(t, err)
	assert.False(t, ok, "nil room must return false (defensive)")
}

func TestHasPermissionToViewRoomInfo_OwnerTrue(t *testing.T) {
	svc, _, _ := newSvc(t)
	room := &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ok, err := svc.HasPermissionToViewRoomInfo("alice", room)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestHasPermissionToViewRoomInfo_MemberTrue(t *testing.T) {
	svc, repo, _ := newSvc(t)
	room := &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	repo.Memberships["bob:r1"] = &model.ChatRoomMembership{UserID: "bob", RoomID: "r1"}
	ok, err := svc.HasPermissionToViewRoomInfo("bob", room)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestHasPermissionToViewRoomInfo_InvitationTrue(t *testing.T) {
	svc, repo, _ := newSvc(t)
	room := &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	repo.Invitations["inv1"] = &model.ChatRoomInvitation{ID: "inv1", UserID: "carol", RoomID: "r1"}
	ok, err := svc.HasPermissionToViewRoomInfo("carol", room)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestHasPermissionToViewRoomInfo_NonMemberFalse(t *testing.T) {
	svc, _, _ := newSvc(t)
	room := &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ok, err := svc.HasPermissionToViewRoomInfo("dave", room)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestHasPermissionToViewRoomInfo_ModeratorBypass(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetModeratorChecker(&stubModeratorChecker{moderators: map[string]bool{"moderator1": true}})
	room := &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ok, err := svc.HasPermissionToViewRoomInfo("moderator1", room)
	require.NoError(t, err)
	assert.True(t, ok, "moderator が non-owner non-member でも閲覧できる")
}

func TestHasPermissionToViewRoomInfo_ModeratorCheckerNotConsulted(t *testing.T) {
	svc, _, _ := newSvc(t)
	// moderator checker 未配線 (= nil) のとき、moderator bypass 経路は skip
	// される。non-owner / non-member は常に false。
	room := &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	ok, err := svc.HasPermissionToViewRoomInfo("someone", room)
	require.NoError(t, err)
	assert.False(t, ok)
}

// --- #1549: chat react / unreact stream events ---

func TestReact_DMPublishesReact(t *testing.T) {
	svc, repo, pub := newSvc(t)
	to := "bob"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to}
	// upstream は 1-on-1 で受信者 (toUserId) だけが react できる。
	reactor := &model.User{ID: "bob", Username: "bob"}
	require.NoError(t, svc.React(context.Background(), "m1", reactor, "👍"))
	require.Len(t, pub.userCalls, 1)
	assert.Equal(t, corechat.EventReact, pub.userCalls[0].eventType)
	body, ok := pub.userCalls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "m1", body["messageId"])
	assert.Equal(t, "👍", body["reaction"])
	assert.NotNil(t, body["user"], "reactor が UserLite で含まれる")
}

func TestReact_RoomPublishesReact(t *testing.T) {
	svc, repo, pub := newSvc(t)
	room := "r1"
	// reactor は room member でなければ react できない (owner = member 扱い)。
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToRoomID: &room}
	require.NoError(t, svc.React(context.Background(), "m1", &model.User{ID: "alice"}, "👍"))
	require.Len(t, pub.roomCalls, 1)
	assert.Equal(t, corechat.EventReact, pub.roomCalls[0].eventType)
	assert.Empty(t, pub.userCalls)
}

func TestUnreact_PublishesUnreact(t *testing.T) {
	svc, repo, pub := newSvc(t)
	to := "bob"
	repo.Messages["m1"] = &model.ChatMessage{
		ID: "m1", FromUserID: "carol", ToUserID: &to,
		Reactions: []string{"bob/👍"},
	}
	require.NoError(t, svc.Unreact(context.Background(), "m1", &model.User{ID: "bob"}, "👍"))
	require.Len(t, pub.userCalls, 1)
	assert.Equal(t, corechat.EventUnreact, pub.userCalls[0].eventType)
}

// **消えていないなら publish しない (#3037)。** 参加者でも、付けていない
// リアクションを指定されたときはイベントを流さない。
func TestUnreact_NotRemovedDoesNotPublish(t *testing.T) {
	to := "bob"

	for _, tt := range []struct {
		name      string
		reactions []string
		reaction  string
	}{
		{"付けていないリアクション", []string{"bob/👍"}, "🎉"},
		{"リアクションが 1 つも無い", nil, "👍"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, pub := newSvc(t)
			repo.Messages["m1"] = &model.ChatMessage{
				ID: "m1", FromUserID: "carol", ToUserID: &to,
				Reactions: tt.reactions,
			}

			require.NoError(t, svc.Unreact(context.Background(), "m1", &model.User{ID: "bob"}, tt.reaction))
			assert.Empty(t, pub.userCalls, "何も消えていないのにイベントを流している")
			assert.Empty(t, pub.roomCalls)
		})
	}
}

// **参加していない利用者の unreact はアクセスエラーにする。** 会話と無関係な
// 利用者が任意の messageId と文字列を投げても、何も消さず何も流さない。
// 存在しない id と同じエラーにして、存在を判別させない。
func TestUnreact_NonParticipantIsAccessError(t *testing.T) {
	to := "bob"
	room := "r1"

	for _, tt := range []struct {
		name     string
		msg      *model.ChatMessage
		actor    string
		reaction string
	}{
		{"DM の第三者", &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to, Reactions: []string{"bob/👍"}}, "mallory", "👍"},
		{"任意の文字列", &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to, Reactions: []string{"bob/👍"}}, "mallory", "<script>alert(1)</script>"},
		// 部屋を抜けた元メンバー。自分の古いリアクションが残っていても、
		// 消して抜けた部屋へイベントを流すことはできない。
		{"部屋を抜けた元メンバー", &model.ChatMessage{ID: "m1", FromUserID: "carol", ToRoomID: &room, Reactions: []string{"mallory/👍"}}, "mallory", "👍"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, pub := newSvc(t)
			repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
			repo.Messages["m1"] = tt.msg
			before := append([]string(nil), tt.msg.Reactions...)

			err := svc.Unreact(context.Background(), "m1", &model.User{ID: tt.actor}, tt.reaction)
			require.ErrorIs(t, err, corechat.ErrMessageAccess)
			assert.Equal(t, before, []string(repo.Messages["m1"].Reactions), "参加していないのにリアクションが消えている")
			assert.Empty(t, pub.userCalls, "参加していないのにイベントを流している")
			assert.Empty(t, pub.roomCalls, "参加していないのにイベントを流している")
		})
	}
}

// room 側: 非メンバーは拒否され、メンバーが自分で付けたものだけ消せて publish される。
func TestUnreact_RoomPublishesOnlyWhenRemoved(t *testing.T) {
	room := "r1"

	svc, repo, pub := newSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	repo.Messages["m1"] = &model.ChatMessage{
		ID: "m1", FromUserID: "carol", ToRoomID: &room,
		Reactions: []string{"alice/👍"},
	}

	err := svc.Unreact(context.Background(), "m1", &model.User{ID: "mallory"}, "👍")
	require.ErrorIs(t, err, corechat.ErrMessageAccess)
	assert.Empty(t, pub.roomCalls)

	require.NoError(t, svc.Unreact(context.Background(), "m1", &model.User{ID: "alice"}, "👍"))
	require.Len(t, pub.roomCalls, 1)
	assert.Equal(t, corechat.EventUnreact, pub.roomCalls[0].eventType)
}

func TestReact_MessageNotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.React(context.Background(), "ghost", &model.User{ID: "alice"}, "👍")
	assert.ErrorIs(t, err, corechat.ErrMessageAccess)
}

func TestUnreact_MessageNotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Unreact(context.Background(), "ghost", &model.User{ID: "alice"}, "👍")
	assert.ErrorIs(t, err, corechat.ErrMessageAccess)
}

// room の行が無い room メッセージもアクセスエラー。
func TestReactUnreact_RoomMissing(t *testing.T) {
	svc, repo, pub := newSvc(t)
	room := "gone"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToRoomID: &room, Reactions: []string{"bob/👍"}}
	assert.ErrorIs(t, svc.React(context.Background(), "m1", &model.User{ID: "bob"}, "🎉"), corechat.ErrMessageAccess)
	assert.ErrorIs(t, svc.Unreact(context.Background(), "m1", &model.User{ID: "bob"}, "👍"), corechat.ErrMessageAccess)
	assert.Empty(t, pub.roomCalls)
}

// --- #1541: React validation (own/others/member/limit/emoji) ---

// 自分のメッセージには react できない。
func TestReact_OwnMessage(t *testing.T) {
	svc, repo, _ := newSvc(t)
	to := "bob"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "alice", ToUserID: &to}
	err := svc.React(context.Background(), "m1", &model.User{ID: "alice"}, "👍")
	assert.ErrorIs(t, err, corechat.ErrMessageAccess)
}

// 自分が member の部屋に送った自分のメッセージも、react / unreact できない。
// DM なら受信者の検査でも弾かれるので、自分のメッセージの検査だけが効くのは
// 部屋のメッセージ。
// 自分のメッセージには react できない。unreact は本家どおり拒否しない (DM の
// 送った側も、部屋のメンバーも、自分のリアクションを外せる)。
func TestReactUnreact_OwnMessage(t *testing.T) {
	t.Run("room", func(t *testing.T) {
		svc, repo, pub := newSvc(t)
		room := "r1"
		repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
		repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "alice", ToRoomID: &room, Reactions: []string{"alice/👍"}}
		assert.ErrorIs(t, svc.React(context.Background(), "m1", &model.User{ID: "alice"}, "👍"), corechat.ErrMessageAccess)
		require.NoError(t, svc.Unreact(context.Background(), "m1", &model.User{ID: "alice"}, "👍"))
		assert.Empty(t, repo.Messages["m1"].Reactions)
		assert.Len(t, pub.roomCalls, 1)
	})
	t.Run("dm sender", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		to := "bob"
		repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "alice", ToUserID: &to, Reactions: []string{"alice/👍"}}
		assert.ErrorIs(t, svc.React(context.Background(), "m1", &model.User{ID: "alice"}, "👍"), corechat.ErrMessageAccess)
		require.NoError(t, svc.Unreact(context.Background(), "m1", &model.User{ID: "alice"}, "👍"))
		assert.Empty(t, repo.Messages["m1"].Reactions)
	})
}

// 1-on-1 で受信者でない第三者は react できない。
func TestReact_OthersMessage1on1(t *testing.T) {
	svc, repo, _ := newSvc(t)
	to := "bob"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to}
	err := svc.React(context.Background(), "m1", &model.User{ID: "dave"}, "👍")
	assert.ErrorIs(t, err, corechat.ErrMessageAccess)
}

// room メッセージは member でなければ react できない。
func TestReact_RoomNotMember(t *testing.T) {
	svc, repo, _ := newSvc(t)
	room := "r1"
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToRoomID: &room}
	err := svc.React(context.Background(), "m1", &model.User{ID: "dave"}, "👍")
	assert.ErrorIs(t, err, corechat.ErrMessageAccess)
}

// 100 reaction を超えると拒否する。
func TestReact_TooManyReactions(t *testing.T) {
	svc, repo, _ := newSvc(t)
	to := "bob"
	reactions := make([]string, maxReactionsForTest)
	for i := range reactions {
		reactions[i] = "u/x"
	}
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to, Reactions: reactions}
	err := svc.React(context.Background(), "m1", &model.User{ID: "bob"}, "👍")
	assert.ErrorIs(t, err, corechat.ErrTooManyReactions)
}

// **上限の検査はアクセス検査より後。** 参加していない利用者には、上限に
// 達したメッセージでも存在しない場合と同じアクセスエラーを返す。
func TestReact_TooManyReactionsCheckedAfterAccess(t *testing.T) {
	to := "bob"
	room := "r1"
	reactions := make([]string, maxReactionsForTest)
	for i := range reactions {
		reactions[i] = "u/x"
	}
	for _, tt := range []struct {
		name string
		msg  *model.ChatMessage
	}{
		{"DM の第三者", &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to, Reactions: reactions}},
		{"部屋の非メンバー", &model.ChatMessage{ID: "m1", FromUserID: "carol", ToRoomID: &room, Reactions: reactions}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, _ := newSvc(t)
			repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "alice"}
			repo.Messages["m1"] = tt.msg
			err := svc.React(context.Background(), "m1", &model.User{ID: "mallory"}, "👍")
			require.ErrorIs(t, err, corechat.ErrMessageAccess)
			assert.NotErrorIs(t, err, corechat.ErrTooManyReactions)
		})
	}
}

// 存在しない custom emoji は ErrNoSuchEmoji。
func TestReact_NoSuchEmoji(t *testing.T) {
	svc, repo, _ := newSvc(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	svc.SetEmojiRepo(emojiRepo)
	to := "bob"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to}
	err := svc.React(context.Background(), "m1", &model.User{ID: "bob"}, ":ghost:")
	assert.ErrorIs(t, err, corechat.ErrNoSuchEmoji)
}

// 存在する local custom emoji は ":name:" に正規化されて publish される。
func TestReact_CustomEmojiOK(t *testing.T) {
	svc, repo, pub := newSvc(t)
	emojiRepo := testutil.NewMockEmojiRepository()
	emojiRepo.Emojis["party@"] = &model.Emoji{Name: "party"}
	svc.SetEmojiRepo(emojiRepo)
	to := "bob"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to}
	require.NoError(t, svc.React(context.Background(), "m1", &model.User{ID: "bob"}, ":party:"))
	require.Len(t, pub.userCalls, 1)
	body, ok := pub.userCalls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ":party:", body["reaction"], "custom emoji は :name: 形式に正規化される")
}

// unicode reaction は variation selector (U+FE0F) を strip して publish される。
func TestReact_UnicodeStripsVariationSelector(t *testing.T) {
	svc, repo, pub := newSvc(t)
	to := "bob"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to}
	require.NoError(t, svc.React(context.Background(), "m1", &model.User{ID: "bob"}, "❤️"))
	require.Len(t, pub.userCalls, 1)
	body, ok := pub.userCalls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "❤", body["reaction"], "U+FE0F が除去される")
}

// maxReactionsForTest mirrors the service-internal cap for the limit test.
const maxReactionsForTest = 100

// ReadUserChat / ReadRoomChat は会話全体既読 (repo へ委譲、no-error)。
func TestReadUserChat_NoError(t *testing.T) {
	svc, _, _ := newSvc(t)
	assert.NoError(t, svc.ReadUserChat(context.Background(), "alice", "bob"))
}

func TestReadRoomChat_NoError(t *testing.T) {
	svc, _, _ := newSvc(t)
	assert.NoError(t, svc.ReadRoomChat(context.Background(), "alice", "r1"))
}

// #2106 N4: Unreact は React と同じく emoji を正規化 (VS strip / custom :name:) してから
// array_remove する。raw (VS 付き) のままだと厳密一致で取り消せず silent fail していた。
func TestUnreact_NormalizesVariationSelector(t *testing.T) {
	svc, repo, pub := newSvc(t)
	to := "bob"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "carol", ToUserID: &to}

	// VS 付き ❤️ で react → "bob/❤" (VS strip) で保存。
	require.NoError(t, svc.React(context.Background(), "m1", &model.User{ID: "bob"}, "❤️"))
	require.Contains(t, repo.AddedReactions, "bob/❤")

	// VS 付き ❤️ (raw) で unreact → 正規化されて同じ "bob/❤" key で array_remove。
	require.NoError(t, svc.Unreact(context.Background(), "m1", &model.User{ID: "bob"}, "❤️"))
	require.Contains(t, repo.RemovedReactions, "bob/❤", "VS strip した key で array_remove する")
	// **`unreact` イベントを名指しで取る (#3037 レビュー)。** 末尾を見る形だと、
	// unreact が publish されなくなっても react の同じ値でアサーションが通り、
	// 検査が黙って空虚になる。
	var unreact *userCall
	for i := range pub.userCalls {
		if pub.userCalls[i].eventType == corechat.EventUnreact {
			unreact = &pub.userCalls[i]
		}
	}
	require.NotNil(t, unreact, "unreact が publish されていない")
	body, ok := unreact.body.(map[string]any)
	require.True(t, ok)
	// stream event も正規化済 reaction で publish。
	assert.Equal(t, "❤", body["reaction"])
}

// #2106 L57: moderator は non-member room の timeline を購読できる (CanViewRoomTimeline)。
func TestCanViewRoomTimeline_ModeratorBypass(t *testing.T) {
	svc, repo, _ := newSvc(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", Name: "test-room", OwnerID: "alice"}
	svc.SetModeratorChecker(&stubModeratorChecker{moderators: map[string]bool{"mod1": true}})

	ok, err := svc.CanViewRoomTimeline("mod1", "r1")
	require.NoError(t, err)
	assert.True(t, ok, "moderator は non-member room を購読できる")

	ok2, err := svc.CanViewRoomTimeline("bob", "r1")
	require.NoError(t, err)
	assert.False(t, ok2, "非 moderator non-member は購読不可")

	ok3, err := svc.CanViewRoomTimeline("alice", "r1")
	require.NoError(t, err)
	assert.True(t, ok3, "owner は moderator でなくても購読可")
}

// --- newChatMessage の Web Push (#2840) ---

// stubChatPusher captures PushNewChatMessage calls.
type stubChatPusher struct {
	mu    sync.Mutex
	calls []chatPushCall
}

type chatPushCall struct {
	userID string
	body   map[string]any
}

func (p *stubChatPusher) PushNewChatMessage(userID string, body map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, chatPushCall{userID, body})
}

func (p *stubChatPusher) recipients() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.calls))
	for _, c := range p.calls {
		out = append(out, c.userID)
	}
	sort.Strings(out)
	return out
}

// mainRecipients returns the users that received a newChatMessage main event.
func (p *stubMainPublisher) mainRecipients() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.calls))
	for _, c := range p.calls {
		if c.eventType == "newChatMessage" {
			out = append(out, c.userID)
		}
	}
	sort.Strings(out)
	return out
}

// 1:1 チャットで main stream と Web Push が**対で**飛ぶ (#2840)。
//
// upstream ChatService.ts は publish と push を必ず同じ場所で対にしている。
// 片方だけになると、タブを閉じている利用者に通知が届かない。
func TestCreateMessageToUser_PushesNewChatMessage(t *testing.T) {
	svc, _, _ := newSvc(t)
	main := &stubMainPublisher{}
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(push)

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"bob"}, push.recipients(), "recipient にだけ push する")
	// **publish と push の宛先が一致すること。**
	assert.Equal(t, main.mainRecipients(), push.recipients())

	// **SW が無条件に参照するフィールドが body に載っていること。**
	// packages/sw/src/scripts/create-notification.ts の newChatMessage 分岐は
	// `fromUser.name` / `fromUser.avatarUrl` を読む。無いと TypeError で
	// **通知が 1 件も出ない** (publish 側は body を読まないので気付けない)。
	require.Len(t, push.calls, 1)
	from, ok := push.calls[0].body["fromUser"].(map[string]any)
	require.True(t, ok, "fromUser が無い: %v", push.calls[0].body)
	assert.Equal(t, "alice", from["id"])
	assert.Contains(t, from, "name")
	assert.Contains(t, from, "avatarUrl")
	assert.Contains(t, push.calls[0].body, "text")
}

// room チャットでも sender 以外の全員に対で飛ぶ (#2840)。
func TestCreateMessageToRoom_PushesNewChatMessage(t *testing.T) {
	svc, repo, _ := newSvc(t)
	// owner=alice, members=bob,carol。sender=bob なら alice と carol に届く。
	seedRoom(t, repo, "r1", "alice", "bob", "carol")
	main := &stubMainPublisher{}
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(push)

	_, err := svc.CreateMessageToRoom(context.Background(), "bob", "r1", "hi", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"alice", "carol"}, push.recipients(), "sender (bob) には push しない")
	assert.Equal(t, main.mainRecipients(), push.recipients())

	// **room では toRoom が要る。** 無いと SW が DM 分岐に落ち、tag が
	// chat:room:<id> ではなく chat:user:<senderId> になって別 room の通知が
	// 同じ tag で潰れる。
	require.NotEmpty(t, push.calls)
	room, ok := push.calls[0].body["toRoom"].(map[string]any)
	require.True(t, ok, "toRoom が無い: %v", push.calls[0].body)
	assert.Equal(t, "r1", room["id"])
	assert.Contains(t, room, "name")
	require.Contains(t, push.calls[0].body, "fromUser")
}

// mute した member には push しない (#2840)。
//
// upstream ChatService.ts は `if (membership.isMuted) continue;` で marker を
// 張らず publish も push も行かない。Web Push を足すと、**利用者が明示的に
// 切った設定が OS 通知として破られる**。
func TestCreateMessageToRoom_SkipsMutedMembers(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "bob", "carol")
	repo.Memberships["carol:r1"].IsMuted = true
	main := &stubMainPublisher{}
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(push)

	_, err := svc.CreateMessageToRoom(context.Background(), "bob", "r1", "hi", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"alice"}, push.recipients(), "mute した carol に push している")
	assert.Equal(t, []string{"alice"}, main.mainRecipients(), "publish も揃えること")
}

// AP 受信の DM 経路でも push する (#2840)。
//
// mk-go は upstream に無い AP 受信経路を持つ (chat 連合は cherrypick 由来)。
// **ここが抜けると、リモートからの DM だけ通知が来ない。**
func TestCreateMessageViaAP_PushesNewChatMessage(t *testing.T) {
	svc, _, _ := newSvc(t)
	main := &stubMainPublisher{}
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(push)
	sender := &model.User{ID: "remote1", Username: "remote1"}

	_, err := svc.CreateMessageViaAP(context.Background(),
		"https://remote.example/chat-messages/1", sender, "local1", "hi", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"local1"}, push.recipients())
	assert.Equal(t, main.mainRecipients(), push.recipients())
	require.Len(t, push.calls, 1)
	from, ok := push.calls[0].body["fromUser"].(map[string]any)
	require.True(t, ok, "fromUser が無い: %v", push.calls[0].body)
	assert.Equal(t, "remote1", from["id"])
}

// pusher 未配線でも落ちない (テスト構成や read-only 構成)。
func TestCreateMessageToUser_NoPusherIsNoop(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetMainStreamPublisher(&stubMainPublisher{})

	_, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hi", "")
	assert.NoError(t, err)
}

// owner は membership 行が mute でも受け取る (#2840)。
//
// **これは 3 周目のレビューで元に戻した判断。** 2 周目で「mute した owner を
// 除外する」形にしたが、mk-go の読み取り側と矛盾していた —
// packRoomDetailed は owner に `isMuted: false` を固定で返し
// (`api/chat/handler.go` の `meID != r.OwnerID` ガード)、フロントは owner に
// ミュートのスイッチを出さない (`room.info.vue` の `v-if="!isOwner"`)。
//
// owner は membership 行を持たない (#2858 で transfer-ownership も行を入れ替える)
// ので通常は読む先が無いが、不整合データで行が残っていた場合にここだけ mute を
// 尊重すると、**API もフロントも「ミュートしていない」と表示するのに通知だけ
// 来ない**状態になり原因に辿り着けない。
// upstream も `concat({isMuted: false})` で owner を never-muted 扱いする。
func TestCreateMessageToRoom_OwnerIsNeverMuted(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "alice", "carol")
	repo.Memberships["alice:r1"].IsMuted = true
	main := &stubMainPublisher{}
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(push)

	_, err := svc.CreateMessageToRoom(context.Background(), "carol", "r1", "hi", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"alice"}, push.recipients(), "owner を mute 扱いで落としている")
	assert.Equal(t, []string{"alice"}, main.mainRecipients())
}

// owner が membership 行を持たない通常構成でも届く。
//
// **上の OwnerIsNeverMuted と対で意味を持つ。** owner の seed を丸ごと消す
// 直し方を弾く。
func TestCreateMessageToRoom_UnmutedOwnerStillReceives(t *testing.T) {
	svc, repo, _ := newSvc(t)
	seedRoom(t, repo, "r1", "alice", "carol")
	main := &stubMainPublisher{}
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(push)

	_, err := svc.CreateMessageToRoom(context.Background(), "carol", "r1", "hi", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"alice"}, push.recipients())
}

// AP 受信の room 経路でも sender を渡す (#2840)。
//
// **remote sender は user 表に居ないことがある** (viaRelay の ephemeral actor は
// DB に載らない)。repo から引き直す実装だと fromUser が nil になり、push が
// 丸ごと落ちて「リモートの room メッセージだけ通知が来ない」になる。
func TestCreateRoomMessageViaAP_PushesNewChatMessage(t *testing.T) {
	svc, repo, _ := newSvc(t)
	// 取り込んだリモート room に remote sender が投げる形 (#2994 以降、AP 経路の
	// room 指定は URI)。
	roomURI := seedRemoteRoom(t, repo, "r1", "alice")
	repo.Memberships["carol:r1"] = &model.ChatRoomMembership{UserID: "carol", RoomID: "r1"}
	repo.Memberships["remote1:r1"] = &model.ChatRoomMembership{UserID: "remote1", RoomID: "r1"}
	main := &stubMainPublisher{}
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(main)
	svc.SetChatPusher(push)
	// **userRepo に居ない** remote actor。
	sender := &model.User{ID: "remote1", Username: "remote1"}

	require.NoError(t, svc.CreateRoomMessageViaAP(
		"https://remote.example/chat-messages/2", sender, roomURI, "hi", ""))

	assert.ElementsMatch(t, []string{"alice", "carol"}, push.recipients())
	require.NotEmpty(t, push.calls)
	from, ok := push.calls[0].body["fromUser"].(map[string]any)
	require.True(t, ok, "fromUser が無い: %v", push.calls[0].body)
	assert.Equal(t, "remote1", from["id"])
}

// sender を解決できないときは push しない (#2840)。
//
// **fromUser の無い body は SW を TypeError で落とす。** 通知が出ないうえに
// push の枠だけ消費するので、無通知に倒すほうが安全。ここでは userRepo が
// 未配線の構成と、lookup が error を返す構成の両方を見る。
func TestCreateMessageToUser_UnresolvedSenderSkipsPush(t *testing.T) {
	for _, tt := range []struct {
		name  string
		users repository.UserRepository
	}{
		{name: "repo unwired", users: nil},
		{name: "lookup fails", users: &failingUserRepo{MockUserRepository: testutil.NewMockUserRepository()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _ := newSvc(t)
			svc.SetUserRepo(tt.users)
			main := &stubMainPublisher{}
			push := &stubChatPusher{}
			svc.SetMainStreamPublisher(main)
			svc.SetChatPusher(push)

			_, err := svc.CreateMessageToUser(context.Background(), "bob", "alice", "hi", "")
			require.NoError(t, err)

			assert.Empty(t, push.recipients(), "sender 不明でも push している")
			// **publish は止めない。** アプリ内の未読は body を読まないので出せる。
			assert.Equal(t, []string{"alice"}, main.mainRecipients())
		})
	}
}

// 添付のみのメッセージでも text キーを落とさない (#2840)。
//
// SW は `${name}: ${body.text}` を無条件に組むので、キーが無いと通知本文が
// 「alice: undefined」になる。upstream は null を送る。
func TestCreateMessageToUser_PushKeepsNullText(t *testing.T) {
	svc, _, _ := newSvc(t)
	push := &stubChatPusher{}
	svc.SetMainStreamPublisher(&stubMainPublisher{})
	svc.SetChatPusher(push)

	_, err := svc.CreateMessageToUser(context.Background(), "bob", "alice", "", "f1")
	require.NoError(t, err)

	require.Len(t, push.calls, 1)
	text, ok := push.calls[0].body["text"]
	require.True(t, ok, "text キーが無い: %v", push.calls[0].body)
	assert.Nil(t, text)
}

// failingUserRepo makes FindByID fail so senderForPush returns nil.
type failingUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingUserRepo) FindByID(string) (*model.User, error) {
	return nil, errors.New("db down")
}
