package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// SystemAccountRepository provides data access for system accounts.
type SystemAccountRepository interface {
	FindByType(typ string) (*model.SystemAccount, error)
	Create(sa *model.SystemAccount) error
}

type systemAccountRepository struct {
	db *gorm.DB
}

// NewSystemAccountRepository creates a new SystemAccountRepository.
func NewSystemAccountRepository(db *gorm.DB) SystemAccountRepository {
	return &systemAccountRepository{db: db}
}

func (r *systemAccountRepository) FindByType(typ string) (*model.SystemAccount, error) {
	if !storable(typ) {
		return nil, ErrNotFound
	}
	var sa model.SystemAccount
	if err := r.db.Where(`"type" = ?`, typ).First(&sa).Error; err != nil {
		return nil, err
	}
	return &sa, nil
}

func (r *systemAccountRepository) Create(sa *model.SystemAccount) error {
	return r.db.Create(sa).Error
}
