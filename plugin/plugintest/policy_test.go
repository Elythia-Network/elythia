package plugintest_test

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type policyInvalidatorRecorder struct{ users []string }

func (r *policyInvalidatorRecorder) InvalidateUser(_ context.Context, userID string) error {
	r.users = append(r.users, userID)
	return nil
}
func (r *policyInvalidatorRecorder) InvalidateRole(context.Context, string) error { return nil }

func TestEffectivePoliciesCapture(t *testing.T) {
	keys := []string{"canSearchNotes"}
	roles := []string{"role-a"}
	recorder := &policyInvalidatorRecorder{}
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(_ plugin.Context, invalidator plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			require.NoError(t, invalidator.InvalidateUser(context.Background(), "user"))
			return plugin.EffectivePolicyRegistration{
				Keys: keys,
				Resolve: func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					req.RoleIDs[0] = "mutated"
					return nil, nil
				},
			}, nil
		},
	}
	registration := plugintest.New(t).WithEffectivePolicyInvalidator(recorder).EffectivePolicies(definition)
	keys[0] = "changed"
	assert.Equal(t, []string{"canSearchNotes"}, registration.Keys)
	require.NoError(t, registration.Validate())
	_, err := registration.Resolve(context.Background(), plugin.EffectivePolicyRequest{RoleIDs: roles})
	require.NoError(t, err)
	assert.Equal(t, []string{"role-a"}, roles)
	assert.Equal(t, []string{"user"}, recorder.users)
}

func TestEffectivePoliciesAbsent(t *testing.T) {
	registration := plugintest.New(t).EffectivePolicies(plugin.Definition{})
	assert.Empty(t, registration.Keys)
	assert.Nil(t, registration.Resolve)
}

func TestEffectivePoliciesRejectsInvalidRegistration(t *testing.T) {
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{Keys: []string{"canSearchNotes"}}, nil
		},
	}
	if os.Getenv("MK_PLUGINTEST_INVALID_REGISTRATION_CHILD") == "1" {
		plugintest.New(t).EffectivePolicies(definition)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEffectivePoliciesRejectsInvalidRegistration$")
	cmd.Env = append(os.Environ(), "MK_PLUGINTEST_INVALID_REGISTRATION_CHILD=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "an invalid registration must fail the harness test process")
	assert.Contains(t, string(output), "EffectivePolicies の登録が不正です")
}

func TestEffectivePoliciesRejectsUnknownNativeKey(t *testing.T) {
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"noSuchPolicyKey"},
				Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					return nil, nil
				},
			}, nil
		},
	}
	if os.Getenv("MK_PLUGINTEST_UNKNOWN_POLICY_KEY_CHILD") == "1" {
		plugintest.New(t).EffectivePolicies(definition)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEffectivePoliciesRejectsUnknownNativeKey$")
	cmd.Env = append(os.Environ(), "MK_PLUGINTEST_UNKNOWN_POLICY_KEY_CHILD=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "an unknown native policy key must fail the harness test process")
	assert.Contains(t, string(output), "EffectivePolicies の登録が不正です")
	assert.NotContains(t, string(output), "noSuchPolicyKey")
}

func TestEffectivePoliciesRejectsInvalidResolverOutput(t *testing.T) {
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"canSearchNotes"},
				Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: "not-a-bool"}}, nil
				},
			}, nil
		},
	}
	if os.Getenv("MK_PLUGINTEST_INVALID_POLICY_OUTPUT_CHILD") == "1" {
		registration := plugintest.New(t).EffectivePolicies(definition)
		_, _ = registration.Resolve(context.Background(), plugin.EffectivePolicyRequest{RoleIDs: []string{}})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEffectivePoliciesRejectsInvalidResolverOutput$")
	cmd.Env = append(os.Environ(), "MK_PLUGINTEST_INVALID_POLICY_OUTPUT_CHILD=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "an invalid policy output must fail the harness test process")
	assert.Contains(t, string(output), "EffectivePolicies の出力が不正です")
}

func TestEffectivePoliciesCaptureActiveAssignments(t *testing.T) {
	assignments := []plugin.ActiveRoleAssignment{{RoleID: "role-a", AssignmentID: "assign-1"}}
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"canSearchNotes"},
				Resolve: func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					req.ActiveAssignments[0].RoleID = "mutated"
					return nil, nil
				},
			}, nil
		},
	}
	registration := plugintest.New(t).EffectivePolicies(definition)
	require.NoError(t, registration.Validate())

	_, err := registration.Resolve(context.Background(), plugin.EffectivePolicyRequest{ActiveAssignments: assignments})
	require.NoError(t, err)
	assert.Equal(t, "role-a", assignments[0].RoleID, "resolver must not alias the caller's ActiveAssignments")
}

func TestEffectivePoliciesAcceptsValidReplacement(t *testing.T) {
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"mentionLimit"},
				Resolve: func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					return []plugin.EffectivePolicyContribution{{
						Key: "mentionLimit", Value: 40, ReplaceRoleID: req.ActiveAssignments[0].RoleID,
					}}, nil
				},
			}, nil
		},
	}
	registration := plugintest.New(t).EffectivePolicies(definition)
	contributions, err := registration.Resolve(context.Background(), plugin.EffectivePolicyRequest{
		ActiveAssignments: []plugin.ActiveRoleAssignment{{RoleID: "role-a", AssignmentID: "assign-1"}},
	})
	require.NoError(t, err)
	require.Len(t, contributions, 1)
	assert.Equal(t, 40, contributions[0].Value)
	assert.Equal(t, "role-a", contributions[0].ReplaceRoleID)
}

