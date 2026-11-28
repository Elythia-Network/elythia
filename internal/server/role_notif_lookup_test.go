package server

import (
	"errors"
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
	packed, ok, err := lookup("r1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "r1", packed["id"])
	assert.Equal(t, 1, packed["usersCount"])

	_, ok, err = lookup("ghost")
	assert.False(t, ok, "a deleted role resolves to false")
	assert.NoError(t, err, "a deleted role is not a failure, so the page may remember it")
}

type failingRoleRepo struct {
	*testutil.MockRoleRepository
	err error
}

func (r failingRoleRepo) FindByID(string) (*model.Role, error) { return nil, r.err }

type failingCountAssignRepo struct {
	*testutil.MockRoleAssignmentRepository
	err error
}

// エラーと一緒に 0 以外を返し、失敗のときに 0 人で出すことを確かめる。
func (r failingCountAssignRepo) CountVisibleByRole(string) (int, error) { return 7, r.err }

// #4263: 取得や人数を数える文の失敗は、エラーとして返す。返さないと、ページの中で
// 「削除済み」や「0 人」として覚えられ、同じロールの通知すべてに広がる。表示は
// これまでどおりで、取得の失敗は通知を落とし、人数の失敗は 0 人で出す。
func TestRoleNotifLookup_ReportsFailures(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "R", IsPublic: true, Target: model.RoleTargetManual}
	boom := errors.New("connection reset")

	svc := corerole.NewService(roleRepo, assignRepo, metaRepo, idGen)
	_, ok, err := newRoleNotifLookup(failingRoleRepo{roleRepo, boom}, svc, idGen)("r1")
	assert.False(t, ok, "the notification is dropped when the role cannot be read")
	assert.ErrorIs(t, err, boom)

	svc = corerole.NewService(roleRepo, failingCountAssignRepo{assignRepo, boom}, metaRepo, idGen)
	packed, ok, err := newRoleNotifLookup(roleRepo, svc, idGen)("r1")
	require.True(t, ok, "a failed usersCount still shows the role")
	assert.Equal(t, 0, packed["usersCount"])
	assert.ErrorIs(t, err, boom)
}
