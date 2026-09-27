package notification

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newTestHook(t *testing.T) (*Hook, *Service, *testutil.MockUserRepository) {
	t.Helper()
	testRedis.FlushAll(context.Background())
	svc := NewService(testRedis.Client, idGen, "")
	// #2106 L35: push が scheduleUnreadPublish guard 経由になったので、Hook test では delay=0 で
	// 同期発火させて push の同期 assert を維持する。
	svc.SetUnreadPublishDelay(0)
	userRepo := testutil.NewMockUserRepository()
	return NewHook(svc, userRepo), svc, userRepo
}

// addLocalUser registers a user with no host so notifyLocalUser delivers.
func addLocalUser(repo *testutil.MockUserRepository, id, username string) {
	repo.Users[id] = &model.User{ID: id, Username: username, UsernameLower: username}
}

func addRemoteUser(repo *testutil.MockUserRepository, id, username, host string) {
	repo.Users[id] = &model.User{ID: id, Username: username, UsernameLower: username, Host: &host}
}

func TestHook_OnNoteCreated_Nil(t *testing.T) {
	h, _, _ := newTestHook(t)
	h.OnNoteCreated(nil, &model.User{ID: "u"}, nil, nil)
	h.OnNoteCreated(&model.Note{}, nil, nil, nil)
}

func TestHook_OnNoteCreated_ReplyNotification(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	parent := &model.Note{ID: "n_parent", UserID: "alice"}
	note := &model.Note{ID: "n_reply", UserID: "bob"}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, parent, nil)

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeReply, out[0].Type)
}

// #1954: reply 先がスレッドをミュートしていれば reply 通知を出さない。
func TestHook_OnNoteCreated_ReplyThreadMutedSkipped(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	muteRepo := testutil.NewMockNoteThreadMutingRepository()
	// alice が thread n_parent (root) をミュート。
	require.NoError(t, muteRepo.Create(&model.NoteThreadMuting{UserID: "alice", ThreadID: "n_parent"}))
	h.SetThreadMutingRepo(muteRepo)

	parent := &model.Note{ID: "n_parent", UserID: "alice"}
	note := &model.Note{ID: "n_reply", UserID: "bob"}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, parent, nil)

	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	assert.Empty(t, out, "thread-muted reply 先には reply 通知を出さない (#1954)")
}

// #1954: reply 先が threaded note (ThreadID 持ち) の場合、その threadId で判定する。
func TestHook_OnNoteCreated_ReplyThreadMuteUsesThreadID(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	muteRepo := testutil.NewMockNoteThreadMutingRepository()
	require.NoError(t, muteRepo.Create(&model.NoteThreadMuting{UserID: "alice", ThreadID: "troot"}))
	h.SetThreadMutingRepo(muteRepo)

	troot := "troot"
	parent := &model.Note{ID: "n_parent", UserID: "alice", ThreadID: &troot}
	note := &model.Note{ID: "n_reply", UserID: "bob"}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, parent, nil)

	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	assert.Empty(t, out, "reply 先ノートの threadId でミュート判定する (#1954)")
}

// #1954: mention 先がスレッドをミュートしていれば mention 通知を出さない。
func TestHook_OnNoteCreated_MentionThreadMutedSkipped(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	muteRepo := testutil.NewMockNoteThreadMutingRepository()
	// alice が thread n_m (note 自身 = root) をミュート。
	require.NoError(t, muteRepo.Create(&model.NoteThreadMuting{UserID: "alice", ThreadID: "n_m"}))
	h.SetThreadMutingRepo(muteRepo)

	note := &model.Note{ID: "n_m", UserID: "bob", Mentions: model.StringArray{"alice"}}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)

	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	assert.Empty(t, out, "thread-muted mention 先には mention 通知を出さない (#1954)")
}

// #1954: renote はスレッドミュートで gate されない (upstream も ungated)。
func TestHook_OnNoteCreated_RenoteThreadMuteNotGated(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	muteRepo := testutil.NewMockNoteThreadMutingRepository()
	require.NoError(t, muteRepo.Create(&model.NoteThreadMuting{UserID: "alice", ThreadID: "n_target"}))
	h.SetThreadMutingRepo(muteRepo)

	target := "n_target"
	original := &model.Note{ID: target, UserID: "alice"}
	note := &model.Note{ID: "n_renote", UserID: "bob", RenoteID: &target}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, original)

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1, "renote はスレッドミュートで抑制されない (#1954)")
	assert.Equal(t, TypeRenote, out[0].Type)
}

