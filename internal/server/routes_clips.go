package server

import (
	"github.com/elythia-network/elythia/internal/api/clips"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerClipsRoutes registers the clips routes on the /api group.
func registerClipsRoutes(api apiRoutes, clipsHandler *clips.Handler) {
	api.POST("/clips/create", clipsHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/clips/show", clipsHandler.Show, middleware.RequireScope("read:account"))
	api.POST("/clips/update", clipsHandler.Update, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/clips/delete", clipsHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/clips/list", clipsHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/clips/add-note", clipsHandler.AddNote, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/clips/remove-note", clipsHandler.RemoveNote, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/clips/notes", clipsHandler.Notes, middleware.RequireScope("read:account"))

	// clips 残り
	api.POST("/clips/favorite", clipsHandler.Favorite, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:clip-favorite"))
	api.POST("/clips/unfavorite", clipsHandler.Unfavorite, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:clip-favorite"))
	api.POST("/clips/my-favorites", clipsHandler.MyFavorites, middleware.RequireAuth(), middleware.RequireScope("read:clip-favorite"))
}
