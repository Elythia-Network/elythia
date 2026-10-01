package reaction_test

import (
	"errors"
	"testing"

	"github.com/shiroha-a/mk/internal/core/reaction"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newService(t *testing.T) (
	*reaction.Service,
	*testutil.MockNoteRepository,
	*testutil.MockNoteReactionRepository,
	*testutil.MockEmojiRepository,
	*testutil.MockFollowingRepository,
) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	reactRepo := testutil.NewMockNoteReactionRepository()
	emojiRepo := testutil.NewMockEmojiRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(noteRepo, reactRepo, emojiRepo, followingRepo, idGen)
	return svc, noteRepo, reactRepo, emojiRepo, followingRepo
}

func seedNote(repo *testutil.MockNoteRepository, id, userID string, vis model.NoteVisibility) *model.Note {
	n := &model.Note{ID: id, UserID: userID, Visibility: vis}
	repo.Notes[id] = n
	return n
}

func TestService_Create_NilUser(t *testing.T) {
	svc, _, _, _, _ := newService(t)
	_, err := svc.Create(nil, "n", "")
	require.Error(t, err)
}

func TestService_Create_NoteNotFound(t *testing.T) {
	svc, _, _, _, _ := newService(t)
	_, err := svc.Create(&model.User{ID: "u"}, "ghost", "")
	require.ErrorIs(t, err, reaction.ErrNoteNotFound)
}

func TestService_Create_NotVisible(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityFollowers)
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "")
	require.ErrorIs(t, err, reaction.ErrNoteNotVisible)
}

func TestService_Create_PureRenote(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	target := "x"
	repo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: model.NoteVisibilityPublic, RenoteID: &target,
	}
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "")
	require.ErrorIs(t, err, reaction.ErrCannotReactToPureRenote)
}

func TestService_Create_HappyPathFallback(t *testing.T) {
	svc, repo, reactRepo, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", "")
	require.NoError(t, err)
	assert.Equal(t, reaction.FallbackReaction, r)
	assert.Len(t, reactRepo.Reactions, 1)
}

func TestService_Create_LegacyTranslation(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", "like")
	require.NoError(t, err)
	assert.Equal(t, "👍", r)
}

// TestService_Create_VariationSelectorStripped covers the #864 fix where
// Unicode emoji variation selector (U+FE0F) is stripped during normalization
// so that mk-go saves the same canonical form as upstream Misskey TS.
func TestService_Create_VariationSelectorStripped(t *testing.T) {
	svc, repo, reactRepo, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	// "\u2764\ufe0f" (= "❤️" with variation selector) should be normalized
	// to "\u2764" (= "❤" without VS-16).
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", "\u2764\ufe0f")
	require.NoError(t, err)
	assert.Equal(t, "\u2764", r)
	require.Len(t, reactRepo.Reactions, 1)
	// reactRepo.Reactions は id-keyed map なので、唯一の entry の Reaction
	// 文字列が strip 後の `❤` (= U+2764) になっていることを確認。
	for _, rec := range reactRepo.Reactions {
		assert.Equal(t, "\u2764", rec.Reaction)
	}
}

func TestService_Create_AlreadyReactedSame(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	_, err = svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.ErrorIs(t, err, reaction.ErrAlreadyReacted)
}

// TestService_Create_DuplicateConvertedToAlreadyReacted: race condition で
// reactionRepo.Create が `repository.ErrDuplicateReaction` を返したとき、
// service は `ErrAlreadyReacted` に変換して caller に返す。`FindByPair`
// 後の窓で並行 Create が入った AP idempotent semantic を保つため (#1186)。
func TestService_Create_DuplicateConvertedToAlreadyReacted(t *testing.T) {
	svc, repo, reactRepo, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	// FindByPair は何も返さず (= 既存 reaction 無いように見える) →
	// service は新規 Create に進む → ここで race 想定の
	// ErrDuplicateReaction を返す。
	reactRepo.CreateErr = repository.ErrDuplicateReaction

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.ErrorIs(t, err, reaction.ErrAlreadyReacted,
		"repository ErrDuplicateReaction must be converted to service ErrAlreadyReacted")
}

func TestService_Create_ReplaceExisting(t *testing.T) {
	svc, repo, reactRepo, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	_, err = svc.Create(&model.User{ID: "viewer"}, "n1", "❤")
	require.NoError(t, err)
	// 古いレコードは削除されているので1件
	assert.Len(t, reactRepo.Reactions, 1)
}

