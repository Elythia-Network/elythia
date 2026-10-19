package server

import (
	"github.com/elythia-network/elythia/internal/api/ap"
	apifederation "github.com/elythia-network/elythia/internal/api/federation"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerFederationRoutes registers the federation routes (federation/*,
// v1/instance/peers and ap/*) on the /api group.
func registerFederationRoutes(api apiRoutes, federationHandler *apifederation.Handler, apHandler *ap.Handler, roleService *corerole.Service) {
	api.POST("/federation/instances", federationHandler.Instances)
	api.GET("/federation/instances", federationHandler.Instances)
	api.POST("/federation/show-instance", federationHandler.ShowInstance)
	api.POST("/federation/followers", federationHandler.Followers)
	api.POST("/federation/following", federationHandler.Following)
	api.POST("/federation/users", federationHandler.Users)
	// admin overview から `misskeyApiGet` で叩かれるため GET も登録 (#421)。
	api.POST("/federation/stats", federationHandler.Stats)
	api.GET("/federation/stats", federationHandler.Stats)
	// upstream 2026.9.1 で `requireCredential: true` / `kind: 'read:account'` /
	// 1 時間 30 回になった (それまでは未認証でも叩けた)。mk-go は #2106 で先に
	// RequireAuth を付けていたので、scope とレート制限 (ratelimit_defs.go) を足して
	// 揃える。RequireModerator にしないのは、frontend がリモートユーザーの
	// メニューで全ログインユーザーに出すため。
	api.POST("/federation/update-remote-user", federationHandler.UpdateRemoteUser, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	// Mastodon 互換のピア一覧 (#2245)。upstream は endpoints/ 配下ではなく
	// ApiServerService.ts で fastify に直接登録しているので、mk-go でも
	// endpoint 群とは別に /api/v1/... として生やす。
	api.GET("/v1/instance/peers", federationHandler.Peers)

	// ap/* — ActivityPub API lookup (実データ: ローカルオブジェクト解決)
	// /api/ap/notes は upstream Misskey にも存在しない vestigial route だった
	// ため #587 で削除済 (旧実装は常に空配列返却の stub)。
	api.POST("/ap/get", apHandler.APIGet, middleware.RequireAdmin(roleService), middleware.RequireScope("read:federation"))
	api.POST("/ap/show", apHandler.APIShow, middleware.RequireAuth(), middleware.RequireScope("read:account"))
}
