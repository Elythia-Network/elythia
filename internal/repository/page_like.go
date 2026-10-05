package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// PageLikeRepository provides data access for the `page_like` table.
type PageLikeRepository interface {
	Create(l *model.PageLike) error
	Delete(l *model.PageLike) error
	FindByPair(userID, pageID string) (*model.PageLike, error)
	Exists(userID, pageID string) (bool, error)
	// ListByUser returns page_like rows owned by userID with cursor
	// (sinceID/untilID) or offset pagination. cursor 指定時は offset 無視
	// (upstream `makePaginationQuery` 同 semantics、ASC/DESC は
	// paginationOrder helper で sinceID-only 時のみ ASC に flip)。
	ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.PageLike, error)
	// ListLikedPageIDs returns the subset of pageIDs that userID has liked, in
	// one query. Used by list endpoints (pages/featured) to batch-populate the
	// per-row `isLiked` flag without N+1 lookups (mirrors flash ListLikedFlashIDs)。
	ListLikedPageIDs(userID string, pageIDs []string) ([]string, error)
}

type pageLikeRepository struct {
	db *gorm.DB
}

// NewPageLikeRepository creates a new PageLikeRepository.
func NewPageLikeRepository(db *gorm.DB) PageLikeRepository {
	return &pageLikeRepository{db: db}
}

func (r *pageLikeRepository) Create(l *model.PageLike) error {
	return r.db.Create(l).Error
}

func (r *pageLikeRepository) Delete(l *model.PageLike) error {
	return r.db.Delete(l).Error
}

func (r *pageLikeRepository) FindByPair(userID, pageID string) (*model.PageLike, error) {
	if !storable(userID) || !storable(pageID) {
		return nil, ErrNotFound
	}
	var l model.PageLike
	if err := r.db.Where("\"userId\" = ? AND \"pageId\" = ?", userID, pageID).First(&l).Error; err != nil {
		return nil, err
	}
	return &l, nil
}

func (r *pageLikeRepository) Exists(userID, pageID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.PageLike{}).
		Where("\"userId\" = ? AND \"pageId\" = ?", userID, pageID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// ListByUser returns page_like rows owned by userID with cursor (sinceID/
// untilID) or offset pagination. cursor 経路は upstream `makePaginationQuery`
// と同じく、sinceID 単独は ASC・それ以外は DESC で order する
// (paginationOrder helper)。i/page-likes で frontend Paginator が fetchOlder
// で untilID を投げてくるため、cursor 未対応だと同 page を無限ループする
// (#1136 follow-up)。
func (r *pageLikeRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.PageLike, error) {
	limit = clampLimit(limit)
	q := r.db.Where(`"userId" = ?`, userID)
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
	var likes []*model.PageLike
	if err := q.Find(&likes).Error; err != nil {
		return nil, err
	}
	return likes, nil
}

// ListLikedPageIDs returns the subset of pageIDs that userID has liked.
func (r *pageLikeRepository) ListLikedPageIDs(userID string, pageIDs []string) ([]string, error) {
	if len(pageIDs) == 0 {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.PageLike{}).
		Where(`"userId" = ? AND "pageId" IN ?`, userID, pageIDs).
		Pluck(`"pageId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
