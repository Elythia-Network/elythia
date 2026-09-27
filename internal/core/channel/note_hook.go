package channel

// NoteCreateHook adapts channel.Service to core/note.ChannelHook so that
// NoteCreateService can validate channel existence and bump counters via the
// service. core/note 側からは小さな interface 経由で参照されるため、ここで
// 型変換 / 単純な転送のみを行う。
type NoteCreateHook struct {
	svc *Service
}

// NewNoteCreateHook constructs a NoteCreateHook bound to the given Service.
func NewNoteCreateHook(svc *Service) *NoteCreateHook {
	return &NoteCreateHook{svc: svc}
}

// EnsureChannelExists implements core/note.ChannelHook. An archived channel
// is reported as ErrChannelNotFound, because it no longer accepts posts.
func (h *NoteCreateHook) EnsureChannelExists(channelID string) error {
	c, err := h.svc.Show(channelID)
	if err != nil {
		return err
	}
	// upstream NoteCreateService は `findOneBy({ id, isArchived: false })` で引くので、
	// アーカイブ済みは NO_SUCH_CHANNEL になる。notes/create・予約投稿の公開
	// (post_scheduled_note)・drafts からの投稿はすべてこの hook を通る。
	if c == nil || c.IsArchived {
		return ErrChannelNotFound
	}
	return nil
}

// OnNotePosted implements core/note.ChannelHook.
func (h *NoteCreateHook) OnNotePosted(channelID, noteID, authorID string) {
	h.svc.OnNotePosted(channelID, noteID, authorID)
}
