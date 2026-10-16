package notification

import (
	"errors"
	"sync"

	"github.com/elythia-network/elythia/internal/model"
)

// errNoNoteSource is returned by a nil NoteSource.
var errNoNoteSource = errors.New("notification: no note source")

// NoteSource yields the note a notification references, reading it at most
// once and sharing the result between the streaming payload and the Web Push
// payload of the same notification (#3611).
//
// 以前は同じ通知の stream の pack と Web Push の pack がそれぞれ
// FindByIDWithRelations で同じノートを読み直していた (リアクション 1 回で
// note + poll + user の組が 3 回)。本家の NotificationService も、作成時に
// 1 回 pack した結果を stream と push の両方に使う。
//
// The note it returns is shared: callers must treat it as read-only.
type NoteSource struct {
	once sync.Once
	load func() (*model.Note, error)
	note *model.Note
	err  error
}

// NewNoteSource returns a source that calls load on first use and caches the
// result (including an error) for every later caller.
func NewNoteSource(load func() (*model.Note, error)) *NoteSource {
	return &NoteSource{load: load}
}

// LoadedNoteSource returns a source for a note the caller already holds with
// the relations the packers read (author, renote / reply targets, poll).
func LoadedNoteSource(n *model.Note) *NoteSource {
	s := &NoteSource{note: n}
	s.once.Do(func() {})
	return s
}

// Note returns the referenced note, loading it on the first call.
func (s *NoteSource) Note() (*model.Note, error) {
	if s == nil {
		return nil, errNoNoteSource
	}
	s.once.Do(func() {
		if s.load == nil {
			s.err = errNoNoteSource
			return
		}
		s.note, s.err = s.load()
		if s.err == nil && s.note == nil {
			s.err = errNoNoteSource
		}
	})
	return s.note, s.err
}

// hydratedForPack reports whether n already carries every relation the note
// packer expands, so it can be packed without being read again: the author and
// poll of the note, of its renote (packed with detail) and of the renote's
// renote, and the authors of the reply embeds (packed without detail, so no
// poll and no children).
//
// 呼び出し元が持っているノートを使い回すのは、packer が読む relation が揃って
// いるときだけにする。揃っていないノートを渡すと、user や引用先が欠けた形で
// 通知が届く (読み直していた頃は欠けなかった)。作者の行が消えているなどで
// 読み直しても埋まらない relation は、1 回余分に読むだけで結果は変わらない。
func hydratedForPack(n *model.Note) bool {
	if !hasDetailRelations(n) {
		return false
	}
	if n.ReplyID != nil && !hasAuthor(n.Reply) {
		return false
	}
	if n.RenoteID != nil {
		r := n.Renote
		if !hasDetailRelations(r) {
			return false
		}
		if r.RenoteID != nil && !hasDetailRelations(r.Renote) {
			return false
		}
		if r.ReplyID != nil && !hasAuthor(r.Reply) {
			return false
		}
	}
	return true
}

// hasDetailRelations reports whether a note packed with detail has its author
// and, when it has a poll, the poll.
func hasDetailRelations(n *model.Note) bool {
	if !hasAuthor(n) {
		return false
	}
	return !n.HasPoll || n.Poll != nil
}

func hasAuthor(n *model.Note) bool {
	return n != nil && n.User != nil
}

// PrimaryNoteIDChecker reports which of ids exist, reading the primary so a
// note deleted a moment ago is not seen through a lagging replica
// (repository.NoteRepository.ExistingNoteIDsOnPrimary, #3611).
type PrimaryNoteIDChecker interface {
	ExistingNoteIDsOnPrimary(ids []string) ([]string, error)
}
