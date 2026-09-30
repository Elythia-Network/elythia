package model

// Quote request states of NoteQuoteRequest.
const (
	QuoteRequestPending  = "pending"
	QuoteRequestAccepted = "accepted"
	QuoteRequestRejected = "rejected"
	// QuoteRequestRevoked は、承認された後に引用される作者が取り消したもの。
	QuoteRequestRevoked = "revoked"
)

// NoteQuoteRequest represents the mk-go-only `note_quote_request` table
// (#3234): the FEP-044f QuoteRequest a local note sent to the author of the
// remote note it quotes, and the answer. An accepted request's approval URI is
// published as the quoting note's `quoteAuthorization`.
type NoteQuoteRequest struct {
	NoteID      string  `gorm:"column:noteId;type:varchar(32);primaryKey" json:"noteId"`
	RequestURI  string  `gorm:"column:requestUri;type:varchar(512);not null" json:"requestUri"`
	State       string  `gorm:"column:state;type:varchar(16);not null" json:"state"`
	ApprovalURI *string `gorm:"column:approvalUri;type:varchar(512)" json:"approvalUri"`
	UpdateSent  bool    `gorm:"column:updateSent;not null" json:"updateSent"`
}

// TableName returns the table name.
func (NoteQuoteRequest) TableName() string { return "note_quote_request" }
