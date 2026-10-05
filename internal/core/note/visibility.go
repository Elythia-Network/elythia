package note

import (
	"slices"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// CanSeeNote reports whether viewer is allowed to see the given note based on
// its visibility level. viewer may be nil for unauthenticated requests.
//
// followingChecker is invoked only when the visibility level requires a follow
// relationship check; pass nil to skip the check (in which case "followers"
// notes are treated as invisible to non-author viewers).
func CanSeeNote(viewer *model.User, n *model.Note, followingChecker repository.FollowingRepository) bool {
	return CanSeeNoteFunc(viewer, n, followsFunc(followingChecker))
}

// followsFunc adapts a FollowingRepository into the follow predicate
// CanSeeNoteFunc takes. nil repo yields nil (= 判定不能 = fail-closed)。
func followsFunc(followingChecker repository.FollowingRepository) func(viewerID, authorID string) bool {
	if followingChecker == nil {
		return nil
	}
	return func(viewerID, authorID string) bool {
		ok, err := followingChecker.Exists(viewerID, authorID)
		return err == nil && ok
	}
}

// CanSeeNoteFunc is CanSeeNote with the follow lookup supplied as a function so
// callers that already know the answer can avoid the query.
//
// **判定そのものは 1 実装に閉じる** (#2752)。antenna の fan-out は note 1 件を
// アクティブ antenna 全件に対して評価するので、repo を渡すと followers note で
// antenna 数ぶんの `Exists` が逐次で飛ぶ。呼び出し側が memo 済みの述語を渡せる
// ようにするための入口で、可視性のルールをここ以外に書かないための形。
//
// follows が nil なら follow 判定は不能 = followers note は投稿者本人以外に
// 見せない (従来の followingChecker == nil と同じ fail-closed)。
func CanSeeNoteFunc(viewer *model.User, n *model.Note, follows func(viewerID, authorID string) bool) bool {
	if n == nil {
		return false
	}
	if n.Visibility == model.NoteVisibilityPublic || n.Visibility == model.NoteVisibilityHome {
		return true
	}
	// 以降 (followers / specified) は viewer 必須。
	if viewer == nil {
		return false
	}
	// 投稿者本人は常に閲覧可。
	if viewer.ID == n.UserID {
		return true
	}
	switch n.Visibility {
	case model.NoteVisibilityFollowers:
		// #2106 N27: reply 先が viewer の followers note は閲覧可 (upstream followers 分岐の
		// replyUserId=meId)。mentions 宛も同様に閲覧可。
		// 注意: main-stream realtime push の #1472 anti-leak gate はこの緩和を含めない
		// canSeeNoteForStream を使う (mentioned 非フォロワーへの本文 realtime push を防ぐ)。
		if n.ReplyUserID != nil && *n.ReplyUserID == viewer.ID {
			return true
		}
		if slices.Contains(n.Mentions, viewer.ID) {
			return true
		}
		if follows == nil {
			return false
		}
		return follows(viewer.ID, n.UserID)
	case model.NoteVisibilitySpecified:
		// upstream shouldHideNote / isVisibleForMe は specified で visibleUserIds
		// だけを見る。mentions は判定材料にしない (本文で @ された だけの相手に
		// direct note を見せない)。
		return slices.Contains(n.VisibleUserIDs, viewer.ID)
	}
	return false
}

// canSeeNoteForStream is the strict main-stream realtime-push visibility gate
// (#1472). Unlike CanSeeNote (#2106 N27 で read 経路を upstream に緩和) it does NOT
// grant visibility via mentions / replyUserId, so a followers note's body/CW is
// never pushed in real time to a mentioned / replied-to non-follower (anti-leak /
// anti-harassment hardening を維持)。visibleUserIds は specified note の正規の宛先
// なので許可する (N13 で reply target も visibleUserIds に入るため specified reply は届く)。
func canSeeNoteForStream(viewer *model.User, n *model.Note, followingChecker repository.FollowingRepository) bool {
	if n == nil {
		return false
	}
	switch n.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome:
		return true
	case model.NoteVisibilityFollowers:
		if viewer == nil {
			return false
		}
		if viewer.ID == n.UserID {
			return true
		}
		if followingChecker == nil {
			return false
		}
		ok, err := followingChecker.Exists(viewer.ID, n.UserID)
		if err != nil {
			return false
		}
		return ok
	case model.NoteVisibilitySpecified:
		if viewer == nil {
			return false
		}
		if viewer.ID == n.UserID {
			return true
		}
		return slices.Contains(n.VisibleUserIDs, viewer.ID)
	}
	return false
}

// ClampVisibilityForReply narrows a reply's visibility so it can never be
// wider than the note it replies to (upstream 2026.7.0 #17747)。
//
// 旧仕様は「reply 先が public 以外なら public→home」だけで、followers 限定
// note への reply を home/public で作成して文脈を晒せた (visibility
// escalation)。upstream は reply 先の可視性ごとに段階クランプする。
//
// ローカル投稿経路と AP 受信経路の双方から呼ぶ (upstream は ApNoteService も
// NoteCreateService.create を通るため、リモート発の reply にも同じクランプが
// 掛かる)。
func ClampVisibilityForReply(replyTargetVisibility, visibility model.NoteVisibility) model.NoteVisibility {
	switch replyTargetVisibility {
	case model.NoteVisibilityHome:
		// home 対象への reply は home 以下のみ可。
		if visibility == model.NoteVisibilityPublic {
			return model.NoteVisibilityHome
		}
	case model.NoteVisibilityFollowers:
		// followers 対象への reply は followers 以下のみ可。
		if visibility == model.NoteVisibilityPublic || visibility == model.NoteVisibilityHome {
			return model.NoteVisibilityFollowers
		}
	case model.NoteVisibilitySpecified:
		// specified 対象への reply は specified のみ可。ローカル経路では手前の
		// ErrCannotReplyToSpecifiedVisibility で弾かれるため到達しないが、AP
		// 受信経路ではここが実効的なガードになる。
		if visibility != model.NoteVisibilitySpecified {
			return model.NoteVisibilitySpecified
		}
	}
	return visibility
}
