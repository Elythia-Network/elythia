package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// AdRepository provides data access for the `ad` table. ListActive is used by
// /api/meta for timeline ad exposure; the rest supports the admin CRUD.
type AdRepository interface {
	// ListActive returns ads whose window contains `now` (startsAt <= now
	// < expiresAt). Ordering is by id DESC so newest ads come first.
	ListActive(now time.Time) ([]*model.Ad, error)
	Create(a *model.Ad) error
	FindByID(id string) (*model.Ad, error)
	// List returns ads ordered by id DESC with limit/offset pagination.
	List(limit, offset int) ([]*model.Ad, error)
	// ListFiltered returns ads with cursor pagination (sinceID/untilID) and an
	// optional publishing filter (#1545, upstream admin/ad/list)。publishing が
	// nil なら全件、true なら配信中 (startsAt<=now<expiresAt)、false なら非配信。
	ListFiltered(publishing *bool, sinceID, untilID string, limit int, now time.Time) ([]*model.Ad, error)
	// UpdateFields applies a partial update. Keys must be GORM column names.
	UpdateFields(id string, fields map[string]any) error
	Delete(id string) error
}

type adRepository struct {
	db *gorm.DB
}

// NewAdRepository constructs the default AdRepository.
func NewAdRepository(db *gorm.DB) AdRepository {
	return &adRepository{db: db}
}

func (r *adRepository) ListActive(now time.Time) ([]*model.Ad, error) {
	var ads []*model.Ad
	if err := r.db.
		Where(`"startsAt" <= ? AND "expiresAt" > ?`, now, now).
		Order(`id DESC`).
		Find(&ads).Error; err != nil {
		return nil, err
	}
	return ads, nil
}

func (r *adRepository) Create(a *model.Ad) error {
	return r.db.Create(a).Error
}

func (r *adRepository) FindByID(id string) (*model.Ad, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var a model.Ad
	if err := r.db.Where(`"id" = ?`, id).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *adRepository) List(limit, offset int) ([]*model.Ad, error) {
	if limit <= 0 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	var ads []*model.Ad
	if err := r.db.Order(`"id" DESC`).Limit(limit).Offset(offset).Find(&ads).Error; err != nil {
		return nil, err
	}
	return ads, nil
}

func (r *adRepository) ListFiltered(publishing *bool, sinceID, untilID string, limit int, now time.Time) ([]*model.Ad, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Model(&model.Ad{})
	// upstream admin/ad/list: publishing=true は配信中 (startsAt<=now<expiresAt)、
	// false は非配信 (expiresAt<=now OR startsAt>now)。
	if publishing != nil {
		if *publishing {
			q = q.Where(`"startsAt" <= ? AND "expiresAt" > ?`, now, now)
		} else {
			q = q.Where(`"expiresAt" <= ? OR "startsAt" > ?`, now, now)
		}
	}
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	var ads []*model.Ad
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&ads).Error; err != nil {
		return nil, err
	}
	return ads, nil
}

func (r *adRepository) UpdateFields(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Ad{}).Where(`"id" = ?`, id).Updates(fields).Error
}

func (r *adRepository) Delete(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.Ad{}).Error
}
