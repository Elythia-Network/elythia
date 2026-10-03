package role_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/shiroha-a/mk/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// registerProvider wires a provider into svc, asserting success.
func registerProvider(t *testing.T, svc *role.Service, name string, keys []string, resolve plugin.EffectivePolicyResolver) {
	t.Helper()
	require.NoError(t, svc.RegisterEffectivePolicyProvider(name, plugin.EffectivePolicyRegistration{Keys: keys, Resolve: resolve}))
}

func assign(t *testing.T, assignRepo *testutil.MockRoleAssignmentRepository, userID, roleID string) {
	t.Helper()
	assignRepo.Assignments[userID+":"+roleID] = &model.RoleAssignment{ID: "a_" + userID + "_" + roleID, UserID: userID, RoleID: roleID}
}

type countingStringer struct{ calls *int }

func (s countingStringer) String() string {
	*s.calls++
	return "ignored"
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

// --- registration validation (load-bearing: Validate must be invoked) ---

func TestRegisterEffectivePolicyProvider_InvokesValidate(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	// Resolve nil => reg.Validate() fails => registration must be rejected.
	err := svc.RegisterEffectivePolicyProvider("bad", plugin.EffectivePolicyRegistration{Keys: []string{"canSearchNotes"}})
	require.Error(t, err, "nil Resolve must be rejected via Validate")
	// registration must NOT be stored
	p, e := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, e)
	assert.Equal(t, false, p["canSearchNotes"], "no provider applied when registration rejected (native default false)")
}

func TestRegisterEffectivePolicyProvider_RejectsEmptyName(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	err := svc.RegisterEffectivePolicyProvider("", plugin.EffectivePolicyRegistration{Keys: []string{"canSearchNotes"}, Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return nil, nil
	}})
	require.Error(t, err, "empty name must be rejected")
}

func TestRegisterEffectivePolicyProvider_RejectsUndeclaredKeyAgainstDefaults(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	reg := plugin.EffectivePolicyRegistration{Keys: []string{"noSuchPolicyKey"}, Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return nil, nil
	}}
	err := svc.RegisterEffectivePolicyProvider("p", reg)
	require.Error(t, err, "key not present in DefaultPolicies must be rejected")
}

func TestRegisterEffectivePolicyProvider_RejectsDuplicateName(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	first := plugin.EffectivePolicyRegistration{Keys: []string{"canSearchNotes"}, Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
	}}
	second := plugin.EffectivePolicyRegistration{Keys: []string{"canSearchNotes"}, Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: false}}, nil
	}}
	require.NoError(t, svc.RegisterEffectivePolicyProvider("same", first))
	require.Error(t, svc.RegisterEffectivePolicyProvider("same", second))
	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, true, policies["canSearchNotes"])
}

// docs/plugins/authoring.md の公開例と同じ Definition が、Definition.Validate、
// factory、production registration、resolution の全経路を通ること。
func TestEffectivePolicy_AuthoringExamplePassesProductionValidation(t *testing.T) {
	def := plugin.Definition{
		Name:       "policy-example",
		APIVersion: plugin.APIVersion,
		EffectivePolicies: func(plugin.Context, plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"driveCapacityMb"},
				Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					return []plugin.EffectivePolicyContribution{
						{Key: "driveCapacityMb", Priority: 1, Value: 1000},
					}, nil
				},
			}, nil
		},
	}

	require.NoError(t, def.Validate())
	reg, err := def.EffectivePolicies(nil, nil)
	require.NoError(t, err)
	svc, _, _, _ := newTestService(t)
	require.NoError(t, svc.RegisterEffectivePolicyProvider(def.Name, reg))
	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 1000, policies["driveCapacityMb"])
}

// --- defensive copies / immutability ---

func TestRegisterEffectivePolicyProvider_CopiesKeysSlice(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	keys := []string{"canSearchNotes"}
	reg := plugin.EffectivePolicyRegistration{Keys: keys, Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
	}}
	require.NoError(t, svc.RegisterEffectivePolicyProvider("p", reg))
	keys[0] = "canInvite" // mutate the caller's slice after registration

	p, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, true, p["canSearchNotes"], "stored registration must not alias the caller's Keys slice")
	assert.Equal(t, false, p["canInvite"], "mutated key must not be honored")
}

func TestEffectivePolicy_RoleIDsSortedAndCloned(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A"}
	assign(t, assignRepo, "u1", "r2")
	assign(t, assignRepo, "u1", "r1")

	var seen []string
	mutated := false
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		seen = append([]string(nil), req.RoleIDs...)
		// mutate the slice the host passed in; must not leak anywhere
		for i := range req.RoleIDs {
			req.RoleIDs[i] = "hacked"
		}
		mutated = true
		return nil, nil
	})

	_, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.True(t, mutated, "provider must have been invoked")
	assert.Equal(t, []string{"r1", "r2"}, seen, "RoleIDs must be sorted")
}

func TestEffectivePolicy_RoleIDsIsolatedBetweenProviders(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A"}
	assign(t, assignRepo, "u1", "r2")
	assign(t, assignRepo, "u1", "r1")

	registerProvider(t, svc, "alpha", []string{"canSearchNotes"}, func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		for i := range req.RoleIDs {
			req.RoleIDs[i] = "mutated"
		}
		return nil, nil
	})
	var seen []string
	registerProvider(t, svc, "beta", []string{"canInvite"}, func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		seen = append([]string(nil), req.RoleIDs...)
		return nil, nil
	})

	_, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, []string{"r1", "r2"}, seen, "each provider must receive an isolated sorted RoleIDs slice")
}

// **provider ごとに request の ActiveAssignments も防御的コピーである。** 1 回の解決で
// host は 1 本の assignments を作って全 provider goroutine へ渡すので、コピーを忘れると
// 片方の書き換えがもう片方に漏れる (RoleIDs を渡しているのと同じ理屈で守っている)。
//
// **観測順を channel で厳密に固定する。** 2 provider の実行順は不定なので、
// `beta が自分の slice を捕まえる` → `alpha が自分の slice を書き換える` の順に
// なります。beta は **複製しない** — slice header だけ外へ持ち出すので、array を
// 共有していると alpha の書き換えがそのまま観測値に現れる。
func TestEffectivePolicy_ActiveAssignmentsIsolatedBetweenProviders(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	assign(t, assignRepo, "u1", "r1")

	// betaCaptured: beta が自分の request slice を捕まえた。
	// alphaMutated: alpha が自分の request slice を書き換えた。
	betaCaptured := make(chan struct{})
	alphaMutated := make(chan struct{})
	var alphaBefore []plugin.ActiveRoleAssignment
	var betaRequest []plugin.ActiveRoleAssignment

	registerProvider(t, svc, "alpha", []string{"canSearchNotes"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			alphaBefore = append([]plugin.ActiveRoleAssignment(nil), req.ActiveAssignments...)
			// **ctx での打ち切りは無い。** beta は自分の resolver で何も待たないので
			// 必ず捕まえ終わる。ここで host の timeout へ逃がすと「alpha が書き換える前に
			// assertion が走る」窓が残り、証明が空振りする。
			<-betaCaptured
			for i := range req.ActiveAssignments {
				req.ActiveAssignments[i] = plugin.ActiveRoleAssignment{RoleID: "hacked", AssignmentID: "hacked"}
			}
			close(alphaMutated)
			return nil, nil
		})
	registerProvider(t, svc, "beta", []string{"canInvite"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			// **複製しない。** slice header だけを外へ持ち出す。array を共有していると
			// alpha の書き換えがそのまま観測値に現れる。
			betaRequest = req.ActiveAssignments
			close(betaCaptured)
			return nil, nil
		})

	_, err := svc.GetUserPoliciesChecked("u1")
	// **error なしで戻った = alpha の resolver が return 済み = 書き換え済み。**
	// providersWG が全 provider を join するので、ここより後で alpha の resolver が
	// 走ることはない。落ちているならそれ以上待たずに落とす (不正な host に対する
	// test timeout を避ける)。
	require.NoError(t, err)
	// **二重の保証。** 上の happens-before に加えて、alpha の書き換えが終わるまで
	// assertion へ進まない。先に走ると beta の観測が空振りする。
	<-alphaMutated

	want := []plugin.ActiveRoleAssignment{{RoleID: "r1", AssignmentID: "a_u1_r1"}}
	assert.Equal(t, want, alphaBefore, "host must hand every provider the same assignments")
	assert.Equal(t, want, betaRequest,
		"alpha rewriting its own slice must not be observable in beta's request")
}

func TestEffectivePolicy_ProviderOnlyGetsActiveRoles(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A"}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "Expired"}
	assign(t, assignRepo, "u1", "r1")
	// expired assignment -> not active
	expired := time.Now().Add(-time.Hour)
	assignRepo.Assignments["u1:r2"] = &model.RoleAssignment{ID: "a2", UserID: "u1", RoleID: "r2", ExpiresAt: &expired}

	var seen []string
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		seen = req.RoleIDs
		return nil, nil
	})
	_, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, []string{"r1"}, seen, "only currently active role IDs are passed")
}

// --- provider contribution semantics ---

func TestEffectivePolicy_ProviderContributionHonored(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
	})
	p, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, true, p["canSearchNotes"], "default false -> provider granted true")
}

func TestEffectivePolicy_UseDefaultFallsBackToNative(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, UseDefault: true, Value: true}}, nil
	})
	p, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, false, p["canSearchNotes"], "UseDefault uses native default (false), not the Value")
}

func TestEffectivePolicy_UseDefaultDoesNotInspectIgnoredValue(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	formatted := 0
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: "canSearchNotes", Priority: 2, UseDefault: true, Value: countingStringer{calls: &formatted}, Order: 1},
			{Key: "canSearchNotes", Priority: 2, UseDefault: true, Value: countingStringer{calls: &formatted}, Order: 0},
		}, nil
	})

	var policies map[string]any
	var err error
	require.NotPanics(t, func() {
		policies, err = svc.GetUserPoliciesChecked("u1")
	})
	require.NoError(t, err)
	assert.Equal(t, false, policies["canSearchNotes"])
	assert.Zero(t, formatted, "UseDefault value must remain completely unobserved")
}

func TestEffectivePolicy_NativeProviderPriorityAggregation(t *testing.T) {
	t.Run("role priority 2 beats provider priority 1", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":2,"value":true}}`))}
		assign(t, assignRepo, "u1", "r1")
		registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 1, Value: false}}, nil
		})
		p, err := svc.GetUserPoliciesChecked("u1")
		require.NoError(t, err)
		assert.Equal(t, true, p["canSearchNotes"], "role priority 2 group dominates provider priority 1")
	})

	t.Run("provider priority 2 beats role priority 1", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":1,"value":false}}`))}
		assign(t, assignRepo, "u1", "r1")
		registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
		})
		p, err := svc.GetUserPoliciesChecked("u1")
		require.NoError(t, err)
		assert.Equal(t, true, p["canSearchNotes"], "provider priority 2 group dominates role priority 1")
	})

	t.Run("same priority aggregates with role", func(t *testing.T) {
		svc, roleRepo, assignRepo, _ := newTestService(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Policies: datatypes.JSON([]byte(`{"canPublicNote":{"useDefault":false,"priority":2,"value":true}}`))}
		assign(t, assignRepo, "u1", "r1")
		registerProvider(t, svc, "p", []string{"canPublicNote"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{Key: "canPublicNote", Priority: 2, Value: false}}, nil
		})
		p, err := svc.GetUserPoliciesChecked("u1")
		require.NoError(t, err)
		assert.Equal(t, true, p["canPublicNote"], "bool OR across same-priority role+provider")
	})
}