func TestService_Create_CustomEmojiLocal(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	emojiRepo.Emojis["smile@"] = &model.Emoji{Name: "smile"}
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":smile:")
	require.NoError(t, err)
	assert.Equal(t, ":smile@.:", r)
}

func TestService_Create_CustomEmojiRemote(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	host := "remote.example"
	emojiRepo.Emojis["smile@remote.example"] = &model.Emoji{Name: "smile", Host: &host}
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":smile@remote.example:")
	require.NoError(t, err)
	assert.Equal(t, ":smile@remote.example:", r)
}

func TestService_Create_CustomEmojiLocalWithDotHost(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	emojiRepo.Emojis["smile@"] = &model.Emoji{Name: "smile"}
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":smile@.:")
	require.NoError(t, err)
	assert.Equal(t, ":smile@.:", r)
}

func TestService_Create_CustomEmojiRemoteWithoutHostInString(t *testing.T) {
	// #459: Misskey TS upstream は :name: 形式 (host 省略) で reaction を
	// 連合させるが、その emoji は reactor の host に紐付いている。受信側
	// (mk-go) は文字列 host が空でも actor.host で emoji table を引き
	// 直し、見つかれば :name@host: に正規化する。
	svc, repo, _, emojiRepo, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	host := "remote.example"
	emojiRepo.Emojis["smile@remote.example"] = &model.Emoji{Name: "smile", Host: &host}

	r, err := svc.Create(&model.User{ID: "remote-user", Host: &host}, "n1", ":smile:")
	require.NoError(t, err)
	assert.Equal(t, ":smile@remote.example:", r, "actor host で emoji を解決して remote canonical 化")
}

func TestService_Create_CustomEmojiNotFoundFallback(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":nonexistent:")
	require.NoError(t, err)
	assert.Equal(t, reaction.FallbackReaction, r)
}

// failingReactionRepo simulates a Create error.
type failingReactionRepo struct {
	*testutil.MockNoteReactionRepository
}

func (f *failingReactionRepo) Create(_ *model.NoteReaction) error {
	return errors.New("boom")
}

func TestService_Create_RepoError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedNote(noteRepo, "n1", "author", model.NoteVisibilityPublic)
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(
		noteRepo,
		&failingReactionRepo{MockNoteReactionRepository: testutil.NewMockNoteReactionRepository()},
		testutil.NewMockEmojiRepository(),
		testutil.NewMockFollowingRepository(),
		idGen,
	)
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	assert.Error(t, err)
}

// failingReactionRepoOnDelete fails on Delete (replace path).
type failingReactionRepoOnDelete struct {
	*testutil.MockNoteReactionRepository
}

func (f *failingReactionRepoOnDelete) Delete(_ *model.NoteReaction) (int64, error) {
	return 0, errors.New("delete boom")
}

func TestService_Create_ReplaceDeleteError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedNote(noteRepo, "n1", "author", model.NoteVisibilityPublic)
	mock := testutil.NewMockNoteReactionRepository()
	// 既存リアクションを差し込む
	mock.Reactions["existing"] = &model.NoteReaction{
		ID: "existing", UserID: "viewer", NoteID: "n1", Reaction: "👍",
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(
		noteRepo,
		&failingReactionRepoOnDelete{MockNoteReactionRepository: mock},
		testutil.NewMockEmojiRepository(),
		testutil.NewMockFollowingRepository(),
		idGen,
	)
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "❤")
	assert.Error(t, err)
}

// #3272: a concurrent replacement may remove the row after FindByPair but
// before Delete. The loser must stop before decrementing the old count or
// publishing removal/creation side effects.
func TestService_Create_ReplaceConcurrentZeroAffected(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedNote(noteRepo, "n1", "author", model.NoteVisibilityPublic)
	noteRepo.ReactionCounts = map[string]map[string]int{
		"n1": {"old": 1},
	}
	mock := testutil.NewMockNoteReactionRepository()
	mock.Reactions["existing"] = &model.NoteReaction{
		ID: "existing", UserID: "viewer", NoteID: "n1", Reaction: "old",
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(
		noteRepo,
		&zeroAffectedDeleteRepo{MockNoteReactionRepository: mock},
		testutil.NewMockEmojiRepository(),
		testutil.NewMockFollowingRepository(),
		idGen,
	)
	notifications := &recordingNotificationHook{}
	federation := &recordingFederationHook{}
	stream := &recordingNoteStreamHook{}
	svc.SetNotificationHook(notifications)
	svc.SetFederationHook(federation)
	svc.SetNoteStreamHook(stream)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "new")

	assert.ErrorIs(t, err, reaction.ErrReactionNotFound)
	assert.Equal(t, map[string]int{"old": 1}, noteRepo.ReactionCounts["n1"])
	assert.Equal(t, map[string]*model.NoteReaction{
		"existing": {ID: "existing", UserID: "viewer", NoteID: "n1", Reaction: "old"},
	}, mock.Reactions)
	assert.Empty(t, notifications.events)
	assert.Empty(t, federation.added)
	assert.Empty(t, federation.removed)
	assert.Empty(t, stream.reacted)
	assert.Empty(t, stream.unreacted)
}

