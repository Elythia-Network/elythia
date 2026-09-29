package model

// NoteQuoteAuthorization represents the mk-go-only `note_quote_authorization`
// table (#3234): a record that a local note may be quoted by a remote note
// (FEP-044f). The row is published as a QuoteAuthorization object; deleting it
// makes third parties treat the quote as unapproved.
type NoteQuoteAuthorization struct {
	ID         string  `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	NoteID     string  `gorm:"column:noteId;type:varchar(32);not null" json:"noteId"`
	QuoterID   string  `gorm:"column:quoterId;type:varchar(32);not null" json:"quoterId"`
	QuotingURI string  `gorm:"column:quotingUri;type:varchar(512);not null" json:"quotingUri"`
	RequestID  *string `gorm:"column:requestId;type:varchar(512)" json:"requestId"`
}

// TableName returns the table name.
func (NoteQuoteAuthorization) TableName() string { return "note_quote_authorization" }
