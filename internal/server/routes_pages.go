package server

import (
	"github.com/elythia-network/elythia/internal/api/pages"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerPagesRoutes registers the pages routes (pages/* and page-push) on
// the /api group.
func registerPagesRoutes(api apiRoutes, pagesHandler *pages.Handler) {
	api.POST("/pages/create", pagesHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:pages"))
	api.POST("/pages/show", pagesHandler.Show)
	api.POST("/pages/update", pagesHandler.Update, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:pages"))
	api.POST("/pages/delete", pagesHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:pages"))
	api.POST("/pages/featured", pagesHandler.Featured)
	api.POST("/pages/like", pagesHandler.Like, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:page-likes"))
	api.POST("/pages/unlike", pagesHandler.Unlike, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:page-likes"))

	// page-push — page scriptが任意のeventをpage所有者のmainに emit する。
	api.POST("/page-push", pagesHandler.PagePush, middleware.RequireAuth(), middleware.RequireSecure())
}
