package roles_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/roles"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const assignmentInternalErrorJSON = `{"error":{"message":"Internal error.","code":"INTERNAL_ERROR","id":"5d37dbcb-891e-41ca-a3d6-e690c97775ac","kind":"server"}}`
const assignmentNoSuchRoleJSON = `{"error":{"message":"No such role.","code":"NO_SUCH_ROLE","id":"4a3b21e8-9c54-4d7a-b1e2-8f6c9d0e5a71","kind":"client"}}`

type assignmentFailingRoleRepo struct{ repository.RoleRepository }

func (r *assignmentFailingRoleRepo) FindByID(string) (*model.Role, error) {
	return nil, errors.New("SELECT role: connection failed")
}

type assignmentExactRepo struct {
	*testutil.MockRoleAssignmentRepository
	err       error
	findCalls int
	listCalls int
	userID    string
	roleID    string
}

func (r *assignmentExactRepo) FindActive(userID, roleID string, at time.Time) (*model.RoleAssignment, error) {
	r.findCalls++
	r.userID, r.roleID = userID, roleID
	if r.err != nil {
		return nil, r.err
	}
	return r.MockRoleAssignmentRepository.FindActive(userID, roleID, at)
}

func (r *assignmentExactRepo) ListByUser(userID string) ([]*model.RoleAssignment, error) {
	r.listCalls++
	return r.MockRoleAssignmentRepository.ListByUser(userID)
}

func assignmentHandler(t *testing.T, roleRepo repository.RoleRepository, assignRepo repository.RoleAssignmentRepository) *roles.Handler {
	t.Helper()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return roles.NewHandler(corerole.NewService(roleRepo, assignRepo, metaRepo, idGen), idGen)
}

func assignmentFixture(t *testing.T) (*roles.Handler, *assignmentExactRepo, *testutil.MockRoleRepository) {
	t.Helper()
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &assignmentExactRepo{MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo)}
	return assignmentHandler(t, roleRepo, assignRepo), assignRepo, roleRepo
}

func postAssignment(h *roles.Handler, body, viewerID string) *httptest.ResponseRecorder {
	vc := newCtxWithViewer(body, viewerID)
	_ = h.AssignmentShow(vc.ctx)
	return vc.rec
}

func TestAssignmentShow_PublicRole(t *testing.T) {
	h, assignments, rolesRepo := assignmentFixture(t)
	rolesRepo.Roles["role"] = &model.Role{ID: "role", Target: model.RoleTargetManual, IsPublic: true, CanEditMembersByModerator: true}

	rec := postAssignment(h, `{"roleId":"role"}`, "viewer")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"assigned":false,"expiresAt":null,"role":{"id":"role","target":"manual","isPublic":true,"canEditMembersByModerator":true}}`, rec.Body.String())
	assert.Equal(t, 1, assignments.findCalls)
	assert.Equal(t, "viewer", assignments.userID)
	assert.Equal(t, "role", assignments.roleID)
	assert.Zero(t, assignments.listCalls)
}

func TestAssignmentShow_PrivateActiveAssignment(t *testing.T) {
	h, assignments, rolesRepo := assignmentFixture(t)
	rolesRepo.Roles["private"] = &model.Role{ID: "private", Target: model.RoleTargetManual, IsPublic: false}
	// **有効期限は time.Now() からの相対値で置く。** ハンドラは
	// `GetUserAssign(..., time.Now())` で期限切れを弾くので、絶対時刻を書くと
	// **その時刻を過ぎた日から落ちる**。実際 `2026-08-20T03:04:05Z` をハードコード
	// していて、その日の 03:04 UTC に develop が赤くなった (#2646)。
	// private role では assigned=false が 400 NO_SUCH_ROLE に化けるので、
	// 症状 (200 のはずが 400) からは時刻依存だと分かりにくい。
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	assignments.Assignments["viewer:private"] = &model.RoleAssignment{UserID: "viewer", RoleID: "private", ExpiresAt: &expires}

	rec := postAssignment(h, `{"roleId":"private"}`, "viewer")
	require.Equal(t, http.StatusOK, rec.Code)
	// 期待値も同じ値から組み立てる。文字列を別に持つと二重管理になる。
	want := fmt.Sprintf(
		`{"assigned":true,"expiresAt":%q,"role":{"id":"private","target":"manual","isPublic":false,"canEditMembersByModerator":false}}`,
		entity.ISOMillis(expires))
	assert.JSONEq(t, want, rec.Body.String())
}

func TestAssignmentShow_PrivateInactiveMatchesMissing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		createRole bool
		expiresAt  *time.Time
	}{
		{name: "missing"},
		{name: "unassigned", createRole: true},
		{name: "expired", createRole: true, expiresAt: func() *time.Time { v := time.Now().Add(-time.Hour); return &v }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, assignments, rolesRepo := assignmentFixture(t)
			if tc.createRole {
				rolesRepo.Roles["secret"] = &model.Role{ID: "secret", IsPublic: false, CanEditMembersByModerator: true}
			}
			if tc.expiresAt != nil {
				assignments.Assignments["viewer:secret"] = &model.RoleAssignment{UserID: "viewer", RoleID: "secret", ExpiresAt: tc.expiresAt}
			}

			rec := postAssignment(h, `{"roleId":"secret"}`, "viewer")
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, assignmentNoSuchRoleJSON, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "secret")
			assert.NotContains(t, rec.Body.String(), "isPublic")
		})
	}
}

func TestAssignmentShow_AssignmentFailurePrecedesRoleDisclosure(t *testing.T) {
	for _, roleExists := range []bool{false, true} {
		roleRepo := testutil.NewMockRoleRepository()
		if roleExists {
			roleRepo.Roles["secret"] = &model.Role{ID: "secret", IsPublic: false}
		}
		assignRepo := &assignmentExactRepo{
			MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo),
			err:                          errors.New("SELECT assignment: connection failed"),
		}
		h := assignmentHandler(t, roleRepo, assignRepo)

		rec := postAssignment(h, `{"roleId":"secret"}`, "viewer")
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.JSONEq(t, assignmentInternalErrorJSON, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "secret")
		assert.NotContains(t, rec.Body.String(), "SELECT")
	}
}

func TestAssignmentShow_RolePersistenceFailure(t *testing.T) {
	base := testutil.NewMockRoleRepository()
	assignRepo := &assignmentExactRepo{MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(base)}
	h := assignmentHandler(t, &assignmentFailingRoleRepo{RoleRepository: base}, assignRepo)

	rec := postAssignment(h, `{"roleId":"secret"}`, "viewer")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, assignmentInternalErrorJSON, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "SELECT")
}

func TestAssignmentShow_InvalidParam(t *testing.T) {
	h, _, _ := assignmentFixture(t)
	rec := postAssignment(h, `{}`, "viewer")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"INVALID_PARAM"`)
}

