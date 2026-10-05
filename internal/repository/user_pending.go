package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// UserPendingRepository manages pending user registrations (invite-based
// signup with email confirmation).
type UserPendingRepository interface {
	Create(p *model.UserPending) error
	FindByCode(code string) (*model.UserPending, error)
	Delete(id string) error
	// DeleteOlderThan removes rows whose id sorts before thresholdID and
	// reports how many were deleted.
	//
	// **期限切れの行を残さない (#3037)。** `PromotePending` は期限切れを
	// 拒否するだけで行を消さないので、`user_pending` は**放置された登録の
	// メールアドレスとパスワードハッシュを無期限に貯め続ける**。DB が漏れた
	// ときに出ていく量が、登録を完了しなかった人のぶんだけ増える。
	//
	// id は時刻を含む (aidx / ULID) ので、閾値も id で表す
	// (`ReversiRepository.DeleteOutdatedGames` と同じ形)。
	DeleteOlderThan(thresholdID string) (int64, error)
}

type userPendingRepository struct {
	db *gorm.DB
}

// NewUserPendingRepository creates a new UserPendingRepository.
func NewUserPendingRepository(db *gorm.DB) UserPendingRepository {
	return &userPendingRepository{db: db}
}

func (r *userPendingRepository) Create(p *model.UserPending) error {
	return r.db.Create(p).Error
}

func (r *userPendingRepository) FindByCode(code string) (*model.UserPending, error) {
	if !storable(code) {
		return nil, ErrNotFound
	}
	var row model.UserPending
	if err := r.db.Where("code = ?", code).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *userPendingRepository) Delete(id string) error {
	return r.db.Where("id = ?", id).Delete(&model.UserPending{}).Error
}

func (r *userPendingRepository) DeleteOlderThan(thresholdID string) (int64, error) {
	// **列に入らない値を投げない (#3025)。** id は varchar なので NUL や
	// 不正な UTF-8 を渡すとクエリごと落ちる。呼び出し元は id generator なので
	// 通常は起きないが、guard の有無が経路ごとに違う状態を作らない。
	if !storable(thresholdID) {
		return 0, nil
	}
	res := r.db.Where(`"id" < ?`, thresholdID).Delete(&model.UserPending{})
	return res.RowsAffected, res.Error
}
