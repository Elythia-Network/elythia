package repository

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/elythia-network/elythia/internal/model"
)

// RelayObservedUserRepository records which remote users were first observed
// through a subscribed relay (#2340).
type RelayObservedUserRepository interface {
	// MarkObserved records userID as relay-derived. 既に記録済みなら何もしない。
	MarkObserved(userID string) error
}

type relayObservedUserRepository struct{ db *gorm.DB }

// NewRelayObservedUserRepository creates the repository.
func NewRelayObservedUserRepository(db *gorm.DB) RelayObservedUserRepository {
	return &relayObservedUserRepository{db: db}
}

// MarkObserved inserts the marker, ignoring a duplicate.
//
// 重複は起こりうる (同じ actor を複数の inbox job が並行に解決する)。観測時刻は
// 最初のものを残す — 掃除の猶予は「いつリレーで見かけ始めたか」を基準にする方が
// 直感に合う。
func (r *relayObservedUserRepository) MarkObserved(userID string) error {
	if userID == "" {
		return nil
	}
	row := &model.RelayObservedUser{UserID: userID, ObservedAt: time.Now()}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error
}
