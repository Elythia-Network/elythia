package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ChatApprovalRepository manages 1-on-1 chat permission overrides.
type ChatApprovalRepository interface {
	Create(a *model.ChatApproval) error
	Delete(userID, otherID string) error
	Exists(userID, otherID string) (bool, error)
	ListByUser(userID string) ([]*model.ChatApproval, error)
}

type chatApprovalRepository struct {
	db *gorm.DB
}

// NewChatApprovalRepository creates a new ChatApprovalRepository.
func NewChatApprovalRepository(db *gorm.DB) ChatApprovalRepository {
	return &chatApprovalRepository{db: db}
}

func (r *chatApprovalRepository) Create(a *model.ChatApproval) error {
	return r.db.Create(a).Error
}

func (r *chatApprovalRepository) Delete(userID, otherID string) error {
	return r.db.Where(`"userId" = ? AND "otherId" = ?`, userID, otherID).
		Delete(&model.ChatApproval{}).Error
}

func (r *chatApprovalRepository) Exists(userID, otherID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.ChatApproval{}).
		Where(`"userId" = ? AND "otherId" = ?`, userID, otherID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *chatApprovalRepository) ListByUser(userID string) ([]*model.ChatApproval, error) {
	var rows []*model.ChatApproval
	if err := r.db.Where(`"userId" = ?`, userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
