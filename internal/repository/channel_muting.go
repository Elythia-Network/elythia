package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChannelMutingRepository provides data access for channel muting.
type ChannelMutingRepository interface {
	Create(m *model.ChannelMuting) error
	Delete(userID, channelID string) error
	// ListByUser returns the user's *active* (non-expired) channel mutes.
	// Timed mutes whose expiresAt has passed are excluded (#1603), matching
	// upstream which treats an expired mute as not muting.
	ListByUser(userID string) ([]*model.ChannelMuting, error)
	// Exists reports whether the user has an *active* (non-expired) mute on
	// the channel.
	Exists(userID, channelID string) (bool, error)
	// DeleteExpired physically removes channel mutes whose expiresAt has
	// passed. Read paths already exclude them; this is the active prune run
	// by the checkExpiredMutings cron (#1603).
	//
	// Returns the userId of every deleted row — **one entry per row**, so
	// duplicates appear when a single user had several mutes expire at once
	// and len() equals the old RowsAffected return. 所有者を返す理由は
	// MutingRepository.DeleteExpired と同じ (#2453)。
	DeleteExpired(now time.Time) ([]string, error)
}

type channelMutingRepository struct {
	db *gorm.DB
}

// NewChannelMutingRepository creates a new ChannelMutingRepository.
func NewChannelMutingRepository(db *gorm.DB) ChannelMutingRepository {
	return &channelMutingRepository{db: db}
}

func (r *channelMutingRepository) Create(m *model.ChannelMuting) error {
	// channel_muting には UNIQUE("userId","channelId") があり (migration 000032)、
	// 期限切れ (未prune) の行が残ったまま再muteすると plain INSERT が制約違反で
	// 500 になる (#1603)。能動的muteは呼び出し側 Exists で 204 short-circuit され
	// るため、ここに来るのは「行なし」か「期限切れ行」のみ。後者は expiresAt を
	// 更新して mute を再活性化させる upsert にする。
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "userId"}, {Name: "channelId"}},
		DoUpdates: clause.AssignmentColumns([]string{"expiresAt"}),
	}).Create(m).Error
}

func (r *channelMutingRepository) Delete(userID, channelID string) error {
	return r.db.Where(`"userId" = ? AND "channelId" = ?`, userID, channelID).
		Delete(&model.ChannelMuting{}).Error
}

func (r *channelMutingRepository) ListByUser(userID string) ([]*model.ChannelMuting, error) {
	var mutes []*model.ChannelMuting
	if err := r.db.
		Where(`"userId" = ?`, userID).
		Where(`"expiresAt" IS NULL OR "expiresAt" > ?`, time.Now()).
		Find(&mutes).Error; err != nil {
		return nil, err
	}
	return mutes, nil
}

func (r *channelMutingRepository) Exists(userID, channelID string) (bool, error) {
	var count int64
	err := r.db.Model(&model.ChannelMuting{}).
		Where(`"userId" = ? AND "channelId" = ?`, userID, channelID).
		Where(`"expiresAt" IS NULL OR "expiresAt" > ?`, time.Now()).
		Count(&count).Error
	return count > 0, err
}

func (r *channelMutingRepository) DeleteExpired(now time.Time) ([]string, error) {
	// mutingRepository.DeleteExpired と同じく DELETE ... RETURNING で 1 文にする。
	var deleted []model.ChannelMuting
	res := r.db.
		Clauses(clause.Returning{Columns: []clause.Column{{Name: "userId"}}}).
		Where(`"expiresAt" IS NOT NULL AND "expiresAt" < ?`, now).
		Delete(&deleted)
	if res.Error != nil {
		return nil, res.Error
	}
	userIDs := make([]string, 0, len(deleted))
	for _, m := range deleted {
		userIDs = append(userIDs, m.UserID)
	}
	return userIDs, nil
}
