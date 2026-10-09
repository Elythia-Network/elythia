package repository

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/elythia-network/elythia/internal/model"
)

// SilentFollowRepository stores the follows that succeeded without a
// notification because of the followee's followApprovalAction "silentFollow"
// (#3466, Elythia-only).
type SilentFollowRepository interface {
	// Record stores that followerID followed followeeID silently. A second
	// record for the same pair replaces the ID, so the row moves to the top
	// of List.
	Record(row *model.SilentFollow) error
	// ListByFollowee returns the silent follows toward followeeID, newest
	// first (oldest first when only sinceID is given, like other paginated
	// lists). limit outside 1..100 falls back to 30.
	ListByFollowee(followeeID string, limit int, sinceID, untilID string) ([]*model.SilentFollow, error)
}

type silentFollowRepository struct {
	db *gorm.DB
}

// NewSilentFollowRepository creates a SilentFollowRepository.
func NewSilentFollowRepository(db *gorm.DB) SilentFollowRepository {
	return &silentFollowRepository{db: db}
}

func (r *silentFollowRepository) Record(row *model.SilentFollow) error {
	// 同じ人から何度来ても 1 行にまとめ、id (= 日時) を最後のものにする。
	// 一覧の並びが「最後に来た順」になる。
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "followeeId"}, {Name: "followerId"}},
		DoUpdates: clause.AssignmentColumns([]string{"id"}),
	}).Create(row).Error
}

func (r *silentFollowRepository) ListByFollowee(followeeID string, limit int, sinceID, untilID string) ([]*model.SilentFollow, error) {
	if !storable(followeeID) || !storable(sinceID) || !storable(untilID) {
		return []*model.SilentFollow{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	q := r.db.Where(`"followeeId" = ?`, followeeID).Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	rows := []*model.SilentFollow{}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
