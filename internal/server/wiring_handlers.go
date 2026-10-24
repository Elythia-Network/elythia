package server

import (
	"github.com/elythia-network/elythia/internal/api/antennas"
	"github.com/elythia-network/elythia/internal/api/ap"
	apiapp "github.com/elythia-network/elythia/internal/api/app"
	apiauth "github.com/elythia-network/elythia/internal/api/auth"
	"github.com/elythia-network/elythia/internal/api/avatardecorations"
	"github.com/elythia-network/elythia/internal/api/blocking"
	apibubblegame "github.com/elythia-network/elythia/internal/api/bubblegame"
	apichannels "github.com/elythia-network/elythia/internal/api/channels"
	"github.com/elythia-network/elythia/internal/api/clips"
	"github.com/elythia-network/elythia/internal/api/drive"
	"github.com/elythia-network/elythia/internal/api/endpoints"
	apifederation "github.com/elythia-network/elythia/internal/api/federation"
	apiflash "github.com/elythia-network/elythia/internal/api/flash"
	"github.com/elythia-network/elythia/internal/api/following"
	apigallery "github.com/elythia-network/elythia/internal/api/gallery"
	apihashtags "github.com/elythia-network/elythia/internal/api/hashtags"
	"github.com/elythia-network/elythia/internal/api/i"
	apiinvite "github.com/elythia-network/elythia/internal/api/invite"
	"github.com/elythia-network/elythia/internal/api/mute"
	"github.com/elythia-network/elythia/internal/api/nodeinfo"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/api/notifications"
	"github.com/elythia-network/elythia/internal/api/pages"
	"github.com/elythia-network/elythia/internal/api/promo"
	"github.com/elythia-network/elythia/internal/api/renotemute"
	apiroles "github.com/elythia-network/elythia/internal/api/roles"
	apitest "github.com/elythia-network/elythia/internal/api/test"
	apiuserlists "github.com/elythia-network/elythia/internal/api/userlists"
	"github.com/elythia-network/elythia/internal/api/users"
	apiwebhooks "github.com/elythia-network/elythia/internal/api/webhooks"
	"github.com/elythia-network/elythia/internal/api/wellknown"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
)

// apiHandlers holds the API handlers setupRoutes builds through buildHandlers
// (#3694).
//
// Fields are named after the local variables setupRoutes has always used,
// without the Handler suffix, and keep the concrete type the constructor
// returns. Only handlers whose constructor just builds a value are listed:
// every constructor here stores its arguments in a new struct (nodeinfo also
// makes an empty cache map) and touches no DB, Redis, outbound client or log,
// starts no goroutine, registers no shutdown hook and writes no global. Their
// dependencies are limited to the repositories, the core services, the
// federation parts, server configuration, the Server's DB handle, echo
// instance and background work group, the ID generator, and the values
// setupRoutes builds before calling buildHandlers and passes in as arguments
// (#3817), and nothing else.
//
// A handler is listed only if building it at the position of usersHandler
// (where setupRoutes calls buildHandlers) adds no late setter edge: no setter
// on it takes a value setupRoutes defines between that position and the
// handler's old one. Handlers that depend on a part still built in
// setupRoutes (the search service, the federation processor, the stream
// manager, the chart engine and so on), take an outbound client, are built
// inside an if or inside another call, or are built before usersHandler stay
// in setupRoutes (docs/architecture.md 4.2).
//
// buildHandlers only constructs. The dependencies that #3817 turned into
// constructor arguments are passed here; every other setter on these handlers
// is still called at its old position in setupRoutes, and every route is still
// registered there.
type apiHandlers struct {
	users             *users.Handler
	i                 *i.Handler
	webhook           *apiwebhooks.Handler
	notifications     *notifications.Handler
	hashtags          *apihashtags.Handler
	gallery           *apigallery.Handler
	blocking          *blocking.Handler
	mute              *mute.Handler
	renoteMute        *renotemute.Handler
	drive             *drive.Handler
	ap                *ap.Handler
	wellknown         *wellknown.Handler
	nodeinfo          *nodeinfo.Handler
	federation        *apifederation.Handler
	channels          *apichannels.Handler
	antennas          *antennas.Handler
	clips             *clips.Handler
	pages             *pages.Handler
	flash             *apiflash.Handler
	following         *following.Handler
	roles             *apiroles.Handler
	userList          *apiuserlists.Handler
	auth              *apiauth.Handler
	app               *apiapp.Handler
	bubbleGame        *apibubblegame.Handler
	endpoints         *endpoints.Handler
	avatarDecorations *avatardecorations.Handler
	promo             *promo.Handler
	invite            *apiinvite.Handler
	echoTest          *apitest.Handler
}

