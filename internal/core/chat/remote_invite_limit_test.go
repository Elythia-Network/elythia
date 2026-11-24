package chat_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// リモートの招待した人 (room の owner) ごとに、1 日に取り込む room の写しを
// ローカルの chat/rooms/create と同じ 10 までにする。既にある room への招待と、
// 別の人の room は数えない。
func TestEnsureRoomViaAP_LimitsRoomsPerInviter(t *testing.T) {
	svc, _ := newRoomFedService(t)
	for i := range 10 {
		require.NoError(t, svc.EnsureRoomViaAP(remoteRoomURI(fmt.Sprintf("lim%d", i)), "R", "", "remoteOwner"))
	}
	err := svc.EnsureRoomViaAP(remoteRoomURI("lim10"), "R", "", "remoteOwner")
	assert.ErrorIs(t, err, corechat.ErrRemoteInviteLimit)

	require.NoError(t, svc.EnsureRoomViaAP(remoteRoomURI("lim0"), "R", "", "remoteOwner"),
		"既に取り込んだ room への招待は数えない")
	require.NoError(t, svc.EnsureRoomViaAP(remoteRoomURI("other0"), "R", "", "otherOwner"),
		"別の人の上限は別に数える")
}

// 1 日より前に取り込んだ room の写しは数えない。
func TestEnsureRoomViaAP_RoomLimitWindow(t *testing.T) {
	svc, repo := newRoomFedService(t)
	idGen, _ := id.NewGenerator("aidx")
	old := time.Now().Add(-25 * time.Hour)
	for i := range 10 {
		row := remoteRoomRow(fmt.Sprintf("old%d", i), "R", "remoteOwner")
		row.ID = idGen.Generate(old)
		require.NoError(t, repo.CreateRoom(row))
	}
	require.NoError(t, svc.EnsureRoomViaAP(remoteRoomURI("fresh"), "R", "", "remoteOwner"))
}

// 招待した人ごとに、1 日に受ける招待をローカルの chat/rooms/invitations/create と
// 同じ 50 までにする。room の定員 (50) より先に効くよう、2 つの room に分けて招く。
// 上限を超えた招待は行も通知も作らない。
func TestCreateInvitationViaAP_LimitsInvitationsPerInviter(t *testing.T) {
	svc, repo := newRoomFedService(t)
	roomA := seedRemoteRoom(t, repo, "roomA", "remoteOwner")
	roomB := seedRemoteRoom(t, repo, "roomB", "remoteOwner")
	for i := range 25 {
		require.NoError(t, svc.CreateInvitationViaAP(roomA, fmt.Sprintf("a%d", i)))
		require.NoError(t, svc.CreateInvitationViaAP(roomB, fmt.Sprintf("b%d", i)))
	}
	notifier := &recordingInvitationNotifier{}
	svc.SetInvitationNotifier(notifier)

	err := svc.CreateInvitationViaAP(roomB, "late")
	assert.ErrorIs(t, err, corechat.ErrRemoteInviteLimit)
	_, ferr := repo.FindInvitation("late", "roomB")
	assert.Error(t, ferr, "上限を超えた招待の行を作らない")
	assert.Empty(t, notifier.calls, "上限を超えた招待は通知しない")

	roomC := seedRemoteRoom(t, repo, "roomC", "otherOwner")
	require.NoError(t, svc.CreateInvitationViaAP(roomC, "late"), "別の人の上限は別に数える")
}

// 1 日より前の招待は数えない。
func TestCreateInvitationViaAP_InvitationLimitWindow(t *testing.T) {
	svc, repo := newRoomFedService(t)
	roomA := seedRemoteRoom(t, repo, "roomA", "remoteOwner")
	seedRemoteRoom(t, repo, "roomB", "remoteOwner")
	idGen, _ := id.NewGenerator("aidx")
	old := time.Now().Add(-25 * time.Hour)
	for i := range 50 {
		require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
			ID: idGen.Generate(old), UserID: fmt.Sprintf("o%d", i), RoomID: "roomB",
		}))
	}
	require.NoError(t, svc.CreateInvitationViaAP(roomA, "fresh"))
}
