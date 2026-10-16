package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingLiteFiller records each FillLites call and marks every lite so the
// test can see that the filled value reaches the response.
type recordingLiteFiller struct {
	calls [][]string
}

func (f *recordingLiteFiller) FillLites(users []*model.User, lites []*entity.UserLite) {
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
		lites[i].Emojis["filled_"+u.ID] = "https://media.example/" + u.ID + ".png"
	}
	f.calls = append(f.calls, ids)
}

// requireUserLite asserts v is a JSON object that satisfies the misskey-js
// UserLite contract, including avatarDecorations / emojis / onlineStatus.
func requireUserLite(t *testing.T, v any, wantID string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	require.True(t, ok, "UserLite must be an object, got %T", v)
	assert.Equal(t, wantID, m["id"])
	_, ok = m["avatarDecorations"].([]any)
	assert.True(t, ok, "avatarDecorations must be an array, got %T", m["avatarDecorations"])
	_, ok = m["emojis"].(map[string]any)
	assert.True(t, ok, "emojis must be an object, got %T", m["emojis"])
	// misskey-js の UserLite.onlineStatus は 4 値の enum。
	assert.Contains(t, []any{"unknown", "online", "active", "offline"}, m["onlineStatus"])
	shapetest.Assert(t, "UserLite", m)
	return m
}

func TestRoomsOwned_ReturnsOwnerAsUserLite(t *testing.T) {
	h, repo := newTestHandler()
	// createdAt は ID から作るので、ID は実際の生成器で作る。
	roomID := h.idGen.Generate(time.Now())
	repo.Rooms[roomID] = &model.ChatRoom{ID: roomID, Name: "Mine", OwnerID: u1.ID, Owner: u1}

	rec := post(h.RoomsOwned, `{}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	shapetest.Assert(t, "ChatRoom", rows[0])
	requireUserLite(t, rows[0]["owner"], u1.ID)
}

// TestRoomTimeline_UserLitesFilledInOneBatch pins that every UserLite of a
// list response (fromUser, reactions[].user, toRoom.owner) has the shared
// UserLite shape and that instance / emojis are resolved with a single
// FillLites call for the whole response, not one per user (N+1).
func TestRoomTimeline_UserLitesFilledInOneBatch(t *testing.T) {
	h, repo := newTestHandler()
	filler := &recordingLiteFiller{}
	h.SetLiteFiller(filler)
	require.True(t, h.HasLiteFiller())

	room := &model.ChatRoom{ID: "r1", Name: "Room", OwnerID: u1.ID, Owner: u1}
	repo.Rooms["r1"] = room
	roomID := room.ID
	text := "hi"
	for _, id := range []string{"m1", "m2"} {
		repo.Messages[id] = &model.ChatMessage{
			ID: id, FromUserID: u2.ID, FromUser: u2, ToRoomID: &roomID, ToRoom: room, Text: &text,
			Reactions: model.StringArray{"u1/👍"},
		}
	}

	rec := post(h.RoomTimeline, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)

	require.Len(t, filler.calls, 1, "instance / emojis must be resolved in one batch per response")
	// 2 件 × (fromUser + reactions[].user + toRoom.owner + room.owner) = 8。
	assert.Len(t, filler.calls[0], 8)

	for _, row := range rows {
		from := requireUserLite(t, row["fromUser"], u2.ID)
		assert.Contains(t, from["emojis"], "filled_u2", "filled value must reach fromUser")
		reactions, ok := row["reactions"].([]any)
		require.True(t, ok)
		require.Len(t, reactions, 1)
		ru := requireUserLite(t, reactions[0].(map[string]any)["user"], u1.ID)
		assert.Contains(t, ru["emojis"], "filled_u1", "filled value must reach reactions[].user")
		toRoom, ok := row["toRoom"].(map[string]any)
		require.True(t, ok)
		owner := requireUserLite(t, toRoom["owner"], u1.ID)
		assert.Contains(t, owner["emojis"], "filled_u1", "filled value must reach toRoom.owner")
	}
}

func TestMessagesShow_ToUserIsUserLite(t *testing.T) {
	h, repo := newTestHandler()
	to := u2.ID
	text := "dm"
	repo.Messages["m1"] = &model.ChatMessage{
		ID: "m1", FromUserID: u1.ID, FromUser: u1, ToUserID: &to, ToUser: u2, Text: &text,
		Reactions: model.StringArray{},
	}
	rec := post(h.MessagesShow, `{"messageId":"m1"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	requireUserLite(t, resp["fromUser"], u1.ID)
	requireUserLite(t, resp["toUser"], u2.ID)
}

func TestPackInvitationByID_UserIsFilled(t *testing.T) {
	h, repo := newTestHandler()
	filler := &recordingLiteFiller{}
	h.SetLiteFiller(filler)
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "General", OwnerID: u1.ID, Owner: u1}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "inv1", UserID: u2.ID, RoomID: "r1"}))

	packed, ok := h.PackInvitationByID("inv1", u2.ID)
	require.True(t, ok)
	raw, err := json.Marshal(packed)
	require.NoError(t, err)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(raw, &resp))
	user := requireUserLite(t, resp["user"], u2.ID)
	assert.Contains(t, user["emojis"], "filled_u2")
	require.Len(t, filler.calls, 1)
}

