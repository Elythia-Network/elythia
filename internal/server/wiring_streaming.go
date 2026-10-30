package server

import (
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/api/clips"
	"github.com/elythia-network/elythia/internal/api/i"
	"github.com/elythia-network/elythia/internal/api/notes"
	"github.com/elythia-network/elythia/internal/api/pages"
	"github.com/elythia-network/elythia/internal/api/streaming"
	"github.com/elythia-network/elythia/internal/api/users"
	coreabuse "github.com/elythia-network/elythia/internal/core/abuse"
	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	"github.com/elythia-network/elythia/internal/core/blocking"
	corebubbleversus "github.com/elythia-network/elythia/internal/core/bubbleversus"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/clip"
	coreephemeral "github.com/elythia-network/elythia/internal/core/ephemeral"
	"github.com/elythia-network/elythia/internal/core/event"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/muting"
	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notification"
	corepoll "github.com/elythia-network/elythia/internal/core/poll"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corerelay "github.com/elythia-network/elythia/internal/core/relay"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	coretimeline "github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/core/userpack"
	corewebpush "github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/stream/channels"
)

// streamingDeps holds the parts wireStreaming receives from setupRoutes
// (#3901). setupRoutes sets every field from the variable of the same name;
// TestSetupRoutesSectionsReceiveSameNamedVariables checks that no field is
// left out or filled from a different variable.
type streamingDeps struct {
	idGen                        id.Generator
	streamPubSub                 *event.PubSubService
	mainStreamPublisher          *stream.MainStreamPublisher
	userRepo                     repository.UserRepository
	noteRepo                     repository.NoteRepository
	metaRepo                     repository.MetaRepository
	followingRepo                repository.FollowingRepository
	piningRepo                   repository.UserNotePiningRepository
	reactionRepo                 repository.NoteReactionRepository
	emojiRepo                    *repository.CachedEmojiRepository
	blockingRepo                 repository.BlockingRepository
	keypairExtraRepo             repository.UserKeypairExtraRepository
	instanceRepo                 repository.InstanceRepository
	antennaRepo                  repository.AntennaRepository
	clipRepo                     repository.ClipRepository
	userListRepo                 repository.UserListRepository
	reversiRepo                  repository.ReversiRepository
	chatRepo                     repository.ChatRepository
	bubbleVersusRecordRepo       repository.BubbleVersusRepository
	abuseReportRepo              repository.AbuseReportRepository
	roleService                  *corerole.Service
	entityPacker                 *entity.Packer
	reversiPublisher             *stream.ReversiGamePublisher
	sharedUserPacker             *userpack.Packer
	reversiFedCache              *corereversi.FederationIDCache
	webPushService               *corewebpush.Service
	noteCreateService            *note.CreateService
	noteQueryService             *note.QueryService
	userService                  *user.Service
	timelineFanoutHook           *coretimeline.FanoutHook
	ephemeralStore               *coreephemeral.Store
	antennaService               *coreantenna.Service
	clipService                  *clip.Service
	reactionService              *corereaction.Service
	notificationService          *notification.Service
	notificationHook             *notification.Hook
	blockingService              *blocking.Service
	mutingService                *muting.Service
	pollService                  *corepoll.Service
	abuseInAppNotifier           *coreabuse.InAppNotifier
	apURLs                       *activitypub.URLBuilder
	apRenderer                   *activitypub.Renderer
	deliverService               *corefederation.DeliverService
	relaySvc                     *corerelay.Service
	profileUpdateHook            *corefederation.ProfileUpdateDeliveryHook
	pinDeliveryHook              *corefederation.PinDeliveryHook
	reactionCountWriter          *corereaction.SwitchableWriter
	roleNotifLookup              func(roleID string) (map[string]any, bool)
	emojiApplicationNotifLookup  func(applicationID string) (entity.EmojiApplicationStatus, bool)
	signupApplicationNotifLookup func(applicationID string) (entity.SignupApplicationStatus, bool)
	abuseNotifLookup             func(reportID string) (entity.AbuseReportStatus, bool)
	federationResolver           *corefederation.Resolver
	federationProcessor          *corefederation.Processor
	noteFieldResolver            *entity.NoteFieldResolver
	notesHandler                 *notes.Handler
	usersHandler                 *users.Handler
	iHandler                     *i.Handler
	clipsHandler                 *clips.Handler
	pagesHandler                 *pages.Handler
	streamRegistry               *stream.Registry
	streamManager                *stream.Manager
	notePublisher                *stream.NotePublisher
	notificationPublisher        *stream.NotificationPublisher
	chatPublisher                *stream.ChatPublisher
}

