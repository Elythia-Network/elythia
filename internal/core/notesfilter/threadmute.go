package notesfilter

import "github.com/elythia-network/elythia/internal/model"

// ApplyThreadMute drops notes belonging to a thread the viewer has muted,
// mirroring upstream QueryService.generateMutedNoteThreadQuery (#1554)。
//
// upstream は `note.id NOT IN (mutedThreadIds)` かつ
// `(note.threadId IS NULL OR note.threadId NOT IN (mutedThreadIds))` で絞る。
// すなわち note.id が muted thread の root であるか、note.threadId が muted
// thread であれば除外する。mutedThreadIDs が空なら no-op。
func ApplyThreadMute(notes []*model.Note, mutedThreadIDs []string) []*model.Note {
	if len(notes) == 0 || len(mutedThreadIDs) == 0 {
		return notes
	}
	muted := make(map[string]struct{}, len(mutedThreadIDs))
	for _, id := range mutedThreadIDs {
		muted[id] = struct{}{}
	}
	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if n == nil {
			continue
		}
		if _, ok := muted[n.ID]; ok {
			continue
		}
		if n.ThreadID != nil {
			if _, ok := muted[*n.ThreadID]; ok {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}