func TestEffectivePolicy_MultipleContributionsPerProviderAndDeterminism(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"canSearchNotes", "canInvite"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		// returned out of Order; host must process deterministically
		return []plugin.EffectivePolicyContribution{
			{Key: "canInvite", Priority: 2, Value: true, Order: 1},
			{Key: "canSearchNotes", Priority: 2, Value: true, Order: 0},
		}, nil
	})
	first, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	second, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, true, first["canSearchNotes"])
	assert.Equal(t, true, first["canInvite"])
	assert.Equal(t, first, second, "resolution must be deterministic across calls")
}

func TestEffectivePolicy_IntegerMaxPreservesHostBoundary(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "driveCapacityMb", Priority: 2, Value: int(math.MaxInt)}}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, int(math.MaxInt), policies["driveCapacityMb"])
}

func TestEffectivePolicy_FractionalIntNativePolicyIsPreserved(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "driveCapacityMb", Priority: 2, Value: 0.5}}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 0.5, policies["driveCapacityMb"])
}

func TestEffectivePolicy_DefaultPolicyNativeTypesAreCovered(t *testing.T) {
	for key, value := range role.DefaultPolicies() {
		switch value.(type) {
		case bool, int, string, []string:
		default:
			t.Errorf("default policy %q has unsupported native type %T", key, value)
		}
	}
}

func TestEffectivePolicy_Int64BoundaryIsHostIntSized(t *testing.T) {
	maxInt64 := int64(math.MaxInt64)
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "driveCapacityMb", Priority: 2, Value: maxInt64}}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	if strconv.IntSize == 32 {
		require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
		assert.Equal(t, role.DefaultPolicies()["driveCapacityMb"], policies["driveCapacityMb"])
		return
	}
	require.NoError(t, err)
	assert.Equal(t, int(maxInt64), policies["driveCapacityMb"])
}

func TestEffectivePolicy_IntegralFloatHostIntBoundary(t *testing.T) {
	inside := float64(math.MaxInt)
	if strconv.IntSize == 64 {
		// float64(MaxInt) rounds to 2^63. Its predecessor is the largest
		// representable integral float64 that converts safely to host int.
		inside = math.Nextafter(inside, 0)
	}

	t.Run("largest safely convertible", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		registerProvider(t, svc, "p", []string{"driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{Key: "driveCapacityMb", Priority: 2, Value: inside}}, nil
		})

		policies, err := svc.GetUserPoliciesChecked("u1")
		require.NoError(t, err)
		assert.Equal(t, int(inside), policies["driveCapacityMb"])
	})

	t.Run("first integral value outside host int", func(t *testing.T) {
		outside := float64(math.MaxInt) + 1
		svc, _, _, _ := newTestService(t)
		registerProvider(t, svc, "p", []string{"driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{Key: "driveCapacityMb", Priority: 2, Value: outside}}, nil
		})

		policies, err := svc.GetUserPoliciesChecked("u1")
		require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
		assert.Equal(t, role.DefaultPolicies()["driveCapacityMb"], policies["driveCapacityMb"])
	})
}

func TestEffectivePolicy_IntegerMaxAboveFloatPrecisionIsExact(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("host int cannot represent values above float64's exact integer range")
	}
	exact := int64(1<<53 + 1)
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "driveCapacityMb", Priority: 2, Value: exact}}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, int(exact), policies["driveCapacityMb"])
}

func TestEffectivePolicy_MultipleIntegerContributionsUseMaxWithoutCumulativeArithmetic(t *testing.T) {
	tests := []struct {
		name  string
		other int
	}{
		{name: "inputs whose sum would overflow", other: 1},
		{name: "inputs whose product would overflow", other: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, _ := newTestService(t)
			registerProvider(t, svc, "alpha", []string{"driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{{Key: "driveCapacityMb", Priority: 2, Value: int(math.MaxInt)}}, nil
			})
			registerProvider(t, svc, "beta", []string{"driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{{Key: "driveCapacityMb", Priority: 2, Value: tt.other}}, nil
			})

			policies, err := svc.GetUserPoliciesChecked("u1")
			require.NoError(t, err)
			assert.Equal(t, int(math.MaxInt), policies["driveCapacityMb"], "this contract max-aggregates and performs no cumulative arithmetic")
		})
	}
}

func TestEffectivePolicy_NegativeIntegerSentinelsPreserveNativeSemantics(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value any
	}{
		{name: "user list member limit typed int", key: "userEachUserListsLimit", value: int(-1)},
		{name: "webhook limit typed int64", key: "webhookLimit", value: int64(-1)},
		{name: "antenna limit integral float64", key: "antennaLimit", value: float64(-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, _ := newTestService(t)
			registerProvider(t, svc, "p", []string{tt.key}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{{Key: tt.key, Priority: 2, Value: tt.value}}, nil
			})

			policies, err := svc.GetUserPoliciesChecked("u1")
			require.NoError(t, err)
			assert.Equal(t, -1, policies[tt.key])
		})
	}
}

func TestEffectivePolicy_NegativeIntegerMaxWithinSelectedPriority(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "alpha", []string{"userEachUserListsLimit"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "userEachUserListsLimit", Priority: 2, Value: -2}}, nil
	})
	registerProvider(t, svc, "beta", []string{"userEachUserListsLimit"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "userEachUserListsLimit", Priority: 2, Value: -1}}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, -1, policies["userEachUserListsLimit"], "priority 2 excludes the positive native default, then Max selects -1")
}

func TestEffectivePolicy_MalformedContributionFailsProviderClosed(t *testing.T) {
	tests := []struct {
		name      string
		keys      []string
		malformed plugin.EffectivePolicyContribution
		nativeKey string
	}{
		{name: "undeclared key", keys: []string{"canSearchNotes", "canSearchUsers"}, malformed: plugin.EffectivePolicyContribution{Key: "canInvite", Priority: 2, Value: true}, nativeKey: "canSearchUsers"},
		{name: "invalid priority", keys: []string{"canSearchNotes", "canSearchUsers"}, malformed: plugin.EffectivePolicyContribution{Key: "canSearchUsers", Priority: 3, Value: true}, nativeKey: "canSearchUsers"},
		{name: "wrong type", keys: []string{"canSearchNotes", "canSearchUsers"}, malformed: plugin.EffectivePolicyContribution{Key: "canSearchUsers", Priority: 2, Value: 1}, nativeKey: "canSearchUsers"},
		{name: "invalid availability", keys: []string{"canSearchNotes", "chatAvailability"}, malformed: plugin.EffectivePolicyContribution{Key: "chatAvailability", Priority: 2, Value: "sometimes"}, nativeKey: "chatAvailability"},
		{name: "NaN number", keys: []string{"canSearchNotes", "driveCapacityMb"}, malformed: plugin.EffectivePolicyContribution{Key: "driveCapacityMb", Priority: 2, Value: math.NaN()}, nativeKey: "driveCapacityMb"},
		{name: "positive infinity number", keys: []string{"canSearchNotes", "driveCapacityMb"}, malformed: plugin.EffectivePolicyContribution{Key: "driveCapacityMb", Priority: 2, Value: math.Inf(1)}, nativeKey: "driveCapacityMb"},
		{name: "negative infinity number", keys: []string{"canSearchNotes", "driveCapacityMb"}, malformed: plugin.EffectivePolicyContribution{Key: "driveCapacityMb", Priority: 2, Value: math.Inf(-1)}, nativeKey: "driveCapacityMb"},
		{name: "malformed string array", keys: []string{"canSearchNotes", "uploadableFileTypes"}, malformed: plugin.EffectivePolicyContribution{Key: "uploadableFileTypes", Priority: 2, Value: []any{"image/png", 7}}, nativeKey: "uploadableFileTypes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, _ := newTestService(t)
			registerProvider(t, svc, "p", tt.keys, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{
					{Key: "canSearchNotes", Priority: 2, Value: true, Order: 0},
					tt.malformed,
				}, nil
			})

			p, err := svc.GetUserPoliciesChecked("u1")
			require.Equal(t, role.ErrEffectivePolicyProvider, err, "checked resolution must return only the fixed sentinel")
			assert.Equal(t, false, p["canSearchNotes"], "one malformed contribution must discard valid output and restore every declared key")
			assert.Equal(t, role.DefaultPolicies()[tt.nativeKey], p[tt.nativeKey])
		})
	}
}

func TestEffectivePolicy_DuplicateKeyOrderFailsProviderClosed(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"canSearchNotes", "canSearchUsers"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: "canSearchNotes", Priority: 2, Value: true, Order: 4},
			{Key: "canSearchNotes", Priority: 1, Value: false, Order: 4},
		}, nil
	})

	p, err := svc.GetUserPoliciesChecked("u1")
	require.Equal(t, role.ErrEffectivePolicyProvider, err)
	assert.Equal(t, false, p["canSearchNotes"])
	assert.Equal(t, role.DefaultPolicies()["canSearchUsers"], p["canSearchUsers"], "duplicate ties must restore every declared key")
}

// --- provider failure: native-only keys + checked sentinel ---

func TestEffectivePolicy_ProviderErrorCheckedRestoresDeclaredKeys(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"canSearchUsers", "driveCapacityMb", "chatAvailability", "uploadableFileTypes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return nil, errors.New("provider exploded")
	})
	p, err := svc.GetUserPoliciesChecked("u1")
	require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	for _, key := range []string{"canSearchUsers", "driveCapacityMb", "chatAvailability", "uploadableFileTypes"} {
		assert.Equal(t, role.DefaultPolicies()[key], p[key])
	}
}

type failingPolicyAssignmentRepo struct {
	*testutil.MockRoleAssignmentRepository
	err error
}

func (r *failingPolicyAssignmentRepo) ListByUser(string) ([]*model.RoleAssignment, error) {
	return nil, r.err
}

func TestEffectivePolicy_RoleLookupErrorSkipsProvidersAndRemainsDistinct(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &failingPolicyAssignmentRepo{
		MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo),
		err:                          errors.New("role lookup failed"),
	}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	var providerCalls atomic.Int32
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		providerCalls.Add(1)
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.Error(t, err)
	assert.NotErrorIs(t, err, role.ErrEffectivePolicyProvider)
	assert.ErrorContains(t, err, "role lookup failed")
	assert.False(t, policies["canSearchNotes"].(bool))
	assert.Zero(t, providerCalls.Load())
}

