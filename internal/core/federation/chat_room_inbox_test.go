package federation_test

import (
	"context"
	"errors"
	"testing"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeChatMessageReceiver is a stub for the 1-on-1 chat path so tests can
// distinguish 1-on-1 routing from group (room) routing.
type fakeChatMessageReceiver struct{ calls int }

func (f *fakeChatMessageReceiver) CreateMessageViaAP(_ context.Context, _ string, _ *model.User, _, _, _ string) (*model.ChatMessage, error) {
	f.calls++
	return &model.ChatMessage{}, nil
}

// fakeChatRoomReceiver records inbound chat room federation calls.
type fakeChatRoomReceiver struct {
	ensureCalls   [][4]string // roomURI, name, summary, ownerUserID
	inviteCalls   [][2]string // roomURI, inviteeUserID
	memberCalls   [][2]string // roomURI, userID
	removeCalls   [][2]string // roomURI, userID (RemoveInvitationViaAP)
	rmMemberCalls [][2]string // roomURI, userID (RemoveMemberViaAP)
	msgCalls      [][3]string // roomURI, senderID, text
	ensureErr     error
	inviteErr     error
	msgErr        error
	memberErr     error
	removeErr     error
	rmMemberErr   error
}

func (f *fakeChatRoomReceiver) CreateRoomMessageViaAP(uri string, sender *model.User, roomURI, text, mfmSource string) error {
	sid := ""
	if sender != nil {
		sid = sender.ID
	}
	f.msgCalls = append(f.msgCalls, [3]string{roomURI, sid, text})
	return f.msgErr
}

func (f *fakeChatRoomReceiver) EnsureRoomViaAP(roomURI, name, summary, ownerUserID string) error {
	f.ensureCalls = append(f.ensureCalls, [4]string{roomURI, name, summary, ownerUserID})
	return f.ensureErr
}

func (f *fakeChatRoomReceiver) CreateInvitationViaAP(roomURI, inviteeUserID string) error {
	f.inviteCalls = append(f.inviteCalls, [2]string{roomURI, inviteeUserID})
	return f.inviteErr
}

func (f *fakeChatRoomReceiver) AddMemberViaAP(roomURI, userID string) error {
	f.memberCalls = append(f.memberCalls, [2]string{roomURI, userID})
	if f.memberErr != nil {
		return f.memberErr
	}
	return nil
}

func (f *fakeChatRoomReceiver) RemoveInvitationViaAP(roomURI, userID string) error {
	f.removeCalls = append(f.removeCalls, [2]string{roomURI, userID})
	if f.removeErr != nil {
		return f.removeErr
	}
	return nil
}

func (f *fakeChatRoomReceiver) RemoveMemberViaAP(roomURI, userID string) error {
	f.rmMemberCalls = append(f.rmMemberCalls, [2]string{roomURI, userID})
	return f.rmMemberErr
}

func TestProcess_ChatRoomInvite_CreatesRoomCopyAndInvitation(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	// invitee は local user bob。
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/users/bob",
		"object": {
			"type": "Group",
			"id": "https://remote.example/chat/rooms/room1",
			"name": "General",
			"summary": "desc",
			"attributedTo": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, p.Process(body))

	require.Len(t, recv.ensureCalls, 1)
	assert.Equal(t, "https://remote.example/chat/rooms/room1", recv.ensureCalls[0][0])
	assert.Equal(t, "General", recv.ensureCalls[0][1])
	assert.Equal(t, "desc", recv.ensureCalls[0][2])
	assert.NotEmpty(t, recv.ensureCalls[0][3], "owner (resolved remote actor) must be set")
	require.Len(t, recv.inviteCalls, 1)
	assert.Equal(t, [2]string{"https://remote.example/chat/rooms/room1", "bob"}, recv.inviteCalls[0])
}

func TestProcess_ChatRoomAccept_AddsMembership(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)

	// remote alice が我々の room 招待を accept した。
	body := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"actor": "https://example.com/users/owner",
			"target": "https://remote.example/users/alice",
			"object": {
				"type": "Group",
				"id": "https://example.com/chat/rooms/room1",
				"name": "General"
			}
		}
	}`)
	require.NoError(t, p.Process(body))

	require.Len(t, recv.memberCalls, 1)
	assert.Equal(t, "https://example.com/chat/rooms/room1", recv.memberCalls[0][0])
	assert.NotEmpty(t, recv.memberCalls[0][1])
}

// remote member の leave を伝える Remove(target=chat room) で actor の
// membership が削除される (#1364)。
func TestProcess_ChatRoomRemove_DeletesMembership(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	body := []byte(`{
		"type": "Remove",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/chat/rooms/room1",
		"object": "https://remote.example/users/alice"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, recv.rmMemberCalls, 1)
	assert.Equal(t, "https://example.com/chat/rooms/room1", recv.rmMemberCalls[0][0])
	assert.NotEmpty(t, recv.rmMemberCalls[0][1], "actor (resolved remote user) membership is removed")
}