func TestService_Delete_NilUser(t *testing.T) {
	svc, _, _, _, _ := newService(t)
	err := svc.Delete(nil, "n")
	require.Error(t, err)
}

func TestService_Delete_NoteNotFound(t *testing.T) {
	svc, _, _, _, _ := newService(t)
	err := svc.Delete(&model.User{ID: "u"}, "ghost")
	require.ErrorIs(t, err, reaction.ErrNoteNotFound)
}

func TestService_Delete_ReactionNotFound(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	err := svc.Delete(&model.User{ID: "viewer"}, "n1")
	require.ErrorIs(t, err, reaction.ErrReactionNotFound)
}

func TestService_Delete_HappyPath(t *testing.T) {
	svc, repo, reactRepo, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	require.NoError(t, svc.Delete(&model.User{ID: "viewer"}, "n1"))
	assert.Empty(t, reactRepo.Reactions)
}

// failingDeleteRepo causes Delete to fail (used for delete path coverage).
type failingDeleteRepo struct {
	*testutil.MockNoteReactionRepository
}

func (f *failingDeleteRepo) Delete(_ *model.NoteReaction) (int64, error) {
	return 0, errors.New("delete fail")
}

func TestService_Delete_RepoError(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedNote(noteRepo, "n1", "author", model.NoteVisibilityPublic)
	mock := testutil.NewMockNoteReactionRepository()
	mock.Reactions["existing"] = &model.NoteReaction{
		ID: "existing", UserID: "viewer", NoteID: "n1", Reaction: "👍",
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(
		noteRepo,
		&failingDeleteRepo{MockNoteReactionRepository: mock},
		testutil.NewMockEmojiRepository(),
		testutil.NewMockFollowingRepository(),
		idGen,
	)
	err := svc.Delete(&model.User{ID: "viewer"}, "n1")
	assert.Error(t, err)
}

func TestService_List_NoteNotFound(t *testing.T) {
	svc, _, _, _, _ := newService(t)
	_, err := svc.List(nil, "ghost", "", "", 10, "")
	require.ErrorIs(t, err, reaction.ErrNoteNotFound)
}

func TestService_List_NotVisible(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityFollowers)
	_, err := svc.List(&model.User{ID: "viewer"}, "n1", "", "", 10, "")
	require.ErrorIs(t, err, reaction.ErrNoteNotVisible)
}

func TestService_List_Filtered(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	_, err := svc.Create(&model.User{ID: "u1"}, "n1", "👍")
	require.NoError(t, err)
	_, err = svc.Create(&model.User{ID: "u2"}, "n1", "❤")
	require.NoError(t, err)

	out, err := svc.List(nil, "n1", "", "", 0, "👍")
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "👍", out[0].Reaction)
}

