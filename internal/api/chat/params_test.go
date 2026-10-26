package chat

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家の paramDef は整数の値を持つ小数 (10.0、1e1) を integer として通す (#3841)。
func TestChatParams_AcceptIntegerValuedFractions(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID, Name: "r1", Owner: u1}
	const page = `"limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0`
	for _, tc := range []struct {
		name    string
		handler func(echo.Context) error
		body    string
	}{
		{"chat/history", h.History, `{"limit":1e1}`},
		{"chat/history room", h.History, `{"room":true,"limit":10.0}`},
		{"chat/messages/search", h.MessagesSearch, `{"query":"x","limit":1e1}`},
		{"chat/messages/user-timeline", h.UserTimeline, `{"userId":"u2",` + page + `}`},
		{"chat/messages/room-timeline", h.RoomTimeline, `{"roomId":"r1",` + page + `}`},
		{"chat/rooms/owned", h.RoomsOwned, `{` + page + `}`},
		{"chat/rooms/joining", h.RoomsJoining, `{` + page + `}`},
		{"chat/rooms/members", h.RoomsMembers, `{"roomId":"r1",` + page + `}`},
		{"chat/rooms/invitations/inbox", h.InvitationsInbox, `{` + page + `}`},
		{"chat/rooms/invitations/outbox", h.InvitationsOutbox, `{"roomId":"r1",` + page + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(tc.handler, tc.body, u1)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// 小数の形の limit も、その値の件数で切る (#3841)。
func TestChatParams_FractionLimitIsHonored(t *testing.T) {
	h, repo := newTestHandler()
	for _, id := range []string{"a1", "a2", "a3"} {
		repo.Rooms[id] = &model.ChatRoom{ID: id, OwnerID: u1.ID, Name: id, Owner: u1}
	}
	assert.Len(t, decodeList(t, post(h.RoomsOwned, `{"limit":2.0}`, u1)), 2)
}

// 本家は limit の範囲外を丸めずに拒否する。検証器が飛ばす入力でも handler が同じに
// 見る (#3841)。
func TestChatParams_RejectOutOfRangeLimit(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID, Name: "r1", Owner: u1}
	for _, tc := range []struct {
		name    string
		handler func(echo.Context) error
		body    string
	}{
		{"chat/messages/search", h.MessagesSearch, `{"query":"x",%s}`},
		{"chat/messages/user-timeline", h.UserTimeline, `{"userId":"u2",%s}`},
		{"chat/messages/room-timeline", h.RoomTimeline, `{"roomId":"r1",%s}`},
		{"chat/rooms/owned", h.RoomsOwned, `{%s}`},
		{"chat/rooms/joining", h.RoomsJoining, `{%s}`},
		{"chat/rooms/members", h.RoomsMembers, `{"roomId":"r1",%s}`},
		{"chat/rooms/invitations/inbox", h.InvitationsInbox, `{%s}`},
		{"chat/rooms/invitations/outbox", h.InvitationsOutbox, `{"roomId":"r1",%s}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, limit := range []string{`"limit":0`, `"limit":-1`, `"limit":101`} {
				body := fmt.Sprintf(tc.body, limit)
				rec := post(tc.handler, body, u1)
				assert.Equal(t, http.StatusBadRequest, rec.Code, body)
				assert.Contains(t, rec.Body.String(), "INVALID_PARAM", body)
			}
		})
	}
}

// 本家の paramDef は name を required の string とするだけなので、空文字の名前でも
// 部屋を作る。キーが無ければ今までどおり 400 (#3841)。
func TestRoomsCreate_EmptyNameIsAccepted(t *testing.T) {
	h, repo := newTestHandler()
	rec := post(h.RoomsCreate, `{"name":""}`, u1)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, repo.Rooms, 1)
	for _, r := range repo.Rooms {
		assert.Empty(t, r.Name)
	}
	assert.Equal(t, http.StatusBadRequest, post(h.RoomsCreate, `{"description":"d"}`, u1).Code)
}

// 本家の paramDef は reaction を required の string とするだけなので、unreact の空の
// reaction も通す。キーが無ければ今までどおり 400 (#3841)。
func TestReactionsDelete_EmptyReactionIsAccepted(t *testing.T) {
	h, repo := newHandlerWithService(t)
	to := u1.ID
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "sender", ToUserID: &to, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	rec := post(h.ReactionsDelete, `{"messageId":"m1","reaction":""}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusBadRequest, post(h.ReactionsDelete, `{"messageId":"m1"}`, u1).Code)
}

// rooms/joined は Elythia 独自の endpoint で、limit の範囲外を丸めると宣言している
// ので、本家に揃えた範囲検査で拒否しない (#3841)。
func TestRoomsJoined_ClampsLimit(t *testing.T) {
	h, repo := newTestHandler()
	for i := range 120 {
		id := fmt.Sprintf("a%03d", i)
		repo.Rooms[id] = &model.ChatRoom{ID: id, OwnerID: u2.ID, Name: id, Owner: u2}
		repo.Memberships[u1.ID+":"+id] = &model.ChatRoomMembership{ID: "m" + id, UserID: u1.ID, RoomID: id}
	}
	for body, want := range map[string]int{
		`{"limit":0}`:   30,
		`{"limit":-1}`:  30,
		`{"limit":500}`: 100,
		`{"limit":2.0}`: 2,
		`{}`:            30,
	} {
		rec := post(h.RoomsJoined, body, u1)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", body, rec.Body.String())
		assert.Len(t, decodeList(t, rec), want, body)
	}
}
