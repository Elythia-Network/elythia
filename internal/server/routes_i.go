package server

import (
	"time"

	apiannouncements "github.com/elythia-network/elythia/internal/api/announcements"
	apiflash "github.com/elythia-network/elythia/internal/api/flash"
	"github.com/elythia-network/elythia/internal/api/i"
	"github.com/elythia-network/elythia/internal/api/notifications"
	"github.com/elythia-network/elythia/internal/api/pages"
	apiwebhooks "github.com/elythia-network/elythia/internal/api/webhooks"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerIRoutes registers the routes of the signed-in account (i/* and
// verify-email) on the /api group.
func registerIRoutes(api apiRoutes, iHandler *i.Handler, webhookHandler *apiwebhooks.Handler, notificationsHandler *notifications.Handler, pagesHandler *pages.Handler, flashHandler *apiflash.Handler, announcementHandler *apiannouncements.Handler, roleService *corerole.Service, rateLimiter *middleware.RateLimiter) {
	api.POST("/i", iHandler.Me, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/update", iHandler.Update, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/federation", iHandler.Federation, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/update-federation", iHandler.UpdateFederation, middleware.RequireAuth(), middleware.RequireScope("write:account"), middleware.RequireSecure(), rateLimiter.ForEndpoint("i/update-federation", &middleware.EndpointLimit{Duration: time.Hour, Max: 20}))
	api.POST("/i/pin", iHandler.Pin, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/i/unpin", iHandler.Unpin, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/registry/get", iHandler.RegistryGet, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/set", iHandler.RegistrySet, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/registry/get-all", iHandler.RegistryGetAll, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/keys-with-type", iHandler.RegistryKeysWithType, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/remove", iHandler.RegistryRemove, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/change-password", iHandler.ChangePassword, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/delete-account", iHandler.DeleteAccount, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/favorites", iHandler.Favorites, middleware.RequireAuth(), middleware.RequireScope("read:favorites"))
	api.POST("/i/regenerate-token", iHandler.RegenerateToken, middleware.RequireAuth(), middleware.RequireSecure())

	api.POST("/i/export-notes", iHandler.ExportNotes, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-following", iHandler.ExportFollowing, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-blocking", iHandler.ExportBlocking, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-mute", iHandler.ExportMute, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-favorites", iHandler.ExportFavorites, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-user-lists", iHandler.ExportUserLists, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-antennas", iHandler.ExportAntennas, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-clips", iHandler.ExportClips, middleware.RequireAuth(), middleware.RequireSecure())
	// i/import-* の canImport* role policy gate は #1020 で追加。policy=false
	// の user (default 全 false) は 403 ROLE_PERMISSION_DENIED を返す。
	api.POST("/i/import-following", iHandler.ImportFollowing,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportFollowing), middleware.RequireSecure())
	api.POST("/i/import-blocking", iHandler.ImportBlocking,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportBlocking), middleware.RequireSecure())
	api.POST("/i/import-muting", iHandler.ImportMuting,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportMuting), middleware.RequireSecure())
	api.POST("/i/import-user-lists", iHandler.ImportUserLists,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportUserLists), middleware.RequireSecure())
	api.POST("/i/import-antennas", iHandler.ImportAntennas,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportAntennas), middleware.RequireSecure())

	api.POST("/i/webhooks/create", webhookHandler.Create, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/webhooks/list", webhookHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/webhooks/show", webhookHandler.Show, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/webhooks/update", webhookHandler.Update, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/webhooks/delete", webhookHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/webhooks/test", webhookHandler.Test, middleware.RequireAuth(), middleware.RequireSecure(), middleware.RequireScope("read:account"))

	api.POST("/i/notifications", notificationsHandler.Show, middleware.RequireAuth(), middleware.RequireScope("read:notifications"))
	api.POST("/i/notifications-grouped", notificationsHandler.Grouped, middleware.RequireAuth(), middleware.RequireScope("read:notifications"))

	// i/* ハンドラ
	// claim-achievement / import-* は upstream で prohibitMoved:true (移行済
	// アカウントは YOUR_ACCOUNT_MOVED 403)。RequireNotMoved を配線する (#1555)。
	api.POST("/i/claim-achievement", iHandler.ClaimAchievement, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/i/apps", iHandler.Apps, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/authorized-apps", iHandler.AuthorizedApps, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/signin-history", iHandler.SigninHistory, middleware.RequireAuth(), middleware.RequireSecure())
	// upstream c07ce75281: サードパーティアプリが自身のアクセストークンを失効
	// できるよう secure を外した。**RequireAuth も付けない** — upstream は
	// requireCredential を外して endpoint 固有の CREDENTIAL_REQUIRED を投げるので、
	// 汎用 middleware に任せると error id が食い違う。認証と「自分のトークン以外は
	// 403」はどちらも handler 側で見る。
	// どの権限のapp tokenでも自分自身は失効できるよう、HideAppUserWithoutReadが
	// 外した利用者をAdmitAppUserで戻す。
	api.POST("/i/revoke-token", iHandler.RevokeToken, middleware.AdmitAppUser())
	api.POST("/i/update-email", iHandler.UpdateEmail, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/verify-email", iHandler.VerifyEmail)
	api.POST("/i/move", iHandler.Move, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireSecure())
	api.POST("/i/2fa/register", iHandler.TwoFARegister, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/done", iHandler.TwoFADone, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/unregister", iHandler.TwoFAUnregister, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/register-key", iHandler.TwoFARegisterKey, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/key-done", iHandler.TwoFAKeyDone, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/remove-key", iHandler.TwoFARemoveKey, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/update-key", iHandler.TwoFAUpdateKey, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/password-less", iHandler.TwoFAPasswordLess, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/gallery/likes", iHandler.GalleryLikes, middleware.RequireAuth(), middleware.RequireScope("read:gallery-likes"))
	api.POST("/i/gallery/posts", iHandler.GalleryPosts, middleware.RequireAuth(), middleware.RequireScope("read:gallery"))
	api.POST("/i/page-likes", iHandler.PageLikes, middleware.RequireAuth(), middleware.RequireScope("read:page-likes"))
	api.POST("/i/registry/get-detail", iHandler.RegistryGetDetail, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/keys", iHandler.RegistryKeys, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/scopes-with-domain", iHandler.RegistryScopesWithDomain, middleware.RequireAuth(), middleware.RequireSecure())

	api.POST("/i/pages", pagesHandler.My, middleware.RequireAuth(), middleware.RequireScope("read:pages"))

	api.POST("/i/flashs", flashHandler.My, middleware.RequireAuth(), middleware.RequireScope("read:flash"))
	api.POST("/i/flashs/likes", flashHandler.MyLikes, middleware.RequireAuth(), middleware.RequireScope("read:flash-likes"))

	api.POST("/i/read-announcement", announcementHandler.ReadAnnouncement, middleware.RequireAuth(), middleware.RequireScope("write:account"))
}
