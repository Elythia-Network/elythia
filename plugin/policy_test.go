package plugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefinitionValidate_PolicyOnly(t *testing.T) {
	definition := Definition{
		Name: "policy-only", APIVersion: APIVersion,
		EffectivePolicies: func(Context, EffectivePolicyInvalidator) (EffectivePolicyRegistration, error) {
			return EffectivePolicyRegistration{}, nil
		},
	}
	require.NoError(t, definition.Validate())
}

func TestEffectivePolicyRegistrationValidate(t *testing.T) {
	resolver := EffectivePolicyResolver(func(context.Context, EffectivePolicyRequest) ([]EffectivePolicyContribution, error) {
		return nil, nil
	})
	tests := []struct {
		name string
		reg  EffectivePolicyRegistration
		want string
	}{
		{name: "valid", reg: EffectivePolicyRegistration{Keys: []string{"a", "b"}, Resolve: resolver}},
		{name: "nil resolver", reg: EffectivePolicyRegistration{Keys: []string{"a"}}, want: "Resolve が nil"},
		{name: "empty keys", reg: EffectivePolicyRegistration{Resolve: resolver}, want: "Keys が空"},
		{name: "empty key", reg: EffectivePolicyRegistration{Keys: []string{""}, Resolve: resolver}, want: "空のキー"},
		{name: "duplicate", reg: EffectivePolicyRegistration{Keys: []string{"a", "a"}, Resolve: resolver}, want: "重複"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.reg.Validate()
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEffectivePolicyRequest_CarriesActiveAssignmentsAlongsideRoleIDs(t *testing.T) {
	// plugin作者は RoleID と AssignmentID を別々に読む。片方だけを持つ型に
	// 戻すと「assignment単位に持つplugin stateが別のroleのもの」と誤って付け替えられる。
	assignment := ActiveRoleAssignment{RoleID: "r1", AssignmentID: "a1"}

	request := EffectivePolicyRequest{
		UserID:            "u1",
		RoleIDs:           []string{"r1", "r2"},
		ActiveAssignments: []ActiveRoleAssignment{assignment},
	}

	assert.Equal(t, "r1", request.ActiveAssignments[0].RoleID)
	assert.Equal(t, "a1", request.ActiveAssignments[0].AssignmentID)
	// ActiveAssignments は RoleIDs を置き換えず、並んで運ぶ。
	assert.Equal(t, []string{"r1", "r2"}, request.RoleIDs)
}
