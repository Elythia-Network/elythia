package chat_test

import (
	"context"
	"errors"
	"testing"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRoomFedService(t *testing.T) (*corechat.Service, *testutil.MockChatRepository) {
	t.Helper()
	repo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	return corechat.NewService(repo, idGen), repo
}

func TestEnsureRoomViaAP_CreatesCopy(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, svc.EnsureRoomViaAP(remoteRoomURI("room1"), "General", "desc", "remoteOwner"))
	// **引き当ては URI。** 行の `id` はこちらで採番するので origin の room id では
	// 引けない (#2994)。
	room, err := repo.FindRoomByURI(remoteRoomURI("room1"))
	require.NoError(t, err)
	assert.Equal(t, "General", room.Name)
	assert.Equal(t, "desc", room.Description)
	assert.Equal(t, "remoteOwner", room.OwnerID)
	require.NotNil(t, room.Host)
	assert.Equal(t, testRemoteRoomHost, *room.Host, "host を埋めていない")
	assert.NotEqual(t, "room1", room.ID, "origin の room id をそのまま行の id にしている")
}

// **この issue (#2994) の中核。** 別のホストが同じ room id の room を先に作って
// いても、正規の room の Invite が通ること。id で keying していた頃は owner 不一致で
// **恒久的に drop** されていた (retry もされない)。
func TestEnsureRoomViaAP_IDCollisionWithLocalRoomIsAllowed(t *testing.T) {
	svc, repo := newRoomFedService(t)
	// 先取りされたローカル room (host / uri は NULL)。
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "Local", OwnerID: "localOwner"}))

	require.NoError(t, svc.EnsureRoomViaAP(remoteRoomURI("room1"), "Remote", "x", "remoteOwner"))

	copied, err := repo.FindRoomByURI(remoteRoomURI("room1"))
	require.NoError(t, err)
	assert.Equal(t, "remoteOwner", copied.OwnerID)
	assert.NotEqual(t, "room1", copied.ID, "先取りされた id を掴んでいる")

	// 先取りしていたローカル room は触られない。
	local, err := repo.FindRoomByID("room1")
	require.NoError(t, err)
	assert.Equal(t, "localOwner", local.OwnerID)
	assert.Equal(t, "Local", local.Name)
	assert.Nil(t, local.URI, "ローカル room に uri が付いている")
}

// 別の 2 ホストが同じ room id を持っていても共存できること。
func TestEnsureRoomViaAP_SameIDOnTwoHostsCoexist(t *testing.T) {
	svc, repo := newRoomFedService(t)
	const a = "https://a.example/chat/rooms/room1"
	const b = "https://b.example/chat/rooms/room1"
	require.NoError(t, svc.EnsureRoomViaAP(a, "A", "", "ownerA"))
	require.NoError(t, svc.EnsureRoomViaAP(b, "B", "", "ownerB"))

	roomA, err := repo.FindRoomByURI(a)
	require.NoError(t, err)
	roomB, err := repo.FindRoomByURI(b)
	require.NoError(t, err)
	assert.NotEqual(t, roomA.ID, roomB.ID, "2 ホスト分の room が同じ行になっている")
	assert.Equal(t, "ownerA", roomA.OwnerID)
	assert.Equal(t, "ownerB", roomB.OwnerID)
	require.NotNil(t, roomA.Host)
	require.NotNil(t, roomB.Host)
	assert.Equal(t, "a.example", *roomA.Host)
	assert.Equal(t, "b.example", *roomB.Host)
}

func TestEnsureRoomViaAP_IdempotentSameOwner(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "Existing", "remoteOwner")))
	// 同一 owner の既存 room には何も書き込まない (idempotent)。
	require.NoError(t, svc.EnsureRoomViaAP(remoteRoomURI("room1"), "Different", "x", "remoteOwner"))
	room, _ := repo.FindRoomByID("room1")
	assert.Equal(t, "Existing", room.Name)
}

// 同じ room URI を別の owner が名乗ってきたら拒否する (hijack ガード)。
func TestEnsureRoomViaAP_OwnerMismatchRejected(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "Existing", "remoteOwner")))
	err := svc.EnsureRoomViaAP(remoteRoomURI("room1"), "Remote", "x", "otherOwner")
	require.Error(t, err)
}

