package repository

import (
	"strings"

	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/gorm"
)

// FlashLikeRepository provides data access for the `flash_like` table.
type FlashLikeRepository interface {
	Create(l *model.FlashLike) error
	Delete(l *model.FlashLike) error
	FindByPair(userID, flashID string) (*model.FlashLike, error)
	Exists(userID, flashID string) (bool, error)
	// ListByUser supports cursor (sinceID/untilID) and offset pagination on
	// flash_like.id (newest first). Cursor 指定時は offset 無視。
	ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.FlashLike, error)
	// ListByUserSearch is ListByUser plus a title/summary ILIKE filter on the
	// joined flash. search is whitespace-split; a row must match every word
	// (each word matches title OR summary). Empty search behaves like ListByUser
	// (#1548, upstream FlashService.myLikes search).
	ListByUserSearch(userID, search, sinceID, untilID string, limit, offset int) ([]*model.FlashLike, error)
	// ListLikedFlashIDs returns the subset of flashIDs that userID has liked, in
	// one query. Used to batch-populate `isLiked` on flash list endpoints
	// (#1548, upstream FlashEntityService.packMany likedFlashIds).
	ListLikedFlashIDs(userID string, flashIDs []string) ([]string, error)
}

type flashLikeRepository struct {
	db *gorm.DB
}

// NewFlashLikeRepository creates a new FlashLikeRepository.
func NewFlashLikeRepository(db *gorm.DB) FlashLikeRepository {
	return &flashLikeRepository{db: db}
}

func (r *flashLikeRepository) Create(l *model.FlashLike) error {
	return r.db.Create(l).Error
}

func (r *flashLikeRepository) Delete(l *model.FlashLike) error {
	return r.db.Delete(l).Error
}

func (r *flashLikeRepository) FindByPair(userID, flashID string) (*model.FlashLike, error) {
	if !storable(userID) || !storable(flashID) {
		return nil, ErrNotFound
	}
	var l model.FlashLike
	if err := r.db.Where("\"userId\" = ? AND \"flashId\" = ?", userID, flashID).First(&l).Error; err != nil {
		return nil, err
	}
	return &l, nil
}

func (r *flashLikeRepository) Exists(userID, flashID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.FlashLike{}).
		Where("\"userId\" = ? AND \"flashId\" = ?", userID, flashID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// likedByUserQuery selects the flash_like rows owned by userID whose flash
// is still visible to userID (public, or owned by userID).
func (r *flashLikeRepository) likedByUserQuery(userID string) *gorm.DB {
	// like した後に非公開化された Flash を一覧に出さない。一覧は script を
	// 含めて返すので、ここを絞らないと flash/show の可視性判定を迂回できる。
	// JOIN なので削除済み Flash の行も同時に落ちる。両表に id/userId 列が
	// あるため Select で flash_like 側を明示する (scan ambiguity 回避)。
	return r.db.Model(&model.FlashLike{}).
		Select(`flash_like.*`).
		Joins(`JOIN flash ON flash.id = flash_like."flashId"`).
		Where(`flash_like."userId" = ?`, userID).
		Where(`(flash.visibility = 'public' OR flash."userId" = ?)`, userID)
}

// ListByUser returns the flash_like rows owned by userID newest first
// (id desc), used by Misskey 互換 i/flashs/likes endpoint. Likes on flashes
// that are no longer visible to userID are excluded.
func (r *flashLikeRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.FlashLike, error) {
	return paginateLikes(r.likedByUserQuery(userID), sinceID, untilID, limit, offset)
}

// ListByUserSearch returns the flash_like rows owned by userID whose joined
// flash matches every search word (title OR summary ILIKE), newest first.
// Empty search delegates to ListByUser. Cursor / offset semantics match
// ListByUser but are anchored on flash_like.id (#1548).
func (r *flashLikeRepository) ListByUserSearch(userID, search, sinceID, untilID string, limit, offset int) ([]*model.FlashLike, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(search) || !storable(userID) {
		return nil, nil
	}
	words := strings.Fields(search)
	if len(words) == 0 {
		return r.ListByUser(userID, sinceID, untilID, limit, offset)
	}
	// 各語を title/summary への ILIKE (語内 OR、語間 AND) で絞る。
	q := r.likedByUserQuery(userID)
	for _, word := range words {
		like := "%" + escapeLike(word) + "%"
		q = q.Where(`flash.title ILIKE ? OR flash.summary ILIKE ?`, like, like)
	}
	return paginateLikes(q, sinceID, untilID, limit, offset)
}

// paginateLikes applies cursor (sinceID/untilID) or offset pagination on
// flash_like.id to q and runs it. Cursor 指定時は offset 無視。
func paginateLikes(q *gorm.DB, sinceID, untilID string, limit, offset int) ([]*model.FlashLike, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	if sinceID != "" {
		q = q.Where(`flash_like.id > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`flash_like.id < ?`, untilID)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "flash_like.id")).Limit(limit)
	if sinceID == "" && untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var rows []*model.FlashLike
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListLikedFlashIDs returns the subset of flashIDs that userID has liked.
func (r *flashLikeRepository) ListLikedFlashIDs(userID string, flashIDs []string) ([]string, error) {
	if len(flashIDs) == 0 {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.FlashLike{}).
		Where(`"userId" = ? AND "flashId" IN ?`, userID, flashIDs).
		Pluck(`"flashId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
