package role_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// Clearing uploadableFileTypes in the admin UI sends `[""]`. Upstream treats
// the resulting empty list as "no type is allowed", so the effective policy
// must stay empty instead of falling back to the default allowlist.
func TestGetUserPolicies_UploadableFileTypesClearedInBasePolicies(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{ID: "x", Policies: datatypes.JSON([]byte(`{"uploadableFileTypes":[""]}`))}

	assert.Equal(t, []string{}, svc.GetUserPolicies("user1")["uploadableFileTypes"])
	assert.Equal(t, []string{}, svc.GetUserPolicies("")["uploadableFileTypes"])
}

func TestGetUserPolicies_UploadableFileTypesClearedInRole(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID: "r1",
		Policies: datatypes.JSON([]byte(`{
			"uploadableFileTypes": {"useDefault": false, "priority": 1, "value": [""]}
		}`)),
	}
	assignRepo.Assignments["user1:r1"] = &model.RoleAssignment{ID: "a1", UserID: "user1", RoleID: "r1"}

	assert.Equal(t, []string{}, svc.GetUserPolicies("user1")["uploadableFileTypes"])
	// 割り当てていない人は既定のまま。
	assert.Equal(t, role.DefaultPolicies()["uploadableFileTypes"], svc.GetUserPolicies("user2")["uploadableFileTypes"])
}

// A provider that returns a typed nil slice says nothing about the policy, so
// the user keeps the default allowlist. Only a non-nil empty slice is an
// explicit empty list (deny every type).
func TestEffectivePolicy_ProviderNilUploadableFileTypesKeepsDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "nil []string", value: []string(nil)},
		{name: "nil []any", value: []any(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, roleRepo, assignRepo, _ := newTestService(t)
			// optOutNotificationTypes は共通部分なので、nil が空の一覧として
			// 混ざるとロールが切った型まで消える。
			roleRepo.Roles["r1"] = &model.Role{
				ID:       "r1",
				Policies: datatypes.JSON([]byte(`{"optOutNotificationTypes":{"useDefault":false,"priority":2,"value":["follow"]}}`)),
			}
			assign(t, assignRepo, "user1", "r1")
			registerProvider(t, svc, "p", []string{"uploadableFileTypes", "optOutNotificationTypes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{
					{Key: "uploadableFileTypes", Priority: 2, Value: tc.value},
					{Key: "optOutNotificationTypes", Priority: 2, Value: tc.value},
				}, nil
			})
			policies, err := svc.GetUserPoliciesChecked("user1")
			require.NoError(t, err)
			// ロールを経た集約は和集合を並べ替えるので、並びは見ない。
			assert.ElementsMatch(t, role.DefaultPolicies()["uploadableFileTypes"], policies["uploadableFileTypes"])
			assert.Equal(t, []string{"follow"}, policies["optOutNotificationTypes"])
		})
	}
}

// A typed nil list must behave like no contribution at all. If it joined the
// cascade, its higher priority would select a tier with no usable candidate
// and the result would fall back to the default, dropping the value a lower
// priority role set.
func TestEffectivePolicy_ProviderNilAtHigherPriorityKeepsRoleValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "nil []string", value: []string(nil)},
		{name: "nil []any", value: []any(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, roleRepo, assignRepo, _ := newTestService(t)
			roleRepo.Roles["r1"] = &model.Role{
				ID: "r1",
				Policies: datatypes.JSON([]byte(`{
					"uploadableFileTypes":{"useDefault":false,"priority":0,"value":["image/*"]},
					"optOutNotificationTypes":{"useDefault":false,"priority":0,"value":["follow"]}
				}`)),
			}
			assign(t, assignRepo, "user1", "r1")

			// 前提: プラグインが無ければロールの値が効いている。
			policies, err := svc.GetUserPoliciesChecked("user1")
			require.NoError(t, err)
			require.Equal(t, []string{"image/*"}, policies["uploadableFileTypes"])
			require.Equal(t, []string{"follow"}, policies["optOutNotificationTypes"])

			registerProvider(t, svc, "p", []string{"uploadableFileTypes", "optOutNotificationTypes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{
					{Key: "uploadableFileTypes", Priority: 2, Value: tc.value},
					{Key: "optOutNotificationTypes", Priority: 2, Value: tc.value},
				}, nil
			})
			policies, err = svc.GetUserPoliciesChecked("user1")
			require.NoError(t, err)
			assert.Equal(t, []string{"image/*"}, policies["uploadableFileTypes"])
			assert.Equal(t, []string{"follow"}, policies["optOutNotificationTypes"])
		})
	}
}

// A typed nil list used as a replacement is not a replacement: the role keeps
// the value the administrator set.
func TestEffectivePolicy_ProviderNilReplacementKeepsRoleValue(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:     "r1",
		Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{
			"uploadableFileTypes":{"useDefault":false,"priority":1,"value":["image/*"]},
			"optOutNotificationTypes":{"useDefault":false,"priority":1,"value":["follow"]}
		}`)),
	}
	assign(t, assignRepo, "user1", "r1")
	registerProvider(t, svc, "p", []string{"uploadableFileTypes", "optOutNotificationTypes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: "uploadableFileTypes", Value: []string(nil), ReplaceRoleID: "r1"},
			{Key: "optOutNotificationTypes", Value: []string(nil), ReplaceRoleID: "r1"},
		}, nil
	})
	policies, err := svc.GetUserPoliciesChecked("user1")
	require.NoError(t, err)
	assert.Equal(t, []string{"image/*"}, policies["uploadableFileTypes"])
	assert.Equal(t, []string{"follow"}, policies["optOutNotificationTypes"])
}

func TestEffectivePolicy_ProviderEmptyUploadableFileTypesDeniesAll(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"uploadableFileTypes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "uploadableFileTypes", Priority: 2, Value: []string{}}}, nil
	})
	policies, err := svc.GetUserPoliciesChecked("user1")
	require.NoError(t, err)
	assert.Equal(t, []string{}, policies["uploadableFileTypes"])
}