func TestCreateInvitationViaAP_CreatesAndIdempotent(t *testing.T) {
	svc, repo := newRoomFedService(t)
	// **room を実在させること。** `chat_room_invitation.roomId` は `chat_room(id)`
	// への FK なので、本番では room の無い招待行は作れない。
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser"))
	inv, err := repo.FindInvitation("localUser", "room1")
	require.NoError(t, err)
	assert.Equal(t, "room1", inv.RoomID)
	firstID := inv.ID
	// 2 回目は no-op (重複作成しない)。
	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser"))
	inv2, _ := repo.FindInvitation("localUser", "room1")
	assert.Equal(t, firstID, inv2.ID)
}

// recordingInvitationNotifier captures OnChatRoomInvitationReceived calls (#1559)。
type recordingInvitationNotifier struct{ calls [][3]string }

func (n *recordingInvitationNotifier) OnChatRoomInvitationReceived(invitee, inviter, invID string) {
	n.calls = append(n.calls, [3]string{invitee, inviter, invID})
}

// #1559 [MEDIUM] AP 経由の招待でも local invitee へ通知し、notifier は room owner。
func TestCreateInvitationViaAP_FiresNotifier(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "R", "remoteOwner")))
	notifier := &recordingInvitationNotifier{}
	svc.SetInvitationNotifier(notifier)

	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser"))
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, "localUser", notifier.calls[0][0])
	assert.Equal(t, "remoteOwner", notifier.calls[0][1], "notifier は room owner")

	// 重複招待では通知しない (no-op)。
	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser"))
	assert.Len(t, notifier.calls, 1)
}

func TestAddMemberViaAP_RequiresPendingInvitation(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	// 招待が無い相手の Accept は membership 化しない (なりすまし防止)。
	require.NoError(t, svc.AddMemberViaAP(remoteRoomURI("room1"), "stranger"))
	_, err := repo.FindMembership("stranger", "room1")
	assert.Error(t, err, "membership should not be created without an invitation")
}

func TestAddMemberViaAP_CreatesMembershipAndConsumesInvitation(t *testing.T) {
	svc, repo := newRoomFedService(t)
	// **room を実在させること。** 無いと owner ガード (#2858) が評価されず、
	// 「全員を owner とみなす」形に壊れても緑のまま通る (= 連合 chat room への
	// 参加が全断する回帰が無検出)。本番では room は必ず存在する。
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "remoteUser"))
	require.NoError(t, svc.AddMemberViaAP(remoteRoomURI("room1"), "remoteUser"))
	_, err := repo.FindMembership("remoteUser", "room1")
	require.NoError(t, err, "membership should be created")
	// 招待は consume される。
	_, err = repo.FindInvitation("remoteUser", "room1")
	assert.Error(t, err, "invitation should be consumed")
}

// owner 宛の Accept は membership 行を作らない (#2858)。owner は暗黙のメンバー
// なので行を持たず、譲渡で招待が残っていた場合にここで実体化させない。
func TestAddMemberViaAP_OwnerDoesNotCreateMembership(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	// **招待は repository に直接入れる。** `CreateInvitationViaAP` は owner 自身
	// への招待を作らなくなったので、そちらで用意すると行が無いまま
	// 「消費された」ように見え、この test が空虚になる。ここで再現したいのは
	// 「所有権の譲渡で owner 宛の招待が残った」状態 (#2858)。
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
		ID: "inv-owner", UserID: "remoteOwner", RoomID: "room1",
	}))

	require.NoError(t, svc.AddMemberViaAP(remoteRoomURI("room1"), "remoteOwner"))

	_, err := repo.FindMembership("remoteOwner", "room1")
	assert.Error(t, err, "owner は membership 行を持たない")
	_, err = repo.FindInvitation("remoteOwner", "room1")
	assert.Error(t, err, "招待は消費する")
}

func TestRemoveInvitationViaAP_DeletesPending(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "remoteUser"))
	require.NoError(t, svc.RemoveInvitationViaAP(remoteRoomURI("room1"), "remoteUser"))
	_, err := repo.FindInvitation("remoteUser", "room1")
	assert.Error(t, err)
	// 存在しない invitation の削除は no-op。
	require.NoError(t, svc.RemoveInvitationViaAP(remoteRoomURI("room1"), "nobody"))
}