// RemoveMemberViaAP が ErrNotFound を返す (room/membership 不在) と恒久的失敗
// として ErrUnsupportedActivity に畳む (retry させない)。
func TestProcess_ChatRoomRemove_NotFoundIsPermanent(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{rmMemberErr: corechat.ErrNotFound}
	p.SetChatRoomReceiver(recv)
	body := []byte(`{
		"type": "Remove",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/chat/rooms/room1",
		"object": "https://remote.example/users/alice"
	}`)
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

// RemoveMemberViaAP の generic error (ErrNotFound/ErrInvalidTarget 以外) は
// retryable として propagate される (ErrUnsupportedActivity に畳まない)。
func TestProcess_ChatRoomRemove_GenericErrorPropagates(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{rmMemberErr: errors.New("db down")}
	p.SetChatRoomReceiver(recv)
	body := []byte(`{
		"type": "Remove",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/chat/rooms/room1",
		"object": "https://remote.example/users/alice"
	}`)
	err := p.Process(body)
	require.Error(t, err)
	assert.NotErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_ChatRoomReject_RemovesInvitation(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)

	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"actor": "https://example.com/users/owner",
			"object": {
				"type": "Group",
				"id": "https://example.com/chat/rooms/room1",
				"name": "General"
			}
		}
	}`)
	require.NoError(t, p.Process(body))

	require.Len(t, recv.removeCalls, 1)
	assert.Equal(t, "https://example.com/chat/rooms/room1", recv.removeCalls[0][0])
}

func TestProcess_ChatRoomInvite_MissingTarget(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"object": {"type": "Group", "id": "https://remote.example/chat/rooms/room1", "name": "G"}
	}`)
	// target が無い Invite は恒久的失敗なので ErrUnsupportedActivity
	// (retry させない)。room/invitation は作られない。
	err := p.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.ensureCalls)
}

func TestProcess_ChatRoomInvite_InviteeNotLocal(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"target": "https://remote.example/users/charlie",
		"object": {"type": "Group", "id": "https://remote.example/chat/rooms/room1", "name": "G"}
	}`)
	// invitee が local user でなければ恒久的失敗 → ErrUnsupportedActivity。
	err := p.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.inviteCalls)
}

func TestProcess_ChatRoomInvite_NoReceiverWired(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	// chatRoomReceiver 未配線でも panic せず未対応扱いになる。
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/users/bob",
		"object": {"type": "Group", "id": "https://remote.example/chat/rooms/room1", "name": "G"}
	}`)
	assert.Error(t, p.Process(body))
}

func TestProcess_ChatRoomAccept_NonGroupInnerIsNoOp(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	// inner が Invite だが object が Group でない (Game) → membership 化しない。
	body := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"object": {"type": "Game", "id": "https://remote.example/reversi/g1"}
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, recv.memberCalls)
}

func TestProcess_ChatRoomInvite_BadRoomIDFallsThrough(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	// Group だが id が /chat/rooms/ 形式でない → chat room 経路に入らない。
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"object": {"type": "Group", "id": "https://remote.example/groups/x"}
	}`)
	_ = p.Process(body)
	assert.Empty(t, recv.ensureCalls)
}

func TestProcess_ChatRoomInvite_EnsureRoomError(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{ensureErr: errors.New("boom")}
	p.SetChatRoomReceiver(recv)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/users/bob",
		"object": {"type": "Group", "id": "https://remote.example/chat/rooms/room1", "name": "G"}
	}`)
	// EnsureRoomViaAP が一過性エラー (DB 等) なら retryable error を伝播。
	err := p.Process(body)
	require.Error(t, err)
	assert.NotErrorIs(t, err, federation.ErrUnsupportedActivity, "transient error must stay retryable")
	assert.Empty(t, recv.inviteCalls)
}

