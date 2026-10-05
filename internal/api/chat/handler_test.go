package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubModLog records moderation-log calls for chat room delete tests (#1541).
type stubModLog struct {
	types    []moderationlog.LogType
	lastInfo map[string]any
}

func (s *stubModLog) Log(_ context.Context, _ string, t moderationlog.LogType, info map[string]any) {
	s.types = append(s.types, t)
	s.lastInfo = info
}

var errMock = assert.AnError

func newTestHandler() (*Handler, *testutil.MockChatRepository) {
	repo := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen)
	// **利用者表も配線する。** `rooms/transfer-ownership` は譲渡先がローカル利用者か
	// を見る (#2994)。router は必ず配線するので、未配線のまま試すと本番と違う枝を
	// 通ることになる。
	users := testutil.NewMockUserRepository()
	users.Users[u1.ID] = u1
	users.Users[u2.ID] = u2
	h.SetUserRepo(users)
	return h, repo
}

// newTestHandlerWithService は chat Service を inject 済みの Handler を返す。
// /api/chat/rooms/show 等 service 経由の権限 gate (upstream 2026.5.4) を
// 走らせる test で利用する。
func newTestHandlerWithService() (*Handler, *testutil.MockChatRepository) {
	repo := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen)
	svc := corechat.NewService(repo, idGen)
	h.SetService(svc)
	return h, repo
}

func post(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = handler(c)
	return rec
}

// seedMember makes userID a member of roomID. 譲渡先はメンバーに限られるので
// (#2858)、transfer 系のテストはこれで前提を揃える。
func seedMember(t *testing.T, repo *testutil.MockChatRepository, userID, roomID string) {
	t.Helper()
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "seed_" + userID + "_" + roomID, UserID: userID, RoomID: roomID,
	}))
}

var u1 = &model.User{ID: "u1", Username: "alice"}
var u2 = &model.User{ID: "u2", Username: "bob"}

// assertErrorCode unmarshals an error response and asserts the Misskey error
// code + id (body shape is {"error": {code, id, message}}).
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, code, id string) {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok, "response must have error object")
	assert.Equal(t, code, errObj["code"])
	assert.Equal(t, id, errObj["id"])
}

// --- Rooms ---

func TestRoomsCreate_Success(t *testing.T) {
	h, repo := newTestHandler()
	rec := post(h.RoomsCreate, `{"name":"test room"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Rooms, 1)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "ChatRoom", resp) // L3 (#1284)
}

func TestRoomsCreate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsCreate, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRoomsCreate_Error(t *testing.T) {
	h, repo := newTestHandler()
	repo.CreateErr = errMock
	rec := post(h.RoomsCreate, `{"name":"x"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRoomsShow_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", Name: "room", OwnerID: "u1", Owner: u1}
	rec := post(h.RoomsShow, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRoomsShow_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsShow, `{"roomId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// #1771: upstream show.ts の noSuchRoom id に揃える。
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "857ae02f-8759-4d20-9adb-6e95fffe4fd7")
}

// upstream 2026.5.4 hasPermissionToViewRoomInfo gate: 他人の room に対する
// show は 404 NO_SUCH_ROOM を返す (= owner / member / 招待 / moderator 以外)。
// 旧 mk-go は権限 check 無しで誰でも room メタを取れる drop-in regression
// 状態だったので、本テストで gate を guard する (#1164 Phase C)。
func TestRoomsShow_NonOwnerNonMember(t *testing.T) {
	h, repo := newTestHandlerWithService()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", Name: "owners-only", OwnerID: "u1", Owner: u1}
	// u2 は room の owner でも member でも 招待 でもない → 404
	rec := post(h.RoomsShow, `{"roomId":"r1"}`, u2)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 招待受信者は閲覧できる (owner でも member でもない user が room を見る経路)。
func TestRoomsShow_InvitationAllowed(t *testing.T) {
	h, repo := newTestHandlerWithService()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", Name: "invited", OwnerID: "u1", Owner: u1}
	repo.Invitations["u2:r1"] = &model.ChatRoomInvitation{ID: "inv1", RoomID: "r1", UserID: "u2"}
	rec := post(h.RoomsShow, `{"roomId":"r1"}`, u2)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// member は閲覧できる。
func TestRoomsShow_MemberAllowed(t *testing.T) {
	h, repo := newTestHandlerWithService()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", Name: "with-member", OwnerID: "u1", Owner: u1}
	repo.Memberships["u2:r1"] = &model.ChatRoomMembership{UserID: "u2", RoomID: "r1"}
	rec := post(h.RoomsShow, `{"roomId":"r1"}`, u2)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRoomsShow_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsShow, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRoomsUpdate_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", Name: "old", OwnerID: "u1"}
	rec := post(h.RoomsUpdate, `{"roomId":"r1","name":"new"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "new", resp["name"])
}

func TestRoomsUpdate_NotOwner(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "other"}
	rec := post(h.RoomsUpdate, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// #1771: upstream update.ts の noSuchRoom id に揃える。
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "fcdb0f92-bda6-47f9-bd05-343e0e020932")
}

func TestRoomsUpdate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsUpdate, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type failUpdateRoomRepo struct{ *testutil.MockChatRepository }

func (f *failUpdateRoomRepo) UpdateRoom(_ *model.ChatRoom) error { return errMock }

func TestRoomsUpdate_Error(t *testing.T) {
	base := testutil.NewMockChatRepository()
	base.Rooms["r1"] = &model.ChatRoom{ID: "r1", Name: "x", OwnerID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failUpdateRoomRepo{base}, idGen)
	rec := post(h.RoomsUpdate, `{"roomId":"r1","name":"y"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRoomsDelete_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	rec := post(h.RoomsDelete, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRoomsDelete_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsDelete, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRoomsDelete_NotOwner(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "other"}
	rec := post(h.RoomsDelete, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// #1541: delete の NO_SUCH_ROOM golden id。
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "d4e3753d-97bf-4a19-ab8e-21080fbc0f4b")
}

// #1541: moderator は他人の room を削除でき、deleteChatRoom を監査ログに残す。
func TestRoomsDelete_ModeratorDeletesOthersRoom(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "other", Name: "victim"}
	h.SetModeratorChecker(fakeModeratorChecker{mods: map[string]bool{u1.ID: true}})
	modLog := &stubModLog{}
	h.SetModLog(modLog)
	rec := post(h.RoomsDelete, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, ok := repo.Rooms["r1"]
	assert.False(t, ok, "room is deleted")
	require.Len(t, modLog.types, 1)
	assert.Equal(t, moderationlog.LogDeleteChatRoom, modLog.types[0])
	assert.Equal(t, "r1", modLog.lastInfo["roomId"])
}

// #1541: owner (非 moderator) の削除は監査ログを残さない。
func TestRoomsDelete_OwnerNoModLog(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	modLog := &stubModLog{}
	h.SetModLog(modLog)
	rec := post(h.RoomsDelete, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, modLog.types, "owner delete must not write a moderation log")
}

// #1541: 非 owner かつ非 moderator は削除できない。
func TestRoomsDelete_NonOwnerNonModerator(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "other"}
	h.SetModeratorChecker(fakeModeratorChecker{mods: map[string]bool{}})
	rec := post(h.RoomsDelete, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	_, ok := repo.Rooms["r1"]
	assert.True(t, ok, "room must not be deleted by a non-owner non-moderator")
}

func TestRoomsOwned(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	rec := post(h.RoomsOwned, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRoomsJoined(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsJoined, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRoomsLeave(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsLeave, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRoomsLeave_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsLeave, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1771: mute は mute:boolean param に従う (mute:true で mute、mute:false で unmute)。
func TestRoomsMute(t *testing.T) {
	h, repo := newTestHandler()
	repo.Memberships["u1:r1"] = &model.ChatRoomMembership{UserID: "u1", RoomID: "r1"}
	rec := post(h.RoomsMute, `{"roomId":"r1","mute":true}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Memberships["u1:r1"].IsMuted)

	rec = post(h.RoomsMute, `{"roomId":"r1","mute":false}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.False(t, repo.Memberships["u1:r1"].IsMuted, "mute:false で unmute される")
}

// #1771: membership 不在は 204 でなく NO_SUCH_ROOM。
func TestRoomsMute_NoMembership(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsMute, `{"roomId":"r1","mute":true}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "c2cde4eb-8d0f-42f1-8f2f-c4d6bfc8e5df")
}

func TestRoomsMute_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsMute, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRoomsUnmute(t *testing.T) {
	h, repo := newTestHandler()
	repo.Memberships["u1:r1"] = &model.ChatRoomMembership{UserID: "u1", RoomID: "r1", IsMuted: true}
	rec := post(h.RoomsUnmute, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRoomsUnmute_NoMembership(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsUnmute, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRoomsUnmute_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsUnmute, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRoomsTransferOwnership(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	seedMember(t, repo, "u2", "r1")
	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "u2", repo.Rooms["r1"].OwnerID)
}

// 譲渡は membership 行を入れ替える。owner が行を持つ状態は upstream に存在せず、
// 読み取り側 (packRoomDetailed / isRoomMember) がその前提で書かれている (#2858)。
func TestRoomsTransferOwnership_SwapsMembershipRows(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	// 譲り受ける側は room を mute しているメンバー。
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{
		ID: "m1", UserID: "u2", RoomID: "r1", IsMuted: true,
	}))

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	_, err := repo.FindMembership("u2", "r1")
	assert.ErrorIs(t, err, testutil.ErrNotFound, "新 owner の membership 行は残らない")

	// 旧 owner が締め出されないこと。isRoomMember は
	// `ownerId == userID || membership` なので、行が無いと両方 false になる。
	old, err := repo.FindMembership("u1", "r1")
	require.NoError(t, err)
	assert.False(t, old.IsMuted)
	assert.True(t, h.isRoomMember(repo.Rooms["r1"], "u1"), "旧 owner は room に残る")

	// 入れ替えなので行数は動かない (譲渡先はメンバーに限られる)。
	members, err := repo.ListMembersByRoom("r1")
	require.NoError(t, err)
	assert.Len(t, members, 1)
}

// 自分への譲渡で自分の membership 行を作らないこと。作ると owner が行を持つ
// 状態を自ら生む。
func TestRoomsTransferOwnership_ToSelfIsNoop(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "u1", repo.Rooms["r1"].OwnerID)
	members, err := repo.ListMembersByRoom("r1")
	require.NoError(t, err)
	assert.Empty(t, members, "owner は membership 行を持たない")
}

// 同時に別の譲渡が確定した場合。もう自分の room ではないので、owner 検査に
// 落ちたときと同じ応答に揃える。
func TestRoomsTransferOwnership_LostRaceIsNoSuchRoom(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	seedMember(t, repo, "u2", "r1")
	repo.TransferOwnershipErr = testutil.ErrNotFound
	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "6ab4d7df-5043-57b9-bd5d-ff9908288473")
}

