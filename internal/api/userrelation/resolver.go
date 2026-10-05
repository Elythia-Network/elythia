// Package userrelation computes the viewer->target relation block shared by
// /api/users/show and /api/blocking/{create,delete}, mirroring upstream
// UserEntityService's pack(target, me) relation fields (isFollowing, isFollowed,
// isBlocking, isBlocked, isMuted, isRenoteMuted, hasPendingFollowRequest*,
// notify, withReplies, followedMessage, memo). Centralizing the computation
// keeps the two call sites from drifting (#1802).
package userrelation

import (
	"sync"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Repos bundles the repositories needed to compute a viewer->target relation
// block. Any nil repo skips its corresponding flags (= test fixtures / partial
// wiring); production wires all of them so the full relation block is emitted.
type Repos struct {
	Following     repository.FollowingRepository
	Blocking      repository.BlockingRepository
	Muting        repository.MutingRepository
	RenoteMuting  repository.RenoteMutingRepository
	FollowRequest repository.FollowRequestRepository
	Memo          repository.UserMemoRepository
}

// Apply computes the viewer->target relation flags and writes them onto
// detailed, matching upstream UserEntityService relation block. It is a no-op
// when detailed/target is nil, or when viewerID is empty (anonymous) or equals
// target.ID (self view) — the same conditions under which upstream omits the
// block. profile may be nil (followedMessage is then skipped). Returns whether
// the viewer follows the target; callers gate follower/following count
// visibility on it (#1558).
//
// 各リポジトリへのクエリは独立なので goroutine で並列実行し、全結果が揃ってから
// detailed に反映する (元々 users/show にインラインだった実装を共有化、#1802)。
func (r Repos) Apply(detailed *entity.UserDetailed, viewerID string, target *model.User, profile *model.UserProfile) bool {
	if detailed == nil || target == nil || viewerID == "" {
		return false
	}
	if viewerID == target.ID {
		// self には relation block (isFollowing 等) を付けない。ただし memo は
		// upstream が `isDetailed && meId` だけで引くので self でも載せる
		// (自分用のメモを自分のプロフィールから読める)。
		r.applyMemo(detailed, viewerID, target.ID)
		return false
	}

	var (
		rel relation
		wg  sync.WaitGroup
	)

	if r.Following != nil {
		wg.Add(3)
		go func() { defer wg.Done(); rel.isFollowing, _ = r.Following.Exists(viewerID, target.ID) }()
		go func() { defer wg.Done(); rel.isFollowed, _ = r.Following.Exists(target.ID, viewerID) }()
		go func() { defer wg.Done(); rel.followRec, _ = r.Following.FindByPair(viewerID, target.ID) }()
	}
	if r.Blocking != nil {
		wg.Add(2)
		go func() { defer wg.Done(); rel.isBlocking, _ = r.Blocking.Exists(viewerID, target.ID) }()
		go func() { defer wg.Done(); rel.isBlocked, _ = r.Blocking.Exists(target.ID, viewerID) }()
	}
	if r.Muting != nil {
		wg.Add(1)
		go func() { defer wg.Done(); rel.isMuted, _ = r.Muting.Exists(viewerID, target.ID) }()
	}
	if r.RenoteMuting != nil {
		wg.Add(1)
		go func() { defer wg.Done(); rel.isRenoteMuted, _ = r.RenoteMuting.Exists(viewerID, target.ID) }()
	}
	if r.FollowRequest != nil {
		wg.Add(2)
		go func() { defer wg.Done(); rel.hasPendingFrom, _ = r.FollowRequest.Exists(viewerID, target.ID) }()
		go func() { defer wg.Done(); rel.hasPendingTo, _ = r.FollowRequest.Exists(target.ID, viewerID) }()
	}
	if r.Memo != nil {
		wg.Add(1)
		go func() { defer wg.Done(); rel.memo, _ = r.Memo.FindByPair(viewerID, target.ID) }()
	}

	wg.Wait()
	return r.write(detailed, rel, profile)
}

// relation is the viewer->target relation state read from the repositories.
type relation struct {
	isFollowing, isFollowed      bool
	isBlocking, isBlocked        bool
	isMuted, isRenoteMuted       bool
	hasPendingFrom, hasPendingTo bool
	followRec                    *model.Following
	memo                         *model.UserMemo
}

// write stores rel onto detailed and reports whether the viewer follows the
// target. Apply と ApplyMany が同じ書き方をするよう 1 箇所にまとめる。
func (r Repos) write(detailed *entity.UserDetailed, rel relation, profile *model.UserProfile) bool {
	viewerIsFollowing := false
	if r.Following != nil {
		isFollowing, isFollowed := rel.isFollowing, rel.isFollowed
		detailed.IsFollowing = &isFollowing
		detailed.IsFollowed = &isFollowed
		viewerIsFollowing = isFollowing
		// notify / withReplies は relation ブロックが存在する限り常に emit する。
		// upstream は `notify: relation.following?.notify ?? 'none'` /
		// `withReplies: relation.following?.withReplies ?? false` で、follow row が
		// 無い / notify 未設定でも default を出す (#1558)。
		none := "none"
		withReplies := false
		if rel.followRec != nil {
			if rel.followRec.Notify != nil {
				notify := *rel.followRec.Notify
				detailed.Notify = &notify
			} else {
				detailed.Notify = &none
			}
			withReplies = rel.followRec.WithReplies
		} else {
			detailed.Notify = &none
		}
		detailed.WithReplies = &withReplies
		// followedMessage は follower にだけ見せる。upstream は未設定でも null を
		// emit する (`relation.isFollowing ? (profile.followedMessage ?? null) :
		// undefined`、#1558 / #2097)。
		if isFollowing && profile != nil {
			entity.SetFollowedMessageForFollower(detailed, profile.FollowedMessage)
		}
	}
	if r.Blocking != nil {
		isBlocking, isBlocked := rel.isBlocking, rel.isBlocked
		detailed.IsBlocking = &isBlocking
		detailed.IsBlocked = &isBlocked
	}
	if r.Muting != nil {
		isMuted := rel.isMuted
		detailed.IsMuted = &isMuted
	}
	if r.RenoteMuting != nil {
		isRenoteMuted := rel.isRenoteMuted
		detailed.IsRenoteMuted = &isRenoteMuted
	}
	if r.FollowRequest != nil {
		hasPendingFrom, hasPendingTo := rel.hasPendingFrom, rel.hasPendingTo
		detailed.HasPendingFollowRequestFromYou = &hasPendingFrom
		detailed.HasPendingFollowRequestToYou = &hasPendingTo
	}
	if r.Memo != nil && rel.memo != nil {
		memo := rel.memo.Memo
		detailed.Memo = &memo
	}
	return viewerIsFollowing
}

// applyMemo fills only the viewer's memo for the target. self-view で relation
// block 抜きに memo だけ欲しいときに使う。
func (r Repos) applyMemo(detailed *entity.UserDetailed, viewerID, targetID string) {
	if r.Memo == nil {
		return
	}
	if memo, err := r.Memo.FindByPair(viewerID, targetID); err == nil && memo != nil {
		detailed.Memo = &memo.Memo
	}
}
