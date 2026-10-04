package userrelation

import (
	"sync"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// ApplyMany is Apply for every user of a list response: details[i] receives
// the relation block from viewerID to targets[i] (profiles[i] may be nil). It
// returns, per index, whether the viewer follows the target. The three slices
// must have the same length; a nil detailed or target is skipped.
//
// The written fields are exactly the ones Apply writes for the same state
// (self entries get only the memo, an anonymous viewer gets nothing). Each
// relation is read with one IN query over all targets when the repository
// supports it (upstream UserEntityService.getRelations), and falls back to
// the per-target query otherwise.
//
// 利用者ごとに Apply すると 1 人あたり 10 回の問い合わせになり、100 人の一覧で
// 1000 回を超える (#3330)。本家 packMany は getRelations で閲覧者の関係を
// まとめて引くので、同じく関係ごとに 1 回へまとめる。
func (r Repos) ApplyMany(viewerID string, details []*entity.UserDetailed, targets []*model.User, profiles []*model.UserProfile) []bool {
	out := make([]bool, len(details))
	if viewerID == "" || len(details) == 0 || len(targets) != len(details) || len(profiles) != len(details) {
		return out
	}
	ids := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for i, t := range targets {
		if t == nil || details[i] == nil {
			continue
		}
		if _, ok := seen[t.ID]; ok {
			continue
		}
		seen[t.ID] = struct{}{}
		ids = append(ids, t.ID)
	}
	if len(ids) == 0 {
		return out
	}
	rels := r.loadMany(viewerID, ids)
	for i, t := range targets {
		d := details[i]
		if t == nil || d == nil {
			continue
		}
		rel := rels[t.ID]
		if t.ID == viewerID {
			// Apply と同じく、自分自身には関係のブロックを付けずメモだけ載せる。
			if r.Memo != nil && rel.memo != nil {
				memo := rel.memo.Memo
				d.Memo = &memo
			}
			continue
		}
		out[i] = r.write(d, rel, profiles[i])
	}
	return out
}

// loadMany reads the relation state from viewerID to every id. Each relation
// is one goroutine, so the batched queries run concurrently like Apply's.
func (r Repos) loadMany(viewerID string, ids []string) map[string]relation {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[string]relation, len(ids))
	)
	// set は ids の関係の 1 項目を書き込む。goroutine ごとに別の項目を書くが、
	// map は共有なのでまとめて排他する。
	set := func(hit map[string]bool, assign func(*relation)) {
		mu.Lock()
		defer mu.Unlock()
		for id := range hit {
			rel := out[id]
			assign(&rel)
			out[id] = rel
		}
	}
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	if r.Following != nil {
		run(func() {
			rows := r.followingRowsFrom(viewerID, ids)
			mu.Lock()
			defer mu.Unlock()
			for id, row := range rows {
				rel := out[id]
				rel.isFollowing = true
				rel.followRec = row
				out[id] = rel
			}
		})
		run(func() {
			hit := filterOrEach(ids, r.Following.FilterFollowingsToAnchor, viewerID, func(id string) (bool, error) {
				return r.Following.Exists(id, viewerID)
			})
			set(hit, func(rel *relation) { rel.isFollowed = true })
		})
	}
	if r.Blocking != nil {
		batch, _ := r.Blocking.(repository.BlockingAnchorFilter)
		run(func() {
			var f func(string, []string) ([]string, error)
			if batch != nil {
				f = batch.FilterBlockingFromAnchor
			}
			hit := filterOrEach(ids, f, viewerID, func(id string) (bool, error) { return r.Blocking.Exists(viewerID, id) })
			set(hit, func(rel *relation) { rel.isBlocking = true })
		})
		run(func() {
			var f func(string, []string) ([]string, error)
			if batch != nil {
				f = batch.FilterBlockingToAnchor
			}
			hit := filterOrEach(ids, f, viewerID, func(id string) (bool, error) { return r.Blocking.Exists(id, viewerID) })
			set(hit, func(rel *relation) { rel.isBlocked = true })
		})
	}
	if r.Muting != nil {
		run(func() {
			var f func(string, []string) ([]string, error)
			if batch, ok := r.Muting.(repository.MutingAnchorFilter); ok {
				f = batch.FilterActiveMutingFromAnchor
			}
			hit := filterOrEach(ids, f, viewerID, func(id string) (bool, error) { return r.Muting.Exists(viewerID, id) })
			set(hit, func(rel *relation) { rel.isMuted = true })
		})
	}
	if r.RenoteMuting != nil {
		run(func() {
			var f func(string, []string) ([]string, error)
			if batch, ok := r.RenoteMuting.(repository.RenoteMutingAnchorFilter); ok {
				f = batch.FilterRenoteMutingFromAnchor
			}
			hit := filterOrEach(ids, f, viewerID, func(id string) (bool, error) { return r.RenoteMuting.Exists(viewerID, id) })
			set(hit, func(rel *relation) { rel.isRenoteMuted = true })
		})
	}
	if r.FollowRequest != nil {
		run(func() {
			hit := filterOrEach(ids, r.FollowRequest.FilterPendingFromAnchor, viewerID, func(id string) (bool, error) {
				return r.FollowRequest.Exists(viewerID, id)
			})
			set(hit, func(rel *relation) { rel.hasPendingFrom = true })
		})
		run(func() {
			hit := filterOrEach(ids, r.FollowRequest.FilterPendingToAnchor, viewerID, func(id string) (bool, error) {
				return r.FollowRequest.Exists(id, viewerID)
			})
			set(hit, func(rel *relation) { rel.hasPendingTo = true })
		})
	}
	if r.Memo != nil {
		run(func() {
			memos := r.memosFrom(viewerID, ids)
			mu.Lock()
			defer mu.Unlock()
			for id, m := range memos {
				rel := out[id]
				rel.memo = m
				out[id] = rel
			}
		})
	}
	wg.Wait()
	return out
}

