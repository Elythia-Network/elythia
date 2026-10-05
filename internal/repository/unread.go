package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AntennaNoteUnreadRepository tracks per-user-per-note antenna unreads.
type AntennaNoteUnreadRepository interface {
	Create(m *model.AntennaNoteUnread) error
	// HasAnyByUser reports whether the user has any unread antenna notes.
	// Used by /api/i to populate hasUnreadAntenna.
	HasAnyByUser(userID string) (bool, error)
	// DeleteByAntennaUser removes all unread rows for a given (user, antenna)
	// pair. Used when a user reads an antenna timeline.
	DeleteByAntennaUser(userID, antennaID string) error
}

type antennaNoteUnreadRepository struct{ db *gorm.DB }

// NewAntennaNoteUnreadRepository constructs a repository backed by db.
func NewAntennaNoteUnreadRepository(db *gorm.DB) AntennaNoteUnreadRepository {
	return &antennaNoteUnreadRepository{db: db}
}

func (r *antennaNoteUnreadRepository) Create(m *model.AntennaNoteUnread) error {
	// (userId, antennaId, noteId) の unique 制約を持つので重複はskipして
	// ログにも残さない (best-effort insert)。
	return r.db.Where(`"userId" = ? AND "antennaId" = ? AND "noteId" = ?`, m.UserID, m.AntennaID, m.NoteID).
		FirstOrCreate(m).Error
}

func (r *antennaNoteUnreadRepository) HasAnyByUser(userID string) (bool, error) {
	// SELECT EXISTS(...) は 1 行見つかった時点で short-circuit するため
	// COUNT(*) より効率的 (未読が多いユーザーで効く)。
	var exists bool
	err := r.db.Raw(
		`SELECT EXISTS(SELECT 1 FROM "antenna_note_unread" WHERE "userId" = ?)`,
		userID,
	).Scan(&exists).Error
	return exists, err
}

func (r *antennaNoteUnreadRepository) DeleteByAntennaUser(userID, antennaID string) error {
	return r.db.Where(`"userId" = ? AND "antennaId" = ?`, userID, antennaID).
		Delete(&model.AntennaNoteUnread{}).Error
}

// ChannelNoteUnreadRepository tracks per-follower-per-note channel unreads.
type ChannelNoteUnreadRepository interface {
	Create(m *model.ChannelNoteUnread) error
	// CreateMany inserts the given rows in bulk, silently skipping ones that
	// violate the (userId, channelId, noteId) unique constraint. Used by
	// channel-note fanout (#320) to avoid per-follower round trips on
	// popular channels. 空 slice は no-op.
	CreateMany(rows []*model.ChannelNoteUnread) error
	// HasAnyByUser reports whether the user has any unread channel notes.
	HasAnyByUser(userID string) (bool, error)
	// DeleteByChannelUser removes all unread rows for a given (user, channel)
	// pair. Used when a user reads a channel timeline.
	DeleteByChannelUser(userID, channelID string) error
}

type channelNoteUnreadRepository struct{ db *gorm.DB }

// NewChannelNoteUnreadRepository constructs a repository backed by db.
func NewChannelNoteUnreadRepository(db *gorm.DB) ChannelNoteUnreadRepository {
	return &channelNoteUnreadRepository{db: db}
}

func (r *channelNoteUnreadRepository) Create(m *model.ChannelNoteUnread) error {
	return r.db.Where(`"userId" = ? AND "channelId" = ? AND "noteId" = ?`, m.UserID, m.ChannelID, m.NoteID).
		FirstOrCreate(m).Error
}

func (r *channelNoteUnreadRepository) CreateMany(rows []*model.ChannelNoteUnread) error {
	if len(rows) == 0 {
		return nil
	}
	// Bulk insert with ON CONFLICT DO NOTHING on the
	// (userId, channelId, noteId) unique index. GORM's clause.OnConflict
	// は composite unique index 全体を対象にする (DoNothing=true)。
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(rows).Error
}

