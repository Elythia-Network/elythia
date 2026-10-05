package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// NoteFavoriteRepository provides data access for note favorites.
type NoteFavoriteRepository interface {
	Create(f *model.NoteFavorite) error
	Delete(userID, noteID string) error
	Exists(userID, noteID string) (bool, error)
	// ListByUser returns favorites filtered by keyset pagination cursors.
	// untilID == "" && sinceID == "" returns the latest page in DESC order.
	// Both empty / one empty matches Misskey's makePaginationQuery semantics.
	ListByUser(userID, untilID, sinceID string, limit int) ([]*model.NoteFavorite, error)
}

type noteFavoriteRepository struct {
	db *gorm.DB
}

func NewNoteFavoriteRepository(db *gorm.DB) NoteFavoriteRepository {
	return &noteFavoriteRepository{db: db}
}

func (r *noteFavoriteRepository) Create(f *model.NoteFavorite) error {
	return r.db.Create(f).Error
}

func (r *noteFavoriteRepository) Delete(userID, noteID string) error {
	return r.db.Where("\"userId\" = ? AND \"noteId\" = ?", userID, noteID).
		Delete(&model.NoteFavorite{}).Error
}

func (r *noteFavoriteRepository) Exists(userID, noteID string) (bool, error) {
	var count int64
	err := r.db.Model(&model.NoteFavorite{}).
		Where("\"userId\" = ? AND \"noteId\" = ?", userID, noteID).Count(&count).Error
	return count > 0, err
}

func (r *noteFavoriteRepository) ListByUser(userID, untilID, sinceID string, limit int) ([]*model.NoteFavorite, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	// お気に入り一覧は frontend で renote / reply 先も描画するので 1 段だけ
	// preload する。深さは note 本体と同じ方針 (#416)。
	q := r.db.Preload("Note").Preload("Note.User").
		Preload("Note.Renote").Preload("Note.Renote.User").
		Preload("Note.Reply").Preload("Note.Reply.User").
		Where("\"userId\" = ?", userID)
	// id は note_favorite.id (aidx) で keyset pagination する。Misskey 本家
	// の makePaginationQuery と同じ向きで、untilId は < cursor (DESC で次
	// ページへ進む)、sinceId は > cursor (新しい方向)。
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	var favs []*model.NoteFavorite
	if err := q.Find(&favs).Error; err != nil {
		return nil, err
	}
	return favs, nil
}
