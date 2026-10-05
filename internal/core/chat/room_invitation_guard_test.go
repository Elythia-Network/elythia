package chat_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// seedRemoteRoom creates the local copy of a remote room the way the inbox
// does (EnsureRoomViaAP) so invitation guards run against a real room row.
// Returns the room's AP URI — **AP 経路の引数は room id ではなく URI** (#2994)。
//
// id を固定して作るので、招待や membership の assert は今までどおり id で書ける。
// 取り込み経路そのものを試すテストだけが、採番された id を URI から引き直す。
func seedRemoteRoom(t *testing.T, repo *testutil.MockChatRepository, roomID, ownerID string) string {
	t.Helper()
	uri := remoteRoomURI(roomID)
	host := testRemoteRoomHost
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{
		ID: roomID, Name: "R", OwnerID: ownerID, Host: &host, URI: &uri,
	}))
	return uri
}

// remoteRoomRow builds a room copy row (host / uri set) the way the inbox does.
func remoteRoomRow(roomID, name, ownerID string) *model.ChatRoom {
	uri := remoteRoomURI(roomID)
	host := testRemoteRoomHost
	return &model.ChatRoom{ID: roomID, Name: name, OwnerID: ownerID, Host: &host, URI: &uri}
}

// 招待者 (= room owner) を block している local invitee へは、AP 経由の招待でも
// 行を作らない。1-on-1 DM (CreateMessageViaAP) と同じ gate を room 招待にも
// 適用する。通知も発火しないこと (招待行が無ければ通知の中身も引けない)。
func TestCreateInvitationViaAP_BlockedInviteeCreatesNoRow(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	blocks := testutil.NewMockBlockingRepository()
	// blocker=localUser (invitee), blockee=remoteOwner (inviter)。
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "localUser", BlockeeID: "remoteOwner"}))
	svc.SetBlockingRepo(blocks)
	notifier := &recordingInvitationNotifier{}
	svc.SetInvitationNotifier(notifier)

	err := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	assert.ErrorIs(t, err, corechat.ErrChatBlocked)
	_, ferr := repo.FindInvitation("localUser", "room1")
	assert.Error(t, ferr, "blocked invitation must not create a row")
	assert.Empty(t, notifier.calls, "blocked invitation must not notify")
}

// block の向きを取り違えていないこと: owner が invitee を block していても
// 招待自体は通る (DM 側の checkBlocked(to, from) と同じ向き)。
func TestCreateInvitationViaAP_ReverseBlockStillInvites(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	blocks := testutil.NewMockBlockingRepository()
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "remoteOwner", BlockeeID: "localUser"}))
	svc.SetBlockingRepo(blocks)

	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser"))
	_, err := repo.FindInvitation("localUser", "room1")
	assert.NoError(t, err, "逆向きの block は招待を止めない")
}

// 無関係な block 行があるだけで招待が止まらないこと (述語が広すぎないか)。
func TestCreateInvitationViaAP_UnrelatedBlockStillInvites(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	blocks := testutil.NewMockBlockingRepository()
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "localUser", BlockeeID: "someoneElse"}))
	svc.SetBlockingRepo(blocks)

	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser"))
	_, err := repo.FindInvitation("localUser", "room1")
	assert.NoError(t, err)
}

// block 判定が引けないときは fail-closed (招待を作らない)。
func TestCreateInvitationViaAP_BlockCheckFailClosed(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	blocks := testutil.NewMockBlockingRepository()
	blocks.ExistsErr = errors.New("db down")
	svc.SetBlockingRepo(blocks)

	err := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	require.Error(t, err)
	assert.NotErrorIs(t, err, corechat.ErrChatBlocked, "一過性エラーを block 判定に丸めない")
	_, ferr := repo.FindInvitation("localUser", "room1")
	assert.Error(t, ferr, "fail-closed: 判定不能なら行を作らない")
}

// room が存在しない招待は作らない (`chat_room_invitation.roomId` は FK)。
// ErrNotFound は呼び出し側で non-retry に落ちる。
func TestCreateInvitationViaAP_RoomMustExist(t *testing.T) {
	svc, repo := newRoomFedService(t)
	err := svc.CreateInvitationViaAP(remoteRoomURI("ghost"), "localUser")
	assert.ErrorIs(t, err, corechat.ErrNotFound)
	_, ferr := repo.FindInvitation("localUser", "ghost")
	assert.Error(t, ferr)
}

// **DB 障害を not-found に丸めない** (#2792)。丸めると一過性のエラーが
// 「そんな room は無い」として non-retry で drop される。
func TestCreateInvitationViaAP_RoomLookupFailureIsError(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	boom := errors.New("db down")
	repo.FindRoomErr = boom

	err := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, corechat.ErrNotFound)
}

// owner 自身への招待は行を作らない (upstream 'yourself')。
func TestCreateInvitationViaAP_OwnerSelfInviteCreatesNoRow(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	notifier := &recordingInvitationNotifier{}
	svc.SetInvitationNotifier(notifier)

	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "remoteOwner"))

	_, err := repo.FindInvitation("remoteOwner", "room1")
	assert.Error(t, err, "owner 自身への招待は作らない")
	assert.Empty(t, notifier.calls)
}

