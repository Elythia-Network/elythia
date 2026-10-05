package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// UserListFavoriteRepository provides data access for user list favorites.
type UserListFavoriteRepository interface {
	Create(fav *model.UserListFavorite) error
	Delete(userID, listID string) error
	ListByUser(userID string) ([]*model.UserListFavorite, error)
	Exists(userID, listID string) (bool, error)
	// CountByList returns the number of favorites of a list. Used by
	// users/lists/show forPublic の likedCount (#1550)。
	CountByList(listID string) (int64, error)
}

type userListFavoriteRepository struct {
	db *gorm.DB
}

// NewUserListFavoriteRepository creates a new UserListFavoriteRepository.
func NewUserListFavoriteRepository(db *gorm.DB) UserListFavoriteRepository {
	return &userListFavoriteRepository{db: db}
}

func (r *userListFavoriteRepository) Create(fav *model.UserListFavorite) error {
	return r.db.Create(fav).Error
}

func (r *userListFavoriteRepository) Delete(userID, listID string) error {
	return r.db.Where(`"userId" = ? AND "userListId" = ?`, userID, listID).
		Delete(&model.UserListFavorite{}).Error
}

func (r *userListFavoriteRepository) ListByUser(userID string) ([]*model.UserListFavorite, error) {
	var favs []*model.UserListFavorite
	if err := r.db.Where(`"userId" = ?`, userID).Find(&favs).Error; err != nil {
		return nil, err
	}
	return favs, nil
}

func (r *userListFavoriteRepository) Exists(userID, listID string) (bool, error) {
	var count int64
	err := r.db.Model(&model.UserListFavorite{}).
		Where(`"userId" = ? AND "userListId" = ?`, userID, listID).Count(&count).Error
	return count > 0, err
}

func (r *userListFavoriteRepository) CountByList(listID string) (int64, error) {
	var count int64
	err := r.db.Model(&model.UserListFavorite{}).
		Where(`"userListId" = ?`, listID).Count(&count).Error
	return count, err
}
