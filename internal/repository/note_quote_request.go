package repository

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/shiroha-a/mk/internal/model"
)

// NoteQuoteRequestRepository stores the quote requests local notes sent to the
// authors of the remote notes they quote, and their answers (#3234, FEP-044f).
type NoteQuoteRequestRepository struct {
	db *gorm.DB
}

// NewNoteQuoteRequestRepository constructs a NoteQuoteRequestRepository.
func NewNoteQuoteRequestRepository(db *gorm.DB) *NoteQuoteRequestRepository {
	return &NoteQuoteRequestRepository{db: db}
}

// FindByNoteID returns the request of the quoting note, or ErrNotFound.
func (r *NoteQuoteRequestRepository) FindByNoteID(noteID string) (*model.NoteQuoteRequest, error) {
	if !storable(noteID) {
		return nil, ErrNotFound
	}
	var q model.NoteQuoteRequest
	if err := r.db.Where(`"noteId" = ?`, noteID).Take(&q).Error; err != nil {
		return nil, err
	}
	return &q, nil
}

// FindByRequestURI returns the request with the given QuoteRequest id, or
// ErrNotFound.
func (r *NoteQuoteRequestRepository) FindByRequestURI(requestURI string) (*model.NoteQuoteRequest, error) {
	if !storable(requestURI) {
		return nil, ErrNotFound
	}
	var q model.NoteQuoteRequest
	if err := r.db.Where(`"requestUri" = ?`, requestURI).Take(&q).Error; err != nil {
		return nil, err
	}
	return &q, nil
}

// Ensure records a pending request for the note unless one already exists, and
// returns the stored row. 既にあれば状態を変えない (承認済みを pending に戻さない)。
func (r *NoteQuoteRequestRepository) Ensure(noteID, requestURI string) (*model.NoteQuoteRequest, error) {
	q := &model.NoteQuoteRequest{NoteID: noteID, RequestURI: requestURI, State: model.QuoteRequestPending}
	if err := r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(q).Error; err != nil {
		return nil, err
	}
	return r.FindByNoteID(noteID)
}

// MarkAccepted records that a pending request was accepted with approvalURI and
// reports whether the Update carrying it still has to be delivered.
//
// **承認を受けるのは保留中のときだけ** (Mastodon の Accept#accept_quote! と同じ)。
// 一度承認されたら承認 URI は変えない — 変えられると、引用される作者が URI を
// 変えた Accept を送るたびに、こちらの利用者のフォロワー全員へ Update を配り直す
// ことになる (レビュー 3 周目)。拒否されたものも承認に戻さない。戻り値が true に
// なるのは、記録された承認が approvalURI で、まだ配り終えていないとき (再試行や、
// 失敗と重なって届いた同じ Accept は送り直す)。
func (r *NoteQuoteRequestRepository) MarkAccepted(noteID, approvalURI string) (bool, error) {
	err := r.db.Model(&model.NoteQuoteRequest{}).
		Where(`"noteId" = ? AND "state" = ?`, noteID, model.QuoteRequestPending).
		Updates(map[string]any{"state": model.QuoteRequestAccepted, "approvalUri": approvalURI, "updateSent": false}).Error
	if err != nil {
		return false, err
	}
	q, err := r.FindByNoteID(noteID)
	if err != nil {
		return false, err
	}
	// 取り消された後の同じ Accept では送らない (取り消しの配り直しは MarkRevoked 側)。
	return q.State == model.QuoteRequestAccepted && q.ApprovalURI != nil && *q.ApprovalURI == approvalURI && !q.UpdateSent, nil
}

// MarkUpdateSent records that the Update for the given state and approval URI
// was delivered. 状態か承認がその間に変わっていたら何もしない (新しい状態の
// Update はまだ配っていない)。
func (r *NoteQuoteRequestRepository) MarkUpdateSent(noteID, state, approvalURI string) error {
	return r.db.Model(&model.NoteQuoteRequest{}).
		Where(`"noteId" = ? AND "state" = ? AND "approvalUri" = ?`, noteID, state, approvalURI).
		Update("updateSent", true).Error
}

// ListByApprovalURI returns the requests whose approval is approvalURI (at
// most limit). 取り消しの Delete を照合するのに使う。承認の URI は相手のホストの
// 任意の値でありうるので一意とは限らない — 1 件だけ拾うと、同じ URI を持つ別の
// 記録に当たって本物の取り消しを取りこぼす。
func (r *NoteQuoteRequestRepository) ListByApprovalURI(approvalURI string, limit int) ([]model.NoteQuoteRequest, error) {
	if !storable(approvalURI) {
		return nil, nil
	}
	var rows []model.NoteQuoteRequest
	if err := r.db.Where(`"approvalUri" = ?`, approvalURI).Order(`"noteId"`).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// MarkRevoked records that an accepted request was revoked by the quoted
// author, and reports whether the Update withdrawing the approval still has to
// be delivered. 承認 URI は残す (取り消しの Update を配り終えたかをそれで照合する)。
// 描画は状態が accepted のときしか承認を付けない。
func (r *NoteQuoteRequestRepository) MarkRevoked(noteID string) (bool, error) {
	err := r.db.Model(&model.NoteQuoteRequest{}).
		Where(`"noteId" = ? AND "state" = ?`, noteID, model.QuoteRequestAccepted).
		Updates(map[string]any{"state": model.QuoteRequestRevoked, "updateSent": false}).Error
	if err != nil {
		return false, err
	}
	q, err := r.FindByNoteID(noteID)
	if err != nil {
		return false, err
	}
	return q.State == model.QuoteRequestRevoked && !q.UpdateSent, nil
}

// MarkRejected records that a pending request was rejected. 承認済みのものは
// 変えない (承認の後の Reject は取り消しなので、呼び出し側が MarkRevoked を使う)。
func (r *NoteQuoteRequestRepository) MarkRejected(noteID string) error {
	return r.db.Model(&model.NoteQuoteRequest{}).
		Where(`"noteId" = ? AND "state" = ?`, noteID, model.QuoteRequestPending).
		Update("state", model.QuoteRequestRejected).Error
}
