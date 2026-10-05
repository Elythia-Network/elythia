package repository

import (
	"errors"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// RetentionAggregationRepository provides data access for retention statistics.
type RetentionAggregationRepository interface {
	ListRecent(limit int) ([]*model.RetentionAggregation, error)
	// ListSince returns retention rows whose createdAt is >= cutoff.
	// Used by the daily aggregation job to update the trailing 31-day
	// cohorts (#421)。
	ListSince(cutoff time.Time) ([]*model.RetentionAggregation, error)
	// FindByDateKey returns the row for an exact dateKey or ErrNotFound.
	FindByDateKey(dateKey string) (*model.RetentionAggregation, error)
	// Insert persists a new row. Returns ErrDuplicateKey when a row with
	// the same dateKey already exists (= the same day was already
	// processed by another worker).
	Insert(row *model.RetentionAggregation) error
	// Update sets `data` and `updatedAt` on the row identified by id.
	Update(id string, updatedAt time.Time, data datatypes.JSON) error
}

// ErrDuplicateKey is returned by Insert when a unique constraint is violated.
var ErrDuplicateKey = errors.New("repository: duplicate key")

type retentionAggregationRepository struct {
	db *gorm.DB
}

// NewRetentionAggregationRepository creates a new RetentionAggregationRepository.
func NewRetentionAggregationRepository(db *gorm.DB) RetentionAggregationRepository {
	return &retentionAggregationRepository{db: db}
}

func (r *retentionAggregationRepository) ListRecent(limit int) ([]*model.RetentionAggregation, error) {
	var records []*model.RetentionAggregation
	if err := r.db.Order(`"id" DESC`).Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

func (r *retentionAggregationRepository) ListSince(cutoff time.Time) ([]*model.RetentionAggregation, error) {
	var records []*model.RetentionAggregation
	if err := r.db.Where(`"createdAt" >= ?`, cutoff).Order(`"createdAt" ASC`).Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

func (r *retentionAggregationRepository) FindByDateKey(dateKey string) (*model.RetentionAggregation, error) {
	if !storable(dateKey) {
		return nil, ErrNotFound
	}
	var row model.RetentionAggregation
	if err := r.db.Where(`"dateKey" = ?`, dateKey).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *retentionAggregationRepository) Insert(row *model.RetentionAggregation) error {
	if err := r.db.Create(row).Error; err != nil {
		// PostgreSQL の unique constraint 違反は SQLSTATE 23505。
		// gorm の generic error 文字列に "duplicate key" が含まれるので、
		// ドライバ依存の細部を呼び出し側に漏らさないため文字列マッチ。
		if isDuplicateKeyErr(err) {
			return ErrDuplicateKey
		}
		return err
	}
	return nil
}

func (r *retentionAggregationRepository) Update(id string, updatedAt time.Time, data datatypes.JSON) error {
	return r.db.Model(&model.RetentionAggregation{}).
		Where(`id = ?`, id).
		Updates(map[string]any{
			"updatedAt": updatedAt,
			"data":      data,
		}).Error
}

func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "SQLSTATE 23505")
}
