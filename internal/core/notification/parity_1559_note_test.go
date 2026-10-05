package notification

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newNoteNotifyHook wires a Hook with following / renote-muting repos so the
// 'note' notification fan-out (#1559) is active.
func newNoteNotifyHook(t *testing.T) (*Hook, *Service, *testutil.MockUserRepository, *testutil.MockFollowingRepository, *testutil.MockRenoteMutingRepository) {
	t.Helper()
	h, svc, userRepo := newTestHook(t)
	followingRepo := testutil.NewMockFollowingRepository()
	renoteMutingRepo := testutil.NewMockRenoteMutingRepository()
	h.SetNoteNotifyRepos(followingRepo, renoteMutingRepo)
	return h, svc, userRepo, followingRepo, renoteMutingRepo
}

// addNotifyFollower registers `follower` as a local user that follows
// `followee` with notify='normal'.
func addNotifyFollower(repo *testutil.MockUserRepository, fr *testutil.MockFollowingRepository, follower, followee string) {
	addLocalUser(repo, follower, follower)
	normal := "normal"
	fr.Followings[follower+"->"+followee] = &model.Following{
		ID:         follower + "->" + followee,
		FollowerID: follower,
		FolloweeID: followee,
		Notify:     &normal,
	}
}

// #1559 [MEDIUM] notify='normal' フォロワーは通常ノートで 'note' 通知を受け取る。
func TestHook_OnNoteCreated_NoteNotificationToNotifyFollower(t *testing.T) {
	h, svc, repo, fr, _ := newNoteNotifyHook(t)
	addLocalUser(repo, "alice", "alice")
	addNotifyFollower(repo, fr, "bob", "alice")

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, nil, nil)

	out, err := svc.List(context.Background(), "bob", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeNote, out[0].Type)
	assert.Equal(t, "alice", out[0].NotifierID)
	assert.Equal(t, "n1", out[0].NoteID)
}

// reply には note 通知を付けない (upstream は data.reply == null のときだけ発火)。
func TestHook_OnNoteCreated_NoteNotificationSkippedForReply(t *testing.T) {
	h, svc, repo, fr, _ := newNoteNotifyHook(t)
	addLocalUser(repo, "alice", "alice")
	addNotifyFollower(repo, fr, "bob", "alice")

	parent := &model.Note{ID: "p", UserID: "carol"}
	replyID := "p"
	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic, ReplyID: &replyID}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, parent, nil)

	out, _ := svc.List(context.Background(), "bob", "", "", 10, nil, nil)
	assert.Empty(t, out, "reply は note 通知を発火しない")
}

// specified 可視性は note 通知の対象外。
func TestHook_OnNoteCreated_NoteNotificationSkippedForSpecified(t *testing.T) {
	h, svc, repo, fr, _ := newNoteNotifyHook(t)
	addLocalUser(repo, "alice", "alice")
	addNotifyFollower(repo, fr, "bob", "alice")

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilitySpecified}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, nil, nil)

	out, _ := svc.List(context.Background(), "bob", "", "", 10, nil, nil)
	assert.Empty(t, out, "specified note は note 通知を発火しない")
}

// pure renote は、その投稿者の renote を mute しているフォロワーには送らない。
func TestHook_OnNoteCreated_NoteNotificationSkipsRenoteMuted(t *testing.T) {
	h, svc, repo, fr, rm := newNoteNotifyHook(t)
	addLocalUser(repo, "alice", "alice")
	addNotifyFollower(repo, fr, "bob", "alice")
	// bob は alice の renote を mute している
	rm.Mutings["m1"] = &model.RenoteMuting{ID: "m1", MuterID: "bob", MuteeID: "alice"}

	target := "t1"
	original := &model.Note{ID: target, UserID: "carol"}
	note := &model.Note{ID: "r1", UserID: "alice", Visibility: model.NoteVisibilityPublic, RenoteID: &target}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, nil, original)

	out, _ := svc.List(context.Background(), "bob", "", "", 10, nil, nil)
	assert.Empty(t, out, "renote-mute 済みフォロワーには pure renote の note 通知を送らない")
}

// quote renote は renote-mute の対象外で、note 通知が届く。
func TestHook_OnNoteCreated_NoteNotificationQuoteIgnoresRenoteMute(t *testing.T) {
	h, svc, repo, fr, rm := newNoteNotifyHook(t)
	addLocalUser(repo, "alice", "alice")
	addNotifyFollower(repo, fr, "bob", "alice")
	rm.Mutings["m1"] = &model.RenoteMuting{ID: "m1", MuterID: "bob", MuteeID: "alice"}

	target := "t1"
	original := &model.Note{ID: target, UserID: "carol"}
	text := "quoted"
	note := &model.Note{ID: "q1", UserID: "alice", Visibility: model.NoteVisibilityPublic, RenoteID: &target, Text: &text}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, nil, original)

	out, err := svc.List(context.Background(), "bob", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeNote, out[0].Type)
}

