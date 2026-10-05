package role_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// policyJSON builds the on-disk `{key: {useDefault, priority, value}}` shape.
//
// **priority を引数に取る。** 固定にすると、`computePolicy` の cascade
// (priority 2 群があればそれだけを集約) を通る入力を一度も試さないまま
// 「HasRolePolicy と一致する」と宣言することになる。実際それで割れていた。
func policyJSON(t *testing.T, key string, value any, useDefault bool, priority int) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		key: map[string]any{"useDefault": useDefault, "priority": priority, "value": value},
	})
	require.NoError(t, err)
	return raw
}

func idsOf(users []*model.User) map[string]bool {
	out := map[string]bool{}
	for _, u := range users {
		out[u.ID] = true
	}
	return out
}

// #2987: 宛先は「審査できる人」= root / 管理者 + そのキーを true にするロール。
func TestService_GetUsersWithPolicy(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	svc.SetUserRepo(userRepo)

	const key = role.PolicyCanManageCustomEmojis
	roleRepo.Roles["grant"] = &model.Role{ID: "grant", Policies: policyJSON(t, key, true, false, 0)}
	roleRepo.Roles["deny"] = &model.Role{ID: "deny", Policies: policyJSON(t, key, false, false, 0)}
	roleRepo.Roles["adminrole"] = &model.Role{ID: "adminrole", IsAdministrator: true}
	// **モデレーターロールは含めない。** HasRolePolicy はモデレーターを短絡しない。
	roleRepo.Roles["modrole"] = &model.Role{ID: "modrole", IsModerator: true}

	for _, a := range []*model.RoleAssignment{
		{ID: "a1", UserID: "granted", RoleID: "grant"},
		{ID: "a2", UserID: "denied", RoleID: "deny"},
		{ID: "a3", UserID: "admin1", RoleID: "adminrole"},
		{ID: "a4", UserID: "mod1", RoleID: "modrole"},
	} {
		require.NoError(t, assignRepo.Create(a))
		userRepo.Users[a.UserID] = &model.User{ID: a.UserID}
	}
	rootID := "root1"
	userRepo.Users[rootID] = &model.User{ID: rootID}
	metaRepo.Meta = &model.Meta{RootUserID: &rootID}

	users, err := svc.GetUsersWithPolicy(key)
	require.NoError(t, err)
	ids := idsOf(users)
	assert.True(t, ids["granted"], "policy を true にするロールのメンバーが入る")
	assert.True(t, ids["admin1"], "管理者は policy を見ずに通る")
	assert.True(t, ids["root1"], "root は常に通る")
	assert.False(t, ids["denied"], "false にしているロールは入らない")
	assert.False(t, ids["mod1"], "モデレーターは短絡しないので入らない")
}

// **useDefault は「与えていない」。** 既定が true のときに全ロールが該当すると、
// 「base policy でも全員へは広げない」が崩れる。
func TestService_GetUsersWithPolicy_UseDefaultDoesNotGrant(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	svc.SetUserRepo(userRepo)
	metaRepo.Meta = &model.Meta{}

	const key = role.PolicyCanManageCustomEmojis
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Policies: policyJSON(t, key, true, true, 0)}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}))
	userRepo.Users["u1"] = &model.User{ID: "u1"}

	users, err := svc.GetUsersWithPolicy(key)
	require.NoError(t, err)
	assert.False(t, idsOf(users)["u1"])
}

// 非 bool の値では与えない (fail-closed)。
func TestService_GetUsersWithPolicy_NonBoolDoesNotGrant(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	svc.SetUserRepo(userRepo)
	metaRepo.Meta = &model.Meta{}

	const key = role.PolicyCanManageCustomEmojis
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Policies: policyJSON(t, key, "true", false, 0)}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}))
	userRepo.Users["u1"] = &model.User{ID: "u1"}

	users, err := svc.GetUsersWithPolicy(key)
	require.NoError(t, err)
	assert.False(t, idsOf(users)["u1"])
}

func TestService_GetUsersWithPolicy_NilUserRepo(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	users, err := svc.GetUsersWithPolicy(role.PolicyCanManageCustomEmojis)
	require.NoError(t, err)
	assert.Nil(t, users)
}