func TestProcess_ChatRoomInvite_OwnerMismatchNotRetried(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{ensureErr: corechat.ErrRoomOwnerMismatch}
	p.SetChatRoomReceiver(recv)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/users/bob",
		"object": {"type": "Group", "id": "https://remote.example/chat/rooms/room1", "name": "G"}
	}`)
	// roomId が無関係なローカル room と衝突する恒久的失敗は retry させない。
	err := p.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.inviteCalls)
}

func TestProcess_ChatRoomInvite_CreateInvitationError(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{inviteErr: errors.New("boom")}
	p.SetChatRoomReceiver(recv)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"target": "https://example.com/users/bob",
		"object": {"type": "Group", "id": "https://remote.example/chat/rooms/room1", "name": "G"}
	}`)
	require.Error(t, p.Process(body))
	require.Len(t, recv.ensureCalls, 1)
}

func TestProcess_ChatRoomAccept_NoReceiverIsNoOp(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	// receiver 未配線でも accept は panic せず no-op。
	body := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"object": {"type": "Group", "id": "https://example.com/chat/rooms/room1"}
		}
	}`)
	require.NoError(t, p.Process(body))
}

func TestProcess_ChatRoomReject_NonGroupAndNoReceiver(t *testing.T) {
	// receiver 未配線 + group inner でも reject は no-op。
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"object": {"type": "Group", "id": "https://example.com/chat/rooms/room1"}
		}
	}`)
	require.NoError(t, p.Process(body))

	// receiver 配線済 + inner が非 Group → no-op (membership/remove 共に呼ばれない)。
	p2, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p2.SetChatRoomReceiver(recv)
	body2 := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {"type": "Invite", "object": {"type": "Game", "id": "https://remote.example/reversi/g1"}}
	}`)
	require.NoError(t, p2.Process(body2))
	assert.Empty(t, recv.removeCalls)
}

func TestProcess_ChatRoomMessage_PersistedToRoom(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	// note の @context が room URI の group chat message。
	//
	// **`to` に複数の recipient が並んでいても room 経路で 1 回だけ取り込む。**
	// 1-on-1 経路 (`handleChatCreate`) が「`to` の先頭しか見ない」ことを group
	// chat の欠落と誤読した issue (#2408) があったので明記しておく。room の
	// メンバー全員へ配るのは受け側 (`CreateRoomMessageViaAP`) の責務で、
	// recipient 配列は参照しない。
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat/messages/m1",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "hello room",
			"to": ["https://example.com/users/bob", "https://remote.example/users/alice"],
			"_misskey_talk": true,
			"@context": "https://remote.example/chat/rooms/room1"
		}
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, recv.msgCalls, 1)
	assert.Equal(t, "https://remote.example/chat/rooms/room1", recv.msgCalls[0][0])
	assert.NotEmpty(t, recv.msgCalls[0][1], "sender (resolved remote actor) must be set")
	assert.Equal(t, "hello room", recv.msgCalls[0][2])
}

func TestProcess_OneOnOneChat_NotRoutedToRoom(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	chatMsg := &fakeChatMessageReceiver{}
	p.SetChatService(chatMsg)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI, ChatScope: "everyone"}
	// @context が無い (room URI でない) ので 1-on-1 経路を通り room には行かない。
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat/messages/m1",
			"type": "Note",
			"content": "hi bob",
			"to": ["https://example.com/users/bob"],
			"_misskey_talk": true
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, recv.msgCalls, "1-on-1 message must not hit room federation")
	assert.Equal(t, 1, chatMsg.calls, "1-on-1 message must route to the 1-on-1 chat receiver")
}

func TestProcess_ChatRoomMessage_UnknownRoomDropped(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{msgErr: corechat.ErrNotFound}
	p.SetChatRoomReceiver(recv)
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat/messages/m1",
			"type": "Note",
			"content": "x",
			"to": ["https://remote.example/users/alice"],
			"_misskey_talk": true,
			"@context": "https://remote.example/chat/rooms/ghost"
		}
	}`)
	// 未関与の room は ErrUnsupportedActivity で drop (retry させない)。
	err := p.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_ChatRoomMessage_NonMemberDropped(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{msgErr: corechat.ErrForbidden}
	p.SetChatRoomReceiver(recv)
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat/messages/m1",
			"type": "Note", "content": "x",
			"to": ["https://remote.example/users/alice"],
			"_misskey_talk": true,
			"@context": "https://remote.example/chat/rooms/room1"
		}
	}`)
	// 非メンバー送信は ErrUnsupportedActivity で drop。
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

func TestProcess_ChatRoomMessage_TransientErrorRetryable(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{msgErr: errors.New("db blip")}
	p.SetChatRoomReceiver(recv)
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat/messages/m1",
			"type": "Note", "content": "x",
			"to": ["https://remote.example/users/alice"],
			"_misskey_talk": true,
			"@context": "https://remote.example/chat/rooms/room1"
		}
	}`)
	// 一過性エラー (DB 等) は retryable error のまま伝播させる。
	err := p.Process(body)
	require.Error(t, err)
	assert.NotErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_ChatRoomMessage_NoReceiverIsUnsupported(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	// chatService のみ配線 (chatRoomReceiver 未配線)。group message は probe され
	// るが room receiver が無いので未対応扱いになり、panic しない。
	p.SetChatService(&fakeChatMessageReceiver{})
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat/messages/m1",
			"type": "Note", "content": "x",
			"to": ["https://remote.example/users/alice"],
			"_misskey_talk": true,
			"@context": "https://remote.example/chat/rooms/room1"
		}
	}`)
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

func TestProcess_ChatRoomMessage_MissingNoteID(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)
	// note id が空の group message は永続化に進めない。
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "",
			"type": "Note", "content": "x",
			"to": ["https://remote.example/users/alice"],
			"_misskey_talk": true,
			"@context": "https://remote.example/chat/rooms/room1"
		}
	}`)
	// note id 欠落は恒久的失敗なので ErrUnsupportedActivity (retry させない)。
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
	assert.Empty(t, recv.msgCalls)
}