func TestEffectivePolicy_MetaBaseFailureIsCheckedButUncheckedStillCallsProviders(t *testing.T) {
	for _, tt := range []struct {
		name      string
		breakMeta func(*testutil.MockMetaRepository)
	}{
		{"fetch", func(repo *testutil.MockMetaRepository) { repo.FetchErr = errors.New("meta unavailable") }},
		{"malformed json", func(repo *testutil.MockMetaRepository) {
			repo.Meta = &model.Meta{ID: "x", Policies: datatypes.JSON([]byte(`{"canDeleteAccount":`))}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, roleRepo, assignRepo, metaRepo := newTestService(t)
			metaRepo.Meta.Policies = datatypes.JSON([]byte(`{"canDeleteAccount":false}`))
			tt.breakMeta(metaRepo)
			var providerCalls atomic.Int32
			registerProvider(t, svc, "account", []string{role.PolicyCanDeleteAccount}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				providerCalls.Add(1)
				return []plugin.EffectivePolicyContribution{{Key: role.PolicyCanDeleteAccount, Value: false}}, nil
			})

			policies, err := svc.GetUserPoliciesChecked("u1")
			require.Error(t, err)
			assert.ErrorContains(t, err, "role: effective policy base")
			assert.Equal(t, true, policies[role.PolicyCanDeleteAccount])
			assert.Zero(t, providerCalls.Load())

			roleRepo.Roles["deny"] = &model.Role{
				ID:       "deny",
				Policies: datatypes.JSON([]byte(`{"canDeleteAccount":{"useDefault":false,"priority":2,"value":false}}`)),
			}
			assign(t, assignRepo, "u1", "deny")
			svc.InvalidateUserRoleCache("u1")
			assert.Equal(t, false, svc.GetUserPolicies("u1")[role.PolicyCanDeleteAccount], "unchecked fallback keeps native role overrides")
			assert.Equal(t, int32(1), providerCalls.Load(), "unchecked consumers keep the existing provider path")
		})
	}
}

func TestEffectivePolicy_CheckedForKeysIgnoresUnrelatedProviderFailures(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	var unrelatedCalls atomic.Int32
	registerProvider(t, svc, "unrelated", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		unrelatedCalls.Add(1)
		return nil, errors.New("unrelated provider failed")
	})
	var accountCalls atomic.Int32
	registerProvider(t, svc, "account", []string{role.PolicyCanDeleteAccount, role.PolicyCanPurgeAccount}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		accountCalls.Add(1)
		return []plugin.EffectivePolicyContribution{
			{Key: role.PolicyCanDeleteAccount, Value: true},
			{Key: role.PolicyCanPurgeAccount, Value: false},
		}, nil
	})

	policies, err := svc.GetUserPoliciesCheckedForKeys("u1", role.PolicyCanDeleteAccount, role.PolicyCanPurgeAccount)
	require.NoError(t, err)
	assert.Equal(t, true, policies[role.PolicyCanDeleteAccount])
	assert.Equal(t, false, policies[role.PolicyCanPurgeAccount])
	assert.Zero(t, unrelatedCalls.Load())
	assert.Equal(t, int32(1), accountCalls.Load())
}

func TestEffectivePolicy_CheckedForKeysReportsRelevantProviderFailure(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "account", []string{role.PolicyCanDeleteAccount}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return nil, errors.New("account provider failed")
	})

	policies, err := svc.GetUserPoliciesCheckedForKeys("u1", role.PolicyCanDeleteAccount, role.PolicyCanPurgeAccount)
	require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	assert.Equal(t, true, policies[role.PolicyCanDeleteAccount], "failed provider restores the native value")
}

func TestEffectivePolicy_ProviderPanicCheckedRestoresDeclaredKeys(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"canSearchUsers", "driveCapacityMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		panic("secret provider panic")
	})

	var p map[string]any
	var err error
	require.NotPanics(t, func() {
		p, err = svc.GetUserPoliciesChecked("u1")
	})
	require.Equal(t, role.ErrEffectivePolicyProvider, err)
	assert.Equal(t, role.DefaultPolicies()["canSearchUsers"], p["canSearchUsers"])
	assert.Equal(t, role.DefaultPolicies()["driveCapacityMb"], p["driveCapacityMb"])
}

func TestEffectivePolicy_CooperativeTimeoutFallsBackToNative(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	var calls atomic.Int32
	registerProvider(t, svc, "slow", []string{"canSearchNotes"}, func(ctx context.Context, _ plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	started := time.Now()
	policies, err := svc.GetUserPoliciesChecked("")
	require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.Equal(t, false, policies["canSearchNotes"])

	started = time.Now()
	_, err = svc.GetUserPoliciesChecked("")
	assert.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	assert.Less(t, time.Since(started), 250*time.Millisecond)
	assert.Equal(t, int32(1), calls.Load())
}

func TestEffectivePolicy_TimeoutDisablesProviderAndBoundsHang(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	var calls atomic.Int32
	registerProvider(t, svc, "hung", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls.Add(1)
		<-block
		return nil, nil
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			policies, err := svc.GetUserPoliciesChecked("u1")
			assert.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
			assert.Equal(t, false, policies["canSearchNotes"])
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("policy resolution did not time out")
	}
	assert.Equal(t, int32(1), calls.Load())

	started := time.Now()
	_, err := svc.GetUserPoliciesChecked("u2")
	assert.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	assert.Less(t, time.Since(started), 250*time.Millisecond)
	assert.Equal(t, int32(1), calls.Load())
}

func TestEffectivePolicy_ConcurrentRequestsShareSuccessfulProviderMiss(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	var calls atomic.Int32
	registerProvider(t, svc, "healthy", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond)
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	var failures atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.GetUserPoliciesChecked("")
			if err != nil {
				failures.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	assert.Zero(t, failures.Load())
	assert.Equal(t, int32(1), calls.Load())

	policies, err := svc.GetUserPoliciesChecked("")
	require.NoError(t, err)
	assert.Equal(t, true, policies["canSearchNotes"])
	assert.Equal(t, int32(1), calls.Load(), "the successful anonymous result remains cached")
}

func TestEffectivePolicy_ConcurrentProvidersShareOneTimeoutWindow(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		registerProvider(t, svc, name, []string{"canSearchNotes"}, func(ctx context.Context, _ plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}

	started := time.Now()
	policies, err := svc.GetUserPoliciesChecked("")
	elapsed := time.Since(started)
	require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	assert.False(t, policies["canSearchNotes"].(bool))
	assert.Less(t, elapsed, 2*time.Second, "independent providers must not add their timeout windows serially")
}

func TestEffectivePolicy_TimeoutDisableWarnsOnceWithoutProviderData(t *testing.T) {
	var logs lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "secret-provider-name", []string{"canSearchNotes"}, func(ctx context.Context, _ plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		<-ctx.Done()
		return nil, errors.New("secret resolver detail")
	})

	_, err := svc.GetUserPoliciesChecked("")
	require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	_, err = svc.GetUserPoliciesChecked("u2")
	require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	output := logs.String()
	// **失敗したら logs を丸ごと出す。** 期待値と実際値だけだと 0 件なのか
	// 2 件なのかも混入元も分からず、#2867 の調査で実際に詰まった。
	assert.Equalf(t, 1, strings.Count(output, "effective policy provider disabled after timeout"),
		"logs:\n%s", output)
	assert.NotContains(t, output, "secret-provider-name")
	assert.NotContains(t, output, "secret resolver detail")
	assert.NotContains(t, output, "canSearchNotes")
}

func TestEffectivePolicy_UncheckedFallbackWarningsAreCountedAndRateLimited(t *testing.T) {
	var logs lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "secret-provider-name", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return nil, errors.New("secret resolver detail")
	})

	for i := range 5 {
		policies := svc.GetUserPolicies(fmt.Sprintf("secret-user-%d", i))
		assert.False(t, policies["canSearchNotes"].(bool))
	}

	output := logs.String()
	assert.Equalf(t, 3, strings.Count(output, "effective policy provider fallback"),
		"counts 1, 2, and 4 are reported instead of logging every request\nlogs:\n%s", output)
	assert.Contains(t, output, "failures=4")
	assert.NotContains(t, output, "secret-provider-name")
	assert.NotContains(t, output, "secret resolver detail")
	assert.NotContains(t, output, "canSearchNotes")
	assert.NotContains(t, output, "secret-user")
}

func TestEffectivePolicy_ResolverGetsFreshDeadlineAfterTokenWait(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	registerProvider(t, svc, "queued", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return nil, nil
		}
		time.Sleep(400 * time.Millisecond)
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
	})

	first := make(chan error, 1)
	go func() {
		_, err := svc.GetUserPoliciesChecked("u1")
		first <- err
	}()
	<-started
	time.AfterFunc(800*time.Millisecond, func() { close(release) })

	policies, err := svc.GetUserPoliciesChecked("u2")
	require.NoError(t, <-first)
	require.NoError(t, err)
	assert.Equal(t, true, policies["canSearchNotes"])
}

func TestEffectivePolicy_ProviderErrorUncheckedRestoresOnlyDeclared(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return nil, errors.New("provider exploded")
	})
	// unchecked form returns a safe map only, failed keys native, unrelated keys untouched
	p := svc.GetUserPolicies("u1")
	assert.Equal(t, false, p["canSearchNotes"], "declared key restored to native")
	assert.Equal(t, true, p["canSearchUsers"], "undeclared key untouched (default true)")
	assert.Equal(t, 100, p["driveCapacityMb"], "undeclared numeric key untouched")
}

func TestEffectivePolicy_FailedProviderKeysUseExactNativePolicy(t *testing.T) {
	tests := []struct {
		name    string
		resolve plugin.EffectivePolicyResolver
	}{
		{
			name: "resolver error",
			resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return nil, errors.New("secret provider error")
			},
		},
		{
			name: "resolver panic",
			resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				panic("secret provider panic")
			},
		},
		{
			name: "malformed output",
			resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{{Key: "maxFileSizeMb", Priority: 2, Value: "not a number"}}, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, roleRepo, assignRepo, metaRepo := newTestService(t)
			svc.SetServerMaxFileSizeMb(100)
			metaRepo.Meta = &model.Meta{
				ID:                               "x",
				ChunkedUploadMaxSessionsPerUser:  50,
				ChunkedUploadMaxPendingMbPerUser: 500,
			}
			roleRepo.Roles["restrictive"] = &model.Role{
				ID: "restrictive",
				Policies: datatypes.JSON([]byte(`{
					"uploadableFileTypes":{"useDefault":false,"priority":1,"value":["image/png"]},
					"maxFileSizeMb":{"useDefault":false,"priority":1,"value":7},
					"chunkedUploadMaxConcurrentSessions":{"useDefault":false,"priority":1,"value":2},
					"chunkedUploadMaxPendingMb":{"useDefault":false,"priority":1,"value":11}
				}`)),
			}
			assign(t, assignRepo, "u1", "restrictive")

			affected := []string{
				"uploadableFileTypes",
				"maxFileSizeMb",
				role.PolicyChunkedUploadMaxConcurrentSessions,
				role.PolicyChunkedUploadMaxPendingMb,
			}
			allSuccessfulKeys := append(append([]string(nil), affected...), "canSearchNotes")
			permissive := func(calls *atomic.Int32) plugin.EffectivePolicyResolver {
				return func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					calls.Add(1)
					return []plugin.EffectivePolicyContribution{
						{Key: "uploadableFileTypes", Priority: 2, Value: []string{"*"}},
						{Key: "maxFileSizeMb", Priority: 2, Value: 90},
						{Key: role.PolicyChunkedUploadMaxConcurrentSessions, Priority: 2, Value: 40},
						{Key: role.PolicyChunkedUploadMaxPendingMb, Priority: 2, Value: 400},
						{Key: "canSearchNotes", Priority: 2, Value: true},
					}, nil
				}
			}

			var alphaCalls atomic.Int32
			var bravoCalls atomic.Int32
			var zuluCalls atomic.Int32
			registerProvider(t, svc, "zulu", allSuccessfulKeys, permissive(&zuluCalls))
			registerProvider(t, svc, "bravo", affected, func(ctx context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				bravoCalls.Add(1)
				return tt.resolve(ctx, req)
			})
			registerProvider(t, svc, "alpha", allSuccessfulKeys, permissive(&alphaCalls))

			assertSafe := func(policies map[string]any) {
				t.Helper()
				assert.Equal(t, []string{"image/png"}, policies["uploadableFileTypes"], "empty means allow-all to the upload consumer")
				assert.Equal(t, 7, policies["maxFileSizeMb"], "zero disables the upload-size gate and the final cap would grant 100")
				assert.Equal(t, 2, policies[role.PolicyChunkedUploadMaxConcurrentSessions], "zero disables the session gate and the final cap would grant 50")
				assert.Equal(t, 11, policies[role.PolicyChunkedUploadMaxPendingMb], "zero disables the pending-size gate and the final cap would grant 500")
				assert.Equal(t, true, policies["canSearchNotes"], "unaffected successful keys remain applied")
			}

			checked, err := svc.GetUserPoliciesChecked("u1")
			require.Equal(t, role.ErrEffectivePolicyProvider, err)
			assertSafe(checked)
			assert.Equal(t, int32(1), alphaCalls.Load())
			assert.Equal(t, int32(1), bravoCalls.Load())
			assert.Equal(t, int32(1), zuluCalls.Load())

			assertSafe(svc.GetUserPolicies("u1"))
			assert.Equal(t, int32(1), alphaCalls.Load())
			assert.Equal(t, int32(2), bravoCalls.Load(), "failed providers are retried")
			assert.Equal(t, int32(1), zuluCalls.Load())
		})
	}
}

