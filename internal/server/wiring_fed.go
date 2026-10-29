package server

import (
	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	corerelay "github.com/elythia-network/elythia/internal/core/relay"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
)

// fed holds the federation parts setupRoutes builds through buildFederation
// (#3688).
//
// Fields are named after the local variables setupRoutes has always used,
// without the Service / Svc suffix, and keep the concrete type the
// constructor returns. Only parts whose constructor just builds a value are
// listed: no DB or Redis access, no outbound call or log, no goroutine, timer,
// shutdown hook or global write, and dependencies limited to the
// repositories, the core services, server configuration, the Server's queue
// client, Redis and DB handles, the ID generator and the parts built before
// them here. The relay service (core/relay) is listed here because it
// depends on the delivery service and the renderer. Parts that need the
// outbound HTTP client (the AP client, fetcher, resolver and everything built
// on them), read the DB at construction, or pair a Start with a shutdown hook
// stay in setupRoutes (docs/architecture.md 4.2).
//
// Three stateless types used to be built more than once in setupRoutes and
// are now built once and shared: inboxSoftwareChecker (built three times
// before), quoteRequestDeliveryHook (twice) and userModerationDeliveryHook
// (twice). Their fields are set only by the constructor, they have no setter,
// and their methods only read the fields, so one value behaves the same as
// one per consumer.
//
// buildFederation only constructs. Every setter on these parts is still called
// at its old position in setupRoutes, because workers started during
// construction must see the same wiring as before (docs/architecture.md 4.1).
// The dependencies #3833 made required constructor arguments (the delivery
// service's host block checker, the note delete hook's repositories) are
// passed here instead; they only make delivery skip more hosts or reach more
// recipients, and nothing delivers between here and their old setters.
type fed struct {
	inboxSoftwareChecker       *corefederation.SuspendedChecker
	apURLs                     *activitypub.URLBuilder
	apRenderer                 *activitypub.Renderer
	deliver                    *corefederation.DeliverService
	relay                      *corerelay.Service
	noteDeliveryHook           *corefederation.NoteDeliveryHook
	followingDeliveryHook      *corefederation.FollowingDeliveryHook
	quoteRequestDeliveryHook   *corefederation.QuoteRequestDeliveryHook
	noteDeleteHook             *corefederation.NoteDeleteDeliveryHook
	deliveryBreaker            *deliveryhealth.Breaker
	userModerationDeliveryHook *corefederation.UserModerationDeliveryHook
	profileUpdateHook          *corefederation.ProfileUpdateDeliveryHook
	pinDeliveryHook            *corefederation.PinDeliveryHook
}

// buildFederation builds the federation parts from w.repos and coreSvcs, in
// the order setupRoutes used to build them. It reads s.config, s.queueClient,
// s.redis and s.db. coreSvcs is the value buildCore returned; it is passed in
// rather than read back from w.core so that calling buildFederation before
// buildCore does not compile (docs/architecture.md 4.2).
func (s *Server) buildFederation(w *wiring, coreSvcs core, idGen id.Generator) fed {
	// 下の行は setupRoutes にあった行をそのまま移した。ゲートが構築の式を引数まで
	// 含めて照合しうるので、repository も setupRoutes と同じ名前の別名で受ける。
	userRepo := w.repos.user
	noteRepo := w.repos.note
	var metaRepo repository.MetaRepository = w.repos.cachedMeta
	followingRepo := w.repos.following
	keypairRepo := w.repos.keypair
	instanceRepo := w.repos.instance
	sysAcctSvc := coreSvcs.sysAcct
	instanceService := coreSvcs.instance
	localFederationService := coreSvcs.localFederation

	// 受信を拒否するソフトウェア (#3362) の判定。連合の resolver、WebFinger で
	// 利用者を引く remoteUserResolver、inbox の processor、ap/show が共有する。
	// 判定のたびに meta を読むだけで状態を持たないので、1 つで足りる (#3688)。
	inboxSoftwareChecker := corefederation.NewInboxSuspendedChecker(metaRepo, instanceRepo)

	apURLs := activitypub.NewURLBuilder(s.config.URL)
	apRenderer := activitypub.NewRenderer(apURLs)

	// ブロック済み・連合しない host の inbox へは enqueue しない (#536)。resolver・inbox と
	// 同じ instanceService を渡す。構築子の必須の引数 (#3833)。
	deliverService := corefederation.NewDeliverService(s.queueClient, userRepo, followingRepo, keypairRepo, apURLs,
		instanceService)
	deliverService.SetLocalFederationObserver(localFederationService)
	deliverService.SetNoteRepo(noteRepo)
	// Relay 連携 (#161)。relay.Service は AP delivery を必要とするので
	// deliverService の直後で作る。setter (SetLdSigning) は setupRoutes の元の位置。
	relaySvc := corerelay.NewService(repository.NewRelayRepository(s.db), sysAcctSvc, apRenderer, deliverService, idGen)
	noteDeliveryHook := corefederation.NewNoteDeliveryHook(deliverService, apRenderer, apURLs, idGen, userRepo, noteRepo)
	followingDeliveryHook := corefederation.NewFollowingDeliveryHook(deliverService, apRenderer, apURLs)
	// QuoteRequest への応答 (#3234)。QuoteRequest の handler と、ブロックで承認を
	// 取り消す QuoteRevoker が共有する。配送の service と renderer しか持たない (#3688)。
	quoteRequestDeliveryHook := corefederation.NewQuoteRequestDeliveryHook(deliverService, apRenderer)
	// userRepo は DM の宛先と既知のリモート inbox を、noteRepo は renote / reply した
	// リモート user を引く (#2995)。構築子の必須の引数 (#3833)。
	noteDeleteHook := corefederation.NewNoteDeleteDeliveryHook(deliverService, apRenderer, apURLs, userRepo, noteRepo)
	// 落ちた配送先へのブレーカー (#3048)。状態は Redis に置くので、作るだけなら
	// Redis に触らない。
	deliveryBreaker := deliveryhealth.NewBreaker(s.redis.Default)
	// アカウントの削除・凍結・解除の配送 (#1759 / #2230)。i/delete-account と
	// admin の両方が使う。状態を持たないので 1 つで足りる (#3688)。
	userModerationDeliveryHook := corefederation.NewUserModerationDeliveryHook(deliverService, apRenderer, userRepo)
	profileUpdateHook := corefederation.NewProfileUpdateDeliveryHook(deliverService, apRenderer, userRepo, keypairRepo)
	profileUpdateHook.SetLocalFederationObserver(localFederationService)
	pinDeliveryHook := corefederation.NewPinDeliveryHook(deliverService, apRenderer, userRepo, noteRepo)

	return fed{
		inboxSoftwareChecker:       inboxSoftwareChecker,
		apURLs:                     apURLs,
		apRenderer:                 apRenderer,
		deliver:                    deliverService,
		relay:                      relaySvc,
		noteDeliveryHook:           noteDeliveryHook,
		followingDeliveryHook:      followingDeliveryHook,
		quoteRequestDeliveryHook:   quoteRequestDeliveryHook,
		noteDeleteHook:             noteDeleteHook,
		deliveryBreaker:            deliveryBreaker,
		userModerationDeliveryHook: userModerationDeliveryHook,
		profileUpdateHook:          profileUpdateHook,
		pinDeliveryHook:            pinDeliveryHook,
	}
}
