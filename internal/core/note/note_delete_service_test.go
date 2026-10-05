package note_test

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteService_Success(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["note1"] = &model.Note{ID: "note1", UserID: "user1"}
	svc := note.NewDeleteService(noteRepo)

	err := svc.Delete(&model.User{ID: "user1"}, "note1")
	require.NoError(t, err)
	assert.Empty(t, noteRepo.Notes)
}

// 返信を削除すると親ノートの repliesCount が減ることを確認する。
// 本家 Misskey の NoteDeleteService と同じ振る舞い。
func TestDeleteService_DecrementsParentRepliesCount(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	parentID := "parent1"
	noteRepo.Notes[parentID] = &model.Note{ID: parentID, UserID: "owner", RepliesCount: 3}
	replyID := "reply1"
	parent := parentID
	noteRepo.Notes[replyID] = &model.Note{ID: replyID, UserID: "replier", ReplyID: &parent}

	svc := note.NewDeleteService(noteRepo)
	require.NoError(t, svc.Delete(&model.User{ID: "replier"}, replyID))

	// 親のカウンタが 2 になっているはず
	assert.Equal(t, int16(2), noteRepo.Notes[parentID].RepliesCount)
}

// ReplyID が無い通常ノートを削除しても余計な IncrementCount は走らず、
// 既存ロジックを壊していないことを回帰テストする。
func TestDeleteService_NoParentNoDecrement(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	other := "other"
	noteRepo.Notes[other] = &model.Note{ID: other, UserID: "owner", RepliesCount: 5}
	noteRepo.Notes["standalone"] = &model.Note{ID: "standalone", UserID: "u"}
	svc := note.NewDeleteService(noteRepo)
	require.NoError(t, svc.Delete(&model.User{ID: "u"}, "standalone"))
	assert.Equal(t, int16(5), noteRepo.Notes[other].RepliesCount)
}

