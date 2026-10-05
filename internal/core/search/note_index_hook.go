package search

import "github.com/elythia-network/elythia/internal/model"

// NoteIndexHook adapts core/search.Service to the note.IndexHook interface
// expected by core/note.CreateService and core/note.DeleteService. It
// forwards Created / Deleted events to the wrapped Service so the
// configured Provider can update its full-text index.
//
// All operations are best-effort: errors returned by the provider are
// silently dropped because note creation / deletion must not fail because of
// a search-index hiccup.  Real backends are expected to log internally.
type NoteIndexHook struct {
	svc *Service
}

// NewNoteIndexHook builds a new adapter around the given Service.
func NewNoteIndexHook(svc *Service) *NoteIndexHook {
	return &NoteIndexHook{svc: svc}
}

// OnNoteCreated forwards to Service.IndexNote, dropping any error.
func (h *NoteIndexHook) OnNoteCreated(n *model.Note) {
	if h == nil || h.svc == nil || n == nil {
		return
	}
	_ = h.svc.IndexNote(n)
}

// OnNoteDeleted forwards to Service.UnindexNote, dropping any error.
func (h *NoteIndexHook) OnNoteDeleted(n *model.Note) {
	if h == nil || h.svc == nil || n == nil {
		return
	}
	_ = h.svc.UnindexNote(n)
}
