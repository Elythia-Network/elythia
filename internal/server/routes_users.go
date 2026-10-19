package server

import (
	apiuserlists "github.com/elythia-network/elythia/internal/api/userlists"
	"github.com/elythia-network/elythia/internal/api/users"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerUsersRoutes registers the users routes (users/*, pinned-users and
// get-online-users-count) on the /api group.
func registerUsersRoutes(api apiRoutes, usersHandler *users.Handler, userListHandler *apiuserlists.Handler, roleService *corerole.Service) {
	// Users endpoint (public) — ユーザー一覧
	api.POST("/users", usersHandler.List)

	// Pinned users (public)
	//
	// upstream pinned-users.ts は res が UserDetailed 配列で、各 pinnedUsers の
	// acct を Acct.parse して host?? IsNull() で local/remote 両方を検索する
	// (#1551)。mk-go も acct の host 部分を解決し UserDetailed で返す。
	api.POST("/pinned-users", usersHandler.PinnedUsers)

	api.POST("/users/show", usersHandler.Show)
	// upstream search.ts:15-16 は requireCredential:false + requiredRolePolicy:
	// canSearchUsers。canSearchUsers の base default は true なので匿名も検索でき、
	// false の role のみ 403 になる。RequireAuth(401) は付けない (#1784)。
	api.POST("/users/search", usersHandler.Search,
		middleware.RequireRolePolicyPublic(roleService, corerole.PolicyCanSearchUsers))
	api.POST("/users/notes", usersHandler.Notes)
	api.POST("/users/followers", usersHandler.Followers)
	api.POST("/users/following", usersHandler.Following)
	api.POST("/users/relation", usersHandler.Relation, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/users/report-abuse", usersHandler.ReportAbuse, middleware.RequireAuth(), middleware.RequireScope("write:report-abuse"))
	api.POST("/users/reactions", usersHandler.Reactions)
	api.POST("/users/featured-notes", usersHandler.FeaturedNotes)
	// 本家は allowGet: true (#3593)。
	api.GET("/users/featured-notes", usersHandler.FeaturedNotes)
	// upstream search-by-username-and-host.ts:13 は requireCredential:false かつ
	// requiredRolePolicy 無し (匿名・全 role に開放)。mk-go が独自に付けていた
	// RequireAuth + canSearchUsers gate を外して揃える (#1766)。
	api.POST("/users/search-by-username-and-host", usersHandler.SearchByUsernameAndHost)
	api.POST("/users/update-memo", usersHandler.UpdateMemo, middleware.RequireAuth(), middleware.RequireScope("write:account"))

	// Phase 7.3: users/* 完全化 (実データハンドラ)
	api.POST("/users/achievements", usersHandler.Achievements)
	api.POST("/users/clips", usersHandler.Clips)
	api.POST("/users/flashs", usersHandler.Flashs)
	api.POST("/users/gallery/posts", usersHandler.GalleryPosts)
	api.POST("/users/pages", usersHandler.Pages)
	api.POST("/users/get-frequently-replied-users", usersHandler.GetFrequentlyRepliedUsers)
	api.POST("/users/get-following-users-by-birthday", usersHandler.GetFollowingUsersByBirthday, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/users/recommendation", usersHandler.UserRecommendation, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	// get-memberships は requireCredential:false の public endpoint (#1550)。
	// RequireScope は app token のみ gate するため public でも残してよい。
	api.POST("/users/lists/get-memberships", usersHandler.ListsGetMemberships, middleware.RequireScope("read:account"))
	api.POST("/users/lists/create-from-public", usersHandler.ListsCreateFromPublic, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/users/lists/favorite", usersHandler.ListsFavorite, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/users/lists/unfavorite", usersHandler.ListsUnfavorite, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/users/lists/update", usersHandler.ListsUpdate, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/users/lists/update-membership", usersHandler.ListsUpdateMembership, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))

	// list は requireCredential:false の public endpoint (userId 指定で他人の
	// public list を閲覧可、#1550)。未認証 && userId 未指定は handler が INVALID_PARAM。
	api.POST("/users/lists/list", userListHandler.List, middleware.RequireScope("read:account"))
	api.POST("/users/lists/create", userListHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	// upstream users/lists/show.ts:16 は requireCredential:false (kind は token
	// 認証時のみ適用)。public list は匿名でも閲覧可能で、Show handler も
	// viewer==nil を許容する。RequireAuth を外し RequireScope のみ残す (匿名は
	// RequireScope を素通り、app token のみ scope を強制、#1766)。
	api.POST("/users/lists/show", userListHandler.Show, middleware.RequireScope("read:account"))
	api.POST("/users/lists/push", userListHandler.Push, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/users/lists/pull", userListHandler.Pull, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/users/lists/delete", userListHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:account"))

	// get-online-users-count — オンラインユーザー数。フロントが
	// `misskeyApiGet` で GET 呼び出ししても catchall に落ちないよう
	// 両メソッドを受ける (#421)。POST のみだと `count` が undefined になり
	// admin overview の「NaN 人」表示の原因になっていた。
	api.GET("/get-online-users-count", usersHandler.OnlineCount)
	api.POST("/get-online-users-count", usersHandler.OnlineCount)
}
