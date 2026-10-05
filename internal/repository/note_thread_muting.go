package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// NoteThreadMutingRepository provides data access for note thread muting.
type NoteThreadMutingRepository interface {
	Create(m *model.NoteThreadMuting) error
	Delete(userID, threadID string) error
	Exists(userID, threadID string) (bool, error)
	// ListMutedThreadIDs returns the threadIds the user has muted. read 経路の
	// thread-mute filter (upstream generateMutedNoteThreadQuery) 用 (#1554)。
	ListMutedThreadIDs(userID string) ([]string, error)
}

type noteThreadMutingRepository struct {
	db *gorm.DB
}

// NewNoteThreadMutingRepository creates a new NoteThreadMutingRepository.
func NewNoteThreadMutingRepository(db *gorm.DB) NoteThreadMutingRepository {
	return &noteThreadMutingRepository{db: db}
}

func (r *noteThreadMutingRepository) Create(m *model.NoteThreadMuting) error {
	return r.db.Create(m).Error
}

func (r *noteThreadMutingRepository) Delete(userID, threadID string) error {
	return r.db.Where(`"userId" = ? AND "threadId" = ?`, userID, threadID).
		Delete(&model.NoteThreadMuting{}).Error
}

func (r *noteThreadMutingRepository) Exists(userID, threadID string) (bool, error) {
	var count int64
	err := r.db.Model(&model.NoteThreadMuting{}).
		Where(`"userId" = ? AND "threadId" = ?`, userID, threadID).Count(&count).Error
	return count > 0, err
}

func (r *noteThreadMutingRepository) ListMutedThreadIDs(userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	var ids []string
	if err := r.db.Model(&model.NoteThreadMuting{}).
		Where(`"userId" = ?`, userID).
		Pluck(`"threadId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
