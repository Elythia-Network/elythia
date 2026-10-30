package chat

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvitationsCreate_RemoteInviteRequiresFederationBeforeInsert(t *testing.T) {
	host := "remote.example"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			h, repo := newTestHandler()
			repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u1.ID}
			userRepo := testutil.NewMockUserRepository()
			userRepo.Users[u1.ID] = u1
			userRepo.Users[u2.ID] = u2
			userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
			h.SetUserRepo(userRepo)
			actor := *u1
			actor.FederationStatus = status

			rec := post(h.InvitationsCreate, `{"roomId":"room","userId":"remote"}`, &actor)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
			assert.Empty(t, repo.Invitations, "refused remote invitation must be rejected before persistence")
		})
	}
}

func TestInvitationsCreate_LocalInviteRemainsAvailableWhenFederationIsOff(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u1.ID}
	actor := *u1
	actor.FederationStatus = model.FederationStatusOff

	rec := post(h.InvitationsCreate, `{"roomId":"room","userId":"u2"}`, &actor)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Invitations, 1)
}

func TestInvitationsAccept_RemoteRoomRequiresFederationBeforeMembership(t *testing.T) {
	host := "remote.example"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			h, repo := newTestHandler()
			repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: "remote-owner", Host: &host}
			repo.Invitations["invitation"] = &model.ChatRoomInvitation{ID: "invitation", UserID: u1.ID, RoomID: "room"}
			actor := *u1
			actor.FederationStatus = status

			rec := post(h.InvitationsAccept, `{"roomId":"room"}`, &actor)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
			assert.Empty(t, repo.Memberships, "refused remote accept must not create membership")
			assert.Len(t, repo.Invitations, 1, "refused remote accept must preserve the invitation")
		})
	}
}

func TestRoomsJoin_RemoteRoomRequiresFederationBeforeMembership(t *testing.T) {
	host := "remote.example"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			h, repo := newTestHandler()
			repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: "remote-owner", Host: &host}
			repo.Invitations["invitation"] = &model.ChatRoomInvitation{ID: "invitation", UserID: u1.ID, RoomID: "room"}
			actor := *u1
			actor.FederationStatus = status

			rec := post(h.RoomsJoin, `{"roomId":"room"}`, &actor)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
			assert.Empty(t, repo.Memberships, "refused remote join must not create membership")
			assert.Len(t, repo.Invitations, 1, "refused remote join must preserve the invitation")
		})
	}
}

// **ローカル所有のルームでも、リモートの参加者がいれば連合 OFF・未確定の利用者は
// 入れない (2026-10-31 の監査)。** 入れると全メンバーの投稿のリモート配送が deliver
// の preflight で黙って止まっていた (実測: 参加前は届き、参加後は届かず、抜けると戻る)。
func TestRoomsJoin_LocalRoomWithRemoteMemberRefusesUnavailableUser(t *testing.T) {
	host := "remote.example"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			h, repo := newTestHandler()
			repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u2.ID}
			repo.Memberships["remote:room"] = &model.ChatRoomMembership{ID: "m-remote", UserID: "remote", RoomID: "room"}
			repo.Invitations["invitation"] = &model.ChatRoomInvitation{ID: "invitation", UserID: u1.ID, RoomID: "room"}
			userRepo := testutil.NewMockUserRepository()
			userRepo.Users[u1.ID] = u1
			userRepo.Users[u2.ID] = u2
			userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
			h.SetUserRepo(userRepo)
			actor := *u1
			actor.FederationStatus = status

			rec := post(h.RoomsJoin, `{"roomId":"room"}`, &actor)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
			assert.Len(t, repo.Memberships, 1, "refused join must not create membership")
			assert.Len(t, repo.Invitations, 1, "refused join must preserve the invitation")
		})
	}
}

// リモートの参加者がいないローカル所有のルームには、連合 OFF でも入れる。
func TestRoomsJoin_LocalRoomWithoutRemoteMembersAdmitsUnavailableUser(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u2.ID}
	repo.Invitations["invitation"] = &model.ChatRoomInvitation{ID: "invitation", UserID: u1.ID, RoomID: "room"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[u1.ID] = u1
	userRepo.Users[u2.ID] = u2
	h.SetUserRepo(userRepo)
	actor := *u1
	actor.FederationStatus = model.FederationStatusOff

	rec := post(h.RoomsJoin, `{"roomId":"room"}`, &actor)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Len(t, repo.Memberships, 1)
}