// **priority 2 の否定は priority 0 の許可に勝つ。** `computePolicy` は
// priority 2 群があればそれだけを集約するので、priority 0 の true は無視される。
// ここが割れると「通知は届くのに審査 endpoint は 403」になる (この機能が
// 防ごうとした状態そのもの)。
func TestService_GetUsersWithPolicy_HighPriorityDenyWins(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	svc.SetUserRepo(userRepo)
	metaRepo.Meta = &model.Meta{}

	const key = role.PolicyCanManageCustomEmojis
	roleRepo.Roles["grant"] = &model.Role{ID: "grant", Policies: policyJSON(t, key, true, false, 0)}
	roleRepo.Roles["veto"] = &model.Role{ID: "veto", Policies: policyJSON(t, key, false, false, 2)}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "grant"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "veto"}))
	userRepo.Users["u1"] = &model.User{ID: "u1"}

	require.False(t, svc.HasRolePolicy("u1", key), "前提: 審査 endpoint は通さない")
	assert.False(t, idsOf(mustUsers(t, svc, key))["u1"],
		"審査できない人へ通知が飛ぶ (届いたのに押せない)")
}

// **列挙された人は必ず `HasRolePolicy` を満たす。**
//
// 逆向き (満たすのに列挙されない) は意図的な例外が 2 つある — `meta.policies`
// で全員に与えた場合と、conditional role でだけ与えられている場合。どちらも
// docs/divergence.md に書いてあるので、ここでは片側だけを不変条件にする。
func TestService_GetUsersWithPolicy_NeverListsSomeoneWhoCannot(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	svc.SetUserRepo(userRepo)

	const key = role.PolicyCanManageCustomEmojis
	roleRepo.Roles["grant"] = &model.Role{ID: "grant", Policies: policyJSON(t, key, true, false, 0)}
	roleRepo.Roles["adminrole"] = &model.Role{ID: "adminrole", IsAdministrator: true}
	roleRepo.Roles["modrole"] = &model.Role{ID: "modrole", IsModerator: true}
	for _, a := range []*model.RoleAssignment{
		{ID: "a1", UserID: "granted", RoleID: "grant"},
		{ID: "a2", UserID: "admin1", RoleID: "adminrole"},
		{ID: "a3", UserID: "mod1", RoleID: "modrole"},
	} {
		require.NoError(t, assignRepo.Create(a))
		userRepo.Users[a.UserID] = &model.User{ID: a.UserID}
	}
	userRepo.Users["nobody"] = &model.User{ID: "nobody"}
	metaRepo.Meta = &model.Meta{}

	// priority 違いも混ぜる (cascade を通る入力を必ず 1 つ含める)。
	roleRepo.Roles["veto"] = &model.Role{ID: "veto", Policies: policyJSON(t, key, false, false, 2)}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a4", UserID: "vetoed", RoleID: "grant"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a5", UserID: "vetoed", RoleID: "veto"}))
	userRepo.Users["vetoed"] = &model.User{ID: "vetoed"}

	listed := idsOf(mustUsers(t, svc, key))
	require.NotEmpty(t, listed, "1 人も列挙されないと、この検査は何も見ていない")
	for id := range listed {
		assert.Truef(t, svc.HasRolePolicy(id, key),
			"%s は審査できないのに通知の宛先になっている (届いたのに押せない)", id)
	}
	assert.False(t, listed["vetoed"], "priority 2 の否定を持つ人は宛先に入らない")
}

func mustUsers(t *testing.T, svc *role.Service, key string) []*model.User {
	t.Helper()
	users, err := svc.GetUsersWithPolicy(key)
	require.NoError(t, err)
	return users
}

// **候補集めは値も priority も見てはいけない。**
//
// `useDefault: true, priority: 2` は「このロールは高優先度でベース値を使う」
// という宣言で、`computePolicy` の cascade では**その群だけが集約される**。
// base が true ならこの人は審査できる。候補集めを「explicit に true か」で
// 絞ると、審査できるのに通知が来ない (偽陰性) 側へ倒れる。
func TestService_GetUsersWithPolicy_UseDefaultAtHighPriorityIsACandidate(t *testing.T) {
	svc, roleRepo, assignRepo, metaRepo := newTestService(t)
	userRepo := testutil.NewMockUserRepository()
	svc.SetUserRepo(userRepo)

	const key = role.PolicyCanManageCustomEmojis
	// base で全員に与えている構成。
	base, err := json.Marshal(map[string]any{key: true})
	require.NoError(t, err)
	metaRepo.Meta = &model.Meta{Policies: base}

	roleRepo.Roles["staff"] = &model.Role{ID: "staff", Policies: policyJSON(t, key, false, true, 2)}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "staff"}))
	userRepo.Users["u1"] = &model.User{ID: "u1"}

	require.True(t, svc.HasRolePolicy("u1", key), "前提: 審査 endpoint は通す")
	assert.True(t, idsOf(mustUsers(t, svc, key))["u1"],
		"審査できるのに通知が来ない (押せるのに届かない)")
}
