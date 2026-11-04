package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// #3999: roleAssigned の通知に載るロールの usersCount は、凍結中とローカルの
// 削除済みを数えない (roles/users がモデレーターでない人に見せる人数)。
func TestRoleNotifLookup_UsersCountSkipsHiddenMembers(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	userRepo := testutil.NewMockUserRepository()
	assignRepo.UserRepo = userRepo
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := corerole.NewService(roleRepo, assignRepo, metaRepo, idGen)

	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "R", IsPublic: true, Target: model.RoleTargetManual}
	userRepo.Users["alive"] = &model.User{ID: "alive", Username: "alive"}
	userRepo.Users["suspended"] = &model.User{ID: "suspended", Username: "suspended", IsSuspended: true}
	userRepo.Users["deleted"] = &model.User{ID: "deleted", Username: "deleted", IsDeleted: true}
	for _, uid := range []string{"alive", "suspended", "deleted"} {
		require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a" + uid, RoleID: "r1", UserID: uid}))
	}

	lookup := newRoleNotifLookup(roleRepo, svc, idGen)
	packed, ok := lookup("r1")
	require.True(t, ok)
	assert.Equal(t, "r1", packed["id"])
	assert.Equal(t, 1, packed["usersCount"])

	_, ok = lookup("ghost")
	assert.False(t, ok, "a deleted role resolves to false")
}
