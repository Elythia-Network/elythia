package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// UserSecurityKeyRepository provides data access for `user_security_key`.
type UserSecurityKeyRepository interface {
	Create(key *model.UserSecurityKey) error
	FindByID(id string) (*model.UserSecurityKey, error)
	ListByUser(userID string) ([]*model.UserSecurityKey, error)
	UpdateName(id, userID, name string) error
	UpdateCounter(id string, counter int64) error
	Delete(id, userID string) error
	DeleteByUser(userID string) error
	CountByUser(userID string) (int64, error)
}

type userSecurityKeyRepository struct {
	db *gorm.DB
}

// NewUserSecurityKeyRepository constructs the repository.
func NewUserSecurityKeyRepository(db *gorm.DB) UserSecurityKeyRepository {
	return &userSecurityKeyRepository{db: db}
}

func (r *userSecurityKeyRepository) Create(key *model.UserSecurityKey) error {
	if key.LastUsed.IsZero() {
		key.LastUsed = time.Now()
	}
	return r.db.Create(key).Error
}

func (r *userSecurityKeyRepository) FindByID(id string) (*model.UserSecurityKey, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var key model.UserSecurityKey
	if err := r.db.Where("id = ?", id).First(&key).Error; err != nil {
		return nil, err
	}
	return &key, nil
}

func (r *userSecurityKeyRepository) ListByUser(userID string) ([]*model.UserSecurityKey, error) {
	var keys []*model.UserSecurityKey
	if err := r.db.Where(`"userId" = ?`, userID).Order(`"lastUsed" DESC`).Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// UpdateName allows the owning user to rename a key. The userID guard prevents
// cross-user mutation even if a credential ID leaks.
func (r *userSecurityKeyRepository) UpdateName(id, userID, name string) error {
	res := r.db.Model(&model.UserSecurityKey{}).
		Where(`id = ? AND "userId" = ?`, id, userID).
		Update("name", name)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UpdateCounter advances the counter and lastUsed timestamp after a successful
// authentication assertion. Counter regression is the caller's responsibility
// to detect (clone-warning per WebAuthn spec).
func (r *userSecurityKeyRepository) UpdateCounter(id string, counter int64) error {
	return r.db.Model(&model.UserSecurityKey{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"counter":  counter,
			"lastUsed": time.Now(),
		}).Error
}

func (r *userSecurityKeyRepository) Delete(id, userID string) error {
	res := r.db.Where(`id = ? AND "userId" = ?`, id, userID).Delete(&model.UserSecurityKey{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// DeleteByUser removes every security key registered by the user. Used by
// admin/unset-mfa (upstream 2026.7.0). 0 行削除はエラーにしない (パスキー
// 未登録ユーザーへの unset-mfa も成功扱いにする upstream 準拠)。
func (r *userSecurityKeyRepository) DeleteByUser(userID string) error {
	return r.db.Where(`"userId" = ?`, userID).Delete(&model.UserSecurityKey{}).Error
}

func (r *userSecurityKeyRepository) CountByUser(userID string) (int64, error) {
	var n int64
	if err := r.db.Model(&model.UserSecurityKey{}).Where(`"userId" = ?`, userID).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}
