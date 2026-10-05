package note

import (
	"errors"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Errors returned by DeleteService.
var (
	// ErrNoteNotFound is returned when the target note does not exist.
	ErrNoteNotFound = errors.New("note not found")
	// ErrNoteAccessDenied is returned when the user is not allowed to delete the note.
	ErrNoteAccessDenied = errors.New("not the author of this note")
)

// DeleteFederationHook is invoked after a note is deleted so that the
// ActivityPub layer can broadcast a Delete activity. パッケージ間の循環依存を
// 避けるためinterfaceで受け取る (実装は core/federation)。
type DeleteFederationHook interface {
	OnNoteDeleted(author *model.User, note *model.Note)
}

// DeleteChartHook is invoked after a note is deleted so the chart
// subsystem can decrement the relevant counters. 失敗はベストエフォート
// で握り潰す前提なので戻り値を持たない。
type DeleteChartHook interface {
	OnNoteDeleted(note *model.Note)
}

// DeleteTimelineHook is invoked after a note is deleted so Redis fanout
// timelines (home/local/global/user/userList) can have the note ID
// purged via LREM (#379)。inbound Delete activity 経由でも local
// `notes/delete` 経由でも同じ DeleteService を通るので、ここに hook を
// 1 つ生やせば両方の経路で timeline 残留を防げる。実装は core/timeline。
type DeleteTimelineHook interface {
	OnNoteDeleted(note *model.Note, author *model.User)
}

// DeleteNoteStreamHook is invoked after a note is deleted so the stream
// layer can publish a `deleted` event to `noteStream:<noteID>` for any
// WebSocket clients that have called subNote on this note (#700)。
// 失敗はベストエフォート扱いなので戻り値を持たない。
type DeleteNoteStreamHook interface {
	OnNoteDeleted(noteID string, deletedAt time.Time)
}

// DeleteNotificationHook is invoked after a note is deleted so notifications
// that pointed at it can be removed (#3201)。実装は core/notification.Hook。
// 失敗はベストエフォート。
type DeleteNotificationHook interface {
	OnNoteDeleted(note *model.Note)
}

// ModeratorDeleteHook is invoked when a moderator deletes another user's note
// so the action can be written to the moderation log (upstream
// NoteDeleteService writes a `deleteNote` moderation log when
// `deleter && note.userId !== deleter.id`, #1765)。失敗はベストエフォート。
type ModeratorDeleteHook interface {
	OnModeratorNoteDeleted(moderatorID string, note *model.Note, author *model.User)
}

// DeleteService provides note deletion logic.
type DeleteService struct {
	noteRepo            repository.NoteRepository
	userRepo            repository.UserRepository
	federationHook      DeleteFederationHook
	indexHook           IndexHook
	chartHook           DeleteChartHook
	timelineHook        DeleteTimelineHook
	noteStreamHook      DeleteNoteStreamHook
	moderatorDeleteHook ModeratorDeleteHook
	notificationHook    DeleteNotificationHook
}

// NewDeleteService creates a new DeleteService.
func NewDeleteService(noteRepo repository.NoteRepository) *DeleteService {
	return &DeleteService{noteRepo: noteRepo}
}

// SetUserRepo attaches a UserRepository so DeleteAs can resolve the note's
// author when a moderator deletes someone else's note (#1538), ensuring the
// federation Delete activity / timeline fanout removal are attributed to the
// author rather than the moderator.
func (s *DeleteService) SetUserRepo(r repository.UserRepository) {
	s.userRepo = r
}

// SetFederationHook attaches a DeleteFederationHook invoked after Delete.
func (s *DeleteService) SetFederationHook(h DeleteFederationHook) {
	s.federationHook = h
}

// SetIndexHook attaches an IndexHook invoked after Delete so the search
// backend can drop the note from its index.
func (s *DeleteService) SetIndexHook(h IndexHook) {
	s.indexHook = h
}

// SetChartHook attaches a chart hook invoked after a note has been
// deleted so the chart subsystem can decrement the relevant counters.
func (s *DeleteService) SetChartHook(h DeleteChartHook) {
	s.chartHook = h
}

// SetTimelineHook attaches a DeleteTimelineHook invoked after Delete so
// Redis fanout timelines can be purged of the note ID (#379)。
func (s *DeleteService) SetTimelineHook(h DeleteTimelineHook) {
	s.timelineHook = h
}

// SetModeratorDeleteHook attaches a ModeratorDeleteHook invoked when a
// moderator deletes another user's note so the action is written to the
// moderation log (#1765)。
func (s *DeleteService) SetModeratorDeleteHook(h ModeratorDeleteHook) {
	s.moderatorDeleteHook = h
}

// SetNotificationHook attaches a DeleteNotificationHook invoked after Delete
// so the renote / quote notification the note left is removed (#3201)。
func (s *DeleteService) SetNotificationHook(h DeleteNotificationHook) {
	s.notificationHook = h
}

// SetNoteStreamHook attaches a DeleteNoteStreamHook invoked after Delete so
// the stream layer can publish `deleted` events on `noteStream:<noteID>`
// (#700)。
func (s *DeleteService) SetNoteStreamHook(h DeleteNoteStreamHook) {
	s.noteStreamHook = h
}

// Delete removes a note authored by the given user. It returns
// ErrNoteNotFound when the note does not exist and ErrNoteAccessDenied when
// the user is not the author. Thin wrapper over DeleteAs with isModerator=false
// (the inbound federation path and existing callers keep author-only semantics).
func (s *DeleteService) Delete(user *model.User, noteID string) error {
	return s.DeleteAs(user, false, noteID)
}

// DeleteAs removes a note on behalf of actor. When isModerator is false the
// actor must be the author (ErrNoteAccessDenied otherwise); when true a
// moderator may delete anyone's note, mirroring upstream NoteDeleteService where
// `!isModerator && note.userId !== me.id` rejects. The note's AUTHOR (resolved
// via userRepo when actor != author) is passed to the federation / timeline
// hooks so the AP Delete activity and home-timeline fanout removal stay
// attributed to the author, not the acting moderator (#1538).
func (s *DeleteService) DeleteAs(actor *model.User, isModerator bool, noteID string) error {
	if actor == nil {
		return errors.New("user is required")
	}

	note, err := s.noteRepo.FindByID(noteID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNoteNotFound
	}

	if !isModerator && note.UserID != actor.ID {
		return ErrNoteAccessDenied
	}

	author := s.resolveAuthor(actor, note)

	// 上流 NoteDeleteService が Delete 冒頭で deletedAt を確定させ publish と
	// federation の Delete activity 両方に渡しているのに合わせ、ここで一度
	// 取得して noteStream publish に使う。federation 側は別途自分で時刻を
	// 決めるが、本実装でも publish 直前に再取得すると noteStream の
	// deletedAt と federation activity の timestamp に不要な差が出るため。
	deletedAt := time.Now()

	if err := s.noteRepo.Delete(note); err != nil {
		return err
	}
	// 返信を削除するときは親ノートの repliesCount を減算する。本家 Misskey の
	// NoteDeleteService と同じく親の local/remote を問わず一律に減算する。
	// DeleteService は local/remote どちらの削除経路でも共通で呼ばれるため、
	// ここに置くと federation 側でも同じ動作になる。失敗はベストエフォート。
	if note.ReplyID != nil {
		_ = s.noteRepo.IncrementCount(*note.ReplyID, "repliesCount", -1)
	}
	if s.federationHook != nil {
		s.federationHook.OnNoteDeleted(author, note)
	}
	// 検索インデックスの撤去もベストエフォート。失敗してもメインのDelete操作は成功扱い。
	if s.indexHook != nil {
		s.indexHook.OnNoteDeleted(note)
	}
	// チャート集計もベストエフォート。
	if s.chartHook != nil {
		s.chartHook.OnNoteDeleted(note)
	}
	// Redis fanout timeline 残留を防ぐ (#379)。ベストエフォート。
	if s.timelineHook != nil {
		s.timelineHook.OnNoteDeleted(note, author)
	}
	// noteStream に deleted を publish して subNote 購読中の WebSocket
	// クライアントへ即時反映する (#700)。失敗はベストエフォート。
	if s.noteStreamHook != nil {
		s.noteStreamHook.OnNoteDeleted(note.ID, deletedAt)
	}
	// 取り消したリノート / 削除した引用の通知を消す (#3201)。local の
	// notes/unrenote と連合の Delete(Note) はここを通る。**連合の Undo(Announce) は
	// 通らない** (noteRepo.Delete を直接呼ぶ) ので processor 側で同じ hook を呼ぶ。
	// ベストエフォート。
	if s.notificationHook != nil {
		s.notificationHook.OnNoteDeleted(note)
	}
	// moderator が他人の note を削除したときは moderation log に deleteNote を
	// 残す (upstream NoteDeleteService の deleter && note.userId !== deleter.id、
	// #1765)。自分の note 削除・federation 経路 (isModerator=false) では発火しない。
	if isModerator && note.UserID != actor.ID && s.moderatorDeleteHook != nil {
		s.moderatorDeleteHook.OnModeratorNoteDeleted(actor.ID, note, author)
	}
	return nil
}

// resolveAuthor returns the note's author for the delete hooks. When the actor
// IS the author (the common, author-self-delete case) the actor is returned
// directly; otherwise (moderator deleting someone else's note) the author is
// looked up via userRepo so the federation Delete / timeline fanout stay
// attributed to the author. Falls back to a minimal user carrying the author id
// when userRepo is unwired or the row is missing.
func (s *DeleteService) resolveAuthor(actor *model.User, note *model.Note) *model.User {
	if note.UserID == actor.ID {
		return actor
	}
	if s.userRepo != nil {
		if u, err := s.userRepo.FindByID(note.UserID); err == nil && u != nil {
			return u
		}
	}
	return &model.User{ID: note.UserID}
}
