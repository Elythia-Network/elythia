package admin_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// **モデレーターが管理者ロールを付け外しできないこと (#3037)。**
//
// `canEditMembersByModerator` は「モデレーターがメンバーを編集してよいか」
// しか見ない。管理者ロールにそのチェックが立っていると、モデレーターは自分
// 自身にそのロールを付けられる = 管理者へ昇格できた。ロールの作成・更新は
// 管理者専用なのに、付け外しだけがこの穴で抜けていた。
//
// **upstream (`assign.ts:69-76`) も同じ状態だが、そちらに合わせない。**
// mk-go を厳しい側に倒して `docs/divergence.md` に記録してある。
func TestRolesAssign_ModeratorCannotGrantAdministratorRole(t *testing.T) {
	h, users, _, roles, assigns := newTestHandlerWithAssign(t)
	users.Users["mod1"] = &model.User{ID: "mod1"}
	roles.Roles["admin-role"] = &model.Role{
		ID:                        "admin-role",
		Target:                    model.RoleTargetManual,
		IsAdministrator:           true,
		CanEditMembersByModerator: true, // ここが立っているのが前提
	}

	rec := doPost(h.RolesAssign, `{"userId":"mod1","roleId":"admin-role"}`, &model.User{ID: "mod1"})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
	assigned, err := assigns.Exists("mod1", "admin-role")
	require.NoError(t, err)
	assert.False(t, assigned, "モデレーターが自分に管理者ロールを付けられている")
}

// **unassign も塞ぐ。** 管理者ロールを外せるなら、モデレーターが管理者を
// 降格させて実質の最上位になれる。
func TestRolesUnassign_ModeratorCannotRevokeAdministratorRole(t *testing.T) {
	h, users, _, roles, assigns := newTestHandlerWithAssign(t)
	users.Users["admin1"] = &model.User{ID: "admin1"}
	roles.Roles["admin-role"] = &model.Role{
		ID:                        "admin-role",
		Target:                    model.RoleTargetManual,
		IsAdministrator:           true,
		CanEditMembersByModerator: true,
	}
	require.NoError(t, assigns.Create(&model.RoleAssignment{ID: "a1", UserID: "admin1", RoleID: "admin-role"}))

	rec := doPost(h.RolesUnassign, `{"userId":"admin1","roleId":"admin-role"}`, &model.User{ID: "mod1"})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
	assigned, err := assigns.Exists("admin1", "admin-role")
	require.NoError(t, err)
	assert.True(t, assigned, "モデレーターが管理者ロールを剥奪できている")
}

// **管理者自身は従来どおり付けられる。** これが無いと「管理者ロールは誰も
// 付けられない」実装でも上の 2 つが通る。
func TestRolesAssign_AdministratorCanGrantAdministratorRole(t *testing.T) {
	h, users, _, roles, assigns := newTestHandlerWithAssign(t)
	users.Users["target"] = &model.User{ID: "target"}
	roles.Roles["admin-role"] = &model.Role{
		ID:              "admin-role",
		Target:          model.RoleTargetManual,
		IsAdministrator: true,
	}
	// 実行者を実際の管理者にする (role service は割り当てから判定する)。
	require.NoError(t, assigns.Create(&model.RoleAssignment{ID: "a0", UserID: "boss", RoleID: "admin-role"}))

	rec := doPost(h.RolesAssign, `{"userId":"target","roleId":"admin-role"}`, &model.User{ID: "boss"})

	require.Equal(t, http.StatusNoContent, rec.Code, "管理者が管理者ロールを付けられない")
	assigned, err := assigns.Exists("target", "admin-role")
	require.NoError(t, err)
	assert.True(t, assigned)
}

