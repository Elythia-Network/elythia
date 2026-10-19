package server

import (
	apihashtags "github.com/elythia-network/elythia/internal/api/hashtags"
)

// registerHashtagsRoutes registers the hashtags routes on the /api group.
func registerHashtagsRoutes(api apiRoutes, hashtagsHandler *apihashtags.Handler) {
	api.POST("/hashtags/list", hashtagsHandler.List)
	api.POST("/hashtags/search", hashtagsHandler.Search)
	api.POST("/hashtags/show", hashtagsHandler.Show)
	api.POST("/hashtags/trend", hashtagsHandler.Trend)
	// upstream Misskey の hashtags/trend は allowGet: true なので
	// frontend (misskeyApiGet) は GET で叩く。WidgetTrends がここを毎分
	// 呼ぶため GET でも応答できるよう同じ handler を 2 重 wire する。
	api.GET("/hashtags/trend", hashtagsHandler.Trend)
	api.POST("/hashtags/users", hashtagsHandler.Users)
}
