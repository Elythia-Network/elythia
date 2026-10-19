package server

import (
	apichannels "github.com/elythia-network/elythia/internal/api/channels"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerChannelsRoutes registers the channels routes on the /api group.
func registerChannelsRoutes(api apiRoutes, channelsHandler *apichannels.Handler, roleService *corerole.Service) {
	// channels/create の canCreateChannel gate は #1020 で middleware に
	// 昇格 (handler 内 RolePolicyChecker → middleware.RequireRolePolicy)。
	api.POST("/channels/create", channelsHandler.Create,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanCreateChannel), middleware.RequireScope("write:channels"))
	api.POST("/channels/show", channelsHandler.Show)
	api.POST("/channels/update", channelsHandler.Update, middleware.RequireAuth(), middleware.RequireScope("write:channels"))
	api.POST("/channels/follow", channelsHandler.Follow, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/unfollow", channelsHandler.Unfollow, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/followed", channelsHandler.Followed, middleware.RequireAuth(), middleware.RequireScope("read:channels"))
	api.POST("/channels/owned", channelsHandler.Owned, middleware.RequireAuth(), middleware.RequireScope("read:channels"))
	api.POST("/channels/featured", channelsHandler.Featured)
	api.POST("/channels/search", channelsHandler.Search)
	api.POST("/channels/timeline", channelsHandler.Timeline)

	// channels 残り
	api.POST("/channels/favorite", channelsHandler.Favorite, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/unfavorite", channelsHandler.Unfavorite, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/mute/create", channelsHandler.MuteCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/mute/delete", channelsHandler.MuteDelete, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/my-favorites", channelsHandler.MyFavorites, middleware.RequireAuth(), middleware.RequireScope("read:channels"))
	api.POST("/channels/mute/list", channelsHandler.MuteList, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("read:channels"))
}
