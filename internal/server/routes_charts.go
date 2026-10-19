package server

import (
	"net/http"

	apicharts "github.com/elythia-network/elythia/internal/api/charts"
)

// registerChartsRoutes registers the charts and retention routes on the /api
// group.
func registerChartsRoutes(api apiRoutes, chartsHandler *apicharts.Handler) {
	chartMethods := []string{http.MethodGet, http.MethodPost}

	api.Match(chartMethods, "/charts/notes", chartsHandler.Notes)
	api.Match(chartMethods, "/charts/users", chartsHandler.Users)
	api.Match(chartMethods, "/charts/drive", chartsHandler.Drive)
	api.Match(chartMethods, "/charts/federation", chartsHandler.Federation)
	api.Match(chartMethods, "/charts/instance", chartsHandler.Instance)
	api.Match(chartMethods, "/charts/ap-request", chartsHandler.ApRequest)
	api.Match(chartMethods, "/charts/active-users", chartsHandler.ActiveUsers)
	api.Match(chartMethods, "/charts/user/notes", chartsHandler.UserNotes)
	api.Match(chartMethods, "/charts/user/drive", chartsHandler.UserDrive)
	api.Match(chartMethods, "/charts/user/following", chartsHandler.UserFollowing)
	api.Match(chartMethods, "/charts/user/pv", chartsHandler.UserPv)
	api.Match(chartMethods, "/charts/user/reactions", chartsHandler.UserReactions)

	// retention — リテンション統計
	api.POST("/retention", chartsHandler.Retention)
	// 本家は allowGet: true (#3593)。
	api.GET("/retention", chartsHandler.Retention)
}
