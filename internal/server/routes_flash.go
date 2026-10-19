package server

import (
	apiflash "github.com/elythia-network/elythia/internal/api/flash"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerFlashRoutes registers the flash (Play) routes on the /api group.
func registerFlashRoutes(api apiRoutes, flashHandler *apiflash.Handler) {
	api.POST("/flash/create", flashHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:flash"))
	api.POST("/flash/show", flashHandler.Show)
	api.POST("/flash/update", flashHandler.Update, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:flash"))
	api.POST("/flash/delete", flashHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:flash"))
	api.POST("/flash/featured", flashHandler.Featured)
	api.POST("/flash/search", flashHandler.Search)
	api.POST("/flash/like", flashHandler.Like, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:flash-likes"))
	api.POST("/flash/unlike", flashHandler.Unlike, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:flash-likes"))
	api.POST("/flash/my", flashHandler.My, middleware.RequireAuth(), middleware.RequireScope("read:flash"))
	api.POST("/flash/my-likes", flashHandler.MyLikes, middleware.RequireAuth(), middleware.RequireScope("read:flash-likes"))
}