func TestService_List_LegacyReactionMatched(t *testing.T) {
	svc, noteRepo, reactRepo, emojiRepo, _ := newService(t)
	seedNote(noteRepo, "n1", "author", model.NoteVisibilityPublic)
	// TS時代の `:smile:` 形式でDBに保存されたリアクションを直接挿入
	emojiRepo.Emojis["smile@"] = &model.Emoji{Name: "smile"}
	reactRepo.Reactions["rx_legacy"] = &model.NoteReaction{
		ID: "rx_legacy", UserID: "u1", NoteID: "n1", Reaction: ":smile:",
	}
	// mk時代の `:smile@.:` 形式でもリアクションを追加
	reactRepo.Reactions["rx_canonical"] = &model.NoteReaction{
		ID: "rx_canonical", UserID: "u2", NoteID: "n1", Reaction: ":smile@.:",
	}

	// `:smile@.:` でフィルタすると両方ヒットする
	out, err := svc.List(nil, "n1", "", "", 10, ":smile@.:")
	require.NoError(t, err)
	assert.Len(t, out, 2)

	// `:smile:` でフィルタしても正規化されて両方ヒットする
	out, err = svc.List(nil, "n1", "", "", 10, ":smile:")
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

func TestIsPureRenote_Variants(t *testing.T) {
	target := "x"
	cases := []struct {
		name string
		n    *model.Note
		want bool
	}{
		{"no renote", &model.Note{}, false},
		{"pure", &model.Note{RenoteID: &target}, true},
		{"with text", &model.Note{RenoteID: &target, Text: ptrString("hi")}, false},
		{"with cw", &model.Note{RenoteID: &target, CW: ptrString("warn")}, false},
		{"with file", &model.Note{RenoteID: &target, FileIDs: model.StringArray{"f1"}}, false},
		{"with poll", &model.Note{RenoteID: &target, HasPoll: true}, false},
	}
	// IsPureRenote はパッケージ内なのでテストヘルパ経由ではなく
	// 同じ判定をServiceの動作で間接的に検証する
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(
		noteRepo,
		testutil.NewMockNoteReactionRepository(),
		testutil.NewMockEmojiRepository(),
		testutil.NewMockFollowingRepository(),
		idGen,
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.n.ID = "n_" + tc.name
			tc.n.UserID = "author"
			tc.n.Visibility = model.NoteVisibilityPublic
			noteRepo.Notes[tc.n.ID] = tc.n
			_, err := svc.Create(&model.User{ID: "viewer"}, tc.n.ID, "👍")
			if tc.want {
				assert.ErrorIs(t, err, reaction.ErrCannotReactToPureRenote)
			} else {
				assert.NoError(t, err)
			}
			delete(noteRepo.Notes, tc.n.ID)
		})
	}
}

func ptrString(s string) *string { return &s }

// recordingNotificationHook captures reaction notification calls.
type recordingNotificationHook struct {
	called   bool
	notifiee string
	notifier string
	noteID   string
	reaction string
	// events records created / removed calls in order (#3201).
	events []string
}

func (h *recordingNotificationHook) OnReactionRemoved(notifieeID, notifierID, noteID string) {
	h.events = append(h.events, "removed "+notifieeID+"/"+notifierID+"/"+noteID)
}

func (h *recordingNotificationHook) OnReactionCreated(notifieeID, notifierID, noteID, rx string) {
	h.called = true
	h.notifiee = notifieeID
	h.notifier = notifierID
	h.noteID = noteID
	h.reaction = rx
	h.events = append(h.events, "created "+notifieeID+"/"+notifierID+"/"+noteID+"/"+rx)
}

func TestService_NotificationHook_OnReaction(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingNotificationHook{}
	svc.SetNotificationHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	assert.True(t, hook.called)
	assert.Equal(t, "author", hook.notifiee)
	assert.Equal(t, "viewer", hook.notifier)
	assert.Equal(t, "n1", hook.noteID)
	assert.Equal(t, "👍", hook.reaction)
}

var errStubReaction = errors.New("reaction stub error")

// stubBlockingChecker for tests.
type stubBlockingChecker struct {
	blocked bool
	err     error
}

func (s *stubBlockingChecker) IsBlocked(_, _ string) (bool, error) {
	return s.blocked, s.err
}

func TestService_Create_Blocked(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	svc.SetBlockingChecker(&stubBlockingChecker{blocked: true})
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.ErrorIs(t, err, reaction.ErrBlocked)
}

func TestService_Create_BlockingCheckerError(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	svc.SetBlockingChecker(&stubBlockingChecker{err: errStubReaction})
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	assert.ErrorIs(t, err, errStubReaction)
}

func TestService_Create_BlockingCheckerSelfSkipped(t *testing.T) {
	// 自分自身のノートにはblockチェックが走らない
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "viewer", model.NoteVisibilityPublic)
	svc.SetBlockingChecker(&stubBlockingChecker{blocked: true})
	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
}

func TestService_NotificationHook_SelfReactionSkipped(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "viewer", model.NoteVisibilityPublic)
	hook := &recordingNotificationHook{}
	svc.SetNotificationHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	assert.False(t, hook.called)
}

// recordingFederationHook captures reaction federation calls.
type recordingFederationHook struct {
	added   []string // noteID:reaction
	removed []string
}

func (h *recordingFederationHook) OnReactionAdded(_ *model.User, target *model.Note, rx string) {
	h.added = append(h.added, target.ID+":"+rx)
}

func (h *recordingFederationHook) OnReactionRemoved(_ *model.User, target *model.Note, rx string) {
	h.removed = append(h.removed, target.ID+":"+rx)
}

