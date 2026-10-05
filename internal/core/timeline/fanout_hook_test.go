package timeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHook(t *testing.T) (*FanoutHook, *FanoutTimelineService, *testutil.MockFollowingRepository) {
	t.Helper()
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	following := testutil.NewMockFollowingRepository()
	return NewFanoutHook(fanout, following), fanout, following
}

func TestFanoutHook_Nil(t *testing.T) {
	h, _, _ := newTestHook(t)
	// nilノートはno-op
	h.OnNoteCreated(nil, &model.User{ID: "u"})
	h.OnNoteCreated(&model.Note{ID: "n"}, nil)
}

func TestFanoutHook_PublicLocal(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "author"} // Hostがnil = local
	h.OnNoteCreated(n, author)

	// home/user/local/global の4タイムラインに入る
	for _, name := range []Name{
		HomeTimelineName("author"),
		UserTimelineName("author"),
		LocalTimeline,
		GlobalTimeline,
	} {
		out, err := fanout.Get(ctx, name, "", "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, out, "timeline %q should contain note", name)
	}
}

func TestFanoutHook_RemoteAuthorSkipsLocal(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	host := "remote.example"
	n := &model.Note{ID: noteID, UserID: "ra", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "ra", Host: &host}
	h.OnNoteCreated(n, author)

	out, err := fanout.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)

	// グローバルには入る
	out, err = fanout.Get(ctx, GlobalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)
}

// リモートユーザーの HTL は作らない。upstream NoteCreateService の
// 「自分自身のHTL」ブロックは note.userHost == null を条件にしている。
// こちらのインスタンスがリモートユーザーのタイムラインを持つ意味が無く、
// Redis を無駄に太らせるだけ。
func TestFanoutHook_RemoteAuthorHasNoHomeTimeline(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	host := "remote.example"
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "ra", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "ra", Host: &host}
	h.OnNoteCreated(n, author)

	out, err := fanout.Get(ctx, HomeTimelineName("ra"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "リモートユーザーの HTL は作らないこと")
}

// ローカルユーザーなら従来どおり自分の HTL に入る。
func TestFanoutHook_LocalAuthorHasHomeTimeline(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "la", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "la"})

	out, err := fanout.Get(ctx, HomeTimelineName("la"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)
}