// conditional role は role_assignment 行を持たず condFormula の read 時評価で決まる。
// この endpoint は行だけを見るので assigned は常に false になる。呼び出し側が
// 「条件を満たしていない」と誤読しないよう role.target を返している (#2633)。
func TestAssignmentShow_ConditionalRoleIsNeverAssigned(t *testing.T) {
	h, assignments, rolesRepo := assignmentFixture(t)
	rolesRepo.Roles["cond"] = &model.Role{ID: "cond", Target: model.RoleTargetConditional, IsPublic: true}

	rec := postAssignment(h, `{"roleId":"cond"}`, "viewer")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"assigned":false,"expiresAt":null,"role":{"id":"cond","target":"conditional","isPublic":true,"canEditMembersByModerator":false}}`, rec.Body.String())
	// condFormula の評価には全 role の走査が要る。exact lookup の O(1) 性を保つため
	// 評価しないので、参照するのは FindActive 1 回だけ。
	assert.Equal(t, 1, assignments.findCalls)
	assert.Zero(t, assignments.listCalls)
}

// private な conditional role は、条件を満たす viewer にも NO_SUCH_ROLE になる。
// existence oracle 対策の秘匿と conditional の制約が重なる箇所で、緩めると
// oracle が戻るため意図的にこの挙動にしている (#2633)。
//
// 保証は「NO_SUCH_ROLE が返る」ことではなく **存在しない role と区別できない**
// ことなので、両者のレスポンスを突き合わせる。status だけを見ると、body に
// target や isPublic が漏れても通ってしまう。
func TestAssignmentShow_PrivateConditionalRoleIsHidden(t *testing.T) {
	h, _, rolesRepo := assignmentFixture(t)
	rolesRepo.Roles["cond"] = &model.Role{ID: "cond", Target: model.RoleTargetConditional, IsPublic: false}

	hidden := postAssignment(h, `{"roleId":"cond"}`, "viewer")
	missing := postAssignment(h, `{"roleId":"nonexistent"}`, "viewer")

	require.Equal(t, http.StatusBadRequest, hidden.Code)
	assert.JSONEq(t, assignmentNoSuchRoleJSON, hidden.Body.String())
	assert.Equal(t, missing.Code, hidden.Code)
	assert.JSONEq(t, missing.Body.String(), hidden.Body.String())
}
