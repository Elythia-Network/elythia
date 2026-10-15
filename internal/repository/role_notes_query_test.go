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

// roles/notes は、投稿者・返信先・リノート先のいずれかの作者が凍結されていれば
// 除く (upstream roles/notes.ts の generateBaseNoteFilteringQuery)。
func TestRoleNotesQuery_ListByRole_ExcludesSuspendedAuthors(t *testing.T) {
	q := NewRoleNotesQuery(testDB)
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)
	noteRepo := NewNoteRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID: "role_rnq_sus", UpdatedAt: now, LastUsedAt: now, Name: "RNQSus",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)

	createTestUser(t, "rnq_sus_member")
	createTestUser(t, "rnq_sus_member2")
	createTestUser(t, "rnq_sus_other")
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "rnq_sus_a1", UserID: "rnq_sus_member", RoleID: role.ID}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "rnq_sus_a2", UserID: "rnq_sus_member2", RoleID: role.ID}))

	mk := func(n *model.Note) *model.Note {
		n.Visibility = model.NoteVisibilityPublic
		n.Reactions = datatypes.JSON([]byte("{}"))
		require.NoError(t, noteRepo.Create(n))
		t.Cleanup(func() { cleanupNote(t, n.ID) })
		return n
	}
	otherID := "rnq_sus_other"
	base := mk(&model.Note{ID: "rnq_sus_n_other", UserID: otherID})
	plain := mk(&model.Note{ID: "rnq_sus_n_plain", UserID: "rnq_sus_member"})
	renote := mk(&model.Note{ID: "rnq_sus_n_rn", UserID: "rnq_sus_member", RenoteID: &base.ID, RenoteUserID: &otherID})
	reply := mk(&model.Note{ID: "rnq_sus_n_rep", UserID: "rnq_sus_member", ReplyID: &base.ID, ReplyUserID: &otherID})
	byMember2 := mk(&model.Note{ID: "rnq_sus_n_m2", UserID: "rnq_sus_member2"})

	list := func() map[string]bool {
		notes, err := q.ListByRole(role.ID, 50, "", "")
		require.NoError(t, err)
		ids := map[string]bool{}
		for _, n := range notes {
			ids[n.ID] = true
		}
		return ids
	}
	ids := list()
	for _, n := range []*model.Note{plain, renote, reply, byMember2} {
		require.True(t, ids[n.ID], "before suspension %s must be listed", n.ID)
	}

	require.NoError(t, testDB.Exec(`UPDATE "user" SET "isSuspended" = true WHERE id IN ?`, []string{otherID, "rnq_sus_member2"}).Error)
	ids = list()
	assert.True(t, ids[plain.ID], "plain note of an active member is kept")
	assert.False(t, ids[renote.ID], "renote of a suspended user is excluded")
	assert.False(t, ids[reply.ID], "reply to a suspended user is excluded")
	assert.False(t, ids[byMember2.ID], "note of a suspended member is excluded")
}