// **DB 障害を not-found に丸めない** (#2792)。
func TestRoomsTransferOwnership_RepositoryErrorIs500(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	seedMember(t, repo, "u2", "r1")
	repo.TransferOwnershipErr = errors.New("db down")
	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRoomsTransferOwnership_NotOwner(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "other"}
	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRoomsTransferOwnership_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsTransferOwnership, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Messages ---

func TestMessagesCreate_Success(t *testing.T) {
	h, repo := newTestHandler()
	text := "hello"
	rec := post(h.MessagesCreate, `{"text":"hello","toRoomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Messages, 1)
	_ = text

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "ChatMessage", resp) // L3 (#1288)
}

func TestMessagesCreate_Error(t *testing.T) {
	h, repo := newTestHandler()
	repo.CreateErr = errMock
	rec := post(h.MessagesCreate, `{"text":"x"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestMessagesCreate_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesCreate, `invalid`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessages_ListEmpty(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Messages, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMessagesSearch_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesSearch, `invalid`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesShow_WithFromUser(t *testing.T) {
	h, repo := newTestHandler()
	repo.Messages["m1"] = &model.ChatMessage{
		ID: "m1", FromUserID: "u1", FromUser: u1,
		Reads: model.StringArray{}, Reactions: model.StringArray{"u2/👍"},
	}
	rec := post(h.MessagesShow, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "alice")
	// misskey_dart の ChatMessage.fromJson は reactions を {reaction} object の
	// 配列として cast する。"<userId>/<reaction>" 文字列のまま返すと落ちる (#1246)。
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	reactions, ok := body["reactions"].([]any)
	require.True(t, ok, "reactions must be an array")
	require.Len(t, reactions, 1)
	r0, ok := reactions[0].(map[string]any)
	require.True(t, ok, "each reaction must be an object")
	assert.Equal(t, "👍", r0["reaction"], "userId prefix must be stripped")
}

func TestRoomsUpdate_WithDescription(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", Name: "old", OwnerID: "u1"}
	rec := post(h.RoomsUpdate, `{"roomId":"r1","description":"desc"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "desc", repo.Rooms["r1"].Description)
}

type mockJoinedRepo struct{ *testutil.MockChatRepository }

func (m *mockJoinedRepo) ListJoinedRooms(_, _, _ string, _ int) ([]*model.ChatRoom, error) {
	return []*model.ChatRoom{{ID: "r1", Name: "room", OwnerID: "u1"}}, nil
}

func TestRoomsJoined_WithRooms(t *testing.T) {
	base := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&mockJoinedRepo{base}, idGen)
	rec := post(h.RoomsJoined, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

type mockMsgRepo struct{ *testutil.MockChatRepository }

func (m *mockMsgRepo) ListMessagesByRoom(_, _, _ string, _ int) ([]*model.ChatMessage, error) {
	return []*model.ChatMessage{{ID: "m1", FromUserID: "u1", Reads: model.StringArray{}, Reactions: model.StringArray{}}}, nil
}
func (m *mockMsgRepo) SearchMessages(_, _ string, _ int, _, _ string) ([]*model.ChatMessage, error) {
	return []*model.ChatMessage{{ID: "m1", FromUserID: "u1", Reads: model.StringArray{}, Reactions: model.StringArray{}}}, nil
}

func TestMessages_WithData(t *testing.T) {
	base := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&mockMsgRepo{base}, idGen)
	// room 発言の閲覧には member であることが必要 (u1 を owner にする)。
	base.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	rec := post(h.Messages, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

// /chat/messages の room 経路も room-timeline と同じ permission gate を持つ
// (非 member は NO_SUCH_ROOM、room-timeline を塞いでも本経路から漏れない)。
func TestMessages_RoomPermission(t *testing.T) {
	h, repo := newTestHandler()
	// 存在しない room → NO_SUCH_ROOM。
	rec := post(h.Messages, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "c4d9f88c-9270-4632-b032-6ed8cee36f7f")
	// 非 member → NO_SUCH_ROOM。
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u2.ID}
	assert.Equal(t, http.StatusBadRequest, post(h.Messages, `{"roomId":"r1"}`, u1).Code)
	// member なら閲覧可。
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: u1.ID, RoomID: "r1"}))
	assert.Equal(t, http.StatusOK, post(h.Messages, `{"roomId":"r1"}`, u1).Code)
}

func TestMessagesSearch_WithData(t *testing.T) {
	base := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&mockMsgRepo{base}, idGen)
	rec := post(h.MessagesSearch, `{"query":"test"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

func TestMessagesShow_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u1", Reads: model.StringArray{}, Reactions: model.StringArray{}}
	rec := post(h.MessagesShow, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMessagesShow_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesShow, `{"messageId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesShow_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesShow, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesUpdate_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u1"}
	rec := post(h.MessagesUpdate, `{"messageId":"m1","text":"updated"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMessagesUpdate_NotOwner(t *testing.T) {
	h, repo := newTestHandler()
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "other"}
	rec := post(h.MessagesUpdate, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesUpdate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesUpdate, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesDelete_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u1"}
	rec := post(h.MessagesDelete, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMessagesDelete_NotOwner(t *testing.T) {
	h, repo := newTestHandler()
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "other"}
	rec := post(h.MessagesDelete, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesDelete_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesDelete, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesRead(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesRead, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMessagesRead_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesRead, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessages_List(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	rec := post(h.Messages, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Phase 9.8: service wiring tests ---

func newHandlerWithService(t *testing.T) (*Handler, *testutil.MockChatRepository) {
	t.Helper()
	repo := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen)
	svc := corechat.NewService(repo, idGen)
	h.SetService(svc)
	return h, repo
}

func TestMessagesCreate_Service_ToUser(t *testing.T) {
	h, repo := newHandlerWithService(t)
	rec := post(h.MessagesCreate, `{"text":"hi","toUserId":"u2"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Messages, 1)
}

func TestMessagesCreate_Service_ToRoom(t *testing.T) {
	h, repo := newHandlerWithService(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	rec := post(h.MessagesCreate, `{"text":"hi","toRoomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Messages, 1)
}

func TestMessagesCreate_Service_NoTarget(t *testing.T) {
	h, _ := newHandlerWithService(t)
	rec := post(h.MessagesCreate, `{"text":"hi"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #1771: create-to-room は room 不在で NO_SUCH_ROOM (NO_SUCH_MESSAGE ではない)。
func TestMessagesCreate_Service_RoomNotFound(t *testing.T) {
	h, _ := newHandlerWithService(t)
	rec := post(h.MessagesCreate, `{"text":"hi","toRoomId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "8098520d-2da5-4e8f-8ee1-df78b55a4ec6")
}

// #1771: create-to-user は recipient 不在で NO_SUCH_USER を返す (userRepo 配線時)。
func TestMessagesCreate_ToUser_NoSuchUser(t *testing.T) {
	h, _ := newHandlerWithService(t)
	h.SetUserRepo(testutil.NewMockUserRepository()) // recipient を seed しない
	rec := post(h.MessagesCreate, `{"text":"hi","toUserId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_USER", "11795c64-40ea-4198-b06e-3c873ed9039d")
}

type stubChatAvailChecker struct{ avail string }

func (s *stubChatAvailChecker) GetUserPolicies(_ string) map[string]any {
	return map[string]any{"chatAvailability": s.avail}
}

// #1796: recipient の chatAvailability が write 不可 (unavailable) なら ACCESS_DENIED。
func TestMessagesCreate_ToUser_RecipientUnavailable(t *testing.T) {
	h, _ := newHandlerWithService(t)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = u2
	h.SetUserRepo(userRepo)
	h.SetChatAvailabilityChecker(&stubChatAvailChecker{avail: "unavailable"})
	rec := post(h.MessagesCreate, `{"text":"hi","toUserId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "ACCESS_DENIED", "1fb7cb09-d46a-4fff-b8df-057708cce513")
}

// #1796: readonly recipient も write 不可で拒否、available は送信成功。
func TestMessagesCreate_ToUser_RecipientReadonlyVsAvailable(t *testing.T) {
	h, repo := newHandlerWithService(t)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = u2
	h.SetUserRepo(userRepo)
	h.SetChatAvailabilityChecker(&stubChatAvailChecker{avail: "readonly"})
	rec := post(h.MessagesCreate, `{"text":"hi","toUserId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	h.SetChatAvailabilityChecker(&stubChatAvailChecker{avail: "available"})
	rec = post(h.MessagesCreate, `{"text":"hi","toUserId":"u2"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Messages, 1)
}

func TestMessagesCreate_Service_RoomForbidden(t *testing.T) {
	h, repo := newHandlerWithService(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "otherUser"}
	rec := post(h.MessagesCreate, `{"text":"hi","toRoomId":"r1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- #1541: create-message input validation ---

// CONTENT_REQUIRED は text も file も無い場合に発火する (create-to-user)。
func TestMessagesCreate_ContentRequired_ToUser(t *testing.T) {
	h, _ := newHandlerWithService(t)
	rec := post(h.MessagesCreate, `{"toUserId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "CONTENT_REQUIRED", "25587321-b0e6-449c-9239-f8925092942c")
}

// CONTENT_REQUIRED は create-to-room では別 golden id を返す。
func TestMessagesCreate_ContentRequired_ToRoom(t *testing.T) {
	h, repo := newHandlerWithService(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	rec := post(h.MessagesCreate, `{"toRoomId":"r1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "CONTENT_REQUIRED", "340517b7-6d04-42c0-bac1-37ee804e3594")
}

// fileId だけ与えれば CONTENT_REQUIRED は発火しない (file が content を満たす)。
func TestMessagesCreate_FileOnly_NoContentRequired(t *testing.T) {
	h, repo := newHandlerWithService(t)
	fileRepo := testutil.NewMockDriveFileRepository()
	owner := u1.ID
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner}
	h.SetDriveFileRepo(fileRepo)
	rec := post(h.MessagesCreate, `{"toUserId":"u2","fileId":"f1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Messages, 1)
}

// RECIPIENT_IS_YOURSELF は自分宛 DM を弾く。
func TestMessagesCreate_RecipientIsYourself(t *testing.T) {
	h, _ := newHandlerWithService(t)
	rec := post(h.MessagesCreate, `{"text":"hi","toUserId":"u1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "RECIPIENT_IS_YOURSELF", "17e2ba79-e22a-4cbc-bf91-d327643f4a7e")
}

// NO_SUCH_FILE は存在しない fileId を弾く (create-to-user golden)。
func TestMessagesCreate_NoSuchFile_ToUser(t *testing.T) {
	h, _ := newHandlerWithService(t)
	h.SetDriveFileRepo(testutil.NewMockDriveFileRepository())
	rec := post(h.MessagesCreate, `{"text":"hi","toUserId":"u2","fileId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_FILE", "4372b8e2-185d-4146-8749-2f68864a3e5f")
}

// NO_SUCH_FILE は他人所有の file も弾く (create-to-room golden)。
func TestMessagesCreate_NoSuchFile_ToRoom_NotOwner(t *testing.T) {
	h, repo := newHandlerWithService(t)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	fileRepo := testutil.NewMockDriveFileRepository()
	other := "otherUser"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other}
	h.SetDriveFileRepo(fileRepo)
	rec := post(h.MessagesCreate, `{"text":"hi","toRoomId":"r1","fileId":"f1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_FILE", "b6accbd3-1d7b-4d9f-bdb7-eb185bac06db")
}

// text maxLength(2000) 超過は INVALID_PARAM。
func TestMessagesCreate_TextTooLong(t *testing.T) {
	h, _ := newHandlerWithService(t)
	long := strings.Repeat("a", 2001)
	rec := post(h.MessagesCreate, `{"text":"`+long+`","toUserId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "INVALID_PARAM", apierr.UUIDInvalidParam)
}

// text は trim されて保存される (前後の空白除去)。
func TestMessagesCreate_TextTrimmed(t *testing.T) {
	h, repo := newHandlerWithService(t)
	rec := post(h.MessagesCreate, `{"text":"  hi  ","toUserId":"u2"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.Messages, 1)
	for _, m := range repo.Messages {
		require.NotNil(t, m.Text)
		assert.Equal(t, "hi", *m.Text)
	}
}

// RoomsCreate の name / description maxLength 超過は INVALID_PARAM。
func TestRoomsCreate_NameTooLong(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsCreate, `{"name":"`+strings.Repeat("a", 257)+`"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "INVALID_PARAM", apierr.UUIDInvalidParam)
}

func TestRoomsCreate_DescriptionTooLong(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.RoomsCreate, `{"name":"ok","description":"`+strings.Repeat("a", 1025)+`"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "INVALID_PARAM", apierr.UUIDInvalidParam)
}

// RoomsUpdate も同じく maxLength を強制する。
func TestRoomsUpdate_DescriptionTooLong(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	rec := post(h.RoomsUpdate, `{"roomId":"r1","description":"`+strings.Repeat("a", 1025)+`"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "INVALID_PARAM", apierr.UUIDInvalidParam)
}

func TestMessagesDelete_Service(t *testing.T) {
	h, repo := newHandlerWithService(t)
	toID := "u2"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u1", ToUserID: &toID}
	rec := post(h.MessagesDelete, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMessagesDelete_Service_NotFound(t *testing.T) {
	h, _ := newHandlerWithService(t)
	rec := post(h.MessagesDelete, `{"messageId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesDelete_Service_Forbidden(t *testing.T) {
	h, repo := newHandlerWithService(t)
	toID := "u1"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &toID}
	rec := post(h.MessagesDelete, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesUpdate_Service(t *testing.T) {
	h, repo := newHandlerWithService(t)
	toID := "u2"
	orig := "old"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u1", ToUserID: &toID, Text: &orig}
	rec := post(h.MessagesUpdate, `{"messageId":"m1","text":"new"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMessagesUpdate_Service_NotFound(t *testing.T) {
	h, _ := newHandlerWithService(t)
	rec := post(h.MessagesUpdate, `{"messageId":"ghost","text":"x"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesUpdate_Service_Forbidden(t *testing.T) {
	h, repo := newHandlerWithService(t)
	toID := "u1"
	orig := "old"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &toID, Text: &orig}
	rec := post(h.MessagesUpdate, `{"messageId":"m1","text":"new"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesRead_Service(t *testing.T) {
	h, repo := newHandlerWithService(t)
	toID := "u1"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &toID}
	rec := post(h.MessagesRead, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMessagesRead_Service_NotFound(t *testing.T) {
	h, _ := newHandlerWithService(t)
	rec := post(h.MessagesRead, `{"messageId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type mockUserMsgRepo struct{ *testutil.MockChatRepository }

func (m *mockUserMsgRepo) ListMessagesByUser(_, _, _, _ string, _ int) ([]*model.ChatMessage, error) {
	return []*model.ChatMessage{{ID: "m2", FromUserID: "u2", Reads: model.StringArray{}, Reactions: model.StringArray{}}}, nil
}

func TestMessages_ListByUser(t *testing.T) {
	base := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&mockUserMsgRepo{base}, idGen)
	rec := post(h.Messages, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

func TestMessages_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Messages, `invalid`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMessagesSearch(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesSearch, `{"query":"hello"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1541: roomId 指定で存在しない room は NO_SUCH_ROOM。
func TestMessagesSearch_RoomNotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MessagesSearch, `{"query":"x","roomId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "460b3669-81b0-4dc9-a997-44442141bf83")
}

// #1541: roomId 指定で非 member の room も NO_SUCH_ROOM。
func TestMessagesSearch_RoomNotMember(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u2.ID}
	rec := post(h.MessagesSearch, `{"query":"x","roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "460b3669-81b0-4dc9-a997-44442141bf83")
}

// #1541: member であれば roomId scope で検索でき、その room のメッセージを返す。
func TestMessagesSearch_RoomScoped(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	roomID := "r1"
	txt := "hello room"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u2", ToRoomID: &roomID, Text: &txt, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	rec := post(h.MessagesSearch, `{"query":"hello","roomId":"r1"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

// #1541: userId scope は相手との 1-on-1 のみを返す。
func TestMessagesSearch_UserScoped(t *testing.T) {
	h, repo := newTestHandler()
	to := "u2"
	txt := "hi bob"
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: u1.ID, ToUserID: &to, Text: &txt, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	// 別人との DM はヒットしない。
	other := "u3"
	txt2 := "hi carol"
	repo.Messages["m2"] = &model.ChatMessage{ID: "m2", FromUserID: u1.ID, ToUserID: &other, Text: &txt2, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	rec := post(h.MessagesSearch, `{"query":"hi","userId":"u2"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

func TestReactionsCreate(t *testing.T) {
	h, repo := newHandlerWithService(t)
	// missing params → 400
	assert.Equal(t, http.StatusBadRequest, post(h.ReactionsCreate, `{}`, u1).Code)
	// #1549/#1541: React は service 経由で message を引くので seed が要る。
	// #1541 で react guard が入ったため、reactor (u1) が受信者である DM にする
	// (own-message でも others-message でもない有効な react)。
	to := u1.ID
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "sender", ToUserID: &to, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	rec := post(h.ReactionsCreate, `{"messageId":"m1","reaction":"👍"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestReactionsDelete(t *testing.T) {
	h, repo := newHandlerWithService(t)
	// missing params → 400
	assert.Equal(t, http.StatusBadRequest, post(h.ReactionsDelete, `{}`, u1).Code)
	// unreact も受信者 (u1) でなければアクセスエラーになるので、u1 宛ての DM にする。
	to := u1.ID
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "sender", ToUserID: &to, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	rec := post(h.ReactionsDelete, `{"messageId":"m1","reaction":"👍"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// recordingPublisher records chat stream publishes.
type recordingPublisher struct {
	mu    sync.Mutex
	calls int
}

func (p *recordingPublisher) PublishUserMessage(context.Context, string, string, string, any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
}

func (p *recordingPublisher) PublishRoomMessage(context.Context, string, string, any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
}

// **存在しない id と、触れられないメッセージを同じ応答にする。** 応答が
// 分かれると、メッセージの存在をエラーから判別できる。react / unreact
// それぞれ upstream の endpoint 固有 id で 400 NO_SUCH_MESSAGE を返し、
// 本文がバイト単位で一致すること、何も消さず何も流さないことを見る。
func TestReactions_AccessErrorIsIndistinguishable(t *testing.T) {
	const (
		reactID   = "9b5839b9-0ba0-4351-8c35-37082093d200"
		unreactID = "c39ea42f-e3ca-428a-ad57-390e0a711595"
	)
	newH := func(t *testing.T) (*Handler, *testutil.MockChatRepository, *recordingPublisher) {
		t.Helper()
		repo := testutil.NewMockChatRepository()
		idGen, _ := id.NewGenerator("aidx")
		h := NewHandler(repo, idGen)
		svc := corechat.NewService(repo, idGen)
		pub := &recordingPublisher{}
		svc.SetStreamingPublisher(pub)
		h.SetService(svc)
		other := "u3"
		room := "r1"
		repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u3"}
		// u1 は参加していない DM と部屋。u1 の古いリアクションが残っている
		// (部屋を抜けた元メンバーの形)。
		repo.Messages["dm"] = &model.ChatMessage{ID: "dm", FromUserID: u2.ID, ToUserID: &other, Reads: model.StringArray{}, Reactions: model.StringArray{"u1/👍"}}
		repo.Messages["room"] = &model.ChatMessage{ID: "room", FromUserID: u2.ID, ToRoomID: &room, Reads: model.StringArray{}, Reactions: model.StringArray{"u1/👍"}}
		repo.Messages["own"] = &model.ChatMessage{ID: "own", FromUserID: u1.ID, ToUserID: &other, Reads: model.StringArray{}, Reactions: model.StringArray{}}
		return h, repo, pub
	}

	// 自分のメッセージは react だけがアクセスエラー。unreact は本家どおり、
	// DM の送った側も自分のリアクションを外せる。
	for _, ep := range []struct {
		name    string
		handler func(*Handler) func(echo.Context) error
		id      string
		denied  []string
	}{
		{"react", func(h *Handler) func(echo.Context) error { return h.ReactionsCreate }, reactID, []string{"dm", "room", "own"}},
		{"unreact", func(h *Handler) func(echo.Context) error { return h.ReactionsDelete }, unreactID, []string{"dm", "room"}},
	} {
		t.Run(ep.name, func(t *testing.T) {
			h, repo, pub := newH(t)
			ghost := post(ep.handler(h), `{"messageId":"ghost","reaction":"👍"}`, u1)
			require.Equal(t, http.StatusBadRequest, ghost.Code)
			assertErrorCode(t, ghost, "NO_SUCH_MESSAGE", ep.id)

			for _, mid := range ep.denied {
				rec := post(ep.handler(h), `{"messageId":"`+mid+`","reaction":"👍"}`, u1)
				assert.Equal(t, http.StatusBadRequest, rec.Code, mid)
				assert.Equal(t, ghost.Body.String(), rec.Body.String(), "%s: 存在しない id と応答が違う", mid)
			}
			assert.Equal(t, model.StringArray{"u1/👍"}, repo.Messages["dm"].Reactions, "参加していない DM のリアクションが変わった")
			assert.Equal(t, model.StringArray{"u1/👍"}, repo.Messages["room"].Reactions, "抜けた部屋のリアクションが変わった")
			assert.Zero(t, pub.calls, "参加していないのにイベントを流している")
		})
	}
}

// **上限の検査はアクセス検査より後。** 参加していない利用者には、上限に
// 達したメッセージでも 500 (too many) ではなく NO_SUCH_MESSAGE を返す。
func TestReactionsCreate_LimitCheckedAfterAccess(t *testing.T) {
	h, repo := newHandlerWithService(t)
	room := "r1"
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u3"}
	full := make(model.StringArray, 100)
	for i := range full {
		full[i] = "u/x"
	}
	repo.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: u2.ID, ToRoomID: &room, Reads: model.StringArray{}, Reactions: full}
	rec := post(h.ReactionsCreate, `{"messageId":"m1","reaction":"👍"}`, u1)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_MESSAGE", "9b5839b9-0ba0-4351-8c35-37082093d200")
}

// --- Invitations ---

func TestInvitationsCreate_Success(t *testing.T) {
	h, repo := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = u2
	h.SetUserRepo(userRepo)
	// owner を populate して nested room.owner (required UserLite) も検証する。
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "General", OwnerID: u1.ID, Owner: u1}))
	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	// upstream res は packed ChatRoomInvitation {id, createdAt, userId, user, roomId, room}。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["id"])
	assert.NotEmpty(t, resp["createdAt"])
	assert.Equal(t, "u2", resp["userId"])
	assert.Equal(t, "r1", resp["roomId"])
	user, ok := resp["user"].(map[string]any)
	require.True(t, ok, "user (UserLite) must be resolved")
	assert.Equal(t, "u2", user["id"])
	room, ok := resp["room"].(map[string]any)
	require.True(t, ok, "room (ChatRoom) must be present")
	assert.Equal(t, "r1", room["id"])
	owner, ok := room["owner"].(map[string]any)
	require.True(t, ok, "room.owner (UserLite) must be present")
	assert.Equal(t, "u1", owner["id"])
	// invitation 行が永続化されている。
	assert.Len(t, repo.Invitations, 1)
}

// userRepo 配線時に invitee user が存在しない (FindByID err) なら、upstream
// packRoomInvitation の throw と同じく 500 を返し、invitation 行も作らない。
// recordingInvitationNotifier captures OnChatRoomInvitationReceived calls (#1559)。
type recordingInvitationNotifier struct{ calls [][3]string }

func (n *recordingInvitationNotifier) OnChatRoomInvitationReceived(invitee, inviter, invID string) {
	n.calls = append(n.calls, [3]string{invitee, inviter, invID})
}

// #1559 [MEDIUM] invitations/create で被招待ユーザーへ通知が発火し、notifier は
// 招待者 (room owner)。
func TestInvitationsCreate_FiresNotifier(t *testing.T) {
	h, repo := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = u2
	h.SetUserRepo(userRepo)
	notifier := &recordingInvitationNotifier{}
	h.SetInvitationNotifier(notifier)
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "General", OwnerID: u1.ID, Owner: u1}))

	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, "u2", notifier.calls[0][0], "invitee")
	assert.Equal(t, "u1", notifier.calls[0][1], "inviter = room owner")
	assert.NotEmpty(t, notifier.calls[0][2], "invitation ID")
}

// #1559 PackInvitationByID: 招待を packed ChatRoomInvitation に解決する。
func TestPackInvitationByID(t *testing.T) {
	h, repo := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = u2
	h.SetUserRepo(userRepo)
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "General", OwnerID: u1.ID, Owner: u1}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "inv1", UserID: "u2", RoomID: "r1"}))

	packed, ok := h.PackInvitationByID("inv1", "u2")
	require.True(t, ok)
	assert.Equal(t, "inv1", packed["id"])
	assert.Equal(t, "r1", packed["roomId"])
	room, ok := packed["room"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "r1", room["id"])

	// 削除済 (存在しない) invitation は (nil,false)。
	_, ok = h.PackInvitationByID("gone", "u2")
	assert.False(t, ok)
}

// room load 失敗時は通知を drop する (= 不完全な invitation を embed しない)。
func TestPackInvitationByID_NoRoom(t *testing.T) {
	h, repo := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = u2
	h.SetUserRepo(userRepo)
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "inv1", UserID: "u2", RoomID: "missing"}))
	_, ok := h.PackInvitationByID("inv1", "u2")
	assert.False(t, ok, "room が無ければ drop")
}

// 被招待者 user が解決できなければ通知を drop する (upstream の throw→null 相当)。
func TestPackInvitationByID_UnresolvableUser(t *testing.T) {
	h, repo := newTestHandler()
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "R", OwnerID: u1.ID}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "inv1", UserID: "ghost", RoomID: "r1"}))

	// userRepo 未配線 → drop
	_, ok := h.PackInvitationByID("inv1", "ghost")
	assert.False(t, ok, "userRepo 未配線なら drop")

	// userRepo 配線済だが invitee user が存在しない → drop
	h.SetUserRepo(testutil.NewMockUserRepository())
	_, ok = h.PackInvitationByID("inv1", "ghost")
	assert.False(t, ok, "invitee user 解決不能なら drop")
}

func TestInvitationsCreate_InviteeNotFound(t *testing.T) {
	h, repo := newTestHandler()
	h.SetUserRepo(testutil.NewMockUserRepository()) // invitee 未登録
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", OwnerID: u1.ID}))
	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"ghost"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, repo.Invitations, "解決不能な invitee には invitation 行を作らない")
}

// userRepo 未配線 (degraded path) では invitation を作成しつつ user field を省略する。
func TestInvitationsCreate_NoUserRepoOmitsUser(t *testing.T) {
	h, repo := newTestHandler()
	h.SetUserRepo(nil) // 未配線の構成を作る
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", OwnerID: u1.ID}))
	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, hasUser := resp["user"]
	assert.False(t, hasUser, "userRepo 未配線では user field を省略する")
	assert.Len(t, repo.Invitations, 1)
}

// upstream: inviter==invitee は 'yourself' で拒否 (generic 500)。
func TestInvitationsCreate_Yourself(t *testing.T) {
	h, repo := newTestHandler()
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", OwnerID: u1.ID}))
	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u1"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, repo.Invitations)
}

// upstream: 既に member / 既に招待済みは拒否 (generic 500)。
func TestInvitationsCreate_AlreadyMemberOrInvited(t *testing.T) {
	h, repo := newTestHandler()
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", OwnerID: u1.ID}))
	// already member
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: "u2", RoomID: "r1"}))
	assert.Equal(t, http.StatusInternalServerError, post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1).Code)
	// already invited
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: "u3", RoomID: "r1"}))
	assert.Equal(t, http.StatusInternalServerError, post(h.InvitationsCreate, `{"roomId":"r1","userId":"u3"}`, u1).Code)
}

// upstream: membership 数が MAX_ROOM_MEMBERS(50) 以上なら 'room is full'。
func TestInvitationsCreate_RoomFull(t *testing.T) {
	h, repo := newTestHandler()
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", OwnerID: u1.ID}))
	for i := 0; i < maxRoomMembers; i++ {
		uid := "filler" + string(rune('a'+i))
		require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: uid, UserID: uid, RoomID: "r1"}))
	}
	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, repo.Invitations)
}

func TestInvitationsCreate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.InvitationsCreate, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInvitationsCreate_NotOwnerRejected(t *testing.T) {
	h, repo := newTestHandler()
	// room owner は u2。u1 は owner でないので招待を作成・連合できない。
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "General", OwnerID: "u2"}))
	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u3"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInvitationsCreate_Error(t *testing.T) {
	h, repo := newTestHandler()
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "General", OwnerID: u1.ID}))
	repo.CreateErr = errMock
	rec := post(h.InvitationsCreate, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestInvitationsDelete(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	repo.Invitations["i1"] = &model.ChatRoomInvitation{ID: "i1", UserID: "u2", RoomID: "r1"}
	rec := post(h.InvitationsDelete, `{"invitationId":"i1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	_, err := repo.FindInvitationByID("i1")
	assert.Error(t, err, "招待は消える")
}

// **owner 以外は消せない。** 以前は invitationId だけで誰でも消せた。
func TestInvitationsDelete_NotOwner(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "someoneElse"}
	repo.Invitations["i1"] = &model.ChatRoomInvitation{ID: "i1", UserID: "u2", RoomID: "r1"}
	rec := post(h.InvitationsDelete, `{"invitationId":"i1"}`, u1)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_INVITATION", "9db2b511-acf0-46f0-9856-ce089fa63b23")
	_, err := repo.FindInvitationByID("i1")
	assert.NoError(t, err, "招待は残る")
}

// 招待された本人も消せない (reject / ignore を使う。delete は連合 Reject を
// 送らないので、開放すると remote room の招待を黙って消せる)。
func TestInvitationsDelete_InviteeCannotDelete(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "someoneElse"}
	repo.Invitations["i1"] = &model.ChatRoomInvitation{ID: "i1", UserID: "u1", RoomID: "r1"}
	rec := post(h.InvitationsDelete, `{"invitationId":"i1"}`, u1)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// **存在の有無を応答で区別しない** (invitationId の総当たりを許さない)。
func TestInvitationsDelete_UnknownIsSameAsForbidden(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "someoneElse"}
	repo.Invitations["i1"] = &model.ChatRoomInvitation{ID: "i1", UserID: "u2", RoomID: "r1"}

	forbidden := post(h.InvitationsDelete, `{"invitationId":"i1"}`, u1)
	missing := post(h.InvitationsDelete, `{"invitationId":"nope"}`, u1)
	assert.Equal(t, forbidden.Code, missing.Code)
	assert.JSONEq(t, forbidden.Body.String(), missing.Body.String())
}

// **DB 障害を not-found に丸めない** (#2792)。招待の lookup と room の lookup の
// どちらが落ちても 500 にする。丸めると、DB 障害の間だけ「消せない」ではなく
// 「そんな招待は無い」と応答が変わり、原因から遠い症状になる。
func TestInvitationsDelete_LookupFailureIs500(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*testutil.MockChatRepository, error)
	}{
		{"invitation lookup", func(r *testutil.MockChatRepository, e error) { r.FindInvitationErr = e }},
		{"room lookup", func(r *testutil.MockChatRepository, e error) { r.FindRoomErr = e }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newTestHandler()
			repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
			repo.Invitations["i1"] = &model.ChatRoomInvitation{ID: "i1", UserID: "u2", RoomID: "r1"}
			tc.set(repo, errors.New("db down"))

			rec := post(h.InvitationsDelete, `{"invitationId":"i1"}`, u1)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)

			tc.set(repo, nil)
			_, err := repo.FindInvitationByID("i1")
			assert.NoError(t, err, "失敗時に消さない")
		})
	}
}

func TestInvitationsDelete_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.InvitationsDelete, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInvitationsAccept(t *testing.T) {
	h, repo := newTestHandler()
	// **room を実在させること。** 無いと FindRoomByID が not-found を返して
	// owner ガード (#2858) が一度も評価されず、「全員を owner とみなす」形に
	// 壊れても緑のまま通る (= 招待を受けても誰も room に入れない回帰が無検出)。
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "someoneElse"}
	repo.Invitations["i1"] = &model.ChatRoomInvitation{ID: "i1", UserID: "u1", RoomID: "r1"}
	rec := post(h.InvitationsAccept, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, repo.Memberships, 1)
}

func TestInvitationsAccept_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.InvitationsAccept, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInvitationsAccept_NoInvitationRejected(t *testing.T) {
	h, repo := newTestHandler()
	// 招待が無ければ membership を作らず NO_SUCH_INVITATION。
	rec := post(h.InvitationsAccept, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, repo.Memberships)
}

func TestInvitationsReject(t *testing.T) {
	h, repo := newTestHandler()
	repo.Invitations["i1"] = &model.ChatRoomInvitation{ID: "i1", UserID: "u1", RoomID: "r1"}
	rec := post(h.InvitationsReject, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.Invitations)
}

func TestInvitationsReject_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.InvitationsReject, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInvitationsReject_NoInvitationRejected(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.InvitationsReject, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMembersBan(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	seedMember(t, repo, "u2", "r1")
	rec := post(h.MembersBan, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	_, err := repo.FindMembership("u2", "r1")
	assert.Error(t, err, "メンバーは退出させられる")
}

// **owner 以外は他人の room からメンバーを追い出せない。**
// 以前は認証さえ通れば任意の room に対して実行できた。
func TestMembersBan_NotOwner(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "someoneElse"}
	seedMember(t, repo, "u2", "r1")
	rec := post(h.MembersBan, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "1ebd1536-1c92-4e6b-9ee0-bf4d14998bdb")
	_, err := repo.FindMembership("u2", "r1")
	assert.NoError(t, err, "メンバーは残る")
}

// room の所在と権限の有無を応答で区別しない。
func TestMembersBan_UnknownRoomIsSameAsForbidden(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "someoneElse"}
	forbidden := post(h.MembersBan, `{"roomId":"r1","userId":"u2"}`, u1)
	missing := post(h.MembersBan, `{"roomId":"nope","userId":"u2"}`, u1)
	assert.Equal(t, forbidden.Code, missing.Code)
	assert.JSONEq(t, forbidden.Body.String(), missing.Body.String())
}

// **DB 障害を not-found に丸めない** (#2792)。
func TestMembersBan_LookupFailureIs500(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	seedMember(t, repo, "u2", "r1")
	repo.FindRoomErr = errors.New("db down")
	rec := post(h.MembersBan, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	repo.FindRoomErr = nil
	_, err := repo.FindMembership("u2", "r1")
	assert.NoError(t, err, "失敗時に消さない")
}

func TestMembersBan_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.MembersBan, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMembersUpdateMembership(t *testing.T) {
	h, _ := newTestHandler()
	// missing params → 400
	assert.Equal(t, http.StatusBadRequest, post(h.MembersUpdateMembership, `{}`, u1).Code)
}

func TestHistory(t *testing.T) {
	h, _ := newTestHandler()
	// default (room=false) → ListUserHistory に dispatch
	rec := post(h.History, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	rec2 := post(h.History, `{"limit":5}`, u1)
	assert.Equal(t, http.StatusOK, rec2.Code)
	// room=true → ListRoomHistory に dispatch (#692)
	rec3 := post(h.History, `{"room":true,"limit":3}`, u1)
	assert.Equal(t, http.StatusOK, rec3.Code)
	// limit clamp: 0 → 10, 9999 → 100 (内部分岐の coverage 確保)
	assert.Equal(t, http.StatusOK, post(h.History, `{"limit":0}`, u1).Code)
	assert.Equal(t, http.StatusOK, post(h.History, `{"limit":9999}`, u1).Code)
}

// historyRepo overrides ListUserHistory / ListRoomHistory to return seeded
// conversations; HasUnread* fall through to the embedded mock (m.Messages).
type historyRepo struct {
	*testutil.MockChatRepository
	userHistory []*model.ChatMessage
	roomHistory []*model.ChatMessage
}

func (r *historyRepo) ListUserHistory(string, int) ([]*model.ChatMessage, error) {
	return r.userHistory, nil
}
func (r *historyRepo) ListRoomHistory(string, int) ([]*model.ChatMessage, error) {
	return r.roomHistory, nil
}

// #1749: history は会話ごとに isRead を載せる (1-on-1)。
func TestHistory_IsRead_DM(t *testing.T) {
	base := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	to := u1.ID
	base.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &to, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	hr := &historyRepo{MockChatRepository: base, userHistory: []*model.ChatMessage{base.Messages["m1"]}}
	h := NewHandler(hr, idGen)

	// 未読: u1 が cm を読んでいない → isRead=false
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(post(h.History, `{}`, u1).Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, false, resp[0]["isRead"])

	// 既読化すると isRead=true
	base.Messages["m1"].Reads = model.StringArray{u1.ID}
	var resp2 []map[string]any
	require.NoError(t, json.Unmarshal(post(h.History, `{}`, u1).Body.Bytes(), &resp2))
	require.Len(t, resp2, 1)
	assert.Equal(t, true, resp2[0]["isRead"])
}

// #1749: room history も isRead を載せる。
func TestHistory_IsRead_Room(t *testing.T) {
	base := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	roomID := "r1"
	base.Messages["m1"] = &model.ChatMessage{ID: "m1", FromUserID: "u2", ToRoomID: &roomID, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	hr := &historyRepo{MockChatRepository: base, roomHistory: []*model.ChatMessage{base.Messages["m1"]}}
	h := NewHandler(hr, idGen)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(post(h.History, `{"room":true}`, u1).Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, false, resp[0]["isRead"], "未読 room message は isRead=false")
}

// --- #1747: list endpoint pagination (limit + id cursor) ---

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestRoomsOwned_Pagination(t *testing.T) {
	h, repo := newTestHandler()
	for _, id := range []string{"a1", "a2", "a3"} {
		repo.Rooms[id] = &model.ChatRoom{ID: id, OwnerID: u1.ID, Name: id, Owner: u1}
	}
	// limit=2 → newest 2 (id 降順 a3, a2)
	resp := decodeList(t, post(h.RoomsOwned, `{"limit":2}`, u1))
	require.Len(t, resp, 2)
	assert.Equal(t, "a3", resp[0]["id"])
	assert.Equal(t, "a2", resp[1]["id"])
	// untilId=a2 → id < a2 (= a1 のみ)
	resp2 := decodeList(t, post(h.RoomsOwned, `{"untilId":"a2"}`, u1))
	require.Len(t, resp2, 1)
	assert.Equal(t, "a1", resp2[0]["id"])
	// sinceId=a2 → id > a2 (= a3 のみ)
	resp3 := decodeList(t, post(h.RoomsOwned, `{"sinceId":"a2"}`, u1))
	require.Len(t, resp3, 1)
	assert.Equal(t, "a3", resp3[0]["id"])
}

func TestRoomsMembers_Pagination(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	for _, mid := range []string{"b1", "b2", "b3"} {
		repo.Memberships[mid+":r1"] = &model.ChatRoomMembership{ID: mid, UserID: mid, RoomID: "r1"}
	}
	resp := decodeList(t, post(h.RoomsMembers, `{"roomId":"r1","limit":2}`, u1))
	require.Len(t, resp, 2)
	assert.Equal(t, "b3", resp[0]["id"])
}

func TestUserTimeline_Pagination(t *testing.T) {
	h, repo := newTestHandler()
	to := "u2"
	for _, mid := range []string{"c1", "c2", "c3"} {
		repo.Messages[mid] = &model.ChatMessage{ID: mid, FromUserID: u1.ID, ToUserID: &to, Reads: model.StringArray{}, Reactions: model.StringArray{}}
	}
	// untilId=c3 → c2, c1 (id < c3)
	resp := decodeList(t, post(h.UserTimeline, `{"userId":"u2","untilId":"c3"}`, u1))
	require.Len(t, resp, 2)
	assert.Equal(t, "c2", resp[0]["id"])
	assert.Equal(t, "c1", resp[1]["id"])
}

// #692 review #7: UserTimeline / RoomTimeline がそれぞれ
// MarkAllReadFromUser / MarkAllReadInRoom を呼んでチャット画面を開いた瞬間に
// 既読化することを assert する (handler 側の wiring を guard)。
type readMarkSpyRepo struct {
	*testutil.MockChatRepository
	markFromUserCalled struct{ reader, fromUser string }
	markInRoomCalled   struct{ reader, room string }
}

func (m *readMarkSpyRepo) MarkAllReadFromUser(reader, fromUser string) error {
	m.markFromUserCalled.reader = reader
	m.markFromUserCalled.fromUser = fromUser
	return nil
}

func (m *readMarkSpyRepo) MarkAllReadInRoom(reader, room string) error {
	m.markInRoomCalled.reader = reader
	m.markInRoomCalled.room = room
	return nil
}

func TestUserTimeline_MarksReadOnOpen(t *testing.T) {
	spy := &readMarkSpyRepo{MockChatRepository: testutil.NewMockChatRepository()}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(spy, idGen)
	rec := post(h.UserTimeline, `{"userId":"u_other"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "u1", spy.markFromUserCalled.reader)
	assert.Equal(t, "u_other", spy.markFromUserCalled.fromUser)
}

// #1541: userRepo 配線時、存在しない userId は NO_SUCH_USER を返す。
func TestUserTimeline_NoSuchUser(t *testing.T) {
	h, _ := newTestHandler()
	h.SetUserRepo(testutil.NewMockUserRepository()) // 空 (u_ghost 未登録)
	rec := post(h.UserTimeline, `{"userId":"u_ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_USER", "11795c64-40ea-4198-b06e-3c873ed9039d")
}

// #1541: userId が存在すれば従来どおり 200。
func TestUserTimeline_UserExists(t *testing.T) {
	h, _ := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u2"] = u2
	h.SetUserRepo(userRepo)
	rec := post(h.UserTimeline, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRoomTimeline_MarksReadOnOpen(t *testing.T) {
	spy := &readMarkSpyRepo{MockChatRepository: testutil.NewMockChatRepository()}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(spy, idGen)
	// u1 を owner にして閲覧権限を満たす。
	spy.Rooms["r_x"] = &model.ChatRoom{ID: "r_x", OwnerID: u1.ID}
	rec := post(h.RoomTimeline, `{"roomId":"r_x"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "u1", spy.markInRoomCalled.reader)
	assert.Equal(t, "r_x", spy.markInRoomCalled.room)
}

// MarkAllRead* が err を返しても timeline 自体は 200 を返す (best-effort)。
type readMarkErrRepo struct {
	*testutil.MockChatRepository
}

func (m *readMarkErrRepo) MarkAllReadFromUser(_, _ string) error { return errMock }
func (m *readMarkErrRepo) MarkAllReadInRoom(_, _ string) error   { return errMock }

func TestTimeline_BestEffortReadMark(t *testing.T) {
	r := &readMarkErrRepo{MockChatRepository: testutil.NewMockChatRepository()}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(r, idGen)
	r.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	assert.Equal(t, http.StatusOK, post(h.UserTimeline, `{"userId":"u2"}`, u1).Code)
	assert.Equal(t, http.StatusOK, post(h.RoomTimeline, `{"roomId":"r1"}`, u1).Code)
}

func TestUnreadCount(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.UnreadCount, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, float64(0), resp["count"])
}

// --- TS-compatible aliases + new handlers ---

func TestMessagesCreateToUser(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	rec := post(h.MessagesCreateToUser, `{"text":"hi","toUserId":"u2"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// recipient が sender を block している場合は YOU_HAVE_BEEN_BLOCKED (403)。
func TestMessagesCreateToUser_Blocked(t *testing.T) {
	repo := testutil.NewMockChatRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen)
	svc := corechat.NewService(repo, idGen)
	blocks := testutil.NewMockBlockingRepository()
	// blocker=u2 (recipient), blockee=u1 (sender)。
	require.NoError(t, blocks.Create(&model.Blocking{ID: "b1", BlockerID: "u2", BlockeeID: "u1"}))
	svc.SetBlockingRepo(blocks)
	h.SetService(svc)

	rec := post(h.MessagesCreateToUser, `{"text":"hi","toUserId":"u2"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, _ := resp["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "YOU_HAVE_BEEN_BLOCKED", errObj["code"])
}

func TestMessagesCreateToRoom(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	rec := post(h.MessagesCreateToRoom, `{"text":"hi","toRoomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUserTimeline(t *testing.T) {
	h, _ := newTestHandler()
	assert.Equal(t, http.StatusBadRequest, post(h.UserTimeline, `{}`, u1).Code)
	rec := post(h.UserTimeline, `{"userId":"u2"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	rec2 := post(h.UserTimeline, `{"userId":"u2","limit":5}`, u1)
	assert.Equal(t, http.StatusOK, rec2.Code)
}

func TestRoomTimeline(t *testing.T) {
	h, repo := newTestHandler()
	assert.Equal(t, http.StatusBadRequest, post(h.RoomTimeline, `{}`, u1).Code)
	// 存在しない room は NO_SUCH_ROOM (endpoint 固有 id)。
	rec0 := post(h.RoomTimeline, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusBadRequest, rec0.Code)
	assertErrorCode(t, rec0, "NO_SUCH_ROOM", "c4d9f88c-9270-4632-b032-6ed8cee36f7f")
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u2.ID}
	// member でも moderator でもない第三者は NO_SUCH_ROOM。
	rec1 := post(h.RoomTimeline, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusBadRequest, rec1.Code)
	assertErrorCode(t, rec1, "NO_SUCH_ROOM", "c4d9f88c-9270-4632-b032-6ed8cee36f7f")
	// member なら閲覧可。
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: u1.ID, RoomID: "r1"}))
	rec := post(h.RoomTimeline, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	rec2 := post(h.RoomTimeline, `{"roomId":"r1","limit":5}`, u1)
	assert.Equal(t, http.StatusOK, rec2.Code)
}

// room-timeline は moderator にも閲覧を許可する (member でなくても)。
func TestRoomTimeline_ModeratorAllowed(t *testing.T) {
	h, repo := newTestHandler()
	h.SetModeratorChecker(fakeModeratorChecker{mods: map[string]bool{u1.ID: true}})
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u2.ID}
	rec := post(h.RoomTimeline, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestReadAll(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.ReadAll, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// #692: mapChatErr が ErrChatScopeViolation / ErrChatScopeUnconfigured を
// それぞれ 403 / 500 にマップする。新規 error path の wiring guard。
func TestMapChatErr_ChatScopeBranches(t *testing.T) {
	h, _ := newTestHandler()
	e := echo.New()
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"violation", corechat.ErrChatScopeViolation, http.StatusForbidden},
		{"unconfigured", corechat.ErrChatScopeUnconfigured, http.StatusInternalServerError},
		{"unknown", errors.New("?"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)
			_ = h.mapChatErr(c, tc.err)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

// #692: packMessageDetailed は createdAt + toUser + toRoom を含めて返す。
func TestPackMessageDetailed_FieldsPresent(t *testing.T) {
	h, _ := newTestHandler()
	idGen, _ := id.NewGenerator("aidx")
	now := idGen.Generate(time.Now())
	host := "remote.example"
	roomID := "r1"
	toUserID := "u_to"
	msg := &model.ChatMessage{
		ID:         now,
		FromUserID: "u_from",
		ToUserID:   &toUserID,
		ToRoomID:   &roomID,
		FromUser:   &model.User{ID: "u_from", Username: "from", Host: &host},
		ToUser:     &model.User{ID: "u_to", Username: "to"},
		ToRoom:     &model.ChatRoom{ID: roomID, Name: "Room"},
		Reads:      model.StringArray{},
		Reactions:  model.StringArray{},
	}
	out := h.packMessageDetailed(msg, "u_from")
	assert.NotEmpty(t, out["createdAt"], "createdAt が ID から派生して埋まること")
	assert.NotNil(t, out["toUser"])
	assert.NotNil(t, out["toRoom"])
	// upstream FE 互換のため `room` alias も入る
	assert.NotNil(t, out["room"])
	// fromUser に avatarUrl の identicon fallback が入る (assertion 失敗時は
	// require.IsType がテスト終了させるので panic せずに見やすいエラーになる)。
	require.IsType(t, map[string]any{}, out["fromUser"])
	from := out["fromUser"].(map[string]any)
	assert.Equal(t, "/identicon/from@remote.example", from["avatarUrl"])
}

func TestInvitationsIgnore(t *testing.T) {
	h, repo := newTestHandler()
	assert.Equal(t, http.StatusBadRequest, post(h.InvitationsIgnore, `{}`, u1).Code)
	repo.Invitations["inv1"] = &model.ChatRoomInvitation{ID: "inv1", UserID: u1.ID, RoomID: "r1"}
	rec := post(h.InvitationsIgnore, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestInvitationsInbox(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.InvitationsInbox, `{}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestInvitationsOutbox(t *testing.T) {
	h, repo := newTestHandler()
	assert.Equal(t, http.StatusBadRequest, post(h.InvitationsOutbox, `{}`, u1).Code)
	// no room → 404
	assert.Equal(t, http.StatusBadRequest, post(h.InvitationsOutbox, `{"roomId":"r1"}`, u1).Code)
	// owner can see outbox
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	rec := post(h.InvitationsOutbox, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	// non-owner → 404
	assert.Equal(t, http.StatusBadRequest, post(h.InvitationsOutbox, `{"roomId":"r1"}`, u2).Code)
}

// #1541: outbox の各 invitation は upstream packRoomInvitation 同様
// createdAt と room を含む。
func TestInvitationsOutbox_ShapeIncludesCreatedAtAndRoom(t *testing.T) {
	h, repo := newTestHandler()
	idGen, _ := id.NewGenerator("aidx")
	invID := idGen.Generate(time.Now())
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID, Name: "room"}
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: invID, UserID: u2.ID, RoomID: "r1"}))
	rec := post(h.InvitationsOutbox, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	entry := resp[0]
	assert.Equal(t, "r1", entry["roomId"])
	assert.Equal(t, u2.ID, entry["userId"])
	assert.NotEmpty(t, entry["createdAt"], "createdAt は invitation id から導出される")
	room, ok := entry["room"].(map[string]any)
	require.True(t, ok, "room (ChatRoom) を含む")
	assert.Equal(t, "r1", room["id"])
	assert.Equal(t, "room", room["name"])
}

// #1541: outbox の NO_SUCH_ROOM は upstream の golden id を返す。
func TestInvitationsOutbox_NoSuchRoomGoldenID(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.InvitationsOutbox, `{"roomId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_ROOM", "a3c6b309-9717-4316-ae94-a69b53437237")
}

func TestRoomsJoin(t *testing.T) {
	h, repo := newTestHandler()
	// invalid param
	assert.Equal(t, http.StatusBadRequest, post(h.RoomsJoin, `{}`, u1).Code)
	// upstream: pending invitation が無いと join できない (generic 500)。
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u2.ID}
	assert.Equal(t, http.StatusInternalServerError, post(h.RoomsJoin, `{"roomId":"r1"}`, u1).Code)
	assert.Empty(t, repo.Memberships, "invitation 無しでは membership を作らない")
	// invitation があれば join 成功し、membership 作成 + invitation 消費。
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: u1.ID, RoomID: "r1"}))
	rec := post(h.RoomsJoin, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, mErr := repo.FindMembership(u1.ID, "r1")
	assert.NoError(t, mErr, "membership が作成される")
	assert.Empty(t, repo.Invitations, "join 成功で invitation は消費される")
}

// 既存 member が invitation 付きで再 join しても冪等に 204 (membership 重複なし)
// + invitation 消費。room-full でも既存 member なら拒否しない (TS との意図的差異)。
func TestRoomsJoin_Idempotent(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u2.ID}
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: u1.ID, RoomID: "r1"}))
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: u1.ID, RoomID: "r1"}))
	// 念のため room を満杯にしても既存 member は通る。
	for i := 0; i < maxRoomMembers; i++ {
		uid := "filler" + string(rune('a'+i))
		require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: uid, UserID: uid, RoomID: "r1"}))
	}
	rec := post(h.RoomsJoin, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)
	mems, _ := repo.ListMembersByRoom("r1")
	assert.Len(t, mems, maxRoomMembers+1, "membership は重複作成されない (既存 m1 + filler 50)")
	assert.Empty(t, repo.Invitations, "再 join でも invitation は消費される")
}

// upstream: membership 数が MAX_ROOM_MEMBERS(50) 以上の room へは新規 join 不可。
func TestRoomsJoin_RoomFull(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u2.ID}
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{ID: "i1", UserID: u1.ID, RoomID: "r1"}))
	for i := 0; i < maxRoomMembers; i++ {
		uid := "filler" + string(rune('a'+i))
		require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: uid, UserID: uid, RoomID: "r1"}))
	}
	rec := post(h.RoomsJoin, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	// room-full 失敗時は invitation を消費せず membership も作らない (再試行可能)。
	assert.NotEmpty(t, repo.Invitations, "room-full では invitation を消費しない")
	_, mErr := repo.FindMembership(u1.ID, "r1")
	assert.Error(t, mErr, "room-full では membership を作らない")
}

// upstream joining.ts は ChatRoomMembership[] (room 付き, id 降順) を返す。
func TestRoomsJoining(t *testing.T) {
	h, repo := newTestHandler()
	idGen, _ := id.NewGenerator("aidx")
	mid1 := idGen.Generate(time.Now())
	mid2 := idGen.Generate(time.Now().Add(time.Second)) // newer
	// owner を populate して nested room.owner (required UserLite) も検証する。
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r1", Name: "General", OwnerID: u2.ID, Owner: u2}))
	require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: "r2", Name: "Other", OwnerID: u2.ID, Owner: u2}))
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: mid1, UserID: u1.ID, RoomID: "r1"}))
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: mid2, UserID: u1.ID, RoomID: "r2"}))
	rec := post(h.RoomsJoining, `{}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	// id 降順 (新しい mid2 が先頭)。
	assert.Equal(t, mid2, rows[0]["id"])
	assert.Equal(t, mid1, rows[1]["id"])
	assert.Equal(t, u1.ID, rows[0]["userId"])
	assert.NotEmpty(t, rows[0]["createdAt"])
	room, ok := rows[0]["room"].(map[string]any)
	require.True(t, ok, "membership は room (ChatRoom) を populate する")
	assert.Equal(t, "r2", room["id"])
	owner, ok := room["owner"].(map[string]any)
	require.True(t, ok, "room.owner (UserLite) must be present")
	assert.Equal(t, "u2", owner["id"])
	// joining は populateUser:false なので user は含めない。
	_, hasUser := rows[0]["user"]
	assert.False(t, hasUser)
}

// repo error 時は 500 を返す。
func TestRoomsJoining_RepoError(t *testing.T) {
	h, repo := newTestHandler()
	repo.ListMembershipsErr = errMock
	rec := post(h.RoomsJoining, `{}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRoomsMembers(t *testing.T) {
	h, repo := newTestHandler()
	assert.Equal(t, http.StatusBadRequest, post(h.RoomsMembers, `{}`, u1).Code)
	// 存在しない room は NO_SUCH_ROOM (endpoint 固有 id)。
	rec0 := post(h.RoomsMembers, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusBadRequest, rec0.Code)
	assertErrorCode(t, rec0, "NO_SUCH_ROOM", "7b9fe84c-eafc-4d21-bf89-485458ed2c18")
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u2.ID}
	// 非 member は NO_SUCH_ROOM (members は moderator も許可しない)。
	h.SetModeratorChecker(fakeModeratorChecker{mods: map[string]bool{u1.ID: true}})
	assert.Equal(t, http.StatusBadRequest, post(h.RoomsMembers, `{"roomId":"r1"}`, u1).Code)
	// member なら閲覧可。
	require.NoError(t, repo.CreateMembership(&model.ChatRoomMembership{ID: "m1", UserID: u1.ID, RoomID: "r1"}))
	rec := post(h.RoomsMembers, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// owner は member 扱いで自室の member 一覧を閲覧できる (isRoomMember の owner 分岐)。
func TestRoomsMembers_OwnerAllowed(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	rec := post(h.RoomsMembers, `{"roomId":"r1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1771: rooms/members の entry は createdAt を含み (required)、schema に無い
// isMuted は出さない。
func TestRoomsMembers_EntryShape(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	idGen, _ := id.NewGenerator("aidx")
	mid := idGen.Generate(time.Now())
	repo.Memberships["u2:r1"] = &model.ChatRoomMembership{ID: mid, UserID: u2.ID, RoomID: "r1", IsMuted: true, User: u2}
	rec := post(h.RoomsMembers, `{"roomId":"r1"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.NotEmpty(t, out[0]["createdAt"], "createdAt は required")
	_, hasMuted := out[0]["isMuted"]
	assert.False(t, hasMuted, "isMuted は ChatRoomMembership schema に無いので出さない")
}

func TestMembersUpdateMembership_Auth(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	repo.Memberships["u2:r1"] = &model.ChatRoomMembership{ID: "m1", UserID: u2.ID, RoomID: "r1"}
	// owner can update
	tr := true
	_ = tr
	rec := post(h.MembersUpdateMembership, `{"roomId":"r1","userId":"u2","isMuted":true}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// non-owner → 404
	assert.Equal(t, http.StatusBadRequest, post(h.MembersUpdateMembership, `{"roomId":"r1","userId":"u2"}`, u2).Code)
	// not a member → 404
	assert.Equal(t, http.StatusNotFound, post(h.MembersUpdateMembership, `{"roomId":"r1","userId":"ghost"}`, u1).Code)
}

// --- AttachedChatMessages (#1218) ---

type fakeModeratorChecker struct{ mods map[string]bool }

func (f fakeModeratorChecker) IsModerator(userID string) bool { return f.mods[userID] }

func attachedHandler(t *testing.T) (*Handler, *testutil.MockChatRepository, *testutil.MockDriveFileRepository) {
	t.Helper()
	h, repo := newTestHandler()
	fileRepo := testutil.NewMockDriveFileRepository()
	h.SetDriveFileRepo(fileRepo)
	h.SetModeratorChecker(fakeModeratorChecker{mods: map[string]bool{"mod1": true}})
	return h, repo, fileRepo
}

func seedFileMsg(repo *testutil.MockChatRepository, id, fileID string) {
	fid := fileID
	to := "u9"
	repo.Messages[id] = &model.ChatMessage{ID: id, FromUserID: "sender", ToUserID: &to, FileID: &fid}
}

func TestAttachedChatMessages_OwnerSeesMessages(t *testing.T) {
	h, repo, fileRepo := attachedHandler(t)
	owner := u1.ID
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner}
	seedFileMsg(repo, "m1", "f1")
	seedFileMsg(repo, "m2", "f1")
	seedFileMsg(repo, "m3", "other") // 別 file は除外

	rec := post(h.AttachedChatMessages, `{"fileId":"f1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 2)
}

func TestAttachedChatMessages_NonOwnerRejected(t *testing.T) {
	h, repo, fileRepo := attachedHandler(t)
	owner := u1.ID
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner}
	seedFileMsg(repo, "m1", "f1")
	// u2 は owner でも moderator でもない → NoSuchFile。
	rec := post(h.AttachedChatMessages, `{"fileId":"f1"}`, u2)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAttachedChatMessages_ModeratorSeesOthersFile(t *testing.T) {
	h, repo, fileRepo := attachedHandler(t)
	owner := u1.ID
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner}
	seedFileMsg(repo, "m1", "f1")
	rec := post(h.AttachedChatMessages, `{"fileId":"f1"}`, &model.User{ID: "mod1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 1)
}

func TestAttachedChatMessages_MissingFileID(t *testing.T) {
	h, _, _ := attachedHandler(t)
	rec := post(h.AttachedChatMessages, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAttachedChatMessages_NoSuchFile(t *testing.T) {
	h, _, _ := attachedHandler(t)
	rec := post(h.AttachedChatMessages, `{"fileId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAttachedChatMessages_NoFileRepoReturnsEmpty(t *testing.T) {
	h, _ := newTestHandler() // fileRepo 未配線
	rec := post(h.AttachedChatMessages, `{"fileId":"f1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// #1556 room message の reactions は {user, reaction} を含む (full/room schema)。
// user を解決できない reaction (削除済 user 等) は drop して user 欠落を防ぐ。
func TestPackMessageDetailed_RoomReactionsIncludeUser(t *testing.T) {
	h, _ := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	h.SetUserRepo(userRepo)

	roomID := "room1"
	m := &model.ChatMessage{ID: "m1", FromUserID: "u2", ToRoomID: &roomID,
		Reactions: []string{"u1/👍", "ghost/😀"}}
	out := h.packMessageDetailed(m, "viewer")

	reactions, ok := out["reactions"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, reactions, 1, "u1 は解決され、ghost (未解決) は drop")
	assert.Equal(t, "👍", reactions[0]["reaction"])
	user, ok := reactions[0]["user"].(map[string]any)
	require.True(t, ok, "room reaction は user (UserLite) を含む")
	assert.Equal(t, "u1", user["id"])
}

// 1on1 (lite) message の reactions は user を含まない。
// #2106 L18: 1on1 message の reactions も room と同じく user を含む (full ChatMessage schema)。
func TestPackMessageDetailed_1on1ReactionsIncludeUser(t *testing.T) {
	h, _ := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	h.SetUserRepo(userRepo)

	toUser := "u3"
	m := &model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &toUser,
		Reactions: []string{"u1/👍"}}
	out := h.packMessageDetailed(m, "viewer")

	reactions, ok := out["reactions"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, reactions, 1)
	assert.Equal(t, "👍", reactions[0]["reaction"])
	user, hasUser := reactions[0]["user"]
	assert.True(t, hasUser, "1on1 reactions も user を含む (frontend が record.user.id を参照)")
	require.NotNil(t, user)
}

// userRepo 未配線の room message は base の {reaction} に degrade する (crash しない)。
func TestPackMessageDetailed_RoomReactionsNoUserRepoDegrades(t *testing.T) {
	h, _ := newTestHandler() // SetUserRepo を呼ばない
	roomID := "room1"
	m := &model.ChatMessage{ID: "m1", FromUserID: "u2", ToRoomID: &roomID,
		Reactions: []string{"u1/👍"}}
	out := h.packMessageDetailed(m, "viewer")
	reactions := out["reactions"].([]map[string]any)
	require.Len(t, reactions, 1)
	assert.Equal(t, "👍", reactions[0]["reaction"])
}

// 譲渡は保留中の招待も消す。残すと新 owner がそれを accept でき、消したばかりの
// membership 行が作り直される (#2858 のレビューで 3 人が独立に再現した経路)。
func TestRoomsTransferOwnership_ConsumesPendingInvitation(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	// メンバーでありながら招待も残っている状態。invitations/create は既存
	// メンバーを弾くので通常は生まれないが、#2858 以前のデータには存在しうる。
	seedMember(t, repo, "u2", "r1")
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
		ID: "inv1", UserID: "u2", RoomID: "r1",
	}))

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)

	_, err := repo.FindInvitation("u2", "r1")
	assert.ErrorIs(t, err, testutil.ErrNotFound, "新 owner 宛の招待は残らない")

	// 招待が残っていたら accept で行が作り直せてしまうので、その経路も塞がって
	// いることを確認する。
	rec = post(h.InvitationsAccept, `{"roomId":"r1"}`, u2)
	assert.Equal(t, http.StatusNotFound, rec.Code, "消費済みなので accept できない")
	members, err := repo.ListMembersByRoom("r1")
	require.NoError(t, err)
	assert.Len(t, members, 1, "旧 owner の 1 行だけ")
}

// 招待が別経路で残っていても owner の accept は membership 行を作らない
// (多層防御。invitation の削除が漏れた場合の受け皿)。
func TestInvitationsAccept_OwnerDoesNotCreateMembership(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u2"}
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
		ID: "inv1", UserID: "u2", RoomID: "r1",
	}))

	rec := post(h.InvitationsAccept, `{"roomId":"r1"}`, u2)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	members, err := repo.ListMembersByRoom("r1")
	require.NoError(t, err)
	assert.Empty(t, members, "owner は membership 行を持たない")
	_, err = repo.FindInvitation("u2", "r1")
	assert.ErrorIs(t, err, testutil.ErrNotFound, "招待は消費する")
}

// rooms/join も同じ (invitation 必須の経路がもう 1 つある)。
func TestRoomsJoin_OwnerDoesNotCreateMembership(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u2"}
	require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
		ID: "inv1", UserID: "u2", RoomID: "r1",
	}))

	rec := post(h.RoomsJoin, `{"roomId":"r1"}`, u2)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	members, err := repo.ListMembersByRoom("r1")
	require.NoError(t, err)
	assert.Empty(t, members, "owner は membership 行を持たない")
}

// **非メンバーへは譲渡できない** (#2858)。旧 owner を membership 行として残す
// 以上、非メンバーへ渡せると「同意していない相手の room に投稿し続けられ、
// 相手は owner なので mute もできない」一方通行の経路になる。
func TestRoomsTransferOwnership_NonMemberRejected(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"outsider"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_MEMBER", "05af3d8f-a5d9-4282-8cf0-4fcc5f4e1734")
	assert.Equal(t, "u1", repo.Rooms["r1"].OwnerID)
	members, err := repo.ListMembersByRoom("r1")
	require.NoError(t, err)
	assert.Empty(t, members, "旧 owner の行も作らない")
}

// 存在しない user も「メンバーではない」として同じ経路で弾かれる。ここが通ると
// `chat_room."ownerId"` の FK 違反で原因の分からない 500 になる。
func TestRoomsTransferOwnership_UnknownUserRejected(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assertErrorCode(t, rec, "NO_SUCH_MEMBER", "05af3d8f-a5d9-4282-8cf0-4fcc5f4e1734")
}

// mock が引数として渡された room を書き換えないこと。実 repository は DB 行しか
// 更新しないので、handler が握っている room オブジェクトは旧 owner のまま残る。
// mock が同じポインタを書き換えると、将来 handler がレスポンスに room を載せた
// ときに「テストだけ通って本番は旧 owner を返す」形になる。
func TestRoomsTransferOwnership_DoesNotMutateCallerRoom(t *testing.T) {
	h, repo := newTestHandler()
	room := &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	repo.Rooms["r1"] = room
	seedMember(t, repo, "u2", "r1")

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)

	assert.Equal(t, "u1", room.OwnerID, "呼び出し元が握る room は書き換えない")
	assert.Equal(t, "u2", repo.Rooms["r1"].OwnerID, "store 側は新 owner")
}

// 自己譲渡は 204 のまま。owner は membership 行を持たないので、早期 return が
// 無いと自分自身が「非メンバー」として NO_SUCH_MEMBER で弾かれる。
func TestRoomsTransferOwnership_ToSelfSkipsMemberCheck(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u1"}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "u1", repo.Rooms["r1"].OwnerID)
	members, err := repo.ListMembersByRoom("r1")
	require.NoError(t, err)
	assert.Empty(t, members, "owner は membership 行を持たない")
}

// **DB 障害を not-found に丸めない** (#2792)。メンバー判定が失敗したら
// 「メンバーではない」ではなく 500 を返す。
func TestRoomsTransferOwnership_MembershipLookupFailureIs500(t *testing.T) {
	h, repo := newTestHandler()
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
	repo.FindMembershipErr = errors.New("db down")

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "u1", repo.Rooms["r1"].OwnerID)
}

// accept / join の owner 判定で room が引けなかった場合も 500 にする。
// not-found に丸めると、DB 障害時に owner が membership 行を作ってしまう。
func TestInvitationAcceptAndJoin_RoomLookupFailureIs500(t *testing.T) {
	boom := errors.New("db down")
	for _, tc := range []struct {
		name string
		call func(*Handler) *httptest.ResponseRecorder
	}{
		{"accept", func(h *Handler) *httptest.ResponseRecorder {
			return post(h.InvitationsAccept, `{"roomId":"r1"}`, u2)
		}},
		{"join", func(h *Handler) *httptest.ResponseRecorder {
			return post(h.RoomsJoin, `{"roomId":"r1"}`, u2)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newTestHandler()
			repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: "u1"}
			require.NoError(t, repo.CreateInvitation(&model.ChatRoomInvitation{
				ID: "inv1", UserID: "u2", RoomID: "r1",
			}))
			repo.FindRoomErr = boom
			assert.Equal(t, http.StatusInternalServerError, tc.call(h).Code)
			members, err := repo.ListMembersByRoom("r1")
			require.NoError(t, err)
			assert.Empty(t, members, "失敗時に membership を作らない")
		})
	}
}

// **当事者でなければメッセージを返さない** (upstream show.ts と同じ判定)。
// 以前は messageId さえ分かれば無関係な DM / room の発言と添付 DriveFile の
// URL まで読めた。
func TestMessagesShow_Authorization(t *testing.T) {
	to := "u2"
	other := "u3"
	showRoomID := "r1"
	for _, tc := range []struct {
		name string
		msg  *model.ChatMessage
		me   *model.User
		want int
	}{
		{"sender", &model.ChatMessage{ID: "m1", FromUserID: "u1", ToUserID: &to}, u1, http.StatusOK},
		{"recipient", &model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &to}, u2, http.StatusOK},
		{"third party (DM)", &model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &other}, u1, http.StatusBadRequest},
		{"room message by someone else", &model.ChatMessage{ID: "m1", FromUserID: "u2", ToRoomID: &showRoomID}, u1, http.StatusBadRequest},
		{"own room message", &model.ChatMessage{ID: "m1", FromUserID: "u1", ToRoomID: &showRoomID}, u1, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newTestHandler()
			require.NoError(t, repo.CreateMessage(tc.msg))
			rec := post(h.MessagesShow, `{"messageId":"m1"}`, tc.me)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

// moderator は読める (upstream も isModerator を迂回路にしている)。
func TestMessagesShow_ModeratorCanRead(t *testing.T) {
	h, repo := newTestHandler()
	h.SetModeratorChecker(fakeModeratorChecker{mods: map[string]bool{u1.ID: true}})
	other := "u3"
	require.NoError(t, repo.CreateMessage(&model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &other}))
	rec := post(h.MessagesShow, `{"messageId":"m1"}`, u1)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// **存在の有無を応答で区別しない** (messageId の総当たりを許さない)。
func TestMessagesShow_UnknownIsSameAsForbidden(t *testing.T) {
	h, repo := newTestHandler()
	other := "u3"
	require.NoError(t, repo.CreateMessage(&model.ChatMessage{ID: "m1", FromUserID: "u2", ToUserID: &other}))
	forbidden := post(h.MessagesShow, `{"messageId":"m1"}`, u1)
	missing := post(h.MessagesShow, `{"messageId":"nope"}`, u1)
	assert.Equal(t, forbidden.Code, missing.Code)
	assert.JSONEq(t, forbidden.Body.String(), missing.Body.String())
}

// **譲渡先はローカル利用者に限る (#2994)。** リモート利用者は AP 経由でこちらの
// room のメンバーになれるので、制限が無いと**ローカルの room の owner が
// リモート利用者**になる。そうなると「owner がリモート = 取り込んだ copy」という
// `chat_room.host` / `uri` の前提が崩れ、backfill がこちらの room に偽のリモート
// URI を刻む (以後その room 宛の Accept / group message が恒久的に drop される)。
func TestRoomsTransferOwnership_RejectsRemoteTarget(t *testing.T) {
	h, repo := newTestHandler()
	remoteHost := "remote.example"
	users := testutil.NewMockUserRepository()
	users.Users[u1.ID] = u1
	users.Users["rmt"] = &model.User{ID: "rmt", Username: "bob", Host: &remoteHost}
	h.SetUserRepo(users)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	seedMember(t, repo, "rmt", "r1")

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"rmt"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, u1.ID, repo.Rooms["r1"].OwnerID, "リモート利用者へ譲渡している")
}

// 未配線では譲渡先がローカルか確かめられないので素通しにしない。
func TestRoomsTransferOwnership_WithoutUserRepoIsError(t *testing.T) {
	h, repo := newTestHandler()
	h.SetUserRepo(nil)
	repo.Rooms["r1"] = &model.ChatRoom{ID: "r1", OwnerID: u1.ID}
	seedMember(t, repo, "u2", "r1")

	rec := post(h.RoomsTransferOwnership, `{"roomId":"r1","userId":"u2"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, u1.ID, repo.Rooms["r1"].OwnerID)
}
