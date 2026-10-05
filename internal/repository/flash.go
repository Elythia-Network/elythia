package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"strings"
)

// FlashRepository provides data access for the `flash` table.
type FlashRepository interface {
	Create(f *model.Flash) error
	FindByID(id string) (*model.Flash, error)
	UpdateFields(flashID string, fields map[string]any) error
	Delete(f *model.Flash) error
	// ListByUser returns flashes owned by userID with cursor (sinceID/untilID)
	// or offset pagination. Cursor 指定時は offset 無視。
	ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Flash, error)
	// ListPublicByUser returns only public flashes (visibility='public') owned
	// by userID, used by users/flashs when viewer is not the owner.
	ListPublicByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Flash, error)
	// ListFeatured returns the most-liked flashes (offset mode) or id-ordered
	// (cursor mode).
	ListFeatured(sinceID, untilID string, limit, offset int) ([]*model.Flash, error)
	// Search performs a substring search across title and summary, with
	// cursor or offset pagination.
	Search(query, sinceID, untilID string, limit, offset int) ([]*model.Flash, error)
	IncrementCount(flashID, column string, delta int) error
}

type flashRepository struct {
	db *gorm.DB
}

// NewFlashRepository creates a new FlashRepository.
func NewFlashRepository(db *gorm.DB) FlashRepository {
	return &flashRepository{db: db}
}

func (r *flashRepository) Create(f *model.Flash) error {
	return r.db.Create(f).Error
}

func (r *flashRepository) FindByID(id string) (*model.Flash, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var f model.Flash
	if err := r.db.First(&f, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *flashRepository) UpdateFields(flashID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Flash{}).Where("id = ?", flashID).Updates(fields).Error
}

func (r *flashRepository) Delete(f *model.Flash) error {
	return r.db.Delete(f).Error
}

// ListByUser returns the flashes owned by userID with cursor (sinceID/untilID)
// or offset pagination. cursor mode は upstream `makePaginationQuery` 同様
// id 基準で sort + filter する (元実装の `updatedAt DESC` は frontend
// Paginator の untilId/sinceId と整合しないため変更)。
func (r *flashRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
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
	var rows []*model.Flash
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *flashRepository) ListPublicByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(userID) || !storable(sinceID) || !storable(untilID) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Where(`"userId" = ? AND visibility = 'public'`, userID)
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
	var rows []*model.Flash
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListFeatured returns the most-liked flashes (offset mode), or id-ordered
// when cursor is supplied (frontend Paginator対応)。
func (r *flashRepository) ListFeatured(sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Session(&gorm.Session{})
	// upstream FlashService.featured: 公開かつ likedCount>0 のみ。非公開 flash
	// が featured に漏れるのを防ぐ。
	q = q.Where("visibility = ?", "public").Where(`"likedCount" > 0`)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" || untilID != "" {
		q = q.Order(paginationOrder(sinceID, untilID, "id"))
	} else {
		q = q.Order(`"likedCount" DESC`)
	}
	q = q.Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.Flash
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Search performs a substring search across title and summary. cursor 指定時
// は id 順、未指定時は updatedAt DESC で従来通り。
func (r *flashRepository) Search(query, sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(query) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	// upstream FlashService.search: 公開 flash のみを検索対象にする。
	q := r.db.Where("visibility = ?", "public")
	// #2106 N8: upstream は query を空白区切りで各語を AND し sqlLikeEscape する
	// (= 全語を含む検索 + wildcard 無効化)。単一 substring + escape 漏れだった旧実装を
	// flash_like.go の search と同じ word-split + escapeLike に揃える。空 query は語が
	// 無いので visibility=public 全件 (= upstream の ILIKE '%%' と同じ)。
	for _, word := range strings.Fields(query) {
		like := "%" + escapeLike(word) + "%"
		q = q.Where("title ILIKE ? OR summary ILIKE ?", like, like)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" || untilID != "" {
		q = q.Order(paginationOrder(sinceID, untilID, "id"))
	} else {
		q = q.Order(`"updatedAt" DESC`)
	}
	q = q.Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.Flash
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// IncrementCount adjusts a counter column on the flash row by delta.
func (r *flashRepository) IncrementCount(flashID, column string, delta int) error {
	return r.db.Model(&model.Flash{}).
		Where("id = ?", flashID).
		UpdateColumn(column, gorm.Expr("\""+column+"\" + ?", delta)).Error
}