func TestFederateInvitationResponse_DeliversAcceptToRemoteOwner(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	ownerURI := "https://remote.example/users/owner"
	userRepo.Users["remoteOwner"] = &model.User{ID: "remoteOwner", Username: "owner", Host: &remoteHost, URI: &ownerURI}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	// remote owner の room copy。
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "remoteOwner")))

	svc.FederateInvitationResponse("room1", "bob", true)
	assert.Equal(t, 1, deliverer.called)
	body := string(deliverer.lastBody)
	assert.Contains(t, body, `"type":"Accept"`)
	assert.Contains(t, body, `"type":"Invite"`)
	// inner Group id は 保存済みの URI を使うした remote room URI。
	assert.Contains(t, body, "https://remote.example/chat/rooms/room1")
	// Accept の actor は応答した local invitee。
	assert.Contains(t, body, "https://local.example/users/bob")
}

func TestFederateInvitationResponse_DeliversRejectToRemoteOwner(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	ownerURI := "https://remote.example/users/owner"
	userRepo.Users["remoteOwner"] = &model.User{ID: "remoteOwner", Username: "owner", Host: &remoteHost, URI: &ownerURI}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "remoteOwner")))

	svc.FederateInvitationResponse("room1", "bob", false)
	assert.Equal(t, 1, deliverer.called)
	assert.Contains(t, string(deliverer.lastBody), `"type":"Reject"`)
}

func TestFederateInvitationResponse_LocalRoomIsNoOp(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	userRepo.Users["localOwner"] = &model.User{ID: "localOwner", Username: "owner"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	// 自前 (local owner) の room は federation しない。
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "localOwner")))

	svc.FederateInvitationResponse("room1", "bob", true)
	assert.Equal(t, 0, deliverer.called)
}

func TestFederateInvitationResponse_NoOpWhenRoomMissing(t *testing.T) {
	svc, _, _, deliverer := newInvitationService(t)
	svc.FederateInvitationResponse("ghost", "bob", true)
	assert.Equal(t, 0, deliverer.called)
}

func TestFederateInvitationResponse_OwnerWithoutURIIsNoOp(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	// remote owner だが URI 未設定 → 配送できないので no-op。
	userRepo.Users["remoteOwner"] = &model.User{ID: "remoteOwner", Username: "owner", Host: &remoteHost}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "remoteOwner")))

	svc.FederateInvitationResponse("room1", "bob", true)
	assert.Equal(t, 0, deliverer.called)
}

func TestFederateInvitationResponse_BadOwnerURIIsNoOp(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	badURI := "not-a-url"
	// **`uri` を持たない行に owner がリモート、という状態では名乗らない。**
	// migration がリモート room を全て埋めるので本来は現れないが、現れたら身元が
	// 分からない — ローカルの正規 URI を返すと「その room はうちのもの」になる。
	userRepo.Users["remoteOwner"] = &model.User{ID: "remoteOwner", Username: "owner", Host: &remoteHost, URI: &badURI}
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "General", OwnerID: "remoteOwner"}))

	svc.FederateInvitationResponse("room1", "bob", true)
	assert.Equal(t, 0, deliverer.called)
}

func TestFederateInvitationResponse_DeliveryFailureSwallowed(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	deliverer.returnErr = assert.AnError
	remoteHost := "remote.example"
	ownerURI := "https://remote.example/users/owner"
	userRepo.Users["remoteOwner"] = &model.User{ID: "remoteOwner", Username: "owner", Host: &remoteHost, URI: &ownerURI}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "remoteOwner")))

	// 配送失敗しても panic/propagate しない。
	svc.FederateInvitationResponse("room1", "bob", false)
	assert.Equal(t, 1, deliverer.called)
}

func TestFederateInvitationResponse_UnwiredIsNoOp(t *testing.T) {
	// AP delivery 未配線の service では何もしない (panic しない)。
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "General", "remoteOwner")))
	svc.FederateInvitationResponse("room1", "bob", true)
}

func TestCreateMessageToRoom_DeliversToRemoteMembers(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	bobURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	// alice (local owner) の room に remote member bob。**ローカル room なので
	// host / uri は NULL** (#2994)。
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "General", OwnerID: "alice"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "mem1", UserID: "bob", RoomID: "room1"}))

	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "room1", "hi room", "")
	require.NoError(t, err)
	assert.Equal(t, 1, deliverer.called)
	body := string(deliverer.lastBody)
	assert.Contains(t, body, `"_misskey_talk":true`)
	assert.Contains(t, body, "https://local.example/chat/rooms/room1")
	assert.Contains(t, body, "https://remote.example/users/bob")
	assert.Contains(t, body, "hi room")
}

func TestCreateMessageToRoom_LocalOnlyRoomNoFederation(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "alice")))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "mem1", UserID: "carol", RoomID: "room1"}))

	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "room1", "hi", "")
	require.NoError(t, err)
	// remote member が居ないので配送しない。
	assert.Equal(t, 0, deliverer.called)
}

