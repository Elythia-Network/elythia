package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ClipRepository provides data access for the `clip` table.
type ClipRepository interface {
	Create(c *model.Clip) error
	FindByID(id string) (*model.Clip, error)
	UpdateFields(clipID string, fields map[string]any) error
	Delete(c *model.Clip) error
	// ListByUser returns clips owned by userID with cursor (sinceID/untilID)
	// or offset pagination. Empty cursors fall back to offset.
	ListByUser(userID string, sinceID, untilID string, limit, offset int) ([]*model.Clip, error)
	// CountByUser returns the number of clips owned by userID. Used by
	// clipLimit policy gate (#1029 PR-1 follow-up).
	CountByUser(userID string) (int64, error)
	// ListPublicByUser returns only public clips owned by userID. Used by
	// users/clips when the viewer is not the owner so LIMIT applies to the
	// already-filtered set. Same cursor semantics as ListByUser.
	ListPublicByUser(userID string, sinceID, untilID string, limit, offset int) ([]*model.Clip, error)
	// ListPublicByIDs returns the public clips whose id is in ids (any owner).
	// notes/clips が「note を含む public clip」を引くのに使う (upstream
	// clipsRepository.findBy({id: In(...), isPublic: true}))。
	ListPublicByIDs(ids []string) ([]*model.Clip, error)
}

type clipRepository struct {
	db *gorm.DB
}

// NewClipRepository creates a new ClipRepository.
func NewClipRepository(db *gorm.DB) ClipRepository {
	return &clipRepository{db: db}
}

func (r *clipRepository) Create(c *model.Clip) error {
	return r.db.Create(c).Error
}

func (r *clipRepository) FindByID(id string) (*model.Clip, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var c model.Clip
	if err := r.db.First(&c, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *clipRepository) ListPublicByIDs(ids []string) ([]*model.Clip, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var clips []*model.Clip
	if err := r.db.Where(`"id" IN ? AND "isPublic" = ?`, ids, true).Find(&clips).Error; err != nil {
		return nil, err
	}
	return clips, nil
}

func (r *clipRepository) UpdateFields(clipID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Clip{}).Where("id = ?", clipID).Updates(fields).Error
}

func (r *clipRepository) Delete(c *model.Clip) error {
	return r.db.Delete(c).Error
}

// ListByUser returns clips owned by userID with cursor or offset pagination.
func (r *clipRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Clip, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Where(`"userId" = ?`, userID)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	// cursor 指定時は offset 無視 (本家 makePaginationQuery と同じ)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.Clip
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CountByUser returns the number of clips owned by the given user.
func (r *clipRepository) CountByUser(userID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.Clip{}).
		Where(`"userId" = ?`, userID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r *clipRepository) ListPublicByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Clip, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(userID) || !storable(sinceID) || !storable(untilID) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Where(`"userId" = ? AND "isPublic" = true`, userID)
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
	var rows []*model.Clip
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
