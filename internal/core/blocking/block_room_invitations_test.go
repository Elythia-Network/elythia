package blocking_test

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// failingRoomInvitationRemover fails every deletion.
type failingRoomInvitationRemover struct{ calls int }

func (r *failingRoomInvitationRemover) DeleteInvitationsToOwnedRooms(string, string) error {
	r.calls++
	return errStub
}

func seedRoomInvitation(t *testing.T, repo *testutil.MockChatRepository, roomID, ownerID, inviteeID string) {
	t.Helper()
	if _, ok := repo.Rooms[roomID]; !ok {
		require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: roomID, OwnerID: ownerID, Name: roomID}))
	}
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: roomID + "-" + inviteeID, RoomID: roomID, UserID: inviteeID}))
}

func remainingInvitations(repo *testutil.MockChatRepository) []string {
	out := make([]string, 0, len(repo.Invitations))
	for id := range repo.Invitations {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// 本家 UserBlockingService.block は removeRoomInvitations を双方向で呼び、
// ブロックした人のルームへのブロックされた人の招待と、その逆を消す。第三者への
// 招待と、第三者のルームへの招待は触らない。インポート (silent) でも消す。
func TestBlock_RemovesRoomInvitationsBothWays(t *testing.T) {
	for _, silent := range []bool{false, true} {
		svc, ur, _, _ := newSvc(t)
		for _, id := range []string{"alice", "bob", "carol"} {
			addUser(ur, id)
		}
		chat := testutil.NewMockChatRepository()
		seedRoomInvitation(t, chat, "aliceRoom1", "alice", "bob")
		seedRoomInvitation(t, chat, "aliceRoom2", "alice", "bob")
		seedRoomInvitation(t, chat, "aliceRoom1", "alice", "carol")
		seedRoomInvitation(t, chat, "bobRoom", "bob", "alice")
		seedRoomInvitation(t, chat, "carolRoom", "carol", "alice")
		seedRoomInvitation(t, chat, "carolRoom", "carol", "bob")
		svc.SetRoomInvitationRemoverForTest(chat)

		block := svc.Block
		if silent {
			block = svc.BlockSilent
		}
		_, err := block("alice", "bob")
		require.NoError(t, err)

		assert.Equal(t, []string{"aliceRoom1-carol", "carolRoom-alice", "carolRoom-bob"}, remainingInvitations(chat), "silent=%t", silent)
	}
}

// 招待の片付けに失敗しても block は成立し、Block も配送する (best-effort)。
// 両方の向きを試みる。
func TestBlock_RoomInvitationCleanupErrorIsBestEffort(t *testing.T) {
	svc, ur, br, _ := newSvc(t)
	addUser(ur, "alice")
	addUser(ur, "bob")
	remover := &failingRoomInvitationRemover{}
	svc.SetRoomInvitationRemoverForTest(remover)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	_, err := svc.Block("alice", "bob")
	require.NoError(t, err)
	assert.Equal(t, 2, remover.calls)
	assert.Equal(t, [][2]string{{"alice", "bob"}}, hook.blocked)
	exists, err := br.Exists("alice", "bob")
	require.NoError(t, err)
	assert.True(t, exists)
}
