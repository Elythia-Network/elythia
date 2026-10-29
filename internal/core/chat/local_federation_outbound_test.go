package chat_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type localFedApprovedChats struct{}

func (localFedApprovedChats) Create(*model.ChatApproval) error { return nil }
func (localFedApprovedChats) Delete(string, string) error      { return nil }
func (localFedApprovedChats) Exists(string, string) (bool, error) {
	return true, nil
}
func (localFedApprovedChats) ListByUser(string) ([]*model.ChatApproval, error) { return nil, nil }

func TestCreateMessageToUser_OffAndPendingCannotPersistRemoteDM(t *testing.T) {
	host := "remote.example"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			svc, repo, _ := newSvc(t)
			svc.SetApprovalRepo(localFedApprovedChats{})
			userRepo := testutil.NewMockUserRepository()
			userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice", FederationStatus: status}
			userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
			svc.SetUserRepo(userRepo)

			_, err := svc.CreateMessageToUser(context.Background(), "alice", "remote", "hello", "")
			require.ErrorIs(t, err, corechat.ErrFederationDisabled)
			assert.Empty(t, repo.Messages, "refused remote DM must be rejected before persistence")
		})
	}
}

func TestCreateMessageToRoom_OffAndPendingCannotPersistWhenRoomHasRemoteMember(t *testing.T) {
	host := "remote.example"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			repo := newFakeRepo()
			idGen, _ := id.NewGenerator("aidx")
			svc := corechat.NewService(repo, idGen, nil, nil, nil)
			userRepo := testutil.NewMockUserRepository()
			userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice", FederationStatus: status}
			userRepo.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host}
			urls := activitypub.NewURLBuilder("https://local.example")
			svc.SetAPDelivery(userRepo, activitypub.NewRenderer(urls), urls, &fakeAPDeliverer{})
			repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: "alice"}
			repo.Memberships["remote:room"] = &model.ChatRoomMembership{ID: "m1", UserID: "remote", RoomID: "room"}

			_, err := svc.CreateMessageToRoom(context.Background(), "alice", "room", "hello", "")
			require.ErrorIs(t, err, corechat.ErrFederationDisabled)
			assert.Empty(t, repo.Messages, "refused room message must be rejected before persistence")
		})
	}
}

func TestCreateMessageToUser_LocalDMRemainsAvailableWhenFederationIsOff(t *testing.T) {
	svc, repo, _ := newSvc(t)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice", FederationStatus: model.FederationStatusOff}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", FederationStatus: model.FederationStatusOn}
	svc.SetUserRepo(userRepo)

	msg, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", "hello", "")
	require.NoError(t, err)
	assert.NotNil(t, repo.Messages[msg.ID])
}

func TestCreateMessageToRoom_LocalRoomRemainsAvailableWhenFederationIsOff(t *testing.T) {
	repo := newFakeRepo()
	idGen, _ := id.NewGenerator("aidx")
	svc := corechat.NewService(repo, idGen, nil, nil, nil)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice", FederationStatus: model.FederationStatusOff}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", FederationStatus: model.FederationStatusOn}
	urls := activitypub.NewURLBuilder("https://local.example")
	svc.SetAPDelivery(userRepo, activitypub.NewRenderer(urls), urls, &fakeAPDeliverer{})
	repo.Rooms["room"] = &model.ChatRoom{ID: "room", OwnerID: "alice"}
	repo.Memberships["bob:room"] = &model.ChatRoomMembership{ID: "m1", UserID: "bob", RoomID: "room"}

	msg, err := svc.CreateMessageToRoom(context.Background(), "alice", "room", "hello", "")
	require.NoError(t, err)
	assert.NotNil(t, repo.Messages[msg.ID], "local-only accounts may still use local rooms")
}