func TestHook_OnNoteCreated_ReplyToSelfSkipped(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")

	parent := &model.Note{ID: "n_parent", UserID: "alice"}
	note := &model.Note{ID: "n_reply", UserID: "alice"}
	h.OnNoteCreated(note, &model.User{ID: "alice"}, parent, nil)

	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	assert.Empty(t, out)
}

func TestHook_OnNoteCreated_PureRenote(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	target := "n_target"
	original := &model.Note{ID: target, UserID: "alice"}
	note := &model.Note{ID: "n_renote", UserID: "bob", RenoteID: &target}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, original)

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeRenote, out[0].Type)
}

func TestHook_OnNoteCreated_QuoteRenote(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	target := "n_target"
	original := &model.Note{ID: target, UserID: "alice"}
	text := "quoted"
	note := &model.Note{ID: "n_quote", UserID: "bob", RenoteID: &target, Text: &text}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, original)

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeQuote, out[0].Type)
}

func TestHook_OnNoteCreated_Mention(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	note := &model.Note{
		ID: "n_mention", UserID: "bob",
		Mentions: model.StringArray{"alice"},
	}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeMention, out[0].Type)
}

func TestHook_OnNoteCreated_MentionSkipsSelfAndUnknown(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "bob", "bob")

	note := &model.Note{
		ID: "n_mention", UserID: "bob",
		Mentions: model.StringArray{"bob", "ghost", ""},
	}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)

	// 自分自身&存在しない/空文字はスキップされる
	out, _ := svc.List(context.Background(), "bob", "", "", 10, nil, nil)
	assert.Empty(t, out)
}

func TestHook_OnNoteCreated_MentionDedupedWithReply(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	parent := &model.Note{ID: "n_p", UserID: "alice"}
	note := &model.Note{
		ID: "n_r", UserID: "bob",
		Mentions: model.StringArray{"alice"},
	}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, parent, nil)

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	// reply通知のみで、mentionは抑制される
	require.Len(t, out, 1)
	assert.Equal(t, TypeReply, out[0].Type)
}

func TestHook_OnNoteCreated_NoteUnreadSpecified(t *testing.T) {
	// specified-visibility note の visibleUserIds に含まれる local user に対し
	// note_unread (isSpecified=true) 行が作られる。
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")
	addLocalUser(repo, "carol", "carol")

	unread := testutil.NewMockNoteUnreadRepository()
	h.SetNoteUnreadRepo(unread)

	note := &model.Note{
		ID:             "n_spec",
		UserID:         "bob",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"alice", "carol", "bob"}, // bob 自身は skip
	}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)

	require.Len(t, unread.Rows, 2)
	for _, r := range unread.Rows {
		assert.True(t, r.IsSpecified, r.UserID)
		assert.False(t, r.IsMentioned, r.UserID)
		assert.Equal(t, "n_spec", r.NoteID)
		assert.Equal(t, "bob", r.NoteUserID)
	}
}

func TestHook_OnNoteCreated_NoteUnreadMentionMergesFlags(t *testing.T) {
	// visibleUserIds と mentions 両方に含まれる場合、isSpecified と
	// isMentioned が両方 true になる。
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	unread := testutil.NewMockNoteUnreadRepository()
	h.SetNoteUnreadRepo(unread)

	note := &model.Note{
		ID:             "n_both",
		UserID:         "bob",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"alice"},
		Mentions:       model.StringArray{"alice"},
	}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)

	require.Len(t, unread.Rows, 1)
	assert.True(t, unread.Rows[0].IsSpecified)
	assert.True(t, unread.Rows[0].IsMentioned)
}

func TestHook_OnNoteCreated_NoteUnreadSkipsRemoteUsers(t *testing.T) {
	// visibleUserIds に remote user が含まれていても note_unread は作らない。
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addRemoteUser(repo, "remote1", "remote1", "other.example")
	addLocalUser(repo, "bob", "bob")

	unread := testutil.NewMockNoteUnreadRepository()
	h.SetNoteUnreadRepo(unread)

	note := &model.Note{
		ID:             "n_mix",
		UserID:         "bob",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"alice", "remote1"},
	}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)

	require.Len(t, unread.Rows, 1)
	assert.Equal(t, "alice", unread.Rows[0].UserID)
}