func TestEffectivePolicy_FailedProviderRestoredSliceMutationIsIsolated(t *testing.T) {
	want := []string{"text/*", "application/json", "image/*", "video/*", "audio/*"}
	shared := role.DefaultPolicies()["uploadableFileTypes"].([]string)
	original := append([]string(nil), shared...)
	defer copy(shared, original)

	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "failing", []string{"uploadableFileTypes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return nil, errors.New("secret provider error")
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.Equal(t, role.ErrEffectivePolicyProvider, err)
	policies["uploadableFileTypes"].([]string)[0] = "application/x-corrupt"

	assert.Equal(t, want, role.DefaultPolicies()["uploadableFileTypes"])
	next, err := svc.GetUserPoliciesChecked("u1")
	require.Equal(t, role.ErrEffectivePolicyProvider, err)
	assert.Equal(t, want, next["uploadableFileTypes"])
	assert.Equal(t, want, role.DefaultPoliciesClone()["uploadableFileTypes"])
}

func TestEffectivePolicy_ProviderSliceDoesNotAliasResolverValue(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	resolverValue := []string{"image/png"}
	var calls atomic.Int32
	registerProvider(t, svc, "provider", []string{"uploadableFileTypes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls.Add(1)
		return []plugin.EffectivePolicyContribution{{Key: "uploadableFileTypes", Priority: 2, Value: resolverValue}}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	resolved := policies["uploadableFileTypes"].([]string)
	resolverValue[0] = "application/x-resolver-corrupt"
	assert.Equal(t, []string{"image/png"}, resolved)

	resolved[0] = "application/x-result-corrupt"
	assert.Equal(t, []string{"application/x-resolver-corrupt"}, resolverValue)
	next, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, []string{"image/png"}, next["uploadableFileTypes"], "cached contributions must not alias resolver or caller slices")
	assert.Equal(t, int32(1), calls.Load())
}

func TestEffectivePolicy_CachesSuccessUntilUserInvalidation(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	calls := 0
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls++
		if calls == 1 {
			return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
		}
		return nil, errors.New("now failing")
	})

	first, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, true, first["canSearchNotes"], "first (successful) call is permissive")

	second, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, true, second["canSearchNotes"])
	assert.Equal(t, 1, calls)

	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))
	third, err := svc.GetUserPoliciesChecked("u1")
	require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	assert.Equal(t, false, third["canSearchNotes"])
	assert.Equal(t, 2, calls)
}

func TestEffectivePolicy_ProviderCacheLimitEvictsLeastRecentlyUsed(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	svc.SetEffectivePolicyProviderCacheEntries(2)
	var callsMu sync.Mutex
	calls := map[string]int{}
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		callsMu.Lock()
		calls[req.UserID]++
		callsMu.Unlock()
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
	})

	for _, userID := range []string{"u1", "u2", "u1", "u3", "u2"} {
		_, err := svc.GetUserPoliciesChecked(userID)
		require.NoError(t, err)
	}

	callsMu.Lock()
	defer callsMu.Unlock()
	assert.Equal(t, map[string]int{"u1": 1, "u2": 2, "u3": 1}, calls)
}

func TestEffectivePolicy_ProviderFailuresAreNotCached(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	var calls atomic.Int32
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls.Add(1)
		return nil, errors.New("test failure")
	})

	for range 2 {
		_, err := svc.GetUserPoliciesChecked("u1")
		require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	}
	assert.Equal(t, int32(2), calls.Load())
}

func TestEffectivePolicy_ConcurrentResolutionAndInvalidationIsRaceFree(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
	})

	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				_, _ = svc.GetUserPoliciesChecked(fmt.Sprintf("u%d-%d", worker, i%5))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 50 {
			_ = svc.InvalidateUser(context.Background(), fmt.Sprintf("u%d-%d", i%4, i%5))
			_ = svc.InvalidateRolePolicies(context.Background(), "r1")
		}
	}()
	wg.Wait()
}

func TestEffectivePolicy_UserInvalidationPreservesOtherUserCache(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	var mu sync.Mutex
	calls := map[string]int{}
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		mu.Lock()
		calls[req.UserID]++
		value := calls[req.UserID] > 1
		mu.Unlock()
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: value}}, nil
	})

	firstU1, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	firstU2, err := svc.GetUserPoliciesChecked("u2")
	require.NoError(t, err)
	assert.False(t, firstU1["canSearchNotes"].(bool))
	assert.False(t, firstU2["canSearchNotes"].(bool))

	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))
	secondU1, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	secondU2, err := svc.GetUserPoliciesChecked("u2")
	require.NoError(t, err)
	assert.True(t, secondU1["canSearchNotes"].(bool))
	assert.False(t, secondU2["canSearchNotes"].(bool))
	mu.Lock()
	assert.Equal(t, map[string]int{"u1": 2, "u2": 1}, calls)
	mu.Unlock()
}

func TestEffectivePolicy_ActiveRoleIDsPartitionProviderCache(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	var calls atomic.Int32
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls.Add(1)
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: len(req.RoleIDs) > 0}}, nil
	})

	withoutRole, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.False(t, withoutRole["canSearchNotes"].(bool))
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	assign(t, assignRepo, "u1", "r1")
	svc.InvalidateUserRoleCache("u1")

	withRole, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.True(t, withRole["canSearchNotes"].(bool))
	assert.Equal(t, int32(2), calls.Load())
}

func TestEffectivePolicy_RoleInvalidationDropsEveryProviderCacheEntry(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	var calls atomic.Int32
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		value := calls.Add(1) > 2
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: value}}, nil
	})

	_, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	_, err = svc.GetUserPoliciesChecked("u2")
	require.NoError(t, err)
	require.NoError(t, svc.InvalidateRolePolicies(context.Background(), "r1"))
	u1, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	u2, err := svc.GetUserPoliciesChecked("u2")
	require.NoError(t, err)
	assert.True(t, u1["canSearchNotes"].(bool))
	assert.True(t, u2["canSearchNotes"].(bool))
	assert.Equal(t, int32(4), calls.Load())
}

func TestEffectivePolicy_UnassignAndReassignCannotResurrectProviderCache(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	var calls atomic.Int32
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		value := calls.Add(1) > 1
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: value}}, nil
	})

	require.NoError(t, svc.Assign("u1", "r1", nil))
	assigned, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.False(t, assigned["canSearchNotes"].(bool))
	require.NoError(t, svc.Unassign("u1", "r1"))
	unassigned, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.True(t, unassigned["canSearchNotes"].(bool))
	require.NoError(t, svc.Assign("u1", "r1", nil))

	reassigned, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.True(t, reassigned["canSearchNotes"].(bool), "reusing the same role IDs must not resurrect pre-unassign provider output")
	assert.Equal(t, int32(3), calls.Load())
}

func TestEffectivePolicy_RoleUpdateInvalidatesSameRoleIDsProviderCache(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "old"}
	assign(t, assignRepo, "u1", "r1")
	var value atomic.Bool
	var calls atomic.Int32
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls.Add(1)
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: value.Load()}}, nil
	})

	before, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.False(t, before["canSearchNotes"].(bool))
	value.Store(true)
	_, err = svc.UpdateFields("r1", map[string]any{"name": "new"})
	require.NoError(t, err)

	after, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.True(t, after["canSearchNotes"].(bool))
	assert.Equal(t, int32(2), calls.Load())
}

func TestEffectivePolicy_UserInvalidationRejectsInFlightCachePublication(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-release
		}
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: call > 1}}, nil
	})

	firstResult := make(chan map[string]any, 1)
	go func() {
		policies, _ := svc.GetUserPoliciesChecked("u1")
		firstResult <- policies
	}()
	<-started
	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))
	close(release)
	assert.False(t, (<-firstResult)["canSearchNotes"].(bool))

	second, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.True(t, second["canSearchNotes"].(bool), "the pre-invalidation result must not be published into the cache")
	assert.Equal(t, int32(2), calls.Load())
}

func TestEffectivePolicy_UserInvalidationDoesNotReturnJoinedStaleFlight(t *testing.T) {
	testEffectivePolicyInvalidationDoesNotReturnJoinedStaleFlight(t, func(svc *role.Service) error {
		return svc.InvalidateUser(context.Background(), "u1")
	})
}

func TestEffectivePolicy_RoleInvalidationDoesNotReturnJoinedStaleFlight(t *testing.T) {
	testEffectivePolicyInvalidationDoesNotReturnJoinedStaleFlight(t, func(svc *role.Service) error {
		return svc.InvalidateRolePolicies(context.Background(), "r1")
	})
}

func testEffectivePolicyInvalidationDoesNotReturnJoinedStaleFlight(t *testing.T, invalidate func(*role.Service) error) {
	t.Helper()
	svc, _, _, _ := newTestService(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-release
		}
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: call > 1}}, nil
	})

	firstResult := make(chan map[string]any, 1)
	go func() {
		policies, _ := svc.GetUserPoliciesChecked("u1")
		firstResult <- policies
	}()
	<-started
	require.NoError(t, invalidate(svc))

	secondEntered := make(chan struct{})
	secondResult := make(chan map[string]any, 1)
	go func() {
		close(secondEntered)
		policies, _ := svc.GetUserPoliciesChecked("u1")
		secondResult <- policies
	}()
	<-secondEntered
	// Give the second request time to observe the existing flight while the
	// first resolver remains blocked. The epoch assertion is deterministic in
	// TestPolicyProviderFlightIsCurrent; this covers the full service path.
	time.Sleep(20 * time.Millisecond)
	close(release)

	assert.False(t, (<-firstResult)["canSearchNotes"].(bool))
	select {
	case second := <-secondResult:
		assert.True(t, second["canSearchNotes"].(bool), "a post-invalidation request must not receive the old flight result")
	case <-time.After(2 * time.Second):
		t.Fatal("post-invalidation request did not complete")
	}
	assert.Equal(t, int32(2), calls.Load())
}