func (r *channelNoteUnreadRepository) HasAnyByUser(userID string) (bool, error) {
	var exists bool
	err := r.db.Raw(
		`SELECT EXISTS(SELECT 1 FROM "channel_note_unread" WHERE "userId" = ?)`,
		userID,
	).Scan(&exists).Error
	return exists, err
}

func (r *channelNoteUnreadRepository) DeleteByChannelUser(userID, channelID string) error {
	return r.db.Where(`"userId" = ? AND "channelId" = ?`, userID, channelID).
		Delete(&model.ChannelNoteUnread{}).Error
}

// NoteUnreadRepository tracks per-user-per-note unread rows for specified /
// mentioned notes. 本家 Misskey の note_unread 相当。
type NoteUnreadRepository interface {
	// Upsert inserts a row, or updates isSpecified / isMentioned by OR-ing
	// with any existing flags (so a row can be a specified DM and a mention
	// at the same time). ID is only used on first insert.
	Upsert(m *model.NoteUnread) error
	// HasAnySpecified reports whether the user has any unread note_unread row
	// with isSpecified = true. Used by /api/i for hasUnreadSpecifiedNotes.
	HasAnySpecified(userID string) (bool, error)
	// HasAnyMentioned reports whether the user has any unread note_unread row
	// with isMentioned = true. Available for future hasUnreadMentions wiring.
	HasAnyMentioned(userID string) (bool, error)
	// DeleteByUserNote removes the row for (userID, noteID). Invoked when the
	// user explicitly reads the note.
	DeleteByUserNote(userID, noteID string) error
	// DeleteAllByUser removes every note_unread row owned by userID. Invoked
	// by notification MarkAllAsRead / Flush so the unread indicator resets
	// when the user reads everything.
	DeleteAllByUser(userID string) error
}

type noteUnreadRepository struct{ db *gorm.DB }

// NewNoteUnreadRepository constructs a NoteUnreadRepository backed by db.
func NewNoteUnreadRepository(db *gorm.DB) NoteUnreadRepository {
	return &noteUnreadRepository{db: db}
}

func (r *noteUnreadRepository) Upsert(m *model.NoteUnread) error {
	// (userId, noteId) unique 制約に対して ON CONFLICT DO UPDATE で flag を
	// OR 集約する。isSpecified / isMentioned はそれぞれ一度 true になった
	// ら永続的に true のまま。
	return r.db.Exec(
		`INSERT INTO "note_unread" ("id", "userId", "noteId", "noteUserId", "isSpecified", "isMentioned")
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT ("userId", "noteId") DO UPDATE SET
		     "isSpecified" = "note_unread"."isSpecified" OR EXCLUDED."isSpecified",
		     "isMentioned" = "note_unread"."isMentioned" OR EXCLUDED."isMentioned"`,
		m.ID, m.UserID, m.NoteID, m.NoteUserID, m.IsSpecified, m.IsMentioned,
	).Error
}

func (r *noteUnreadRepository) HasAnySpecified(userID string) (bool, error) {
	var exists bool
	err := r.db.Raw(
		`SELECT EXISTS(SELECT 1 FROM "note_unread" WHERE "userId" = ? AND "isSpecified" = true)`,
		userID,
	).Scan(&exists).Error
	return exists, err
}

func (r *noteUnreadRepository) HasAnyMentioned(userID string) (bool, error) {
	var exists bool
	err := r.db.Raw(
		`SELECT EXISTS(SELECT 1 FROM "note_unread" WHERE "userId" = ? AND "isMentioned" = true)`,
		userID,
	).Scan(&exists).Error
	return exists, err
}

func (r *noteUnreadRepository) DeleteByUserNote(userID, noteID string) error {
	return r.db.Where(`"userId" = ? AND "noteId" = ?`, userID, noteID).
		Delete(&model.NoteUnread{}).Error
}

func (r *noteUnreadRepository) DeleteAllByUser(userID string) error {
	return r.db.Where(`"userId" = ?`, userID).Delete(&model.NoteUnread{}).Error
}