func TestHook_OnNoteCreated_NoteUnreadSkippedWhenRepoUnset(t *testing.T) {
	// repo 未配線なら何もせず panic もしない。
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	note := &model.Note{
		ID:             "n_spec",
		UserID:         "bob",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"alice"},
	}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)
	// no panic, no rows anywhere
}

func TestHook_OnNoteCreated_NoteUnreadPublicNoop(t *testing.T) {
	// public / home 等の specified でない note は isSpecified=false。
	// mention があれば isMentioned=true の行のみ作る。
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	addLocalUser(repo, "bob", "bob")

	unread := testutil.NewMockNoteUnreadRepository()
	h.SetNoteUnreadRepo(unread)

	note := &model.Note{
		ID:         "n_pub",
		UserID:     "bob",
		Visibility: model.NoteVisibilityPublic,
		Mentions:   model.StringArray{"alice"},
	}
	h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)

	require.Len(t, unread.Rows, 1)
	assert.False(t, unread.Rows[0].IsSpecified)
	assert.True(t, unread.Rows[0].IsMentioned)
}

func TestService_MarkAllAsRead_ClearsNoteUnread(t *testing.T) {
	// MarkAllAsRead が呼ばれたら note_unread 行を全削除し、
	// HasUnreadSpecifiedNotes が false に戻ることを確認する (#319 BUG 修正)。
	svc := NewService(testRedis.Client, idGen, "")
	unread := testutil.NewMockNoteUnreadRepository()
	svc.SetNoteUnreadRepo(unread)

	_ = unread.Upsert(&model.NoteUnread{
		ID: "nu1", UserID: "alice", NoteID: "n1", NoteUserID: "bob", IsSpecified: true,
	})
	_ = unread.Upsert(&model.NoteUnread{
		ID: "nu2", UserID: "alice", NoteID: "n2", NoteUserID: "carol", IsMentioned: true,
	})

	has, _ := svc.HasUnreadSpecifiedNotes(context.Background(), "alice")
	require.True(t, has)

	require.NoError(t, svc.MarkAllAsRead(context.Background(), "alice", false))

	has, err := svc.HasUnreadSpecifiedNotes(context.Background(), "alice")
	require.NoError(t, err)
	assert.False(t, has)
	assert.Empty(t, unread.Rows, "alice の note_unread は全削除される")
}

func TestService_Flush_ClearsNoteUnread(t *testing.T) {
	// Flush (account deletion 等) でも note_unread を clear する。
	testRedis.FlushAll(context.Background())
	svc := NewService(testRedis.Client, idGen, "")
	unread := testutil.NewMockNoteUnreadRepository()
	svc.SetNoteUnreadRepo(unread)

	_ = unread.Upsert(&model.NoteUnread{
		ID: "nu1", UserID: "alice", NoteID: "n1", NoteUserID: "bob", IsSpecified: true,
	})

	require.NoError(t, svc.Flush(context.Background(), "alice"))
	assert.Empty(t, unread.Rows)
}

func TestHook_OnNoteCreated_NoteUnreadSkipsMentionWithoutUserRepo(t *testing.T) {
	// userRepo が未配線でも mention 解決を試みずに panic せず、
	// visibleUserIds 経由の note_unread だけ作られる (nil deref 回避)。
	testRedis.FlushAll(context.Background())
	svc := NewService(testRedis.Client, idGen, "")
	h := NewHook(svc, nil) // userRepo 未配線

	unread := testutil.NewMockNoteUnreadRepository()
	h.SetNoteUnreadRepo(unread)

	note := &model.Note{
		ID:             "n_nur",
		UserID:         "bob",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"alice"},
		Mentions:       model.StringArray{"alice", "unknown"},
	}
	// userRepo nil のため notifyLocalUser は no-op になり、recordNoteUnreads
	// は visibleUserIds の alice のみ採用する。
	assert.NotPanics(t, func() {
		h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)
	})
	require.Len(t, unread.Rows, 1)
	assert.Equal(t, "alice", unread.Rows[0].UserID)
	assert.True(t, unread.Rows[0].IsSpecified)
	assert.False(t, unread.Rows[0].IsMentioned, "userRepo 未配線のため mention 判定は保留")
}