func TestCreateMessageToRoom_RemoteRoomUsesStoredURI(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	ownerURI := "https://remote.example/users/owner"
	userRepo.Users["owner"] = &model.User{ID: "owner", Username: "owner", Host: &remoteHost, URI: &ownerURI}
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	// remote owner の room copy に local member alice が投稿する。
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "owner")))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "mem1", UserID: "alice", RoomID: "room1"}))

	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "room1", "hi", "")
	require.NoError(t, err)
	assert.Equal(t, 1, deliverer.called)
	// room URI は 保存済みの URI を使うした remote URI になる。
	assert.Contains(t, string(deliverer.lastBody), "https://remote.example/chat/rooms/room1")
}

func TestCreateMessageToRoom_DeliveryFailureSwallowed(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	deliverer.returnErr = assert.AnError
	remoteHost := "remote.example"
	bobURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "alice")))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "mem1", UserID: "bob", RoomID: "room1"}))

	// 配送失敗してもメッセージ作成は成功する。
	msg, err := svc.CreateMessageToRoom(context.Background(), "alice", "room1", "hi", "")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 1, deliverer.called)
}

func TestCreateRoomMessageViaAP_PersistsForMember(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "General", "localOwner")))
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "mem1", UserID: "rmt", RoomID: "room1"}))
	sender := &model.User{ID: "rmt", Username: "rmt"}

	uri := "https://remote.example/chat/messages/m1"
	err := svc.CreateRoomMessageViaAP(uri, sender, remoteRoomURI("room1"), "hi room", "")
	require.NoError(t, err)
	stored, ferr := repo.FindMessageByURI(uri)
	require.NoError(t, ferr)
	assert.Equal(t, "rmt", stored.FromUserID)
	require.NotNil(t, stored.ToRoomID)
	assert.Equal(t, "room1", *stored.ToRoomID)
}

func TestCreateRoomMessageViaAP_DedupByURI(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "General", "rmt")))
	sender := &model.User{ID: "rmt", Username: "rmt"}
	uri := "https://remote.example/chat/messages/m1"
	require.NoError(t, svc.CreateRoomMessageViaAP(uri, sender, remoteRoomURI("room1"), "hi", ""))
	// 同一 URI の再送は重複作成しない (AP retry 対策)。
	require.NoError(t, svc.CreateRoomMessageViaAP(uri, sender, remoteRoomURI("room1"), "hi", ""))
	assert.Len(t, repo.Messages, 1)
}

func TestCreateRoomMessageViaAP_UnknownRoom(t *testing.T) {
	svc, _ := newRoomFedService(t)
	sender := &model.User{ID: "rmt", Username: "rmt"}
	err := svc.CreateRoomMessageViaAP("https://remote.example/chat/messages/m1", sender,
		remoteRoomURI("ghost"), "hi", "")
	assert.ErrorIs(t, err, corechat.ErrNotFound)

	// **host を持たない URI は room の身元にならない。** id だけでローカル room に
	// 届いてしまう形なので、not-found ではなく不正な指定として弾く。
	err = svc.CreateRoomMessageViaAP("https://remote.example/chat/messages/m2", sender,
		"/chat/rooms/ghost", "hi", "")
	assert.ErrorIs(t, err, corechat.ErrInvalidTarget)
}

func TestCreateRoomMessageViaAP_NonMemberForbidden(t *testing.T) {
	svc, repo := newRoomFedService(t)
	// owner = localOwner、sender rmt は member でない。
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "General", "localOwner")))
	sender := &model.User{ID: "rmt", Username: "rmt"}
	err := svc.CreateRoomMessageViaAP("https://remote.example/chat/messages/m1", sender, remoteRoomURI("room1"), "hi", "")
	assert.ErrorIs(t, err, corechat.ErrForbidden)
}

func TestLeaveRoom_DeliversRemoveToRemoteMembers(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	bobURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"} // local owner
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"} // local, leaving
	// ローカル room (alice が owner) なので host / uri は NULL。
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "G", OwnerID: "alice"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "bob", RoomID: "room1"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m2", UserID: "carol", RoomID: "room1"}))

	require.NoError(t, svc.LeaveRoom(context.Background(), "carol", "room1"))
	// membership は削除される。
	_, err := chatRepo.FindMembership("carol", "room1")
	assert.Error(t, err)
	// Remove が remote member bob へ配送される。
	assert.Equal(t, 1, deliverer.called)
	body := string(deliverer.lastBody)
	assert.Contains(t, body, `"type":"Remove"`)
	assert.Contains(t, body, "https://local.example/chat/rooms/room1")
	assert.Contains(t, body, "https://local.example/users/carol")
}