// TestMessagesUpdate_TextLength pins the 2000-character limit of
// chat/messages/update (same as create-to-user / create-to-room), counted in
// characters, on both the service path and the legacy path.
func TestMessagesUpdate_TextLength(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantCode int
	}{
		{"exactly 2000 multibyte characters are accepted", strings.Repeat("あ", 2000), http.StatusNoContent},
		{"2001 characters are rejected", strings.Repeat("a", 2001), http.StatusBadRequest},
	}
	setups := []struct {
		name  string
		setup func() (*Handler, map[string]*model.ChatMessage)
	}{
		{"service", func() (*Handler, map[string]*model.ChatMessage) {
			h, repo := newTestHandlerWithService()
			return h, repo.Messages
		}},
		{"legacy", func() (*Handler, map[string]*model.ChatMessage) {
			h, repo := newTestHandler()
			return h, repo.Messages
		}},
	}
	for _, s := range setups {
		for _, tc := range cases {
			t.Run(s.name+"/"+tc.name, func(t *testing.T) {
				h, msgs := s.setup()
				old := "old"
				to := u2.ID
				msgs["m1"] = &model.ChatMessage{ID: "m1", FromUserID: u1.ID, ToUserID: &to, Text: &old}
				body, err := json.Marshal(map[string]any{"messageId": "m1", "text": tc.text})
				require.NoError(t, err)
				rec := post(h.MessagesUpdate, string(body), u1)
				require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
				if tc.wantCode == http.StatusBadRequest {
					assertErrorCode(t, rec, "INVALID_PARAM", apierr.UUIDInvalidParam)
					assert.Equal(t, "old", *msgs["m1"].Text, "rejected update must not change the text")
				} else {
					assert.Equal(t, tc.text, *msgs["m1"].Text)
				}
			})
		}
	}
}

// inboxRepo serves fixed rows from ListInvitationsByUser (the shared mock
// returns none).
type inboxRepo struct {
	*testutil.MockChatRepository
	rows []*model.ChatRoomInvitation
}

func (r *inboxRepo) ListInvitationsByUser(_ string, _ bool, _, _ string, _ int) ([]*model.ChatRoomInvitation, error) {
	return r.rows, nil
}

// TestInvitationsInbox_ShapeIsChatRoomInvitation pins each inbox row as
// upstream ChatRoomInvitation: createdAt and user (the invitee, i.e. the
// viewer) are required.
func TestInvitationsInbox_ShapeIsChatRoomInvitation(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	roomID := idGen.Generate(time.Now())
	invID := idGen.Generate(time.Now())
	room := &model.ChatRoom{ID: roomID, Name: "room", OwnerID: u1.ID, Owner: u1}
	repo := &inboxRepo{
		MockChatRepository: testutil.NewMockChatRepository(),
		rows:               []*model.ChatRoomInvitation{{ID: invID, UserID: u2.ID, RoomID: roomID, Room: room}},
	}
	h := NewHandler(repo, idGen)
	filler := &recordingLiteFiller{}
	h.SetLiteFiller(filler)

	rec := post(h.InvitationsInbox, `{}`, u2)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	// golden に ChatRoomInvitation が無いので、本家 packedChatRoomInvitationSchema の
	// 必須キーを直接確かめる。
	for _, k := range []string{"id", "createdAt", "userId", "user", "roomId", "room"} {
		assert.Contains(t, rows[0], k)
	}
	assert.Equal(t, invID, rows[0]["id"])
	assert.Equal(t, u2.ID, rows[0]["userId"])
	assert.Equal(t, roomID, rows[0]["roomId"])
	createdAt, ok := rows[0]["createdAt"].(string)
	require.True(t, ok)
	_, err := time.Parse(time.RFC3339, createdAt)
	assert.NoError(t, err, "createdAt must be a date-time")
	user := requireUserLite(t, rows[0]["user"], u2.ID)
	assert.Contains(t, user["emojis"], "filled_u2")
	room0, ok := rows[0]["room"].(map[string]any)
	require.True(t, ok)
	requireUserLite(t, room0["owner"], u1.ID)
}

