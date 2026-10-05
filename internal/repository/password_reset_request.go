package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// PasswordResetRequestRepository provides data access for password reset requests.
type PasswordResetRequestRepository interface {
	Create(req *model.PasswordResetRequest) error
	FindByToken(token string) (*model.PasswordResetRequest, error)
	Delete(id string) error
}

type passwordResetRequestRepository struct {
	db *gorm.DB
}

// NewPasswordResetRequestRepository creates a new PasswordResetRequestRepository.
func NewPasswordResetRequestRepository(db *gorm.DB) PasswordResetRequestRepository {
	return &passwordResetRequestRepository{db: db}
}

func (r *passwordResetRequestRepository) Create(req *model.PasswordResetRequest) error {
	return r.db.Create(req).Error
}

func (r *passwordResetRequestRepository) FindByToken(token string) (*model.PasswordResetRequest, error) {
	if !storable(token) {
		return nil, ErrNotFound
	}
	var req model.PasswordResetRequest
	if err := r.db.Where(`"token" = ?`, token).First(&req).Error; err != nil {
		return nil, err
	}
	return &req, nil
}

func (r *passwordResetRequestRepository) Delete(id string) error {
	return r.db.Delete(&model.PasswordResetRequest{}, `"id" = ?`, id).Error
}
