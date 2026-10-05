package notesfilter

import (
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// FilterVisible drops notes the viewer is not allowed to see, per
// core/note.CanSeeNote. Used for bounded note sets (featured / pinned / clip)
// where post-fetch filtering is acceptable; deeply paginated endpoints should
// push visibility into SQL instead to avoid under-filled pages.
//
// followingRepo == nil の場合、followers visibility は投稿者本人以外には
// 見せない (fail-closed)。
func FilterVisible(viewer *model.User, notes []*model.Note, followingRepo repository.FollowingRepository) []*model.Note {
	if len(notes) == 0 {
		return notes
	}
	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if corenote.CanSeeNote(viewer, n, followingRepo) {
			out = append(out, n)
		}
	}
	return out
}
