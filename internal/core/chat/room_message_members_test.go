package chat_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roomMessageCountingRepo struct {
	repository.ChatRepository
	listMembersCalls    int
	failListMembersCall int
}

func (r *roomMessageCountingRepo) ListMembersByRoom(roomID string) ([]*model.ChatRoomMembership, error) {
	r.listMembersCalls++
	if r.listMembersCalls == r.failListMembersCall {
		return nil, errors.New("synthetic membership lookup failure")
	}
	return r.ChatRepository.ListMembersByRoom(roomID)
}

func newRoomMessageMembersFixture(t *testing.T, failListMembersCall int) (*corechat.Service, *roomMessageCountingRepo, *fakeAPDeliverer) {
	t.Helper()
	baseRepo := testutil.NewMockChatRepository()
	countingRepo := &roomMessageCountingRepo{
		ChatRepository:      baseRepo,
		failListMembersCall: failListMembersCall,
	}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := corechat.NewService(countingRepo, idGen, nil, nil, nil)

	userRepo := testutil.NewMockUserRepository()
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/remote"
	userRepo.Users["owner"] = &model.User{ID: "owner", Username: "owner"}
	userRepo.Users["sender"] = &model.User{ID: "sender", Username: "sender"}
	userRepo.Users["remote"] = &model.User{ID: "remote", Username: "remote", Host: &remoteHost, URI: &remoteURI}
	urls := activitypub.NewURLBuilder("https://local.example")
	deliverer := &fakeAPDeliverer{}
	svc.SetAPDelivery(userRepo, activitypub.NewRenderer(urls), urls, deliverer)
	svc.SetMainStreamPublisher(&stubMainPublisher{})

	baseRepo.Rooms["room1"] = &model.ChatRoom{ID: "room1", Name: "General", OwnerID: "owner"}
	baseRepo.Memberships["sender:room1"] = &model.ChatRoomMembership{UserID: "sender", RoomID: "room1"}
	baseRepo.Memberships["remote:room1"] = &model.ChatRoomMembership{UserID: "remote", RoomID: "room1"}
	return svc, countingRepo, deliverer
}

func TestCreateMessageToRoom_ReusesStreamMemberSnapshotForAP(t *testing.T) {
	svc, repo, deliverer := newRoomMessageMembersFixture(t, 0)
	_, err := svc.CreateMessageToRoom(context.Background(), "sender", "room1", "hello", "")
	require.NoError(t, err)
	assert.Equal(t, 1, repo.listMembersCalls, "AP delivery should reuse the successful stream membership lookup")
	assert.Equal(t, 1, deliverer.called)
	assert.Contains(t, string(deliverer.lastBody), "https://remote.example/users/remote")
}

func TestCreateMessageToRoom_RetriesAPMemberLookupAfterStreamFailure(t *testing.T) {
	svc, repo, deliverer := newRoomMessageMembersFixture(t, 1)
	_, err := svc.CreateMessageToRoom(context.Background(), "sender", "room1", "hello", "")
	require.NoError(t, err)
	assert.Equal(t, 2, repo.listMembersCalls, "AP delivery must perform its own lookup when the stream lookup fails")
	assert.Equal(t, 1, deliverer.called)
	assert.Contains(t, string(deliverer.lastBody), "https://remote.example/users/remote")
}