// **メンバーでないなら配送しない。** 検査が無いと、任意の利用者が room ID を
// 指定するだけで、その room の remote メンバー全員へ自分の署名付き Remove を
// 配送させられる (受信側は冪等 no-op なので実害は配送量だが、増幅になる)。
func TestLeaveRoom_NonMemberDoesNotFederate(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	bobURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	userRepo.Users["mallory"] = &model.User{ID: "mallory", Username: "mallory"}
	// ローカル room (alice が owner) なので host / uri は NULL。
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "G", OwnerID: "alice"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "bob", RoomID: "room1"}))

	require.NoError(t, svc.LeaveRoom(context.Background(), "mallory", "room1"))

	assert.Equal(t, 0, deliverer.called, "非メンバーの leave で Remove を配送しない")
	_, err := chatRepo.FindMembership("bob", "room1")
	assert.NoError(t, err, "他人の membership は消えない")
}

// **DB 障害を「メンバーではない」に丸めない** (#2792)。丸めると、障害の間だけ
// leave が黙って成功扱いになり、membership が残ったまま利用者には「退出した」と
// 見える。
func TestLeaveRoom_MembershipLookupFailureIsError(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	// ローカル room (alice が owner) なので host / uri は NULL。
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "G", OwnerID: "alice"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "bob", RoomID: "room1"}))
	chatRepo.FindMembershipErr = errors.New("db down")

	require.Error(t, svc.LeaveRoom(context.Background(), "bob", "room1"))

	chatRepo.FindMembershipErr = nil
	_, err := chatRepo.FindMembership("bob", "room1")
	assert.NoError(t, err, "失敗時に membership を消さない")
	assert.Equal(t, 0, deliverer.called)
}

func TestLeaveRoom_LocalOnlyNoFederation(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	// ローカル room (alice が owner) なので host / uri は NULL。
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "G", OwnerID: "alice"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "carol", RoomID: "room1"}))

	require.NoError(t, svc.LeaveRoom(context.Background(), "carol", "room1"))
	_, err := chatRepo.FindMembership("carol", "room1")
	assert.Error(t, err)
	// remote member が居ないので配送しない。
	assert.Equal(t, 0, deliverer.called)
}

func TestLeaveRoom_RemoteLeaverDoesNotFederate(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	rmtURI := "https://remote.example/users/rmt"
	bobURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["rmt"] = &model.User{ID: "rmt", Username: "rmt", Host: &remoteHost, URI: &rmtURI}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	// ローカル room (alice が owner) なので host / uri は NULL。
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "G", OwnerID: "alice"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "rmt", RoomID: "room1"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m2", UserID: "bob", RoomID: "room1"}))

	// remote user の leave は当人の instance が配送する (我々は受信側)。membership
	// は削除するが本インスタンスからは Remove を送らない。
	require.NoError(t, svc.LeaveRoom(context.Background(), "rmt", "room1"))
	assert.Equal(t, 0, deliverer.called)
}

func TestLeaveRoom_InvalidArgs(t *testing.T) {
	svc, _ := newRoomFedService(t)
	assert.ErrorIs(t, svc.LeaveRoom(context.Background(), "", "room1"), corechat.ErrInvalidTarget)
	assert.ErrorIs(t, svc.LeaveRoom(context.Background(), "u1", ""), corechat.ErrInvalidTarget)
}

func TestLeaveRoom_RoomMissingStillDeletesMembership(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	// room を作らない (FindRoomByID 失敗) → membership 削除のみ、federation skip。
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "carol", RoomID: "ghost"}))
	require.NoError(t, svc.LeaveRoom(context.Background(), "carol", "ghost"))
	_, err := chatRepo.FindMembership("carol", "ghost")
	assert.Error(t, err)
	assert.Equal(t, 0, deliverer.called)
}

func TestLeaveRoom_RemoteRoomUsesStoredURI(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	ownerURI := "https://remote.example/users/owner"
	userRepo.Users["owner"] = &model.User{ID: "owner", Username: "owner", Host: &remoteHost, URI: &ownerURI}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"} // local, leaving
	// remote owner の room copy。carol(local) が leave → remote owner へ Remove。
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "G", "owner")))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "carol", RoomID: "room1"}))

	require.NoError(t, svc.LeaveRoom(context.Background(), "carol", "room1"))
	assert.Equal(t, 1, deliverer.called)
	// room URI は 保存済みの URI を使うした remote URI。
	assert.Contains(t, string(deliverer.lastBody), "https://remote.example/chat/rooms/room1")
}