// **管理者ロール以外は従来どおり。** モデレーターが
// `canEditMembersByModerator` の立った普通のロールを付けられること。
func TestRolesAssign_ModeratorStillGrantsOrdinaryRole(t *testing.T) {
	h, users, _, roles, assigns := newTestHandlerWithAssign(t)
	users.Users["u1"] = &model.User{ID: "u1"}
	roles.Roles["ordinary"] = &model.Role{
		ID:                        "ordinary",
		Target:                    model.RoleTargetManual,
		CanEditMembersByModerator: true,
	}

	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"ordinary"}`, &model.User{ID: "mod1"})

	require.Equal(t, http.StatusNoContent, rec.Code)
	assigned, err := assigns.Exists("u1", "ordinary")
	require.NoError(t, err)
	assert.True(t, assigned)
}

// **条件つきロール越しの間接的な昇格も塞ぐ (#3037 レビュー 2 周目)。**
//
// `checkConditionalPrivilege` は「利用者本人が条件を満たせるか」、
// `requireCanEditRoleMembers` は「そのロール自身が管理者ロールか」しか見ない。
// だから
//
//	staff            = 手動 / 権限なし / canEditMembersByModerator: true
//	conditional-boss = 条件つき / isAdministrator / roleAssignedTo: staff
//
// という 2 つを作ると、**モデレーターが `staff` を自分に付けるだけで管理者に
// なれる**。`roleAssignedTo` は「誰かが配る必要がある」ので自己付与判定は
// 通り、`staff` 自身は管理者ロールではないので付け外しの判定も通る。
func TestRolesAssign_ModeratorCannotGrantARoleThatUnlocksAConditionalAdmin(t *testing.T) {
	h, users, _, roles, assigns := newTestHandlerWithAssign(t)
	users.Users["mod1"] = &model.User{ID: "mod1"}
	roles.Roles["staff"] = &model.Role{
		ID:                        "staff",
		Target:                    model.RoleTargetManual,
		CanEditMembersByModerator: true,
	}
	roles.Roles["conditional-boss"] = &model.Role{
		ID:              "conditional-boss",
		Target:          model.RoleTargetConditional,
		IsAdministrator: true,
		CondFormula:     []byte(`{"type":"roleAssignedTo","roleId":"staff"}`),
	}

	rec := doPost(h.RolesAssign, `{"userId":"mod1","roleId":"staff"}`, &model.User{ID: "mod1"})

	assert.Equal(t, http.StatusBadRequest, rec.Code, "間接的な昇格が通っている: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
	assigned, err := assigns.Exists("mod1", "staff")
	require.NoError(t, err)
	assert.False(t, assigned, "モデレーターが自分に昇格用のロールを付けられている")
}

// **参照されていない素のロールは従来どおりモデレーターが配れる。**
// これが無いと「モデレーターは何も配れない」実装でも上のテストが通る。
func TestRolesAssign_ModeratorCanStillGrantAnUnreferencedRole(t *testing.T) {
	h, users, _, roles, assigns := newTestHandlerWithAssign(t)
	users.Users["mod1"] = &model.User{ID: "mod1"}
	users.Users["target"] = &model.User{ID: "target"}
	roles.Roles["staff"] = &model.Role{
		ID:                        "staff",
		Target:                    model.RoleTargetManual,
		CanEditMembersByModerator: true,
	}

	rec := doPost(h.RolesAssign, `{"userId":"target","roleId":"staff"}`, &model.User{ID: "mod1"})

	require.Less(t, rec.Code, 300, "普通のロールを配れなくなっている: %s", rec.Body.String())
	assigned, err := assigns.Exists("target", "staff")
	require.NoError(t, err)
	assert.True(t, assigned)
}

// **外す側も塞ぐ。** 付けられるなら外せるので、片方だけでは意味がない。
func TestRolesUnassign_ModeratorCannotRemoveARoleThatUnlocksAConditionalAdmin(t *testing.T) {
	h, users, _, roles, assigns := newTestHandlerWithAssign(t)
	users.Users["boss"] = &model.User{ID: "boss"}
	roles.Roles["staff"] = &model.Role{
		ID:                        "staff",
		Target:                    model.RoleTargetManual,
		CanEditMembersByModerator: true,
	}
	roles.Roles["conditional-boss"] = &model.Role{
		ID:              "conditional-boss",
		Target:          model.RoleTargetConditional,
		IsAdministrator: true,
		CondFormula:     []byte(`{"type":"roleAssignedTo","roleId":"staff"}`),
	}
	require.NoError(t, assigns.Create(&model.RoleAssignment{ID: "a1", UserID: "boss", RoleID: "staff"}))

	rec := doPost(h.RolesUnassign, `{"userId":"boss","roleId":"staff"}`, &model.User{ID: "mod1"})

	assert.Equal(t, http.StatusBadRequest, rec.Code, "降格が通っている: %s", rec.Body.String())
	assigned, err := assigns.Exists("boss", "staff")
	require.NoError(t, err)
	assert.True(t, assigned, "モデレーターが管理者を降格させられている")
}

// failingRoleListRepo makes only List fail — the partial outage the
// indirect-privilege check has to survive.
type failingRoleListRepo struct {
	*testutil.MockRoleRepository
}

func (r *failingRoleListRepo) List() ([]*model.Role, error) {
	return nil, errors.New("role list is down")
}

// **ロールを列挙できない窓で付け外しを通さない (#3037 レビュー 2 周目)。**
//
// 「特権を配らないことを確かめてから触る」判定なので、fail-open にすると
// まさにこの昇格が成立する (#2792 と同じ形)。
func TestRolesAssign_UndeterminedIndirectPrivilegeIsNotTreatedAsSafe(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["mod1"] = &model.User{ID: "mod1"}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["staff"] = &model.Role{
		ID:                        "staff",
		Target:                    model.RoleTargetManual,
		CanEditMembersByModerator: true,
	}
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	flaky := &failingRoleListRepo{MockRoleRepository: roleRepo}
	roleSvc := corerole.NewService(flaky, assignRepo, metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleSvc, metaRepo, userRepo, idGen)

	rec := doPost(h.RolesAssign, `{"userId":"mod1","roleId":"staff"}`, &model.User{ID: "mod1"})

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"ロールを列挙できない窓で付け外しを通している: %s", rec.Body.String())
	assigned, aerr := assignRepo.Exists("mod1", "staff")
	require.NoError(t, aerr)
	assert.False(t, assigned)
}
