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

// DeleteByAuthorAndQuoter removes the approvals authorID gave to quoterID's
// quotes and returns them (#3234 段階 4、作者が相手をブロックしたとき)。消した行は
// 取り消しの Delete を組み立てるのに使う。
func (r *NoteQuoteAuthorizationRepository) DeleteByAuthorAndQuoter(authorID, quoterID string) ([]model.NoteQuoteAuthorization, error) {
	if !storable(authorID) || !storable(quoterID) {
		return nil, nil
	}
	var rows []model.NoteQuoteAuthorization
	if err := r.db.Raw(`DELETE FROM "note_quote_authorization" a USING "note" n
		WHERE a."noteId" = n."id" AND n."userId" = ? AND a."quoterId" = ?
		RETURNING a.*`, authorID, quoterID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Remove deletes quoterID's approval of quotingURI for the note, if any.
// 相手で絞る — quotingURI は拒否の経路では確かめていない値なので、他人の承認を
// 消させない。
func (r *NoteQuoteAuthorizationRepository) Remove(noteID, quotingURI, quoterID string) error {
	if !storable(noteID) || !storable(quotingURI) || !storable(quoterID) {
		return nil
	}
	return r.db.Where(`"noteId" = ? AND "quotingUri" = ? AND "quoterId" = ?`, noteID, quotingURI, quoterID).
		Delete(&model.NoteQuoteAuthorization{}).Error
}