func TestService_HasUnreadSpecifiedNotes_UsesRepoWhenSet(t *testing.T) {
	// repo 注入時は Redis scan でなく repo.HasAnySpecified を参照する。
	svc := NewService(testRedis.Client, idGen, "")
	unread := testutil.NewMockNoteUnreadRepository()
	svc.SetNoteUnreadRepo(unread)

	// 空 repo: false
	got, err := svc.HasUnreadSpecifiedNotes(context.Background(), "alice")
	require.NoError(t, err)
	assert.False(t, got)

	// repo に行を追加 → true
	_ = unread.Upsert(&model.NoteUnread{
		ID: "nu1", UserID: "alice", NoteID: "n1", NoteUserID: "bob", IsSpecified: true,
	})
	got, err = svc.HasUnreadSpecifiedNotes(context.Background(), "alice")
	require.NoError(t, err)
	assert.True(t, got)
}

func TestHook_OnFollowed(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	h.OnFollowed("bob", "alice")

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeFollow, out[0].Type)
}

func TestHook_OnFollowRequested(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	h.OnFollowRequested("bob", "alice")

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeReceiveFollowReq, out[0].Type)
}

// #739: OnFollowRejected は受信側 (followee) の receiveFollowRequest 通知を
// follower の通知者 ID で絞って削除する。本家 TS と同じ semantics で、
// rejected を follower 側に積まない (notification spam 抑制)。
func TestHook_OnFollowRejected(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	// 既存の receive-follow-request 通知を alice (followee) 側に作っておく
	h.OnFollowRequested("bob", "alice")
	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, TypeReceiveFollowReq, out[0].Type)

	// reject されたら follower=bob 由来の receive-follow-request 通知を削除
	h.OnFollowRejected("bob", "alice")
	out, err = svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out, "receiveFollowRequest from rejected follower should be removed")
}

// nil hook は no-op。OnFollowRejected の guard 分岐を踏む。
func TestHook_OnFollowRejected_NilSvcNoop(t *testing.T) {
	h := &Hook{}
	// パニックしないことを確認
	h.OnFollowRejected("bob", "alice")
}

func TestHook_OnFollowAccepted(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "bob", "bob")
	h.OnFollowAccepted("bob", "alice")

	out, err := svc.List(context.Background(), "bob", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeFollowRequestAccept, out[0].Type)
}

func TestHook_OnPollVote(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	h.OnPollVote("alice", "bob", "n1", 2)

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypePollVote, out[0].Type)
	require.NotNil(t, out[0].Choice)
	assert.Equal(t, 2, *out[0].Choice)
}

func TestHook_OnReactionCreated(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	h.OnReactionCreated("alice", "bob", "n1", "👍")

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeReaction, out[0].Type)
	assert.Equal(t, "👍", out[0].Reaction)
}

