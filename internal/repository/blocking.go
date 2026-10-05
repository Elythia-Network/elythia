package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// BlockingRepository provides data access for the `blocking` table.
type BlockingRepository interface {
	Create(b *model.Blocking) error
	Delete(b *model.Blocking) error
	FindByPair(blockerID, blockeeID string) (*model.Blocking, error)
	Exists(blockerID, blockeeID string) (bool, error)
	// ListByBlocker supports cursor (sinceID/untilID) and offset pagination.
	ListByBlocker(blockerID, sinceID, untilID string, limit, offset int) ([]*model.Blocking, error)
	// ListBlockerIDs returns the blockerIDs of every block row whose blockeeId
	// equals the given user. Used by note-list endpoints (antennas/notes,
	// roles/notes) to drop notes authored by someone who has blocked the
	// viewer, matching upstream generateBlockedUserQueryForNotes (#1544).
	ListBlockerIDs(blockeeID string) ([]string, error)
	// ListBlockeeIDs returns the blockeeIDs of every block row whose blockerId
	// equals the given user (= who blockerID blocks). users/followers・following
	// で viewer の isBlocking を batch 解決するのに使う (#2106 N3)。
	ListBlockeeIDs(blockerID string) ([]string, error)
}

type blockingRepository struct {
	db *gorm.DB
}

// NewBlockingRepository creates a new BlockingRepository.
func NewBlockingRepository(db *gorm.DB) BlockingRepository {
	return &blockingRepository{db: db}
}

func (r *blockingRepository) Create(b *model.Blocking) error {
	return r.db.Create(b).Error
}

func (r *blockingRepository) Delete(b *model.Blocking) error {
	return r.db.Delete(b).Error
}

func (r *blockingRepository) FindByPair(blockerID, blockeeID string) (*model.Blocking, error) {
	if !storable(blockerID) || !storable(blockeeID) {
		return nil, ErrNotFound
	}
	var b model.Blocking
	if err := r.db.Where("\"blockerId\" = ? AND \"blockeeId\" = ?", blockerID, blockeeID).First(&b).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *blockingRepository) Exists(blockerID, blockeeID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.Blocking{}).
		Where("\"blockerId\" = ? AND \"blockeeId\" = ?", blockerID, blockeeID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *blockingRepository) ListByBlocker(blockerID, sinceID, untilID string, limit, offset int) ([]*model.Blocking, error) {
	q := r.db.Where(`"blockerId" = ?`, blockerID)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.Blocking
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *blockingRepository) ListBlockerIDs(blockeeID string) ([]string, error) {
	if blockeeID == "" {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.Blocking{}).
		Where(`"blockeeId" = ?`, blockeeID).
		Pluck(`"blockerId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// ListBlockeeIDs returns the blockeeIDs of every block row whose blockerId equals
// blockerID (= who blockerID blocks). Used by users/followers・following to batch
// resolve viewer's isBlocking (#2106 N3).
func (r *blockingRepository) ListBlockeeIDs(blockerID string) ([]string, error) {
	if blockerID == "" {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.Blocking{}).
		Where(`"blockerId" = ?`, blockerID).
		Pluck(`"blockeeId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
