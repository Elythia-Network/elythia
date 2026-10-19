package server

import (
	apifetchexternal "github.com/elythia-network/elythia/internal/api/fetchexternal"
	apifetchrss "github.com/elythia-network/elythia/internal/api/fetchrss"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerFetchRoutes registers the routes that fetch an external resource
// for the client (fetch-rss and fetch-external-resources) on the /api group.
func registerFetchRoutes(api apiRoutes, fetchExternalHandler *apifetchexternal.Handler, fetchRSSHandler *apifetchrss.Handler) {
	api.POST("/fetch-external-resources", fetchExternalHandler.Fetch, middleware.RequireAuth(), middleware.RequireSecure())

	api.GET("/fetch-rss", fetchRSSHandler.Fetch)
	api.POST("/fetch-rss", fetchRSSHandler.Fetch)
}
