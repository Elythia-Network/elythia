package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// UsedUsernameRepository provides data access for the used_username table.
type UsedUsernameRepository interface {
	Create(username string) error
	Exists(username string) (bool, error)
}

type usedUsernameRepository struct {
	db *gorm.DB
}

// NewUsedUsernameRepository creates a new UsedUsernameRepository.
func NewUsedUsernameRepository(db *gorm.DB) UsedUsernameRepository {
	return &usedUsernameRepository{db: db}
}

func (r *usedUsernameRepository) Create(username string) error {
	return r.db.Create(&model.UsedUsername{Username: username}).Error
}

func (r *usedUsernameRepository) Exists(username string) (bool, error) {
	var count int64
	err := r.db.Model(&model.UsedUsername{}).Where(`"username" = ?`, username).Count(&count).Error
	return count > 0, err
}