// filterOrEach returns the set of ids for which the relation holds: one batch
// query when batch is non-nil, else one exists query per id.
//
// 問い合わせが失敗したときは Apply と同じく「関係なし」に倒す (Apply も
// Exists の err を捨てて false にしている)。
func filterOrEach(ids []string, batch func(anchorID string, candidateIDs []string) ([]string, error), anchorID string, exists func(id string) (bool, error)) map[string]bool {
	hit := make(map[string]bool)
	if batch != nil {
		got, err := batch(anchorID, ids)
		if err != nil {
			return hit
		}
		for _, id := range got {
			hit[id] = true
		}
		return hit
	}
	for _, id := range ids {
		if ok, err := exists(id); err == nil && ok {
			hit[id] = true
		}
	}
	return hit
}

// followingRowsFrom returns the viewer's following rows to ids, keyed by
// followee.
func (r Repos) followingRowsFrom(viewerID string, ids []string) map[string]*model.Following {
	out := make(map[string]*model.Following)
	if batch, ok := r.Following.(repository.FollowingRowsFromAnchorReader); ok {
		rows, err := batch.ListFollowingRowsFromAnchor(viewerID, ids)
		if err != nil {
			return out
		}
		for _, row := range rows {
			out[row.FolloweeID] = row
		}
		return out
	}
	for _, id := range ids {
		if row, err := r.Following.FindByPair(viewerID, id); err == nil && row != nil {
			out[id] = row
		}
	}
	return out
}

// memosFrom returns the viewer's memos about ids, keyed by target.
func (r Repos) memosFrom(viewerID string, ids []string) map[string]*model.UserMemo {
	out := make(map[string]*model.UserMemo)
	if batch, ok := r.Memo.(repository.UserMemoBatchReader); ok {
		rows, err := batch.ListByUserAndTargets(viewerID, ids)
		if err != nil {
			return out
		}
		for _, row := range rows {
			out[row.TargetUserID] = row
		}
		return out
	}
	for _, id := range ids {
		if m, err := r.Memo.FindByPair(viewerID, id); err == nil && m != nil {
			out[id] = m
		}
	}
	return out
}