func TestService_FederationHook_OnCreate(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	assert.Equal(t, []string{"n1:👍"}, hook.added)
	assert.Empty(t, hook.removed)
}

func TestService_FederationHook_OnReplace(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	_, err = svc.Create(&model.User{ID: "viewer"}, "n1", "🎉")
	require.NoError(t, err)
	// 置き換え時には Removed (古い) → Added (新しい) の順に発火
	assert.Equal(t, []string{"n1:👍", "n1:🎉"}, hook.added)
	assert.Equal(t, []string{"n1:👍"}, hook.removed)
}

func TestService_FederationHook_OnDelete(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingFederationHook{}
	svc.SetFederationHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	require.NoError(t, svc.Delete(&model.User{ID: "viewer"}, "n1"))
	assert.Equal(t, []string{"n1:👍"}, hook.removed)
}

// recordingChartHook captures chart hook fires from the reaction
// service.
type recordingChartHook struct {
	created [][2]string // [reactorID, noteID]
}

func (h *recordingChartHook) OnReactionCreated(reactor *model.User, note *model.Note) {
	h.created = append(h.created, [2]string{reactor.ID, note.ID})
}

func TestService_ChartHook_OnCreate(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingChartHook{}
	svc.SetChartHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	require.Len(t, hook.created, 1)
	assert.Equal(t, [2]string{"viewer", "n1"}, hook.created[0])
}

// recordingNoteStreamHook captures noteStream publish hook calls (#700)。
type recordingNoteStreamHook struct {
	reacted   []reactedCall
	unreacted []unreactedCall
}

type reactedCall struct {
	noteID, userID, reaction string
	emoji                    *reaction.NoteStreamEmoji
}

type unreactedCall struct {
	noteID, userID, reaction string
}

func (h *recordingNoteStreamHook) OnReacted(noteID, userID, rx string, emoji *reaction.NoteStreamEmoji) {
	h.reacted = append(h.reacted, reactedCall{noteID, userID, rx, emoji})
}

func (h *recordingNoteStreamHook) OnUnreacted(noteID, userID, rx string) {
	h.unreacted = append(h.unreacted, unreactedCall{noteID, userID, rx})
}

// 通常 (Unicode) リアクション付与で `reacted` が emoji=nil で publish される。
func TestService_NoteStreamHook_OnCreate_Unicode(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	require.Len(t, hook.reacted, 1)
	assert.Equal(t, "n1", hook.reacted[0].noteID)
	assert.Equal(t, "viewer", hook.reacted[0].userID)
	assert.Equal(t, "👍", hook.reacted[0].reaction)
	assert.Nil(t, hook.reacted[0].emoji)
	assert.Empty(t, hook.unreacted)
}

// ローカルカスタム絵文字なら emoji 名 `name@.` と publicUrl が wire 化される。
func TestService_NoteStreamHook_OnCreate_LocalCustomEmoji(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	emojiRepo.Emojis["smile@"] = &model.Emoji{
		Name:        "smile",
		PublicURL:   "https://example.test/emoji/smile.webp",
		OriginalURL: "https://example.test/emoji/smile-orig.webp",
	}
	hook := &recordingNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":smile:")
	require.NoError(t, err)
	require.Len(t, hook.reacted, 1)
	assert.Equal(t, ":smile@.:", hook.reacted[0].reaction)
	require.NotNil(t, hook.reacted[0].emoji)
	assert.Equal(t, "smile@.", hook.reacted[0].emoji.Name)
	assert.Equal(t, "https://example.test/emoji/smile.webp", hook.reacted[0].emoji.URL)
}

// publicUrl 空なら originalUrl にフォールバックする (TS upstream 互換)。
func TestService_NoteStreamHook_OnCreate_PublicURLFallbackToOriginal(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	emojiRepo.Emojis["smile@"] = &model.Emoji{
		Name:        "smile",
		PublicURL:   "",
		OriginalURL: "https://example.test/emoji/smile-orig.webp",
	}
	hook := &recordingNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":smile:")
	require.NoError(t, err)
	require.Len(t, hook.reacted, 1)
	require.NotNil(t, hook.reacted[0].emoji)
	assert.Equal(t, "https://example.test/emoji/smile-orig.webp", hook.reacted[0].emoji.URL)
}

