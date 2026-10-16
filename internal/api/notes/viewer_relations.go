package notes

import (
	"fmt"

	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// SetRelationCache wires the per-user relation cache (#3607). The timeline
// and the post-fetch mute / block filter read the viewer's mute, block,
// follow and channel lists from it instead of the repositories. nil keeps
// reading the repositories.
func (h *Handler) SetRelationCache(rc *repository.RelationCache) {
	h.relationCache = rc
}

// requestRelations loads each of one viewer's relation lists at most once
// during a request (#3607).
//
// タイムラインは TimelineFilter に載せる一覧と、取得後の mute / block の判定
// (newMuteBlockFilter) とで、同じミュート・ブロック・チャンネルのミュートを
// 別々に引いていた。ここに 1 回だけ読んで両方へ渡す。一覧が 1 リクエストの中で
// 変わらないのは以前と同じ。
//
// 失敗も覚える。TimelineFilter 側は失敗を握り潰して空にし (以前からの
// best-effort)、取得後の判定は同じ失敗を受けて 500 にする (fail-closed)。
//
// 並行に呼ばない (newMuteBlockFilter と同じ前提)。
type requestRelations struct {
	h      *Handler
	viewer *model.User

	muted, renoteMuted, blockers, followees, mutedChannels, followedChannels relationList
}

type relationList struct {
	loaded bool
	ids    []string
	err    error
}

func (l *relationList) get(load func() ([]string, error)) ([]string, error) {
	if !l.loaded {
		l.ids, l.err = load()
		l.loaded = true
	}
	return l.ids, l.err
}

func (h *Handler) newRequestRelations(viewer *model.User) *requestRelations {
	return &requestRelations{h: h, viewer: viewer}
}

// mutedUserIDs returns the viewer's active mutes. nil viewer / unwired repo
// yields nil.
func (r *requestRelations) mutedUserIDs() ([]string, error) {
	if r.viewer == nil || r.h.mutingRepo == nil {
		return nil, nil
	}
	return r.muted.get(func() ([]string, error) {
		if rc := r.h.relationCache; rc != nil {
			return rc.MutedUserIDs(r.viewer.ID)
		}
		return r.h.mutingRepo.ListMuteeIDs(r.viewer.ID)
	})
}

func (r *requestRelations) renoteMutedUserIDs() ([]string, error) {
	if r.viewer == nil || r.h.renoteMutingRepo == nil {
		return nil, nil
	}
	return r.renoteMuted.get(func() ([]string, error) {
		if rc := r.h.relationCache; rc != nil {
			return rc.RenoteMutedUserIDs(r.viewer.ID)
		}
		return r.h.renoteMutingRepo.ListMuteeIDs(r.viewer.ID)
	})
}

func (r *requestRelations) blockerIDs() ([]string, error) {
	if r.viewer == nil || r.h.blockingRepo == nil {
		return nil, nil
	}
	return r.blockers.get(func() ([]string, error) {
		if rc := r.h.relationCache; rc != nil {
			return rc.BlockerIDs(r.viewer.ID)
		}
		return r.h.blockingRepo.ListBlockerIDs(r.viewer.ID)
	})
}

func (r *requestRelations) followeeIDs() ([]string, error) {
	if r.viewer == nil || r.h.userFollowingRepo == nil {
		return nil, nil
	}
	return r.followees.get(func() ([]string, error) {
		if rc := r.h.relationCache; rc != nil {
			return rc.FolloweeIDs(r.viewer.ID)
		}
		return r.h.userFollowingRepo.ListFolloweeIDs(r.viewer.ID)
	})
}

func (r *requestRelations) mutedChannelIDs() ([]string, error) {
	if r.viewer == nil || r.h.channelMutingRepo == nil {
		return nil, nil
	}
	return r.mutedChannels.get(func() ([]string, error) {
		if rc := r.h.relationCache; rc != nil {
			return rc.MutedChannelIDs(r.viewer.ID)
		}
		rows, err := r.h.channelMutingRepo.ListByUser(r.viewer.ID)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, nil
		}
		ids := make([]string, 0, len(rows))
		for _, m := range rows {
			ids = append(ids, m.ChannelID)
		}
		return ids, nil
	})
}

// followedChannelIDsRaw returns the followed channels before removing the
// muted ones.
func (r *requestRelations) followedChannelIDsRaw() ([]string, error) {
	if r.viewer == nil || r.h.channelFollowingRepo == nil {
		return nil, nil
	}
	return r.followedChannels.get(func() ([]string, error) {
		if rc := r.h.relationCache; rc != nil {
			return rc.FollowedChannelIDs(r.viewer.ID)
		}
		// 1 user の follow channel 数は通常少ないが、暴走防止に上限で打ち切る。
		const pageSize = 100
		const maxFollowedChannels = 500
		var followed []string
		for offset := 0; offset < maxFollowedChannels; offset += pageSize {
			rows, err := r.h.channelFollowingRepo.ListFollowed(r.viewer.ID, "", "", pageSize, offset)
			if err != nil {
				return nil, err
			}
			for _, f := range rows {
				followed = append(followed, f.FolloweeID)
			}
			if len(rows) < pageSize {
				break
			}
		}
		return followed, nil
	})
}

// listers adapts the memoized lists to notesfilter.LoadMuteBlockSets. A
// dimension whose repository is unwired stays nil, as before.
func (r *requestRelations) listers() (notesfilter.MuteeIDLister, notesfilter.BlockerIDLister, notesfilter.ChannelMuteLister) {
	var (
		m  notesfilter.MuteeIDLister
		b  notesfilter.BlockerIDLister
		cm notesfilter.ChannelMuteLister
	)
	if r.h.mutingRepo != nil {
		m = memoMutees{r}
	}
	if r.h.blockingRepo != nil {
		b = memoBlockers{r}
	}
	if r.h.channelMutingRepo != nil {
		cm = memoChannelMutes{r}
	}
	return m, b, cm
}

// memo* は LoadMuteBlockSets が渡す ID を使わず、この要求の閲覧者の一覧を返す。
// 違う ID で呼ばれたら取り違えなので、黙って返さずにエラーにする。

type memoMutees struct{ r *requestRelations }

func (m memoMutees) ListMuteeIDs(muterID string) ([]string, error) {
	if err := m.r.checkViewer(muterID); err != nil {
		return nil, err
	}
	return m.r.mutedUserIDs()
}

type memoBlockers struct{ r *requestRelations }

func (m memoBlockers) ListBlockerIDs(blockeeID string) ([]string, error) {
	if err := m.r.checkViewer(blockeeID); err != nil {
		return nil, err
	}
	return m.r.blockerIDs()
}

type memoChannelMutes struct{ r *requestRelations }

func (m memoChannelMutes) ListByUser(userID string) ([]*model.ChannelMuting, error) {
	if err := m.r.checkViewer(userID); err != nil {
		return nil, err
	}
	ids, err := m.r.mutedChannelIDs()
	if err != nil {
		return nil, err
	}
	rows := make([]*model.ChannelMuting, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, &model.ChannelMuting{UserID: userID, ChannelID: id})
	}
	return rows, nil
}

func (r *requestRelations) checkViewer(userID string) error {
	if r.viewer == nil || r.viewer.ID != userID {
		return fmt.Errorf("request relations: asked for %q, loaded for another viewer", userID)
	}
	return nil
}
