package server

import (
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
)

// newRoleNotifLookup resolves the role of a roleAssigned notification (#1559)
// to its packed form at read time. It reports false when the role has been
// deleted.
func newRoleNotifLookup(roleRepo repository.RoleRepository, roleService *corerole.Service, idGen id.Generator) func(roleID string) (map[string]any, bool) {
	return func(roleID string) (map[string]any, bool) {
		r, err := roleRepo.FindByID(roleID)
		if err != nil || r == nil {
			return nil, false
		}
		// 受け取る人を問わず、roles/users がモデレーターでない人に見せる人数で出す (#3999)。
		return entity.PackRole(r, roleService.CountVisibleAssignedUsers(roleID), idGen, corerole.DefaultPolicies()), true
	}
}
