package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// AvatarDecorationRepository handles persistence for the `avatar_decoration`
// table. Avatar decorations are instance-wide assets administrators can attach
// to user profiles.
type AvatarDecorationRepository interface {
	Create(d *model.AvatarDecoration) error
	FindByID(id string) (*model.AvatarDecoration, error)
	List() ([]*model.AvatarDecoration, error)
	UpdateFields(id string, fields map[string]any) error
	Delete(id string) error
}

type avatarDecorationRepository struct {
	db *gorm.DB
}

// NewAvatarDecorationRepository returns a GORM-backed AvatarDecorationRepository.
func NewAvatarDecorationRepository(db *gorm.DB) AvatarDecorationRepository {
	return &avatarDecorationRepository{db: db}
}

func (r *avatarDecorationRepository) Create(d *model.AvatarDecoration) error {
	return r.db.Create(d).Error
}

func (r *avatarDecorationRepository) FindByID(id string) (*model.AvatarDecoration, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var d model.AvatarDecoration
	if err := r.db.Where(`"id" = ?`, id).First(&d).Error; err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *avatarDecorationRepository) List() ([]*model.AvatarDecoration, error) {
	var rows []*model.AvatarDecoration
	if err := r.db.Order(`"id" DESC`).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *avatarDecorationRepository) UpdateFields(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.AvatarDecoration{}).Where(`"id" = ?`, id).Updates(fields).Error
}

func (r *avatarDecorationRepository) Delete(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.AvatarDecoration{}).Error
}
