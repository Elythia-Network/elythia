package role

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2106 S5: time-limited assignment を持つ user の role cache entry は固定 TTL (5分)
// ではなく assignment の expiresAt で失効する (期限切れ role の延命防止)。
func TestGetUserRoles_CacheExpiryCappedAtAssignmentExpiry(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(roleRepo, assignRepo, metaRepo, idGen)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Temp"}

	// time-limited (90s < 5min TTL) → entry は soon で失効する。
	soon := time.Now().Add(90 * time.Second)
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{
		ID: "a1", UserID: "user1", RoleID: "r1", ExpiresAt: &soon, Role: roleRepo.Roles["r1"],
	}
	_, err := svc.GetUserRoles("user1")
	require.NoError(t, err)
	svc.userRoleCacheMu.RLock()
	v, ok := svc.userRoleCache["user1"]
	svc.userRoleCacheMu.RUnlock()
	require.True(t, ok)
	assert.False(t, v.expiresAt.After(soon),
		"cache entry must expire by the assignment expiresAt, not the 5min TTL")

	// 期限なし assignment は従来通り full TTL (~5min)。
	assignRepo.Assignments["user2:r1"] = &model.RoleAssignment{
		ID: "a2", UserID: "user2", RoleID: "r1", Role: roleRepo.Roles["r1"],
	}
	_, err = svc.GetUserRoles("user2")
	require.NoError(t, err)
	svc.userRoleCacheMu.RLock()
	v2, ok := svc.userRoleCache["user2"]
	svc.userRoleCacheMu.RUnlock()
	require.True(t, ok)
	assert.True(t, v2.expiresAt.After(time.Now().Add(4*time.Minute)),
		"non-expiring assignment keeps the full TTL")
}

func TestGetUserRoles_ExpiredEntryPublishesAndReturnsIndependentSnapshot(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(roleRepo, assignRepo, metaRepo, idGen)
	oldRole := &model.Role{ID: "r1", Name: "Old"}
	roleRepo.Roles["r1"] = oldRole
	assignRepo.Assignments["u1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "u1", RoleID: "r1"}

	_, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	svc.userRoleCacheMu.RLock()
	v, ok := svc.userRoleCache["u1"]
	svc.userRoleCacheMu.RUnlock()
	require.True(t, ok)
	svc.userRoleCacheMu.Lock()
	v.expiresAt = time.Now().Add(-time.Second)
	svc.userRoleCacheMu.Unlock()
	replacement := &model.Role{ID: "r1", Name: "New"}
	roleRepo.Roles["r1"] = replacement

	refreshed, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	require.Len(t, refreshed, 1)
	assert.Equal(t, "New", refreshed[0].Name)
	require.NotSame(t, replacement, refreshed[0])
	refreshed[0].Name = "Changed"

	cached, err := svc.GetUserRoles("u1")
	require.NoError(t, err)
	assert.Equal(t, "New", cached[0].Name)
}

func TestUserInvalidationDoesNotRetainInactiveEpochState(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(roleRepo, assignRepo, metaRepo, idGen)
	require.NoError(t, svc.RegisterEffectivePolicyProvider("p", plugin.EffectivePolicyRegistration{
		Keys: []string{"canSearchNotes"},
		Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return nil, nil
		},
	}))

	for i := range 1000 {
		require.NoError(t, svc.InvalidateUser(context.Background(), fmt.Sprintf("u%d", i)))
	}

	svc.userRoleCacheMu.RLock()
	assert.Empty(t, svc.userRoleEpoch)
	svc.userRoleCacheMu.RUnlock()
	svc.policyProviderMu.RLock()
	runtime := svc.policyProviders[0].runtime
	svc.policyProviderMu.RUnlock()
	runtime.cacheMu.Lock()
	assert.Empty(t, runtime.userEpoch)
	runtime.cacheMu.Unlock()
}