// visibility=specified で自分が宛先に入っているときは自分の HTL に積まない
// (宛先としての配送で届くため二重になる)。upstream も同条件。
func TestFanoutHook_SpecifiedWithSelfSkipsOwnHome(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "la",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"la", "other"},
	}
	h.OnNoteCreated(n, &model.User{ID: "la"})

	out, err := fanout.Get(ctx, HomeTimelineName("la"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// 宛先に自分が入っていない specified なら自分の HTL に積む。
func TestFanoutHook_SpecifiedWithoutSelfKeepsOwnHome(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	n := &model.Note{
		ID: noteID, UserID: "la",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"other"},
	}
	h.OnNoteCreated(n, &model.User{ID: "la"})

	out, err := fanout.Get(ctx, HomeTimelineName("la"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)
}

// upstream は LTL を返信の有無で 3 本に分ける。素の localTimeline に返信を
// 入れないことで「他人の他人への返信は LTL に出ない」を成立させ、自分宛ての
// 返信は専用キーから合流させる。
func TestFanoutHook_ReplyGoesToSeparateLocalKeys(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	srcID := idGen.Generate(time.Now())
	replyUser := "target"
	noteID := idGen.Generate(time.Now())
	n := &model.Note{
		ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic,
		ReplyID: &srcID, ReplyUserID: &replyUser,
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	out, err := fanout.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "素の localTimeline に返信を入れないこと")

	out, err = fanout.Get(ctx, LocalTimelineWithReplies, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)

	out, err = fanout.Get(ctx, LocalTimelineWithReplyToName(replyUser), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "宛先別キーに積むこと")
}

// 自己スレッド (自分への返信) は返信扱いしない。upstream isReply も
// replyUserId !== userId を条件にしている。
func TestFanoutHook_SelfThreadStaysInLocalTimeline(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	srcID := idGen.Generate(time.Now())
	self := "author"
	noteID := idGen.Generate(time.Now())
	n := &model.Note{
		ID: noteID, UserID: self, Visibility: model.NoteVisibilityPublic,
		ReplyID: &srcID, ReplyUserID: &self,
	}
	h.OnNoteCreated(n, &model.User{ID: self})

	out, err := fanout.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)
}

// リモート宛ての返信は宛先別キーを作らない (引く人が居ない)。
func TestFanoutHook_ReplyToRemoteHasNoReplyToKey(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	srcID := idGen.Generate(time.Now())
	replyUser, remoteHost := "remoteUser", "remote.example"
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "author", Visibility: model.NoteVisibilityPublic,
		ReplyID: &srcID, ReplyUserID: &replyUser, ReplyUserHost: &remoteHost,
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	out, err := fanout.Get(ctx, LocalTimelineWithReplyToName(replyUser), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestFanoutHook_FollowersVisibilityNoGlobal(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers}
	author := &model.User{ID: "author"}
	h.OnNoteCreated(n, author)

	out, err := fanout.Get(ctx, GlobalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)

	out, err = fanout.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestFanoutHook_SpecifiedDoesNotFanoutToFollowers(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower", FolloweeID: "author"}

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilitySpecified}
	author := &model.User{ID: "author"}
	h.OnNoteCreated(n, author)

	out, err := fanout.Get(ctx, HomeTimelineName("follower"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestFanoutHook_FanoutsToFollowers(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author"}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "follower2", FolloweeID: "author"}

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	for _, fid := range []string{"follower1", "follower2"} {
		out, err := fanout.Get(ctx, HomeTimelineName(fid), "", "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, out, "follower %s home should receive", fid)
	}
}

// #1686: channel note は author の (user) follower ではなく channel の follower
// の home へ fanout する。author home / LTL / GTL / user-follower home には乗せ
// ない (upstream pushToTl の channelId 分岐)。
func TestFanoutHook_ChannelNoteToChannelFollowers(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	// author は user-follower "uf" を持つが、channel note では uf の home に push
	// されないことを確認する。
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "uf", FolloweeID: "author"}
	chMock := testutil.NewMockChannelFollowingRepository()
	chMock.Followings["cf1"] = &model.ChannelFollowing{ID: "cf1", FolloweeID: "ch1", FollowerID: "cf"}
	h.SetChannelFollowerRepo(chMock)

	noteID := idGen.Generate(time.Now())
	ch := "ch1"
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic, ChannelID: &ch}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// channel follower "cf" の home に入る
	out, err := fanout.Get(ctx, HomeTimelineName("cf"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "channel follower home should receive channel note")

	// author home / user-follower home / LTL / GTL には入らない
	for _, name := range []Name{
		HomeTimelineName("author"),
		HomeTimelineName("uf"),
		LocalTimeline,
		GlobalTimeline,
	} {
		out, err := fanout.Get(ctx, name, "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, out, "channel note should NOT be in %q", name)
	}

	// user timeline (step 1) には引き続き入る (users/notes 互換、scope 外)。
	out, err = fanout.Get(ctx, UserTimelineName("author"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "author user timeline still receives channel note")
}

// #1686: channel note の OnNoteDeleted は channel follower の home からのみ掃除
// する (push と対称)。
func TestFanoutHook_OnNoteDeleted_ChannelNote(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()
	chMock := testutil.NewMockChannelFollowingRepository()
	chMock.Followings["cf1"] = &model.ChannelFollowing{ID: "cf1", FolloweeID: "ch1", FollowerID: "cf"}
	h.SetChannelFollowerRepo(chMock)

	noteID := idGen.Generate(time.Now())
	ch := "ch1"
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic, ChannelID: &ch}
	h.OnNoteCreated(n, &model.User{ID: "author"})
	// push 済を確認
	out, err := fanout.Get(ctx, HomeTimelineName("cf"), "", "", 10)
	require.NoError(t, err)
	require.Equal(t, []string{noteID}, out)

	h.OnNoteDeleted(n, &model.User{ID: "author"})
	out, err = fanout.Get(ctx, HomeTimelineName("cf"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "channel follower home should be cleaned after delete")
}

// 他人宛 reply は WithReplies=true の follower にだけ push される (#1047)。
// upstream Misskey TS の `following.withReplies` setting と互換にするため、
// fanout 段階で per-follower flag を見て push を制御する。
func TestFanoutHook_ReplyToOtherSkipsFollowersWithoutWithReplies(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	// follower1 は withReplies=false (default), follower2 は true
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author", WithReplies: false}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "follower2", FolloweeID: "author", WithReplies: true}

	noteID := idGen.Generate(time.Now())
	replyID := "reply-target"
	otherUser := "other-user"
	n := &model.Note{
		ID:          noteID,
		UserID:      "author",
		Visibility:  model.NoteVisibilityPublic,
		ReplyID:     &replyID,
		ReplyUserID: &otherUser, // 他人宛 reply
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// follower1 (withReplies=false) は受け取らない
	out, err := fanout.Get(ctx, HomeTimelineName("follower1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "follower1 should not receive reply when WithReplies=false")

	// follower2 (withReplies=true) は受け取る
	out, err = fanout.Get(ctx, HomeTimelineName("follower2"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "follower2 should receive reply when WithReplies=true")

	// author 本人の home には常に push される
	out, err = fanout.Get(ctx, HomeTimelineName("author"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "author's own home should always receive own reply")
}

// self-thread (replyUserId == userId) は WithReplies 設定に関わらず全 follower
// に push される (#1047)。upstream の TL filter で `replyUserId = note.userId`
// 経路で残るので fanout でも全 push する semantics。
func TestFanoutHook_SelfThreadReplyFanoutsToAllFollowers(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author", WithReplies: false}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "follower2", FolloweeID: "author", WithReplies: true}

	noteID := idGen.Generate(time.Now())
	replyID := "reply-target"
	selfID := "author"
	n := &model.Note{
		ID:          noteID,
		UserID:      "author",
		Visibility:  model.NoteVisibilityPublic,
		ReplyID:     &replyID,
		ReplyUserID: &selfID, // self-thread
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// withReplies=false でも self-thread は受け取る
	for _, fid := range []string{"follower1", "follower2"} {
		out, err := fanout.Get(ctx, HomeTimelineName(fid), "", "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, out, "follower %s should receive self-thread reply regardless of WithReplies", fid)
	}
}

// TestFanoutHook_ReplyToFollowerIsPushedRegardlessOfWithReplies guards
// #1150: 「他人が follower 本人宛にした reply」は WithReplies 設定に関わら
// ず follower 本人の home TL に push される。stream filter `replyShouldEmit`
// が `replyToMe` escape hatch を持つので fanout 側も symmetric に揃える
// (= reload で消える非対称挙動を解消)。
//
// scenario: author が follower1 / follower2 を持ち、follower1 が
// withReplies=false。author が follower1 宛 reply を作ると、follower1 の
// home TL に push される (= self への reply は default で受け取りたい)。
// follower2 (= 無関係) も withReplies=true なので受け取る。
func TestFanoutHook_ReplyToFollowerIsPushedRegardlessOfWithReplies(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author", WithReplies: false}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "follower2", FolloweeID: "author", WithReplies: false}

	noteID := idGen.Generate(time.Now())
	replyID := "reply-target"
	follower1ID := "follower1"
	n := &model.Note{
		ID:          noteID,
		UserID:      "author",
		Visibility:  model.NoteVisibilityPublic,
		ReplyID:     &replyID,
		ReplyUserID: &follower1ID, // follower1 宛 reply
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// follower1 は自分宛 reply なので受け取る (replyToMe escape hatch)
	out, err := fanout.Get(ctx, HomeTimelineName("follower1"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "follower1 (= reply target) should receive reply regardless of WithReplies")

	// follower2 は無関係な reply なので withReplies=false の通常 gate で skip
	out, err = fanout.Get(ctx, HomeTimelineName("follower2"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "follower2 (= unrelated) should not receive reply when WithReplies=false")
}

// TestFanoutHook_MentionedFollowerEscapesReplyFilter guards #1195: 他人 A
// → 他人 B reply で本文に follower C への mention が含まれているとき、
// C は withReplies=false でも home TL に push される (= mk-go 独自 escape
// hatch、upstream Misskey TS は持たない意図的 deviation)。
//
// scenario: author が follower1 / follower2 を持ち、follower1 / follower2
// 共に withReplies=false。author が `replyTargetUser` 宛 reply を作り、
// note.Mentions に follower1 を含める。
//   - follower1 は mentioned → push (mention escape hatch)
//   - follower2 は mentioned でも replyToMe でも selfThread でもない →
//     既存 reply gate で drop
func TestFanoutHook_MentionedFollowerEscapesReplyFilter(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author", WithReplies: false}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "follower2", FolloweeID: "author", WithReplies: false}

	noteID := idGen.Generate(time.Now())
	replyID := "reply-target"
	otherUser := "other-user"
	n := &model.Note{
		ID:          noteID,
		UserID:      "author",
		Visibility:  model.NoteVisibilityPublic,
		ReplyID:     &replyID,
		ReplyUserID: &otherUser, // 他人宛 reply
		Mentions:    []string{"follower1"},
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// follower1 は mention されているので withReplies=false でも push される
	out, err := fanout.Get(ctx, HomeTimelineName("follower1"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "mentioned follower1 should receive reply regardless of WithReplies")

	// follower2 は mention されていないので default reply gate で drop
	out, err = fanout.Get(ctx, HomeTimelineName("follower2"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "non-mentioned follower2 should NOT receive reply when WithReplies=false")
}

// TestFanoutHook_FollowersOnlyReply_DropsFollowersNotFollowingReplyTarget
// guards #1152: reply 対象 note の `visibility=followers` の場合、reply
// target を follow していない follower は drop される (= stream filter
// `replyShouldEmit` と symmetric)。「context が見えない reply 本文だけが
// 流れる」 privacy 漏洩を防ぐ。
//
// scenario: author が follower1 / follower2 を持ち、author が `replyTarget`
// (visibility=followers) に reply を作る。
//   - follower1 は replyTarget の author (= reply.userId) を follow → push
//   - follower2 は replyTarget の author を follow していない → drop
//
// 両 follower 共に withReplies=true なので #1150 経路は skip されない。
func TestFanoutHook_FollowersOnlyReply_DropsFollowersNotFollowingReplyTarget(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	// follower1 / follower2 共に author を withReplies=true で follow。
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author", WithReplies: true}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "follower2", FolloweeID: "author", WithReplies: true}
	// follower1 → replyTargetUser (= 自分が reply 先を follow している)
	following.Followings["f3"] = &model.Following{ID: "f3", FollowerID: "follower1", FolloweeID: "replyTargetUser"}

	noteID := idGen.Generate(time.Now())
	replyTargetID := "reply-target-note"
	replyTargetUser := "replyTargetUser"
	n := &model.Note{
		ID:          noteID,
		UserID:      "author",
		Visibility:  model.NoteVisibilityPublic, // 本 note 自体の visibility は public
		ReplyID:     &replyTargetID,
		ReplyUserID: &replyTargetUser,
		// preload された reply 対象 note (= 自分は followers-only)
		Reply: &model.Note{
			ID:         replyTargetID,
			UserID:     replyTargetUser,
			Visibility: model.NoteVisibilityFollowers,
		},
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// follower1: replyTargetUser を follow しているので reply を受け取る
	out, err := fanout.Get(ctx, HomeTimelineName("follower1"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "follower1 (= replyTargetUser を follow 中) は reply 受け取る")

	// follower2: replyTargetUser を follow していないので drop
	out, err = fanout.Get(ctx, HomeTimelineName("follower2"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "follower2 (= replyTargetUser 非 follow) は followers-only reply を受け取らない")
}

// TestFanoutHook_FollowersOnlyReply_ReplyToFollowerEscapeHatch covers #1152
// + #1150 の組み合わせ: follower が reply target 本人なら、follow 関係
// (= 自己 follow は不要) をチェックせず push する。`isReplyToFollower`
// escape hatch が followers-only gate より優先される。
func TestFanoutHook_FollowersOnlyReply_ReplyToFollowerEscapeHatch(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author", WithReplies: true}
	// follower1 自身を replyTargetUser に設定 (= 自分宛 reply)
	follower1 := "follower1"

	noteID := idGen.Generate(time.Now())
	replyTargetID := "reply-target-note"
	n := &model.Note{
		ID:          noteID,
		UserID:      "author",
		Visibility:  model.NoteVisibilityPublic,
		ReplyID:     &replyTargetID,
		ReplyUserID: &follower1, // follower1 宛 reply
		Reply: &model.Note{
			ID:         replyTargetID,
			UserID:     follower1,
			Visibility: model.NoteVisibilityFollowers,
		},
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// follower1 は reply target 本人なので followers-only gate を escape して受け取る
	out, err := fanout.Get(ctx, HomeTimelineName("follower1"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "follower1 (= reply target 本人) は followers-only gate を escape")
}

// TestFanoutHook_MentionEscapeDoesNotBypassFollowersVisibilityGate guards
// #1195 の scope boundary: mention escape hatch は basic withReplies gate のみ
// を pass させ、followers-visibility privacy gate (= reply target を follow
// していない follower への reply 本文露出防止) は引き続き drop する。stream
// 側で同 scenario を `TestReplyShouldEmit_MentionEscapeDoesNotOverrideFollowersVisibility`
// で pin 済み、fanout 側もここで同 symmetry を pin して privacy boundary が
// 将来 escape に飲み込まれないようにする。
//
// scenario: A が `replyTargetUser` (visibility=followers) に reply、本文に
// follower1 を mention。follower1 は author を withReplies=true で follow し
// ているが replyTargetUser は follow していない。
//   - mention されていても followers-visibility gate は通らないので drop。
func TestFanoutHook_MentionEscapeDoesNotBypassFollowersVisibilityGate(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author", WithReplies: true}
	// follower1 は replyTargetUser を follow していない (= followers-only gate
	// が effective)。

	noteID := idGen.Generate(time.Now())
	replyTargetID := "reply-target-note"
	replyTargetUser := "replyTargetUser"
	n := &model.Note{
		ID:          noteID,
		UserID:      "author",
		Visibility:  model.NoteVisibilityPublic,
		ReplyID:     &replyTargetID,
		ReplyUserID: &replyTargetUser,
		Mentions:    []string{"follower1"}, // mention されていても escape は basic gate のみ
		Reply: &model.Note{
			ID:         replyTargetID,
			UserID:     replyTargetUser,
			Visibility: model.NoteVisibilityFollowers,
		},
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// follower1 は mention されているが replyTargetUser を follow していない
	// ので followers-visibility gate で drop される (privacy boundary 維持)。
	out, err := fanout.Get(ctx, HomeTimelineName("follower1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "mentioned follower should still be dropped by followers-visibility gate (#1195 scope boundary)")
}

// 通常 note (reply 無し) は WithReplies 設定に関わらず全 follower に push される。
func TestFanoutHook_NonReplyFanoutsToAllFollowers(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author", WithReplies: false}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "follower2", FolloweeID: "author", WithReplies: true}

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	for _, fid := range []string{"follower1", "follower2"} {
		out, err := fanout.Get(ctx, HomeTimelineName(fid), "", "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, out, "follower %s should receive non-reply note regardless of WithReplies", fid)
	}
}

// failingFollowingRepo errors out on ListFollowers to exercise the warning path.
type failingFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFollowingRepo) ListFollowers(_ string, _, _ int) ([]*model.Following, error) {
	return nil, assertError{}
}

type assertError struct{}

func (assertError) Error() string { return "boom" }

func TestFanoutHook_ListFollowersError(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	following := &failingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository()}
	h := NewFanoutHook(fanout, following)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	// エラーがあっても上には伝搬しない
	h.OnNoteCreated(n, &model.User{ID: "author"})
}

func TestFanoutHook_FanoutsAcrossPages(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	following := testutil.NewMockFollowingRepository()
	// 201人のフォロワーを用意してページ境界を踏ませる
	for i := range 201 {
		fid := "f-" + idGen.Generate(time.Now().Add(time.Duration(i)*time.Microsecond))
		following.Followings[fid] = &model.Following{
			ID:         fid,
			FollowerID: "follower-" + fid,
			FolloweeID: "author",
		}
	}
	h := NewFanoutHook(fanout, following)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})
	// 配信成功 (詳細な検証はsmoke扱い)
}

func TestFanoutHook_PushErrorIsLogged(t *testing.T) {
	// closed clientへのpushでエラーを発生させる. ログ出力されるだけで例外なし.
	following := testutil.NewMockFollowingRepository()
	fanout := NewFanoutTimelineService(closedClient(t), idGen, "")
	h := NewFanoutHook(fanout, following)
	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "u", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "u"})
}

func TestFanoutHook_NilFollowingRepo(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	h := NewFanoutHook(fanout, nil)
	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "u", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "u"})
}

// stubStreamingPublisher records every PublishNote call.
type stubStreamingPublisher struct {
	topics []string
}

func (s *stubStreamingPublisher) PublishNote(topic string, _ *model.Note, _ *model.User) {
	s.topics = append(s.topics, topic)
}

func TestFanoutHook_PublishesStreamingTopics(t *testing.T) {
	h, _, following := newTestHook(t)
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author"}
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	assert.Contains(t, pub.topics, "homeTimeline:author")
	assert.Contains(t, pub.topics, "homeTimeline:follower1")
	assert.Contains(t, pub.topics, "localTimeline")
	assert.Contains(t, pub.topics, "globalTimeline")
}

func TestFanoutHook_PublishesChannelTopic(t *testing.T) {
	h, _, _ := newTestHook(t)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	chID := "ch1"
	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic, ChannelID: &chID}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	assert.Contains(t, pub.topics, "channel:ch1")
}

func TestFanoutHook_NoChannelTopicWithoutChannel(t *testing.T) {
	h, _, _ := newTestHook(t)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	for _, tp := range pub.topics {
		assert.NotContains(t, tp, "channel:", "no channel topic expected for a channel-less note")
	}
}

type stubUserRoles struct{ roles []*model.Role }

func (s stubUserRoles) GetUserRoles(_ string) ([]*model.Role, error) { return s.roles, nil }

func TestFanoutHook_PublishesRoleTimelines(t *testing.T) {
	h, _, _ := newTestHook(t)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)
	h.SetUserRolesLookup(stubUserRoles{roles: []*model.Role{{ID: "r1"}, {ID: "r2"}}})

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	assert.Contains(t, pub.topics, "roleTimeline:r1")
	assert.Contains(t, pub.topics, "roleTimeline:r2")
}

func TestFanoutHook_NoRoleTimelineForNonPublic(t *testing.T) {
	h, _, _ := newTestHook(t)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)
	h.SetUserRolesLookup(stubUserRoles{roles: []*model.Role{{ID: "r1"}}})

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	for _, tp := range pub.topics {
		assert.NotContains(t, tp, "roleTimeline:", "non-public note must not hit role timelines")
	}
}

func TestFanoutHook_PublishesHashtagTopics(t *testing.T) {
	h, _, _ := newTestHook(t)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic, Tags: []string{"GoLang", "ＡＢＣ", "golang"}}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// normalized (NFKC+lower) + deduped: golang (x2 -> 1), abc.
	assert.Contains(t, pub.topics, "hashtag:golang")
	assert.Contains(t, pub.topics, "hashtag:abc")
	count := 0
	for _, tp := range pub.topics {
		if tp == "hashtag:golang" {
			count++
		}
	}
	assert.Equal(t, 1, count, "duplicate normalized tag must publish once")
}

func TestFanoutHook_StreamingHomeOnlyForFollowersVisibility(t *testing.T) {
	h, _, _ := newTestHook(t)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// followers visibility は localTimeline / globalTimeline 配信なし
	assert.NotContains(t, pub.topics, "localTimeline")
	assert.NotContains(t, pub.topics, "globalTimeline")
	assert.Contains(t, pub.topics, "homeTimeline:author")
}

func TestFanoutHook_HomeVisibilityNotInLocalTimeline(t *testing.T) {
	// home visibility はフォロワー向けなので、LTL / GTL の fanout キャッシュにも
	// streaming トピックにも流してはいけない (本家 Misskey 仕様)。
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityHome}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// LTL / GTL の fanout キャッシュには入らない
	for _, name := range []Name{LocalTimeline, GlobalTimeline} {
		out, err := fanout.Get(ctx, name, "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, out, "home visibility note must not enter %q cache", name)
	}

	// streaming トピックにも配信されない
	assert.NotContains(t, pub.topics, "localTimeline")
	assert.NotContains(t, pub.topics, "globalTimeline")
	// homeTimeline には入る (本人 + フォロワー)
	assert.Contains(t, pub.topics, "homeTimeline:author")
}

func TestFanoutHook_StreamingPublisherUnsetIsNoOp(t *testing.T) {
	h, _, _ := newTestHook(t)
	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})
}

func TestFanoutHook_StreamingFanoutErrorPath(t *testing.T) {
	// failingFollowingRepo は ListFollowers でエラーを返す → fanoutToFollowersAndStream
	// は早期 return し、follower への streaming publish は届かない
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	following := &failingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository()}
	h := NewFanoutHook(fanout, following)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})
	// follower 配信は届かないが、自分宛と local/global は publish される
	assert.Contains(t, pub.topics, "homeTimeline:author")
}

func TestFanoutHook_StreamingFanoutAcrossPages(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	following := testutil.NewMockFollowingRepository()
	for i := range 201 {
		fid := "f-" + idGen.Generate(time.Now().Add(time.Duration(i)*time.Microsecond))
		following.Followings[fid] = &model.Following{
			ID:         fid,
			FollowerID: "follower-" + fid,
			FolloweeID: "author",
		}
	}
	h := NewFanoutHook(fanout, following)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})
	// 201 人 + 自分 + local + global = 204 トピック以上
	assert.GreaterOrEqual(t, len(pub.topics), 201+1+1+1)
}

// countingFollowingRepo wraps MockFollowingRepository to count ListFollowers
// calls. 旧実装は Redis push と streaming publish で同じ followers ページネ
// ーションを 2 回繰り返していた。マージ後は 1 ページにつき 1 call で済む
// ことを担保する (#300 2-4)。
//
// #1468 review の N+1 解消で `Exists` から `FilterFollowingsToAnchor` へ
// 切り替えた regression gate にも使う。`existsCalls` / `filterToAnchorCalls` /
// `lastCandidates` を見て「Exists は呼ばれない」「batch query は 1 回・
// candidates は distinct owner」を assert する。
type countingFollowingRepo struct {
	*testutil.MockFollowingRepository
	listFollowersCalls  atomic.Int64
	existsCalls         int
	filterToAnchorCalls int
	lastCandidates      []string
}

func (c *countingFollowingRepo) ListFollowers(userID string, limit, offset int) ([]*model.Following, error) {
	c.listFollowersCalls.Add(1)
	return c.MockFollowingRepository.ListFollowers(userID, limit, offset)
}

func (c *countingFollowingRepo) Exists(followerID, followeeID string) (bool, error) {
	c.existsCalls++
	return c.MockFollowingRepository.Exists(followerID, followeeID)
}

func (c *countingFollowingRepo) FilterFollowingsToAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	c.filterToAnchorCalls++
	c.lastCandidates = append([]string(nil), candidateIDs...)
	return c.MockFollowingRepository.FilterFollowingsToAnchor(anchorID, candidateIDs)
}

// インターフェース実装は完全に MockFollowingRepository に委譲するための
// コンパイル時アサート。
var _ repository.FollowingRepository = (*countingFollowingRepo)(nil)

// failingFilterFollowingRepo wraps MockFollowingRepository so
// FilterFollowingsToAnchor always errors. Used to assert the fail-closed branch
// of fanoutToUserLists when batch follow check is unavailable (#1468 review).
type failingFilterFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFilterFollowingRepo) FilterFollowingsToAnchor(_ string, _ []string) ([]string, error) {
	return nil, assertError{}
}

var _ repository.FollowingRepository = (*failingFilterFollowingRepo)(nil)

func TestFanoutHook_FollowersFanoutSinglePassListsFollowersOnce(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	mock := testutil.NewMockFollowingRepository()
	for i := range 3 {
		fid := "f-" + idGen.Generate(time.Now().Add(time.Duration(i)*time.Microsecond))
		mock.Followings[fid] = &model.Following{
			ID:         fid,
			FollowerID: "follower-" + fid,
			FolloweeID: "author",
		}
	}
	following := &countingFollowingRepo{MockFollowingRepository: mock}
	h := NewFanoutHook(fanout, following)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// 3 followers, pageSize=200 なので 1 page で読み切る → 1 call が期待値。
	// 旧実装は Redis push 用 + streaming publish 用で 2 page をフェッチ。
	assert.Equal(t, int64(1), following.listFollowersCalls.Load(),
		"merged loop must list followers exactly once per page (perf #300 2-4)")
	// Redis push と streaming publish が両方届く。
	for fid := range mock.Followings {
		topic := "homeTimeline:" + mock.Followings[fid].FollowerID
		assert.Contains(t, pub.topics, topic)
	}
}

// pageSize=200 を超えるフォロワー数でも、1 ページにつき 1 call で済む
// (旧実装では 2 ページ x 2 ループ = 4 call、新実装は 2 page = 2 call)。
func TestFanoutHook_FollowersFanoutSinglePassAcrossPages(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	mock := testutil.NewMockFollowingRepository()
	for i := range 250 { // 200 + 50, 2 pages
		fid := "f-" + idGen.Generate(time.Now().Add(time.Duration(i)*time.Microsecond))
		mock.Followings[fid] = &model.Following{
			ID:         fid,
			FollowerID: "follower-" + fid,
			FolloweeID: "author",
		}
	}
	following := &countingFollowingRepo{MockFollowingRepository: mock}
	h := NewFanoutHook(fanout, following)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// 250 followers / pageSize=200 = 2 pages → 2 calls (新実装)。
	// 旧実装なら 4 calls。
	assert.Equal(t, int64(2), following.listFollowersCalls.Load(),
		"merged loop on 250 followers must take 2 page calls (was 4 before #300 2-4)")
}

func TestFanoutHook_StreamingPublisherNilFollowingRepo(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	h := NewFanoutHook(fanout, nil)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)
	// followingRepo nil でも publish 自体は走る (followers パートだけスキップ)
	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "u", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "u"})
}

// --- UserList Timeline Fanout ---

// stubUserListLookup implements UserListMemberLookup for testing.
type stubUserListLookup struct {
	// memberToLists maps userID -> list IDs containing that user.
	memberToLists map[string][]string
	// listOwners maps listID -> ownerID. ListIDsAndOwnersByMember (#1465) で
	// followers visibility note の per-list owner follow gate を再現する。
	// 旧テストで未設定なら listID 自身を owner として扱う (= public note 経路の
	// 既存挙動を保つ defensive fallback)。
	listOwners map[string]string
}

func (s *stubUserListLookup) ListIDsByMember(userID string) ([]string, error) {
	return s.memberToLists[userID], nil
}

// ListIDsAndOwnersByMember returns {listID: ownerID} for lists containing memberID.
// listOwners が nil の test では listID 自身を owner として返す
// (= follow gate に到達しない public note path の既存挙動を破壊しない)。
func (s *stubUserListLookup) ListIDsAndOwnersByMember(memberID string) (map[string]string, error) {
	out := make(map[string]string)
	for _, listID := range s.memberToLists[memberID] {
		if s.listOwners != nil {
			if owner, ok := s.listOwners[listID]; ok {
				out[listID] = owner
				continue
			}
		}
		// fallback: listID 自体を owner 扱い (test compat)
		out[listID] = listID
	}
	return out, nil
}

func TestFanoutHook_FanoutToUserLists(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	lookup := &stubUserListLookup{memberToLists: map[string][]string{
		"author": {"list1", "list2"},
	}}
	h.SetUserListRepo(lookup)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// ユーザーリストタイムラインにノートが配信されること
	for _, listID := range []string{"list1", "list2"} {
		out, err := fanout.Get(ctx, UserListTimelineName(listID), "", "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, out, "userListTimeline:%s should contain note", listID)
	}
}

func TestFanoutHook_FanoutToUserLists_StreamingPublish(t *testing.T) {
	h, _, _ := newTestHook(t)

	lookup := &stubUserListLookup{memberToLists: map[string][]string{
		"author": {"list1"},
	}}
	h.SetUserListRepo(lookup)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// ストリーミングにuserListTimeline:list1が配信されること
	assert.Contains(t, pub.topics, "userListTimeline:list1")
}

func TestFanoutHook_FanoutToUserLists_SpecifiedVisibilitySkipped(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	lookup := &stubUserListLookup{memberToLists: map[string][]string{
		"author": {"list1"},
	}}
	h.SetUserListRepo(lookup)

	noteID := idGen.Generate(time.Now())
	// specified visibilityはフォロワー配信対象外 → ユーザーリストにも配信されない
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilitySpecified}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	out, err := fanout.Get(ctx, UserListTimelineName("list1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestFanoutHook_FanoutToUserLists_NilLookup(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	// userListRepoがnilの場合はユーザーリストへの配信をスキップ
	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	out, err := fanout.Get(ctx, UserListTimelineName("list1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// failingUserListLookup always returns an error.
type failingUserListLookup struct{}

func (f *failingUserListLookup) ListIDsByMember(_ string) ([]string, error) {
	return nil, assertError{}
}

func (f *failingUserListLookup) ListIDsAndOwnersByMember(_ string) (map[string]string, error) {
	return nil, assertError{}
}

func TestFanoutHook_FanoutToUserLists_LookupError(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	h.SetUserListRepo(&failingUserListLookup{})

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	// エラーがあっても上位に伝搬しない（ログ出力のみ）
	h.OnNoteCreated(n, &model.User{ID: "author"})

	out, err := fanout.Get(ctx, UserListTimelineName("list1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// --- followers visibility user list fanout gate (#1465) ----------------------

// followers visibility note: list owner が author を follow していない場合、
// その list には push されない。
func TestFanoutHook_FanoutToUserLists_FollowersVisibility_NonFollowerOwnerDropped(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()

	// list1 は owner=alice、list2 は owner=bob。author は両 list の member。
	lookup := &stubUserListLookup{
		memberToLists: map[string][]string{"author": {"list1", "list2"}},
		listOwners:    map[string]string{"list1": "alice", "list2": "bob"},
	}
	h.SetUserListRepo(lookup)
	// alice だけが author を follow。bob は follow していない。
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "author"}

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// alice の list は push される
	out, err := fanout.Get(ctx, UserListTimelineName("list1"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "list1 (owned by follower alice) should receive followers note")

	// bob の list は push されない
	out, err = fanout.Get(ctx, UserListTimelineName("list2"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "list2 (owned by non-follower bob) should NOT receive followers note")
}

// followers visibility note: owner == author の list には follow check 無しで
// push される (本人 short-circuit)。
func TestFanoutHook_FanoutToUserLists_FollowersVisibility_SelfOwnedList(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()

	// author 自身が owner の self-list。following は空 (= author は誰も follow していない)。
	lookup := &stubUserListLookup{
		memberToLists: map[string][]string{"author": {"self-list"}},
		listOwners:    map[string]string{"self-list": "author"},
	}
	h.SetUserListRepo(lookup)
	_ = following // 未使用だが setup の対称性のため取得

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	out, err := fanout.Get(ctx, UserListTimelineName("self-list"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "self-owned list should always receive own followers note")
}

// followers visibility note + followingRepo 未配線 → fail-closed (= 他 owner の
// list へは push しない)。NewFanoutHook の constructor が必ず followingRepo を
// 受け取るので production では起きない経路だが、safety net として確認する。
func TestFanoutHook_FanoutToUserLists_FollowersVisibility_NilFollowingRepoFailClosed(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	// followingRepo を解除 (production では起きない nil state)。
	h.followingRepo = nil

	lookup := &stubUserListLookup{
		memberToLists: map[string][]string{"author": {"list1", "self-list"}},
		listOwners:    map[string]string{"list1": "alice", "self-list": "author"},
	}
	h.SetUserListRepo(lookup)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	// 本人 list には push される
	out, err := fanout.Get(ctx, UserListTimelineName("self-list"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)

	// 他 owner の list には push されない (fail-closed)
	out, err = fanout.Get(ctx, UserListTimelineName("list1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// public/home visibility note は per-list owner follow check を経ずに全 list
// に push される (旧経路 hot path の regression guard)。
func TestFanoutHook_FanoutToUserLists_PublicNote_NoFollowCheck(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	// list1 は owner=alice (author を follow していない)。にもかかわらず public
	// note は push される。
	lookup := &stubUserListLookup{
		memberToLists: map[string][]string{"author": {"list1"}},
		listOwners:    map[string]string{"list1": "alice"},
	}
	h.SetUserListRepo(lookup)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	out, err := fanout.Get(ctx, UserListTimelineName("list1"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)
}

// #1468 review: distinct owner 集合に対する FilterFollowingsToAnchor 1 query で
// 「author を follow している owner」を判定し、per-owner Exists の N+1 を消す。
// 5 list / 4 distinct owner (うち 1 名重複所有) のシナリオで、batch query は
// 1 回・候補は distinct owner 数 (3 名、本人除く) だけ渡る・本人 list は
// 無条件 push される、を assert する。
func TestFanoutHook_FanoutToUserLists_FollowersVisibility_BatchFollowCheck_SingleCall(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	// list1 / list2 -> alice (follower / 同一 owner で 2 list)
	// list3 -> bob (follower)
	// list4 -> carol (non-follower)
	// list5 -> author (本人)
	lookup := &stubUserListLookup{
		memberToLists: map[string][]string{"author": {"list1", "list2", "list3", "list4", "list5"}},
		listOwners: map[string]string{
			"list1": "alice", "list2": "alice",
			"list3": "bob",
			"list4": "carol",
			"list5": "author",
		},
	}
	h.SetUserListRepo(lookup)

	counting := &countingFollowingRepo{
		MockFollowingRepository: testutil.NewMockFollowingRepository(),
	}
	require.NoError(t, counting.Create(&model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "author"}))
	require.NoError(t, counting.Create(&model.Following{ID: "f2", FollowerID: "bob", FolloweeID: "author"}))
	h.followingRepo = counting

	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(
		&model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers},
		&model.User{ID: "author"},
	)

	// follower 所有 list + 本人 list には push、non-follower には push されない
	for _, listID := range []string{"list1", "list2", "list3", "list5"} {
		out, err := fanout.Get(ctx, UserListTimelineName(listID), "", "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, out, "%s should receive followers note", listID)
	}
	out, err := fanout.Get(ctx, UserListTimelineName("list4"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "list4 (non-follower carol) should NOT receive followers note")

	// N+1 解消の核心: batch query は 1 回・per-owner Exists は呼ばれない
	assert.Equal(t, 1, counting.filterToAnchorCalls,
		"FilterFollowingsToAnchor should be called exactly once for the whole fanout")
	assert.Equal(t, 0, counting.existsCalls,
		"Exists should not be called after #1468 review N+1 fix")
	// distinct owner 数 (本人除外) が候補に渡っていることを assert
	require.Len(t, counting.lastCandidates, 3)
	assert.ElementsMatch(t, []string{"alice", "bob", "carol"}, counting.lastCandidates,
		"candidates should be the distinct non-self owners")
}

// #1468 review: FilterFollowingsToAnchor がエラーを返した時は本人 list 以外を
// push しない fail-closed 挙動。旧実装は per-owner Exists エラーをスキップして
// 他 owner を続行していたが、batch query では「誰が follow しているか分からない」
// 状況なので全体を保守的に閉じる。
func TestFanoutHook_FanoutToUserLists_FollowersVisibility_BatchFollowCheck_Error_FailClosed(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	lookup := &stubUserListLookup{
		memberToLists: map[string][]string{"author": {"list1", "self-list"}},
		listOwners: map[string]string{
			"list1":     "alice", // 本来 follow しているが query が落ちる経路
			"self-list": "author",
		},
	}
	h.SetUserListRepo(lookup)

	failing := &failingFilterFollowingRepo{
		MockFollowingRepository: testutil.NewMockFollowingRepository(),
	}
	// alice は author を follow しているが、 batch query 自体が boom する
	require.NoError(t, failing.Create(&model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "author"}))
	h.followingRepo = failing

	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(
		&model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers},
		&model.User{ID: "author"},
	)

	// 本人 list には push される
	out, err := fanout.Get(ctx, UserListTimelineName("self-list"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)
	// 他 owner の list は fail-closed で push されない
	out, err = fanout.Get(ctx, UserListTimelineName("list1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "non-self list must not receive note when batch follow check errors out")
}

// followers visibility 経路の lookup error はベストエフォートで握り潰す。
func TestFanoutHook_FanoutToUserLists_FollowersVisibility_LookupError(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	h.SetUserListRepo(&failingUserListLookup{})

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityFollowers}
	// エラーが上位に伝搬しないこと
	h.OnNoteCreated(n, &model.User{ID: "author"})

	out, err := fanout.Get(ctx, UserListTimelineName("list1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// --- OnNoteDeleted (#379) ---

func TestFanoutHook_OnNoteDeleted_Nil(t *testing.T) {
	h, _, _ := newTestHook(t)
	h.OnNoteDeleted(nil, &model.User{ID: "u"})
	h.OnNoteDeleted(&model.Note{ID: "n"}, nil)
}

func TestFanoutHook_OnNoteDeleted_PurgesAllTimelines(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author"}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "follower2", FolloweeID: "author"}

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "author"}
	h.OnNoteCreated(n, author)

	// 配信された 5 つの timeline すべてに入っていることを前提として確認
	for _, name := range []Name{
		HomeTimelineName("author"),
		HomeTimelineName("follower1"),
		HomeTimelineName("follower2"),
		UserTimelineName("author"),
		LocalTimeline,
		GlobalTimeline,
	} {
		out, err := fanout.Get(ctx, name, "", "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, out, "precondition: %q should contain note", name)
	}

	// 削除すると全部から消える
	h.OnNoteDeleted(n, author)
	for _, name := range []Name{
		HomeTimelineName("author"),
		HomeTimelineName("follower1"),
		HomeTimelineName("follower2"),
		UserTimelineName("author"),
		LocalTimeline,
		GlobalTimeline,
	} {
		out, err := fanout.Get(ctx, name, "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, out, "after delete: %q should be empty", name)
	}
}

func TestFanoutHook_OnNoteDeleted_RemoteAuthorSkipsLocal(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	host := "remote.example"
	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "ra", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "ra", Host: &host}

	// LocalTimeline に直接 LPUSH しておいて、OnNoteDeleted で消えないことを確認
	require.NoError(t, fanout.client.LPush(ctx, fanout.key(LocalTimeline), noteID).Err())

	h.OnNoteDeleted(n, author)

	out, err := fanout.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "remote author の delete は LocalTimeline を触らない")
}

func TestFanoutHook_OnNoteDeleted_FollowersListError(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	following := &failingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository()}
	h := NewFanoutHook(fanout, following)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	// エラーがあっても上位に伝搬しない
	h.OnNoteDeleted(n, &model.User{ID: "author"})
}

func TestFanoutHook_OnNoteDeleted_FollowersAcrossPages(t *testing.T) {
	testRedis.FlushAll(context.Background())
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	following := testutil.NewMockFollowingRepository()
	for i := range 201 {
		fid := "f-" + idGen.Generate(time.Now().Add(time.Duration(i)*time.Microsecond))
		following.Followings[fid] = &model.Following{
			ID:         fid,
			FollowerID: "follower-" + fid,
			FolloweeID: "author",
		}
	}
	h := NewFanoutHook(fanout, following)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteDeleted(n, &model.User{ID: "author"})
}

func TestFanoutHook_OnNoteDeleted_RemoveErrorIsLogged(t *testing.T) {
	following := testutil.NewMockFollowingRepository()
	fanout := NewFanoutTimelineService(closedClient(t), idGen, "")
	h := NewFanoutHook(fanout, following)
	noteID := idGen.Generate(time.Now())
	h.OnNoteDeleted(
		&model.Note{ID: noteID, UserID: "u", Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "u"},
	)
}

// specified も宛先の follower の home に push されるので、削除でも同じ範囲を
// 掃除する (push と remove の対称性)。
func TestFanoutHook_OnNoteDeleted_SpecifiedCleansFollowerHome(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower1", FolloweeID: "author"}

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilitySpecified}
	author := &model.User{ID: "author"}

	require.NoError(t, fanout.client.LPush(ctx, fanout.key(HomeTimelineName("follower1")), noteID).Err())

	h.OnNoteDeleted(n, author)

	out, err := fanout.Get(ctx, HomeTimelineName("follower1"), "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out, "push した範囲は delete でも消す")
}

func TestFanoutHook_OnNoteDeleted_UserListPurge(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	lookup := &stubUserListLookup{memberToLists: map[string][]string{
		"author": {"list1", "list2"},
	}}
	h.SetUserListRepo(lookup)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "author"}
	h.OnNoteCreated(n, author)
	h.OnNoteDeleted(n, author)

	for _, listID := range []string{"list1", "list2"} {
		out, err := fanout.Get(ctx, UserListTimelineName(listID), "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, out, "userListTimeline:%s should be purged", listID)
	}
}

func TestFanoutHook_OnNoteDeleted_UserListLookupError(t *testing.T) {
	h, _, _ := newTestHook(t)
	h.SetUserListRepo(&failingUserListLookup{})

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.OnNoteDeleted(n, &model.User{ID: "author"})
}

// #2106 L34: remote / hibernated follower の home TL には push しない (local non-hibernated のみ)。
func TestFanoutHook_SkipsRemoteAndHibernatedFollowers(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	remoteHost := "remote.example"
	following.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "localf", FolloweeID: "author"}
	following.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "remotef", FolloweeID: "author", FollowerHost: &remoteHost}
	following.Followings["f3"] = &model.Following{ID: "f3", FollowerID: "hibf", FolloweeID: "author", IsFollowerHibernated: true}

	noteID := idGen.Generate(time.Now())
	n := &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	author := &model.User{ID: "author"}
	h.OnNoteCreated(n, author)

	out, err := fanout.Get(ctx, HomeTimelineName("localf"), "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out, "local non-hibernated follower receives home push")

	for _, fid := range []string{"remotef", "hibf"} {
		out, err := fanout.Get(ctx, HomeTimelineName(fid), "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, out, "follower %q (remote/hibernated) should not receive home push", fid)
	}
}

// specified (DM) もリスト TL に push する。ただしリスト所有者が author 本人か
// 宛先に含まれる場合だけ。upstream NoteCreateService の userListMemberships
// ループと同じ判定で、他人宛ての DM を他人のリストに流さない。
func TestFanoutHook_SpecifiedToUserLists(t *testing.T) {
	h, fanout, _ := newTestHook(t)
	ctx := context.Background()

	lists := &stubUserLists{owners: map[string]string{
		"list_mine":  "alice",  // 宛先に入っている所有者
		"list_other": "dave",   // 宛先外
		"list_self":  "author", // author 本人のリスト
	}}
	h.SetUserListRepo(lists)

	noteID := idGen.Generate(time.Now())
	n := &model.Note{
		ID: noteID, UserID: "author",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"alice"},
	}
	h.OnNoteCreated(n, &model.User{ID: "author"})

	for _, tc := range []struct {
		list string
		want bool
	}{
		{"list_mine", true},
		{"list_self", true},
		{"list_other", false},
	} {
		out, err := fanout.Get(ctx, UserListTimelineName(tc.list), "", "", 10)
		require.NoError(t, err)
		if tc.want {
			assert.Equal(t, []string{noteID}, out, tc.list)
		} else {
			assert.Empty(t, out, tc.list)
		}
	}
}

// stubUserLists implements UserListMemberLookup for the tests above.
type stubUserLists struct{ owners map[string]string }

func (s *stubUserLists) ListIDsByMember(string) ([]string, error) {
	ids := make([]string, 0, len(s.owners))
	for id := range s.owners {
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *stubUserLists) ListIDsAndOwnersByMember(string) (map[string]string, error) {
	return s.owners, nil
}
