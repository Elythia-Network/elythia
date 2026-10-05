package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PromoNoteRepository provides data access for the `promo_note` table.
type PromoNoteRepository interface {
	Create(p *model.PromoNote) error
	ListActive(now time.Time) ([]*model.PromoNote, error)
	// Exists reports whether a promo row exists for noteID. Used by
	// admin/promo/create to return ALREADY_PROMOTED on duplicates.
	Exists(noteID string) (bool, error)
}

type promoNoteRepository struct {
	db *gorm.DB
}

// NewPromoNoteRepository constructs the default PromoNoteRepository.
func NewPromoNoteRepository(db *gorm.DB) PromoNoteRepository {
	return &promoNoteRepository{db: db}
}

func (r *promoNoteRepository) Create(p *model.PromoNote) error {
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(p).Error
}

func (r *promoNoteRepository) ListActive(now time.Time) ([]*model.PromoNote, error) {
	var promos []*model.PromoNote
	if err := r.db.Where(`"expiresAt" > ?`, now).Find(&promos).Error; err != nil {
		return nil, err
	}
	return promos, nil
}

func (r *promoNoteRepository) Exists(noteID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.PromoNote{}).Where(`"noteId" = ?`, noteID).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// PromoReadRepository provides data access for the `promo_read` table.
type PromoReadRepository interface {
	MarkRead(p *model.PromoRead) error
	IsRead(userID, noteID string) bool
}

type promoReadRepository struct {
	db *gorm.DB
}

// NewPromoReadRepository constructs the default PromoReadRepository.
func NewPromoReadRepository(db *gorm.DB) PromoReadRepository {
	return &promoReadRepository{db: db}
}

func (r *promoReadRepository) MarkRead(p *model.PromoRead) error {
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(p).Error
}

func (r *promoReadRepository) IsRead(userID, noteID string) bool {
	var count int64
	r.db.Model(&model.PromoRead{}).Where(`"userId" = ? AND "noteId" = ?`, userID, noteID).Count(&count)
	return count > 0
}
