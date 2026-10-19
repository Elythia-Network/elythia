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
	"github.com/elythia-network/elythia/internal/misc/id"
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
// federation parts, server configuration, the Server's DB handle and echo
// instance, the ID generator and nothing else.
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
// buildHandlers only constructs. Every setter on these handlers is still
// called at its old position in setupRoutes, and every route is still
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
// in the order setupRoutes used to build them. It reads s.db, s.config and
// s.echo (the endpoints handler reads the echo routes when a request comes,
// not here, because not every route is registered yet). coreSvcs and fedSvcs
// are the values buildCore and buildFederation returned; they are passed in
// rather than read back from w.core and w.fed so that calling buildHandlers
// before them does not compile (docs/architecture.md 4.2).
func (s *Server) buildHandlers(w *wiring, coreSvcs core, fedSvcs fed, idGen id.Generator) apiHandlers {
	// 下の行は setupRoutes にあった行をそのまま移した。ゲートが構築の式を引数まで
	// 含めて照合しうるので、repository・core・連合の部品も setupRoutes と同じ名前の
	// 別名で受ける。
	userRepo := w.repos.user
	noteRepo := w.repos.note
	keypairRepo := w.repos.keypair
	webhookRepo := w.repos.webhook
	userListRepo := w.repos.userList
	authSessionRepo := w.repos.authSession
	bubbleGameRepo := w.repos.bubbleGame
	promoReadRepo := w.repos.promoRead
	signupTicketRepo := w.repos.signupTicket
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

	usersHandler := users.NewHandler(userService, followingService, noteRepo, idGen)
	iHandler := i.NewHandler(userService, idGen)
	webhookHandler := apiwebhooks.NewHandler(webhookRepo, idGen)
	notificationsHandler := notifications.NewHandler(notificationService, idGen)
	hashtagsHandler := apihashtags.NewHandler(s.db)
	galleryHandler := apigallery.NewHandler(s.db, idGen)
	blockingHandler := blocking.NewHandler(blockingService, userRepo, idGen)
	muteHandler := mute.NewHandler(mutingService, userRepo, idGen)
	renoteMuteHandler := renotemute.NewHandler(renoteMutingService, userRepo, idGen)
	driveHandler := drive.NewHandler(driveService, idGen)
	apHandler := ap.NewHandler(apRenderer, userService, noteQueryService, keypairRepo, idGen)
	wellknownHandler := wellknown.NewHandler(apURLs, userService, s.config.Host, s.config.URL)
	nodeinfoHandler := nodeinfo.NewHandler(s.config)
	federationHandler := apifederation.NewHandler(instanceService)
	channelsHandler := apichannels.NewHandler(channelService, idGen)
	antennasHandler := antennas.NewHandler(antennaService, noteRepo, idGen)
	clipsHandler := clips.NewHandler(clipService, idGen)
	pagesHandler := pages.NewHandler(pageService, idGen)
	flashHandler := apiflash.NewHandler(flashService, userRepo, idGen)
	followingHandler := following.NewHandler(followingService, userService)
	rolesHandler := apiroles.NewHandler(roleService, idGen)
	userListHandler := apiuserlists.NewHandler(userListRepo, idGen)
	authHandler := apiauth.NewHandler(authSessionRepo, s.config, idGen)
	appHandler := apiapp.NewHandler(authSessionRepo, idGen)
	bubbleGameHandler := apibubblegame.NewHandler(bubbleGameRepo, idGen)
	// **Lister は closure で遅延評価する。** ここで作る時点ではまだ全ルートが
	// 生えていないので、呼ばれたときに数える必要がある。
	endpointsHandler := endpoints.NewHandler(func() []string { return apiEndpointNames(s.echo) })
	avatarDecorationsHandler := avatardecorations.NewHandler(s.db, roleService.ExistingRoleIDSet, roleService.PublicRoleIDSet)
	promoHandler := promo.NewHandler(noteRepo, promoReadRepo, idGen)
	inviteHandler := apiinvite.NewHandler(signupTicketRepo, idGen)
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
