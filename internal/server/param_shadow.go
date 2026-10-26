package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/server/paramshadow"
)

// paramShadowContextKey is the echo.Context key under which the /api group
// hands the param shadow validation (#3677) to withParamShadow.
const paramShadowContextKey = "mk.server.paramShadow"

// paramEnforcedEndpoints are the upstream endpoints whose params are
// validated with upstream's paramDef and rejected with upstream's 400
// (clips/* in #3742, the first step of #3667; charts/* in #3744; hashtags/*
// in #3746; antennas/* in #3748; flash/* in #3750; gallery/* in #3754;
// following/* in #3756; channels/* in #3758; blocking/*, mute/* and
// renote-mute/* in #3760; roles/*, invite/* and notifications/* in #3762;
// app/*, auth/*, miauth/gen-token and my/apps in #3764; sw/* and
// announcements in #3766; promo/read, email-address/available,
// username/available, ap/*, bubble-game/ranking and bubble-game/register in
// #3768; federation/* except federation/instances in #3770; pages/* and
// page-push in #3772; the read-only top-level endpoints in #3774; users and
// users/* in #3837; drive and drive/* in #3839; chat/* in #3841; reversi/* except reversi/match in #3843; export-custom-emojis,
// fetch-external-resources, request-reset-password, reset-password,
// verify-email and v2/admin/emoji/list in #3845; notes and notes/* except
// notes/create in #3847; i and i/* except i/update, i/notifications and
// i/notifications-grouped in #3851; admin/ad/*, admin/announcements/*,
// admin/avatar-decorations/*, admin/invite/*, admin/promo/create, admin/meta,
// admin/server-info, admin/get-index-stats, admin/get-table-stats and
// admin/show-moderation-logs in #3854).
// The other upstream endpoints are only validated and counted (#3677).
//
// 本家に揃える対象はパッケージ単位で広げる(#3667)。足したら、そのendpoint
// で本家と判定が分かれる入力を、手書きの検査が拒否していないか(本家が通す
// のに400にしていないか)を確かめる。TestEnforcedEndpointsMatchTheCorpusが
// ajvのcorpusで確かめる。
var paramEnforcedEndpoints = []string{
	"clips/add-note",
	"clips/create",
	"clips/delete",
	"clips/favorite",
	"clips/list",
	"clips/my-favorites",
	"clips/notes",
	"clips/remove-note",
	"clips/show",
	"clips/unfavorite",
	"clips/update",
	"charts/active-users",
	"charts/ap-request",
	"charts/drive",
	"charts/federation",
	"charts/instance",
	"charts/notes",
	"charts/user/drive",
	"charts/user/following",
	"charts/user/notes",
	"charts/user/pv",
	"charts/user/reactions",
	"charts/users",
	"hashtags/list",
	"hashtags/search",
	"hashtags/show",
	"hashtags/trend",
	"hashtags/users",
	"antennas/create",
	"antennas/delete",
	"antennas/list",
	"antennas/notes",
	"antennas/remove-note",
	"antennas/show",
	"antennas/update",
	"flash/create",
	"flash/delete",
	"flash/featured",
	"flash/like",
	"flash/my",
	"flash/my-likes",
	"flash/search",
	"flash/show",
	"flash/unlike",
	"flash/update",
	"gallery/featured",
	"gallery/popular",
	"gallery/posts",
	"gallery/posts/create",
	"gallery/posts/delete",
	"gallery/posts/like",
	"gallery/posts/show",
	"gallery/posts/unlike",
	"gallery/posts/update",
	"following/create",
	"following/delete",
	"following/invalidate",
	"following/list",
	"following/requests/accept",
	"following/requests/cancel",
	"following/requests/list",
	"following/requests/reject",
	"following/requests/sent",
	"following/update",
	"following/update-all",
	"channels/create",
	"channels/favorite",
	"channels/featured",
	"channels/follow",
	"channels/followed",
	"channels/mute/create",
	"channels/mute/delete",
	"channels/mute/list",
	"channels/my-favorites",
	"channels/owned",
	"channels/search",
	"channels/show",
	"channels/timeline",
	"channels/unfavorite",
	"channels/unfollow",
	"channels/update",
	"blocking/create",
	"blocking/delete",
	"blocking/list",
	"mute/create",
	"mute/delete",
	"mute/list",
	"renote-mute/create",
	"renote-mute/delete",
	"renote-mute/list",
	"roles/list",
	"roles/notes",
	"roles/show",
	"roles/users",
	"invite/create",
	"invite/delete",
	"invite/limit",
	"invite/list",
	"notifications/create",
	"notifications/flush",
	"notifications/mark-all-as-read",
	"notifications/test-notification",
	"app/create",
	"app/show",
	"auth/accept",
	"auth/session/generate",
	"auth/session/show",
	"auth/session/userkey",
	"miauth/gen-token",
	"my/apps",
	"sw/register",
	"sw/show-registration",
	"sw/unregister",
	"sw/update-registration",
	"announcements",
	"announcements/show",
	"promo/read",
	"email-address/available",
	"username/available",
	"ap/get",
	"ap/show",
	"bubble-game/ranking",
	"bubble-game/register",
	// federation/instancesは、独自のsortの値(+host / -host)が本家のenumに
	// 無いので、まだ載せない(#3770)。
	"federation/followers",
	"federation/following",
	"federation/show-instance",
	"federation/stats",
	"federation/update-remote-user",
	"federation/users",
	"page-push",
	"pages/create",
	"pages/delete",
	"pages/featured",
	"pages/like",
	"pages/show",
	"pages/unlike",
	"pages/update",
	// どのパッケージにも属さない、参照だけのendpoint(#3774)。
	"emoji",
	"emojis",
	"endpoint",
	"endpoints",
	"fetch-rss",
	"get-avatar-decorations",
	"get-online-users-count",
	"meta",
	"ping",
	"pinned-users",
	"retention",
	"server-info",
	"stats",
	"test",
	"users",
	"users/achievements",
	"users/clips",
	"users/featured-notes",
	"users/flashs",
	"users/followers",
	"users/following",
	"users/gallery/posts",
	"users/get-following-users-by-birthday",
	"users/get-frequently-replied-users",
	"users/lists/create",
	"users/lists/create-from-public",
	"users/lists/delete",
	"users/lists/favorite",
	"users/lists/get-memberships",
	"users/lists/list",
	"users/lists/pull",
	"users/lists/push",
	"users/lists/show",
	"users/lists/unfavorite",
	"users/lists/update",
	"users/lists/update-membership",
	"users/notes",
	"users/pages",
	"users/reactions",
	"users/recommendation",
	"users/relation",
	"users/report-abuse",
	"users/search",
	"users/search-by-username-and-host",
	"users/show",
	"users/update-memo",
	"drive",
	"drive/files",
	"drive/files/attached-chat-messages",
	"drive/files/attached-notes",
	"drive/files/check-existence",
	"drive/files/create",
	"drive/files/delete",
	"drive/files/find",
	"drive/files/find-by-hash",
	"drive/files/move-bulk",
	"drive/files/show",
	"drive/files/update",
	"drive/files/upload-from-url",
	"drive/folders",
	"drive/folders/create",
	"drive/folders/delete",
	"drive/folders/find",
	"drive/folders/show",
	"drive/folders/update",
	"drive/stream",
	"chat/history",
	"chat/messages/create-to-room",
	"chat/messages/create-to-user",
	"chat/messages/delete",
	"chat/messages/react",
	"chat/messages/room-timeline",
	"chat/messages/search",
	"chat/messages/show",
	"chat/messages/unreact",
	"chat/messages/user-timeline",
	"chat/read-all",
	"chat/rooms/create",
	"chat/rooms/delete",
	"chat/rooms/invitations/create",
	"chat/rooms/invitations/ignore",
	"chat/rooms/invitations/inbox",
	"chat/rooms/invitations/outbox",
	"chat/rooms/join",
	"chat/rooms/joining",
	"chat/rooms/leave",
	"chat/rooms/members",
	"chat/rooms/mute",
	"chat/rooms/owned",
	"chat/rooms/show",
	"chat/rooms/update",
	"reversi/cancel-match",
	"reversi/games",
	"reversi/invitations",
	"reversi/show-game",
	"reversi/surrender",
	"reversi/verify",
	"export-custom-emojis",
	"fetch-external-resources",
	"request-reset-password",
	"reset-password",
	"verify-email",
	"v2/admin/emoji/list",
	"notes",
	"notes/children",
	"notes/clips",
	"notes/conversation",
	"notes/delete",
	"notes/drafts/count",
	"notes/drafts/create",
	"notes/drafts/delete",
	"notes/drafts/list",
	"notes/drafts/update",
	"notes/favorites/create",
	"notes/favorites/delete",
	"notes/featured",
	"notes/global-timeline",
	"notes/hybrid-timeline",
	"notes/local-timeline",
	"notes/mentions",
	"notes/polls/recommendation",
	"notes/polls/vote",
	"notes/reactions",
	"notes/reactions/create",
	"notes/reactions/delete",
	"notes/renotes",
	"notes/replies",
	"notes/search",
	"notes/search-by-tag",
	"notes/show",
	"notes/show-partial-bulk",
	"notes/state",
	"notes/thread-muting/create",
	"notes/thread-muting/delete",
	"notes/timeline",
	"notes/translate",
	"notes/unrenote",
	"notes/user-list-timeline",
	"i",
	"i/2fa/done",
	"i/2fa/key-done",
	"i/2fa/password-less",
	"i/2fa/register",
	"i/2fa/register-key",
	"i/2fa/remove-key",
	"i/2fa/unregister",
	"i/2fa/update-key",
	"i/apps",
	"i/authorized-apps",
	"i/change-password",
	"i/claim-achievement",
	"i/delete-account",
	"i/export-antennas",
	"i/export-blocking",
	"i/export-clips",
	"i/export-favorites",
	"i/export-following",
	"i/export-mute",
	"i/export-notes",
	"i/export-user-lists",
	"i/favorites",
	"i/gallery/likes",
	"i/gallery/posts",
	"i/import-antennas",
	"i/import-blocking",
	"i/import-following",
	"i/import-muting",
	"i/import-user-lists",
	"i/move",
	"i/page-likes",
	"i/pages",
	"i/pin",
	"i/read-announcement",
	"i/regenerate-token",
	"i/registry/get",
	"i/registry/get-all",
	"i/registry/get-detail",
	"i/registry/keys",
	"i/registry/keys-with-type",
	"i/registry/remove",
	"i/registry/scopes-with-domain",
	"i/registry/set",
	"i/revoke-token",
	"i/signin-history",
	"i/unpin",
	"i/update-email",
	"i/webhooks/create",
	"i/webhooks/delete",
	"i/webhooks/list",
	"i/webhooks/show",
	"i/webhooks/test",
	"i/webhooks/update",
	"admin/ad/create",
	"admin/ad/delete",
	"admin/ad/list",
	"admin/ad/update",
	"admin/announcements/create",
	"admin/announcements/delete",
	"admin/announcements/list",
	"admin/announcements/update",
	"admin/avatar-decorations/create",
	"admin/avatar-decorations/delete",
	"admin/avatar-decorations/list",
	"admin/avatar-decorations/update",
	"admin/invite/create",
	"admin/invite/list",
	"admin/promo/create",
	"admin/get-index-stats",
	"admin/get-table-stats",
	"admin/meta",
	"admin/server-info",
	"admin/show-moderation-logs",
}

