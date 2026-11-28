package server

import (
	"errors"

	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
)

// newRoleNotifLookup resolves the role of a roleAssigned notification (#1559)
// to its packed form at read time. It reports false with a nil error when the
// role has been deleted, and a non-nil error when the role or its usersCount
// could not be read (entity.RoleLookup).
func newRoleNotifLookup(roleRepo repository.RoleRepository, roleService *corerole.Service, idGen id.Generator) entity.RoleLookup {
	return func(roleID string) (map[string]any, bool, error) {
		r, err := roleRepo.FindByID(roleID)
		if errors.Is(err, repository.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			// 通知を落とすのはこれまでどおり。エラーを返すのは、ページの中で
			// 「削除済み」として覚えさせないため (#4263)。記録は entity の側で、
			// ロールごとに 1 回だけ行う (entity.RoleLookup の約束)。
			return nil, false, err
		}
		if r == nil {
			return nil, false, nil
		}
		// 受け取る人を問わず、roles/users がモデレーターでない人に見せる人数で出す (#3999)。
		// 数えられなければ 0 で出し (これまでどおり)、エラーを返して覚えさせない。
		n, err := roleService.CountVisibleAssignedUsersErr(roleID)
		return entity.PackRole(r, n, idGen, corerole.DefaultPolicies()), true, err
	}
}