// countingUserRepo counts FindManyByIDs calls.
type countingUserRepo struct {
	*testutil.MockUserRepository
	findMany int
}

func (r *countingUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	r.findMany++
	return r.MockUserRepository.FindManyByIDs(ids)
}

// TestRoomTimeline_ReactionUsersResolvedOncePerList pins that the reacting
// users of a list response are resolved with one query for the whole list,
// and that the response equals packing each message on its own.
func TestRoomTimeline_ReactionUsersResolvedOncePerList(t *testing.T) {
	h, repo := newTestHandler()
	users := &countingUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	users.Users[u1.ID] = u1
	users.Users[u2.ID] = u2
	h.SetUserRepo(users)

	room := &model.ChatRoom{ID: "r1", Name: "Room", OwnerID: u1.ID, Owner: u1}
	repo.Rooms["r1"] = room
	roomID := room.ID
	// u2 は真ん中のメッセージにだけ付ける (先頭・末尾のどちらから並んでも、
	// 1 件目の利用者だけ引く実装では解決できない)。
	reactions := []model.StringArray{{"u1/👍"}, {"u2/🎉", "ghost/😀"}, {"u1/🎉"}}
	msgs := make([]*model.ChatMessage, 0, len(reactions))
	for i, r := range reactions {
		m := &model.ChatMessage{
			ID: fmt.Sprintf("m%d", i), FromUserID: u2.ID, FromUser: u2, ToRoomID: &roomID, ToRoom: room,
			Reactions: r,
		}
		repo.Messages[m.ID] = m
		msgs = append(msgs, m)
	}

	rec := post(h.RoomTimeline, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, users.findMany, "reacting users must be resolved once per list, not per message")

	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 3)

	// 応答は 1 件ずつ pack したものと同じ (まとめ引きで中身が変わらない)。
	byID := make(map[string]*model.ChatMessage, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}
	for _, row := range got {
		single, err := json.Marshal(h.packMessageDetailed(byID[row["id"].(string)], u1.ID))
		require.NoError(t, err)
		var want map[string]any
		require.NoError(t, json.Unmarshal(single, &want))
		assert.Equal(t, want, row)
	}
	// ghost (未解決) の reaction は落ちる。
	for _, row := range got {
		if row["id"] == "m1" {
			assert.Len(t, row["reactions"], 1)
		}
	}
}

// failingManyUsers fails FindManyByIDs.
type failingManyUsers struct {
	*testutil.MockUserRepository
}

func (failingManyUsers) FindManyByIDs([]string) ([]*model.User, error) {
	return nil, errors.New("db down")
}

// TestPackMessages_ReactionLookupFailureDropsReactions pins the existing
// behaviour kept by the batched lookup: when the reacting users cannot be
// loaded, every reaction is dropped (reactions[].user is required), instead of
// degrading to the user-less {reaction} shape.
func TestPackMessages_ReactionLookupFailureDropsReactions(t *testing.T) {
	h, _ := newTestHandler()
	h.SetUserRepo(failingManyUsers{MockUserRepository: testutil.NewMockUserRepository()})
	roomID := "r1"
	msgs := []*model.ChatMessage{{ID: "m1", FromUserID: u2.ID, ToRoomID: &roomID, Reactions: model.StringArray{"u1/👍"}}}
	out := h.packMessagesDetailed(msgs, u1.ID)
	require.Len(t, out, 1)
	assert.Equal(t, []map[string]any{}, out[0]["reactions"])
}

// detailedOnlyRepo returns ToUser / ToRoom only from FindMessageDetailedByID,
// like the real repository (FindMessageByID does not load them).
type detailedOnlyRepo struct {
	*testutil.MockChatRepository
}

func (r *detailedOnlyRepo) FindMessageByID(id string) (*model.ChatMessage, error) {
	m, err := r.MockChatRepository.FindMessageByID(id)
	if err != nil {
		return nil, err
	}
	m.ToUser, m.ToRoom = nil, nil
	return m, nil
}

// TestMessagesShow_UsesDetailedLookup pins that chat/messages/show reads the
// message with its recipient and room loaded (upstream show.ts returns
// packMessageDetailed with toUser / toRoom).
func TestMessagesShow_UsesDetailedLookup(t *testing.T) {
	repo := &detailedOnlyRepo{MockChatRepository: testutil.NewMockChatRepository()}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen)
	to := u2.ID
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: u1.ID, FromUser: u1, ToUserID: &to, ToUser: u2}

	rec := post(h.MessagesShow, `{"messageId":"m1"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	requireUserLite(t, resp["toUser"], u2.ID)
}
