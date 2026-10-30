package server

import (
	"net/http"
	urlpkg "net/url"
	"time"

	"github.com/elythia-network/elythia/internal/api/ap"
	"github.com/elythia-network/elythia/internal/api/federation"
	"github.com/elythia-network/elythia/internal/api/inbox"
	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/api/nodeinfo"
	"github.com/elythia-network/elythia/internal/api/oauth"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	"github.com/elythia-network/elythia/internal/api/wellknown"
	"github.com/elythia-network/elythia/internal/core/chart/charthook"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
)

// federationRoutesDeps holds the parts wireFederationRoutes receives from
// setupRoutes (#3901). setupRoutes sets every field from the variable of the
// same name; TestSetupRoutesSectionsReceiveSameNamedVariables checks that no
// field is left out or filled from a different variable.
type federationRoutesDeps struct {
	idGen                   id.Generator
	userRepo                repository.UserRepository
	noteRepo                repository.NoteRepository
	metaRepo                repository.MetaRepository
	followingRepo           repository.FollowingRepository
	followRequestRepo       repository.FollowRequestRepository
	userRelationRepo        repository.UserRelationReader
	piningRepo              repository.UserNotePiningRepository
	blockingRepo            repository.BlockingRepository
	mutingRepo              repository.MutingRepository
	renoteMutingRepo        repository.RenoteMutingRepository
	driveFileRepo           repository.DriveFileRepository
	keypairExtraRepo        repository.UserKeypairExtraRepository
	channelRepo             repository.ChannelRepository
	clipRepo                repository.ClipRepository
	pageRepo                repository.PageRepository
	flashRepo               repository.FlashRepository
	reversiRepo             repository.ReversiRepository
	announcementRepo        repository.AnnouncementRepository
	sigCapRepo              repository.InstanceSignatureCapabilityRepository
	quoteAuthorizationRepo  *repository.NoteQuoteAuthorizationRepository
	userMemoRepo            repository.UserMemoRepository
	galleryRepo             repository.GalleryRepository
	accessTokenRepo         repository.AccessTokenRepository
	chartHooks              *charthook.Hooks
	roleService             *corerole.Service
	entityPacker            *entity.Packer
	coreSvcs                core
	instanceService         *coreinstance.Service
	proxyAccountResolver    meta.ProxyAccountResolver
	apFetcher               *corefederation.APFetcher
	federationResolver      *corefederation.Resolver
	federationProcessor     *corefederation.Processor
	sigCapBuffer            *coreinstance.CapabilityBuffer
	rateLimiter             *middleware.RateLimiter
	setupDetector           *meta.SetupDetector
	chunkedUploadCapability func() (int64, bool)
	frontend                echo.HandlerFunc
	streamRevokePublisher   *stream.StreamRevokePublisher
	apHandler               *ap.Handler
	wellknownHandler        *wellknown.Handler
	nodeinfoHandler         *nodeinfo.Handler
	federationHandler       *federation.Handler
}