func TestLeaveRoom_DeliveryFailureSwallowed(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	deliverer.returnErr = assert.AnError
	remoteHost := "remote.example"
	bobURI := "https://remote.example/users/bob"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	// ローカル room (alice が owner) なので host / uri は NULL。
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "G", OwnerID: "alice"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "bob", RoomID: "room1"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m2", UserID: "carol", RoomID: "room1"}))

	// 配送失敗しても LeaveRoom は成功し membership は削除される。
	require.NoError(t, svc.LeaveRoom(context.Background(), "carol", "room1"))
	assert.Equal(t, 1, deliverer.called)
}

func TestLeaveRoom_UnwiredNoFederation(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "G", "alice")))
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "carol", RoomID: "room1"}))
	// AP delivery 未配線でも panic せず membership 削除のみ。
	require.NoError(t, svc.LeaveRoom(context.Background(), "carol", "room1"))
	_, err := repo.FindMembership("carol", "room1")
	assert.Error(t, err)
}

func TestRemoveMemberViaAP_DeletesMembershipIdempotent(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "G", "localOwner")))
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "rmt", RoomID: "room1"}))

	require.NoError(t, svc.RemoveMemberViaAP(remoteRoomURI("room1"), "rmt"))
	_, err := repo.FindMembership("rmt", "room1")
	assert.Error(t, err)
	// 存在しない membership の削除は no-op。
	require.NoError(t, svc.RemoveMemberViaAP(remoteRoomURI("room1"), "rmt"))
	// 引数欠落は ErrInvalidTarget。
	assert.ErrorIs(t, svc.RemoveMemberViaAP("", "rmt"), corechat.ErrInvalidTarget)
}

func TestCreateMessageToRoom_RemoteSenderDoesNotFederate(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	senderURI := "https://remote.example/users/rmt"
	bobURI := "https://remote.example/users/bob"
	userRepo.Users["rmt"] = &model.User{ID: "rmt", Username: "rmt", Host: &remoteHost, URI: &senderURI}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	// remote owner の room copy。sender 自身も remote member。
	require.NoError(t, chatRepo.CreateRoom(remoteRoomRow("room1", "General", "rmt")))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "mem1", UserID: "bob", RoomID: "room1"}))

	// remote sender が起点のメッセージは本インスタンスからは federation しない。
	_, err := svc.CreateMessageToRoom(context.Background(), "rmt", "room1", "hi", "")
	require.NoError(t, err)
	assert.Equal(t, 0, deliverer.called)
}

// **DB 障害を not-found に丸めない** (#2792)。丸めると一過性のエラーの間だけ
// owner ガードが素通りし、owner に membership 行ができる。
func TestAddMemberViaAP_RoomLookupFailureIsError(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "remoteUser"))
	repo.FindRoomErr = errors.New("db down")

	require.Error(t, svc.AddMemberViaAP(remoteRoomURI("room1"), "remoteUser"))

	repo.FindRoomErr = nil
	_, err := repo.FindMembership("remoteUser", "room1")
	assert.Error(t, err, "失敗時に membership を作らない")
}

// **DB 障害を「招待が無い」に丸めない** (#2792)。丸めると inbox の job が成功
// 扱いで retry されず、membership が永久に作られないまま相手だけが「参加した」
// と認識したままになる。
func TestAddMemberViaAP_InvitationLookupFailureIsError(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "remoteUser"))
	repo.FindInvitationErr = errors.New("db down")

	require.Error(t, svc.AddMemberViaAP(remoteRoomURI("room1"), "remoteUser"))

	repo.FindInvitationErr = nil
	_, err := repo.FindMembership("remoteUser", "room1")
	assert.Error(t, err, "失敗時に membership を作らない")
}

// --- #2994: 自ホストの room URI と取り込んだ copy を引き分ける ---

// **自分の room への Accept は通ること。** こちらが remote user を招待すると、
// 相手は**こちらが送った URI** (自ホスト) で Accept を返す。取り込んだ copy しか
// 見ない実装だと、その Accept が丸ごと落ちて membership が永久に作られない。
func TestAddMemberViaAP_ResolvesOwnRoomByLocalURI(t *testing.T) {
	svc, repo, _, _ := newInvitationService(t)
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "Mine", OwnerID: "localOwner"}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "rmt", RoomID: "room1"}))

	require.NoError(t, svc.AddMemberViaAP("https://local.example/chat/rooms/room1", "rmt"))

	_, err := repo.FindMembership("rmt", "room1")
	require.NoError(t, err, "自分の room への Accept が membership にならない")
}

