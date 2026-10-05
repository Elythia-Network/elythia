package notesfilter

import "github.com/elythia-network/elythia/internal/model"

// ApplySuspended drops notes whose author is suspended, mirroring upstream
// QueryService.generateSuspendedUserQueryForNote (applied via
// generateBaseNoteFilteringQuery). It relies on the note's User relation being
// preloaded; notes without a loaded User are kept (the filter cannot evaluate
// suspension without the author row). Applies regardless of viewer, matching
// upstream which filters suspended authors for everyone (#1783).
func ApplySuspended(notes []*model.Note) []*model.Note {
	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if n.User != nil && n.User.IsSuspended {
			continue
		}
		out = append(out, n)
	}
	return out
}
