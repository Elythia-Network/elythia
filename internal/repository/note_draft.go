package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// NoteDraftRepository handles note_draft persistence.
type NoteDraftRepository interface {
	Create(draft *model.NoteDraft) error
	FindByIDAndUser(id, userID string) (*model.NoteDraft, error)
	// FindByID returns the draft by primary key without ownership check.
	// Used by internal worker paths (= scheduled note processor) that act on
	// behalf of the draft owner stored in the row (#1040)。
	FindByID(id string) (*model.NoteDraft, error)
	// ListByUser lists a user's drafts with keyset pagination. Ordering follows
	// paginationOrder, so a sinceID-only page comes back oldest-first.
	// sinceID/untilID page on the draft id (aidx, time-ordered) mirroring
	// upstream makePaginationQuery; scheduled, when non-nil, filters on
	// isActuallyScheduled. mk-go は他 repo と同様 id-based keyset のみで、
	// upstream の sinceDate/untilDate (NoteDraft に createdAt 列が無い) は
	// 非対応 (#1538)。
	ListByUser(userID, sinceID, untilID string, scheduled *bool, limit int) ([]*model.NoteDraft, error)
	Update(draft *model.NoteDraft) error
	// Delete returns the number of rows affected so callers can detect
	// "no such draft" without a separate FindByIDAndUser pre-check (single
	// SQL round-trip + no TOCTOU race)。0 = 該当 draft なし、1 = 削除成功。
	Delete(id, userID string) (int64, error)
	CountByUser(userID string) (int64, error)
	// CountScheduledByUser returns the number of drafts owned by userID that
	// are flagged with isActuallyScheduled=true. Used to enforce
	// scheduledNoteLimit role policy (#1040)。
	CountScheduledByUser(userID string) (int64, error)
}

type noteDraftRepository struct {
	db *gorm.DB
}

// NewNoteDraftRepository creates a new NoteDraftRepository.
func NewNoteDraftRepository(db *gorm.DB) NoteDraftRepository {
	return &noteDraftRepository{db: db}
}

func (r *noteDraftRepository) Create(draft *model.NoteDraft) error {
	return r.db.Create(draft).Error
}

func (r *noteDraftRepository) FindByIDAndUser(id, userID string) (*model.NoteDraft, error) {
	if !storable(id) || !storable(userID) {
		return nil, ErrNotFound
	}
	var d model.NoteDraft
	if err := r.db.Where(`"id" = ? AND "userId" = ?`, id, userID).First(&d).Error; err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *noteDraftRepository) ListByUser(userID, sinceID, untilID string, scheduled *bool, limit int) ([]*model.NoteDraft, error) {
	if limit <= 0 {
		limit = 30
	}
	q := r.db.Where(`"userId" = ?`, userID)
	// id は note_draft.id (aidx) で keyset pagination する。note_favorite と
	// 同じ向き: untilId は < cursor (DESC で次ページ)、sinceId は > cursor。
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if scheduled != nil {
		q = q.Where(`"isActuallyScheduled" = ?`, *scheduled)
	}
	var drafts []*model.NoteDraft
	// upstream notes/drafts/list.ts は makePaginationQuery を使う (#2713)。
	if err := q.Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit).Find(&drafts).Error; err != nil {
		return nil, err
	}
	return drafts, nil
}

func (r *noteDraftRepository) Update(draft *model.NoteDraft) error {
	return r.db.Save(draft).Error
}

func (r *noteDraftRepository) Delete(id, userID string) (int64, error) {
	tx := r.db.Where(`"id" = ? AND "userId" = ?`, id, userID).Delete(&model.NoteDraft{})
	return tx.RowsAffected, tx.Error
}

func (r *noteDraftRepository) CountByUser(userID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.NoteDraft{}).Where(`"userId" = ?`, userID).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r *noteDraftRepository) FindByID(id string) (*model.NoteDraft, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var d model.NoteDraft
	if err := r.db.Where(`"id" = ?`, id).First(&d).Error; err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *noteDraftRepository) CountScheduledByUser(userID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.NoteDraft{}).
		Where(`"userId" = ? AND "isActuallyScheduled" = true`, userID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
