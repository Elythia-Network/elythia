package server

import (
	apigallery "github.com/elythia-network/elythia/internal/api/gallery"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerGalleryRoutes registers the gallery routes on the /api group.
func registerGalleryRoutes(api apiRoutes, galleryHandler *apigallery.Handler) {
	api.POST("/gallery/featured", galleryHandler.Featured)
	api.POST("/gallery/popular", galleryHandler.Popular)
	api.POST("/gallery/posts", galleryHandler.Posts)
	api.POST("/gallery/posts/create", galleryHandler.PostsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:gallery"))
	api.POST("/gallery/posts/show", galleryHandler.PostsShow)
	api.POST("/gallery/posts/delete", galleryHandler.PostsDelete, middleware.RequireAuth(), middleware.RequireScope("write:gallery"))
	api.POST("/gallery/posts/update", galleryHandler.PostsUpdate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:gallery"))
	api.POST("/gallery/posts/like", galleryHandler.PostsLike, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:gallery-likes"))
	api.POST("/gallery/posts/unlike", galleryHandler.PostsUnlike, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:gallery-likes"))
}
