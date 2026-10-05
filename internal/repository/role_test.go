package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func cancelledDB(t *testing.T) *gorm.DB {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return testDB.WithContext(ctx)
}

func cleanupRole(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "role_assignment" WHERE "roleId" = ?`, id)
	testDB.Exec(`DELETE FROM "role" WHERE id = ?`, id)
}

func createTestUser(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`INSERT INTO "user" (id, username, "usernameLower", "avatarDecorations") VALUES (?, ?, ?, '[]') ON CONFLICT DO NOTHING`, id, id, id)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
}

// --- RoleRepository ---

func TestRoleRepository_CRUD(t *testing.T) {
	repo := NewRoleRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID:              "role_test_1",
		UpdatedAt:       now,
		LastUsedAt:      now,
		Name:            "Test Admin",
		Description:     "Test admin role",
		IsAdministrator: true,
		IsPublic:        true,
		Target:          model.RoleTargetManual,
		Policies:        datatypes.JSON([]byte("{}")),
		CondFormula:     datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(role))
	defer cleanupRole(t, role.ID)

	// FindByID
	found, err := repo.FindByID(role.ID)
	require.NoError(t, err)
	assert.Equal(t, "Test Admin", found.Name)
	assert.True(t, found.IsAdministrator)

	// List
	roles, err := repo.List()
	require.NoError(t, err)
	assert.NotEmpty(t, roles)

	// UpdateFields
	require.NoError(t, repo.UpdateFields(role.ID, map[string]any{"name": "Updated"}))
	updated, err := repo.FindByID(role.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated", updated.Name)

	// Delete
	require.NoError(t, repo.Delete(role.ID))
	_, err = repo.FindByID(role.ID)
	assert.Error(t, err)
}

func TestRoleRepository_FindByID_NotFound(t *testing.T) {
	repo := NewRoleRepository(testDB)
	_, err := repo.FindByID("ghost")
	assert.Error(t, err)
}

// --- RoleAssignmentRepository ---

func TestRoleAssignmentRepository_DeleteExpired(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID: "role_de_test", UpdatedAt: now, LastUsedAt: now, Name: "Mod",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")), IsModerator: true,
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)
	createTestUser(t, "de_u1")
	createTestUser(t, "de_u2")
	createTestUser(t, "de_u3")

	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "de_a1", UserID: "de_u1", RoleID: role.ID}))                     // perpetual
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "de_a2", UserID: "de_u2", RoleID: role.ID, ExpiresAt: &future})) // not expired
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "de_a3", UserID: "de_u3", RoleID: role.ID, ExpiresAt: &past}))   // expired

	n, err := assignRepo.DeleteExpired(now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "期限切れ 1 件だけ削除")
	ok, _ := assignRepo.Exists("de_u1", role.ID)
	assert.True(t, ok, "perpetual は残る")
	ok, _ = assignRepo.Exists("de_u2", role.ID)
	assert.True(t, ok, "未期限切れは残る")
	ok, _ = assignRepo.Exists("de_u3", role.ID)
	assert.False(t, ok, "期限切れは削除済み")
}

func TestRoleAssignmentRepository_CRUD(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID: "role_a_test", UpdatedAt: now, LastUsedAt: now, Name: "Mod",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")), IsModerator: true,
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)

	createTestUser(t, "ra_user1")

	// Create
	a := &model.RoleAssignment{ID: "ra_1", UserID: "ra_user1", RoleID: role.ID}
	require.NoError(t, assignRepo.Create(a))

	// Exists
	exists, err := assignRepo.Exists("ra_user1", role.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	// ListByUser (with role preload)
	assignments, err := assignRepo.ListByUser("ra_user1")
	require.NoError(t, err)
	assert.Len(t, assignments, 1)
	assert.NotNil(t, assignments[0].Role)
	assert.Equal(t, "Mod", assignments[0].Role.Name)

	// ListByRole (with user preload)
	byRole, err := assignRepo.ListByRole(role.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, byRole, 1)

	// Delete
	require.NoError(t, assignRepo.Delete("ra_user1", role.ID))
	exists, err = assignRepo.Exists("ra_user1", role.ID)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestRoleAssignmentRepository_FindActive(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)
	now := time.Now()
	role := &model.Role{
		ID: "role_find_active", UpdatedAt: now, LastUsedAt: now, Name: "Exact",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)
	otherRole := &model.Role{
		ID: "role_find_active_other", UpdatedAt: now, LastUsedAt: now, Name: "Other",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(otherRole))
	defer cleanupRole(t, otherRole.ID)
	createTestUser(t, "find_active_u1")
	createTestUser(t, "find_active_u2")
	createTestUser(t, "find_active_u3")

	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	future := at.Add(time.Second)
	boundary := at
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "find_active_a1", UserID: "find_active_u1", RoleID: role.ID, ExpiresAt: &future}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "find_active_a2", UserID: "find_active_u2", RoleID: otherRole.ID}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "find_active_a3", UserID: "find_active_u3", RoleID: role.ID}))

	active, err := assignRepo.FindActive("find_active_u1", role.ID, at)
	require.NoError(t, err)
	require.NotNil(t, active)
	require.NotNil(t, active.ExpiresAt)
	assert.WithinDuration(t, future, *active.ExpiresAt, time.Millisecond)

	active, err = assignRepo.FindActive("find_active_u2", role.ID, at)
	require.NoError(t, err)
	assert.Nil(t, active, "an active assignment to another role is confirmed absence for the target role")

	require.NoError(t, testDB.Model(&model.RoleAssignment{}).Where("id = ?", "find_active_a1").Update("expiresAt", boundary).Error)
	active, err = assignRepo.FindActive("find_active_u1", role.ID, at)
	require.NoError(t, err)
	assert.Nil(t, active, "expiresAt equal to evaluation time is inactive")

	active, err = assignRepo.FindActive("find_active_u3", role.ID, at)
	require.NoError(t, err)
	require.NotNil(t, active, "nil expiresAt is active")
}

func TestRoleAssignmentRepository_FindActive_Error(t *testing.T) {
	repo := NewRoleAssignmentRepository(cancelledDB(t))
	_, err := repo.FindActive("user", "role", time.Now())
	assert.Error(t, err)
}

func TestRoleRepository_List_Error(t *testing.T) {
	db := cancelledDB(t)
	repo := NewRoleRepository(db)
	_, err := repo.List()
	assert.Error(t, err)
}

func TestRoleAssignmentRepository_ListByUser_Error(t *testing.T) {
	db := cancelledDB(t)
	repo := NewRoleAssignmentRepository(db)
	_, err := repo.ListByUser("x")
	assert.Error(t, err)
}

func TestRoleAssignmentRepository_ListByRole_WithPagination(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID: "role_pag_test", UpdatedAt: now, LastUsedAt: now, Name: "Pag",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)

	createTestUser(t, "pag_u1")
	createTestUser(t, "pag_u2")
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "pag_a1", UserID: "pag_u1", RoleID: role.ID}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "pag_a2", UserID: "pag_u2", RoleID: role.ID}))

	// limit=1
	result, err := assignRepo.ListByRole(role.ID, "", "", 1)
	require.NoError(t, err)
	assert.Len(t, result, 1)

	// untilID で keyset cursor (id DESC で pag_a2 → pag_a1 と進む途中)
	result, err = assignRepo.ListByRole(role.ID, "pag_a2", "", 10)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "pag_a1", result[0].ID)

	// sinceID で ASC cursor (pag_a1 の次は pag_a2)
	result, err = assignRepo.ListByRole(role.ID, "", "pag_a1", 10)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "pag_a2", result[0].ID)
}

func TestRoleAssignmentRepository_CountActiveByRole(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID: "role_cnt_test", UpdatedAt: now, LastUsedAt: now, Name: "Cnt",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)

	createTestUser(t, "cnt_u1")
	createTestUser(t, "cnt_u2")
	createTestUser(t, "cnt_u3")
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "cnt_a1", UserID: "cnt_u1", RoleID: role.ID}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "cnt_a2", UserID: "cnt_u2", RoleID: role.ID, ExpiresAt: &future}))
	// 期限切れは active count に含めない。
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "cnt_a3", UserID: "cnt_u3", RoleID: role.ID, ExpiresAt: &past}))

	// 別 role の assignment は count に混ざらない (cross-role isolation)。
	otherRole := &model.Role{
		ID: "role_cnt_other", UpdatedAt: now, LastUsedAt: now, Name: "Other",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(otherRole))
	defer cleanupRole(t, otherRole.ID)
	createTestUser(t, "cnt_u4")
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "cnt_a4", UserID: "cnt_u4", RoleID: otherRole.ID}))

	n, err := assignRepo.CountActiveByRole(role.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "active (nil + future) のみ数え、expired と別 role は除外")

	n, err = assignRepo.CountActiveByRole(otherRole.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestRoleAssignmentRepository_ListByRole_Error(t *testing.T) {
	db := cancelledDB(t)
	repo := NewRoleAssignmentRepository(db)
	_, err := repo.ListByRole("x", "", "", 10)
	assert.Error(t, err)
}

func TestRoleAssignmentRepository_Exists_Error(t *testing.T) {
	db := cancelledDB(t)
	repo := NewRoleAssignmentRepository(db)
	_, err := repo.Exists("x", "y")
	assert.Error(t, err)
}

func TestRoleAssignmentRepository_ExpiredExcluded(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID: "role_exp_test", UpdatedAt: now, LastUsedAt: now, Name: "Temp",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)

	createTestUser(t, "ra_user2")

	expired := now.Add(-1 * time.Hour)
	a := &model.RoleAssignment{ID: "ra_exp", UserID: "ra_user2", RoleID: role.ID, ExpiresAt: &expired}
	require.NoError(t, assignRepo.Create(a))

	// ListByUser — expired assignment excluded
	assignments, err := assignRepo.ListByUser("ra_user2")
	require.NoError(t, err)
	assert.Empty(t, assignments)

	// Exists — expired → false
	exists, err := assignRepo.Exists("ra_user2", role.ID)
	require.NoError(t, err)
	assert.False(t, exists)

	// ListByRole — expired excluded
	byRole, err := assignRepo.ListByRole(role.ID, "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, byRole)

	// cleanup
	testDB.Exec(`DELETE FROM "role_assignment" WHERE id = ?`, a.ID)
}

// upstream Misskey #17334 (= 2026.5.0 fix / triage #1007): admin role を複数
// 持つ user の ID が重複して返る bug の regression guard。mk-go は `id IN
// (SELECT ra."userId" ...)` の SQL semantics で自動 dedup する設計のため
// upstream の Set 経由 dedup と等価。本 test は user に 2 つの admin role を
// assign し、ListUsers(State="admin") が当該 user を 1 行だけ返すことを確認する。
func TestUserRepository_ListUsers_AdminFilterDedupsMultipleRoles(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)
	userRepo := NewUserRepository(testDB)

	now := time.Now()
	role1 := &model.Role{
		ID: "role_admin_dedup_1", UpdatedAt: now, LastUsedAt: now, Name: "AdminA",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")), IsAdministrator: true,
	}
	role2 := &model.Role{
		ID: "role_admin_dedup_2", UpdatedAt: now, LastUsedAt: now, Name: "AdminB",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")), IsAdministrator: true,
	}
	require.NoError(t, roleRepo.Create(role1))
	require.NoError(t, roleRepo.Create(role2))
	defer cleanupRole(t, role1.ID)
	defer cleanupRole(t, role2.ID)

	createTestUser(t, "admin_dedup_u1")
	// 同一 user に 2 つの admin role を assign (upstream bug の再現条件)
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{
		ID: "ra_dedup_1", UserID: "admin_dedup_u1", RoleID: role1.ID,
	}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{
		ID: "ra_dedup_2", UserID: "admin_dedup_u1", RoleID: role2.ID,
	}))

	// State="admin" の filter で当該 user が 1 度だけ返ることを確認
	users, err := userRepo.ListUsers(model.UserListFilter{Limit: 100, State: "admin", Sort: "+createdAt"})
	require.NoError(t, err)
	count := 0
	for _, u := range users {
		if u.ID == "admin_dedup_u1" {
			count++
		}
	}
	assert.Equal(t, 1, count, "user with multiple admin roles should appear exactly once (= SQL IN subquery dedup)")
}

