package server

import (
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/api/i"
	"github.com/elythia-network/elythia/internal/api/users"
	"github.com/elythia-network/elythia/internal/api/webhooks"
	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	"github.com/elythia-network/elythia/internal/core/chart/charthook"
	"github.com/elythia-network/elythia/internal/core/event"
	"github.com/elythia-network/elythia/internal/core/featured"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	coremove "github.com/elythia-network/elythia/internal/core/move"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	coretransfer "github.com/elythia-network/elythia/internal/core/transfer"
	coretwofactor "github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/core/userlist"
	"github.com/elythia-network/elythia/internal/core/userpack"
	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/stream"
)

// accountsDeps holds the parts wireAccounts receives from setupRoutes
// (#3901). setupRoutes sets every field from the variable of the same name;
// TestSetupRoutesSectionsReceiveSameNamedVariables checks that no field is
// left out or filled from a different variable.
type accountsDeps struct {
	idGen                      id.Generator
	streamPubSub               *event.PubSubService
	relationReloadPublisher    *relationReloadPublisherAdapter
	mainStreamPublisher        *stream.MainStreamPublisher
	userListStreamPublisher    *stream.UserListStreamPublisher
	userRepo                   repository.UserRepository
	noteRepo                   repository.NoteRepository
	metaRepo                   repository.MetaRepository
	followingRepo              repository.FollowingRepository
	followRequestRepo          repository.FollowRequestRepository
	piningRepo                 repository.UserNotePiningRepository
	reactionRepo               repository.NoteReactionRepository
	emojiRepo                  *repository.CachedEmojiRepository
	blockingRepo               repository.BlockingRepository
	mutingRepo                 repository.MutingRepository
	renoteMutingRepo           repository.RenoteMutingRepository
	driveFileRepo              repository.DriveFileRepository
	instanceRepo               repository.InstanceRepository
	antennaRepo                repository.AntennaRepository
	clipRepo                   repository.ClipRepository
	clipNoteRepo               repository.ClipNoteRepository
	pageRepo                   repository.PageRepository
	pageLikeRepo               repository.PageLikeRepository
	flashRepo                  repository.FlashRepository
	noteFavoriteRepo           repository.NoteFavoriteRepository
	userListRepo               repository.UserListRepository
	chatRepo                   repository.ChatRepository
	clipFavoriteRepo           repository.ClipFavoriteRepository
	userListFavoriteRepo       repository.UserListFavoriteRepository
	channelNoteUnreadRepo      repository.ChannelNoteUnreadRepository
	antennaNoteUnreadRepo      repository.AntennaNoteUnreadRepository
	signinRepo                 repository.SigninRepository
	avatarDecorationRepo       repository.AvatarDecorationRepository
	moveSystemAccountRepo      repository.SystemAccountRepository
	userSecurityKeyRepo        repository.UserSecurityKeyRepository
	registryRepo               repository.RegistryRepository
	abuseReportRepo            repository.AbuseReportRepository
	userMemoRepo               repository.UserMemoRepository
	galleryRepo                repository.GalleryRepository
	chartHooks                 *charthook.Hooks
	roleService                *corerole.Service
	sharedUserPacker           *userpack.Packer
	coreSvcs                   core
	featuredService            *featured.Service
	instanceService            *coreinstance.Service
	followingService           *corefollowing.Service
	antennaService             *coreantenna.Service
	notificationService        *notification.Service
	webhookService             *corewebhook.Service
	driveReader                *coretransfer.RepoBackedDriveReader
	listProxyFollow            *userlist.ProxyFollower
	passwordFailureGuard       *passwordguard.RedisGuard
	apURLs                     *activitypub.URLBuilder
	apRenderer                 *activitypub.Renderer
	deliverService             *corefederation.DeliverService
	userModerationDeliveryHook *corefederation.UserModerationDeliveryHook
	reactionCountWriter        *corereaction.SwitchableWriter
	localHost                  string
	federationResolver         *corefederation.Resolver
	federationProcessor        *corefederation.Processor
	webauthnSvc                *coretwofactor.WebAuthnService
	noteFieldResolver          *entity.NoteFieldResolver
	usersHandler               *users.Handler
	iHandler                   *i.Handler
	webhookHandler             *webhooks.Handler
}