func TestEffectivePolicy_AnonymousInvokesProvidersWithoutRepositoryLookup(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	// repositoryをnilにして、匿名解決がrole、assignment、userを検索すればpanicさせる。
	svc := role.NewService(nil, nil, nil, idGen)

	var requests []plugin.EffectivePolicyRequest
	var requestsMu sync.Mutex
	for _, name := range []string{"bravo", "alpha"} {
		registerProvider(t, svc, name, []string{"canSearchNotes"}, func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			requestsMu.Lock()
			requests = append(requests, req)
			requestsMu.Unlock()
			return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
		})
	}

	policies, err := svc.GetUserPoliciesChecked("")
	require.NoError(t, err)
	assert.Equal(t, true, policies["canSearchNotes"])
	require.Len(t, requests, 2)
	for _, req := range requests {
		assert.Equal(t, "", req.UserID)
		assert.NotNil(t, req.RoleIDs)
		assert.Empty(t, req.RoleIDs)
	}
}

func TestEffectivePolicy_NoProviderAnonymousFastPathAllocations(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	cloneAllocs := testing.AllocsPerRun(100, func() {
		_ = role.DefaultPoliciesClone()
	})
	policyAllocs := testing.AllocsPerRun(100, func() {
		_ = svc.GetUserPolicies("")
	})
	assert.Equal(t, cloneAllocs, policyAllocs)
}

func TestEffectivePolicy_AnonymousUsesNativeBaselineOrderingCloningAndCaps(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	metaRepo.Meta = &model.Meta{
		ID:       "x",
		Policies: datatypes.JSON([]byte(`{"canSearchUsers":false}`)),
	}
	resolverValue := []string{"image/png"}
	registerProvider(t, svc, "zulu", []string{"canSearchNotes", "maxFileSizeMb", "uploadableFileTypes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: "canSearchNotes", Priority: 1, Value: true},
			{Key: "maxFileSizeMb", Priority: 2, Value: 500},
			{Key: "uploadableFileTypes", Priority: 2, Value: resolverValue},
		}, nil
	})
	registerProvider(t, svc, "alpha", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: false}}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("")
	require.NoError(t, err)
	assert.Equal(t, false, policies["canSearchNotes"], "higher-priority anonymous contribution wins")
	assert.Equal(t, false, policies["canSearchUsers"], "meta-overlaid anonymous baseline is preserved")
	assert.Equal(t, 100, policies["maxFileSizeMb"], "instance caps apply after anonymous contributions")
	assert.Equal(t, []string{"image/png"}, policies["uploadableFileTypes"])

	resolverValue[0] = "application/x-resolver-corrupt"
	resolved := policies["uploadableFileTypes"].([]string)
	assert.Equal(t, []string{"image/png"}, resolved)
	resolved[0] = "application/x-result-corrupt"
	next, err := svc.GetUserPoliciesChecked("")
	require.NoError(t, err)
	assert.Equal(t, []string{"image/png"}, next["uploadableFileTypes"], "anonymous provider output remains cached")
}

func TestEffectivePolicy_AnonymousProviderFailuresUseNativeFallback(t *testing.T) {
	tests := []struct {
		name    string
		resolve plugin.EffectivePolicyResolver
	}{
		{name: "error", resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return nil, errors.New("secret provider error")
		}},
		{name: "panic", resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			panic("secret provider panic")
		}},
		{name: "malformed", resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{Key: "uploadableFileTypes", Priority: 2, Value: []any{"image/png", 7}}}, nil
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, _ := newTestService(t)
			registerProvider(t, svc, "successful", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
				return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
			})
			registerProvider(t, svc, "failing", []string{"uploadableFileTypes"}, tt.resolve)

			policies, err := svc.GetUserPoliciesChecked("")
			require.Equal(t, role.ErrEffectivePolicyProvider, err)
			assert.Equal(t, role.DefaultPolicies()["uploadableFileTypes"], policies["uploadableFileTypes"])
			assert.Equal(t, true, policies["canSearchNotes"], "unaffected anonymous contributions remain")

			unchecked := svc.GetUserPolicies("")
			assert.Equal(t, role.DefaultPolicies()["uploadableFileTypes"], unchecked["uploadableFileTypes"])
			assert.Equal(t, true, unchecked["canSearchNotes"])
		})
	}
}

func TestEffectivePolicy_AnonymousCachesSuccessUntilRoleInvalidation(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	calls := 0
	registerProvider(t, svc, "p", []string{"canSearchNotes"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		calls++
		if calls == 1 {
			return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Priority: 2, Value: true}}, nil
		}
		return nil, errors.New("now failing")
	})

	first, err := svc.GetUserPoliciesChecked("")
	require.NoError(t, err)
	assert.Equal(t, true, first["canSearchNotes"])

	second, err := svc.GetUserPoliciesChecked("")
	require.NoError(t, err)
	assert.Equal(t, true, second["canSearchNotes"])
	assert.Equal(t, 1, calls)

	require.NoError(t, svc.InvalidateRolePolicies(context.Background(), "r1"))
	third, err := svc.GetUserPoliciesChecked("")
	require.Equal(t, role.ErrEffectivePolicyProvider, err)
	assert.Equal(t, false, third["canSearchNotes"])
	assert.Equal(t, 2, calls)
}

// --- server caps are applied last ---

func TestEffectivePolicy_ServerCapAppliedAfterProvider(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	registerProvider(t, svc, "p", []string{"maxFileSizeMb"}, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{{Key: "maxFileSizeMb", Priority: 0, Value: 500}}, nil
	})
	p, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 100, p["maxFileSizeMb"], "server cap clamps the provider-raised value last")
}

func TestEffectivePolicy_ChunkedUploadMetaFailureLeavesNativeBaselineWithoutError(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	svc.SetServerMaxFileSizeMb(20)

	checked, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err, "meta cap failure must not leak through the checked provider error contract")
	assert.Equal(t, true, checked[role.PolicyCanUseChunkedUpload])
	assert.Equal(t, 20, checked["maxFileSizeMb"], "the independent server file-size cap still applies")
	assert.Equal(t, 4, checked[role.PolicyChunkedUploadMaxConcurrentSessions], "an unknown cap must not synthesize a numeric limit")
	assert.Equal(t, 1024, checked[role.PolicyChunkedUploadMaxPendingMb])

	unchecked := svc.GetUserPolicies("u1")
	assert.Equal(t, true, unchecked[role.PolicyCanUseChunkedUpload])
	assert.Equal(t, 20, unchecked["maxFileSizeMb"])
}

func TestEffectivePolicy_ChunkedUploadMetaFailureLeavesOrderedProviderValuesUncapped(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	keys := []string{
		role.PolicyCanUseChunkedUpload,
		role.PolicyChunkedUploadMaxConcurrentSessions,
		role.PolicyChunkedUploadMaxPendingMb,
	}
	registerProvider(t, svc, "zulu", keys, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: role.PolicyCanUseChunkedUpload, Priority: 2, Value: true},
			{Key: role.PolicyChunkedUploadMaxConcurrentSessions, Priority: 2, Value: math.MaxInt},
			{Key: role.PolicyChunkedUploadMaxPendingMb, Priority: 2, Value: -1},
		}, nil
	})
	registerProvider(t, svc, "alpha", keys, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: role.PolicyCanUseChunkedUpload, Priority: 0, Value: true},
			{Key: role.PolicyChunkedUploadMaxConcurrentSessions, Priority: 0, Value: 99},
			{Key: role.PolicyChunkedUploadMaxPendingMb, Priority: 0, Value: 99},
		}, nil
	})

	checked, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, true, checked[role.PolicyCanUseChunkedUpload])
	assert.Equal(t, math.MaxInt, checked[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, -1, checked[role.PolicyChunkedUploadMaxPendingMb])

	unchecked := svc.GetUserPolicies("u1")
	assert.Equal(t, true, unchecked[role.PolicyCanUseChunkedUpload])
	assert.Equal(t, math.MaxInt, unchecked[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, -1, unchecked[role.PolicyChunkedUploadMaxPendingMb])
}

func TestEffectivePolicy_ChunkedUploadLoadedMetaCapsProviderValues(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	metaRepo.Meta = &model.Meta{
		ID:                               "x",
		ChunkedUploadEnabled:             true,
		ChunkedUploadMaxSessionsPerUser:  3,
		ChunkedUploadMaxPendingMbPerUser: 96,
	}
	keys := []string{
		role.PolicyCanUseChunkedUpload,
		role.PolicyChunkedUploadMaxConcurrentSessions,
		role.PolicyChunkedUploadMaxPendingMb,
	}
	registerProvider(t, svc, "provider", keys, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: role.PolicyCanUseChunkedUpload, Priority: 2, Value: true},
			{Key: role.PolicyChunkedUploadMaxConcurrentSessions, Priority: 2, Value: math.MaxInt},
			{Key: role.PolicyChunkedUploadMaxPendingMb, Priority: 2, Value: -1},
		}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, true, policies[role.PolicyCanUseChunkedUpload])
	assert.Equal(t, 3, policies[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, 96, policies[role.PolicyChunkedUploadMaxPendingMb])
}

func TestEffectivePolicy_NegativeUnlimitedCannotBypassPositiveInstanceCaps(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	metaRepo.Meta = &model.Meta{
		ID:                               "x",
		ChunkedUploadMaxSessionsPerUser:  2,
		ChunkedUploadMaxPendingMbPerUser: 64,
	}
	keys := []string{"maxFileSizeMb", role.PolicyChunkedUploadMaxConcurrentSessions, role.PolicyChunkedUploadMaxPendingMb}
	registerProvider(t, svc, "p", keys, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: "maxFileSizeMb", Priority: 2, Value: -1},
			{Key: role.PolicyChunkedUploadMaxConcurrentSessions, Priority: 2, Value: -1},
			{Key: role.PolicyChunkedUploadMaxPendingMb, Priority: 2, Value: -1},
		}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 100, policies["maxFileSizeMb"])
	assert.Equal(t, 2, policies[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, 64, policies[role.PolicyChunkedUploadMaxPendingMb])
}

func TestEffectivePolicy_ZeroUnlimitedCannotBypassPositiveInstanceCaps(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	metaRepo.Meta = &model.Meta{
		ID:                               "x",
		ChunkedUploadMaxSessionsPerUser:  2,
		ChunkedUploadMaxPendingMbPerUser: 64,
	}
	keys := []string{"maxFileSizeMb", role.PolicyChunkedUploadMaxConcurrentSessions, role.PolicyChunkedUploadMaxPendingMb}
	registerProvider(t, svc, "p", keys, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: "maxFileSizeMb", Priority: 2, Value: 0},
			{Key: role.PolicyChunkedUploadMaxConcurrentSessions, Priority: 2, Value: 0},
			{Key: role.PolicyChunkedUploadMaxPendingMb, Priority: 2, Value: 0},
		}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 100, policies["maxFileSizeMb"])
	assert.Equal(t, 2, policies[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, 64, policies[role.PolicyChunkedUploadMaxPendingMb])
}

