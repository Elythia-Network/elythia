package entity

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// roleTSTimeFormat is Misskey's ISO timestamp format (ms precision, Z).
const roleTSTimeFormat = "2006-01-02T15:04:05.000Z"

// PackRole builds the upstream-compatible Role response shape (mirrors
// RoleEntityService.pack). Both admin (/api/admin/roles/*) and public
// (/api/roles/*) endpoints return this shape in Misskey.
//
// usersCount is the number of non-expired assignments (caller resolves it via
// RoleAssignmentRepository.CountActiveByRole). defaultPolicies is the instance
// DEFAULT_POLICIES map (key -> default value); each policy key absent from the
// role's own overrides is filled with {useDefault:true, priority:0, value}.
// idGen derives createdAt from the role ID (aidx); on failure it falls back to
// updatedAt so the timestamp is always a valid ISO string.
func PackRole(r *model.Role, usersCount int, idGen id.Generator, defaultPolicies map[string]any) map[string]any {
	createdAt := r.UpdatedAt.UTC().Format(roleTSTimeFormat)
	if idGen != nil {
		if t, err := idGen.ParseTime(r.ID); err == nil {
			createdAt = t.UTC().Format(roleTSTimeFormat)
		}
	}

	// policies: role の override に default-fill を被せる (upstream pack と同じ)。
	policies := map[string]any{}
	if len(r.Policies) > 0 {
		_ = json.Unmarshal(r.Policies, &policies)
	}
	for k, v := range defaultPolicies {
		if cur, ok := policies[k]; !ok || cur == nil {
			policies[k] = map[string]any{
				"useDefault": true,
				"priority":   0,
				"value":      v,
			}
		}
	}

	// condFormula は jsonb を object として返す (空/不正/非object なら {})。
	// upstream packedRoleSchema は condFormula を object 必須としているため、
	// 配列やスカラーが入っていても object 契約を守る。
	condFormula := map[string]any{}
	if len(r.CondFormula) > 0 {
		var parsed any
		if json.Unmarshal(r.CondFormula, &parsed) == nil {
			if m, ok := parsed.(map[string]any); ok {
				condFormula = m
			}
		}
	}

	return map[string]any{
		"id":                              r.ID,
		"createdAt":                       createdAt,
		"updatedAt":                       r.UpdatedAt.UTC().Format(roleTSTimeFormat),
		"name":                            r.Name,
		"description":                     r.Description,
		"color":                           r.Color,
		"iconUrl":                         r.IconURL,
		"target":                          r.Target,
		"condFormula":                     condFormula,
		"isPublic":                        r.IsPublic,
		"isAdministrator":                 r.IsAdministrator,
		"isModerator":                     r.IsModerator,
		"isExplorable":                    r.IsExplorable,
		"asBadge":                         r.AsBadge,
		"preserveAssignmentOnMoveAccount": r.PreserveAssignmentOnMoveAccount,
		"canEditMembersByModerator":       r.CanEditMembersByModerator,
		"displayOrder":                    r.DisplayOrder,
		"policies":                        policies,
		"usersCount":                      usersCount,
	}
}
