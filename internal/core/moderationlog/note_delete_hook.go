package moderationlog

import (
	"context"

	"github.com/elythia-network/elythia/internal/model"
)

// NoteDeleteHook adapts the moderation-log Service to core/note's
// ModeratorDeleteHook, writing a `deleteNote` entry when a moderator deletes
// another user's note. Mirrors upstream NoteDeleteService which logs
// `deleteNote` when `deleter && note.userId !== deleter.id` (#1765).
type NoteDeleteHook struct {
	svc *Service
}

// NewNoteDeleteHook creates a NoteDeleteHook backed by svc.
func NewNoteDeleteHook(svc *Service) *NoteDeleteHook {
	return &NoteDeleteHook{svc: svc}
}

// OnModeratorNoteDeleted writes the deleteNote moderation log entry.
// Best-effort: the moderation-log Service swallows its own errors.
func (h *NoteDeleteHook) OnModeratorNoteDeleted(moderatorID string, note *model.Note, author *model.User) {
	if h.svc == nil || note == nil {
		return
	}
	// upstream は {noteId, noteUserId, noteUserUsername, noteUserHost, note} を
	// 記録する。mk-go は note 本体の埋め込みは省き、追跡に必要な scalar のみ残す。
	info := map[string]any{
		"noteId":       note.ID,
		"noteUserId":   note.UserID,
		"noteUserHost": note.UserHost,
	}
	if author != nil {
		info["noteUserUsername"] = author.Username
	}
	h.svc.Log(context.Background(), moderatorID, LogDeleteNote, info)
}