// **他ホストの綴りで来ても、こちらの room への Accept は通ること。**
//
// 相手がこちらの送った URI をそのまま返すとは限らない — 自分の copy から
// `${config.url}/chat/rooms/${room.id}` を組み立て直す実装は**相手の host +
// こちらの room id** を送ってくる。host まで一致を求めると、#2994 以前は通っていた
// 相手との interop が落ちる。
//
// **安全性は URI ではなく招待が担保する** (下のテスト)。
func TestAddMemberViaAP_AcceptsOwnRoomUnderPeerHostSpelling(t *testing.T) {
	svc, repo, _, _ := newInvitationService(t)
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "Mine", OwnerID: "localOwner"}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "rmt", RoomID: "room1"}))

	require.NoError(t, svc.AddMemberViaAP("https://peer.example/chat/rooms/room1", "rmt"))
	_, err := repo.FindMembership("rmt", "room1")
	require.NoError(t, err, "相手の綴りで来た Accept を落としている")
}

// **招待が無ければ membership は作られない。** room の引き当てを緩めても、
// なりすまし参加はここで止まる (認可は URI ではなく招待が持つ)。
func TestAddMemberViaAP_WithoutInvitationStaysOut(t *testing.T) {
	svc, repo, _, _ := newInvitationService(t)
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "Mine", OwnerID: "localOwner"}))

	require.NoError(t, svc.AddMemberViaAP("https://evil.example/chat/rooms/room1", "stranger"))
	_, err := repo.FindMembership("stranger", "room1")
	assert.Error(t, err, "招待が無いのに membership を作っている")
}

// 取り込んだ copy を「自ホストの room」として id で掴まないこと。
func TestResolveRoom_DoesNotGrabCopyByID(t *testing.T) {
	svc, repo, _, _ := newInvitationService(t)
	// ローカルで採番した id がたまたま自ホスト URI の id と一致する copy。
	uri := "https://remote.example/chat/rooms/origin1"
	host := "remote.example"
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{
		ID: "room1", Name: "Copy", OwnerID: "remoteOwner", Host: &host, URI: &uri,
	}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "rmt", RoomID: "room1"}))

	err := svc.AddMemberViaAP("https://local.example/chat/rooms/room1", "rmt")
	require.ErrorIs(t, err, corechat.ErrNotFound)
	_, ferr := repo.FindMembership("rmt", "room1")
	assert.Error(t, ferr)
}

// **自ホストの URI を名乗る Invite は取り込まない。** 取り込むと「ローカル room
// なのに uri が付いた行」ができ、以後その room への Accept が URI 経路と id 経路の
// どちらでも引けてしまう。federation 側の sameDeliveryHost が先に落とすが、
// 判断は書き込みの隣にも置く。
func TestEnsureRoomViaAP_RejectsOwnHostURI(t *testing.T) {
	svc, repo, _, _ := newInvitationService(t)
	err := svc.EnsureRoomViaAP("https://local.example/chat/rooms/room1", "X", "", "remoteOwner")
	require.ErrorIs(t, err, corechat.ErrInvalidTarget)
	_, ferr := repo.FindRoomByURI("https://local.example/chat/rooms/room1")
	assert.Error(t, ferr, "自ホストの URI で room を作っている")
}

// URL builder が未配線でも、こちらの room への Accept は id で引き当てられること。
// (`s.urls` は `EnsureRoomViaAP` の自ホスト拒否にだけ要る。)
func TestResolveRoom_WithoutURLBuilderStillResolvesLocalRooms(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "Mine", OwnerID: "localOwner"}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "rmt", RoomID: "room1"}))

	require.NoError(t, svc.AddMemberViaAP("https://local.example/chat/rooms/room1", "rmt"))
	_, err := repo.FindMembership("rmt", "room1")
	require.NoError(t, err)
}

