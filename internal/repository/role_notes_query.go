package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// RoleNotesQuery fetches notes by role via GORM.
type RoleNotesQuery struct {
	db *gorm.DB
}

// NewRoleNotesQuery creates a new RoleNotesQuery.
func NewRoleNotesQuery(db *gorm.DB) *RoleNotesQuery {
	return &RoleNotesQuery{db: db}
}

// ListByRole returns public notes from users assigned to the given role.
func (q *RoleNotesQuery) ListByRole(roleID string, limit int, sinceID, untilID string) ([]*model.Note, error) {
	// preloadNoteRelations で User + Renote/Reply (+User) を preload し、
	// note.go 側の他の list 系クエリと同一の embed 方針に揃える (#416)。
	// upstream notes.ts は fanout timeline 経由で「現に role を持つ user」だけを
	// 対象にする。mk-go は role_assignment を JOIN するため、失効済み割当
	// (expiresAt <= now) の user の note が漏れないよう expiresAt を除外する (#1544)。
	query := preloadNoteRelations(q.db).Model(&model.Note{}).
		Joins(`JOIN "role_assignment" ON "role_assignment"."userId" = "note"."userId"`).
		Where(`"role_assignment"."roleId" = ?`, roleID).
		Where(`("role_assignment"."expiresAt" IS NULL OR "role_assignment"."expiresAt" > ?)`, time.Now()).
		Where(`"note"."visibility" = 'public'`).
		Order(paginationOrder(sinceID, untilID, `"note"."id"`)).
		Limit(limit)

	if sinceID != "" {
		query = query.Where(`"note"."id" > ?`, sinceID)
	}
	if untilID != "" {
		query = query.Where(`"note"."id" < ?`, untilID)
	}

	var notes []*model.Note
	if err := query.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}
