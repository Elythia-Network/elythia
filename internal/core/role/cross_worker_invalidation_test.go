package role_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **ロールの変更を他のワーカーへ伝える (#3037)。**
//
// `roleCacheTTL` は 5 分で、複数プロセス構成 (`MK_ONLY_SERVER` /
// `MK_ONLY_QUEUE`) は明示的にサポートされている。伝えないと、侵害された管理者の
// ロールを剥奪しても**剥奪操作を受け付けなかった側のノードでは最大 5 分間、
// 管理 API が通り続ける**。
func newHookedService(t *testing.T) (*role.Service, *int) {
	t.Helper()
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	calls := 0
	svc.SetInvalidationHook(func() { calls++ })
	return svc, &calls
}

func TestInvalidationHook_FiresOnUserInvalidation(t *testing.T) {
	svc, calls := newHookedService(t)
	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))
	assert.Equal(t, 1, *calls, "利用者単位の invalidate が他ワーカーへ伝わらない")
}

func TestInvalidationHook_FiresOnRoleInvalidation(t *testing.T) {
	svc, calls := newHookedService(t)
	require.NoError(t, svc.InvalidateRolePolicies(context.Background(), "r1"))
	assert.Equal(t, 1, *calls, "ロール単位の invalidate が他ワーカーへ伝わらない")
}

// **受信側から再通知しない。** ここで通知すると、ワーカー同士が通知を投げ合って
// 止まらなくなる。
func TestInvalidateAllCachesLocally_DoesNotNotify(t *testing.T) {
	svc, calls := newHookedService(t)
	svc.InvalidateAllCachesLocally()
	assert.Equal(t, 0, *calls, "受信側から publish し返している (通知のループになる)")
}

// **受信側がキャッシュを実際に落としていること。** hook を繋いでも、受け取った
// 側が何もしなければ意味が無い。
func TestInvalidateAllCachesLocally_DropsTheRoleCache(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	admin := &model.Role{ID: "r1", IsAdministrator: true}
	roleRepo.Roles["r1"] = admin
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "a1", RoleID: "r1", UserID: "u1"}))
	require.True(t, svc.IsAdministrator("u1"), "前提: 付与されている")

	// 他ワーカーが剥奪した状況を作る (このプロセスの invalidate は通っていない)。
	require.NoError(t, assignRepo.Delete("u1", "r1"))
	assert.True(t, svc.IsAdministrator("u1"), "前提: キャッシュに残っている")

	svc.InvalidateAllCachesLocally()
	assert.False(t, svc.IsAdministrator("u1"), "受信しても古いロールが残っている")
}

// hook 未配線でも落ちない (テストや単体構成)。
func TestInvalidationHook_UnwiredIsSafe(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	svc.SetInvalidationHook(nil)

	require.NotPanics(t, func() {
		_ = svc.InvalidateUser(context.Background(), "u1")
		_ = svc.InvalidateRolePolicies(context.Background(), "r1")
		svc.InvalidateAllCachesLocally()
	})
}
