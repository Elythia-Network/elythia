package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestPackRoleAssignmentLookup(t *testing.T) {
	r := &model.Role{ID: "r1", Target: model.RoleTargetManual, IsPublic: true, CanEditMembersByModerator: true}
	assert.Equal(t, map[string]any{
		"assigned":  false,
		"expiresAt": nil,
		"role": map[string]any{
			"id":                        "r1",
			"target":                    model.RoleTargetManual,
			"isPublic":                  true,
			"canEditMembersByModerator": true,
		},
	}, PackRoleAssignmentLookup(nil, r))

	expiresAt := time.Date(2026, 1, 2, 12, 4, 5, 123000000, time.FixedZone("JST", 9*60*60))
	got := PackRoleAssignmentLookup(&model.RoleAssignment{ExpiresAt: &expiresAt}, r)
	assert.Equal(t, true, got["assigned"])
	assert.Equal(t, "2026-01-02T03:04:05.123Z", got["expiresAt"])
}

// conditional role は role_assignment 行を持たないので assigned は常に false になる。
// 呼び出し側がそれを「条件を満たしていない」と誤読しないための手掛かりが target。
func TestPackRoleAssignmentLookup_ConditionalTargetIsExposed(t *testing.T) {
	r := &model.Role{ID: "cond", Target: model.RoleTargetConditional, IsPublic: true}
	got := PackRoleAssignmentLookup(nil, r)

	assert.Equal(t, false, got["assigned"])
	role, ok := got["role"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, model.RoleTargetConditional, role["target"])
}

// JSON では target が文字列として出ること (map の値は model.RoleTarget だが、
// レスポンスの契約は upstream packedRoleSchema と同じ "manual" / "conditional")。
func TestPackRoleAssignmentLookup_TargetSerializesAsString(t *testing.T) {
	for _, target := range []model.RoleTarget{model.RoleTargetManual, model.RoleTargetConditional} {
		b, err := json.Marshal(PackRoleAssignmentLookup(nil, &model.Role{ID: "r1", Target: target}))
		assert.NoError(t, err)
		assert.Contains(t, string(b), `"target":"`+string(target)+`"`)
	}
}
