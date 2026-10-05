package antenna

import "github.com/elythia-network/elythia/internal/model"

// NoteCreateHook adapts antenna.Service to core/note.AntennaHook so that
// NoteCreateService can fan newly-created notes out to matching antennas
// without depending directly on this package.
type NoteCreateHook struct {
	svc *Service
}

// NewNoteCreateHook constructs a NoteCreateHook bound to the given Service.
func NewNoteCreateHook(svc *Service) *NoteCreateHook {
	return &NoteCreateHook{svc: svc}
}

// OnNoteCreated implements core/note.AntennaHook.
func (h *NoteCreateHook) OnNoteCreated(n *model.Note, author *model.User) {
	h.svc.OnNoteCreated(n, author)
}

// IsAntennaHook implements federation.AntennaHook の marker。TimelineFanoutHook
// との取り違えを型で防ぐためだけに存在する (#2743)。
func (h *NoteCreateHook) IsAntennaHook() {}