// wireStreaming wires the streaming connection manager (its lookups and the
// pubsub subscriptions it starts on a server role), the server and queue
// stats publishers, the publishers handed to the existing services, the
// reversi, bubble game versus and chat services with their WebSocket
// channels, and registers /streaming. setupRoutes calls it where these
// statements used to be, so the subscriptions start at the same point of the
// construction (docs/architecture.md 4.1), and receives the services it
// returns under the same names. It binds every field of d to the name
// setupRoutes uses, so the moved statements read the same as before (#3901).
func (s *Server) wireStreaming(d streamingDeps) (*coreabuse.CreatedNotifier, *stream.ServerStatsPublisher, *corereversi.Service, *stream.BubbleVersusPublisher, *corebubbleversus.Service, *corechat.Service, *channels.ChatRoomFactory) {
	idGen := d.idGen
	streamPubSub := d.streamPubSub
	mainStreamPublisher := d.mainStreamPublisher
	userRepo := d.userRepo
	noteRepo := d.noteRepo
	metaRepo := d.metaRepo
	followingRepo := d.followingRepo
	piningRepo := d.piningRepo
	reactionRepo := d.reactionRepo
	emojiRepo := d.emojiRepo
	blockingRepo := d.blockingRepo
	keypairExtraRepo := d.keypairExtraRepo
	instanceRepo := d.instanceRepo
	antennaRepo := d.antennaRepo
	clipRepo := d.clipRepo
	userListRepo := d.userListRepo
	reversiRepo := d.reversiRepo
	chatRepo := d.chatRepo
	bubbleVersusRecordRepo := d.bubbleVersusRecordRepo
	abuseReportRepo := d.abuseReportRepo
	roleService := d.roleService
	entityPacker := d.entityPacker
	reversiPublisher := d.reversiPublisher
	sharedUserPacker := d.sharedUserPacker
	reversiFedCache := d.reversiFedCache
	webPushService := d.webPushService
	noteCreateService := d.noteCreateService
	noteQueryService := d.noteQueryService
	userService := d.userService
	timelineFanoutHook := d.timelineFanoutHook
	ephemeralStore := d.ephemeralStore
	antennaService := d.antennaService
	clipService := d.clipService
	reactionService := d.reactionService
	notificationService := d.notificationService
	notificationHook := d.notificationHook
	blockingService := d.blockingService
	mutingService := d.mutingService
	pollService := d.pollService
	abuseInAppNotifier := d.abuseInAppNotifier
	apURLs := d.apURLs
	apRenderer := d.apRenderer
	deliverService := d.deliverService
	relaySvc := d.relaySvc
	profileUpdateHook := d.profileUpdateHook
	pinDeliveryHook := d.pinDeliveryHook
	reactionCountWriter := d.reactionCountWriter
	roleNotifLookup := d.roleNotifLookup
	emojiApplicationNotifLookup := d.emojiApplicationNotifLookup
	signupApplicationNotifLookup := d.signupApplicationNotifLookup
	abuseNotifLookup := d.abuseNotifLookup
	federationResolver := d.federationResolver
	federationProcessor := d.federationProcessor
	noteFieldResolver := d.noteFieldResolver
	notesHandler := d.notesHandler
	usersHandler := d.usersHandler
	iHandler := d.iHandler
	clipsHandler := d.clipsHandler
	pagesHandler := d.pagesHandler
	streamRegistry := d.streamRegistry
	streamManager := d.streamManager
	notePublisher := d.notePublisher
	notificationPublisher := d.notificationPublisher
	chatPublisher := d.chatPublisher

	// 2. Channel registry: Misskey 互換のチャンネル名と factory の対応は
	// builtinStreamChannels (wiring_stream.go) の一覧にまとめ、chat の service を
	// 作った後で 1 回だけ登録する (#3692)。

	// 3. Connection manager と各 publisher への配線。manager と publisher は
	// buildStream で作る (#3692)。
	s.streamManager = streamManager
	// readNotificationメッセージをnotificationServiceに橋渡しする
	streamManager.SetNotificationReader(&notifReaderAdapter{svc: notificationService})
	// hardMutedWords (#787)・muteBlockSnapshot (#1711)・role policy (#1942)・
	// ugcVisibilityForVisitor の lookup は、buildStream で manager の構築子に渡す (#3827)。
	// followeeSnapshot (#1063): home/hybrid/local timeline channel が reply gate
	// に使う「followeeID -> withReplies」map を接続確立時に 1 回だけ fetch する。
	// 失敗 / anonymous は nil 返却で degrade — channel は 3 escape hatch のみで
	// 動く (= 旧来の "全 reply drop" よりは upstream 互換に近い)。
	streamManager.SetFollowingSnapshotLookup(&followingSnapshotAdapter{repo: followingRepo})
	// subNote visibility gate (#1460 IDOR fix): handleSubNote が任意の noteID
	// で noteStream を subscribe させると、followers / specified note の
	// reacted / unreacted / pollVoted / deleted event が非対象 viewer に
	// 漏れる (HTTP 側 #1444 i/notifications IDOR の WS 版)。noteQueryService
	// の RequireVisible が NoteVisibilityChecker interface を自動 satisfy する。
	streamManager.SetNoteVisibilityChecker(noteQueryService)
	// onlineStatus の source。upstream と同じく WebSocket 接続中だけ
	// lastActiveDate を更新する (REST では更新しない、#2350)。
	streamManager.SetLastActiveRecorder(&lastActiveRecorderAdapter{userRepo: userRepo, background: s.background})
	// 以下 3 つの subscriber は **WebSocket 接続を持つ role でだけ**起動する
	// (#2459)。queue role には connection が無いので、購読しても受け取った
	// event を捨てるだけで Redis pubsub の帯域を食う。
	if s.role.RunsServer() {
		// hardMutedWords 変更時に reload event を受け取って該当 connection の
		// rules を refresh する subscriber を起動 (#791)。i/update 側 publisher
		// と同じ topic 名を共有 (= stream.WordMuteReloadTopic)。
		streamManager.SubscribeWordMuteReload()
		// follow / mute / block 変更を受け取って接続中の snapshot を取り直す (#2400)。
		streamManager.SubscribeRelationReload()
		// broadcast stream (emojiAdded/Updated/Deleted 等) を全 connection へ forward (#2046)。
		streamManager.SubscribeBroadcast()
		// 資格情報の失効 (token 再生成 / access token 失効 / 凍結 / 削除) を受けて、
		// その資格情報で張られた接続を閉じる。publish は失効を処理したプロセスが
		// 行うので、接続を持つ全プロセスが購読する (mk-go 独自、docs/divergence.md)。
		streamManager.SubscribeStreamRevoke()
	}
	// 失効 event に相乗りして、各プロセスが自分の tokenCache から対象利用者の
	// entry を落とす。tokenCache はプロセス内の map なので、Web ノードが複数
	// あると失効を処理したノード以外に旧 token の entry が残り、閉じた接続が
	// そのノードへ再接続すると無期限に残る。
	// 自プロセスだけで捨てる口を使う。失効の通知は全プロセスに届くので、ここから
	// さらに authCacheInvalidated を publish すると、受けたプロセスの数だけ同じ
	// 通知が飛ぶ。
	streamManager.OnStreamRevoke(s.auth.InvalidateTokensForUserLocally)
	// 2 回目の閉じ処理は tokenCache の TTL が切れた後に行う (DB を引いた直後に
	// 無効化 event を追い越して積まれた entry も、そこで確実に切れている)。
	streamManager.SetRevokeRecheckDelay(middleware.AuthCacheTTL + 5*time.Second)
	// WebSocket は接続時に 1 度しか認証しないので、tokenCache を落とすだけでは
	// 既存の接続が閉じない。失効を扱う handler (i・oauth・admin) へは、buildStream で
	// 作った revoke publisher を構築子の引数で配る (#3823、#3827)。publisher は自プロセスの
	// Manager も持つ — publish が失敗しても、ここに居る接続は閉じる。
	// profile 編集を remote followers に Update(Person) で配信する (#1560)。
	// actor endpoint と同じ shape にするため ed25519 keypair を、follower 以外の
	// relay subscriber にも届けるため relay broadcaster を配線する。hook は
	// buildFederation で作る (#3688。下の pinDeliveryHook も同じ)。
	profileUpdateHook.SetKeypairExtraRepo(keypairExtraRepo)
	profileUpdateHook.SetRelayBroadcaster(relaySvc)
	iHandler.SetProfileUpdateHook(profileUpdateHook)
	// note pin/unpin の Add/Remove(featured collection) を followers/relay へ配信 (#2024)。
	pinDeliveryHook.SetRelayBroadcaster(relaySvc)
	iHandler.SetPinDeliveryHook(pinDeliveryHook)
	// note / notification の publisher は buildStream で作る (#3692)。setter はここ。
	// Packer は buildStream で構築子に渡す (#3827)。
	notePublisher.SetEmojiLookup(emojiRepo)
	notePublisher.SetInstanceLookup(instanceRepo)
	notePublisher.SetReactionReader(reactionCountWriter)
	notePublisher.SetReactionPairReader(reactionRepo) // #2067: renote embed の pair cache を note_reaction から埋める
	notePublisher.SetFieldResolver(noteFieldResolver)
	notificationPublisher.SetRepos(userRepo, noteRepo, idGen)
	notificationPublisher.SetInstanceLookup(instanceRepo)
	notificationPublisher.SetEmojiLookup(emojiRepo)
	// followingRepo: Pack 内 noteVisibleToNotifiee の followers visibility
	// gate (#1471) に必要。未配線時は CanSeeNote semantics に合わせて
	// followers note を本人以外に embed しない (= fail-closed)。
	notificationPublisher.SetFollowingChecker(followingRepo)
	notificationPublisher.SetRoleLookup(roleNotifLookup)
	notificationPublisher.SetAbuseReportLookup(abuseNotifLookup)
	notificationPublisher.SetEmojiApplicationLookup(emojiApplicationNotifLookup)
	notificationPublisher.SetSignupApplicationLookup(signupApplicationNotifLookup)
	// streaming の通知 payload にも note の files / channel を載せる (#2735)。
	notificationPublisher.SetFieldResolver(noteFieldResolver)
	// 通報の通知 (通知欄 #2868 / admin stream #1549 / abuseReport system webhook
	// #1542)。**local と連合 (Flag) で同じ notifier を共有する** — 入口ごとに
	// 配線していたので Flag の経路だけ admin stream と webhook が抜けていた
	// (#3256)。連打の絞り (#3200) も通報の出どころで変わらない。webhook は
	// recipientRepo (無効にした通知先の除外に使う) が揃う後段で SetWebhook する。
	// 世代の箱 (#3661) と Packer (#3794) は構築子の必須の引数で渡す (#3823)。
	abuseCreatedNotifier := coreabuse.NewCreatedNotifier(abuseInAppNotifier, roleService, stream.NewAdminStreamPublisher(streamPubSub),
		s.background, entityPacker)
	usersHandler.SetAbuseReportCreatedNotifier(abuseCreatedNotifier)

	// server / queue stats publishers (#344)。起動時から tick を回して
	// `serverStats` / `queueStats` トピックへ定期 publish する。
	// ShutdownでStop()を呼ぶ (server.go 側でdefer)。
	// publisher が ring buffer を持つので、channel factory に StatsLogProvider
	// として渡して requestLog 応答に historical 値を返せるようにする (#571 item 2)。
	//
	// **publish は server role でだけ回す** (#2459)。どちらも同じ pubsub
	// トピックへ投げるので、2 ノードが並行して publish すると購読側は
	// **別マシンの値を交互に受け取り**、管理画面のグラフが振動する。
	// factory の登録は role に関係なく行う (WebSocket が生えるのは server
	// role だけなので、queue role では誰も subscribe しない)。
	//
	// 無効のあいだは集めも流しもしない (本家 ServerStatsService と同じ)。
	// tick ごとに見るので、管理画面での切り替えは再起動を待たずに効く。判定は
	// 構築子の必須の引数で渡す (#3827)。
	serverStatsPub := stream.NewServerStatsPublisher(streamPubSub, 0, func() bool {
		m, err := metaRepo.Fetch()
		return err == nil && m != nil && m.EnableServerMachineStats
	})
	if s.role.RunsServer() {
		s.addPlainStartStopper("serverStatsPub", serverStatsPub)
	}
	// queueInspector が無い起動 (テスト等) では nil のまま。チャンネルの一覧
	// (builtinStreamChannels) が、そのとき fallback の factory を選ぶ (#3692)。
	var queueStatsPub *stream.QueueStatsPublisher
	if s.queueInspector != nil {
		queueStatsPub = stream.NewQueueStatsPublisher(&queueStatsInspectorAdapter{inner: s.queueInspector}, streamPubSub, 0)
		if s.role.RunsServer() {
			s.addPlainStartStopper("queueStatsPub", queueStatsPub)
		}
	}

	// 4. 既存サービスへ publisher を注入する。これらはいずれも nil 安全な
	//    setter で、未設定なら何もしない (テスト互換)。
	timelineFanoutHook.SetStreamingPublisher(notePublisher)
	// antenna realtime 配信 (#1573): FanoutHook と同じ NotePublisher を共有し、
	// OnNoteCreated で match した note を antennaTimeline:<id> へ publish させる。
	// これが無いと WS antenna channel は live note を受信しない (Stream XAdd
	// のみで pubsub publish が欠落していた)。
	antennaService.SetStreamingPublisher(notePublisher)
	notificationService.SetStreamingPublisher(notificationPublisher)
	// main streamのpublisherは、#3676の時点ではpollExpiryWorker(構築の途中で起動し、
	// 初回のTickで通知を作る)より後で入れていた。前へ移すと、構築中に作ったpollEndedの
	// 通知がpackerを通らない形でunreadNotificationとして流れた(SetPackerはここより後)。
	// #3703でworkerの起動はStartBackground(構築の後)へ移ったので、構築中に通知を作る
	// ことはもう無い。元の配線と同じにするため、streamingのpublisherの隣に残す。
	notificationService.SetMainStreamPublisher(mainStreamPublisher)
	// readAllNotifications は upstream postReadAllNotifications と同じく
	// main stream と Web Push の両方へ送る。これが無いと SW 側の
	// readAllNotifications 分岐 (表示中の OS 通知を閉じる) が発火しない。
	notificationService.SetReadAllPusher(webPushService)
	notificationService.SetPacker(notificationPublisher)
	// ロール単位の通知 opt-out (#2898)。**配線しないと optOutNotificationTypes
	// を設定しても効かない** — Service は resolver 未設定なら全ての通知を通す。
	notificationService.SetPolicyResolver(roleService)
	pagesHandler.SetUserSource(userService)

	// 5. Reversi WebSocket channel (Phase 9.6) の service。channel の登録は下の
	// registerStreamChannels でまとめて行う (#3692。バブルゲームと chat も同じ)。
	reversiService := corereversi.NewService(reversiRepo, reversiPublisher, s.redis.Default)
	// stream で配る対局の UserLite も、API と同じくリモートのアイコンをメディア
	// プロキシに通す。配線しないとリモートの人は identicon になる。
	reversiService.SetAvatarURLFunc(entityPacker.IdenticonURL)

	// バブルゲームの対戦 (#3230)。対局の状態は Redis にだけ置き、記録 (両者の
	// 報告と勝敗) は DB に書く (#3232)。
	bubbleVersusPublisher := stream.NewBubbleVersusPublisher(streamPubSub, entityPacker)
	bubbleVersusService := corebubbleversus.NewService(s.redis.Default, bubbleVersusPublisher, blockingService, idGen)
	bubbleVersusService.SetRecordStore(bubbleVersusRecordRepo)

	// 6. Chat WebSocket channels (Phase 9.8): chatRoom と chatUser の service
	//
	// Packer (#3794)・stream / push の fromUser・toUser の instance・絵文字 (#3438)・
	// 1-on-1 DM で recipient が sender を block している場合に弾く repository
	// (upstream YOU_HAVE_BEEN_BLOCKED 互換) は、構築子の必須の引数で渡す (#3823)。
	chatService := corechat.NewService(chatRepo, idGen, entityPacker, sharedUserPacker, blockingRepo)
	chatService.SetStreamingPublisher(chatPublisher)
	chatService.SetMainStreamPublisher(mainStreamPublisher)
	// newChatMessage は upstream ChatService.ts と同じく main stream への publish と
	// Web Push を対で送る。これが無いと、タブを閉じている利用者に届かない (#2840)。
	chatService.SetChatPusher(webPushService)
	// push の body に fromUser を載せるため sender を引く (#2840)。
	chatService.SetUserRepo(userRepo)
	// CherryPick 互換 AP 連合: 1-on-1 DM を Create+Note(_misskey_talk:true) で配送 (#692)。
	chatService.SetAPDelivery(userRepo, apRenderer, apURLs, deliverService)
	// chatScope=followers/following/mutual 判定用に following repo を渡す (#692)。
	chatService.SetFollowingRepo(followingRepo)
	// chat/rooms/show の権限 gate で moderator bypass を効かせる
	// (upstream 2026.5.4 hasPermissionToViewRoomInfo 互換、#1164 Phase C)。
	chatService.SetModeratorChecker(roleService)
	// React で custom emoji (:name:) reaction の存在検証に使う (#1541)。
	chatService.SetEmojiRepo(emojiRepo)
	// 1-on-1 DM の chatApproval bypass + 送信後 approval 挿入に使う (#1748)。
	chatService.SetApprovalRepo(repository.NewChatApprovalRepository(s.db))
	chatRoomFactory := channels.NewChatRoomFactory(chatService)
	// WebSocket のチャンネルの登録 (#3692)。チャンネル名と factory の対応は
	// builtinStreamChannels の一覧にだけ書く。registry は factory を map に入れる
	// だけで、引くのは接続を受けたとき (dispatcher の Lookup)。登録の位置は、前に
	// 散らばっていた 22 行のうち最後の行 (chatUser) があった位置で、/streaming を
	// 生やすより前。factory が依存する service と publisher (reversi・バブルゲームの
	// 対戦・chat、stats) は、setter を呼んだのと同じ名前から渡す。
	registerStreamChannels(streamRegistry, builtinStreamChannels(streamChannelDeps{
		antennas:     antennaRepo,
		userLists:    userListRepo,
		clips:        clipRepo,
		roles:        roleService,
		serverStats:  serverStatsPub,
		queueStats:   queueStatsPub,
		reversi:      reversiService,
		bubbleVersus: bubbleVersusService,
		chatRoom:     chatRoomFactory,
		chat:         chatService,
	}))
	// Phase 9.7: federation processor / reversi handler に reversi 依存を注入。
	federationProcessor.SetReversi(reversiService, reversiRepo, idGen, reversiFedCache)
	federationProcessor.SetAbuseReportRepo(abuseReportRepo, idGen)
	// リモートからの通報 (AP Flag) もローカルと同じ通知を出す (#2868 / #3256)。
	// **配線しないと通報の出どころで通知の有無が変わる。**
	federationProcessor.SetAbuseReportCreatedNotifier(abuseCreatedNotifier)
	federationProcessor.SetPinningRepo(piningRepo, idGen)
	federationProcessor.SetRelayMarker(relaySvc)
	federationProcessor.SetRelayActorChecker(relaySvc)
	federationProcessor.SetChatService(chatService)
	// group chat (room) federation の inbound 招待処理 (#1203)。chatService が
	// ChatRoomReceiver も実装している。
	federationProcessor.SetChatRoomReceiver(chatService)
	// inbound actor self-delete 時に remote user の notes/drive/following を
	// cascade purge する job を enqueue する (#1220)。
	federationProcessor.SetAccountDeleteEnqueuer(s.queueClient)
	// リレー投稿を DB ではなく Redis へ書く経路 (#2332)。有効化されていない
	// 間は ResolveNoteEphemeral が通常経路へ倒れるので挙動は変わらない。
	federationResolver.SetEphemeralSink(ephemeralStore)
	// リレー経由で初めて観測した remote user を記録する (#2340)。孤児掃除の
	// 対象をリレー由来に限定するために使う。
	federationResolver.SetRelayObservedMarker(repository.NewRelayObservedUserRepository(s.db))
	// ephemeral note を DB 行へ昇格させる経路 (#2332)。外部キーで参照される
	// 必要が生じた時点だけで呼ばれる。閲覧では呼ばない (リンクを踏まれるたびに
	// 永続化されると DB を膨らませない目的が崩れる)。
	ephemeralMaterializer := coreephemeral.NewMaterializer(ephemeralStore, noteRepo, federationResolver)
	reactionService.SetNoteMaterializer(ephemeralMaterializer)   // note_reaction.noteId
	noteCreateService.SetNoteMaterializer(ephemeralMaterializer) // note.replyId / renoteId
	pollService.SetNoteMaterializer(ephemeralMaterializer)       // poll_vote.noteId
	clipService.SetNoteMaterializer(ephemeralMaterializer)       // clip_note.noteId
	// ノートを伴わない契機。いずれも user への外部キーだけを要求する。
	mutingService.SetUserMaterializer(ephemeralMaterializer)   // muting.muteeId
	blockingService.SetUserMaterializer(ephemeralMaterializer) // blocking.blockeeId
	notesHandler.SetNoteMaterializer(ephemeralMaterializer)    // note_favorite.noteId
	clipsHandler.SetNoteMaterializer(ephemeralMaterializer)    // clips/add-note の事前チェック
	// 閲覧経路は Redis から返すだけで materialize しない。読み取りのたびに
	// TTL を打ち直して「開いた直後に期限切れ」を防ぐ。
	noteQueryService.SetEphemeralReader(ephemeralStore)
	// DB 行が ephemeral を上書きしたとき FTT から旧 ID を除く。残すと hydrate で
	// ephemeral 側が拾われ二重表示になる。
	federationResolver.SetEphemeralTimelineRemover(timelineFanoutHook)
	federationProcessor.SetFanoutHook(timelineFanoutHook)
	// リモートの投稿を antenna に載せる (#2743)。リレー経由は processor 側で
	// 除外している。
	federationProcessor.SetAntennaHook(coreantenna.NewNoteCreateHook(antennaService))
	federationProcessor.SetNotificationHook(notificationHook)

	// 5. /streaming エンドポイント配線
	streamingHandler := streaming.NewHandler(streamManager)
	s.echo.GET("/streaming", streamingHandler.Stream)
	return abuseCreatedNotifier, serverStatsPub, reversiService, bubbleVersusPublisher, bubbleVersusService, chatService, chatRoomFactory
}
