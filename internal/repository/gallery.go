package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// GalleryRepository provides data access for gallery posts and likes
// scoped to a single user. i/gallery/posts と i/gallery/likes の
// per-user 一覧で利用する。
type GalleryRepository interface {
	// ListByUser supports cursor (sinceID/untilID) and offset pagination.
	// Cursor 指定時は offset 無視。
	ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.GalleryPost, error)
	// ListLikesByUser same cursor semantics.
	ListLikesByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.GalleryLike, error)
	// FindPostsByIDs returns the posts whose id is in ids. Used by
	// i/gallery/likes to embed the full GalleryPost in each like row.
	FindPostsByIDs(ids []string) ([]*model.GalleryPost, error)
	// ExistsLike reports whether userID has liked postID. GalleryPost.isLiked
	// 用 (upstream galleryLikesRepository.exists({postId, userId}))。
	ExistsLike(userID, postID string) (bool, error)
}

type galleryRepository struct {
	db *gorm.DB
}

// NewGalleryRepository builds a GalleryRepository backed by gorm.
func NewGalleryRepository(db *gorm.DB) GalleryRepository {
	return &galleryRepository{db: db}
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return 10
	}
	if limit > 100 {
		return 100
	}
	return limit
}

func (r *galleryRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.GalleryPost, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(userID) || !storable(sinceID) || !storable(untilID) {
		return nil, nil
	}
	limit = clampLimit(limit)
	// packGalleryPost が p.User を読んで user フィールドを埋めるので、
	// FindPostsByIDs と揃えて User を Preload する。Preload なしだと
	// i/gallery/posts のレスポンスから user が抜け落ちて i/gallery/likes
	// との shape 不整合になる (#520 review)。
	q := r.db.Preload("User").Where(`"userId" = ?`, userID)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var posts []*model.GalleryPost
	if err := q.Find(&posts).Error; err != nil {
		return nil, err
	}
	return posts, nil
}

func (r *galleryRepository) ExistsLike(userID, postID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.GalleryLike{}).
		Where(`"userId" = ? AND "postId" = ?`, userID, postID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *galleryRepository) FindPostsByIDs(ids []string) ([]*model.GalleryPost, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var posts []*model.GalleryPost
	if err := r.db.Preload("User").Where("id IN ?", ids).Find(&posts).Error; err != nil {
		return nil, err
	}
	return posts, nil
}

func (r *galleryRepository) ListLikesByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.GalleryLike, error) {
	limit = clampLimit(limit)
	q := r.db.Where(`"userId" = ?`, userID)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var likes []*model.GalleryLike
	if err := q.Find(&likes).Error; err != nil {
		return nil, err
	}
	return likes, nil
}
