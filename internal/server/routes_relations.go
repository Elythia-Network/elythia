package server

import (
	"github.com/elythia-network/elythia/internal/api/blocking"
	"github.com/elythia-network/elythia/internal/api/following"
	"github.com/elythia-network/elythia/internal/api/mute"
	"github.com/elythia-network/elythia/internal/api/renotemute"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerRelationsRoutes registers the user relation routes (following/*,
// blocking/*, mute/* and renote-mute/*) on the /api group.
func registerRelationsRoutes(api apiRoutes, blockingHandler *blocking.Handler, muteHandler *mute.Handler, renoteMuteHandler *renotemute.Handler, followingHandler *following.Handler) {
	api.POST("/blocking/create", blockingHandler.Create, middleware.RequireAuth(), middleware.RequireScope("write:blocks"))
	api.POST("/blocking/delete", blockingHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:blocks"))
	api.POST("/blocking/list", blockingHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:blocks"))

	api.POST("/mute/create", muteHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:mutes"))
	api.POST("/mute/delete", muteHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:mutes"))
	api.POST("/mute/list", muteHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:mutes"))

	api.POST("/renote-mute/create", renoteMuteHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:mutes"))
	api.POST("/renote-mute/delete", renoteMuteHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:mutes"))
	api.POST("/renote-mute/list", renoteMuteHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:mutes"))

	api.POST("/following/create", followingHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:following"))
	api.POST("/following/delete", followingHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/list", followingHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:following"))
	api.POST("/following/requests/list", followingHandler.ListRequests, middleware.RequireAuth(), middleware.RequireScope("read:following"))
	api.POST("/following/requests/accept", followingHandler.AcceptRequest, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/requests/reject", followingHandler.RejectRequest, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/requests/cancel", followingHandler.CancelRequest, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	// following 残り
	api.POST("/following/invalidate", followingHandler.Invalidate, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/update", followingHandler.UpdateFollow, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/update-all", followingHandler.UpdateFollowAll, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/requests/sent", followingHandler.RequestsSent, middleware.RequireAuth(), middleware.RequireScope("read:following"))
	// Elythia 独自 (#3466): 通知を出さずに成立させたフォローの一覧。
	api.POST("/following/silent/list", followingHandler.ListSilent, middleware.RequireAuth(), middleware.RequireScope("read:following"))
}
