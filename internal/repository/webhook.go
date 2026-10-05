package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// WebhookRepository handles webhook persistence.
type WebhookRepository interface {
	Create(webhook *model.Webhook) error
	FindByID(id string) (*model.Webhook, error)
	FindByIDAndUserID(id, userID string) (*model.Webhook, error)
	ListByUserID(userID string) ([]*model.Webhook, error)
	// CountByUserID returns the number of webhooks owned by userID. Used by
	// webhookLimit policy gate (#1029 PR-1 follow-up).
	CountByUserID(userID string) (int64, error)
	ListActiveByUserID(userID string) ([]*model.Webhook, error)
	Update(webhook *model.Webhook) error
	UpdateLatestStatus(id string, sentAt time.Time, status int) error
	Delete(id, userID string) error
}

type webhookRepository struct {
	db *gorm.DB
}

// NewWebhookRepository creates a new WebhookRepository.
func NewWebhookRepository(db *gorm.DB) WebhookRepository {
	return &webhookRepository{db: db}
}

func (r *webhookRepository) Create(webhook *model.Webhook) error {
	return r.db.Create(webhook).Error
}

func (r *webhookRepository) FindByID(id string) (*model.Webhook, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var w model.Webhook
	if err := r.db.Where(`"id" = ?`, id).First(&w).Error; err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *webhookRepository) FindByIDAndUserID(id, userID string) (*model.Webhook, error) {
	if !storable(id) || !storable(userID) {
		return nil, ErrNotFound
	}
	var w model.Webhook
	if err := r.db.Where(`"id" = ? AND "userId" = ?`, id, userID).First(&w).Error; err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *webhookRepository) ListByUserID(userID string) ([]*model.Webhook, error) {
	var webhooks []*model.Webhook
	if err := r.db.Where(`"userId" = ?`, userID).Order(`"id" DESC`).Find(&webhooks).Error; err != nil {
		return nil, err
	}
	return webhooks, nil
}

// CountByUserID returns the number of webhooks owned by the given user.
func (r *webhookRepository) CountByUserID(userID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.Webhook{}).
		Where(`"userId" = ?`, userID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r *webhookRepository) ListActiveByUserID(userID string) ([]*model.Webhook, error) {
	var webhooks []*model.Webhook
	if err := r.db.Where(`"userId" = ? AND active = ?`, userID, true).Find(&webhooks).Error; err != nil {
		return nil, err
	}
	return webhooks, nil
}

func (r *webhookRepository) Update(webhook *model.Webhook) error {
	return r.db.Save(webhook).Error
}

// UpdateLatestStatus updates latestSentAt/latestStatus atomically for a single
// webhook row without touching any other fields.
func (r *webhookRepository) UpdateLatestStatus(id string, sentAt time.Time, status int) error {
	return r.db.Model(&model.Webhook{}).
		Where(`"id" = ?`, id).
		Updates(map[string]any{
			"latestSentAt": sentAt,
			"latestStatus": status,
		}).Error
}

func (r *webhookRepository) Delete(id, userID string) error {
	return r.db.Where(`"id" = ? AND "userId" = ?`, id, userID).Delete(&model.Webhook{}).Error
}
