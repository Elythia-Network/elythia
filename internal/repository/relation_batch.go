package repository

import (
	"time"

	"github.com/shiroha-a/mk/internal/model"
)

// FollowingRowsFromAnchorReader returns the following rows from anchorID to
// any of candidateIDs (followerId=anchorID AND followeeId IN candidates) in one
// query. The rows carry notify / withReplies, which the relation block of a
// packed user needs besides isFollowing.
//
// 一覧の利用者ごとに FindByPair と Exists を引くと、利用者の数だけ往復が増える
// (#3330)。本家 getRelations も閲覧者の following を 1 回で引く。
type FollowingRowsFromAnchorReader interface {
	ListFollowingRowsFromAnchor(anchorID string, candidateIDs []string) ([]*model.Following, error)
}

// BlockingAnchorFilter batch-computes isBlocking / isBlocked across a user
// list, mirroring FollowingRepository.FilterFollowingsFromAnchor.
type BlockingAnchorFilter interface {
	// FilterBlockingFromAnchor returns the subset of candidateIDs that anchorID
	// blocks (blockerId=anchorID AND blockeeId IN candidates).
	FilterBlockingFromAnchor(anchorID string, candidateIDs []string) ([]string, error)
	// FilterBlockingToAnchor returns the subset of candidateIDs that block
	// anchorID (blockerId IN candidates AND blockeeId=anchorID).
	FilterBlockingToAnchor(anchorID string, candidateIDs []string) ([]string, error)
}

// MutingAnchorFilter batch-computes isMuted across a user list.
type MutingAnchorFilter interface {
	// FilterActiveMutingFromAnchor returns the subset of candidateIDs that
	// anchorID mutes with an active (non-expired) mute, the same rows Exists
	// counts.
	FilterActiveMutingFromAnchor(anchorID string, candidateIDs []string) ([]string, error)
}

// RenoteMutingAnchorFilter batch-computes isRenoteMuted across a user list.
type RenoteMutingAnchorFilter interface {
	// FilterRenoteMutingFromAnchor returns the subset of candidateIDs that
	// anchorID renote-mutes.
	FilterRenoteMutingFromAnchor(anchorID string, candidateIDs []string) ([]string, error)
}

// UserMemoBatchReader returns the memos userID wrote about any of targetIDs in
// one query.
type UserMemoBatchReader interface {
	ListByUserAndTargets(userID string, targetIDs []string) ([]*model.UserMemo, error)
}

var (
	_ FollowingRowsFromAnchorReader = (*followingRepository)(nil)
	_ BlockingAnchorFilter          = (*blockingRepository)(nil)
	_ MutingAnchorFilter            = (*mutingRepository)(nil)
	_ RenoteMutingAnchorFilter      = (*renoteMutingRepository)(nil)
	_ UserMemoBatchReader           = (*userMemoRepository)(nil)
)

func (r *followingRepository) ListFollowingRowsFromAnchor(anchorID string, candidateIDs []string) ([]*model.Following, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	var rows []*model.Following
	if err := r.db.Where(`"followerId" = ? AND "followeeId" IN ?`, anchorID, candidateIDs).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *blockingRepository) FilterBlockingFromAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.Blocking{}).
		Where(`"blockerId" = ? AND "blockeeId" IN ?`, anchorID, candidateIDs).
		Pluck(`"blockeeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *blockingRepository) FilterBlockingToAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.Blocking{}).
		Where(`"blockerId" IN ? AND "blockeeId" = ?`, candidateIDs, anchorID).
		Pluck(`"blockerId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *mutingRepository) FilterActiveMutingFromAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	var ids []string
	// Exists と同じく期限切れの行を数えない (cron が消すまでの間も解除済みとして扱う)。
	if err := r.db.Model(&model.Muting{}).
		Where(`"muterId" = ? AND "muteeId" IN ?`, anchorID, candidateIDs).
		Where(`"expiresAt" IS NULL OR "expiresAt" > ?`, time.Now()).
		Pluck(`"muteeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *renoteMutingRepository) FilterRenoteMutingFromAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	if anchorID == "" || len(candidateIDs) == 0 {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.RenoteMuting{}).
		Where(`"muterId" = ? AND "muteeId" IN ?`, anchorID, candidateIDs).
		Pluck(`"muteeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *userMemoRepository) ListByUserAndTargets(userID string, targetIDs []string) ([]*model.UserMemo, error) {
	if userID == "" || len(targetIDs) == 0 {
		return nil, nil
	}
	var rows []*model.UserMemo
	if err := r.db.Where(`"userId" = ? AND "targetUserId" IN ?`, userID, targetIDs).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
