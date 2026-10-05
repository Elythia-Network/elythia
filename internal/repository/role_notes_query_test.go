package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// RoleNotesQueryはrole_assignment経由でnoteを拾うJOINクエリ。本テストでは
// ざっくりとJOINが成立し、sinceId/untilId分岐が実行されることだけ確かめる。
// データ整合性の厳密なテストは上位のcore/role側に任せる。
func TestRoleNotesQuery_ListByRole(t *testing.T) {
	q := NewRoleNotesQuery(testDB)

	// 空結果でもクエリがエラーにならないこと
	notes, err := q.ListByRole("nonexistent_role", 10, "", "")
	require.NoError(t, err)
	assert.Empty(t, notes)

	// sinceId / untilId 分岐を通す
	notes, err = q.ListByRole("nonexistent_role", 5, "since_dummy", "until_dummy")
	require.NoError(t, err)
	assert.Empty(t, notes)
}

// TestRoleNotesQuery_ListByRole_ExcludesExpiredAssignment は #1544 の
// regression guard: role_assignment.expiresAt が失効した user の note は
// roles/notes に漏れないこと。
func TestRoleNotesQuery_ListByRole_ExcludesExpiredAssignment(t *testing.T) {
	q := NewRoleNotesQuery(testDB)
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)
	noteRepo := NewNoteRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID: "role_rnq_exp", UpdatedAt: now, LastUsedAt: now, Name: "RNQExp",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)

	createTestUser(t, "rnq_active")
	createTestUser(t, "rnq_expired")
	defer cleanupUser(t, "rnq_active")
	defer cleanupUser(t, "rnq_expired")

	past := now.Add(-time.Hour)
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "rnq_a_active", UserID: "rnq_active", RoleID: role.ID}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "rnq_a_expired", UserID: "rnq_expired", RoleID: role.ID, ExpiresAt: &past}))

	activeNote := &model.Note{ID: "rnq_n_active", UserID: "rnq_active", Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	expiredNote := &model.Note{ID: "rnq_n_expired", UserID: "rnq_expired", Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	require.NoError(t, noteRepo.Create(activeNote))
	require.NoError(t, noteRepo.Create(expiredNote))
	defer cleanupNote(t, activeNote.ID)
	defer cleanupNote(t, expiredNote.ID)

	notes, err := q.ListByRole(role.ID, 50, "", "")
	require.NoError(t, err)
	ids := make(map[string]bool, len(notes))
	for _, n := range notes {
		ids[n.ID] = true
	}
	assert.True(t, ids["rnq_n_active"], "有効な割当の note は含まれる")
	assert.False(t, ids["rnq_n_expired"], "失効した割当の note は除外される")
}