// wireFederationRoutes registers the routes outside /api that other servers
// and crawlers reach: the ActivityPub resources with the SSR pages and feeds
// that share their paths (/notes/:id, /@:acct), the discovery documents
// (well-known, nodeinfo), the OAuth2 / IndieAuth provider and the inbox, and
// wires the federation API handler. setupRoutes calls it where these
// statements used to be, so the registration order is unchanged, and
// receives the handlers it returns under the same names. It binds every field
// of d to the name setupRoutes uses, so the moved statements read the same as
// before (#3901).
func (s *Server) wireFederationRoutes(d federationRoutesDeps) (*ssrMetaHandler, *feedHandler, *twemojiBadgeHandler, *oauth.Handler, *inbox.Handler) {
	idGen := d.idGen
	userRepo := d.userRepo
	noteRepo := d.noteRepo
	metaRepo := d.metaRepo
	followingRepo := d.followingRepo
	followRequestRepo := d.followRequestRepo
	userRelationRepo := d.userRelationRepo
	piningRepo := d.piningRepo
	blockingRepo := d.blockingRepo
	mutingRepo := d.mutingRepo
	renoteMutingRepo := d.renoteMutingRepo
	driveFileRepo := d.driveFileRepo
	keypairExtraRepo := d.keypairExtraRepo
	channelRepo := d.channelRepo
	clipRepo := d.clipRepo
	pageRepo := d.pageRepo
	flashRepo := d.flashRepo
	reversiRepo := d.reversiRepo
	announcementRepo := d.announcementRepo
	sigCapRepo := d.sigCapRepo
	quoteAuthorizationRepo := d.quoteAuthorizationRepo
	userMemoRepo := d.userMemoRepo
	galleryRepo := d.galleryRepo
	accessTokenRepo := d.accessTokenRepo
	chartHooks := d.chartHooks
	roleService := d.roleService
	entityPacker := d.entityPacker
	coreSvcs := d.coreSvcs
	instanceService := d.instanceService
	proxyAccountResolver := d.proxyAccountResolver
	apFetcher := d.apFetcher
	federationResolver := d.federationResolver
	federationProcessor := d.federationProcessor
	sigCapBuffer := d.sigCapBuffer
	rateLimiter := d.rateLimiter
	setupDetector := d.setupDetector
	chunkedUploadCapability := d.chunkedUploadCapability
	frontend := d.frontend
	streamRevokePublisher := d.streamRevokePublisher
	apHandler := d.apHandler
	wellknownHandler := d.wellknownHandler
	nodeinfoHandler := d.nodeinfoHandler
	federationHandler := d.federationHandler

	// ActivityPub resource endpoints
	apHandler.SetRemote(apFetcher, federationResolver)
	apHandler.SetExternalRedirectPolicy(func() bool {
		m, err := metaRepo.Fetch()
		if err != nil || m == nil {
			// 読めないときは既定 (true) に倒す。設定は外部への 3xx を止める
			// 硬化で、止められないときは従来どおり転送するだけ。
			return true
		}
		return m.AllowExternalApRedirect
	}, s.config.URL)
	// FEP-521a Multikey 対応で actor JSON に assertionMethod[] を expose する
	// ため Ed25519 keypair repo を wire (#1067 / #1069)。
	apHandler.SetKeypairExtraRepo(keypairExtraRepo)
	apHandler.SetFollowingRepo(followingRepo) // #1877 followers/following collection
	apHandler.SetNoteRepo(noteRepo)           // #1878 outbox collection
	// ap/show が返す UserDetailedNotMe に viewer relation block を埋める (#1778)。
	apHandler.SetRelationRepos(userrelation.Repos{
		Following:     followingRepo,
		Blocking:      blockingRepo,
		Muting:        mutingRepo,
		RenoteMuting:  renoteMutingRepo,
		FollowRequest: followRequestRepo,
		Memo:          userMemoRepo,
		Relation:      userRelationRepo,
	})
	// AP リソース系エンドポイントは Accept ヘッダで content negotiation する。
	// ブラウザからのリロード (Accept: text/html など) では SPA 用の HTML を
	// 返したいので、フォールバックとして frontendHTML を注入しておく。
	apHandler.SetNonAPFallback(frontend)
	s.echo.GET("/users/:id", apHandler.User)
	s.echo.GET("/users/:id/collections/featured", apHandler.Featured) // #1876 pinned notes
	s.echo.GET("/users/:id/followers", apHandler.Followers)           // #1877
	s.echo.GET("/users/:id/following", apHandler.Following)           // #1877
	s.echo.GET("/users/:id/outbox", apHandler.Outbox)                 // #1878
	// permalink の SSR meta (#2345 の続き)。AP クライアントは Accept で
	// 振り分けて従来どおり apHandler に渡す。
	ssrMeta := newSSRMetaHandler(
		s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability, setupDetector,
		ssrMetaDeps{
			User:         userRepo,
			Note:         noteRepo,
			Pining:       piningRepo,
			Page:         pageRepo,
			Clip:         clipRepo,
			Flash:        flashRepo,
			Gallery:      galleryRepo,
			Drive:        driveFileRepo,
			Channel:      channelRepo,
			Reversi:      reversiRepo,
			Announcement: announcementRepo,
			IDGen:        idGen,
		},
		entityPacker, // #3796: SSR の OGP 画像のファイルを包む
	)
	s.echo.GET("/notes/:id", func(c echo.Context) error {
		// 同一 URL が Accept 次第で HTML / AP JSON / 302 を返すので、HTML 変種
		// (ssrMeta 側) にも Vary が要る。閉包で立てれば両分岐に載る (#2506)。
		c.Response().Header().Set("Vary", "Accept")
		if prefersHTML(c) {
			return ssrMeta.NotePage(c)
		}
		return apHandler.Note(c)
	})
	// Create / Announce の activity id (renderer が広告する <note URI>/activity)
	// の dereference 先 (#2507)。upstream と同じくローカルノート専用。
	s.echo.GET("/notes/:id/activity", apHandler.NoteActivity)
	// FEP-044f の承認の実体 (#3234)。
	apHandler.SetQuoteAuthorizationStore(quoteAuthorizationRepo)
	s.echo.GET("/notes/:id/quote-authorizations/:authId", apHandler.QuoteAuthorization)
	// ユーザーフィード (#2345)。upstream ClientServerService と同じく
	// /@:user.rss / .atom / .json を返す。
	feedHost := s.config.URL
	if pu, err := urlpkg.Parse(s.config.URL); err == nil && pu.Host != "" {
		feedHost = pu.Host
	}
	// 依存は構築子の必須の引数で渡す (#3827)。ugcVisibilityForVisitor の lookup は毎回読む。
	feedH := newFeedHandler(s.config.URL, feedHost,
		feedUserResolver{repo: userRepo},
		noteRepo,
		idGen.ParseTime,
		func() string { return metaUGCVisibility(metaRepo) },
		func(userID string) *model.UserProfile {
			p, err := userRepo.FindProfileByUserID(userID)
			if err != nil {
				return nil
			}
			return p
		},
		func(u *model.User) string {
			if u.AvatarURL != nil && *u.AvatarURL != "" {
				return *u.AvatarURL
			}
			return entityPacker.IdenticonURL(u)
		},
		feedNoteHTML(feedHost))

	// /@<acct> は AP のユーザー解決とフィードの入口を兼ねる。Echo のルータは
	// Fastify と違い `/@:user.rss` のようなセグメント内リテラルを解釈できない
	// ので、ここで拡張子を見て振り分ける (#2345)。
	s.echo.GET("/@:acct", func(c echo.Context) error {
		// 同一 URL が Accept 次第で HTML / AP JSON / 301 を返すので、HTML 変種
		// (ssrMeta 側) にも Vary が要る (#2506)。feed は拡張子で URL 自体が
		// 変わるが、同じ閉包を通るので付いていて害は無い。
		c.Response().Header().Set("Vary", "Accept")
		if handled, err := feedH.TryServe(c, c.Param("acct")); handled {
			return err
		}
		if prefersHTML(c) {
			return ssrMeta.UserPage(c)
		}
		return apHandler.UserByAcct(c)
	})
	// プロフィールのサブページ。frontend が描画するので中身は SPA shell だが、
	// クローラ / リンク展開のために user の meta は載せる (upstream も同じ)。
	s.echo.GET("/@:acct/pages/:page", ssrMeta.UserPagePage)
	s.echo.GET("/@:acct/:sub", ssrMeta.UserPage)
	s.echo.GET("/clips/:clip", ssrMeta.ClipPage)
	// upstream ClientServerService が SSR している残りの permalink (#2531)。
	// SPA catchall より前に置かないと shell が先に返る。
	s.echo.GET("/channels/:channel", ssrMeta.ChannelPage)
	s.echo.GET("/reversi/g/:game", ssrMeta.ReversiGamePage)
	s.echo.GET("/announcements/:id", ssrMeta.AnnouncementPage)
	// upstream の noindex ページ (#2531)。タグの組み合わせは無限に増えるので
	// 検索対象にしない。
	s.echo.GET("/tags/:tag", ssrMeta.NoIndexPage)
	s.echo.GET("/user-tags/:tag", ssrMeta.NoIndexPage)
	// 連合先の情報ページ (#3030)。upstream は SSR していないので SPA shell が
	// そのまま index される。**自鯖の情報ではないものを検索結果に出す意味が
	// 無い**うえ、連合先の数だけ URL が増えて検索のノイズになる。
	// `:host` で自ホストも指せるが、あれも運用者向けの詳細ページなので分けない。
	s.echo.GET("/instance-info/:host", ssrMeta.NoIndexPage)
	s.echo.GET("/play/:id", ssrMeta.FlashPage)
	s.echo.GET("/gallery/:post", ssrMeta.GalleryPage)

	// Discovery endpoints
	// federation='none' のとき discovery を 403 で塞ぐため metaRepo を inject (#1924)。
	wellknownHandler.SetMetaRepo(metaRepo)
	// 非 API の Web リソース (#2345)。SPA catchall (s.echo.GET("/*")) より先に
	// 登録しないと index.html が 200 で返り、実装済みに見えて中身が HTML という
	// 分かりにくい壊れ方をする。
	webResHandler := newWebResourceHandler(s.config, metaRepo)
	s.echo.GET("/robots.txt", webResHandler.RobotsTxt)
	s.echo.GET("/opensearch.xml", webResHandler.OpenSearchXML)
	s.echo.GET("/api.json", s.OpenAPISpec)
	// /api.jsonを表示するAPIの説明のページ(#3810)。本家と同じくScalarを使う。
	s.echo.GET("/api-doc", s.APIDoc)
	// Web Push の通知バッジ (#2345)。twemoji SVG を単色 PNG に焼いて返す。
	// 焼いた PNG をキャッシュの一覧に登録するので (#3776)、handler を変数に取り出す。
	twemojiBadge := newTwemojiBadgeHandler(frontendutil.TwemojiDir())
	s.echo.GET("/twemoji-badge/*", twemojiBadge.Serve)

	s.echo.GET("/.well-known/webfinger", wellknownHandler.Webfinger)
	s.echo.GET("/.well-known/host-meta", wellknownHandler.HostMeta)
	s.echo.GET("/.well-known/host-meta.json", wellknownHandler.HostMetaJSON)
	s.echo.GET("/.well-known/nodeinfo", wellknownHandler.NodeInfoDiscovery)
	s.echo.GET("/.well-known/oauth-authorization-server", wellknownHandler.OAuthAuthorizationServer)
	// W3C "A Well-Known URL for Changing Passwords"。パスワードマネージャが
	// 変更画面を機械的に見つけるために引く (upstream PR 17901)。
	s.echo.GET("/.well-known/change-password", wellknownHandler.ChangePassword)
	// upstream `fastify.options('/.well-known/*')` 相当。
	s.echo.OPTIONS("/.well-known/*", wellknownHandler.Preflight)

	// OAuth2 / IndieAuth provider (#1899)。RFC8414 discovery が広告する
	// /oauth/authorize /decision /token を実装する。client discovery fetch は
	// attacker-suppliable な client_id URL を叩くため SSRF-guarded transport を使う。
	// 明示ルートは SPA catchall (s.echo.GET("/*")) より Echo router 上で優先される。
	oauthHandler := oauth.NewHandler(
		oauth.NewRedisStore(s.redis.Default),
		&http.Client{
			Timeout:   10 * time.Second,
			Transport: oauthDiscoveryTransport(s.config.AllowedPrivateNetworks, s.config.TestMode, s.outboundOpts()...),
		},
		userRepo,
		accessTokenRepo,
		idGen,
		s.config.URL,
		// 本家 OAuth2ProviderService は NODE_ENV === 'test' のとき http の
		// client_id を許可する (本番は https のみ)。TestMode を同じ用途で使う。
		s.config.TestMode,
		frontendConsentHTML(s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability, setupDetector),
		// code 再利用で token を revoke したとき、auth cache に残った entry で
		// 通り続けないよう即時失効させる (RFC6749 §4.1.2)。構築子の必須の引数 (#3823)。
		s.auth,
		// 失効させた token で張られた WebSocket も閉じる。revoke publisher は buildStream
		// で作る。構築子の必須の引数 (#3827)。
		streamRevokePublisher,
	)
	// 匿名で外部への取得と Redis への保存を起こせるので、接続元ごとに絞る。
	s.echo.GET("/oauth/authorize", oauthHandler.Authorize, middleware.RequireIPRateLimit(rateLimiter, middleware.OAuthAuthorizeRateLimitKey))
	s.echo.POST("/oauth/decision", oauthHandler.Decision)
	// upstream は token server にだけ fastifyCors を登録している
	// (createTokenServer)。ブラウザ上の SPA が PKCE でトークン交換できるように
	// するためで、authorize / decision は CORS 対象外。
	// Echo の CORS middleware は Origin ヘッダが無いと何も付けないが、
	// @fastify/cors は origin: '*' なら Origin の有無に関わらず
	// Access-Control-Allow-Origin: * を返す。token endpoint はその挙動が
	// upstream の e2e で検証されているので、wrapper で無条件に付与する。
	oauthCORS := func(next echo.HandlerFunc) echo.HandlerFunc {
		inner := echomw.CORSWithConfig(echomw.CORSConfig{
			AllowOrigins: []string{"*"},
			AllowMethods: []string{http.MethodPost, http.MethodOptions},
		})(next)
		return func(c echo.Context) error {
			if c.Response().Header().Get(echo.HeaderAccessControlAllowOrigin) == "" {
				c.Response().Header().Set(echo.HeaderAccessControlAllowOrigin, "*")
			}
			return inner(c)
		}
	}
	s.echo.POST("/oauth/token", oauthHandler.Token, oauthCORS)
	s.echo.OPTIONS("/oauth/token", func(c echo.Context) error {
		return c.NoContent(http.StatusNoContent)
	}, oauthCORS)
	// unknown /oauth/* は 404 (client が endpoint 対応有無を判別できるよう、
	// upstream の catch-all 同様)。明示ルートが優先されるので authorize/decision/
	// token はここに落ちない。
	s.echo.Any("/oauth/*", oauthHandler.NotFound)

	nodeinfoHandler.SetMetaRepo(metaRepo)
	nodeinfoHandler.SetUsageRepos(userRepo, noteRepo)
	// metadata.proxyAccountName 用に meta と同じ resolver を共有する (#1777)。
	nodeinfoHandler.SetProxyAccountResolver(proxyAccountResolver)
	s.echo.GET("/nodeinfo/2.1", nodeinfoHandler.Version2_1)
	// /.well-known/nodeinfo が 2.0 リンクも advertise するので 2.0 も配信する (#1777)。
	s.echo.GET("/nodeinfo/2.0", nodeinfoHandler.Version2_0)

	// Inbox endpoints
	// inbox admission の host 署名+一致チェックに自ホストを渡す (#1949。第 3 引数)。
	// #534: signature 検証成功後の Process(body) を inbox queue に逃がす (第 4 引数)。
	// nil なら legacy 同期処理にフォールバックする (テスト互換)。
	// ブロック済み・連合しない host の actor を 403 にする (第 5 引数)。
	inboxHandler := inbox.NewHandler(federationResolver, federationProcessor,
		s.config.Host, s.queueClient, instanceService)
	inboxHandler.SetLocalFederationObserver(coreSvcs.localFederation)
	inboxHandler.SetUserRepo(userRepo)
	inboxHandler.SetNoteRepo(noteRepo)
	inboxHandler.SetInstanceTracker(instanceService)
	inboxHandler.SetSignatureCapabilityRecorder(sigCapBuffer)
	inboxHandler.SetChartHook(chartHooks)
	// inbox の 64KB body limit は global pre-auth な middleware.BodyLimitByPath で適用する
	// (#1958 / #2075)。route-level に置くと global な auth.Authenticate の token 抽出 read が
	// 先に走って bypass されるため、ここでは limit を付けない。
	s.echo.POST("/inbox", inboxHandler.Inbox)
	s.echo.POST("/users/:id/inbox", inboxHandler.Inbox)

	// Federation endpoints
	federationHandler.SetFollowingRepo(followingRepo)
	federationHandler.SetUserRepo(userRepo)
	federationHandler.SetIDGen(idGen)
	federationHandler.SetResolver(federationResolver)
	// moderationNote は公開エンドポイントで moderator にのみ返す (情報漏洩対策)。
	federationHandler.SetModeratorChecker(roleService)
	// instance 一覧 / show-instance の signatureCapability field (#2393)。
	federationHandler.SetSignatureCapabilityLookup(sigCapRepo)
	return ssrMeta, feedH, twemojiBadge, oauthHandler, inboxHandler
}
