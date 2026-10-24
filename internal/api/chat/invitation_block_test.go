package chat

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

func newInvitationBlockHandler(t *testing.T) (*Handler, *testutil.MockChatRepository, *testutil.MockBlockingRepository) {
	t.Helper()
	h, repo := newTestHandler()
	blocks := testutil.NewMockBlockingRepository()
	svc := corechat.NewService(repo, h.idGen, nil, nil, blocks)
	h.SetService(svc)
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "General", OwnerID: u1.ID, Owner: u1}))
	return h, repo, blocks
}

// 本家 createRoomInvitation と同じく、どちらかが相手をブロックしていれば招待を
// 作らない。本家は素の Error ('blocked' / 'blocking') なので応答は 500。
func TestInvitationsCreate_BlockedEitherWay(t *testing.T) {
	tests := []struct {
		name             string
		blocker, blockee string
	}{
		{name: "invitee blocks inviter", blocker: "u2", blockee: "u1"},
		{name: "inviter blocks invitee", blocker: "u1", blockee: "u2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, blocks := newInvitationBlockHandler(t)
			require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: tc.blocker, BlockeeID: tc.blockee}))
			notifier := &recordingInvitationNotifier{}
			h.SetInvitationNotifier(notifier)

			rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Empty(t, repo.Invitations, "ブロックの間柄では招待の行を作らない")
			assert.Empty(t, notifier.calls, "通知も出さない")
		})
	}
}

// ブロックの判定が引けないときも招待を作らない (fail-closed)。
func TestInvitationsCreate_BlockLookupErrorFailsClosed(t *testing.T) {
	h, repo, blocks := newInvitationBlockHandler(t)
	blocks.ExistsErr = errMock

	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, repo.Invitations)
}

// 第三者どうしのブロックは招待に関係しない (境界の反対側)。
func TestInvitationsCreate_UnrelatedBlockDoesNotPrevent(t *testing.T) {
	h, repo, blocks := newInvitationBlockHandler(t)
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "u2", BlockeeID: "u3"}))
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b2", BlockerID: "u3", BlockeeID: "u1"}))

	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Invitations, 1)
}

// 招待の承諾 (invitations/accept) と参加 (rooms/join) でも、ルームの持ち主との
// どちらの向きのブロックも見る (Elythia の追加)。ブロックの時に招待を消せずに
// 残っても、それで参加できないようにする。応答は招待の作成と同じ 500 で、招待は残す。
func TestAcceptAndJoin_BlockedEitherWay(t *testing.T) {
	endpoints := []struct {
		name    string
		handler func(h *Handler) func(echo.Context) error
	}{
		{name: "invitations/accept", handler: func(h *Handler) func(echo.Context) error { return h.InvitationsAccept }},
		{name: "rooms/join", handler: func(h *Handler) func(echo.Context) error { return h.RoomsJoin }},
	}
	blocks := []struct {
		name             string
		blocker, blockee string
	}{
		{name: "member blocks owner", blocker: "u2", blockee: "u1"},
		{name: "owner blocks member", blocker: "u1", blockee: "u2"},
	}
	for _, ep := range endpoints {
		for _, b := range blocks {
			t.Run(ep.name+"/"+b.name, func(t *testing.T) {
				h, repo, br := newInvitationBlockHandler(t)
				require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "u2", RoomID: "r1"}))
				require.NoError(t, br.Create(&model.Blocking{ID: "b1", BlockerID: b.blocker, BlockeeID: b.blockee}))

				rec := post(ep.handler(h), `{"roomId":"r1"}`, u2)
				assert.Equal(t, http.StatusInternalServerError, rec.Code)
				assert.Empty(t, repo.Memberships, "ブロックの間柄では参加させない")
				assert.Len(t, repo.Invitations, 1, "招待は残す")
			})
		}
		t.Run(ep.name+"/lookup error fails closed", func(t *testing.T) {
			h, repo, br := newInvitationBlockHandler(t)
			require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "u2", RoomID: "r1"}))
			br.ExistsErr = errMock

			rec := post(ep.handler(h), `{"roomId":"r1"}`, u2)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Empty(t, repo.Memberships)
		})
		t.Run(ep.name+"/unrelated block does not prevent", func(t *testing.T) {
			h, repo, br := newInvitationBlockHandler(t)
			require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "u2", RoomID: "r1"}))
			require.NoError(t, br.Create(&model.Blocking{ID: "b1", BlockerID: "u2", BlockeeID: "u3"}))

			rec := post(ep.handler(h), `{"roomId":"r1"}`, u2)
			assert.Equal(t, http.StatusNoContent, rec.Code)
			assert.Len(t, repo.Memberships, 1)
			assert.Empty(t, repo.Invitations)
		})
	}
}

// サービスが未配線ならブロックの判定をしない (テストや起動途中の構成)。
func TestBlockedWithRoomOwner_NoService(t *testing.T) {
	h, _ := newTestHandler()
	assert.False(t, h.blockedWithRoomOwner("u2", &model.ChatRoom{ID: "r1", OwnerID: "u1"}))
}
