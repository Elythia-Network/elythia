package server

import (
	"time"

	coreabuse "github.com/elythia-network/elythia/internal/core/abuse"
	coreachievement "github.com/elythia-network/elythia/internal/core/achievement"
	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	corechannel "github.com/elythia-network/elythia/internal/core/channel"
	coreclip "github.com/elythia-network/elythia/internal/core/clip"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	coreephemeral "github.com/elythia-network/elythia/internal/core/ephemeral"
	"github.com/elythia-network/elythia/internal/core/event"
	corefeatured "github.com/elythia-network/elythia/internal/core/featured"
	coreflash "github.com/elythia-network/elythia/internal/core/flash"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	corehashtag "github.com/elythia-network/elythia/internal/core/hashtag"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	coremediaproxy "github.com/elythia-network/elythia/internal/core/mediaproxy"
	coremodlog "github.com/elythia-network/elythia/internal/core/moderationlog"
	coremuting "github.com/elythia-network/elythia/internal/core/muting"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	corepage "github.com/elythia-network/elythia/internal/core/page"
	corepoll "github.com/elythia-network/elythia/internal/core/poll"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	coreretention "github.com/elythia-network/elythia/internal/core/retention"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	coresignup "github.com/elythia-network/elythia/internal/core/signup"
	coresystemaccount "github.com/elythia-network/elythia/internal/core/systemaccount"
	coretimeline "github.com/elythia-network/elythia/internal/core/timeline"
	coretransfer "github.com/elythia-network/elythia/internal/core/transfer"
	coretwofactor "github.com/elythia-network/elythia/internal/core/twofactor"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	coreuserlist "github.com/elythia-network/elythia/internal/core/userlist"
	"github.com/elythia-network/elythia/internal/core/userpack"
	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
	corewebpush "github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/stream"
)

// core holds the core services setupRoutes builds through buildCore (#3686).
//
// Fields are named after the local variables setupRoutes has always used,
// without the Service / Svc suffix, and keep the concrete type the
// constructor returns. Only services whose constructor just builds a value
// are listed: no DB or Redis access, no outbound call or log, no goroutine,
// timer, shutdown hook or global write, and dependencies limited to the
// repositories, the services built before them here and the values
// buildCore receives as arguments (roleService from buildRole, the stream
// pubsub and the two packers, #3825). Services that need a stream publisher
// other than one built from that pubsub, a handler, a federation part or a
// value read from the DB at construction are not built here: the relay service is built by
// buildFederation (#3688), the others stay in setupRoutes
// (docs/architecture.md 4.2).
//
// buildCore only constructs. The setters on these services are still called
// at their old positions in setupRoutes, because workers started during
// construction must see the same wiring as before (docs/architecture.md 4.1).
// The exception is the dependencies #3825 turned into required constructor
// arguments: they are passed here instead of through a setter.
type core struct {
	featured             *corefeatured.Service
	hashtag              *corehashtag.Service
	modLog               *coremodlog.Service
	reversiFedCache      *corereversi.FederationIDCache
	totpReplayGuard      *coretwofactor.RedisReplayGuard
	webPush              *corewebpush.Service
	instance             *coreinstance.Service
	role                 *corerole.Service
	signup               *coresignup.Service
	noteCreate           *corenote.CreateService
	noteDelete           *corenote.DeleteService
	noteQuery            *corenote.QueryService
	user                 *coreuser.Service
	following            *corefollowing.Service
	fanoutTimeline       *coretimeline.FanoutTimelineService
	timelineFanoutHook   *coretimeline.FanoutHook
	ephemeralStore       *coreephemeral.Store
	timeline             *coretimeline.Service
	channel              *corechannel.Service
	antenna              *coreantenna.Service
	clip                 *coreclip.Service
	page                 *corepage.Service
	flash                *coreflash.Service
	reaction             *corereaction.Service
	notification         *corenotification.Service
	notificationHook     *corenotification.Hook
	webhook              *corewebhook.Service
	webhookNoteHook      *corewebhook.NoteCreateHook
	webhookReactionHook  *corewebhook.ReactionCreateHook
	webhookFollowingHook *corewebhook.FollowingHook
	blocking             *coreblocking.Service
	muting               *coremuting.Service
	renoteMuting         *coremuting.RenoteService
	poll                 *corepoll.Service
	localDriveStorage    *coredrive.LocalStorage
	driveStorage         *coredrive.MetaStorage
	drive                *coredrive.Service
	imgProcessor         *coredrive.DefaultImageProcessor
	driveReader          *coretransfer.RepoBackedDriveReader
	importer             *coretransfer.Importer
	sysAcct              *coresystemaccount.Service
	listProxyFollow      *coreuserlist.ProxyFollower
	retention            *coreretention.Service
	achievement          *coreachievement.Service
	proxyAllowlist       *coremediaproxy.DBAllowlistChecker
	abuseInAppNotifier   *coreabuse.InAppNotifier
}