// リモートカスタム絵文字は `name@host` で wire 化される。
func TestService_NoteStreamHook_OnCreate_RemoteCustomEmoji(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	host := "remote.example"
	emojiRepo.Emojis["smile@remote.example"] = &model.Emoji{
		Name:      "smile",
		Host:      &host,
		PublicURL: "https://remote.example/emoji/smile.webp",
	}
	hook := &recordingNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":smile@remote.example:")
	require.NoError(t, err)
	require.Len(t, hook.reacted, 1)
	assert.Equal(t, ":smile@remote.example:", hook.reacted[0].reaction)
	require.NotNil(t, hook.reacted[0].emoji)
	assert.Equal(t, "smile@remote.example", hook.reacted[0].emoji.Name)
}

// Delete 経路で `unreacted` が `:name@.:` 形式で publish される。
func TestService_NoteStreamHook_OnDelete(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	emojiRepo.Emojis["smile@"] = &model.Emoji{Name: "smile"}
	hook := &recordingNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":smile:")
	require.NoError(t, err)
	require.NoError(t, svc.Delete(&model.User{ID: "viewer"}, "n1"))
	require.Len(t, hook.unreacted, 1)
	assert.Equal(t, "n1", hook.unreacted[0].noteID)
	assert.Equal(t, "viewer", hook.unreacted[0].userID)
	assert.Equal(t, ":smile@.:", hook.unreacted[0].reaction)
}

// #2106 N16: 同じ user が別 reaction に置き換えた場合、新 reaction の reacted と
// 古い reaction の unreacted を両方発火する (upstream ReactionService.create は置き換えを
// delete() 経由で行い unreacted を publish する)。旧テストは「unreacted は走らない」と
// 誤った upstream 互換を主張していた。
func TestService_NoteStreamHook_OnReplace_PublishesBoth(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	_, err := svc.Create(&model.User{ID: "viewer"}, "n1", "👍")
	require.NoError(t, err)
	_, err = svc.Create(&model.User{ID: "viewer"}, "n1", "🎉")
	require.NoError(t, err)
	require.Len(t, hook.reacted, 2)
	assert.Equal(t, "👍", hook.reacted[0].reaction)
	assert.Equal(t, "🎉", hook.reacted[1].reaction)
	require.Len(t, hook.unreacted, 1, "置き換えで古い reaction の unreacted を publish")
	assert.Equal(t, "👍", hook.unreacted[0].reaction)
}

// 古い `:name:` 形式 (TS-era DB レコード) で保存された reaction を Delete
// した場合も canonical `:name@.:` で wire 化される。
func TestService_NoteStreamHook_OnDelete_LegacyShortForm(t *testing.T) {
	svc, noteRepo, reactRepo, _, _ := newService(t)
	seedNote(noteRepo, "n1", "author", model.NoteVisibilityPublic)
	// Delete 経路は reactRepo.FindByPair で legacy `:smile:` 形式の record を
	// 拾う想定。canonical 化は decodeReactionForStream が担う。
	reactRepo.Reactions["legacy"] = &model.NoteReaction{
		ID:       "legacy",
		UserID:   "viewer",
		NoteID:   "n1",
		Reaction: ":smile:",
	}
	hook := &recordingNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	require.NoError(t, svc.Delete(&model.User{ID: "viewer"}, "n1"))
	require.Len(t, hook.unreacted, 1)
	assert.Equal(t, ":smile@.:", hook.unreacted[0].reaction)
}

// --- #1538: reactionAcceptance + role/sensitive/media-silence gates ---

type stubRoles struct{ byUser map[string][]*model.Role }

func (s stubRoles) GetUserRoles(id string) ([]*model.Role, error) { return s.byUser[id], nil }

type stubMediaSilence struct{ hosts map[string]bool }

func (s stubMediaSilence) IsMediaSilenced(host string) bool { return s.hosts[host] }

// seedLocalEmoji registers a local (host nil) custom emoji under the mock key.
func seedLocalEmoji(repo *testutil.MockEmojiRepository, name string, sensitive bool, roleIDs []string) {
	repo.Emojis[name+"@"] = &model.Emoji{
		ID: "e_" + name, Name: name, IsSensitive: sensitive,
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray(roleIDs),
	}
}

func reactionStored(reactRepo *testutil.MockNoteReactionRepository) string {
	for _, rec := range reactRepo.Reactions {
		return rec.Reaction
	}
	return ""
}

func withAcceptance(n *model.Note, acc string) *model.Note { n.ReactionAcceptance = &acc; return n }

