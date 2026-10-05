package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// SystemWebhookRepository handles system_webhook persistence. System webhooks
// are instance-wide (not per-user) and are managed by administrators via
// admin/system-webhook/* endpoints.
type SystemWebhookRepository interface {
	Create(w *model.SystemWebhook) error
	FindByID(id string) (*model.SystemWebhook, error)
	List() ([]*model.SystemWebhook, error)
	ListActive() ([]*model.SystemWebhook, error)
	Update(w *model.SystemWebhook) error
	UpdateAdminFields(id string, fields map[string]any) error
	UpdateLatestStatus(id string, sentAt time.Time, status int) error
	Delete(id string) error
}

type systemWebhookRepository struct {
	db *gorm.DB
}

// NewSystemWebhookRepository returns a GORM-backed SystemWebhookRepository.
func NewSystemWebhookRepository(db *gorm.DB) SystemWebhookRepository {
	return &systemWebhookRepository{db: db}
}

func (r *systemWebhookRepository) Create(w *model.SystemWebhook) error {
	return r.db.Create(w).Error
}

func (r *systemWebhookRepository) FindByID(id string) (*model.SystemWebhook, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var w model.SystemWebhook
	if err := r.db.Where(`"id" = ?`, id).First(&w).Error; err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *systemWebhookRepository) List() ([]*model.SystemWebhook, error) {
	var rows []*model.SystemWebhook
	if err := r.db.Order(`"id" DESC`).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *systemWebhookRepository) ListActive() ([]*model.SystemWebhook, error) {
	var rows []*model.SystemWebhook
	if err := r.db.Where(`"isActive" = ?`, true).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *systemWebhookRepository) Update(w *model.SystemWebhook) error {
	return r.db.Save(w).Error
}

// UpdateAdminFields applies a partial update for admin-editable columns only.
// latestSentAt/latestStatus は配送 processor 側で atomic に書き込まれるため、
// admin 経路の Update で上書きしないようこちらを使う。
func (r *systemWebhookRepository) UpdateAdminFields(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.SystemWebhook{}).
		Where(`"id" = ?`, id).
		Updates(fields).Error
}

// UpdateLatestStatus atomically updates only latestSentAt/latestStatus on a
// single row. Used by the delivery processor to record the outcome of each
// HTTP attempt.
func (r *systemWebhookRepository) UpdateLatestStatus(id string, sentAt time.Time, status int) error {
	return r.db.Model(&model.SystemWebhook{}).
		Where(`"id" = ?`, id).
		Updates(map[string]any{
			"latestSentAt": sentAt,
			"latestStatus": status,
		}).Error
}

func (r *systemWebhookRepository) Delete(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.SystemWebhook{}).Error
}
