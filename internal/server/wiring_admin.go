package server

import (
	"context"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/api/adminbackup"
	apiannouncements "github.com/elythia-network/elythia/internal/api/announcements"
	apiemojiapplications "github.com/elythia-network/elythia/internal/api/emojiapplications"
	"github.com/elythia-network/elythia/internal/api/flash"
	"github.com/elythia-network/elythia/internal/api/gallery"
	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/api/users"
	coreabuse "github.com/elythia-network/elythia/internal/core/abuse"
	"github.com/elythia-network/elythia/internal/core/avatardecoration"
	"github.com/elythia-network/elythia/internal/core/dbhealth"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/driveusage"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/core/emojimeta"
	"github.com/elythia-network/elythia/internal/core/event"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/fedrule"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/core/gonecleanup"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	coremodlog "github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/procstats"
	corerelay "github.com/elythia-network/elythia/internal/core/relay"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/core/systemaccount"
	coretwofactor "github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/core/userpack"
	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/stream"
)

// adminDeps holds the parts wireAdmin receives from setupRoutes (#3901).
// setupRoutes sets every field from the variable of the same name;
// TestSetupRoutesSectionsReceiveSameNamedVariables checks that no field is
// left out or filled from a different variable.
type adminDeps struct {
	idGen                        id.Generator
	internalPubSub               *event.PubSubService
	userRepo                     repository.UserRepository
	noteRepo                     repository.NoteRepository
	metaRepo                     repository.MetaRepository
	followingRepo                repository.FollowingRepository
	followRequestRepo            repository.FollowRequestRepository
	emojiRepo                    *repository.CachedEmojiRepository
	driveFileRepo                repository.DriveFileRepository
	suspensionOriginRepo         repository.UserSuspensionOriginRepository
	instanceRepo                 repository.InstanceRepository
	systemWebhookRepo            repository.SystemWebhookRepository
	userIPRepo                   repository.UserIPRepository
	goneInstanceRepo             *repository.GoneInstanceRepository
	signupTicketRepo             repository.RegistrationTicketRepository
	signinRepo                   repository.SigninRepository
	avatarDecorationRepo         repository.AvatarDecorationRepository
	recipientRepo                repository.AbuseReportNotificationRecipientRepository
	ipLookupLogRepo              repository.IPLookupLogRepository
	emojiApplicationRepoForNotif repository.EmojiApplicationRepository
	fedRuleRepo                  *repository.FederationRuleRepository
	sigCapRepo                   repository.InstanceSignatureCapabilityRepository
	userSecurityKeyRepo          repository.UserSecurityKeyRepository
	abuseReportRepo              repository.AbuseReportRepository
	adRepo                       repository.AdRepository
	emojiDecorationResolver      *avatardecoration.EmojiResolver
	roleService                  *corerole.Service
	avatarDecorationResolver     *avatardecoration.Resolver
	entityPacker                 *entity.Packer
	sharedUserPacker             *userpack.Packer
	modLogService                *coremodlog.Service
	totpReplayGuard              *coretwofactor.RedisReplayGuard
	instanceService              *coreinstance.Service
	signupService                *signup.Service
	noteDeleteService            *note.DeleteService
	userService                  *user.Service
	followingService             *corefollowing.Service
	notificationService          *notification.Service
	webhookService               *corewebhook.Service
	localDriveStorage            *coredrive.LocalStorage
	driveStorage                 *coredrive.MetaStorage
	driveService                 *coredrive.Service
	sysAcctSvc                   *systemaccount.Service
	passwordFailureGuard         *passwordguard.RedisGuard
	dbHealth                     *dbhealth.Service
	driveUsage                   *driveusage.Service
	apRenderer                   *activitypub.Renderer
	deliverService               *corefederation.DeliverService
	relaySvc                     *corerelay.Service
	deliveryBreaker              *deliveryhealth.Breaker
	userModerationDeliveryHook   *corefederation.UserModerationDeliveryHook
	apFetcher                    *corefederation.APFetcher
	fedRuleHits                  *fedrule.HitStore
	fedRuleService               *fedrule.Service
	metadataFetcher              *coreinstance.FetchMetadataService
	deliverProcessor             *processors.DeliverProcessor
	deliveryHealth               *deliveryhealth.Service
	inboxHealth                  *deliveryhealth.Service
	metaHandler                  *meta.Handler
	signupApplicationService     *signupapplication.Service
	webauthnSvc                  *coretwofactor.WebAuthnService
	streamRevokePublisher        *stream.StreamRevokePublisher
	usersHandler                 *users.Handler
	galleryHandler               *gallery.Handler
	flashHandler                 *flash.Handler
	broadcastPublisher           *stream.BroadcastPublisher
	abuseCreatedNotifier         *coreabuse.CreatedNotifier
	selfPacker                   *meself.Packer
	announcementHandler          *apiannouncements.Handler
}