// buildHandlers builds the API handlers from w.repos, coreSvcs and fedSvcs,
// in the order setupRoutes used to build them. It reads s.db, s.config,
// s.background and s.echo (the endpoints handler reads the echo routes when a
// request comes, not here, because not every route is registered yet).
// coreSvcs and fedSvcs are the values buildCore and buildFederation returned;
// they are passed in rather than read back from w.core and w.fed so that
// calling buildHandlers before them does not compile (docs/architecture.md
// 4.2).
//
// The remaining arguments are the values setupRoutes builds before calling
// buildHandlers that the handlers take as required constructor arguments
// (#3817): the note hider, the entity packer, the user packer shared with the
// follow webhooks, the cached meta repository, and the read-time lookups of
// the abuseReport and signupApplicationReceived notifications. A dependency
// whose absence degrades a handler silently is a constructor argument, so
// leaving it out does not compile, where it used to be a setter that the
// startup check (recordCriticalWiring) had to catch.
func (s *Server) buildHandlers(
	w *wiring,
	coreSvcs core,
	fedSvcs fed,
	idGen id.Generator,
	noteHider *notehide.Hider,
	entityPacker *entity.Packer,
	sharedUserPacker *userpack.Packer,
	metaRepo repository.MetaRepository,
	abuseNotifStates notifications.AbuseReportStateLookup,
	abuseUnresolvedCount func() (int64, error),
	signupApplicationNotifLookup entity.SignupApplicationLookup,
) apiHandlers {
	// 下の構築の行は、#3694 で setupRoutes にあった行を移したもので、#3817 で構築子の
	// 必須の引数にした依存を足した (setupRoutes で setter に渡していたのと同じ式)。
	// ゲートが構築の式を引数まで含めて照合しうるので、repository・core・連合の部品も
	// setupRoutes と同じ名前の別名で受ける。
	userRepo := w.repos.user
	noteRepo := w.repos.note
	keypairRepo := w.repos.keypair
	webhookRepo := w.repos.webhook
	userListRepo := w.repos.userList
	authSessionRepo := w.repos.authSession
	bubbleGameRepo := w.repos.bubbleGame
	promoReadRepo := w.repos.promoRead
	signupTicketRepo := w.repos.signupTicket
	driveFileRepo := w.repos.driveFile
	mutingRepo := w.repos.muting
	blockingRepo := w.repos.blocking
	channelMutingRepo := w.repos.channelMuting
	userRelationRepo := w.repos.userRelation
	userService := coreSvcs.user
	followingService := coreSvcs.following
	notificationService := coreSvcs.notification
	blockingService := coreSvcs.blocking
	mutingService := coreSvcs.muting
	renoteMutingService := coreSvcs.renoteMuting
	driveService := coreSvcs.drive
	noteQueryService := coreSvcs.noteQuery
	instanceService := coreSvcs.instance
	channelService := coreSvcs.channel
	antennaService := coreSvcs.antenna
	clipService := coreSvcs.clip
	pageService := coreSvcs.page
	flashService := coreSvcs.flash
	roleService := coreSvcs.role
	apRenderer := fedSvcs.apRenderer
	apURLs := fedSvcs.apURLs
	inboxSoftwareChecker := fedSvcs.inboxSoftwareChecker

	// ugcVisibilityForVisitor の lookup は、notes 側と同じ理由で無条件に渡し、毎回読む
	// (#2106 S3 / #2708)。roleService は users/lists/create-from-public の
	// userListLimit / userEachUserListsLimit (#1550)、userRelationRepo は users/show の
	// 関係を 1 文で引く reader (#3608)。
	usersHandler := users.NewHandler(userService, followingService, noteRepo, idGen, noteHider, entityPacker,
		roleService, metaRepo, func() string { return metaUGCVisibility(metaRepo) },
		channelMutingRepo, mutingRepo, blockingRepo, userRelationRepo)
	iHandler := i.NewHandler(userService, idGen)
	webhookHandler := apiwebhooks.NewHandler(webhookRepo, idGen, roleService) // #1029: webhookLimit
	notificationsHandler := notifications.NewHandler(notificationService, idGen, entityPacker, noteHider,
		roleService, abuseNotifStates, abuseUnresolvedCount, signupApplicationNotifLookup)
	// #3330: hashtags/users のピン留め・移行先とモデレーター向けの項目、#3628: hashtags/trend から
	// hiddenTags / sensitiveWords のタグを外す。
	hashtagsHandler := apihashtags.NewHandler(s.db, entityPacker, usersHandler, roleService, metaRepo)
	galleryHandler := apigallery.NewHandler(s.db, idGen, entityPacker)
	// #3330: blocking/list のピン留め・移行先と、create・delete の応答を本家の
	// UserDetailedNotMe に揃える packer。
	blockingHandler := blocking.NewHandler(blockingService, userRepo, idGen, entityPacker, usersHandler, sharedUserPacker)
	muteHandler := mute.NewHandler(mutingService, userRepo, idGen, entityPacker, usersHandler)                   // #3330
	renoteMuteHandler := renotemute.NewHandler(renoteMutingService, userRepo, idGen, entityPacker, usersHandler) // #3330
	driveHandler := drive.NewHandler(driveService, idGen, entityPacker, noteHider, s.background)                 // #3661: 世代の停止で待つ
	// ap/show の federation-allow gate (#1557)。instanceService が blocked / federation
	// policy を判定する。local host は port 無しの hostname (config 起動時に validate
	// 済で非空保証) を渡す — IsBlocked/IsAllowed は hostname suffix match のため。
	// 受信を拒否するソフトウェアのホストも FEDERATION_NOT_ALLOWED にする (#3563)。
	// usersHandler は ap/show の利用者のピン留め・移行先を埋める (#3330)。
	apHandler := ap.NewHandler(apRenderer, userService, noteQueryService, keypairRepo, idGen, noteHider, entityPacker,
		instanceService, s.config.Hostname, inboxSoftwareChecker, usersHandler)
	wellknownHandler := wellknown.NewHandler(apURLs, userService, s.config.Host, s.config.URL)
	nodeinfoHandler := nodeinfo.NewHandler(s.config)
	// #1957-a / #3330: followers/following/users の embed user を本家 packMany と同じ形に。
	federationHandler := apifederation.NewHandler(instanceService, entityPacker, sharedUserPacker)
	// channels/timeline の blocked-host filter と、匿名 visitor への
	// ugcVisibilityForVisitor gate (notes 側と同じく無条件・毎回読む。#2708)。
	channelsHandler := apichannels.NewHandler(channelService, idGen, noteHider, entityPacker, metaRepo,
		func() string { return metaUGCVisibility(metaRepo) })
	// Notes の blocked-host filter (#2106 N5) と mute/block/channel-mute filter (#1544)。
	antennasHandler := antennas.NewHandler(antennaService, noteRepo, idGen, noteHider, entityPacker, metaRepo,
		mutingRepo, blockingRepo, channelMutingRepo)
	// Notes の blocked-host filter と muted/blocked-user filter (#1562)、匿名 visitor への
	// ugcVisibilityForVisitor gate (notes 側と同じく無条件・毎回読む)。
	clipsHandler := clips.NewHandler(clipService, idGen, noteHider, entityPacker, metaRepo,
		func() string { return metaUGCVisibility(metaRepo) }, mutingRepo, blockingRepo)
	// #1662: page の attachedFiles / eyeCatchingImage の解決と所有者の確認、#3330: pageEvent の
	// 利用者を持ち主から見た本家 pack と同じ形に。
	pagesHandler := pages.NewHandler(pageService, idGen, entityPacker, driveFileRepo, sharedUserPacker)
	flashHandler := apiflash.NewHandler(flashService, userRepo, idGen, entityPacker)
	// #1912 / #3330: following/list・requests/* の embed user を本家 packMany と同じ形に。
	followingHandler := following.NewHandler(followingService, userService, entityPacker, sharedUserPacker)
	// roles/notes の blocked-host filter と、#1973 / #3330: roles/users の embed user を
	// 本家 packMany と同じ形に。
	rolesHandler := apiroles.NewHandler(roleService, idGen, noteHider, entityPacker, metaRepo, sharedUserPacker)
	// #1029: userListLimit / userEachUserListsLimit
	userListHandler := apiuserlists.NewHandler(userListRepo, idGen, entityPacker, roleService)
	// #3330: auth/session/userkey・miauth check のピン留め・移行先、#3661: 世代の停止で待つ。
	authHandler := apiauth.NewHandler(authSessionRepo, s.config, idGen, entityPacker, usersHandler, s.background)
	appHandler := apiapp.NewHandler(authSessionRepo, idGen)
	bubbleGameHandler := apibubblegame.NewHandler(bubbleGameRepo, idGen, entityPacker)
	// **Lister は closure で遅延評価する。** ここで作る時点ではまだ全ルートが
	// 生えていないので、呼ばれたときに数える必要がある。
	endpointsHandler := endpoints.NewHandler(func() []string { return apiEndpointNames(s.echo) })
	avatarDecorationsHandler := avatardecorations.NewHandler(s.db, roleService.ExistingRoleIDSet, roleService.PublicRoleIDSet)
	promoHandler := promo.NewHandler(noteRepo, promoReadRepo, idGen)
	inviteHandler := apiinvite.NewHandler(signupTicketRepo, idGen, entityPacker, roleService, roleService)
	// /api/test の echo 用。test mode の /api/reset-db の handler (apitest.NewHandler) と
	// 型は同じだが、フィールドを持たない別の値で、test mode のときだけ作る方とは
	// まとめない (setupRoutes の if の中に残す)。
	echoTestHandler := apitest.NewEchoHandler()

	return apiHandlers{
		users:             usersHandler,
		i:                 iHandler,
		webhook:           webhookHandler,
		notifications:     notificationsHandler,
		hashtags:          hashtagsHandler,
		gallery:           galleryHandler,
		blocking:          blockingHandler,
		mute:              muteHandler,
		renoteMute:        renoteMuteHandler,
		drive:             driveHandler,
		ap:                apHandler,
		wellknown:         wellknownHandler,
		nodeinfo:          nodeinfoHandler,
		federation:        federationHandler,
		channels:          channelsHandler,
		antennas:          antennasHandler,
		clips:             clipsHandler,
		pages:             pagesHandler,
		flash:             flashHandler,
		following:         followingHandler,
		roles:             rolesHandler,
		userList:          userListHandler,
		auth:              authHandler,
		app:               appHandler,
		bubbleGame:        bubbleGameHandler,
		endpoints:         endpointsHandler,
		avatarDecorations: avatarDecorationsHandler,
		promo:             promoHandler,
		invite:            inviteHandler,
		echoTest:          echoTestHandler,
	}
}
