package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UserMemoRepository provides data access for the `user_memo` table.
type UserMemoRepository interface {
	// CreateOrUpdate upserts a memo for the (userId, targetUserId) pair.
	CreateOrUpdate(memo *model.UserMemo) error
	// FindByPair returns the memo from userId about targetUserId.
	FindByPair(userID, targetUserID string) (*model.UserMemo, error)
	// Delete removes a memo by (userId, targetUserId).
	Delete(userID, targetUserID string) error
}

type userMemoRepository struct {
	db *gorm.DB
}

// NewUserMemoRepository constructs the default UserMemoRepository.
func NewUserMemoRepository(db *gorm.DB) UserMemoRepository {
	return &userMemoRepository{db: db}
}

func (r *userMemoRepository) CreateOrUpdate(memo *model.UserMemo) error {
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "userId"}, {Name: "targetUserId"}},
		DoUpdates: clause.AssignmentColumns([]string{"memo"}),
	}).Create(memo).Error
}

func (r *userMemoRepository) FindByPair(userID, targetUserID string) (*model.UserMemo, error) {
	if !storable(userID) || !storable(targetUserID) {
		return nil, ErrNotFound
	}
	var m model.UserMemo
	if err := r.db.Where(`"userId" = ? AND "targetUserId" = ?`, userID, targetUserID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *userMemoRepository) Delete(userID, targetUserID string) error {
	return r.db.Where(`"userId" = ? AND "targetUserId" = ?`, userID, targetUserID).Delete(&model.UserMemo{}).Error
}
