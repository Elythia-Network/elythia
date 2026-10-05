package notesfilter

import "github.com/elythia-network/elythia/internal/model"

// ApplyRenoteChannelMute drops notes that renote/quote a note posted to one of
// the viewer's muted channels, mirroring upstream channels/timeline.ts which
// ANDs `renoteChannelId IS NULL OR renoteChannelId NOT IN (mutedChannelIds)`
// (ignoreAuthorChannelFromMute keeps the current channel out of the muted set).
// The caller must exclude the current channel from mutedChannelIDs (#1790)。
func ApplyRenoteChannelMute(notes []*model.Note, mutedChannelIDs []string) []*model.Note {
	if len(mutedChannelIDs) == 0 {
		return notes
	}
	muted := toSet(mutedChannelIDs)
	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if n.RenoteChannelID != nil {
			if _, ok := muted[*n.RenoteChannelID]; ok {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}