func TestEffectivePolicy_NegativeUnlimitedSurvivesExplicitlyDisabledInstanceCaps(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	svc.SetServerMaxFileSizeMb(0)
	metaRepo.Meta = &model.Meta{ID: "x"}
	keys := []string{"maxFileSizeMb", role.PolicyChunkedUploadMaxConcurrentSessions, role.PolicyChunkedUploadMaxPendingMb}
	registerProvider(t, svc, "p", keys, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: "maxFileSizeMb", Priority: 2, Value: -1},
			{Key: role.PolicyChunkedUploadMaxConcurrentSessions, Priority: 2, Value: -1},
			{Key: role.PolicyChunkedUploadMaxPendingMb, Priority: 2, Value: -1},
		}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, -1, policies["maxFileSizeMb"])
	assert.Equal(t, -1, policies[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, -1, policies[role.PolicyChunkedUploadMaxPendingMb])
}

func TestEffectivePolicy_PositiveValuesBelowInstanceCapsRemainUnchanged(t *testing.T) {
	svc, _, _, metaRepo := newTestService(t)
	svc.SetServerMaxFileSizeMb(100)
	metaRepo.Meta = &model.Meta{
		ID:                               "x",
		ChunkedUploadMaxSessionsPerUser:  8,
		ChunkedUploadMaxPendingMbPerUser: 512,
	}
	keys := []string{"maxFileSizeMb", role.PolicyChunkedUploadMaxConcurrentSessions, role.PolicyChunkedUploadMaxPendingMb}
	registerProvider(t, svc, "p", keys, func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
		return []plugin.EffectivePolicyContribution{
			{Key: "maxFileSizeMb", Priority: 2, Value: 50},
			{Key: role.PolicyChunkedUploadMaxConcurrentSessions, Priority: 2, Value: 4},
			{Key: role.PolicyChunkedUploadMaxPendingMb, Priority: 2, Value: 256},
		}, nil
	})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 50, policies["maxFileSizeMb"])
	assert.Equal(t, 4, policies[role.PolicyChunkedUploadMaxConcurrentSessions])
	assert.Equal(t, 256, policies[role.PolicyChunkedUploadMaxPendingMb])
}

// --- invalidation ---

func TestInvalidateUser_DropsUserPolicyCache(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`))}
	assign(t, assignRepo, "u1", "r1")
	assert.Equal(t, true, svc.GetUserPolicies("u1")["canSearchNotes"])

	// out-of-band role change + explicit user invalidation
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":false}}`))}
	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))
	assert.Equal(t, false, svc.GetUserPolicies("u1")["canSearchNotes"], "InvalidateUser must drop the cached policy input")
}

type blockingAssignmentRepo struct {
	*testutil.MockRoleAssignmentRepository

	mu         sync.Mutex
	calls      int
	firstRoles []*model.Role
	nextRoles  []*model.Role
	entered    chan struct{}
	release    chan struct{}
}

func (r *blockingAssignmentRepo) ListByUser(userID string) ([]*model.RoleAssignment, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	roles := append([]*model.Role(nil), r.nextRoles...)
	if first {
		roles = append([]*model.Role(nil), r.firstRoles...)
	}
	r.mu.Unlock()

	if first {
		close(r.entered)
		<-r.release
	}
	assignments := make([]*model.RoleAssignment, 0, len(roles))
	for i, resolvedRole := range roles {
		assignments = append(assignments, &model.RoleAssignment{
			ID:     fmt.Sprintf("a%d", i),
			UserID: userID,
			RoleID: resolvedRole.ID,
			Role:   resolvedRole,
		})
	}
	return assignments, nil
}

func TestInvalidateUser_InFlightMissCannotRepublishStaleRoles(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	oldRole := &model.Role{ID: "r1", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`))}
	freshRole := &model.Role{ID: "r1", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":false}}`))}
	assignRepo := &blockingAssignmentRepo{
		MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo),
		firstRoles:                   []*model.Role{oldRole},
		nextRoles:                    []*model.Role{freshRole},
		entered:                      make(chan struct{}),
		release:                      make(chan struct{}),
	}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	firstResult := make(chan map[string]any, 1)
	go func() {
		firstResult <- svc.GetUserPolicies("u1")
	}()
	<-assignRepo.entered
	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))
	close(assignRepo.release)
	assert.Equal(t, true, (<-firstResult)["canSearchNotes"])

	assert.Equal(t, false, svc.GetUserPolicies("u1")["canSearchNotes"], "the pre-invalidation miss must not republish its stale snapshot")
}

func TestInvalidateUser_OtherUserDoesNotDiscardInFlightRoleSnapshot(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	oldRole := &model.Role{ID: "r1", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`))}
	freshRole := &model.Role{ID: "r1", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":false}}`))}
	assignRepo := &blockingAssignmentRepo{
		MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo),
		firstRoles:                   []*model.Role{oldRole},
		nextRoles:                    []*model.Role{freshRole},
		entered:                      make(chan struct{}),
		release:                      make(chan struct{}),
	}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	firstResult := make(chan map[string]any, 1)
	go func() {
		firstResult <- svc.GetUserPolicies("u1")
	}()
	<-assignRepo.entered
	require.NoError(t, svc.InvalidateUser(context.Background(), "u2"))
	close(assignRepo.release)
	assert.Equal(t, true, (<-firstResult)["canSearchNotes"])

	assert.Equal(t, true, svc.GetUserPolicies("u1")["canSearchNotes"], "another user's invalidation must not discard this user's in-flight snapshot")
	assignRepo.mu.Lock()
	assert.Equal(t, 1, assignRepo.calls)
	assignRepo.mu.Unlock()
}

func TestInvalidateUser_PreservesUnrelatedCachedUser(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`))}
	assign(t, assignRepo, "u2", "r1")
	assert.Equal(t, true, svc.GetUserPolicies("u2")["canSearchNotes"])

	delete(assignRepo.Assignments, "u2:r1")
	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))

	assert.Equal(t, true, svc.GetUserPolicies("u2")["canSearchNotes"], "a user invalidation must not evict unrelated completed entries")
}

func TestInvalidateRolePolicies_DropsAllHolders(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`))}
	assign(t, assignRepo, "u1", "r1")
	assign(t, assignRepo, "u2", "r1")
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B"}
	assign(t, assignRepo, "u3", "r2")

	assert.Equal(t, true, svc.GetUserPolicies("u1")["canSearchNotes"])
	assert.Equal(t, true, svc.GetUserPolicies("u2")["canSearchNotes"])

	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Policies: datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":false}}`))}
	assert.Equal(t, true, svc.GetUserPolicies("u1")["canSearchNotes"], "repository replacement must remain hidden until invalidation")
	require.NoError(t, svc.InvalidateRolePolicies(context.Background(), "r1"))
	assert.Equal(t, false, svc.GetUserPolicies("u1")["canSearchNotes"])
	assert.Equal(t, false, svc.GetUserPolicies("u2")["canSearchNotes"])
}

func TestInvalidateRolePolicies_DropsCachedConditionalHolder(t *testing.T) {
	svc, roleRepo, _, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{
		ID:          "r1",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`)),
	}
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "bot", IsBot: true}))
	svc.SetUserRepo(userRepo)
	assert.Equal(t, true, svc.GetUserPolicies("u1")["canSearchNotes"])

	roleRepo.Roles["r1"] = &model.Role{
		ID:          "r1",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":false}}`)),
	}
	require.NoError(t, svc.InvalidateRolePolicies(context.Background(), "r1"))

	assert.Equal(t, false, svc.GetUserPolicies("u1")["canSearchNotes"], "conditional holders are not enumerable from role assignments")
}

type blockingRoleListRepo struct {
	*testutil.MockRoleRepository

	mu      sync.Mutex
	calls   int
	current []*model.Role
	entered chan struct{}
	release chan struct{}
}

func (r *blockingRoleListRepo) List() ([]*model.Role, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	roles := append([]*model.Role(nil), r.current...)
	r.mu.Unlock()
	if first {
		close(r.entered)
		<-r.release
	}
	return roles, nil
}

func (r *blockingRoleListRepo) setRoles(roles ...*model.Role) {
	r.mu.Lock()
	r.current = append([]*model.Role(nil), roles...)
	r.mu.Unlock()
}

func TestInvalidateUser_DoesNotDiscardInFlightSharedRoleList(t *testing.T) {
	conditionalRole := &model.Role{
		ID:          "r1",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`)),
	}
	baseRoleRepo := testutil.NewMockRoleRepository()
	roleRepo := &blockingRoleListRepo{
		MockRoleRepository: baseRoleRepo,
		current:            []*model.Role{conditionalRole},
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	assignRepo := testutil.NewMockRoleAssignmentRepository(baseRoleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	userRepo := testutil.NewMockUserRepository()
	for _, userID := range []string{"u1", "u3"} {
		require.NoError(t, userRepo.Create(&model.User{ID: userID, Username: userID, IsBot: true}))
	}
	svc.SetUserRepo(userRepo)

	firstResult := make(chan map[string]any, 1)
	go func() {
		firstResult <- svc.GetUserPolicies("u1")
	}()
	<-roleRepo.entered
	require.NoError(t, svc.InvalidateUser(context.Background(), "u2"))
	close(roleRepo.release)
	assert.Equal(t, true, (<-firstResult)["canSearchNotes"])
	assert.Equal(t, true, svc.GetUserPolicies("u3")["canSearchNotes"])

	roleRepo.mu.Lock()
	assert.Equal(t, 1, roleRepo.calls, "a user invalidation must not discard the shared role-list result")
	roleRepo.mu.Unlock()
}

func TestInvalidateRolePolicies_InFlightConditionalSnapshotCannotRepublish(t *testing.T) {
	oldRole := &model.Role{
		ID:          "r1",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":true}}`)),
	}
	freshRole := &model.Role{
		ID:          "r1",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isBot"}`)),
		Policies:    datatypes.JSON([]byte(`{"canSearchNotes":{"useDefault":false,"priority":0,"value":false}}`)),
	}
	baseRoleRepo := testutil.NewMockRoleRepository()
	roleRepo := &blockingRoleListRepo{
		MockRoleRepository: baseRoleRepo,
		current:            []*model.Role{oldRole},
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	assignRepo := testutil.NewMockRoleAssignmentRepository(baseRoleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "bot", IsBot: true}))
	svc.SetUserRepo(userRepo)

	firstResult := make(chan map[string]any, 1)
	go func() {
		firstResult <- svc.GetUserPolicies("u1")
	}()
	<-roleRepo.entered
	roleRepo.setRoles(freshRole)
	require.NoError(t, svc.InvalidateRolePolicies(context.Background(), "r1"))
	close(roleRepo.release)
	assert.Equal(t, true, (<-firstResult)["canSearchNotes"])

	assert.Equal(t, false, svc.GetUserPolicies("u1")["canSearchNotes"], "the in-flight conditional snapshot must not survive invalidation")
}

