package server

import (
	apiroles "github.com/elythia-network/elythia/internal/api/roles"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerRolesRoutes registers the roles routes on the /api group.
func registerRolesRoutes(api apiRoutes, rolesHandler *apiroles.Handler) {
	api.POST("/roles/list", rolesHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/roles/show", rolesHandler.Show)
	api.POST("/roles/assignment-show", rolesHandler.AssignmentShow, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/roles/users", rolesHandler.Users)
	api.POST("/roles/notes", rolesHandler.Notes, middleware.RequireAuth(), middleware.RequireScope("read:account"))
}