// buildRole builds the role service from w.repos (#3825).
//
// role.Service は buildCore の前に作る。entity の Packer と共有の userpack の
// Packer が roleService に依存し、buildCore で作る部品の構築子がその 2 つを
// 受け取るため。作るだけで DB にも Redis にも触らない (buildCore と同じ条件)。
func (s *Server) buildRole(w *wiring, idGen id.Generator) *corerole.Service {
	roleRepo := w.repos.role
	roleAssignmentRepo := w.repos.roleAssignment
	var metaRepo repository.MetaRepository = w.repos.cachedMeta
	return corerole.NewService(roleRepo, roleAssignmentRepo, metaRepo, idGen)
}

// buildCore builds the core services from w.repos, in the order setupRoutes
// used to build them, except that blocking is built before reaction so that
// reaction can take it as a constructor argument (#3825). It reads s.redis, s.config, s.queueClient and s.db.
// roleService is built by buildRole before it (#3825) and is stored in the
// role field as is. streamPubSub is the stream pubsub setupRoutes creates
// first; buildCore only builds publishers on it and never subscribes.
// entityPacker and sharedUserPacker are the packers setupRoutes builds right
// after buildRole (#3825); the parts that pack entities take them as required
// constructor arguments.
func (s *Server) buildCore(w *wiring, idGen id.Generator, roleService *corerole.Service, streamPubSub *event.PubSubService,
	entityPacker *entity.Packer, sharedUserPacker *userpack.Packer) core {
	// 下の行は setupRoutes にあった行をそのまま移した。ゲートが構築の式を引数まで
	// 含めて照合するので、repository も同じ名前の別名で受ける。
	userRepo := w.repos.user
	noteRepo := w.repos.note
	cachedMeta := w.repos.cachedMeta
	var metaRepo repository.MetaRepository = cachedMeta
	pollRepo := w.repos.poll
	followingRepo := w.repos.following
	followRequestRepo := w.repos.followRequest
	piningRepo := w.repos.pining
	reactionRepo := w.repos.reaction
	emojiRepo := w.repos.emoji
	blockingRepo := w.repos.blocking
	mutingRepo := w.repos.muting
	renoteMutingRepo := w.repos.renoteMuting
	pollVoteRepo := w.repos.pollVote
	driveFileRepo := w.repos.driveFile
	driveFolderRepo := w.repos.driveFolder
	keypairRepo := w.repos.keypair
	instanceRepo := w.repos.instance
	channelRepo := w.repos.channel
	channelFollowingRepo := w.repos.channelFollowing
	cachedAntenna := w.repos.cachedAntenna
	var antennaRepo repository.AntennaRepository = cachedAntenna
	clipRepo := w.repos.clip
	clipNoteRepo := w.repos.clipNote
	pageRepo := w.repos.page
	pageLikeRepo := w.repos.pageLike
	flashRepo := w.repos.flash
	flashLikeRepo := w.repos.flashLike
	userListRepo := w.repos.userList
	webhookRepo := w.repos.webhook
	systemWebhookRepo := w.repos.systemWebhook
	retentionRepo := w.repos.retention
	hashtagRepo := w.repos.hashtag
	modLogRepo := w.repos.modLog
	systemAccountRepo := w.repos.systemAccount
	chatRepo := w.repos.chat
	signupTicketRepo := w.repos.signupTicket
	signupApplicationRepo := w.repos.signupApplication

	// featured engagement ランキング (#1687)。reaction / renote の hook が
	// global / in-channel / per-user の Redis windowed ZSET を更新し、
	// notes/featured・users/featured-notes が読む。upstream と同じ default Redis。
	featuredService := corefeatured.NewService(s.redis.Default)
	// Hashtag service: ノート作成 (local / federation 両経路) で hashtag table の
	// mentionedUsersCount / mentionedUserIds を更新する (#680)。/api/hashtags/list
	// 等の trends ranking はこの集計を見るので、未配線だと空集合のままになる。
	hashtagService := corehashtag.NewService(hashtagRepo, idGen)
	// moderation log (#1548 / #1765)。admin / announcement / gallery / flash と、moderator の
	// note 削除が同じ instance を共有する。
	modLogService := coremodlog.New(modLogRepo, idGen, s.background) // #3661: 世代の停止で待つ
	// reversi の連合 (Phase 9.7)。FederationIDCache は本家 Misskey DB スキーマ互換の
	// ため DB カラムを持たず Redis のみで session↔gameID の双方向 mapping を保持する。
	// api handler と federation inbox で同じインスタンスを共有する。
	reversiFedCache := corereversi.NewFederationIDCache(s.redis.Default)
	// TOTP replay guard — accept した TOTP コードを Redis に SETNX で
	// 記録し、acceptance window 内 (~90s) の再利用を refuse する
	// (RFC 6238 §5.2)。mk-go が先行実装した hardening だが、upstream も
	// 2026.6.0 の UserAuthService.validateOtp で同等機構 (Redis SET NX EX、
	// TTL 90s) を入れたため現在は挙動が揃っている。
	// signin と /api/i の 2 経路で共通の guard を共有する。
	totpReplayGuard := coretwofactor.NewRedisReplayGuard(s.redis.Default)
	// Web Push (Phase 9.3)。通知・チャット・既読の push を queue へ積む。
	webPushService := corewebpush.NewService(s.queueClient)
	// Instance management (Phase 3 Step H)。インスタンスの停止・サイレンスの判定を、
	// 連合・following・reaction・配送が共有する。
	instanceService := coreinstance.NewService(instanceRepo, metaRepo, idGen)
	// PromotePending を transaction 化して partial failure rollback と invitation
	// ticket の SELECT FOR UPDATE ロックを有効化する (#600 item 2 + #604)。
	// 承認制の登録を確認メールに乗せた経路 (#2571) で、申請の確定もアカウント作成と
	// 同じ tx に入れる (#2576)。
	signupService := coresignup.NewService(userRepo, metaRepo, idGen, s.db, signupTicketRepo, signupApplicationRepo)
	// 世代の箱 (#3661: 世代の停止で待つ)、main stream の投稿を包む Packer (#3790)、
	// 本家互換の返信・引用のブロックの検査 (#254)、meta のセンシティブワードと禁止ワード、
	// canPublicNote=false の利用者の public 投稿を home に降格させる silencing (#1024)、
	// fileIds の所有者の検査。
	noteCreateService := corenote.NewCreateService(noteRepo, pollRepo, idGen, followingRepo,
		s.background, entityPacker, blockingRepo, metaRepo, roleService, driveFileRepo)
	// 投稿後のフックの同時実行の枠を設定の数で作り直す(#3780)。
	noteCreateService.SetHookConcurrency(s.config.NoteHookConcurrency)
	noteDeleteService := corenote.NewDeleteService(noteRepo)
	noteQueryService := corenote.NewQueryService(noteRepo, followingRepo)
	// 利用者の詳細を包む Packer (#3792)。
	userService := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen, entityPacker)
	// サイレンスしたホストからのフォローを承認制にする (upstream UserFollowingService の
	// 4 つ目の OR 条件)。「静かに成立させる」(#3466) で通したフォローを記録する
	// (following/silent/list が読む)。follow 系の main stream は共有の userpack の Packer で
	// 本家の UserDetailedNotMe に揃える (#3330)。
	followingService := corefollowing.NewService(userRepo, followingRepo, followRequestRepo, idGen,
		entityPacker, instanceService, repository.NewSilentFollowRepository(s.db), sharedUserPacker)
	// keyPrefix で TS 本家と同じ `<host>:list:*` 名前空間に揃える (#362)。
	fanoutTimelineService := coretimeline.NewFanoutTimelineService(
		s.redis.Timelines, idGen, s.config.RedisForTimelines.KeyPrefix())
	timelineFanoutHook := coretimeline.NewFanoutHook(fanoutTimelineService, followingRepo)
	// リレー経由でしか観測しない投稿の置き場 (#2332)。DB に入れず Redis に
	// TTL 付きで置き、ローカルユーザーが触ったときだけ materialize する。
	// meta を都度読むのは、起動時固定にすると管理画面の変更が再起動まで
	// 効かないため (#2315 と同じ轍を踏まない)。
	ephemeralStore := coreephemeral.NewStore(
		s.redis.Timelines, s.config.RedisForTimelines.KeyPrefix(),
		func() coreephemeral.Settings {
			m, err := metaRepo.Fetch()
			if err != nil || m == nil {
				return coreephemeral.Settings{}
			}
			return coreephemeral.Settings{
				Enabled: m.EnableEphemeralRelayNotes,
				TTL:     time.Duration(m.EphemeralRelayNoteTTLMinutes) * time.Minute,
			}
		})
	timelineService := coretimeline.NewService(fanoutTimelineService, noteRepo, followingRepo)
	channelService := corechannel.NewService(channelRepo, channelFollowingRepo, noteRepo, idGen)
	antennaService := coreantenna.NewService(antennaRepo, userRepo, s.redis.Default, idGen, roleService) // #1029: antennaLimit
	// roleService は clipLimit / noteEachClipsLimit (#1029)。クリップの更新・ノートの追加と
	// 削除・クリップの削除を stream の clip チャンネルへ流す (本家 2026.10.1、#3809)。
	clipService := coreclip.NewService(clipRepo, clipNoteRepo, noteRepo, idGen,
		roleService, stream.NewClipStreamPublisher(streamPubSub))
	pageService := corepage.NewService(pageRepo, pageLikeRepo, idGen)
	flashService := coreflash.NewService(flashRepo, flashLikeRepo, idGen)
	// block 時に保留中の follow request を双方向で取り消す (upstream
	// UserBlockingService.block の cancelRequest 相当)。既存のフォローも双方向に解除する。
	// following.Service の unfollow を通すので、カウント・チャート・Undo(Follow) /
	// Reject(Follow) の配送・unfollow のイベントが通常の解除と同じになる (#3330)。
	// ブロックされた側のリストからブロックした人を外し (本家 removeFromList、#3330)、
	// 互いの持つチャットルームへの招待を消す (本家 removeRoomInvitations)。
	// reactionService が blockingChecker に使うので、reactionService より前で作る (#3825)。
	blockingService := coreblocking.NewService(userRepo, blockingRepo, idGen,
		followingService, followingService, userListRepo, chatRepo)
	// #1538: reactionAcceptance gate — role-gated emoji + media-silenced host。
	// 自分をブロックしている相手の投稿へのリアクションも弾く。
	reactionService := corereaction.NewService(noteRepo, reactionRepo, emojiRepo, followingRepo, idGen,
		roleService, instanceService, blockingService)
	// keyPrefix で TS 本家と同じ `<host>:notificationTimeline:*` 等に揃える (#362)。
	notificationService := corenotification.NewService(s.redis.Default, idGen, s.config.Redis.KeyPrefix(),
		s.background) // #3661: 世代の停止で送り切る
	notificationHook := corenotification.NewHook(notificationService, userRepo)
	webhookService := corewebhook.NewService(s.queueClient, webhookRepo, systemWebhookRepo, s.config.URL)
	// payload の投稿を包む Packer (#3790)。follow / unfollow の Webhook は main stream と
	// 同じ共有の userpack の Packer で包む (#3269、#3330)。
	webhookNoteHook := corewebhook.NewNoteCreateHook(webhookService, idGen, entityPacker)
	webhookReactionHook := corewebhook.NewReactionCreateHook(webhookService, idGen, entityPacker)
	webhookFollowingHook := corewebhook.NewFollowingHook(webhookService, sharedUserPacker)
	mutingService := coremuting.NewService(userRepo, mutingRepo, idGen)
	renoteMutingService := coremuting.NewRenoteService(userRepo, renoteMutingRepo, idGen)
	// note 著者にブロックされた user の投票を弾く (upstream youHaveBeenBlocked、#1538)。
	pollService := corepoll.NewService(noteRepo, pollRepo, pollVoteRepo, followingRepo, idGen, blockingService)
	// localDriveStorage は driveStorage が S3 に切り替わっても、
	// storedInternal=true な drive_file を `/files/:accessKey` で提供する
	// fallback として常に保持する (#1414)。
	localDriveStorage := coredrive.NewLocalStorage("./drive-files", s.config.DriveURL)
	// backend は meta から都度解決する (#2315)。起動時に固定していた頃は、
	// コントロールパネルでオブジェクトストレージを有効にしても再起動するまで
	// ローカルに書き続けていた。upstream Misskey は metaUpdated で meta を
	// in-place 更新し、アップロードのたびに S3 client を組み立てるので即反映
	// される。指紋が変わらない限り backend は使い回すので、都度解決でも
	// S3 client を作り直すコストはかからない。
	driveStorage := coredrive.NewMetaStorage(metaRepo.Fetch, "./drive-files", s.config.DriveURL)
	// ファイルを包む Packer (#3796)。drive/files/show は moderator なら他人 / リモート
	// ユーザー所有の file も返す (upstream の roleService.isModerator 経路と一致)。
	// moderator が他人の drive ファイルのセンシティブを変えたとき・削除したときの
	// 監査ログ (#3678)。
	driveService := coredrive.NewService(driveFileRepo, driveFolderRepo, driveStorage, idGen,
		entityPacker, roleService, modLogService, userRepo)
	imgProcessor := coredrive.NewDefaultImageProcessor()
	driveReader := coretransfer.NewRepoBackedDriveReader(driveFileRepo, driveStorage)
	importer := coretransfer.NewImporter(coretransfer.ImporterDeps{
		UserRepo:     userRepo,
		UserListRepo: userListRepo,
		AntennaRepo:  antennaRepo,
		Drive:        driveReader,
		Following:    coretransfer.NewFollowingServiceAdapter(followingService),
		Blocking:     blockingService,
		Muting:       mutingService,
		Notifier:     notificationService,
		IDGen:        idGen,
		SelfHost:     s.config.Host,
	})
	sysAcctSvc := coresystemaccount.NewService(userRepo, keypairRepo, systemAccountRepo, idGen)
	// #1704: list に remote user を追加したとき proxy account がフォローして連合
	// 投稿を受信できるようにする enqueuer。push / create-from-public で使う。
	listProxyFollow := coreuserlist.NewProxyFollower(sysAcctSvc, s.queueClient)
	retentionSvc := coreretention.NewService(userRepo, retentionRepo, idGen)
	// #1762: local note を favorite されたとき著者に myNoteFavorited1 を付与する
	// server-side achievement infra。claim-achievement と同じ profile.achievements
	// を更新する。
	achievementService := coreachievement.NewService(userService, notificationService)
	proxyAllowlist := coremediaproxy.NewDBAllowlistChecker(s.db)
	// 最後の引数は buildRepos の abuseReportRepo と同じ種類だが、ゲート
	// (TestAbuseReportNotifierIsWired) が構築の式そのものを照合するので、ここで作る
	// (#3684)。db しか持たないので、2 つあっても動きは同じ。
	abuseInAppNotifier := coreabuse.NewInAppNotifier(roleService, notificationService, repository.NewAbuseReportRepository(s.db))

	return core{
		featured:             featuredService,
		hashtag:              hashtagService,
		modLog:               modLogService,
		reversiFedCache:      reversiFedCache,
		totpReplayGuard:      totpReplayGuard,
		webPush:              webPushService,
		instance:             instanceService,
		role:                 roleService,
		signup:               signupService,
		noteCreate:           noteCreateService,
		noteDelete:           noteDeleteService,
		noteQuery:            noteQueryService,
		user:                 userService,
		following:            followingService,
		fanoutTimeline:       fanoutTimelineService,
		timelineFanoutHook:   timelineFanoutHook,
		ephemeralStore:       ephemeralStore,
		timeline:             timelineService,
		channel:              channelService,
		antenna:              antennaService,
		clip:                 clipService,
		page:                 pageService,
		flash:                flashService,
		reaction:             reactionService,
		notification:         notificationService,
		notificationHook:     notificationHook,
		webhook:              webhookService,
		webhookNoteHook:      webhookNoteHook,
		webhookReactionHook:  webhookReactionHook,
		webhookFollowingHook: webhookFollowingHook,
		blocking:             blockingService,
		muting:               mutingService,
		renoteMuting:         renoteMutingService,
		poll:                 pollService,
		localDriveStorage:    localDriveStorage,
		driveStorage:         driveStorage,
		drive:                driveService,
		imgProcessor:         imgProcessor,
		driveReader:          driveReader,
		importer:             importer,
		sysAcct:              sysAcctSvc,
		listProxyFollow:      listProxyFollow,
		retention:            retentionSvc,
		achievement:          achievementService,
		proxyAllowlist:       proxyAllowlist,
		abuseInAppNotifier:   abuseInAppNotifier,
	}
}
