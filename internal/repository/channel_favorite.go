package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ChannelFavoriteRepository provides data access for channel favorites.
type ChannelFavoriteRepository interface {
	Create(fav *model.ChannelFavorite) error
	Delete(userID, channelID string) error
	ListByUser(userID string) ([]*model.ChannelFavorite, error)
	Exists(userID, channelID string) (bool, error)
	// ExistsMany returns a channelID → bool map indicating which of the
	// given channels userID currently favorites. Used by /channels list
	// endpoints to embed isFavorited without N+1 (#522).
	ExistsMany(userID string, channelIDs []string) (map[string]bool, error)
}

type channelFavoriteRepository struct {
	db *gorm.DB
}

// NewChannelFavoriteRepository creates a new ChannelFavoriteRepository.
func NewChannelFavoriteRepository(db *gorm.DB) ChannelFavoriteRepository {
	return &channelFavoriteRepository{db: db}
}

func (r *channelFavoriteRepository) Create(fav *model.ChannelFavorite) error {
	return r.db.Create(fav).Error
}

func (r *channelFavoriteRepository) Delete(userID, channelID string) error {
	return r.db.Where(`"userId" = ? AND "channelId" = ?`, userID, channelID).
		Delete(&model.ChannelFavorite{}).Error
}

func (r *channelFavoriteRepository) ListByUser(userID string) ([]*model.ChannelFavorite, error) {
	var favs []*model.ChannelFavorite
	if err := r.db.Where(`"userId" = ?`, userID).Find(&favs).Error; err != nil {
		return nil, err
	}
	return favs, nil
}

func (r *channelFavoriteRepository) Exists(userID, channelID string) (bool, error) {
	var count int64
	err := r.db.Model(&model.ChannelFavorite{}).
		Where(`"userId" = ? AND "channelId" = ?`, userID, channelID).Count(&count).Error
	return count > 0, err
}

// ExistsMany resolves "user has X favorited?" for a set of channel IDs in
// a single query. 空入力 / 空 userID は空 map を返す。
func (r *channelFavoriteRepository) ExistsMany(userID string, channelIDs []string) (map[string]bool, error) {
	if userID == "" || len(channelIDs) == 0 {
		return map[string]bool{}, nil
	}
	var favorited []string
	if err := r.db.Model(&model.ChannelFavorite{}).
		Where(`"userId" = ? AND "channelId" IN ?`, userID, channelIDs).
		Pluck(`"channelId"`, &favorited).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(favorited))
	for _, id := range favorited {
		out[id] = true
	}
	return out, nil
}