func TestDeleteService_NotFound(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	svc := note.NewDeleteService(noteRepo)

	err := svc.Delete(&model.User{ID: "user1"}, "missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, note.ErrNoteNotFound))
}

func TestDeleteService_AccessDenied(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["note1"] = &model.Note{ID: "note1", UserID: "owner"}
	svc := note.NewDeleteService(noteRepo)

	err := svc.Delete(&model.User{ID: "intruder"}, "note1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, note.ErrNoteAccessDenied))
	assert.Len(t, noteRepo.Notes, 1)
}

func TestDeleteService_NilUser(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	svc := note.NewDeleteService(noteRepo)

	err := svc.Delete(nil, "x")
	require.Error(t, err)
}

// failingDeleteRepo wraps MockNoteRepository to fail on Delete only.
type failingDeleteRepo struct {
	*testutil.MockNoteRepository
}

func (r *failingDeleteRepo) Delete(_ *model.Note) error {
	return errors.New("delete failed")
}

func TestDeleteService_RepoDeleteError(t *testing.T) {
	mockRepo := testutil.NewMockNoteRepository()
	mockRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}
	svc := note.NewDeleteService(&failingDeleteRepo{MockNoteRepository: mockRepo})
	err := svc.Delete(&model.User{ID: "user1"}, "n1")
	require.Error(t, err)
}

// recordingDeleteFederationHook captures delete federation calls.
type recordingDeleteFederationHook struct {
	called bool
	author *model.User
	note   *model.Note
}

func (h *recordingDeleteFederationHook) OnNoteDeleted(author *model.User, n *model.Note) {
	h.called = true
	h.author = author
	h.note = n
}

func TestDeleteService_FederationHookInvoked(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingDeleteFederationHook{}
	svc.SetFederationHook(hook)

	require.NoError(t, svc.Delete(&model.User{ID: "user1"}, "n1"))
	assert.True(t, hook.called)
	assert.Equal(t, "n1", hook.note.ID)
}

// recordingModeratorDeleteHook captures deleteNote moderation-log hook firings.
type recordingModeratorDeleteHook struct {
	calls       int
	moderatorID string
	note        *model.Note
	author      *model.User
}

func (h *recordingModeratorDeleteHook) OnModeratorNoteDeleted(moderatorID string, n *model.Note, a *model.User) {
	h.calls++
	h.moderatorID = moderatorID
	h.note = n
	h.author = a
}

// #1765: moderator が他人の note を削除したとき deleteNote modlog hook が発火する。
func TestDeleteService_ModeratorDeleteHook_OtherUsersNote(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author1"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingModeratorDeleteHook{}
	svc.SetModeratorDeleteHook(hook)

	require.NoError(t, svc.DeleteAs(&model.User{ID: "mod1"}, true, "n1"))
	assert.Equal(t, 1, hook.calls)
	assert.Equal(t, "mod1", hook.moderatorID)
	require.NotNil(t, hook.note)
	assert.Equal(t, "n1", hook.note.ID)
	require.NotNil(t, hook.author)
	assert.Equal(t, "author1", hook.author.ID)
}

// 自分の note 削除 / 著者本人の削除 (isModerator=false) では発火しない。
func TestDeleteService_ModeratorDeleteHook_NotFiredForSelfOrAuthor(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["own"] = &model.Note{ID: "own", UserID: "mod1"}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "user2"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingModeratorDeleteHook{}
	svc.SetModeratorDeleteHook(hook)

	// moderator が自分の note を削除 (note.userId == actor) → 発火しない。
	require.NoError(t, svc.DeleteAs(&model.User{ID: "mod1"}, true, "own"))
	// 著者本人の削除 (Delete = isModerator false) → 発火しない。
	require.NoError(t, svc.Delete(&model.User{ID: "user2"}, "n2"))
	assert.Equal(t, 0, hook.calls)
}

// recordingDeleteIndexHook captures index hook calls so we can check the
// search-index hookup wired into DeleteService.
type recordingDeleteIndexHook struct {
	deleted *model.Note
}

func (h *recordingDeleteIndexHook) OnNoteCreated(_ *model.Note) {}
func (h *recordingDeleteIndexHook) OnNoteDeleted(n *model.Note) { h.deleted = n }

func TestDeleteService_IndexHookInvoked(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingDeleteIndexHook{}
	svc.SetIndexHook(hook)

	require.NoError(t, svc.Delete(&model.User{ID: "user1"}, "n1"))
	require.NotNil(t, hook.deleted)
	assert.Equal(t, "n1", hook.deleted.ID)
}

// recordingDeleteChartHook captures chart hook firings on the delete
// path so we can check the chart subsystem hookup.
type recordingDeleteChartHook struct {
	deleted *model.Note
}

func (h *recordingDeleteChartHook) OnNoteDeleted(n *model.Note) { h.deleted = n }

func TestDeleteService_ChartHookInvoked(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingDeleteChartHook{}
	svc.SetChartHook(hook)

	require.NoError(t, svc.Delete(&model.User{ID: "user1"}, "n1"))
	require.NotNil(t, hook.deleted)
	assert.Equal(t, "n1", hook.deleted.ID)
}

// recordingDeleteTimelineHook captures timeline-purge hook calls (#379)。
type recordingDeleteTimelineHook struct {
	called bool
	note   *model.Note
	author *model.User
}

func (h *recordingDeleteTimelineHook) OnNoteDeleted(n *model.Note, author *model.User) {
	h.called = true
	h.note = n
	h.author = author
}

func TestDeleteService_TimelineHookInvoked(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingDeleteTimelineHook{}
	svc.SetTimelineHook(hook)

	require.NoError(t, svc.Delete(&model.User{ID: "user1"}, "n1"))
	assert.True(t, hook.called)
	require.NotNil(t, hook.note)
	assert.Equal(t, "n1", hook.note.ID)
	require.NotNil(t, hook.author)
	assert.Equal(t, "user1", hook.author.ID)
}

// recordingDeleteNoteStreamHook captures noteStream publish hook calls (#700)。
type recordingDeleteNoteStreamHook struct {
	called    bool
	noteID    string
	deletedAt time.Time
}

func (h *recordingDeleteNoteStreamHook) OnNoteDeleted(noteID string, deletedAt time.Time) {
	h.called = true
	h.noteID = noteID
	h.deletedAt = deletedAt
}

// 削除に成功すると `noteStream:<id>` 用の hook が `deleted` payload を
// 1 度だけ受け取ることを確認する。
func TestDeleteService_NoteStreamHookInvoked(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingDeleteNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	before := time.Now()
	require.NoError(t, svc.Delete(&model.User{ID: "user1"}, "n1"))
	after := time.Now()
	assert.True(t, hook.called)
	assert.Equal(t, "n1", hook.noteID)
	// deletedAt は呼び出し前後の time.Now() の間に収まる。
	assert.False(t, hook.deletedAt.Before(before))
	assert.False(t, hook.deletedAt.After(after))
}

// 削除が失敗 (権限なし) した場合 noteStream hook は呼ばれない。
func TestDeleteService_NoteStreamHookNotFiredOnFailure(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "owner"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingDeleteNoteStreamHook{}
	svc.SetNoteStreamHook(hook)

	err := svc.Delete(&model.User{ID: "intruder"}, "n1")
	require.Error(t, err)
	assert.False(t, hook.called)
}

// captureTimelineHook records the author passed to OnNoteDeleted so tests can
// assert moderator deletes attribute the note to its author, not the moderator.
type captureTimelineHook struct {
	author *model.User
	note   *model.Note
}

func (c *captureTimelineHook) OnNoteDeleted(n *model.Note, author *model.User) {
	c.author = author
	c.note = n
}

func TestDeleteService_ModeratorDeletesOthersNote(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["author"] = &model.User{ID: "author", Username: "author"}
	hook := &captureTimelineHook{}
	svc := note.NewDeleteService(noteRepo)
	svc.SetUserRepo(userRepo)
	svc.SetTimelineHook(hook)

	require.NoError(t, svc.DeleteAs(&model.User{ID: "mod"}, true, "n1"))
	assert.Empty(t, noteRepo.Notes)
	require.NotNil(t, hook.author)
	assert.Equal(t, "author", hook.author.ID, "delete hooks must receive the note author, not the moderator")
}

func TestDeleteService_NonModeratorNonAuthorDenied(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	svc := note.NewDeleteService(noteRepo)

	err := svc.DeleteAs(&model.User{ID: "intruder"}, false, "n1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, note.ErrNoteAccessDenied))
	assert.Len(t, noteRepo.Notes, 1)
}

func TestDeleteService_AuthorSelfDeleteUsesActor(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author"}
	hook := &captureTimelineHook{}
	svc := note.NewDeleteService(noteRepo)
	svc.SetTimelineHook(hook)

	require.NoError(t, svc.DeleteAs(&model.User{ID: "author"}, false, "n1"))
	require.NotNil(t, hook.author)
	assert.Equal(t, "author", hook.author.ID)
}

// recordingDeleteNotificationHook captures the notes handed to the notification
// hook (#3201).
type recordingDeleteNotificationHook struct{ notes []*model.Note }

func (h *recordingDeleteNotificationHook) OnNoteDeleted(n *model.Note) { h.notes = append(h.notes, n) }

// #3201: 削除したノートを通知の hook に渡す (リノート / 引用の通知を消すため)。
// モデレーターが他人のノートを消した場合も同じ。
func TestDeleteService_NotificationHookInvoked(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	target := "author"
	noteRepo.Notes["rn"] = &model.Note{ID: "rn", UserID: "user1", RenoteUserID: &target}
	noteRepo.Notes["rn2"] = &model.Note{ID: "rn2", UserID: "user2", RenoteUserID: &target}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingDeleteNotificationHook{}
	svc.SetNotificationHook(hook)

	require.NoError(t, svc.Delete(&model.User{ID: "user1"}, "rn"))
	require.NoError(t, svc.DeleteAs(&model.User{ID: "mod"}, true, "rn2"))
	require.Len(t, hook.notes, 2)
	assert.Equal(t, "rn", hook.notes[0].ID)
	assert.Equal(t, "rn2", hook.notes[1].ID)
}

// 削除に失敗したら通知は消さない。
func TestDeleteService_NotificationHookNotFiredOnFailure(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "owner"}
	svc := note.NewDeleteService(noteRepo)
	hook := &recordingDeleteNotificationHook{}
	svc.SetNotificationHook(hook)

	require.Error(t, svc.Delete(&model.User{ID: "intruder"}, "n1"))
	assert.Empty(t, hook.notes)
}