// #2061: ListByLastUsed は lastUsedAt DESC で返す。
func TestRoleRepository_ListByLastUsed(t *testing.T) {
	repo := NewRoleRepository(testDB)
	mk := func(id string, year int) {
		require.NoError(t, repo.Create(&model.Role{
			ID: id, Name: id, UpdatedAt: time.Now(),
			LastUsedAt:  time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC),
			Target:      model.RoleTargetManual,
			Policies:    datatypes.JSON([]byte("{}")),
			CondFormula: datatypes.JSON([]byte("{}")),
		}))
		t.Cleanup(func() { cleanupRole(t, id) })
	}
	mk("rlu_old", 2020)
	mk("rlu_new", 2024)
	mk("rlu_mid", 2022)

	roles, err := repo.ListByLastUsed()
	require.NoError(t, err)
	// seed した 3 件が lastUsedAt DESC 順 (new→mid→old) で並ぶこと (他の既存 role が
	// 混ざりうるので相対順序で検証)。
	pos := map[string]int{}
	for i, r := range roles {
		pos[r.ID] = i
	}
	assert.Less(t, pos["rlu_new"], pos["rlu_mid"], "new は mid より前 (#2061)")
	assert.Less(t, pos["rlu_mid"], pos["rlu_old"], "mid は old より前 (#2061)")
}