func TestInvalidateRolePolicies_EmptyRoleNoop(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	require.NoError(t, svc.InvalidateRolePolicies(context.Background(), ""))
}

// newCountingTestService は newTestService と同じ形の戻り値に、既存の
// countingAssignmentRepo (role_service_test.go) を挟んだもの。**型を使い回す**
// ことで「1 query」の主張が全 test で同じ数え方になる。
func newCountingTestService(t *testing.T) (*role.Service, *testutil.MockRoleRepository, *testutil.MockRoleAssignmentRepository, *countingAssignmentRepo) {
	t.Helper()
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	idGen, _ := id.NewGenerator("aidx")
	counting := &countingAssignmentRepo{MockRoleAssignmentRepository: assignRepo}
	return role.NewService(roleRepo, counting, metaRepo, idGen), roleRepo, assignRepo, counting
}

func TestEffectivePolicy_ActiveAssignmentsCoverOnlyActiveManualRoles(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B", Target: model.RoleTargetManual}
	// conditional role は condFormula で一致する。assignment row は無い。
	roleRepo.Roles["r3"] = &model.Role{ID: "r3", Name: "C", Target: model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isLocal"}`))}
	// **conditional に切り替えた role を指す残骸行。** `role_assignment` の行はロールを
	// 切り替えても消えないので、`Role` が populate 済みで `Target=conditional` にしたまま
	// 入り込んだ行になる。
	// resolved roles には入る (native contribution がある) が assignment 同一性は無いので、
	// ActiveAssignments には入れてはいけない。
	roleRepo.Roles["r4"] = &model.Role{ID: "r4", Name: "D", Target: model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isLocal"}`))}
	assign(t, assignRepo, "u1", "r1")
	assign(t, assignRepo, "u1", "r2")
	assignRepo.Assignments["u1:r4"] = &model.RoleAssignment{ID: "a_u1_r4", UserID: "u1", RoleID: "r4"}
	// orphan assignment: role_assignment の行だけが残っている。
	assignRepo.Assignments["u1:r9"] = &model.RoleAssignment{ID: "a_u1_r9", UserID: "u1", RoleID: "r9"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1"} // Host nil なので isLocal が真
	svc.SetUserRepo(userRepo)

	var request plugin.EffectivePolicyRequest
	registerProvider(t, svc, "ctx", []string{"canSearchNotes"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			request = req
			return nil, nil
		})

	_, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)

	assert.Equal(t, []string{"r1", "r2", "r3", "r4"}, request.RoleIDs, "RoleIDs は conditional を含むという既存契約を変えない")
	assert.Equal(t, []plugin.ActiveRoleAssignment{
		{RoleID: "r1", AssignmentID: "a_u1_r1"},
		{RoleID: "r2", AssignmentID: "a_u1_r2"},
	}, request.ActiveAssignments,
		"assignment 行が無い conditional role / conditional を指す残骸行 / role 行が無い orphan は ActiveAssignments に入らない")
	// **部分集合の不変条件。** RoleIDs にあるのに ActiveAssignments に入らない、あるいは
	// 逆が起きたら host 側で契約が壊れている。
	for _, a := range request.ActiveAssignments {
		assert.Contains(t, request.RoleIDs, a.RoleID)
	}
}

func TestEffectivePolicy_ActiveAssignmentsAreNonNilForAnonymous(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	var request plugin.EffectivePolicyRequest
	var seen bool
	registerProvider(t, svc, "ctx", []string{"canSearchNotes"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			seen = true
			request = req
			return nil, nil
		})

	_, err := svc.GetUserPoliciesChecked("")
	require.NoError(t, err)
	require.True(t, seen, "匿名解決でも provider は呼ばれるので ActiveAssignments は非nil空sliceである必要がある")
	assert.NotNil(t, request.ActiveAssignments)
	assert.Empty(t, request.ActiveAssignments)
}

func TestEffectivePolicy_ActiveAssignmentsExcludeExpiredAssignments(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B", Target: model.RoleTargetManual}
	assign(t, assignRepo, "u1", "r1")
	past := time.Now().Add(-time.Hour)
	assignRepo.Assignments["u1:r2"] = &model.RoleAssignment{ID: "a_u1_r2", UserID: "u1", RoleID: "r2", ExpiresAt: &past}

	var request plugin.EffectivePolicyRequest
	registerProvider(t, svc, "ctx", []string{"canSearchNotes"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			request = req
			return nil, nil
		})

	_, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, []plugin.ActiveRoleAssignment{{RoleID: "r1", AssignmentID: "a_u1_r1"}}, request.ActiveAssignments)
}

// **1 回の解決で ListByUser は 1 回だけ。** roles と activeAssignments を別々に
// 読む設計に戻ると 2 回になり、hot path に query が 1 本増える。
func TestEffectivePolicy_ResolutionIssuesExactlyOneQuery(t *testing.T) {
	svc, roleRepo, assignRepo, counting := newCountingTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	assign(t, assignRepo, "u1", "r1")
	var seen int
	registerProvider(t, svc, "ctx", []string{"canSearchNotes"},
		func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			seen++
			return nil, nil
		})

	_, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 1, seen)
	assert.Equal(t, 1, counting.listByUserCalls, "roles と activeAssignments は 1 回の ListByUser から共に作られる")

	// warm cache: provider 自身が LRU に当たって resolver を呼ばないが、role
	// スナップショットは native pass の前に必ず通る。ここで 2 本目が出たら検出できる。
	_, err = svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 1, counting.listByUserCalls, "warm cache は 2 つとも答える")
}

// **invalidation は「読み直す」ために存在する。** 空の assignments を返して黙る、
// という別の故障を許さない。
//
// `InvalidateUser` (= per-user の role cache と provider cache を落とす公開の
// 無効化入口) を使う。`InvalidateUserRoleCache` だけだと role snapshot は読み直す
// ものの provider の成功結果 LRU が同じ key を残したままなので、resolver は2回目に
// 呼ばれず「解決後に渡された assignments」を観測できない。
func TestEffectivePolicy_InvalidationRefetchesBothInsteadOfEmptyingAssignments(t *testing.T) {
	svc, roleRepo, assignRepo, counting := newCountingTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	assign(t, assignRepo, "u1", "r1")
	want := []plugin.ActiveRoleAssignment{{RoleID: "r1", AssignmentID: "a_u1_r1"}}
	var got [][]plugin.ActiveRoleAssignment
	registerProvider(t, svc, "ctx", []string{"canSearchNotes"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			got = append(got, req.ActiveAssignments)
			return nil, nil
		})

	_, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	require.NoError(t, svc.InvalidateUser(context.Background(), "u1"))
	_, err = svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)

	assert.Equal(t, 2, counting.listByUserCalls, "invalidated な user は読み直す")
	require.Len(t, got, 2, "provider は 2 回とも呼ばれる")
	assert.Equal(t, want, got[0])
	assert.Equal(t, want, got[1], "invalidation 後の解決で assignments が空に堕ちてはいけない")
}

// **repository の読み損ねは握り潰さない。** activeAssignments を空で埋めた partial
// snapshot を渡すと、plugin には「その role に active な assignment が無い」という
// 嘘が見えるので、provider を起動する前に error にする。
//
// 既存の `TestEffectivePolicy_RoleLookupErrorSkipsProvidersAndRemainsDistinct` は同じ
// 経路を「provider を起動しない」角度で固定している。こちらは assignments 側の契約を
// 明示する **regression guard** で、実装前から緑になる（既存の `GetUserRoles` が既に
// error を返すため）。RED は Step 2 の内部 test で観測する。
func TestEffectivePolicy_AssignmentRepositoryFailureSkipsProviders(t *testing.T) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := &failingPolicyAssignmentRepo{
		MockRoleAssignmentRepository: testutil.NewMockRoleAssignmentRepository(roleRepo),
		err:                          errors.New("assignment lookup failed"),
	}
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	var providerCalls atomic.Int32
	var assignments []plugin.ActiveRoleAssignment
	registerProvider(t, svc, "p", []string{"canSearchNotes"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			providerCalls.Add(1)
			assignments = req.ActiveAssignments
			return nil, nil
		})

	_, err := svc.GetUserPoliciesChecked("u1")

	require.Error(t, err)
	assert.ErrorContains(t, err, "role: effective policy inputs")
	assert.Zero(t, providerCalls.Load(), "role 入力を読めないとき provider を起動しない")
	assert.Nil(t, assignments, "assignment を空で埋めた値で provider を起動しない")
}

func BenchmarkEffectivePolicy_NoProviderAnonymous(b *testing.B) {
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(b, err)
	svc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = svc.GetUserPolicies("")
	}
}

// 置換は native priority を引き継ぐ。**priority 0 で押し込まない。**
//
//	r1: mentionLimit priority 1 = 10（native 結果は 10）
//	r2: mentionLimit priority 0 = 100
//	r1 を 40 に置換 → priority 1 の group だけが集約されるので 40。
//	priority 0 で押し込んでいたら priority 1 group が消えて max(40, 100) = 100。
func TestEffectivePolicy_ReplacementKeepsTheNativePriority(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":0,"value":100}}`))}
	assign(t, assignRepo, "u1", "r1")
	assign(t, assignRepo, "u1", "r2")

	// 置換が無ければ priority 1 の group が 10 で決まる。
	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	require.Equal(t, 10, policies["mentionLimit"], "native は priority 1 の group だけで決まる")

	registerProvider(t, svc, "level", []string{"mentionLimit"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: 40, ReplaceRoleID: req.ActiveAssignments[0].RoleID,
			}}, nil
		})

	policies, err = svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, 40, policies["mentionLimit"], "置換は native の priority を引き継ぐので r2 の 100 に負けない")
}

// **置換でも explicit は contribution の値に従う。** explicit は intersection する policy
// だけで意味を持ち、「そのロールが明示的に設定したか」を表す。どちらのロールも key を
// 宣言していないので native は base 参加 = 明示設定なしとして intersection には何も
// 乗らない。r1 を明示値で置換すると初めて乗る。
func TestEffectivePolicy_ReplacementCarriesTheExplicitFlagForIntersection(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B", Target: model.RoleTargetManual}
	assign(t, assignRepo, "u1", "r1")
	assign(t, assignRepo, "u1", "r2")

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	require.Equal(t, []string{}, policies[role.PolicyOptOutNotificationTypes], "どちらも未設定なので intersection は空")

	registerProvider(t, svc, "level", []string{role.PolicyOptOutNotificationTypes},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: role.PolicyOptOutNotificationTypes, Value: []string{"note"}, ReplaceRoleID: "r1",
			}}, nil
		})

	policies, err = svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, []string{"note"}, policies[role.PolicyOptOutNotificationTypes],
		"明示値での置換だけが intersection に参加する")
}

// **`UseDefault: true` の置換は「この role の override を base に戻す」** = native の
// useDefault と同じ扱い。explicit は立たない。
func TestEffectivePolicy_ReplacementWithUseDefaultStaysUnset(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual}
	roleRepo.Roles["r2"] = &model.Role{ID: "r2", Name: "B", Target: model.RoleTargetManual}
	assign(t, assignRepo, "u1", "r1")
	assign(t, assignRepo, "u1", "r2")
	registerProvider(t, svc, "level", []string{role.PolicyOptOutNotificationTypes},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: role.PolicyOptOutNotificationTypes, UseDefault: true, Value: []string{"note"}, ReplaceRoleID: "r1",
			}}, nil
		})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err)
	assert.Equal(t, []string{}, policies[role.PolicyOptOutNotificationTypes],
		"UseDefault の置換は明示設定にならず intersection に参加しない")
}

