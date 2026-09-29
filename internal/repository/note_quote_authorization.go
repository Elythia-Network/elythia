package repository

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/shiroha-a/mk/internal/model"
)

// NoteQuoteAuthorizationRepository stores quote approvals granted by local
// authors (#3234, FEP-044f).
type NoteQuoteAuthorizationRepository struct {
	db *gorm.DB
}

// NewNoteQuoteAuthorizationRepository constructs a NoteQuoteAuthorizationRepository.
func NewNoteQuoteAuthorizationRepository(db *gorm.DB) *NoteQuoteAuthorizationRepository {
	return &NoteQuoteAuthorizationRepository{db: db}
}

// FindByIDAndNoteID returns the approval with the given id for the given note,
// or ErrNotFound.
func (r *NoteQuoteAuthorizationRepository) FindByIDAndNoteID(id, noteID string) (*model.NoteQuoteAuthorization, error) {
	if !storable(id) || !storable(noteID) {
		return nil, ErrNotFound
	}
	var a model.NoteQuoteAuthorization
	if err := r.db.Where(`"id" = ? AND "noteId" = ?`, id, noteID).Take(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// FindByNoteIDAndQuotingURI returns the approval of quotingURI for the note, or
// ErrNotFound.
func (r *NoteQuoteAuthorizationRepository) FindByNoteIDAndQuotingURI(noteID, quotingURI string) (*model.NoteQuoteAuthorization, error) {
	if !storable(noteID) || !storable(quotingURI) {
		return nil, ErrNotFound
	}
	var a model.NoteQuoteAuthorization
	if err := r.db.Where(`"noteId" = ? AND "quotingUri" = ?`, noteID, quotingURI).Take(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// Ensure records the approval unless one already exists for (noteId,
// quotingUri), and returns the stored row. 同じ引用の QuoteRequest は再送され
// うるので、同じ承認 (同じ URI) を返し続ける。
func (r *NoteQuoteAuthorizationRepository) Ensure(a *model.NoteQuoteAuthorization) (*model.NoteQuoteAuthorization, error) {
	res := r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(a)
	if res.Error != nil {
		return nil, res.Error
	}
	return r.FindByNoteIDAndQuotingURI(a.NoteID, a.QuotingURI)
}
