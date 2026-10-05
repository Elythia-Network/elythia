package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// AbuseReportNotificationRecipientRepository handles persistence for the
// `abuse_report_notification_recipient` table. Recipients are instance-wide
// (administrator-managed) and determine who is notified when a user abuse
// report is filed.
type AbuseReportNotificationRecipientRepository interface {
	Create(r *model.AbuseReportNotificationRecipient) error
	FindByID(id string) (*model.AbuseReportNotificationRecipient, error)
	List() ([]*model.AbuseReportNotificationRecipient, error)
	Update(id string, fields map[string]any) error
	Delete(id string) error
}

type abuseReportNotificationRecipientRepository struct {
	db *gorm.DB
}

// NewAbuseReportNotificationRecipientRepository returns a GORM-backed
// AbuseReportNotificationRecipientRepository.
func NewAbuseReportNotificationRecipientRepository(db *gorm.DB) AbuseReportNotificationRecipientRepository {
	return &abuseReportNotificationRecipientRepository{db: db}
}

func (r *abuseReportNotificationRecipientRepository) Create(rec *model.AbuseReportNotificationRecipient) error {
	return r.db.Create(rec).Error
}

func (r *abuseReportNotificationRecipientRepository) FindByID(id string) (*model.AbuseReportNotificationRecipient, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var rec model.AbuseReportNotificationRecipient
	if err := r.db.Where(`"id" = ?`, id).First(&rec).Error; err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *abuseReportNotificationRecipientRepository) List() ([]*model.AbuseReportNotificationRecipient, error) {
	var rows []*model.AbuseReportNotificationRecipient
	if err := r.db.Order(`"id" DESC`).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Update applies partial field updates by primary key. Callers should pass
// column names (camelCase, matching GORM column tags) as map keys.
func (r *abuseReportNotificationRecipientRepository) Update(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.AbuseReportNotificationRecipient{}).
		Where(`"id" = ?`, id).
		Updates(fields).Error
}

func (r *abuseReportNotificationRecipientRepository) Delete(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.AbuseReportNotificationRecipient{}).Error
}
