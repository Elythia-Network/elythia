package server

import (
	apiannouncements "github.com/elythia-network/elythia/internal/api/announcements"
)

// registerAnnouncementsRoutes registers the announcements routes on the /api
// group.
func registerAnnouncementsRoutes(api apiRoutes, announcementHandler *apiannouncements.Handler) {
	api.POST("/announcements", announcementHandler.List)

	// announcements/show — 個別アナウンスメント取得
	api.POST("/announcements/show", announcementHandler.Show)
}