// notify を設定していないフォロワーには note 通知を送らない。
func TestHook_OnNoteCreated_NoteNotificationSkipsNonNotifyFollower(t *testing.T) {
	h, svc, repo, fr, _ := newNoteNotifyHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")
	// notify 無しの follow edge
	fr.Followings["bob->alice"] = &model.Following{ID: "f", FollowerID: "bob", FolloweeID: "alice"}

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, nil, nil)

	out, _ := svc.List(context.Background(), "bob", "", "", 10, nil, nil)
	assert.Empty(t, out, "notify 未設定のフォロワーには送らない")
}

// followingRepo 未配線なら note 通知の fan-out 自体が無効。
func TestHook_OnNoteCreated_NoteNotificationDisabledWithoutRepo(t *testing.T) {
	h, svc, repo := newTestHook(t) // SetNoteNotifyRepos を呼ばない
	addLocalUser(repo, "alice", "alice")

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, nil, nil)

	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	assert.Empty(t, out)
}

// #1559 login / createToken / test は notifier を持たず本人へ届く bare 通知。
func TestHook_SelfNotifications(t *testing.T) {
	cases := []struct {
		name string
		fire func(h *Hook, userID string)
		want Type
	}{
		{"login", func(h *Hook, u string) { h.OnLogin(u) }, TypeLogin},
		{"createToken", func(h *Hook, u string) { h.OnCreateToken(u) }, TypeCreateToken},
		{"test", func(h *Hook, u string) { h.OnTest(u) }, TypeTest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, svc, repo := newTestHook(t)
			addLocalUser(repo, "alice", "alice")
			c.fire(h, "alice")
			out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
			require.NoError(t, err)
			require.Len(t, out, 1)
			assert.Equal(t, c.want, out[0].Type)
			assert.Empty(t, out[0].NotifierID, "self-notification は notifier を持たない")
		})
	}
}

// #1559 [MEDIUM] OnRoleAssigned は roleId を Extra に格納した通知を本人へ作る。
func TestHook_OnRoleAssigned(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	h.OnRoleAssigned("alice", "role1")
	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeRoleAssigned, out[0].Type)
	assert.Empty(t, out[0].NotifierID)
	assert.Equal(t, "role1", out[0].Extra["roleId"])
}

// roleAssigned 通知も remote user 宛ては送られない。
func TestHook_OnRoleAssigned_RemoteSkipped(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addRemoteUser(repo, "remote", "remote", "example.com")
	h.OnRoleAssigned("remote", "role1")
	out, _ := svc.List(context.Background(), "remote", "", "", 10, nil, nil)
	assert.Empty(t, out)
}

// #1559 [MEDIUM] OnChatRoomInvitationReceived は notifier(招待者) と invitationId
// を持つ通知を被招待ユーザーへ作る。
func TestHook_OnChatRoomInvitationReceived(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "invitee", "invitee")
	addLocalUser(repo, "inviter", "inviter")
	h.OnChatRoomInvitationReceived("invitee", "inviter", "inv1")
	out, err := svc.List(context.Background(), "invitee", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeChatRoomInvitationReceived, out[0].Type)
	assert.Equal(t, "inviter", out[0].NotifierID)
	assert.Equal(t, "inv1", out[0].Extra["invitationId"])
}

// remote invitee 宛ては送られない。
func TestHook_OnChatRoomInvitationReceived_RemoteSkipped(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addRemoteUser(repo, "remote", "remote", "example.com")
	h.OnChatRoomInvitationReceived("remote", "inviter", "inv1")
	out, _ := svc.List(context.Background(), "remote", "", "", 10, nil, nil)
	assert.Empty(t, out)
}

// 自己通知でも remote user 宛ては送られない (notifyLocalUser の host guard)。
func TestHook_SelfNotifications_RemoteSkipped(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addRemoteUser(repo, "remote", "remote", "example.com")
	h.OnLogin("remote")
	out, _ := svc.List(context.Background(), "remote", "", "", 10, nil, nil)
	assert.Empty(t, out)
}

// 自己フォロー edge (follower == author) は note 通知から除外する。
func TestHook_OnNoteCreated_NoteNotificationSkipsSelfFollow(t *testing.T) {
	h, svc, repo, fr, _ := newNoteNotifyHook(t)
	addLocalUser(repo, "alice", "alice")
	normal := "normal"
	fr.Followings["self"] = &model.Following{ID: "self", FollowerID: "alice", FolloweeID: "alice", Notify: &normal}

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, nil, nil)

	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	assert.Empty(t, out, "自己フォロー edge には note 通知を送らない")
}

// failingFollowingNotifyRepo は ListFollowersToNotify でエラーを返す。
type failingFollowingNotifyRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFollowingNotifyRepo) ListFollowersToNotify(string) ([]*model.Following, error) {
	return nil, assertErr{}
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }

// ListFollowersToNotify が失敗しても panic せず、note 通知を発火しない。
func TestHook_OnNoteCreated_NoteNotificationListError(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	h.SetNoteNotifyRepos(&failingFollowingNotifyRepo{MockFollowingRepository: testutil.NewMockFollowingRepository()}, nil)

	note := &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	assert.NotPanics(t, func() { h.OnNoteCreated(note, &model.User{ID: "alice"}, nil, nil) })
	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	assert.Empty(t, out)
}
