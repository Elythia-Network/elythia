package chat

import (
	"net/http"
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
