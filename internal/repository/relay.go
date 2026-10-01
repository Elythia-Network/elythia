package repository

import (
	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/gorm"
)

// RelayRepository provides data access for the `relay` table.
type RelayRepository interface {
	Create(r *model.Relay) error
	FindByID(id string) (*model.Relay, error)
	List() ([]*model.Relay, error)
	ListByStatus(status string) ([]*model.Relay, error)
	// UpdateStatusFrom sets status to `to` only when the row's current status
	// is `from`, and reports whether a row was updated.
	UpdateStatusFrom(id, from, to string) (bool, error)
	Delete(id string) error
}

type relayRepository struct {
	db *gorm.DB
}

// NewRelayRepository creates a RelayRepository backed by gorm.
func NewRelayRepository(db *gorm.DB) RelayRepository {
	return &relayRepository{db: db}
}

func (r *relayRepository) Create(rel *model.Relay) error {
	return r.db.Create(rel).Error
}

func (r *relayRepository) FindByID(id string) (*model.Relay, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var rel model.Relay
	if err := r.db.Where(`"id" = ?`, id).First(&rel).Error; err != nil {
		return nil, err
	}
	return &rel, nil
}

func (r *relayRepository) List() ([]*model.Relay, error) {
	var rels []*model.Relay
	if err := r.db.Order(`"id" DESC`).Find(&rels).Error; err != nil {
		return nil, err
	}
	return rels, nil
}

func (r *relayRepository) ListByStatus(status string) ([]*model.Relay, error) {
	var rels []*model.Relay
	if err := r.db.Where(`"status" = ?`, status).Find(&rels).Error; err != nil {
		return nil, err
	}
	return rels, nil
}

// UpdateStatusFrom は現在の status を条件に含めた 1 文の UPDATE にする。
// 読んでから書く 2 段にすると、遅れて届いた Accept / Reject が判定の後に
// 割り込んで、確定済みの relay を書き換えうる (本家 updateRequestingRelayStatus も
// `{ id, status: 'requesting' }` を条件に update する)。
func (r *relayRepository) UpdateStatusFrom(id, from, to string) (bool, error) {
	res := r.db.Model(&model.Relay{}).
		Where(`"id" = ? AND "status" = ?`, id, from).
		Update("status", to)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *relayRepository) Delete(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.Relay{}).Error
}