func TestService_Create_LikeOnlyForcesHeart(t *testing.T) {
	svc, repo, reactRepo, emojiRepo, _ := newService(t)
	seedLocalEmoji(emojiRepo, "custom", false, nil)
	withAcceptance(seedNote(repo, "n1", "author", model.NoteVisibilityPublic), "likeOnly")
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":custom:")
	require.NoError(t, err)
	assert.Equal(t, reaction.FallbackReaction, r, "likeOnly note must coerce custom emoji to heart")
	assert.Equal(t, reaction.FallbackReaction, reactionStored(reactRepo))
}

func TestService_Create_LikeOnlyForRemote(t *testing.T) {
	host := "remote.example"
	t.Run("remote reactor -> heart", func(t *testing.T) {
		svc, repo, _, emojiRepo, _ := newService(t)
		emojiRepo.Emojis["custom@remote.example"] = &model.Emoji{ID: "e_r", Name: "custom"}
		withAcceptance(seedNote(repo, "n1", "author", model.NoteVisibilityPublic), "likeOnlyForRemote")
		r, err := svc.Create(&model.User{ID: "ruser", Host: &host}, "n1", ":custom:")
		require.NoError(t, err)
		assert.Equal(t, reaction.FallbackReaction, r)
	})
	t.Run("local reactor -> emoji kept", func(t *testing.T) {
		svc, repo, _, emojiRepo, _ := newService(t)
		seedLocalEmoji(emojiRepo, "custom", false, nil)
		withAcceptance(seedNote(repo, "n1", "author", model.NoteVisibilityPublic), "likeOnlyForRemote")
		r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":custom:")
		require.NoError(t, err)
		assert.Equal(t, ":custom@.:", r)
	})
}

func TestService_Create_NonSensitiveOnly(t *testing.T) {
	t.Run("sensitive emoji -> heart", func(t *testing.T) {
		svc, repo, _, emojiRepo, _ := newService(t)
		seedLocalEmoji(emojiRepo, "sens", true, nil)
		withAcceptance(seedNote(repo, "n1", "author", model.NoteVisibilityPublic), "nonSensitiveOnly")
		r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":sens:")
		require.NoError(t, err)
		assert.Equal(t, reaction.FallbackReaction, r)
	})
	t.Run("non-sensitive emoji -> kept", func(t *testing.T) {
		svc, repo, _, emojiRepo, _ := newService(t)
		seedLocalEmoji(emojiRepo, "ok", false, nil)
		withAcceptance(seedNote(repo, "n1", "author", model.NoteVisibilityPublic), "nonSensitiveOnly")
		r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":ok:")
		require.NoError(t, err)
		assert.Equal(t, ":ok@.:", r)
	})
}

func TestService_Create_RoleGatedEmoji(t *testing.T) {
	t.Run("reactor without role -> heart", func(t *testing.T) {
		svc, repo, _, emojiRepo, _ := newService(t)
		seedLocalEmoji(emojiRepo, "vip", false, []string{"role-vip"})
		svc.SetUserRolesProvider(stubRoles{byUser: map[string][]*model.Role{}})
		seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
		r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":vip:")
		require.NoError(t, err)
		assert.Equal(t, reaction.FallbackReaction, r)
	})
	t.Run("reactor with role -> kept", func(t *testing.T) {
		svc, repo, _, emojiRepo, _ := newService(t)
		seedLocalEmoji(emojiRepo, "vip", false, []string{"role-vip"})
		svc.SetUserRolesProvider(stubRoles{byUser: map[string][]*model.Role{"viewer": {{ID: "role-vip"}}}})
		seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
		r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":vip:")
		require.NoError(t, err)
		assert.Equal(t, ":vip@.:", r)
	})
}

func TestService_Create_MediaSilencedHostHeart(t *testing.T) {
	host := "media.bad"
	svc, repo, _, emojiRepo, _ := newService(t)
	emojiRepo.Emojis["custom@media.bad"] = &model.Emoji{ID: "e_m", Name: "custom"}
	svc.SetMediaSilenceChecker(stubMediaSilence{hosts: map[string]bool{"media.bad": true}})
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	r, err := svc.Create(&model.User{ID: "ruser", Host: &host}, "n1", ":custom:")
	require.NoError(t, err)
	assert.Equal(t, reaction.FallbackReaction, r, "custom emoji reaction from media-silenced host must be heart")
}

// regression: no reactionAcceptance + unwired deps -> custom emoji kept (the
// default reaction path is unchanged by #1538).
func TestService_Create_NoAcceptanceKeepsEmoji(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedLocalEmoji(emojiRepo, "custom", true, []string{"role-x"}) // sensitive + role-gated, but no acceptance/deps
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":custom:")
	require.NoError(t, err)
	assert.Equal(t, ":custom@.:", r)
}

