package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ClipFavoriteRepository provides data access for clip favorites.
type ClipFavoriteRepository interface {
	Create(fav *model.ClipFavorite) error
	Delete(userID, clipID string) error
	ListByUser(userID string) ([]*model.ClipFavorite, error)
	Exists(userID, clipID string) (bool, error)
	// CountByClip returns the number of favorites for a clip. clip 応答の
	// favoritedCount 実カウント用 (#1562、upstream clipFavoritesRepository.countBy)。
	CountByClip(clipID string) (int64, error)
}

type clipFavoriteRepository struct {
	db *gorm.DB
}

// NewClipFavoriteRepository creates a new ClipFavoriteRepository.
func NewClipFavoriteRepository(db *gorm.DB) ClipFavoriteRepository {
	return &clipFavoriteRepository{db: db}
}

func (r *clipFavoriteRepository) Create(fav *model.ClipFavorite) error {
	return r.db.Create(fav).Error
}

func (r *clipFavoriteRepository) Delete(userID, clipID string) error {
	return r.db.Where(`"userId" = ? AND "clipId" = ?`, userID, clipID).
		Delete(&model.ClipFavorite{}).Error
}

func (r *clipFavoriteRepository) ListByUser(userID string) ([]*model.ClipFavorite, error) {
	var favs []*model.ClipFavorite
	if err := r.db.Where(`"userId" = ?`, userID).Find(&favs).Error; err != nil {
		return nil, err
	}
	return favs, nil
}

func (r *clipFavoriteRepository) Exists(userID, clipID string) (bool, error) {
	var count int64
	err := r.db.Model(&model.ClipFavorite{}).
		Where(`"userId" = ? AND "clipId" = ?`, userID, clipID).Count(&count).Error
	return count > 0, err
}

func (r *clipFavoriteRepository) CountByClip(clipID string) (int64, error) {
	var count int64
	err := r.db.Model(&model.ClipFavorite{}).
		Where(`"clipId" = ?`, clipID).Count(&count).Error
	return count, err
}
