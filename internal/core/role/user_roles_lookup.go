package role

import (
	"log/slog"

	"github.com/elythia-network/elythia/internal/model"
)

// UserRolesLookupAdapter wraps a role.Service so that entity.PackUserLite /
// PackUserDetailed can populate the `roles` / `badgeRoles` JSON fields
// without entity depending on core/role (= preserves layering: api → core →
// entity, never reverse). Implements entity.UserRolesLookup (issue #1103).
type UserRolesLookupAdapter struct {
	provider UserRolesProvider
}

// UserRolesProvider is the subset of role.Service used by the adapter.
// Defined here as an interface so tests can inject a stub without dragging
// the full Service into the test surface.
type UserRolesProvider interface {
	GetUserRoles(userID string) ([]*model.Role, error)
}

// NewUserRolesLookup returns an entity.UserRolesLookup that resolves a
// user's assigned roles via the role service. Use entity.SetUserRolesLookup
// to wire. Passing nil provider yields an adapter that always returns nil
// (= entity sees empty roles, behaves like pre-#1103).
func NewUserRolesLookup(provider UserRolesProvider) *UserRolesLookupAdapter {
	return &UserRolesLookupAdapter{provider: provider}
}

// LookupUserRoles delegates to role.Service.GetUserRoles. Errors are logged
// and a nil slice is returned — the calling entity layer then renders an
// empty `roles` / `badgeRoles` array, matching upstream's behaviour for
// users with no role assignments. role.Service already short-circuits with
// a per-user in-memory cache (#761), so this is a sync function with no
// hot-path DB cost.
func (a *UserRolesLookupAdapter) LookupUserRoles(userID string) []*model.Role {
	if a == nil || a.provider == nil {
		return nil
	}
	roles, err := a.provider.GetUserRoles(userID)
	if err != nil {
		// role 取得失敗は frontend に空 roles を返す方が安全 (= roles 表示
		// が一時的に消えるだけで、誤って他 user の role を出すことはない)。
		// 観測用に slog で残す。
		slog.Warn("user roles lookup: GetUserRoles failed", "userID", userID, "err", err)
		return nil
	}
	return roles
}
