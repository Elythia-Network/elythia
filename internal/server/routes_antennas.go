package server

import (
	"github.com/elythia-network/elythia/internal/api/antennas"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerAntennasRoutes registers the antennas routes on the /api group.
func registerAntennasRoutes(api apiRoutes, antennasHandler *antennas.Handler) {
	api.POST("/antennas/create", antennasHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/antennas/show", antennasHandler.Show, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/antennas/update", antennasHandler.Update, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/antennas/delete", antennasHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/antennas/list", antennasHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/antennas/notes", antennasHandler.Notes, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/antennas/remove-note", antennasHandler.RemoveNote, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account")) // #2069 #17463
}
