package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UserPublickeyRepository provides data access for remote user public keys.
type UserPublickeyRepository interface {
	Upsert(pk *model.UserPublickey) error
	FindByUserID(userID string) (*model.UserPublickey, error)
	FindByKeyID(keyID string) (*model.UserPublickey, error)
	Delete(userID string) error
}

type userPublickeyRepository struct {
	db *gorm.DB
}

// NewUserPublickeyRepository creates a new UserPublickeyRepository.
func NewUserPublickeyRepository(db *gorm.DB) UserPublickeyRepository {
	return &userPublickeyRepository{db: db}
}

func (r *userPublickeyRepository) Upsert(pk *model.UserPublickey) error {
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "userId"}},
		DoUpdates: clause.AssignmentColumns([]string{"keyId", "keyPem"}),
	}).Create(pk).Error
}

func (r *userPublickeyRepository) FindByUserID(userID string) (*model.UserPublickey, error) {
	if !storable(userID) {
		return nil, ErrNotFound
	}
	var pk model.UserPublickey
	if err := r.db.Where(`"userId" = ?`, userID).First(&pk).Error; err != nil {
		return nil, err
	}
	return &pk, nil
}

// FindByKeyID looks up an RSA primary key by keyId alone (global). This is safe
// ONLY because the resolver enforces a write-time invariant that a row's keyId
// host always equals its owning actor's host (fetchActor の publicKey.id host
// binding, Fix A)。この不変条件が崩れると、攻撃者が victim の keyId で自分の鍵を
// 植え込んで LD-Signature 経路の verify をすり抜ける key confusion が再発する。
// 新たに keyId を保存する経路を足すときは必ず host binding を通すこと
// (#security: HTTP-sig key confusion)。
func (r *userPublickeyRepository) FindByKeyID(keyID string) (*model.UserPublickey, error) {
	if !storable(keyID) {
		return nil, ErrNotFound
	}
	var pk model.UserPublickey
	if err := r.db.Where(`"keyId" = ?`, keyID).First(&pk).Error; err != nil {
		return nil, err
	}
	return &pk, nil
}

func (r *userPublickeyRepository) Delete(userID string) error {
	return r.db.Where(`"userId" = ?`, userID).Delete(&model.UserPublickey{}).Error
}
