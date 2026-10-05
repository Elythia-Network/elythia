package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// BubbleGameRepository handles bubble_game_record persistence.
type BubbleGameRepository interface {
	Create(record *model.BubbleGameRecord) error
	Ranking(gameMode string, limit int) ([]*model.BubbleGameRecord, error)
}

type bubbleGameRepository struct {
	db *gorm.DB
}

// NewBubbleGameRepository creates a new BubbleGameRepository.
func NewBubbleGameRepository(db *gorm.DB) BubbleGameRepository {
	return &bubbleGameRepository{db: db}
}

func (r *bubbleGameRepository) Create(record *model.BubbleGameRecord) error {
	return r.db.Create(record).Error
}

func (r *bubbleGameRepository) Ranking(gameMode string, limit int) ([]*model.BubbleGameRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	// 直近7日間のスコアランキング
	since := time.Now().Add(-7 * 24 * time.Hour)
	var records []*model.BubbleGameRecord
	if err := r.db.Preload("User").
		Where(`"gameMode" = ? AND "seededAt" > ?`, gameMode, since).
		Order(`"score" DESC`).Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}