// wireAdmin builds the admin API handler and wires its dependencies (users,
// moderation, roles, emojis and emoji applications, federation, abuse
// reports, the system webhooks and mail), and builds the handler of the DB
// backup screen with its re-authentication and the /backup-download route.
// setupRoutes calls it where these statements used to be and receives the
// handlers and the service it returns under the same names. It binds every
// field of d to the name setupRoutes uses, so the moved statements read the
// same as before (#3901).
func (s *Server) wireAdmin(d adminDeps) (*apiadmin.Handler, *emojiapplication.Service, *apiemojiapplications.Handler, *adminbackup.Handler) {
	idGen := d.idGen
	internalPubSub := d.internalPubSub
	userRepo := d.userRepo
	noteRepo := d.noteRepo
	metaRepo := d.metaRepo
	followingRepo := d.followingRepo
	followRequestRepo := d.followRequestRepo
	emojiRepo := d.emojiRepo
	driveFileRepo := d.driveFileRepo
	suspensionOriginRepo := d.suspensionOriginRepo
	instanceRepo := d.instanceRepo
	systemWebhookRepo := d.systemWebhookRepo
	userIPRepo := d.userIPRepo
	goneInstanceRepo := d.goneInstanceRepo
	signupTicketRepo := d.signupTicketRepo
	signinRepo := d.signinRepo
	avatarDecorationRepo := d.avatarDecorationRepo
	recipientRepo := d.recipientRepo
	ipLookupLogRepo := d.ipLookupLogRepo
	emojiApplicationRepoForNotif := d.emojiApplicationRepoForNotif
	fedRuleRepo := d.fedRuleRepo
	sigCapRepo := d.sigCapRepo
	userSecurityKeyRepo := d.userSecurityKeyRepo
	abuseReportRepo := d.abuseReportRepo
	adRepo := d.adRepo
	emojiDecorationResolver := d.emojiDecorationResolver
	roleService := d.roleService
	avatarDecorationResolver := d.avatarDecorationResolver
	entityPacker := d.entityPacker
	sharedUserPacker := d.sharedUserPacker
	modLogService := d.modLogService
	totpReplayGuard := d.totpReplayGuard
	instanceService := d.instanceService
	signupService := d.signupService
	noteDeleteService := d.noteDeleteService
	userService := d.userService
	followingService := d.followingService
	notificationService := d.notificationService
	webhookService := d.webhookService
	localDriveStorage := d.localDriveStorage
	driveStorage := d.driveStorage
	driveService := d.driveService
	sysAcctSvc := d.sysAcctSvc
	passwordFailureGuard := d.passwordFailureGuard
	dbHealth := d.dbHealth
	driveUsage := d.driveUsage
	apRenderer := d.apRenderer
	deliverService := d.deliverService
	relaySvc := d.relaySvc
	deliveryBreaker := d.deliveryBreaker
	userModerationDeliveryHook := d.userModerationDeliveryHook
	apFetcher := d.apFetcher
	fedRuleHits := d.fedRuleHits
	fedRuleService := d.fedRuleService
	metadataFetcher := d.metadataFetcher
	deliverProcessor := d.deliverProcessor
	deliveryHealth := d.deliveryHealth
	inboxHealth := d.inboxHealth
	metaHandler := d.metaHandler
	signupApplicationService := d.signupApplicationService
	webauthnSvc := d.webauthnSvc
	streamRevokePublisher := d.streamRevokePublisher
	usersHandler := d.usersHandler
	galleryHandler := d.galleryHandler
	flashHandler := d.flashHandler
	broadcastPublisher := d.broadcastPublisher
	abuseCreatedNotifier := d.abuseCreatedNotifier
	selfPacker := d.selfPacker
	announcementHandler := d.announcementHandler

	// Admin endpoints (Phase 5)
	// 通報の abuseReport system webhook (#1542)。local と Flag の両方に効く
	// (#3256)。recipientRepo がここで揃うため本箇所で配線する。
	abuseCreatedNotifier.SetWebhook(webhookService, recipientRepo, coreabuse.UserLookups{Instances: instanceRepo, Emojis: emojiRepo}, idGen)
	// 通報のメール (#3265)。SMTP の設定は送るたびに meta から読み直すので、
	// 未設定でも配線しておいてよい (そのときは送らない)。
	abuseCreatedNotifier.SetMail(miscsmtp.SubjectBodySenderFromMeta(metaRepo, s.config.ProxySMTP), recipientRepo, userRepo, metaRepo)
	// 通知の lookup が先に作った実体を使い回す (2 つ作らない)。
	emojiApplicationRepo := emojiApplicationRepoForNotif
	// **入れ忘れると黙って劣化する依存は、構築子の必須の引数で渡す (#3823)。**
	adminHandler := apiadmin.NewHandler(signupService, roleService, metaRepo, userRepo, idGen,
		s.background,     // #3661: 世代の停止で待つ
		entityPacker,     // #3792: 利用者の詳細を包む
		usersHandler,     // #3330: find-by-email・update-proxy-account のピン留め・移行先
		sharedUserPacker, // #3330: show-users・roles/users・abuse-user-reports・show-moderation-logs の利用者を本家 packMany と同じ形に
		// モデレーターの suspend / unsuspend を local 由来として刻む (#2973)。
		suspensionOriginRepo,
		// admin/suspend-user / admin/unsuspend-user / admin/accounts/delete が
		// target user の全 token cache entry を即時 invalidate するために、
		// auth middleware を inject する (#965)。未配線時は 30 秒 cache TTL 待ち
		// で stale な user が auth 通過する security regression が残る。
		// i/regenerate-token (#884) と同じく AuthMiddleware が duck-typed で
		// UserTokenInvalidator interface を満たしている。
		s.auth,
		// 凍結・削除した利用者の WebSocket を閉じる (tokenCache の失効とは独立)。
		streamRevokePublisher,
		// IP 照会の監査 (#3106)。**未配線だと照会が記録されないまま通る** — 照会は
		// 成立するので誰も気付けない。
		iplookuplog.NewService(ipLookupLogRepo, idGen),
		// admin/emoji/copy で remote 画像を local drive に保存するための fetcher
		// (#670)。outboundClient 経由なので SSRF / proxy / outgoing address が他の
		// outbound 経路と同じ設定で適用される。
		apiadmin.NewEmojiImageFetcher(s.outboundClient(10*time.Second), driveService, s.config.UserAgent),
	)
	adminHandler.SetSelfPacker(selfPacker) // #3784: show-users・roles/users・update-proxy-account で自分を MeDetailed に
	adminHandler.SetPasswordHasher(s.passwordHasher)
	// catalog 更新を entity 側 packer に即時反映する (#2258)。TTL 任せだと
	// 作成直後に装着された decoration が lookup miss で silent drop される。
	adminHandler.SetAvatarDecorationInvalidator(avatarDecorationResolver)
	adminHandler.SetEmojiDecorationInvalidator(emojiDecorationResolver)
	// admin/ad/* の変更を /api/meta の response cache に反映する (#2649)。
	adminHandler.SetMetaResponseInvalidator(metaHandler)
	adminHandler.SetInstanceRepo(instanceRepo)
	adminHandler.SetDeliveryHealthProvider(deliveryHealth)
	if deliveryBreaker != nil {
		adminHandler.SetDeliveryBreaker(deliveryBreaker)
	}
	adminHandler.SetInboxHealthProvider(inboxHealth)
	// IP からアカウントを引く口 (#3104)。**この行を落とすと admin/ip/accounts が
	// 500 を返す** — 空の結果は「その IP を使ったアカウントは無い」という誤った
	// 事実になり、調査の結論を反転させる。
	adminHandler.SetIPSearchRepo(repository.NewUserIPSearchRepository(s.db))
	adminHandler.SetIPLookupLogRepo(ipLookupLogRepo)
	// ドライブ使用量の集計 (#3053)。**この行を落とすと admin/drive/usage が
	// 500 を返す** — 0 バイトを返して「使っていない」と誤認させるよりよい。
	adminHandler.SetDriveUsageProvider(driveUsage)
	adminHandler.SetSignupApplicationReviewer(signupApplicationService)
	// カスタム絵文字の登録申請 (#2934)。承認までは emoji 行を作らず、
	// 専用テーブルに閉じ込める (signup_application と同じ形)。
	//
	// **承認時の emoji 生成は adminHandler が行う。** service に持たせると
	// admin/emoji/add と別経路になり、MIME の allowlist などの検証が申請
	// だけ抜ける。ここでは依存を逆向きに渡すだけ。
	emojiApplicationService := emojiapplication.NewService(
		emojiApplicationRepo,
		emojiRepo,
		driveFileRepo,
		idGen,
		adminHandler,
		emojiapplication.NewResultNotifier(notificationService),
		// 期間上限をロールから引く (#2958)。無いと上限が丸ごと効かなくなるので、
		// 構築子の必須の引数で渡す (#3823)。
		roleService,
		// 申請枠の手動リセット (#2962)。**無いとリセットは 500 で失敗する**
		// (`ErrQuotaResetUnavailable`) ので、戻したつもりで戻っていない状態にはならない。
		// ただし既存のリセットも読めなくなる = 過去に戻した枠が再び満杯に見える。
		// 渡し忘れはコンパイルで、nil リテラルは静的ゲート
		// (`emoji_application_gate_test.go`) で、変数経由の nil は組み立てたサーバーを
		// 見る TestBuiltHandlersHaveNoNilDependencies で落とす。
		repository.NewEmojiApplicationQuotaResetRepository(s.db),
	)
	// 申請が出されたことを**審査できる人**へ知らせる (#2987)。モデレーターでは
	// なく `canManageCustomEmojis` を持つ人 — 審査 endpoint の gate と揃える。
	emojiApplicationService.SetReceivedNotifier(
		emojiapplication.NewReceivedNotifier(notificationService, roleService))
	emojiApplicationHandler := apiemojiapplications.NewHandler(
		emojiApplicationService, emojiApplicationRepo, driveFileRepo)
	// 「自分の申請」の画像プレビュー (#2989)。承認済みは `emojiId`、未承認の
	// リモート申請は `remoteHost + remoteName` で引く。**未配線だと preview が
	// `unknown` になる** — 「消えた」と断定しないので害は「理由が出ない」だけ。
	emojiApplicationHandler.SetEmojiLookup(emojiRepo)
	adminHandler.SetEmojiApplicationReviewer(emojiApplicationService)
	adminHandler.SetEmojiApplicationRepo(emojiApplicationRepo)
	// 連合セルフ診断 (#2463)。migration 本数は起動時に数えず 0 を渡す
	// (server 側は既に migrate 済みで動いている前提。適用漏れの検出は
	// `elythia doctor` の担当で、あちらは同梱ファイルを数えられる)。
	adminHandler.SetDatabaseHealth(dbHealth)
	if s.selfCheckRunner == nil {
		s.selfCheckRunner = &selfCheckAdapter{
			checker: selfcheck.NewChecker(s.config.URL),
			deps:    selfcheck.LocalDeps{DB: s.db, Redis: s.redis.Default, DBHealth: dbHealth.Report},
		}
	}
	adminHandler.SetSelfCheckRunner(s.selfCheckRunner)
	// 連合先との疎通の診断 (#3055)。**通信は SSRF-safe な outboundClient だけ**で、
	// 上の self-check の client (ガード無し) とは繋がない。自ホストは断る。
	if s.remoteChecker == nil {
		s.remoteChecker = s.newRemoteCheckAdapter(remoteCheckSources{
			fetcher:   apFetcher,
			instances: instanceService,
			users:     repository.NewRemoteUserSampler(s.db),
			sigCaps:   sigCapRepo,
			degraded:  deliverProcessor.Ed25519Degraded,
			delivery:  deliveryHealth,
			breaker:   deliveryBreaker,
		})
	}
	adminHandler.SetRemoteChecker(s.remoteChecker, corefederation.NormalizeGateHost(s.config.URL))
	// 消えたインスタンスとのフォロー関係の片付け (#3067)。自動では消さず、管理者が
	// 候補を見て実行する。
	adminHandler.SetGoneInstanceCleaner(gonecleanup.NewService(goneInstanceRepo, instanceService, followingService))
	adminHandler.SetFederationRuleManager(fedrule.NewManager(fedRuleRepo, fedRuleService, fedRuleHits, idGen, func() {
		if err := internalPubSub.Publish(context.Background(), "federationRulesUpdated", struct{}{}); err != nil {
			slog.Warn("fedrule: publish federationRulesUpdated failed", "err", err)
		}
	}))
	// admin/federation/update-instance の suspend / unsuspend を deliver hot path
	// の suspend 判定 cache へ TTL を待たず即時反映する (#1407 review)。
	adminHandler.SetInstanceSuspendCacheInvalidator(instanceService)
	// admin/show-user の signins field を実データで埋める (#1198)。signinRepo
	// は signin handler と共有する (先頭の repository の節で作る)。
	adminHandler.SetSigninRepo(signinRepo)
	adminHandler.SetAbuseRepo(abuseReportRepo)
	adminHandler.SetAbuseForwarder(coreabuse.NewForwarder(abuseReportRepo, sysAcctSvc, apRenderer, deliverService))
	// suspend / delete-account / unsuspend 時に対象 local user の AP Delete /
	// Undo(Delete) を remote instances へ配信する (#1759)。i/delete-account と同じ
	// hook を使う (#3688)。
	adminHandler.SetUserModerationFederationHook(userModerationDeliveryHook)
	adminHandler.SetDeleteAccountEnqueuer(s.queueClient)
	adminHandler.SetServerURL(s.config.URL)
	adminHandler.SetConfigSetupPassword(s.config.SetupPassword)
	adminHandler.SetSMTPProxyURL(s.config.ProxySMTP)
	adminHandler.SetModLogService(modLogService)
	// #1765: moderator が他人の note を削除したとき deleteNote moderation log を残す。
	noteDeleteService.SetModeratorDeleteHook(coremodlog.NewNoteDeleteHook(modLogService))
	// announcements (別パッケージの Handler) も AdminCreate/Update/Delete で
	// modlog を書くため同じ service instance を共有する。
	announcementHandler.SetModLogService(modLogService)
	announcementHandler.SetUserRepo(userRepo)
	galleryHandler.SetModLog(modLogService) // #1548: moderator による gallery post 削除の監査ログ
	flashHandler.SetModLog(modLogService)   // #1548: moderator による flash 削除の監査ログ
	adminHandler.SetEmojiRepo(emojiRepo)
	// emoji の add/update/delete を broadcast stream へ流し emoji picker を live-refresh (#2046)。
	adminHandler.SetBroadcastPublisher(broadcastPublisher)
	adminHandler.SetDriveFileRepo(driveFileRepo)
	adminHandler.SetDriveBulkDeleter(driveService) // #1772: delete-all-files の物理削除 + 使用量減算
	// bulk drive cleanup (clean-remote-files / delete-all-files-of-a-user) で
	// object storage の物理オブジェクトも消すため storage backend を渡す。
	adminHandler.SetStorageDeleter(driveStorage)
	// storedInternal=true な行はローカル FS にあるので、bulk cleanup がそちらも
	// 消せるようにする (#2315)。
	adminHandler.SetLocalStorageDeleter(localDriveStorage)
	// clean-remote-files / cleanup の実体削除を objectStorage queue へ逃がす (#2325)。
	adminHandler.SetObjectStorageEnqueuer(s.queueClient)
	adminHandler.SetAdminDB(s.db)
	adminHandler.SetUserIPRepo(userIPRepo)
	adminHandler.SetSecurityKeyRepo(userSecurityKeyRepo)
	adminHandler.SetEmojiImportEnqueuer(s.queueClient)
	// admin/emoji/fetch-remote-meta が使うメタデータ取得 (#2698)。**取得先の host は
	// 絵文字の host = 相手が決める値**なので、SSRF ガード付きの transport を組む。
	// **outbound の共通設定を渡す (#3037)。** 渡し忘れるとこの経路だけが
	// `config.proxy` を通らず、サーバーの素の IP でリモートへ出る (#638)。
	adminHandler.SetRemoteEmojiMetaFetcher(emojimeta.NewFetcher(s.config.AllowedPrivateNetworks, s.config.UserAgent, s.outboundOpts()...))
	adminHandler.SetRelayService(relaySvc)
	adminHandler.SetSystemWebhookRepo(systemWebhookRepo)
	// admin/system-webhook/test は webhookService.DispatchSystemTest で real
	// delivery (queue) を経由する。SSRF-safe transport は配送 processor 側
	// (NewWebhookProcessor) で適用済 (#1542、旧 SetWebhookTestClient を廃止)。
	adminHandler.SetRecipientRepo(recipientRepo)
	// resolve-abuse-user-report 時に abuseReportResolved system webhook を発火
	// する dispatcher (#1723)。
	adminHandler.SetSystemWebhookDispatcher(webhookService)
	adminHandler.SetAdRepo(adRepo)
	adminHandler.SetAvatarDecorationRepo(avatarDecorationRepo)
	adminHandler.SetInviteRepo(signupTicketRepo)
	adminHandler.SetPromoNoteRepo(repository.NewPromoNoteRepository(s.db))
	adminHandler.SetNoteFinder(noteRepo)
	if s.queueInspector != nil {
		adminHandler.SetQueueInspector(&queueInspectorAdapter{inner: s.queueInspector})
		// mk-go 独自の worker runtime block (#2277)。/metrics は無認証公開で
		// LB ACL 必須のため admin からは読めない。その情報を moderator が
		// control panel で見られるようにする。
		adminHandler.SetQueueRuntimeProvider(s.newQueueRuntimeProvider())
	}
	// per-queue db (Redis INFO: memory / clients / uptime) を admin queue 画面に
	// 出すため job-queue Redis を配線 (#1393)。
	if s.redis != nil && s.redis.JobQueue != nil {
		adminHandler.SetQueueRedis(jobQueueRedisInfo{rdb: s.redis.JobQueue})
	}
	// admin/server-metrics の uptime 起点 (#2395)。
	adminHandler.SetProcStatsDeps(procstats.Deps{StartedAt: s.startedAt})
	adminHandler.SetInstanceMetadataFetcher(metadataFetcher)
	adminHandler.SetSystemAccountFetcher(sysAcctSvc)
	// admin/federation/remove-all-following が host 単位で全 follower を
	// detach するために followingRepo + Unfollow enqueuer が必要 (#587)。
	// 実 row 削除と Reject(Follow) 配送は worker (UnfollowProcessor) が
	// followingService.Unfollow を呼んで行う。本家 TS の queueService.
	// createUnfollowJob と等価。
	adminHandler.SetFollowingRepo(followingRepo)
	adminHandler.SetUnfollowEnqueuer(s.queueClient)
	// suspend 時の双方向 followRequest 削除に使う (#1759)。
	adminHandler.SetFollowRequestRepo(followRequestRepo)

	// DB のバックアップの管理画面 (#3462、Elythia 独自)。閲覧を含む全ての操作に、
	// 管理者・ブラウザでログインした token・2FA かパスキーの登録・操作ごとの
	// 再認証を課す (strongauth)。RequireAdmin / RequireSecure は gate が読む
	// 目印でもあり、handler の中の再認証が同じ条件をもう一度見る (付け忘れても閉じる)。
	// 再認証は route の middleware でなく handler の中で行い、引数の検証より後に
	// 走らせる (#3878)。reauth-challenge はパスキーの challenge を出すだけの前段
	// なので、再認証は課さず、それ以外の条件を handler の中で見る。
	backupVerifierDeps := strongauth.Deps{
		Roles:    roleService,
		Profiles: userService,
		Keys:     userSecurityKeyRepo,
		Guard:    passwordFailureGuard,
		Replay:   totpReplayGuard,
	}
	if webauthnSvc != nil {
		backupVerifierDeps.Passkeys = webauthnSvc
	}
	backupVerifier, err := strongauth.New(backupVerifierDeps)
	if err != nil {
		panic("admin/backup: " + err.Error())
	}
	backupHandler := adminbackup.NewHandler(newBackupAdminService(s.config, s.redis.Default), backupVerifier, modLogService)
	// 画面からのリストア (#3463 (6c)) は daemon へ転送し、本体では再認証しない。本体でも
	// 再認証すると、TOTP のコードを本体と daemon で二重に消費して落ちる。再認証を
	// 含む全条件は daemon が DB を見て照合する。管理者と native token の確認は、本体が
	// 動いている間の前段として残す (メンテナンス中は maintmode が転送するので通らない)。
	backupHandler.SetForwarder(newRestoreForwarder(s.config))
	// ディレクトリの保存先のダウンロード。ブラウザのダウンロードは Authorization を
	// 付けられないので、admin/backup/download が出した短い期限の token で渡す
	// (S3 の署名付き URL と同じ扱い)。token は path でなく query の `token` に
	// 置く。本体のアクセスログは query の `token` を伏せ (internal/misc/redact)、
	// nginx の参照設定のアクセスログは query を含まない $uri を記録する。ただし
	// nginx の error_log はエラー行にリクエスト行をそのまま付けるので、そこには
	// 残りうる (伏せる手段が無い。有効なのは 5 分)。
	s.echo.GET("/backup-download", backupHandler.ServeDownload)
	return adminHandler, emojiApplicationService, emojiApplicationHandler, backupHandler
}