// #2106 N17: 未キャッシュの remote custom emoji で filter しても (❤ に化けず) その
// reaction だけを返す。旧実装は emojiRepo に無い remote emoji を ❤ fallback していた。
func TestService_List_RemoteCustomEmojiFilter(t *testing.T) {
	svc, noteRepo, reactRepo, _, _ := newService(t)
	seedNote(noteRepo, "n1", "author", model.NoteVisibilityPublic)
	// emojiRepo には foo@remote.example を入れない (= 未キャッシュ remote emoji)
	reactRepo.Reactions["rx_remote"] = &model.NoteReaction{
		ID: "rx_remote", UserID: "u1", NoteID: "n1", Reaction: ":foo@remote.example:",
	}
	reactRepo.Reactions["rx_heart"] = &model.NoteReaction{
		ID: "rx_heart", UserID: "u2", NoteID: "n1", Reaction: "❤",
	}

	out, err := svc.List(nil, "n1", "", "", 10, ":foo@remote.example:")
	require.NoError(t, err)
	require.Len(t, out, 1, "remote custom emoji filter は該当 reaction のみ返す (❤ に化けない)")
	assert.Equal(t, ":foo@remote.example:", out[0].Reaction)
}

// zeroAffectedDeleteRepo simulates a concurrent delete: FindByPair finds the row
// (embedded mock) but Delete reports 0 affected rows (already removed by a racer).
type zeroAffectedDeleteRepo struct {
	*testutil.MockNoteReactionRepository
}

func (z *zeroAffectedDeleteRepo) Delete(_ *model.NoteReaction) (int64, error) {
	return 0, nil
}

// #2106 L40: 並行 delete で affected==0 のときは ErrReactionNotFound を返し、カウントを
// 二重デクリメントしない (upstream の affected!==1 ガード相当)。
func TestService_Delete_ConcurrentZeroAffected(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	seedNote(noteRepo, "n1", "author", model.NoteVisibilityPublic)
	mock := testutil.NewMockNoteReactionRepository()
	mock.Reactions["existing"] = &model.NoteReaction{
		ID: "existing", UserID: "viewer", NoteID: "n1", Reaction: "👍",
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(
		noteRepo,
		&zeroAffectedDeleteRepo{MockNoteReactionRepository: mock},
		testutil.NewMockEmojiRepository(),
		testutil.NewMockFollowingRepository(),
		idGen,
	)
	err := svc.Delete(&model.User{ID: "viewer"}, "n1")
	assert.ErrorIs(t, err, reaction.ErrReactionNotFound)
}

// #3201: 取り消したリアクションの通知を消す。
func TestService_NotificationHook_OnDeleteRemovesNotification(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingNotificationHook{}
	svc.SetNotificationHook(hook)
	viewer := &model.User{ID: "viewer"}

	_, err := svc.Create(viewer, "n1", "👍")
	require.NoError(t, err)
	require.NoError(t, svc.Delete(viewer, "n1"))
	assert.Equal(t, []string{"created author/viewer/n1/👍", "removed author/viewer/n1"}, hook.events)

	// 取り消し済みをもう一度取り消しても (並行 unreact / 存在しない) 何もしない。
	require.Error(t, svc.Delete(viewer, "n1"))
	assert.Len(t, hook.events, 2)
}

// 付け替えは古い通知を消してから新しい通知を作る。残すと付け替えのたびに増える。
func TestService_NotificationHook_ReplaceRemovesOldNotification(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingNotificationHook{}
	svc.SetNotificationHook(hook)
	viewer := &model.User{ID: "viewer"}

	_, err := svc.Create(viewer, "n1", "👍")
	require.NoError(t, err)
	_, err = svc.Create(viewer, "n1", "🎉")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"created author/viewer/n1/👍",
		"removed author/viewer/n1",
		"created author/viewer/n1/🎉",
	}, hook.events)
}

// 自分のノートへのリアクションは通知を作っていないので、消しにも行かない。
func TestService_NotificationHook_SelfReactionIsNotTouched(t *testing.T) {
	svc, repo, _, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	hook := &recordingNotificationHook{}
	svc.SetNotificationHook(hook)
	author := &model.User{ID: "author"}

	_, err := svc.Create(author, "n1", "👍")
	require.NoError(t, err)
	_, err = svc.Create(author, "n1", "🎉")
	require.NoError(t, err)
	require.NoError(t, svc.Delete(author, "n1"))
	assert.Empty(t, hook.events)
}