// **競合は pair 単位。** provider 全体を失敗扱いにしてしまうと、その provider の無関係な
// key まで native に戻ってしまう。置換したい plugin だけを黙らせる形の被害を出さない。
func TestEffectivePolicy_ReplacementConflictFallsBackToNative(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	assign(t, assignRepo, "u1", "r1")
	replace := func(value int) plugin.EffectivePolicyResolver {
		return func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: value, ReplaceRoleID: req.ActiveAssignments[0].RoleID,
			}}, nil
		}
	}
	registerProvider(t, svc, "level-a", []string{"mentionLimit"}, replace(40))
	registerProvider(t, svc, "level-b", []string{"mentionLimit"}, replace(90))

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.ErrorIs(t, err, role.ErrEffectivePolicyReplacementConflict)
	assert.Equal(t, 10, policies["mentionLimit"], "競合した pair は管理者が設定した native 値が残る")
	assert.NotErrorIs(t, err, role.ErrEffectivePolicyProvider, "競合は provider 失敗と区別する")
}

// 競合した key 以外は、**両 provider の通常contributionを通常通り集約する。**
func TestEffectivePolicy_ReplacementConflictKeepsOtherContributions(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	assign(t, assignRepo, "u1", "r1")
	build := func(mentionLimit, userListLimit int) plugin.EffectivePolicyResolver {
		return func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{
				{Key: "mentionLimit", Value: mentionLimit, ReplaceRoleID: req.ActiveAssignments[0].RoleID},
				{Key: "userListLimit", Priority: 1, Value: userListLimit},
			}, nil
		}
	}
	registerProvider(t, svc, "level-a", []string{"mentionLimit", "userListLimit"}, build(40, 50))
	registerProvider(t, svc, "level-b", []string{"mentionLimit", "userListLimit"}, build(90, 60))

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.ErrorIs(t, err, role.ErrEffectivePolicyReplacementConflict)
	assert.Equal(t, 10, policies["mentionLimit"], "競合した pair だけ native")
	assert.Equal(t, 60, policies["userListLimit"], "競合していない key は両 provider の contribution を集約する")
}

// **provider 失敗は宣言 key を native へ戻す（既存挙動）。** 他 provider の置換も同じ key
// なら巻き戻る。既存 sentinel は単独のときは素のまま。
func TestEffectivePolicy_FailedProviderWinsOverAnotherProvidersReplacement(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	assign(t, assignRepo, "u1", "r1")
	registerProvider(t, svc, "broken", []string{"mentionLimit"},
		func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return nil, errors.New("provider storage failure")
		})
	registerProvider(t, svc, "level", []string{"mentionLimit"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: 40, ReplaceRoleID: req.ActiveAssignments[0].RoleID,
			}}, nil
		})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.Equal(t, role.ErrEffectivePolicyProvider, err, "単独の provider 失敗は素の sentinel")
	assert.Equal(t, 10, policies["mentionLimit"], "失敗 provider の宣言 key は native へ戻る")
}

// **provider 失敗と競合が同時に起きたら両方の error が errors.Is で辿れる。**
func TestEffectivePolicy_JoinsProviderFailureAndReplacementConflict(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	assign(t, assignRepo, "u1", "r1")
	replace := func(value int) plugin.EffectivePolicyResolver {
		return func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: value, ReplaceRoleID: req.ActiveAssignments[0].RoleID,
			}}, nil
		}
	}
	registerProvider(t, svc, "broken", []string{"canSearchNotes"},
		func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return nil, errors.New("provider storage failure")
		})
	registerProvider(t, svc, "level-a", []string{"mentionLimit"}, replace(40))
	registerProvider(t, svc, "level-b", []string{"mentionLimit"}, replace(90))

	_, err := svc.GetUserPoliciesChecked("u1")
	require.ErrorIs(t, err, role.ErrEffectivePolicyProvider)
	require.ErrorIs(t, err, role.ErrEffectivePolicyReplacementConflict)
}

// **unchecked 解決は fallback map を返すだけ。** error は地表に出さない。
func TestEffectivePolicy_ReplacementConflictUncheckedResolutionFallsBack(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	assign(t, assignRepo, "u1", "r1")
	replace := func(value int) plugin.EffectivePolicyResolver {
		return func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: value, ReplaceRoleID: req.ActiveAssignments[0].RoleID,
			}}, nil
		}
	}
	registerProvider(t, svc, "level-a", []string{"mentionLimit"}, replace(40))
	registerProvider(t, svc, "level-b", []string{"mentionLimit"}, replace(90))

	policies := svc.GetUserPolicies("u1")
	assert.Equal(t, 10, policies["mentionLimit"])
}

func TestEffectivePolicy_ReplacementConflictUncheckedWarningsAreCountedAndRateLimited(t *testing.T) {
	var logs lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	svc, roleRepo, assignRepo, _ := newTestService(t)
	// Service が構築時 logger を保持し、解決時の global default に書かないことも固定する。
	slog.SetDefault(previous)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	for i := range 5 {
		assign(t, assignRepo, fmt.Sprintf("u%d", i), "r1")
	}
	replace := func(value int) plugin.EffectivePolicyResolver {
		return func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: value, ReplaceRoleID: req.ActiveAssignments[0].RoleID,
			}}, nil
		}
	}
	registerProvider(t, svc, "level-a", []string{"mentionLimit"}, replace(40))
	registerProvider(t, svc, "level-b", []string{"mentionLimit"}, replace(90))

	for i := range 5 {
		policies := svc.GetUserPolicies(fmt.Sprintf("u%d", i))
		assert.Equal(t, 10, policies["mentionLimit"])
	}

	output := logs.String()
	assert.Equalf(t, 3, strings.Count(output, "effective policy replacement conflict"),
		"occurrences 1, 2, and 4 are reported instead of logging every request\nlogs:\n%s", output)
	assert.Contains(t, output, "occurrences=4")
	assert.Contains(t, output, "mentionLimit:r1")
}

// **置換が active でない role を名乗れば provider 全体が失敗扱い。** malformed output と
// 同じ扱いなので、宣言 key は native へ戻る。
func TestEffectivePolicy_ReplacementOfInactiveRoleFailsTheProvider(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	assign(t, assignRepo, "u1", "r1")
	registerProvider(t, svc, "level", []string{"mentionLimit"},
		func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{Key: "mentionLimit", Value: 40, ReplaceRoleID: "r-other"}}, nil
		})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.Equal(t, role.ErrEffectivePolicyProvider, err)
	assert.Equal(t, 10, policies["mentionLimit"])
}

// **置換で priority を選ぶと provider 全体が失敗扱い。** 0 以外は malformed。
func TestEffectivePolicy_ReplacementChoosingAPriorityFailsTheProvider(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	assign(t, assignRepo, "u1", "r1")
	registerProvider(t, svc, "level", []string{"mentionLimit"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: 40, Priority: 1, ReplaceRoleID: req.ActiveAssignments[0].RoleID,
			}}, nil
		})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.Equal(t, role.ErrEffectivePolicyProvider, err)
	assert.Equal(t, 10, policies["mentionLimit"])
}

// **置換の target は resolver を呼ぶ前に確定していなければならない。** host は resolver に
// `ActiveAssignments` を**複製して**渡すので、resolver は自分の slice を書き換えられる。
// 複製し直してから「置換してよい role」を判定すると、resolver が「active な role を
// conditional な role に差し替えて」置換を通できてしまう。
//
// 攻撃者は `RoleIDs` を見るだけで conditional role の ID を知れるので、差し込む ID は
// 実際に手に入る:
//
//	r1 (manual, active):      mentionLimit priority 1 = 10
//	r-conditional (条件一致): mentionLimit priority 1 = 20 → native は max(10, 20) = 20
//	                        RoleIDs には出るが ActiveAssignments には出ない
//
// 差し替えが通る (修正前): priority 1 group が max(10, 40) = 40 になる
// 差し替えが弾かれる (修正後): provider 全体が失敗扱いで native の 20 に戻る
func TestEffectivePolicy_ReplacementCannotForgeTheTargetThroughTheRequest(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	roleRepo.Roles["r-conditional"] = &model.Role{ID: "r-conditional", Name: "C", Target: model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isLocal"}`)),
		Policies:    datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":20}}`))}
	assign(t, assignRepo, "u1", "r1")
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1"} // Host nil なので isLocal が真
	svc.SetUserRepo(userRepo)

	registerProvider(t, svc, "level", []string{"mentionLimit"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			// r1 は実際に active な手動ロールなのに、resolver が自分の slice を conditional
			// な role に書き換えて、その target の置換を返す。
			req.ActiveAssignments[0].RoleID = "r-conditional"
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: 40, ReplaceRoleID: "r-conditional",
			}}, nil
		})

	policies, err := svc.GetUserPoliciesChecked("u1")
	// **assert 2 つで報告する。** 「差し替えが通った」証拠は error が nil なことと
	// 値が 40 になることの両方で出る。require で止めると片方しか見えない。
	assert.Equal(t, role.ErrEffectivePolicyProvider, err, "resolver が書き換えた target は置換先にならない")
	assert.Equal(t, 20, policies["mentionLimit"], "宣言 key は native へ戻る")
}

// **逆に、request を書き換えても「本来の対象」は置換できる。** target の判定は
// **resolver 呼び出し前**の集合で行うので、resolver 側の書き込みで本来 active だった
// role まで失ってはいけない（= 置換をすべて不正扱いにするような過大な修正の防線）。
//
//	r1 を 40 に置換 → priority 1 group が max(40, r-conditional の 20) = 40
func TestEffectivePolicy_RequestMutationDoesNotHideTheGenuineReplacementTarget(t *testing.T) {
	svc, roleRepo, assignRepo, _ := newTestService(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "A", Target: model.RoleTargetManual,
		Policies: datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":10}}`))}
	roleRepo.Roles["r-conditional"] = &model.Role{ID: "r-conditional", Name: "C", Target: model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isLocal"}`)),
		Policies:    datatypes.JSON([]byte(`{"mentionLimit":{"priority":1,"value":20}}`))}
	assign(t, assignRepo, "u1", "r1")
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1"} // Host nil なので isLocal が真
	svc.SetUserRepo(userRepo)

	registerProvider(t, svc, "level", []string{"mentionLimit"},
		func(_ context.Context, req plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
			// 書き換えるのは snapshot 済みの slice だけ。返す置換は実際に active だった r1 を
			// 名乗るので、呼び出し前に確定した集合なら通る。
			req.ActiveAssignments[0].RoleID = "r-conditional"
			return []plugin.EffectivePolicyContribution{{
				Key: "mentionLimit", Value: 40, ReplaceRoleID: "r1",
			}}, nil
		})

	policies, err := svc.GetUserPoliciesChecked("u1")
	require.NoError(t, err, "resolver が request を書き換えたこと自体は provider 失敗ではない")
	assert.Equal(t, 40, policies["mentionLimit"], "呼び出し前に確定した active な role は置換できる")
}