func TestEffectivePoliciesRejectsInvalidReplacement(t *testing.T) {
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"mentionLimit"},
				Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					// 宣言していない role の置換は host でも不正。harness が本番と同じ契約で
					// 弾くことを確認する。
					return []plugin.EffectivePolicyContribution{{
						Key: "mentionLimit", Value: 40, ReplaceRoleID: "role-not-active",
					}}, nil
				},
			}, nil
		},
	}
	if os.Getenv("MK_PLUGINTEST_INVALID_REPLACEMENT_CHILD") == "1" {
		registration := plugintest.New(t).EffectivePolicies(definition)
		_, _ = registration.Resolve(context.Background(), plugin.EffectivePolicyRequest{
			ActiveAssignments: []plugin.ActiveRoleAssignment{{RoleID: "role-a", AssignmentID: "assign-1"}},
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEffectivePoliciesRejectsInvalidReplacement$")
	cmd.Env = append(os.Environ(), "MK_PLUGINTEST_INVALID_REPLACEMENT_CHILD=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "an invalid replacement must fail the harness test process")
	assert.Contains(t, string(output), "EffectivePolicies の出力が不正です")
}

func TestEffectivePoliciesRejectsReplacementChoosingAPriority(t *testing.T) {
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"mentionLimit"},
				Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					return []plugin.EffectivePolicyContribution{{
						Key: "mentionLimit", Value: 40, Priority: 1, ReplaceRoleID: "role-a",
					}}, nil
				},
			}, nil
		},
	}
	if os.Getenv("MK_PLUGINTEST_REPLACEMENT_PRIORITY_CHILD") == "1" {
		registration := plugintest.New(t).EffectivePolicies(definition)
		_, _ = registration.Resolve(context.Background(), plugin.EffectivePolicyRequest{
			ActiveAssignments: []plugin.ActiveRoleAssignment{{RoleID: "role-a", AssignmentID: "assign-1"}},
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEffectivePoliciesRejectsReplacementChoosingAPriority$")
	cmd.Env = append(os.Environ(), "MK_PLUGINTEST_REPLACEMENT_PRIORITY_CHILD=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "a replacement that chooses its own priority must fail the harness test process")
	assert.Contains(t, string(output), "EffectivePolicies の出力が不正です")
}

// **置換してよい role は resolver を呼ぶ前に確定させる。** harness は resolver に
// `ActiveAssignments` を**複製して**渡すので、resolver は自分の slice を書き換えられる。
// 複製し直してから判定すると「active な role を conditional な role に差し替える」置換が
// harness を通ってしまう。**caller の slice を守っているだけでは不十分で、判定に使う集合も
// 呼び出し前のもの**でなければならない。
func TestEffectivePoliciesRejectsReplacementOfAForgedRoleID(t *testing.T) {
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"mentionLimit"},
				Resolve: func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					// 本番の RoleIDs には conditional な role も出るので、その ID を
					// 差し込んで target にするのは実際にできる攻撃。
					req.ActiveAssignments[0].RoleID = "role-conditional"
					return []plugin.EffectivePolicyContribution{{
						Key: "mentionLimit", Value: 40, ReplaceRoleID: "role-conditional",
					}}, nil
				},
			}, nil
		},
	}
	if os.Getenv("MK_PLUGINTEST_FORGED_REPLACEMENT_CHILD") == "1" {
		registration := plugintest.New(t).EffectivePolicies(definition)
		_, _ = registration.Resolve(context.Background(), plugin.EffectivePolicyRequest{
			ActiveAssignments: []plugin.ActiveRoleAssignment{{RoleID: "role-a", AssignmentID: "assign-1"}},
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEffectivePoliciesRejectsReplacementOfAForgedRoleID$")
	cmd.Env = append(os.Environ(), "MK_PLUGINTEST_FORGED_REPLACEMENT_CHILD=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "a replacement of a role the resolver forged must fail the harness test process")
	assert.Contains(t, string(output), "EffectivePolicies の出力が不正です")
}

// **逆に、resolver が request を書き換えても本来の対象は置換できる。** 上の test と対で
// 「呼び出し前に確定した集合で判定している」ことの防線になる。ここまで通らないと
// 全置換を弾くような過大な修正に落ちる。
func TestEffectivePoliciesAcceptsReplacementAfterTheRequestWasMutated(t *testing.T) {
	definition := plugin.Definition{
		Name: "policy", APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"mentionLimit"},
				Resolve: func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					req.ActiveAssignments[0].RoleID = "role-conditional"
					return []plugin.EffectivePolicyContribution{{
						Key: "mentionLimit", Value: 40, ReplaceRoleID: "role-a",
					}}, nil
				},
			}, nil
		},
	}
	registration := plugintest.New(t).EffectivePolicies(definition)
	contributions, err := registration.Resolve(context.Background(), plugin.EffectivePolicyRequest{
		ActiveAssignments: []plugin.ActiveRoleAssignment{{RoleID: "role-a", AssignmentID: "assign-1"}},
	})
	require.NoError(t, err, "resolver の request 書き換え自体は不正ではない")
	require.Len(t, contributions, 1)
	assert.Equal(t, "role-a", contributions[0].ReplaceRoleID, "呼び出し前に active だった role は置換できる")
}