// #1775: notificationRecieveConfig による種別ごとの受信ゲート。
func TestPassesReceiveConfig(t *testing.T) {
	mkProfile := func(userID, cfgJSON string) *model.UserProfile {
		return &model.UserProfile{UserID: userID, NotificationRecieveConfig: datatypes.JSON(cfgJSON)}
	}

	t.Run("never blocks the configured type", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"never"}}`)
		assert.False(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
		// 設定されていない type は許可。
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeMention, NotifierID: "bob"}))
	})

	t.Run("empty / missing config is allowed", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{}`)
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
		// profile 自体が無い場合も許可 (best-effort)。
		assert.True(t, h.passesReceiveConfig("ghost", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
	})

	t.Run("following gate", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"following"}}`)
		fr := testutil.NewMockFollowingRepository()
		h.SetNoteNotifyRepos(fr, nil)
		assert.False(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
		require.NoError(t, fr.Create(&model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "bob"}))
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
	})

	t.Run("mutualFollow requires both directions", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"mutualFollow"}}`)
		fr := testutil.NewMockFollowingRepository()
		require.NoError(t, fr.Create(&model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "bob"}))
		h.SetNoteNotifyRepos(fr, nil)
		assert.False(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
		require.NoError(t, fr.Create(&model.Following{ID: "f2", FollowerID: "bob", FolloweeID: "alice"}))
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
	})

	t.Run("followingOrFollower gate", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"followingOrFollower"}}`)
		fr := testutil.NewMockFollowingRepository()
		h.SetNoteNotifyRepos(fr, nil)
		assert.False(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
		// follower 方向だけでも許可。
		require.NoError(t, fr.Create(&model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice"}))
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
	})

	t.Run("list gate: member allowed, non-member blocked", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"list","userListId":"L1"}}`)
		ulr := testutil.NewMockUserListRepository()
		ulr.Lists["L1"] = &model.UserList{ID: "L1", UserID: "alice", Name: "friends"}
		require.NoError(t, ulr.AddMember(&model.UserListMembership{ID: "m1", UserListID: "L1", UserID: "bob"}))
		h.SetUserListRepo(ulr)
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
		assert.False(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "carol"}))
	})

	// 他人の list を userListId に指定されていても、そのメンバーを通さない。
	// 通すと通知の届き方で他人の list のメンバー構成を確かめられる。
	t.Run("list owned by another user never passes", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"list","userListId":"L2"}}`)
		ulr := testutil.NewMockUserListRepository()
		ulr.Lists["L2"] = &model.UserList{ID: "L2", UserID: "dave", Name: "dave's list"}
		require.NoError(t, ulr.AddMember(&model.UserListMembership{ID: "m1", UserListID: "L2", UserID: "bob"}))
		h.SetUserListRepo(ulr)
		assert.False(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
	})

	// 存在しない list は誰も通さない。lookup の障害は他の関係性 gate と同じく許可側。
	t.Run("missing list denies, lookup error permits", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"list","userListId":"gone"}}`)
		h.SetUserListRepo(testutil.NewMockUserListRepository())
		assert.False(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))

		h.SetUserListRepo(failingListLookupRepo{MockUserListRepository: testutil.NewMockUserListRepository()})
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
	})

	// dep 未配線時は gate を素通り (best-effort)。
	t.Run("missing deps are permissive", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"following"}}`)
		// followingRepo 未配線。
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
		repo.Profiles["alice"] = mkProfile("alice", `{"reaction":{"type":"list","userListId":"L1"}}`)
		// userListRepo 未配線。
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
		// NotifierID 空のときは関係性 gate を評価せず許可。
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: ""}))
	})

	// 不正 JSON は許可側に倒す。
	t.Run("invalid config json is permissive", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		repo.Profiles["alice"] = mkProfile("alice", `not json`)
		assert.True(t, h.passesReceiveConfig("alice", CreateInput{Type: TypeReaction, NotifierID: "bob"}))
	})
}

// #1775: 'never' 設定の type は notifyLocalUser 経由でも実際に配信が抑制される。
func TestHook_OnReactionCreated_NeverConfigSuppresses(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	repo.Profiles["alice"] = &model.UserProfile{UserID: "alice", NotificationRecieveConfig: datatypes.JSON(`{"reaction":{"type":"never"}}`)}
	h.OnReactionCreated("alice", "bob", "n1", "👍")

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out, "reaction=never の notifiee には reaction 通知を配信しない")
}

func TestHook_NotifyRemoteSkipped(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addRemoteUser(repo, "alice", "alice", "remote.example")
	h.OnFollowed("bob", "alice")
	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestHook_NotifyMissingUserSkipped(t *testing.T) {
	h, svc, _ := newTestHook(t)
	h.OnFollowed("bob", "ghost")
	out, _ := svc.List(context.Background(), "ghost", "", "", 10, nil, nil)
	assert.Empty(t, out)
}

func TestHook_NotifyWithoutUserRepo(t *testing.T) {
	// userRepo == nil の場合はホストチェックをスキップする
	testRedis.FlushAll(context.Background())
	svc := NewService(testRedis.Client, idGen, "")
	h := NewHook(svc, nil)
	h.OnFollowed("bob", "alice")

	out, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
}

func TestHook_NotifyServiceErrorLogged(t *testing.T) {
	// closed clientではCreate失敗 → ログ出力されるだけで例外なし
	svc := NewService(closedClient(t), idGen, "")
	repo := testutil.NewMockUserRepository()
	addLocalUser(repo, "alice", "alice")
	h := NewHook(svc, repo)
	h.OnFollowed("bob", "alice")
}

// stubMuteChecker for hook tests.
type stubMuteChecker struct {
	muted bool
	err   error
}

func (s *stubMuteChecker) IsMuted(_, _ string) (bool, error) {
	return s.muted, s.err
}

func TestHook_NotifyMutedNotifierSkipped(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	h.SetMuteChecker(&stubMuteChecker{muted: true})

	h.OnFollowed("bob", "alice")
	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	assert.Empty(t, out)
}

func TestHook_NotifyMuteCheckerErrorAllowed(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	h.SetMuteChecker(&stubMuteChecker{err: errSentinel})

	h.OnFollowed("bob", "alice")
	out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	// muteチェッカーがエラーを返した場合は通知を許可する
	assert.Len(t, out, 1)
}

var errSentinel = newSentinelError("mute checker error")

type sentinelError struct{ msg string }

func (e *sentinelError) Error() string { return e.msg }

func newSentinelError(s string) error { return &sentinelError{msg: s} }

func TestIsQuote_Variants(t *testing.T) {
	target := "x"
	cases := []struct {
		name string
		n    *model.Note
		want bool
	}{
		{"no renote", &model.Note{}, false},
		{"pure renote", &model.Note{RenoteID: &target}, false},
		{"with text", &model.Note{RenoteID: &target, Text: ptrString("hi")}, true},
		{"with cw", &model.Note{RenoteID: &target, CW: ptrString("warn")}, true},
		{"with file", &model.Note{RenoteID: &target, FileIDs: model.StringArray{"f1"}}, true},
		{"with poll", &model.Note{RenoteID: &target, HasPoll: true}, true},
		{"with reply", &model.Note{RenoteID: &target, ReplyID: ptrString("r1")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isQuote(tc.n))
		})
	}
}

func ptrString(s string) *string { return &s }

// --- Web Push publisher integration ---

type stubWebPushPublisher struct {
	calls []struct {
		userID string
		body   map[string]any
	}
}

func (s *stubWebPushPublisher) PushNotification(userID string, body map[string]any) {
	s.calls = append(s.calls, struct {
		userID string
		body   map[string]any
	}{userID, body})
}

type stubUserPacker struct{ out map[string]any }

func (s stubUserPacker) PackUserByID(_ string) (map[string]any, bool) {
	if s.out == nil {
		return nil, false
	}
	return s.out, true
}

type stubNotePacker struct {
	out          map[string]any
	gotViewerIDs *[]string
}

func (s stubNotePacker) PackNoteByID(_, viewerID string) (map[string]any, bool) {
	if s.gotViewerIDs != nil {
		*s.gotViewerIDs = append(*s.gotViewerIDs, viewerID)
	}
	if s.out == nil {
		return nil, false
	}
	return s.out, true
}

func TestHook_WebPushPushedAfterCreate(t *testing.T) {
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")

	pub := &stubWebPushPublisher{}
	h.SetWebPushPublisher(pub)
	h.SetPackers(
		stubUserPacker{out: map[string]any{"id": "bob", "username": "bob"}},
		stubNotePacker{out: map[string]any{"id": "n1", "text": "hi"}},
	)

	h.OnReactionCreated("alice", "bob", "n1", "\U0001F44D")
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "alice", pub.calls[0].userID)
	body := pub.calls[0].body
	assert.Equal(t, "reaction", body["type"])
	assert.Equal(t, "bob", body["userId"])
	assert.Equal(t, "n1", body["noteId"])
	assert.Equal(t, "\U0001F44D", body["reaction"])
	assert.NotNil(t, body["user"])
	assert.NotNil(t, body["note"])
	assert.NotNil(t, body["createdAt"])
}

// TestHook_WebPushNoteGatedByRecipient verifies buildPushBody threads the
// RECIPIENT (notifiee) id into the note packer so the embedded note is gated by
// the push recipient's visibility (#1572), not the notifier's.
func TestHook_WebPushNoteGatedByRecipient(t *testing.T) {
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")

	pub := &stubWebPushPublisher{}
	h.SetWebPushPublisher(pub)
	var viewerIDs []string
	h.SetPackers(
		stubUserPacker{out: map[string]any{"id": "bob"}},
		stubNotePacker{out: map[string]any{"id": "n1"}, gotViewerIDs: &viewerIDs},
	)

	h.OnReactionCreated("alice", "bob", "n1", "\U0001F44D") // alice = recipient, bob = reactor
	require.Len(t, pub.calls, 1)
	require.Equal(t, []string{"alice"}, viewerIDs, "note packer must be gated by the recipient (notifiee), not the notifier")
}

func TestHook_WebPushWithChoiceField(t *testing.T) {
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")

	pub := &stubWebPushPublisher{}
	h.SetWebPushPublisher(pub)
	h.OnPollVote("alice", "bob", "n1", 3)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, 3, pub.calls[0].body["choice"])
}

func TestHook_WebPushWithoutPackers(t *testing.T) {
	h, _, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	pub := &stubWebPushPublisher{}
	h.SetWebPushPublisher(pub)
	// packers not set -> user/note fields omitted but id/type/userId still present
	h.OnFollowed("bob", "alice")
	require.Len(t, pub.calls, 1)
	body := pub.calls[0].body
	assert.Equal(t, "follow", body["type"])
	assert.Nil(t, body["user"])
	assert.Nil(t, body["note"])
}

func TestHook_WebPushSkippedWhenCreateFails(t *testing.T) {
	svc := NewService(closedClient(t), idGen, "")
	repo := testutil.NewMockUserRepository()
	addLocalUser(repo, "alice", "alice")
	h := NewHook(svc, repo)
	pub := &stubWebPushPublisher{}
	h.SetWebPushPublisher(pub)
	h.OnFollowed("bob", "alice")
	assert.Empty(t, pub.calls, "push must not fire when Create fails")
}

// upstream Misskey #17335 (= 2026.5.0 fix / triage #1006) + #17363 (= 2026.5.1
// fix / triage #1010): specified 可視性 note は visibleUserIds に含まれない
// target に通知を送らない (情報漏洩防止)。public / home / followers は filter
// なし (followers は upstream #17363 で null = no filter に揃った)。
func TestHook_OnNoteCreated_VisibilityFiltersMentions(t *testing.T) {
	tests := []struct {
		name        string
		visibility  model.NoteVisibility
		visible     []string
		mentioned   []string
		wantNotifyA bool // alice
		wantNotifyC bool // charlie
	}{
		{
			name:        "public_notifies_all",
			visibility:  model.NoteVisibilityPublic,
			visible:     nil,
			mentioned:   []string{"alice", "charlie"},
			wantNotifyA: true,
			wantNotifyC: true,
		},
		{
			name:        "home_notifies_all",
			visibility:  model.NoteVisibilityHome,
			visible:     nil,
			mentioned:   []string{"alice", "charlie"},
			wantNotifyA: true,
			wantNotifyC: true,
		},
		{
			name:        "followers_notifies_all_per_17363",
			visibility:  model.NoteVisibilityFollowers,
			visible:     nil,
			mentioned:   []string{"alice", "charlie"},
			wantNotifyA: true,
			wantNotifyC: true,
		},
		{
			name:        "specified_only_visible_targets",
			visibility:  model.NoteVisibilitySpecified,
			visible:     []string{"alice"}, // charlie は visibleUserIds 外
			mentioned:   []string{"alice", "charlie"},
			wantNotifyA: true,
			wantNotifyC: false,
		},
		{
			name:        "specified_empty_visible_drops_all",
			visibility:  model.NoteVisibilitySpecified,
			visible:     nil,
			mentioned:   []string{"alice", "charlie"},
			wantNotifyA: false,
			wantNotifyC: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, svc, repo := newTestHook(t)
			addLocalUser(repo, "alice", "alice")
			addLocalUser(repo, "charlie", "charlie")
			addLocalUser(repo, "bob", "bob")

			note := &model.Note{
				ID:             "n_" + tc.name,
				UserID:         "bob",
				Visibility:     tc.visibility,
				VisibleUserIDs: model.StringArray(tc.visible),
				Mentions:       model.StringArray(tc.mentioned),
			}
			h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, nil)

			outA, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
			outC, _ := svc.List(context.Background(), "charlie", "", "", 10, nil, nil)
			if tc.wantNotifyA {
				require.Len(t, outA, 1, "alice should receive mention notification")
				assert.Equal(t, TypeMention, outA[0].Type)
			} else {
				assert.Empty(t, outA, "alice should NOT receive notification")
			}
			if tc.wantNotifyC {
				require.Len(t, outC, 1, "charlie should receive mention notification")
				assert.Equal(t, TypeMention, outC[0].Type)
			} else {
				assert.Empty(t, outC, "charlie should NOT receive notification")
			}
		})
	}
}

// reply / renote 通知も visibility filter の対象になる (= upstream
// NotificationManager の queue 全体に filter がかかるため)。specified note で
// reply target が visibleUserIds 外なら reply 通知も発火しないことを seal。
func TestHook_OnNoteCreated_VisibilityFiltersReplyAndRenote(t *testing.T) {
	t.Run("specified_reply_target_outside_visible_skipped", func(t *testing.T) {
		h, svc, repo := newTestHook(t)
		addLocalUser(repo, "alice", "alice")
		addLocalUser(repo, "bob", "bob")
		// 親 note は alice の note。reply は specified visibility だが
		// visibleUserIds に alice を含まない (= 異常 / 攻撃シナリオ)。
		parent := &model.Note{ID: "n_parent", UserID: "alice"}
		note := &model.Note{
			ID:             "n_reply",
			UserID:         "bob",
			Visibility:     model.NoteVisibilitySpecified,
			VisibleUserIDs: model.StringArray{"someone_else"},
		}
		h.OnNoteCreated(note, &model.User{ID: "bob"}, parent, nil)
		out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
		assert.Empty(t, out, "reply target が visibleUserIds 外なら通知しない")
	})

	t.Run("specified_renote_target_outside_visible_skipped", func(t *testing.T) {
		h, svc, repo := newTestHook(t)
		addLocalUser(repo, "alice", "alice")
		addLocalUser(repo, "bob", "bob")
		target := "n_target"
		original := &model.Note{ID: target, UserID: "alice"}
		note := &model.Note{
			ID:             "n_renote",
			UserID:         "bob",
			RenoteID:       &target,
			Visibility:     model.NoteVisibilitySpecified,
			VisibleUserIDs: model.StringArray{"someone_else"},
		}
		h.OnNoteCreated(note, &model.User{ID: "bob"}, nil, original)
		out, _ := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
		assert.Empty(t, out, "renote target が visibleUserIds 外なら通知しない")
	})
}

// notifyVisibleToTarget の各 visibility case を直接 unit test。
func TestHook_NotifyVisibleToTarget(t *testing.T) {
	h, _, _ := newTestHook(t)
	tests := []struct {
		name       string
		visibility model.NoteVisibility
		visible    []string
		target     string
		want       bool
	}{
		{"empty_visibility_allows_all", "", nil, "alice", true},
		{"public_allows_all", model.NoteVisibilityPublic, nil, "alice", true},
		{"home_allows_all", model.NoteVisibilityHome, nil, "alice", true},
		{"followers_allows_all_per_17363", model.NoteVisibilityFollowers, nil, "alice", true},
		{"specified_allows_if_in_list", model.NoteVisibilitySpecified, []string{"alice", "bob"}, "alice", true},
		{"specified_blocks_if_not_in_list", model.NoteVisibilitySpecified, []string{"bob"}, "alice", false},
		{"specified_empty_list_blocks_all", model.NoteVisibilitySpecified, nil, "alice", false},
		{"unknown_visibility_blocks", "weird", nil, "alice", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &model.Note{Visibility: tc.visibility, VisibleUserIDs: model.StringArray(tc.visible)}
			assert.Equal(t, tc.want, h.notifyVisibleToTarget(n, tc.target))
		})
	}
}

// #2106 L35 / #2224: 作成から unreadPublishDelay 以内に MarkAllAsRead されると、
// unreadNotification だけでなく Web Push も抑制される。
func TestHook_WebPushSuppressedAfterRead(t *testing.T) {
	h, svc, repo := newTestHook(t)
	svc.SetUnreadPublishDelay(50 * time.Millisecond) // deferred 化して read レースを作る
	addLocalUser(repo, "alice", "alice")

	pub := &stubWebPushPublisher{}
	h.SetWebPushPublisher(pub)
	h.SetPackers(
		stubUserPacker{out: map[string]any{"id": "bob"}},
		stubNotePacker{out: map[string]any{"id": "n1"}},
	)

	h.OnReactionCreated("alice", "bob", "n1", "\U0001F44D") // deferred push をスケジュール
	// delay 前に既読化 (latestRead marker を最新へ前進)。
	require.NoError(t, svc.MarkAllAsRead(context.Background(), "alice", false))
	// delay 経過を待つ。
	time.Sleep(120 * time.Millisecond)
	assert.Empty(t, pub.calls, "delay 内既読化で Web Push が抑制される")
}

type failingListLookupRepo struct {
	*testutil.MockUserListRepository
}

func (failingListLookupRepo) FindByID(string) (*model.UserList, error) {
	return nil, errors.New("db down")
}