// wireAccounts wires the users API handler (lookups, relations, remote
// stats, user lists) and the account API handler (profile updates, 2FA,
// account migration, export / import and account deletion, webhooks).
// setupRoutes calls it where these statements used to be and receives the
// parts it returns under the same names. It binds every field of d to the
// name setupRoutes uses, so the moved statements read the same as before
// (#3901).
func (s *Server) wireAccounts(d accountsDeps) (*corefederation.RemoteStatsFetcher, *coremove.Service) {
	idGen := d.idGen
	streamPubSub := d.streamPubSub
	relationReloadPublisher := d.relationReloadPublisher
	mainStreamPublisher := d.mainStreamPublisher
	userListStreamPublisher := d.userListStreamPublisher
	userRepo := d.userRepo
	noteRepo := d.noteRepo
	metaRepo := d.metaRepo
	followingRepo := d.followingRepo
	followRequestRepo := d.followRequestRepo
	piningRepo := d.piningRepo
	reactionRepo := d.reactionRepo
	emojiRepo := d.emojiRepo
	blockingRepo := d.blockingRepo
	mutingRepo := d.mutingRepo
	renoteMutingRepo := d.renoteMutingRepo
	driveFileRepo := d.driveFileRepo
	instanceRepo := d.instanceRepo
	antennaRepo := d.antennaRepo
	clipRepo := d.clipRepo
	clipNoteRepo := d.clipNoteRepo
	pageRepo := d.pageRepo
	pageLikeRepo := d.pageLikeRepo
	flashRepo := d.flashRepo
	noteFavoriteRepo := d.noteFavoriteRepo
	userListRepo := d.userListRepo
	chatRepo := d.chatRepo
	clipFavoriteRepo := d.clipFavoriteRepo
	userListFavoriteRepo := d.userListFavoriteRepo
	channelNoteUnreadRepo := d.channelNoteUnreadRepo
	antennaNoteUnreadRepo := d.antennaNoteUnreadRepo
	signinRepo := d.signinRepo
	avatarDecorationRepo := d.avatarDecorationRepo
	moveSystemAccountRepo := d.moveSystemAccountRepo
	userSecurityKeyRepo := d.userSecurityKeyRepo
	registryRepo := d.registryRepo
	abuseReportRepo := d.abuseReportRepo
	userMemoRepo := d.userMemoRepo
	galleryRepo := d.galleryRepo
	chartHooks := d.chartHooks
	roleService := d.roleService
	sharedUserPacker := d.sharedUserPacker
	coreSvcs := d.coreSvcs
	featuredService := d.featuredService
	instanceService := d.instanceService
	followingService := d.followingService
	antennaService := d.antennaService
	notificationService := d.notificationService
	webhookService := d.webhookService
	driveReader := d.driveReader
	listProxyFollow := d.listProxyFollow
	passwordFailureGuard := d.passwordFailureGuard
	apURLs := d.apURLs
	apRenderer := d.apRenderer
	deliverService := d.deliverService
	userModerationDeliveryHook := d.userModerationDeliveryHook
	reactionCountWriter := d.reactionCountWriter
	localHost := d.localHost
	federationResolver := d.federationResolver
	federationProcessor := d.federationProcessor
	webauthnSvc := d.webauthnSvc
	noteFieldResolver := d.noteFieldResolver
	usersHandler := d.usersHandler
	iHandler := d.iHandler
	webhookHandler := d.webhookHandler

	// Users endpoints
	// users/lists/update-membership の withReplies 変更を userUpdated として流し、接続中の
	// userList channel snapshot を live 更新する (#2051)。
	usersHandler.SetUserListEventPublisher(userListStreamPublisher)
	usersHandler.SetLocalHost(localHost)
	usersHandler.SetServerURL(s.config.URL)
	usersHandler.SetChartHook(chartHooks)
	usersHandler.SetFeaturedRanking(featuredService) // #1687: users/featured-notes ランキング
	usersHandler.SetPiningRepo(piningRepo)
	usersHandler.SetFollowingRepo(followingRepo)
	usersHandler.SetProxyFollow(listProxyFollow) // #1704: create-from-public remote member proxy follow
	usersHandler.SetRenoteMutingRepo(renoteMutingRepo)
	usersHandler.SetFollowRequestRepo(followRequestRepo)
	usersHandler.SetInstanceRepo(instanceRepo)
	usersHandler.SetEmojiRepo(emojiRepo)
	usersHandler.SetReactionReader(reactionCountWriter)
	usersHandler.SetClipRepo(clipRepo)
	usersHandler.SetClipFavoriteRepo(clipFavoriteRepo) // #1562: users/clips の favoritedCount / isFavorited
	usersHandler.SetClipNoteRepo(clipNoteRepo)         // #2243: users/clips の notesCount 実カウント
	usersHandler.SetFlashRepo(flashRepo)
	usersHandler.SetGalleryRepo(galleryRepo)
	usersHandler.SetPageRepo(pageRepo)
	// users/pages の page も attachedFiles / eyeCatchingImage を解決する (#1662)。
	usersHandler.SetDriveFileRepo(driveFileRepo)
	usersHandler.SetNoteFieldResolver(noteFieldResolver)
	usersHandler.SetUserRepo(userRepo)
	usersHandler.SetNoteReactionRepo(reactionRepo)
	sharedUserPacker.SetDetailExtras(usersHandler)
	sharedUserPacker.SetDetailExtrasMany(usersHandler) // #3330: 一覧の DetailedMany のピン留め・移行先・instance・絵文字
	// **連合を切った相手へ取りに行かない。** この経路は未認証の
	// `/api/users/show` から呼ばれるので、放っておくと defederate した相手に
	// 「誰をいつ見たか」が漏れる。判定は構築子の必須の引数で渡す (#3823)。
	remoteStatsFetcher := corefederation.NewRemoteStatsFetcher(s.config.AllowedPrivateNetworks, s.config.UserAgent, instanceService.CanFetchOptionalRemoteData, s.outboundOpts()...)
	// フォローしてきたリモートの人のアカウントの作成日時を、Misskey 系の相手
	// なら `/api/users/show` から埋める (#3465)。統計と同じ fetcher を使うので、
	// 待ち時間の上限・キャッシュ・SSRF 対策・連合を切った相手の扱いが揃う。
	federationProcessor.SetAccountCreatedAtFiller(corefederation.NewAccountCreatedAtFiller(remoteStatsFetcher, instanceRepo, userRepo).Fill)
	usersHandler.SetRemoteStatsFetcher(&remoteStatsFetcherAdapter{
		fetcher: remoteStatsFetcher,
	})

	usersHandler.SetAbuseRepo(abuseReportRepo)
	usersHandler.SetMemoRepo(userMemoRepo)
	usersHandler.SetUserListFavoriteRepo(userListFavoriteRepo)
	usersHandler.SetUserListRepo(userListRepo)
	// users/reactions で moderator/admin が remote / 非 public reactions を
	// 閲覧できるようにする (upstream の iAmModerator bypass)。
	usersHandler.SetModeratorChecker(roleService)

	// Account endpoints
	iHandler.SetHardMutePublisher(&hardMutePublisherAdapter{pubsub: streamPubSub})
	iHandler.SetRelationReloadPublisher(relationReloadPublisher)
	iHandler.SetMainStreamPublisher(mainStreamPublisher)
	iHandler.SetUserRepo(userRepo)
	// フォロー申請の作成・承認・取り消しで followee に meUpdated を流す (#3330)。
	followingService.SetMeUpdatedPublisher(iHandler)
	iHandler.SetPasswordFailureGuard(passwordFailureGuard)
	iHandler.SetPasswordHasher(s.passwordHasher)
	// upstream UserAuthService と同じテスト用バイパス。testMode 以外では無効。
	iHandler.SetTestMode(s.config.TestMode)
	iHandler.SetRegistryRepo(registryRepo)
	iHandler.SetMetaRepo(metaRepo)
	iHandler.SetServerURL(s.config.URL)
	// i/update-email の verifymail / truemail SaaS 呼び出しも SSRF-safe
	// transport + forward proxy 経由にする (#638)。
	iHandler.SetEmailValidationClient(s.outboundClient(10 * time.Second))
	iHandler.SetVerifyLinkClient(s.outboundClient(10 * time.Second)) // #1786: rel=me link 検証
	// SMTP メール送信を i/update-email 用に注入する。meta の SMTP 設定に従い、
	// SenderFromMeta が closure 内で per-call 再 Fetch するため admin UI の
	// 設定変更は即座に反映される (#1112)。smtpSecure 反映は #1111。
	iHandler.SetEmailSender(miscsmtp.SenderFromMeta(metaRepo, s.config.ProxySMTP))
	if webauthnSvc != nil {
		iHandler.SetWebAuthn(webauthnSvc, userSecurityKeyRepo)
	}
	iHandler.SetSigninRepo(signinRepo)
	// i/import-* の fileId 所有権検証 (#1555)。未配線だと fail-closed
	// (NO_SUCH_FILE) になるため production では必ず wire する。
	iHandler.SetDriveFileRepo(driveFileRepo)
	// i/import-antennas の TOO_MANY_ANTENNAS 同期 enforce 用 (#1667)。
	iHandler.SetImportDriveReader(driveReader)
	iHandler.SetAntennaCounter(antennaRepo)
	// P4-6 (#166): i/gallery/*, i/page-likes。i/authorized-apps と i/revoke-token の
	// accessTokenRepo は構築子の引数で渡す (#3827)。
	iHandler.SetGalleryRepo(galleryRepo)
	iHandler.SetPageLikeRepo(pageLikeRepo)
	// P4-6 followup (#174): i/move は federation resolver + deliverService を使って
	// 宛先 actor 解決 → alsoKnownAs 検証 → MovedTo 書き込み → Move activity 配送。
	// #2419: フォロワー以外の関連データ (ブロック / ミュート / リスト) も移行先へ
	// 引き継ぐ。いずれも「旧側を消さずに新側を足す」片方向で、旧アカウントの関係は
	// 残す。引き継ぎの repository は構築子の必須の引数で渡す (#3823)。
	accountMover := coremove.NewService(userRepo, followingRepo, apURLs, apRenderer, federationResolver, deliverService,
		blockingRepo, mutingRepo, userListRepo, idGen)
	accountMover.SetLocalFederationObserver(coreSvcs.localFederation)
	// #2418: 移行時に旧アカウントのローカルフォロワーを移行先へ follow させる。
	// relationship queue (#2403) に載るので AP 配送を詰まらせない。
	accountMover.SetFollowQueue(s.queueClient)
	// proxy account はリスト購読のため機械的に follow しているだけなので
	// 引き継ぎ対象から外す。newProxyAccountResolver は username を返す別用途の
	// resolver なので、ここでは user id を引く専用のもの (moveSystemAccountRepo) を渡す。
	accountMover.SetProxyAccountResolver(func() (string, bool) {
		sa, err := moveSystemAccountRepo.FindByType("proxy")
		if err != nil || sa == nil {
			return "", false
		}
		return sa.UserID, true
	})
	accountMover.SetBlockQueue(s.queueClient)
	accountMover.SetRoleAssigner(roleService)
	accountMover.SetAntennaMover(antennaService)
	// 本家 adjustFollowingCounts と同じく、旧アカウントがリモートなら instance の
	// followersCount をローカルのフォロワー数ぶん減らし、chart も動かす (#3330)。
	accountMover.SetInstanceStats(instanceRepo, corefollowing.MetaInstanceStatsGate(metaRepo), chartHooks)
	iHandler.SetAccountMover(accountMover)
	// #2414: リモートアカウントの移行を検知したら、同じ引き継ぎ処理を走らせる。
	// federation → core/move の一方向依存で循環しない。
	federationResolver.SetMoveProcessor(accountMover)
	// #2415: 移行直後 (相互確認済み + 2h 以内) だけ import の上限を緩和する。
	iHandler.SetMoveInValidator(accountMover)
	iHandler.SetFavoriteRepo(noteFavoriteRepo)
	// Phase 7-2 (#244): /api/i の未読系フィールドを実クエリ化。
	iHandler.SetNotificationService(notificationService)
	iHandler.SetAchievementNotifier(notificationService)
	iHandler.SetFollowRequestRepo(followRequestRepo)
	iHandler.SetChatRepo(chatRepo)
	// Phase 7-2 follow-up (#271)
	iHandler.SetAntennaUnreadRepo(antennaNoteUnreadRepo)
	iHandler.SetChannelUnreadRepo(channelNoteUnreadRepo)
	iHandler.SetMeUnreadFlagsReader(repository.NewMeUnreadFlagsRepository(s.db)) // #3608: 未読の真偽値 (note_unread を含む) を 1 文で引く
	// Phase 7-3 (#245): pinnedNoteIds / pinnedNotes / pinnedPageId / pinnedPage
	iHandler.SetPiningRepo(piningRepo)
	iHandler.SetNoteRepo(noteRepo)
	iHandler.SetPageRepo(pageRepo)
	iHandler.SetInstanceRepo(instanceRepo)
	iHandler.SetEmojiRepo(emojiRepo)
	iHandler.SetReactionReader(reactionCountWriter)
	iHandler.SetAvatarDecorationRepo(avatarDecorationRepo)
	iHandler.SetNoteFieldResolver(noteFieldResolver)
	// announcementRepo (/api/i の未読のお知らせ) は、下の meself.NewPacker の直前で入れる。

	// i/export-* and i/import-* (Phase 9.4)
	iHandler.SetTransferEnqueuer(s.queueClient)
	// #2230: i/delete-account の cascade 削除 (notes/drive/following purge) と AP Delete(actor)
	// 配信を admin/delete-account と同経路で配線する。未配線だと論理削除フラグだけ立って実削除が走らない。
	iHandler.SetDeleteAccountEnqueuer(s.queueClient)
	iHandler.SetAccountDeletionFederationHook(userModerationDeliveryHook)

	// i/webhooks/* — Webhook管理 (実データ)
	webhookHandler.SetDispatcher(webhookService)
	return remoteStatsFetcher, accountMover
}