func TestProcess_NonGroupInvite_NotRoutedToChatRoom(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	recv := &fakeChatRoomReceiver{}
	p.SetChatRoomReceiver(recv)

	// object が Game (reversi) の Invite は chat room 経路に入らない。
	// reversi 未配線なので未対応扱いになるが、chatRoomReceiver は呼ばれない。
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"object": {"type": "Game", "id": "https://remote.example/reversi/g1"}
	}`)
	_ = p.Process(body)
	assert.Empty(t, recv.ensureCalls, "Game invite must not hit chat room federation")
}

// **知らない room への Accept / Reject は retry しない (#2994)。** room を URI で
// 引くようになったので、取り込んでいない room は `ErrNotFound` で返る。生で返すと
// inbox job が retry を使い切って dead になる (待っても room は現れない)。
func TestProcess_ChatRoomAcceptReject_UnknownRoomIsAcked(t *testing.T) {
	const acceptBody = `{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"actor": "https://example.com/users/owner",
			"target": "https://remote.example/users/alice",
			"object": {"type": "Group", "id": "https://example.com/chat/rooms/gone", "name": "G"}
		}
	}`
	const rejectBody = `{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"actor": "https://example.com/users/owner",
			"target": "https://remote.example/users/alice",
			"object": {"type": "Group", "id": "https://example.com/chat/rooms/gone", "name": "G"}
		}
	}`
	for _, tc := range []struct {
		name string
		body string
		recv *fakeChatRoomReceiver
	}{
		{"Accept / room が無い", acceptBody, &fakeChatRoomReceiver{memberErr: corechat.ErrNotFound}},
		{"Accept / URI が収まらない", acceptBody, &fakeChatRoomReceiver{memberErr: corechat.ErrInvalidTarget}},
		{"Reject / room が無い", rejectBody, &fakeChatRoomReceiver{removeErr: corechat.ErrNotFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _, _ := newProcessor(t, aliceActor)
			p.SetChatRoomReceiver(tc.recv)
			err := p.Process([]byte(tc.body))
			require.ErrorIs(t, err, federation.ErrUnsupportedActivity,
				"知らない room への応答が retry される")
		})
	}
}

// DB 障害のような一過性の失敗は retry させる (not-found に丸めない、#2792)。
func TestProcess_ChatRoomAccept_TransientErrorIsRetried(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	boom := errors.New("dial tcp: connection refused")
	p.SetChatRoomReceiver(&fakeChatRoomReceiver{memberErr: boom})
	body := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"actor": "https://example.com/users/owner",
			"target": "https://remote.example/users/alice",
			"object": {"type": "Group", "id": "https://example.com/chat/rooms/room1", "name": "G"}
		}
	}`)
	err := p.Process(body)
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, federation.ErrUnsupportedActivity)
}