// **綴りが違っても同じ room として引き当てること (#2994 レビュー M1)。**
// 比較側 (`sameDeliveryHost`) は punycode と既定ポートを畳むので、畳まずに保存すると
// **同じ room が別々の identity になり**、Invite で作った行に本文が届かなくなる。
func TestEnsureRoomViaAP_CanonicalizesURISpelling(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, svc.EnsureRoomViaAP("https://Remote.Example:443/chat/rooms/room1", "G", "", "remoteOwner"))

	// 別綴りで来ても同じ行に当たる (2 行目は作られない)。
	require.NoError(t, svc.EnsureRoomViaAP("https://remote.example/chat/rooms/room1", "G", "", "remoteOwner"))

	room, err := repo.FindRoomByURI("https://remote.example/chat/rooms/room1")
	require.NoError(t, err)
	require.NotNil(t, room.Host)
	assert.Equal(t, "remote.example", *room.Host, "host に既定ポートや大文字が残っている")
	assert.Len(t, repo.Rooms, 1, "綴り違いで room が 2 行できている")

	// **引き当て側も畳むこと。** 保存だけ畳んでも、別綴りで来た本文が
	// 「知らない room」になって恒久的に落ちる。
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "rmt", RoomID: room.ID}))
	require.NoError(t, svc.AddMemberViaAP("https://REMOTE.example:443/chat/rooms/room1", "rmt"))
	_, merr := repo.FindMembership("rmt", room.ID)
	require.NoError(t, merr, "別綴りの URI で同じ room を引けていない")
}

// **room URI が分からない room へは配送しない (#2994 レビュー L1)。**
// `@context` は `omitempty` なので、空のまま送ると room の印が落ちて**受信側が
// 1-on-1 DM として取り込む** (`to` の先頭 1 人宛の私信になる)。
func TestCreateMessageToRoom_NoDeliveryWhenRoomURIUnknown(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	bobURI := "https://remote.example/users/bob"
	badOwnerURI := "not-a-url"
	userRepo.Users["remoteOwner"] = &model.User{ID: "remoteOwner", Username: "owner", Host: &remoteHost, URI: &badOwnerURI}
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	// `uri` を持たないのに owner がリモート、という身元の分からない行。
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "G", OwnerID: "remoteOwner"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "alice", RoomID: "room1"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m2", UserID: "bob", RoomID: "room1"}))

	// 送るのはローカルの alice、受け取る相手にリモートの bob が居る。
	_, err := svc.CreateMessageToRoom(context.Background(), "alice", "room1", "hi", "")
	require.NoError(t, err)
	assert.Equal(t, 0, deliverer.called, "room が分からないまま配送している (DM に化ける)")
}

// 退出の Remove も同じ — どの room の Remove か伝わらないなら送らない。
func TestLeaveRoom_NoDeliveryWhenRoomURIUnknown(t *testing.T) {
	svc, chatRepo, userRepo, deliverer := newInvitationService(t)
	remoteHost := "remote.example"
	bobURI := "https://remote.example/users/bob"
	badOwnerURI := "not-a-url"
	userRepo.Users["remoteOwner"] = &model.User{ID: "remoteOwner", Username: "owner", Host: &remoteHost, URI: &badOwnerURI}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &remoteHost, URI: &bobURI}
	require.NoError(t, chatRepo.CreateRoom(&model.ChatRoom{ID: "room1", Name: "G", OwnerID: "remoteOwner"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "carol", RoomID: "room1"}))
	require.NoError(t, chatRepo.CreateMembership(&model.ChatRoomMembership{ID: "m2", UserID: "bob", RoomID: "room1"}))

	require.NoError(t, svc.LeaveRoom(context.Background(), "carol", "room1"))
	assert.Equal(t, 0, deliverer.called, "room が分からないまま Remove を送っている")
}

// **room の引き当てで起きた DB 障害を「知らない room」に丸めない (#2792)。**
// 丸めると inbox job が ack されて、招待や退出が黙って落ちる。
func TestResolveRoom_IDLookupFailureIsError(t *testing.T) {
	repo := &roomIDLookupFailingRepo{
		MockChatRepository: testutil.NewMockChatRepository(),
		err:                errors.New("dial tcp: connection refused"),
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(repo, idGen)

	err := svc.RemoveMemberViaAP("https://remote.example/chat/rooms/room1", "rmt")
	require.Error(t, err)
	assert.NotErrorIs(t, err, corechat.ErrNotFound, "DB 障害が not-found に丸められている")
	assert.ErrorIs(t, err, repo.err)
}

// roomIDLookupFailingRepo lets FindRoomByURI miss normally while FindRoomByID
// fails, so the id-lookup guard is reachable on its own.
type roomIDLookupFailingRepo struct {
	*testutil.MockChatRepository
	err error
}

func (r *roomIDLookupFailingRepo) FindRoomByID(string) (*model.ChatRoom, error) { return nil, r.err }
