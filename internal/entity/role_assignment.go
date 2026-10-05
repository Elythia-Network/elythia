package entity

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// PackRoleAssignmentLookup builds the exact-assignment response shared by the
// self and administrator endpoints. The assigned flag reflects the
// `role_assignment` row only; it never evaluates a conditional role's formula.
//
// target=conditional の role は行を持たず condFormula の read 時評価で決まるので、
// 条件を満たしていても assigned は false になる。**呼び出し側がそれを判別できる
// ように role.target を返す** (#2633)。落とすと「条件を満たすのに false」と
// 「そもそも付与が無い」が区別できなくなる。
func PackRoleAssignmentLookup(assignment *model.RoleAssignment, r *model.Role) map[string]any {
	assigned := assignment != nil
	var expiresAt *time.Time
	if assigned {
		expiresAt = assignment.ExpiresAt
	}
	return map[string]any{
		"assigned":  assigned,
		"expiresAt": ISOMillisPtr(expiresAt),
		"role": map[string]any{
			"id":                        r.ID,
			"target":                    r.Target,
			"isPublic":                  r.IsPublic,
			"canEditMembersByModerator": r.CanEditMembersByModerator,
		},
	}
}
