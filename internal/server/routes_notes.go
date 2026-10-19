package server

import (
	"github.com/elythia-network/elythia/internal/api/notes"
	"github.com/elythia-network/elythia/internal/api/promo"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerNotesRoutes registers the notes routes (notes/*, the bulk /notes
// and promo/read) on the /api group.
func registerNotesRoutes(api apiRoutes, notesHandler *notes.Handler, promoHandler *promo.Handler) {
	api.POST("/notes/create", notesHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:notes"))
	api.POST("/notes/show", notesHandler.Show)
	api.POST("/notes/delete", notesHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:notes"))
	api.POST("/notes/renotes", notesHandler.Renotes)
	api.POST("/notes/replies", notesHandler.Replies)
	api.POST("/notes/children", notesHandler.Children)
	api.POST("/notes/conversation", notesHandler.Conversation)
	// upstream search.ts は requireCredential:false で匿名も handler に到達させ、
	// canSearchNotes policy を handler 内で検査して UNAVAILABLE を返す (#1783)。
	// route 側の RequireAuth + RequireRolePolicy は外す。
	api.POST("/notes/search", notesHandler.Search)
	api.POST("/notes/state", notesHandler.State, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/timeline", notesHandler.Timeline, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/local-timeline", notesHandler.LocalTimeline)
	api.POST("/notes/global-timeline", notesHandler.GlobalTimeline)
	api.POST("/notes/hybrid-timeline", notesHandler.HybridTimeline, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/reactions", notesHandler.Reactions)
	api.GET("/notes/reactions", notesHandler.Reactions)
	api.POST("/notes/reactions/create", notesHandler.ReactionsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:reactions"))
	api.POST("/notes/reactions/delete", notesHandler.ReactionsDelete, middleware.RequireAuth(), middleware.RequireScope("write:reactions"))
	api.POST("/notes/polls/vote", notesHandler.PollsVote, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:votes"))

	api.POST("/notes/favorites/create", notesHandler.FavoritesCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:favorites"))
	api.POST("/notes/favorites/delete", notesHandler.FavoritesDelete, middleware.RequireAuth(), middleware.RequireScope("write:favorites"))
	api.POST("/notes/featured", notesHandler.Featured)
	api.GET("/notes/featured", notesHandler.Featured)
	api.POST("/notes/unrenote", notesHandler.Unrenote, middleware.RequireAuth(), middleware.RequireScope("write:notes"))
	api.POST("/notes/mentions", notesHandler.Mentions, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/user-list-timeline", notesHandler.UserListTimeline, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/search-by-tag", notesHandler.SearchByTag)
	api.POST("/notes/clips", notesHandler.Clips)
	// canUseTranslator は upstream translate.ts 同様 handler 側で評価し、拒否時は
	// 503 UNAVAILABLE を返す (RequireRolePolicy の 403 ではない、#2010)。
	api.POST("/notes/translate", notesHandler.Translate,
		middleware.RequireAuth(),
		middleware.RequireScope("read:account"))
	api.POST("/notes/show-partial-bulk", notesHandler.ShowPartialBulk)

	api.POST("/notes/drafts/list", notesHandler.DraftsList, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("read:account"))
	api.POST("/notes/drafts/create", notesHandler.DraftsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/notes/drafts/update", notesHandler.DraftsUpdate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/notes/drafts/delete", notesHandler.DraftsDelete, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/notes/drafts/count", notesHandler.DraftsCount, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("read:account"))
	api.POST("/notes/thread-muting/create", notesHandler.ThreadMutingCreate, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/notes/thread-muting/delete", notesHandler.ThreadMutingDelete, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/notes/polls/recommendation", notesHandler.PollsRecommendation, middleware.RequireAuth(), middleware.RequireScope("read:account"))

	// promo/read — プロモノートの既読マーク (認証必須)
	api.POST("/promo/read", promoHandler.Read,
		middleware.RequireAuth(), middleware.RequireScope("write:account"))

	// notes (plain) — bulk note lookup by noteIds
	api.POST("/notes", notesHandler.BulkShow)
}
