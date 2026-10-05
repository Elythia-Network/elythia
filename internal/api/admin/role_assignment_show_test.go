package admin_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const assignmentAdminInternalErrorJSON = `{"error":{"message":"Internal error.","code":"INTERNAL_ERROR","id":"5d37dbcb-891e-41ca-a3d6-e690c97775ac","kind":"server"}}`

type adminExactAssignmentRepo struct {
	*testutil.MockRoleAssignmentRepository
	err       error
	findCalls int
	listCalls int
	userID    string
	roleID    string
}

func (r *adminExactAssignmentRepo) FindActive(userID, roleID string, at time.Time) (*model.RoleAssignment, error) {
	r.findCalls++
	r.userID, r.roleID = userID, roleID
	if r.err != nil {
		return nil, r.err
	}
	return r.MockRoleAssignmentRepository.FindActive(userID, roleID, at)
}

func (r *adminExactAssignmentRepo) ListByUser(userID string) ([]*model.RoleAssignment, error) {
	r.listCalls++
	return r.MockRoleAssignmentRepository.ListByUser(userID)
}

type adminCheckedRoleRepo struct {
	repository.RoleRepository
	role  *model.Role
	err   error
	calls int
}

type adminFailingUserRepo struct {
	repository.UserRepository
	err error
}

func (r *adminFailingUserRepo) FindByID(string) (*model.User, error) {
	return nil, r.err
}

func (r *adminCheckedRoleRepo) FindByID(string) (*model.Role, error) {
	r.calls++
	if r.calls == 1 && r.role != nil {
		return r.role, nil
	}
	return nil, r.err
}

func assignmentAdminHandler(t *testing.T, roleRepo repository.RoleRepository, userRepo repository.UserRepository, assignRepo repository.RoleAssignmentRepository) *admin.Handler {
	t.Helper()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	roleService := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	return admin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleService, metaRepo, userRepo, idGen)
}

func TestRolesAssignmentShow_ExactLookup(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["role"] = &model.Role{ID: "role", Target: model.RoleTargetManual, IsPublic: true, CanEditMembersByModerator: true}
	baseAssignments := testutil.NewMockRoleAssignmentRepository(roleRepo)
	baseAssignments.Assignments["user:other"] = &model.RoleAssignment{UserID: "user", RoleID: "other"}
	assignments := &adminExactAssignmentRepo{MockRoleAssignmentRepository: baseAssignments}
	users := testutil.NewMockUserRepository()
	users.Users["user"] = &model.User{ID: "user"}
	h := assignmentAdminHandler(t, roleRepo, users, assignments)

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"role","userId":"user"}`, &model.User{ID: "moderator"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"assigned":false,"expiresAt":null,"role":{"id":"role","target":"manual","isPublic":true,"canEditMembersByModerator":true}}`, rec.Body.String())
	assert.Equal(t, 1, assignments.findCalls)
	assert.Equal(t, "user", assignments.userID)
	assert.Equal(t, "role", assignments.roleID)
	assert.Zero(t, assignments.listCalls)
}

func TestRolesAssignmentShow_LockedRoleReadableByModerator(t *testing.T) {
	h, users, _, rolesRepo, assignments := newTestHandlerWithAssign(t)
	rolesRepo.Roles["role"] = &model.Role{ID: "role", Target: model.RoleTargetManual, CanEditMembersByModerator: false}
	rolesRepo.Roles["modrole"] = &model.Role{ID: "modrole", IsModerator: true}
	users.Users["mod"] = &model.User{ID: "mod"}
	users.Users["user"] = &model.User{ID: "user"}
	assignments.Assignments["mod:modrole"] = &model.RoleAssignment{UserID: "mod", RoleID: "modrole"}

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"role","userId":"user"}`, &model.User{ID: "mod"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"assigned":false,"expiresAt":null,"role":{"id":"role","target":"manual","isPublic":false,"canEditMembersByModerator":false}}`, rec.Body.String())
}

func TestRolesAssignmentShow_RoleNotFound(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignments := &adminExactAssignmentRepo{MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo)}
	users := testutil.NewMockUserRepository()
	users.Users["user"] = &model.User{ID: "user"}
	h := assignmentAdminHandler(t, roleRepo, users, assignments)

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"missing","userId":"user"}`, &model.User{ID: "moderator"})

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"NO_SUCH_ROLE"`)
	assert.Zero(t, assignments.findCalls)
}