// 連合 OFF・未確定の利用者を、リモートの参加者がいるルームへ招待できない。
func TestInvitationsCreate_UnavailableLocalInviteeIntoFederatedRoomIsRefused(t *testing.T) {
	host := "remote.example"
	h, repo := newTestHandler()
	repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u1.ID}
	repo.Memberships["remote:room"] = &model.ChatRoomMembership{ID: "m-remote", UserID: "remote", RoomID: "room"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[u1.ID] = u1
	offUser := *u2
	offUser.FederationStatus = model.FederationStatusOff
	userRepo.Users[u2.ID] = &offUser
	userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
	h.SetUserRepo(userRepo)

	rec := post(h.InvitationsCreate, `{"roomId":"room","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
	assert.Empty(t, repo.Invitations, "refused invitation must not be persisted")
}

// 連合 OFF・未確定のメンバーがいるルームへ、リモートの利用者を招待できない。
func TestInvitationsCreate_RemoteInviteeIntoRoomWithUnavailableMemberIsRefused(t *testing.T) {
	host := "remote.example"
	h, repo := newTestHandler()
	repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u1.ID}
	repo.Memberships["u2:room"] = &model.ChatRoomMembership{ID: "m-off", UserID: u2.ID, RoomID: "room"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[u1.ID] = u1
	offUser := *u2
	offUser.FederationStatus = model.FederationStatusOff
	userRepo.Users[u2.ID] = &offUser
	userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
	h.SetUserRepo(userRepo)

	rec := post(h.InvitationsCreate, `{"roomId":"room","userId":"remote"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
	assert.Empty(t, repo.Invitations, "refused invitation must not be persisted")
}

// invitations/accept も rooms/join と同じく、リモートの参加者がいるローカル所有の
// ルームへ連合 OFF・未確定の利用者を入れない (招待を受けてから OFF に切り替えて
// 承諾する経路。2026-10-31 の監査のレビュー)。
func TestInvitationsAccept_LocalRoomWithRemoteMemberRefusesUnavailableUser(t *testing.T) {
	host := "remote.example"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			h, repo := newTestHandler()
			repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u2.ID}
			repo.Memberships["remote:room"] = &model.ChatRoomMembership{ID: "m-remote", UserID: "remote", RoomID: "room"}
			repo.Invitations["invitation"] = &model.ChatRoomInvitation{ID: "invitation", UserID: u1.ID, RoomID: "room"}
			userRepo := testutil.NewMockUserRepository()
			userRepo.Users[u1.ID] = u1
			userRepo.Users[u2.ID] = u2
			userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
			h.SetUserRepo(userRepo)
			actor := *u1
			actor.FederationStatus = status

			rec := post(h.InvitationsAccept, `{"roomId":"room"}`, &actor)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
			assert.Len(t, repo.Memberships, 1, "refused accept must not create membership")
			assert.Len(t, repo.Invitations, 1, "refused accept must preserve the invitation")
		})
	}
}

// リモートの参加者は、未承諾の招待にもいる。招待が 1 ページ (100 件) を超えても
// 最後まで引くこと。
func TestRoomsJoin_RemoteInviteeBeyondTheFirstPageIsSeen(t *testing.T) {
	host := "remote.example"
	h, repo := newTestHandler()
	repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u2.ID}
	repo.Invitations["invitation"] = &model.ChatRoomInvitation{ID: "zzz-invitation", UserID: u1.ID, RoomID: "room"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[u1.ID] = u1
	userRepo.Users[u2.ID] = u2
	for i := range 150 {
		id := "local" + strconv.Itoa(i)
		userRepo.Users[id] = &model.User{ID: id, Username: id, FederationStatus: model.FederationStatusOn}
		// id の降順で 100 件より後ろに来るよう、リモートの招待を一番小さい id にする。
		repo.Invitations["inv-"+id] = &model.ChatRoomInvitation{ID: "inv-" + string(rune('a'+i/26)) + string(rune('a'+i%26)), UserID: id, RoomID: "room"}
	}
	userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
	repo.Invitations["inv-remote"] = &model.ChatRoomInvitation{ID: "inv-0remote", UserID: "remote", RoomID: "room"}
	h.SetUserRepo(userRepo)
	actor := *u1
	actor.FederationStatus = model.FederationStatusOff

	rec := post(h.RoomsJoin, `{"roomId":"room"}`, &actor)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
}

// 所有権の譲渡でも、リモートの参加者がいるルームの owner を連合 OFF・未確定の
// 利用者にしない (join / invitations と同じ判定。2026-10-31 の監査のレビュー)。
func TestRoomsTransferOwnership_RefusesUnavailableOwnerOfFederatedRoom(t *testing.T) {
	host := "remote.example"
	h, repo := newTestHandler()
	repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: u1.ID}
	repo.Memberships["remote:room"] = &model.ChatRoomMembership{ID: "m-remote", UserID: "remote", RoomID: "room"}
	repo.Memberships["u2:room"] = &model.ChatRoomMembership{ID: "m-u2", UserID: u2.ID, RoomID: "room"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[u1.ID] = u1
	offUser := *u2
	offUser.FederationStatus = model.FederationStatusOff
	userRepo.Users[u2.ID] = &offUser
	userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
	h.SetUserRepo(userRepo)

	rec := post(h.RoomsTransferOwnership, `{"roomId":"room","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertErrorCode(t, rec, "FEDERATION_DISABLED", "ff6844bb-b2d5-4e8c-a3d1-14e0ac15d5a1")
	assert.Equal(t, u1.ID, repo.Rooms["room"].OwnerID, "refused transfer must keep the owner")
}