// paramShadowMiddleware returns the /api group middleware that enables the
// param validation for the request: rejecting for paramEnforcedEndpoints,
// and counting only (the param shadow validation) for the other upstream
// endpoints unless disableParamShadow. The validation itself runs in
// withParamShadow, at the end of the route's middleware. It returns nil only
// when the embedded upstream paramDefs do not load; the handlers' own checks
// then stay the only ones.
//
// It also adds the part paramShadowRecorder (#3699), whose stop writes the
// counts of this server generation.
func (s *Server) paramShadowMiddleware() echo.MiddlewareFunc {
	eps, err := paramschema.Upstream()
	if err != nil {
		slog.Warn("param validation disabled: the embedded upstream paramDefs do not load", "err", err)
		return nil
	}
	opts := []paramshadow.Option{paramshadow.WithEnforced(paramEnforcedEndpoints...)}
	if s.config.DisableParamShadow {
		// 止めるのは数えるだけの検証。本家に揃えた分の拒否は止めない。記録は
		// 検証器のpanicとcompileの失敗だけになる。
		opts = append(opts, paramshadow.WithoutShadow())
		slog.Info("param shadow validation disabled (disableParamShadow); the enforced endpoints are still validated",
			"enforced", len(paramEnforcedEndpoints))
	}
	for name, ep := range eps {
		if ep.CompileErr != nil {
			slog.Warn("param validation: an upstream paramDef does not compile; its requests go to the handler and are counted as compile_error",
				"endpoint", name, "err", ep.CompileErr)
		}
	}
	interval := time.Duration(s.config.ParamShadowLogSeconds) * time.Second
	rec := paramshadow.NewRecorder(slog.Default(), interval)
	// 定期の要約は、リクエストのgoroutineではなく世代の箱(bgwork)で書く。
	// 箱が止まった後(停止の途中)は、下のhookが最後の分を書く。
	rec.SetAsync(func(flush func()) bool {
		// 箱が止まり始めていたら任せない(Recordが自分で書く)。GoAsに渡すと、
		// 捨てていないのに「捨てた」と警告し、Droppedを数えてしまう(#3677の
		// レビュー2周目L-1)。見てからGoAsまでの間に止まり始めた場合だけは残る。
		select {
		case <-s.background.Stopping():
			return false
		default:
		}
		return s.background.GoAs("paramshadow summary", func(context.Context) { flush() })
	})
	// 停止のときに、最後の要約からの分を出す。メンテナンスで作り直す世代も
	// それぞれが自分の分を出す。
	s.addCloser("paramShadowRecorder", rec)
	shadow := paramshadow.Middleware(eps, rec, opts...)
	if !s.config.DisableParamShadow {
		slog.Info("param shadow validation enabled", "endpoints", len(eps), "enforced", len(paramEnforcedEndpoints), "logInterval", interval.String())
	}
	return paramValidationGroupMiddleware(shadow)
}

// paramValidationGroupMiddleware hands shadow to withParamShadow.
func paramValidationGroupMiddleware(shadow echo.MiddlewareFunc) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(paramShadowContextKey, shadow)
			return next(c)
		}
	}
}

// withParamShadow runs the param shadow validation around next when the /api
// group enabled it for the request. apiRoutes appends it after the route's
// own middleware, so it sees the requests that passed the credential and
// permission checks: the requests upstream hands to ajv.
//
// **routeのmiddlewareの末尾で走らせる。** groupのmiddlewareはrouteの認証・
// 権限の検査より先に走るので、そこで検証すると、本家ならajvに届かない
// リクエスト(401 / 403になるもの)まで数えてしまう。
func withParamShadow(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if shadow, ok := c.Get(paramShadowContextKey).(echo.MiddlewareFunc); ok {
			return shadow(next)(c)
		}
		return next(c)
	}
}