// **chat メッセージの id は配送してきた actor のホストに縛る (#3037)。**
//
// この分岐は `ingestCreateNote` の手前で短絡するので、通常の note に掛かる
// `id host == attributedTo host` の検査を一度も通らない。縛らないと、署名が
// 通るリモート actor が**任意ホストの URI を名乗って chat メッセージを 1 通
// 送れる**。以後その URI を使う正規の連合 chat メッセージは
// `FindMessageByURI` が hit して黙って捨てられる = 相手のメッセージを先回りして
// 潰せる。
func TestProcess_ChatMessage_RejectsCrossHostID(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{
			name: "1-on-1",
			body: `{
				"type": "Create",
				"actor": "https://remote.example/users/alice",
				"object": {
					"id": "https://victim.example/chat/messages/m1",
					"type": "Note",
					"content": "hi bob",
					"to": ["https://example.com/users/bob"],
					"_misskey_talk": true
				}
			}`,
		},
		{
			name: "room",
			body: `{
				"type": "Create",
				"actor": "https://remote.example/users/alice",
				"object": {
					"id": "https://victim.example/chat/messages/m1",
					"type": "Note",
					"attributedTo": "https://remote.example/users/alice",
					"content": "hello room",
					"to": ["https://example.com/users/bob"],
					"_misskey_talk": true,
					"@context": "https://remote.example/chat/rooms/room1"
				}
			}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, repo, _, _ := newProcessor(t, aliceActor)
			recv := &fakeChatRoomReceiver{}
			p.SetChatRoomReceiver(recv)
			chatMsg := &fakeChatMessageReceiver{}
			p.SetChatService(chatMsg)
			bobURI := "https://example.com/users/bob"
			repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI, ChatScope: "everyone"}

			err := p.Process([]byte(tt.body))
			assert.ErrorIs(t, err, federation.ErrUnsupportedActivity,
				"別ホストの id を名乗る chat メッセージを受け入れている")
			assert.Empty(t, recv.msgCalls, "room 経路に届いている")
			assert.Equal(t, 0, chatMsg.calls, "1-on-1 経路に届いている")
		})
	}
}

// **同じホストなら通ったまま。** これが無いと「chat をすべて拒否する」実装でも
// 上のテストが通る。表記ゆれ (punycode / 大文字) も同一ホストとして扱う。
func TestProcess_ChatMessage_SameHostIDStillAccepted(t *testing.T) {
	for _, id := range []string{
		"https://remote.example/chat/messages/m1",
		"https://REMOTE.example/chat/messages/m2",
		"https://remote.example:443/chat/messages/m3",
	} {
		t.Run(id, func(t *testing.T) {
			p, repo, _, _ := newProcessor(t, aliceActor)
			chatMsg := &fakeChatMessageReceiver{}
			p.SetChatService(chatMsg)
			bobURI := "https://example.com/users/bob"
			repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI, ChatScope: "everyone"}

			body := `{
				"type": "Create",
				"actor": "https://remote.example/users/alice",
				"object": {
					"id": "` + id + `",
					"type": "Note",
					"content": "hi bob",
					"to": ["https://example.com/users/bob"],
					"_misskey_talk": true
				}
			}`
			require.NoError(t, p.Process([]byte(body)))
			assert.Equal(t, 1, chatMsg.calls, "同じホストの id を弾いている")
		})
	}
}
