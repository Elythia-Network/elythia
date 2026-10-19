package server

import (
	apibubblegame "github.com/elythia-network/elythia/internal/api/bubblegame"
	apireversi "github.com/elythia-network/elythia/internal/api/reversi"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerGamesRoutes registers the reversi and bubble game routes on the
// /api group.
func registerGamesRoutes(api apiRoutes, reversiHandler *apireversi.Handler, bubbleGameHandler *apibubblegame.Handler, bubbleVersusHandler *apibubblegame.VersusHandler) {
	api.POST("/reversi/games", reversiHandler.Games)
	api.POST("/reversi/invitations", reversiHandler.Invitations, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/reversi/show-game", reversiHandler.ShowGame)
	api.POST("/reversi/match", reversiHandler.Match, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/reversi/cancel-match", reversiHandler.CancelMatch, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/reversi/surrender", reversiHandler.Surrender, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/reversi/verify", reversiHandler.Verify)

	// bubble-game/* — バブルゲーム (実データ)
	api.POST("/bubble-game/register", bubbleGameHandler.Register, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	// ranking は upstream ranking.ts が allowGet:true。frontend drop-and-fusion.vue は
	// misskeyApiGet で GET を投げるため GET+POST を二重 wire する (hashtags/trend や
	// fetch-rss と同様、#1774)。
	api.POST("/bubble-game/ranking", bubbleGameHandler.Ranking)
	api.GET("/bubble-game/ranking", bubbleGameHandler.Ranking)

	api.POST("/bubble-game/versus/invite", bubbleVersusHandler.Invite, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/bubble-game/versus/invitations", bubbleVersusHandler.Invitations, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/bubble-game/versus/show", bubbleVersusHandler.Show, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/bubble-game/versus/accept", bubbleVersusHandler.Accept, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/bubble-game/versus/decline", bubbleVersusHandler.Decline, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/bubble-game/versus/cancel", bubbleVersusHandler.Cancel, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/bubble-game/versus/report", bubbleVersusHandler.Report, middleware.RequireAuth(), middleware.RequireScope("write:account"))

	api.POST("/bubble-game/versus/history", bubbleVersusHandler.History, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/bubble-game/versus/record", bubbleVersusHandler.ShowRecord, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/bubble-game/versus/set-public", bubbleVersusHandler.SetPublic, middleware.RequireAuth(), middleware.RequireScope("write:account"))
}