func TestRolesAssignmentShow_RolePersistenceFailure(t *testing.T) {
	wantErr := errors.New("role SELECT failed")
	baseRoles := testutil.NewMockRoleRepository()
	roleRepo := &adminCheckedRoleRepo{RoleRepository: baseRoles, err: wantErr}
	assignments := &adminExactAssignmentRepo{MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(baseRoles)}
	users := testutil.NewMockUserRepository()
	users.Users["user"] = &model.User{ID: "user"}
	h := assignmentAdminHandler(t, roleRepo, users, assignments)

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"role","userId":"user"}`, &model.User{ID: "moderator"})

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, assignmentAdminInternalErrorJSON, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), wantErr.Error())
	assert.Zero(t, assignments.findCalls)
}

func TestRolesAssignmentShow_UserNotFound(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["role"] = &model.Role{ID: "role"}
	assignments := &adminExactAssignmentRepo{MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo)}
	users := testutil.NewMockUserRepository()
	h := assignmentAdminHandler(t, roleRepo, users, assignments)

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"role","userId":"missing"}`, &model.User{ID: "moderator"})

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"NO_SUCH_USER"`)
	assert.Zero(t, assignments.findCalls)
}

func TestRolesAssignmentShow_UserPersistenceFailure(t *testing.T) {
	wantErr := errors.New("user SELECT failed")
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["role"] = &model.Role{ID: "role"}
	assignments := &adminExactAssignmentRepo{MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo)}
	users := &adminFailingUserRepo{UserRepository: testutil.NewMockUserRepository(), err: wantErr}
	h := assignmentAdminHandler(t, roleRepo, users, assignments)

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"role","userId":"user"}`, &model.User{ID: "moderator"})

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, assignmentAdminInternalErrorJSON, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), wantErr.Error())
	assert.Zero(t, assignments.findCalls)
}

func TestRolesAssignmentShow_AssignmentPersistenceFailure(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["role"] = &model.Role{ID: "role", CanEditMembersByModerator: true}
	baseAssignments := testutil.NewMockRoleAssignmentRepository(roleRepo)
	assignments := &adminExactAssignmentRepo{MockRoleAssignmentRepository: baseAssignments, err: errors.New("SELECT assignment failed")}
	users := testutil.NewMockUserRepository()
	users.Users["user"] = &model.User{ID: "user"}
	h := assignmentAdminHandler(t, roleRepo, users, assignments)

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"role","userId":"user"}`, &model.User{ID: "moderator"})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, assignmentAdminInternalErrorJSON, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "SELECT")
}

func TestRolesAssignmentShow_UsesSingleRoleSnapshot(t *testing.T) {
	roleRepo := &adminCheckedRoleRepo{
		RoleRepository: testutil.NewMockRoleRepository(),
		role:           &model.Role{ID: "role", IsPublic: true, CanEditMembersByModerator: true},
		err:            errors.New("role changed after authorization"),
	}
	assignments := testutil.NewMockRoleAssignmentRepository(testutil.NewMockRoleRepository())
	users := testutil.NewMockUserRepository()
	users.Users["user"] = &model.User{ID: "user"}
	h := assignmentAdminHandler(t, roleRepo, users, assignments)

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"role","userId":"user"}`, &model.User{ID: "moderator"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, roleRepo.calls)
}

// conditional role は role_assignment 行を持たないので assigned は常に false になる。
// 既存の admin/roles/users も ListByRole で同じテーブルを引くため挙動は揃っており、
// 判別の手掛かりとして role.target を返している (#2633)。
func TestRolesAssignmentShow_ConditionalRoleIsNeverAssigned(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["cond"] = &model.Role{ID: "cond", Target: model.RoleTargetConditional, IsPublic: false}
	assignments := &adminExactAssignmentRepo{MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo)}
	users := testutil.NewMockUserRepository()
	users.Users["user"] = &model.User{ID: "user"}
	h := assignmentAdminHandler(t, roleRepo, users, assignments)

	rec := doPost(h.RolesAssignmentShow, `{"roleId":"cond","userId":"user"}`, &model.User{ID: "moderator"})
	require.Equal(t, http.StatusOK, rec.Code)
	// self 側と違い private でも moderator には body を返すので、conditional の
	// 制約だけが効く形になる。
	assert.JSONEq(t, `{"assigned":false,"expiresAt":null,"role":{"id":"cond","target":"conditional","isPublic":false,"canEditMembersByModerator":false}}`, rec.Body.String())
	// condFormula の評価には全 role の走査が要る。評価しないので参照は FindActive 1 回。
	assert.Equal(t, 1, assignments.findCalls)
	assert.Zero(t, assignments.listCalls)
}
