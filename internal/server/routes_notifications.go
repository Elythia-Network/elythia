package server

import (
	"github.com/elythia-network/elythia/internal/api/notifications"
	apisw "github.com/elythia-network/elythia/internal/api/sw"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerNotificationsRoutes registers the notifications and push
// subscription routes (notifications/* and sw/*) on the /api group.
func registerNotificationsRoutes(api apiRoutes, notificationsHandler *notifications.Handler, swHandler *apisw.Handler) {
	api.POST("/notifications/mark-all-as-read", notificationsHandler.MarkAllAsRead, middleware.RequireAuth(), middleware.RequireScope("write:notifications"))
	api.POST("/notifications/create", notificationsHandler.Create, middleware.RequireAuth(), middleware.RequireScope("write:notifications"))
	api.POST("/notifications/flush", notificationsHandler.Flush, middleware.RequireAuth(), middleware.RequireScope("write:notifications"))
	api.POST("/notifications/test-notification", notificationsHandler.TestNotification, middleware.RequireAuth(), middleware.RequireScope("write:notifications"))

	api.POST("/sw/register", swHandler.Register, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/sw/show-registration", swHandler.ShowRegistration, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/sw/update-registration", swHandler.UpdateRegistration, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/sw/unregister", swHandler.Unregister)
}