// 既にメンバーなら招待を作らない (upstream 'already member')。
func TestCreateInvitationViaAP_ExistingMemberCreatesNoRow(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "localUser", RoomID: "room1"}))
	notifier := &recordingInvitationNotifier{}
	svc.SetInvitationNotifier(notifier)

	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser"))

	_, err := repo.FindInvitation("localUser", "room1")
	assert.Error(t, err, "既存メンバーへは招待を作らない")
	assert.Empty(t, notifier.calls)
}

// membership 判定の DB 障害を「メンバーではない」に丸めない (#2792)。
func TestCreateInvitationViaAP_MembershipLookupFailureIsError(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	boom := errors.New("db down")
	repo.FindMembershipErr = boom

	err := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	assert.ErrorIs(t, err, boom)
	_, ferr := repo.FindInvitation("localUser", "room1")
	assert.Error(t, ferr)
}

// 定員 (upstream MAX_ROOM_MEMBERS = 50) を AP 経路にも効かせる。
func TestCreateInvitationViaAP_RoomFullRejected(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	for i := 0; i < 50; i++ {
		require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
			ID: fmt.Sprintf("m%d", i), UserID: fmt.Sprintf("member%d", i), RoomID: "room1",
		}))
	}

	err := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	assert.ErrorIs(t, err, corechat.ErrRoomFull)
	_, ferr := repo.FindInvitation("localUser", "room1")
	assert.Error(t, ferr, "満室の room へは招待を作らない")
}

// 未消化の招待も定員に数える。数えないと、誰も accept しない room 1 つで
// invitation 行と通知を無制限に作れる (remote 側は room id を自由に決められる)。
func TestCreateInvitationViaAP_PendingInvitationsCountTowardCapacity(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	for i := 0; i < 50; i++ {
		require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
			ID: fmt.Sprintf("i%02d", i), UserID: fmt.Sprintf("victim%d", i), RoomID: "room1",
		}))
	}

	err := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	assert.ErrorIs(t, err, corechat.ErrRoomFull)
	_, ferr := repo.FindInvitation("localUser", "room1")
	assert.Error(t, ferr)
}

// 定員手前 (members + pending = 49) では通ること。境界で 1 つずれていると
// 上の 2 つは緑のままなので、通る側も固定する。
func TestCreateInvitationViaAP_JustUnderCapacityStillInvites(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	for i := 0; i < 25; i++ {
		require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
			ID: fmt.Sprintf("m%d", i), UserID: fmt.Sprintf("member%d", i), RoomID: "room1",
		}))
	}
	for i := 0; i < 24; i++ {
		require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
			ID: fmt.Sprintf("i%02d", i), UserID: fmt.Sprintf("invitee%d", i), RoomID: "room1",
		}))
	}

	require.NoError(t, svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser"))
	_, err := repo.FindInvitation("localUser", "room1")
	assert.NoError(t, err)
}

// メンバー列挙が引けないときは fail-closed (定員判定を素通りさせない)。
func TestCreateInvitationViaAP_MemberListFailureIsError(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	repo.ListMembersErr = errors.New("db down")

	err := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	require.Error(t, err)
	_, ferr := repo.FindInvitation("localUser", "room1")
	assert.Error(t, ferr)
}

// 既存招待の検索が失敗したら、そのまま作りに行かない (#2792)。
func TestCreateInvitationViaAP_InvitationLookupFailureIsError(t *testing.T) {
	svc, repo := newRoomFedService(t)
	seedRemoteRoom(t, repo, "room1", "remoteOwner")
	boom := errors.New("db down")
	repo.FindInvitationErr = boom

	err := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	assert.ErrorIs(t, err, boom)
}

// invitationListFailingRepo makes ListInvitationsByRoom fail while every other
// call behaves like the normal mock.
//
// `testutil.MockChatRepository` はこの呼び出しの error 注入口を持たないので、
// ここで包む (testutil は他の担当と共有するため触らない)。
type invitationListFailingRepo struct {
	*testutil.MockChatRepository
	err error
}

func (r *invitationListFailingRepo) ListInvitationsByRoom(string, string, string, int) ([]*model.ChatRoomInvitation, error) {
	return nil, r.err
}

// 未消化の招待を数えられないときも fail-closed (定員判定を素通りさせない)。
func TestCreateInvitationViaAP_InvitationListFailureIsError(t *testing.T) {
	base := newFakeRepo()
	require.NoError(t, base.CreateRoom(remoteRoomRow("room1", "R", "remoteOwner")))
	boom := errors.New("db down")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := corechat.NewService(&invitationListFailingRepo{MockChatRepository: base, err: boom}, idGen)

	cerr := svc.CreateInvitationViaAP(remoteRoomURI("room1"), "localUser")

	assert.ErrorIs(t, cerr, boom)
	_, ferr := base.FindInvitation("localUser", "room1")
	assert.Error(t, ferr, "fail-closed: 数えられないなら行を作らない")
}
