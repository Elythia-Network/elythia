package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/pprof"
	urlpkg "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/activitypub/ld"
	"github.com/shiroha-a/mk/internal/activitypub/mfm"
	apiadmin "github.com/shiroha-a/mk/internal/api/admin"
	apiannouncements "github.com/shiroha-a/mk/internal/api/announcements"
	"github.com/shiroha-a/mk/internal/api/antennas"
	"github.com/shiroha-a/mk/internal/api/ap"
	apiapp "github.com/shiroha-a/mk/internal/api/app"
	apiauth "github.com/shiroha-a/mk/internal/api/auth"
	"github.com/shiroha-a/mk/internal/api/avatardecorations"
	"github.com/shiroha-a/mk/internal/api/blocking"
	apibubblegame "github.com/shiroha-a/mk/internal/api/bubblegame"
	apichannels "github.com/shiroha-a/mk/internal/api/channels"
	apicharts "github.com/shiroha-a/mk/internal/api/charts"
	apichat "github.com/shiroha-a/mk/internal/api/chat"
	"github.com/shiroha-a/mk/internal/api/clips"
	"github.com/shiroha-a/mk/internal/api/drive"
	apiemojiapplications "github.com/shiroha-a/mk/internal/api/emojiapplications"
	apiemojis "github.com/shiroha-a/mk/internal/api/emojis"
	"github.com/shiroha-a/mk/internal/api/endpoints"
	apifederation "github.com/shiroha-a/mk/internal/api/federation"
	apifetchexternal "github.com/shiroha-a/mk/internal/api/fetchexternal"
	apifetchrss "github.com/shiroha-a/mk/internal/api/fetchrss"
	apiflash "github.com/shiroha-a/mk/internal/api/flash"
	"github.com/shiroha-a/mk/internal/api/following"
	apigallery "github.com/shiroha-a/mk/internal/api/gallery"
	apihashtags "github.com/shiroha-a/mk/internal/api/hashtags"
	"github.com/shiroha-a/mk/internal/api/i"
	"github.com/shiroha-a/mk/internal/api/inbox"
	apiinvite "github.com/shiroha-a/mk/internal/api/invite"
	"github.com/shiroha-a/mk/internal/api/meself"
	"github.com/shiroha-a/mk/internal/api/meta"
	"github.com/shiroha-a/mk/internal/api/mute"
	"github.com/shiroha-a/mk/internal/api/nodeinfo"
	"github.com/shiroha-a/mk/internal/api/notehide"
	"github.com/shiroha-a/mk/internal/api/notes"
	"github.com/shiroha-a/mk/internal/api/notifications"
	"github.com/shiroha-a/mk/internal/api/oauth"
	"github.com/shiroha-a/mk/internal/api/pages"
	"github.com/shiroha-a/mk/internal/api/promo"
	apiproxy "github.com/shiroha-a/mk/internal/api/proxy"
	"github.com/shiroha-a/mk/internal/api/renotemute"
	apiresetpassword "github.com/shiroha-a/mk/internal/api/resetpassword"
	apireversi "github.com/shiroha-a/mk/internal/api/reversi"
	apiroles "github.com/shiroha-a/mk/internal/api/roles"
	apisignin "github.com/shiroha-a/mk/internal/api/signin"
	apisignup "github.com/shiroha-a/mk/internal/api/signup"
	"github.com/shiroha-a/mk/internal/api/stats"
	"github.com/shiroha-a/mk/internal/api/streaming"
	apisw "github.com/shiroha-a/mk/internal/api/sw"
	apitest "github.com/shiroha-a/mk/internal/api/test"
	apiurl "github.com/shiroha-a/mk/internal/api/url"
	apiuserlists "github.com/shiroha-a/mk/internal/api/userlists"
	"github.com/shiroha-a/mk/internal/api/userrelation"
	"github.com/shiroha-a/mk/internal/api/users"
	apiwebhooks "github.com/shiroha-a/mk/internal/api/webhooks"
	"github.com/shiroha-a/mk/internal/api/wellknown"
	coreabuse "github.com/shiroha-a/mk/internal/core/abuse"
	coreachievement "github.com/shiroha-a/mk/internal/core/achievement"
	coreannouncement "github.com/shiroha-a/mk/internal/core/announcement"
	coreantenna "github.com/shiroha-a/mk/internal/core/antenna"
	"github.com/shiroha-a/mk/internal/core/avatardecoration"
	coreblocking "github.com/shiroha-a/mk/internal/core/blocking"
	corecaptcha "github.com/shiroha-a/mk/internal/core/captcha"
	corechannel "github.com/shiroha-a/mk/internal/core/channel"
	"github.com/shiroha-a/mk/internal/core/chart"
	"github.com/shiroha-a/mk/internal/core/chart/charthook"
	corechat "github.com/shiroha-a/mk/internal/core/chat"
	coreclip "github.com/shiroha-a/mk/internal/core/clip"
	"github.com/shiroha-a/mk/internal/core/deliveryhealth"
	coredrive "github.com/shiroha-a/mk/internal/core/drive"
	"github.com/shiroha-a/mk/internal/core/driveusage"
	"github.com/shiroha-a/mk/internal/core/emojiapplication"
	coreemojiimport "github.com/shiroha-a/mk/internal/core/emojiimport"
	"github.com/shiroha-a/mk/internal/core/emojimeta"
	coreephemeral "github.com/shiroha-a/mk/internal/core/ephemeral"
	"github.com/shiroha-a/mk/internal/core/event"
	corefeatured "github.com/shiroha-a/mk/internal/core/featured"
	corefederation "github.com/shiroha-a/mk/internal/core/federation"
	coreflash "github.com/shiroha-a/mk/internal/core/flash"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	corehashtag "github.com/shiroha-a/mk/internal/core/hashtag"
	coreinstance "github.com/shiroha-a/mk/internal/core/instance"
	"github.com/shiroha-a/mk/internal/core/iplog"
	"github.com/shiroha-a/mk/internal/core/iplookuplog"
	coremediaproxy "github.com/shiroha-a/mk/internal/core/mediaproxy"
	coremodlog "github.com/shiroha-a/mk/internal/core/moderationlog"
	coremoderatoractivity "github.com/shiroha-a/mk/internal/core/moderatoractivity"
	coremove "github.com/shiroha-a/mk/internal/core/move"
	coremuting "github.com/shiroha-a/mk/internal/core/muting"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	corenotification "github.com/shiroha-a/mk/internal/core/notification"
	corepage "github.com/shiroha-a/mk/internal/core/page"
	"github.com/shiroha-a/mk/internal/core/passwordguard"
	corepoll "github.com/shiroha-a/mk/internal/core/poll"
	"github.com/shiroha-a/mk/internal/core/procstats"
	corereaction "github.com/shiroha-a/mk/internal/core/reaction"
	corerelay "github.com/shiroha-a/mk/internal/core/relay"
	coreretention "github.com/shiroha-a/mk/internal/core/retention"
	corereversi "github.com/shiroha-a/mk/internal/core/reversi"
	corerole "github.com/shiroha-a/mk/internal/core/role"
	coresearch "github.com/shiroha-a/mk/internal/core/search"
	"github.com/shiroha-a/mk/internal/core/selfcheck"
	coresignup "github.com/shiroha-a/mk/internal/core/signup"
	"github.com/shiroha-a/mk/internal/core/signupapplication"
	"github.com/shiroha-a/mk/internal/core/signupform"
	coresystemaccount "github.com/shiroha-a/mk/internal/core/systemaccount"
	coretimeline "github.com/shiroha-a/mk/internal/core/timeline"
	coretransfer "github.com/shiroha-a/mk/internal/core/transfer"
	coretranslate "github.com/shiroha-a/mk/internal/core/translate"
	coretwofactor "github.com/shiroha-a/mk/internal/core/twofactor"
	coreurlpreview "github.com/shiroha-a/mk/internal/core/urlpreview"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	coreuserlist "github.com/shiroha-a/mk/internal/core/userlist"
	corewebhook "github.com/shiroha-a/mk/internal/core/webhook"
	corewebpush "github.com/shiroha-a/mk/internal/core/webpush"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/frontendutil"
	"github.com/shiroha-a/mk/internal/misc/id"
	miscsmtp "github.com/shiroha-a/mk/internal/misc/smtp"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/processors"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/stream"
	"github.com/shiroha-a/mk/internal/stream/channels"
	"github.com/shiroha-a/mk/plugin"
	"log/slog"
)

func (s *Server) setupRoutes(plugins []plugin.Definition, openPluginStorage pluginStorageOpener) {
	idGen, err := id.NewGenerator(s.config.ID)
	if err != nil {
		idGen, _ = id.NewGenerator("aidx")
	}

	// Repositories
	// userRepo は server.New で構築した CachedUserRepository を再利用する。
	// auth middleware と service 層が同じ cache を共有することで mutation
	// 時の invalidate が両方に反映される (#300 3-3)。
	userRepo := s.userRepo
	noteRepo := repository.NewNoteRepository(s.db)
	// cross-worker cache invalidation (#1740) のため concrete *CachedMetaRepository
	// を保持する。internal event bus との配線は streamPubSub 生成箇所で行う。
	cachedMeta := repository.NewCachedMetaRepositoryWithTTL(repository.NewMetaRepository(s.db), 5*time.Minute)
	var metaRepo repository.MetaRepository = cachedMeta
	// Seed the singleton meta row on first boot so that fresh installs
	// can run /api/admin/accounts/create (initial setup) without tripping
	// over a missing meta row.
	if err := metaRepo.EnsureInitial(idGen.Generate(time.Now())); err != nil {
		slog.Error("failed to ensure initial meta row", "err", err)
	}
	pollRepo := repository.NewPollRepository(s.db)
	followingRepo := repository.NewFollowingRepository(s.db)
	followRequestRepo := repository.NewFollowRequestRepository(s.db)
	piningRepo := repository.NewUserNotePiningRepository(s.db)
	reactionRepo := repository.NewNoteReactionRepository(s.db)
	// emoji の ListLocal はタイムライン描画ごとに毎回フルスキャンしていた
	// hot path だが、絵文字テーブルの mutation は admin 経由のみで頻度が低い。
	// 5 分 TTL の in-memory cache + mutation 時 invalidate で DB 負荷を消す
	// (#300 3-6)。
	emojiRepo := repository.NewCachedEmojiRepository(repository.NewEmojiRepository(s.db))
	// 絵文字由来のアバターデコレーション (#2975) の解決に使う。**catalog と同じ形の
	// TTL cache だが、載せるのはローカルかつ非センシティブなものだけ**で、その
	// 条件がそのまま「表示してよいか」の判定になっている。絵文字をセンシティブに
	// したり消したりすると、後始末なしに次の TTL で全プロフィールから消える。
	//
	// **`emojiRepo` より後・絵文字を変える配線より前に置く。** zip インポータと
	// admin handler の両方がこの invalidator を受け取る。
	emojiDecorationResolver := avatardecoration.NewEmojiResolver(emojiRepo)
	blockingRepo := repository.NewBlockingRepository(s.db)
	mutingRepo := repository.NewMutingRepository(s.db)
	renoteMutingRepo := repository.NewRenoteMutingRepository(s.db)
	pollVoteRepo := repository.NewPollVoteRepository(s.db)
	driveFileRepo := repository.NewDriveFileRepository(s.db)
	driveFolderRepo := repository.NewDriveFolderRepository(s.db)
	keypairRepo := repository.NewUserKeypairRepository(s.db)
	// mk-go は #2378 以前、ローカルユーザーの RSA 秘密鍵を PKCS#1 で発行していた。
	// mk-go 自身は両形式を読めるが **Misskey TS は PKCS#8 しか読めない** ので、
	// そのまま TS へ引き渡すとそのユーザーの送信側の連合が全滅する (受信は動くので
	// 片方向だけ静かに壊れる)。既存インスタンスを救うため起動時に変換する。
	//
	// 変換するのは PEM のエンコーディングだけで鍵そのものは変わらない。公開鍵も
	// 鍵 ID も不変なので連合相手から見て何も変わらない。冪等なので毎回走ってよく、
	// 対象が無ければ 1 クエリで終わる。失敗は警告だけで起動継続。
	if converted, err := keypairRepo.NormalizePrivateKeysToPKCS8(); err != nil {
		slog.Warn("user_keypair PKCS#8 正規化に失敗", "err", err)
	} else if converted > 0 {
		slog.Info("user_keypair を PKCS#8 に正規化した", "converted", converted)
	}
	keypairExtraRepo := repository.NewUserKeypairExtraRepository(s.db)
	// 凍結の由来 (local / remote)。モデレーターの判断をリモートに巻き戻させない (#2973)。
	suspensionOriginRepo := repository.NewUserSuspensionOriginRepository(s.db)
	instanceRepo := repository.NewInstanceRepository(s.db)
	// instance.{followersCount,followingCount} は following service の
	// adjustInstanceCountsForFollowing (Follow/Unfollow/AcceptRequest) と
	// blocking service の auto-unfollow で incremental に維持される
	// (admin/overview の federation pie chart の data source)。起動時の
	// backfill は、再起動時点での following テーブルとの整合性回復 +
	// direct DB 改変や過去の counter drift への安全網として残す (#421)。
	// 失敗は警告だけで起動継続。
	if err := instanceRepo.RecomputeFollowCounts(); err != nil {
		slog.Warn("instance follow-counts recompute failed", "err", err)
	}
	channelRepo := repository.NewChannelRepository(s.db)
	channelFollowingRepo := repository.NewChannelFollowingRepository(s.db)
	// antenna は inbound note 1 件ごとに ListAllActive が引かれる (#2752)。
	// プロセス内キャッシュを挟み、書き込みで invalidate する。cross-worker の
	// 伝播は下の internal:antennaUpdated (meta と同じ形)。
	cachedAntenna := repository.NewCachedAntennaRepository(repository.NewAntennaRepository(s.db))
	var antennaRepo repository.AntennaRepository = cachedAntenna
	clipRepo := repository.NewClipRepository(s.db)
	clipNoteRepo := repository.NewClipNoteRepository(s.db)
	pageRepo := repository.NewPageRepository(s.db)
	pageLikeRepo := repository.NewPageLikeRepository(s.db)
	flashRepo := repository.NewFlashRepository(s.db)
	flashLikeRepo := repository.NewFlashLikeRepository(s.db)
	roleRepo := repository.NewRoleRepository(s.db)
	roleAssignmentRepo := repository.NewRoleAssignmentRepository(s.db)
	swSubRepo := repository.NewSwSubscriptionRepository(s.db)
	noteFavoriteRepo := repository.NewNoteFavoriteRepository(s.db)
	noteThreadMutingRepo := repository.NewNoteThreadMutingRepository(s.db)
	userListRepo := repository.NewUserListRepository(s.db)
	webhookRepo := repository.NewWebhookRepository(s.db)
	systemWebhookRepo := repository.NewSystemWebhookRepository(s.db)
	reversiRepo := repository.NewReversiRepository(s.db)
	// userIPRepo は signin handler (Phase 6) と clean cron processor (#1563)
	// の双方で使うため、他 repo と同じ早い段階で構築する。
	userIPRepo := repository.NewUserIPRepository(s.db)
	// 認証済みリクエストの IP を記録する (#3103)。**meta は呼び出しのたびに読む**
	// ので、`enableIpLogging` の切り替えが再起動なしで効く (#3107)。重複排除の窓は
	// upstream の `userIpHistories` に合わせて 1 時間。
	ipLogService := iplog.NewService(userIPRepo, metaRepo, iplog.DedupeWindow)
	chatRepo := repository.NewChatRepository(s.db)
	channelFavoriteRepo := repository.NewChannelFavoriteRepository(s.db)
	channelMutingRepo := repository.NewChannelMutingRepository(s.db)
	clipFavoriteRepo := repository.NewClipFavoriteRepository(s.db)
	userListFavoriteRepo := repository.NewUserListFavoriteRepository(s.db)
	retentionRepo := repository.NewRetentionAggregationRepository(s.db)
	promoReadRepo := repository.NewPromoReadRepository(s.db)
	hashtagRepo := repository.NewHashtagRepository(s.db)

	// Core services
	roleService := corerole.NewService(roleRepo, roleAssignmentRepo, metaRepo, idGen)
	roleService.SetEffectivePolicyProviderCacheEntries(s.config.EffectivePolicyProviderCacheEntries)
	// server 全体の upload 上限 (MB) を role policy maxFileSizeMb の cap に使う (#2069/#17389)。
	roleService.SetServerMaxFileSizeMb(int(s.config.MaxFileSize / (1024 * 1024)))
	// drop-in 互換 (#785): Misskey TS は signup root user を user.isRoot=true で
	// マークし meta.rootUserId は set しない。TS DB を引き継いだ mk-go では
	// meta.rootUserId=nil となり admin paths で 403 が返る regression を防ぐ
	// ため、role.Service に userRepo を渡して user.isRoot fallback を有効化する。
	roleService.SetUserRepo(userRepo)
	// プラグインのルートで管理者判定に使う (#2477)。setupPlugins より前に入れる。
	s.pluginRoles = roleService
	s.roleService = roleService
	signupService := coresignup.NewService(userRepo, metaRepo, idGen)
	// ActivityPub 配信のためにローカルユーザーは RSA 鍵対を必要とする。
	signupService.SetKeypairRepo(keypairRepo)
	// FEP-521a Multikey 対応で Ed25519 鍵対も併発行 (#1067 / #1068)。
	signupService.SetKeypairExtraRepo(keypairExtraRepo)
	noteCreateService := corenote.NewCreateService(noteRepo, pollRepo, idGen, followingRepo)
	// Phase 7-1 follow-up (#254): 本家互換の残存 error 検出ロジック用依存を注入。
	// Setter経由なのでテストでは任意に省略可能。既存の同repository変数を再利用。
	noteCreateService.SetBlockingRepo(blockingRepo)
	noteCreateService.SetDriveFileRepo(driveFileRepo)
	noteCreateService.SetMetaRepo(metaRepo)
	noteCreateService.SetChannelRepo(channelRepo)
	// canPublicNote=false の user の public note を home に降格させる
	// silencing 機構 (#1024)。
	noteCreateService.SetSilencingProvider(roleService)
	// mentionLimit を role policy から引く (#2321)。未配線だと
	// DefaultMentionLimit 固定になり、ロールで設定した値が効かない。
	noteCreateService.SetRolePolicyProvider(roleService)
	// reply/mention の main-stream event をスレッドミュート済 recipient に出さない (#1954)。
	noteCreateService.SetThreadMutingRepo(noteThreadMutingRepo)
	noteDeleteService := corenote.NewDeleteService(noteRepo)
	noteDeleteService.SetUserRepo(userRepo) // #1538: resolve author for moderator delete
	noteQueryService := corenote.NewQueryService(noteRepo, followingRepo)
	noteQueryService.SetFavoriteRepo(noteFavoriteRepo)
	noteQueryService.SetThreadMutingRepo(noteThreadMutingRepo)
	userService := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	// PinNote の上限を role policy `pinLimit` で override 可能にする (#1029)。
	userService.SetRolePolicyProvider(roleService)
	// /api/i/update の avatarId / bannerId 経路 (#467) で drive_file の
	// 所有権検証 + URL コピーが必要なため、driveFileRepo を配線する。
	userService.SetDriveFileRepository(driveFileRepo)
	// users/search-by-username-and-host で host=<self-hostname> 入力時に
	// local 限定にする upstream 互換 remap (#1064) のため、自 hostname を
	// Service に渡す。空文字なら remap は "." 一致のみになる。
	userService.SetSelfHostname(s.config.Hostname)
	followingService := corefollowing.NewService(userRepo, followingRepo, followRequestRepo, idGen)
	// remote instance の followersCount / followingCount を Follow/Unfollow 時に
	// incremental 更新する (#596)。未配線でも本機能には影響しないが、admin
	// dashboard の federation pie chart が起動直後以外で 0 に偏る。
	followingService.SetInstanceRepo(instanceRepo)

	// Timeline services (Redis-backed fanout)
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
	// 引用先が DB 側にある (= リレー由来でない投稿を引用している) ケースを
	// 埋められるようにする (#2397)。未配線だと Redis 側の引用先しか復元できず、
	// 残りが「削除された投稿」として描画される。
	ephemeralStore.SetNoteLookup(noteRepo)
	// timeline の hydrate が DB に無い ID を Redis から補えるようにする。
	// store 自体は常に配線し、機能の有効・無効は取り込み側で判定する
	// (無効化しても既に置かれているぶんは TTL 切れまで読めた方がよい)。
	timelineService := coretimeline.NewService(fanoutTimelineService, noteRepo, followingRepo)
	timelineService.SetEphemeralLookup(ephemeralStore)
	// Phase DB-compat (#51): meta から timeline cache cap を動的に読む。
	// 4 つのカラム (perLocal / perRemote / perHome / perList) が反映される。
	timelineFanoutHook.SetCacheLimitsProvider(coretimeline.NewMetaRepoCacheLimits(metaRepo))
	// meta の FTT トグル 2 つをまとめて配線する。
	//   enableFanoutTimeline          -> push / read の両方。無効時は Redis を
	//                                    触らず DB 直行 (upstream の FTT 全停止)
	//   enableFanoutTimelineDbFallback -> read の DB fallback だけ (#2762)。
	//                                    off にすると Redis の持ち分だけ返す。
	//                                    対象は home / local / hybrid の 3 経路で、
	//                                    global は upstream に揃えて対象外
	coretimeline.WireMetaToggles(timelineFanoutHook, timelineService, metaRepo)
	timelineFanoutHook.SetUserListRepo(userListRepo)
	timelineFanoutHook.SetUserRolesLookup(roleService) // #1549: roleTimeline fanout
	// #1686: channel note を channel follower の home timeline へ fanout する。
	timelineFanoutHook.SetChannelFollowerRepo(channelFollowingRepo)
	noteCreateService.SetFanoutHook(timelineFanoutHook)
	// #379: Delete (inbound activity / local notes/delete どちらも) で
	// Redis fanout timelines から note ID を LREM するための hook。
	noteDeleteService.SetTimelineHook(timelineFanoutHook)

	// Channels (Phase 4.2)
	channelService := corechannel.NewService(channelRepo, channelFollowingRepo, noteRepo, idGen)
	// channels/update の moderator bypass (#1540): owner でなくても moderator
	// なら他人の channel を編集できる upstream 互換のため roleService を inject。
	channelService.SetModeratorChecker(roleService)
	// Phase 7-2 follow-up (#271): channel note 着信時に follower ごとの
	// unread row を作成して /api/i の hasUnreadChannel に反映する。
	channelNoteUnreadRepo := repository.NewChannelNoteUnreadRepository(s.db)
	channelService.SetUnreadRepo(channelNoteUnreadRepo)
	noteUnreadRepo := repository.NewNoteUnreadRepository(s.db)
	noteCreateService.SetChannelHook(corechannel.NewNoteCreateHook(channelService))

	// Antennas (Phase 4.3)
	antennaService := coreantenna.NewService(antennaRepo, userRepo, s.redis.Default, idGen)
	antennaService.SetFollowingRepo(followingRepo)
	antennaService.SetUserListRepo(userListRepo)
	antennaService.SetRolePolicyProvider(roleService) // #1029: antennaLimit
	// excludeNotesInSensitiveChannel の判定 (upstream AntennaService:117)。
	antennaService.SetSensitiveChannelLookup(&sensitiveChannelLookup{repo: channelRepo})
	// 宙吊り ID の除去は「本当に DB に無いか」を primary で確かめてから行う
	// (#2719)。レプリカを見ると複製前の行を消してしまう。
	antennaService.SetPrimaryNoteExistence(noteRepo)
	// Phase 7-2 follow-up (#271): antenna 着信時に所有者の unread row を
	// 作成して /api/i の hasUnreadAntenna に反映する。
	antennaNoteUnreadRepo := repository.NewAntennaNoteUnreadRepository(s.db)
	antennaService.SetUnreadRepo(antennaNoteUnreadRepo)
	noteCreateService.SetAntennaHook(coreantenna.NewNoteCreateHook(antennaService))

	// Clips (Phase 4.4)
	clipService := coreclip.NewService(clipRepo, clipNoteRepo, noteRepo, idGen)
	clipService.SetRolePolicyProvider(roleService) // #1029: clipLimit / noteEachClipsLimit

	// Pages / Flash (Phase 4.5)
	pageService := corepage.NewService(pageRepo, pageLikeRepo, idGen)
	flashService := coreflash.NewService(flashRepo, flashLikeRepo, idGen)

	// featured engagement ランキング (#1687)。reaction / renote の hook が
	// global / in-channel / per-user の Redis windowed ZSET を更新し、
	// notes/featured・users/featured-notes が読む。upstream と同じ default Redis。
	featuredService := corefeatured.NewService(s.redis.Default)
	noteCreateService.SetFeaturedRanking(featuredService)

	// Reactions
	reactionService := corereaction.NewService(noteRepo, reactionRepo, emojiRepo, followingRepo, idGen)
	reactionService.SetFeaturedRanking(featuredService)

	// Reactions buffering (issue #57): meta.enableReactionsBuffering が true なら
	// Redis にバッファし、30秒ごとに DB へ flush する。
	var reactionCountWriter corereaction.ReactionCountWriter
	if bufMeta, err := metaRepo.Fetch(); err == nil && bufMeta.EnableReactionsBuffering {
		reactionCountWriter = corereaction.NewBufferedWriter(s.redis.Default, noteRepo)
		reactionService.SetCountWriter(reactionCountWriter)
	} else {
		reactionCountWriter = reactionService.CountWriter()
	}

	// Notifications (Redis Streams)
	// keyPrefix で TS 本家と同じ `<host>:notificationTimeline:*` 等に揃える (#362)。
	notificationService := corenotification.NewService(s.redis.Default, idGen, s.config.Redis.KeyPrefix())
	notificationService.SetNoteUnreadRepo(noteUnreadRepo)
	notificationHook := corenotification.NewHook(notificationService, userRepo)
	notificationHook.SetNoteUnreadRepo(noteUnreadRepo)
	// note 通知 (notify='normal' フォロワーへの投稿通知、#1559) の fan-out 依存。
	notificationHook.SetNoteNotifyRepos(followingRepo, renoteMutingRepo)
	// notificationRecieveConfig type=='list' gate の member 確認用 (#1775)。
	notificationHook.SetUserListRepo(userListRepo)
	// reply/mention 通知をスレッドミュート済 recipient に出さない (#1954)。
	notificationHook.SetThreadMutingRepo(noteThreadMutingRepo)
	noteCreateService.SetNotificationHook(notificationHook)
	noteCreateService.SetUserRepo(userRepo)
	// roleAssigned 通知 (#1559): local public role 割当時に発火し、entity 側で
	// read 時に packed role へ解決する。lookup は role 削除済なら (nil,false)。
	roleService.SetRoleAssignNotifier(notificationHook)
	abuseReportRepoForNotif := repository.NewAbuseReportRepository(s.db)
	roleNotifLookup := func(roleID string) (map[string]any, bool) {
		r, err := roleRepo.FindByID(roleID)
		if err != nil || r == nil {
			return nil, false
		}
		return entity.PackRole(r, roleService.CountAssignedUsers(roleID), idGen, corerole.DefaultPolicies()), true
	}
	// abuseReport 通知の現在の状態を read 時に引く (#2868)。通知は作成時点しか
	// 持たないので、他のモデレーターが対処しても通知欄は「未対応」のまま残る。
	// 通報が消えていれば通知ごと drop する (roleAssigned と同じ形)。
	//
	// **REST は batch、WebSocket は 1 件。** 通知一覧はページあたり最大 100 件
	// なので 1 件ずつ引くと数百 SELECT が直列に走る。realtime は 1 件ずつ届く
	// ので同じ関数を 1 要素で呼ぶ。
	// 絵文字の登録申請の結果を read 時に引く (#2934)。通知には applicationId
	// しか積んでいないので、承認/却下と却下理由はここで解決する。
	//
	// **申請が消えていたら false を返して通知ごと落とす。** 経緯を辿れない
	// 「処理されました」だけの通知は読んだ人に何も伝えない。
	emojiApplicationRepoForNotif := repository.NewEmojiApplicationRepository(s.db)
	emojiApplicationNotifLookup := func(applicationID string) (entity.EmojiApplicationStatus, bool) {
		app, err := emojiApplicationRepoForNotif.FindByID(applicationID)
		if err != nil {
			if !repository.IsNotFound(err) {
				// **DB 障害を「削除済み」に丸めない (#2792)。**
				slog.Warn("notification: emoji application lookup failed",
					"applicationId", applicationID, "err", err)
			}
			return entity.EmojiApplicationStatus{}, false
		}
		out := entity.EmojiApplicationStatus{Name: app.Name, Status: app.Status}
		if app.RejectReason != nil {
			out.RejectReason = *app.RejectReason
		}
		return out, true
	}

	// アカウントの登録申請の read 時の状態 (#2987)。**回答は載せない** —
	// 運営者が定義した任意の項目で氏名や連絡先が入りうる。
	signupApplicationRepoForNotif := repository.NewSignupApplicationRepository(s.db)
	signupApplicationNotifLookup := func(applicationID string) (entity.SignupApplicationStatus, bool) {
		app, err := signupApplicationRepoForNotif.FindByID(applicationID)
		if err != nil {
			if !repository.IsNotFound(err) {
				// **DB 障害を「削除済み」に丸めない (#2792)。**
				slog.Warn("notification: signup application lookup failed",
					"applicationId", applicationID, "err", err)
			}
			return entity.SignupApplicationStatus{}, false
		}
		return entity.SignupApplicationStatus{Status: app.Status}, true
	}

	abuseNotifStates := abuseReportRepoForNotif.FindStatesByIDs
	abuseNotifLookup := func(reportID string) (entity.AbuseReportStatus, bool) {
		states, err := abuseNotifStates([]string{reportID})
		if err != nil {
			// **DB 障害を「削除済み」に丸めない (#2792)。** realtime はこの 1 件を
			// 落とすしかないが、既読位置を動かさないので未読の合図は残る。
			slog.Warn("notification: abuse report state lookup failed", "reportId", reportID, "err", err)
			return entity.AbuseReportStatus{}, false
		}
		st, ok := states[reportID]
		if !ok {
			return entity.AbuseReportStatus{}, false
		}
		out := entity.AbuseReportStatus{Resolved: st.Resolved}
		if st.ResolvedAs != nil {
			out.ResolvedAs = *st.ResolvedAs
		}
		if st.AssigneeID != nil {
			out.AssigneeID = *st.AssigneeID
		}
		return out, true
	}
	followingService.SetNotificationHook(notificationHook)
	reactionService.SetNotificationHook(notificationHook)

	// Web Push (Phase 9.3): 通知作成後にサブスクライバーへpush配信する。
	webPushService := corewebpush.NewService(s.queueClient)
	notificationHook.SetWebPushPublisher(webPushService)
	notificationHook.SetPackers(
		corewebpush.NewUserRepoPacker(userRepo),
		corewebpush.NewNoteRepoPacker(noteRepo, idGen, followingRepo, driveFileRepo),
	)
	webPushCache := corewebpush.NewSubscriptionCache(swSubRepo, s.redis.Default)
	// Web Push delivery: webpush-go の HTTPClient field に SSRF-safe client
	// を注入して FCM / Mozilla / Apple endpoint への配送も outbound 政策
	// (forward proxy / outgoingAddress 等) に従わせる (#638)。
	webPushSender := processors.LibraryWebPushSender{HTTPClient: s.outboundClient(30 * time.Second)}
	webPushProcessor := processors.NewWebPushProcessor(
		webPushCache, swSubRepo, metaRepo, webPushSender, s.config.URL,
	)
	s.queueServer.Handle(queue.TaskTypeWebPush, webPushProcessor.Handle)

	// Webhook (Phase 9.5): ユーザー/システムwebhookの配信を非同期処理する。
	webhookService := corewebhook.NewService(s.queueClient, webhookRepo, systemWebhookRepo, s.config.URL)
	webhookNoteHook := corewebhook.NewNoteCreateHook(webhookService, idGen)
	// reply/mention webhook をスレッドミュート済 recipient に出さない (#1965)。
	webhookNoteHook.SetThreadMutingRepo(noteThreadMutingRepo)
	// note payload の embed (renote/reply/引用先) を受信者の可視性で gate する。
	webhookNoteHook.SetFollowingRepo(followingRepo)
	noteCreateService.SetWebhookHook(webhookNoteHook)
	webhookReactionHook := corewebhook.NewReactionCreateHook(webhookService, idGen)
	webhookReactionHook.SetFollowingRepo(followingRepo)
	reactionService.SetWebhookHook(webhookReactionHook)
	followingService.SetWebhookHook(corewebhook.NewFollowingHook(webhookService))
	signupService.SetWebhookHook(corewebhook.NewSignupHook(webhookService))
	// Webhook delivery: SSRF-safe transport + forward proxy 経由で user-supplied
	// URL に POST する (#638)。Timeout は processors 側の DefaultWebhookTimeout
	// に揃える。
	webhookProcessor := processors.NewWebhookProcessor(webhookRepo, systemWebhookRepo, s.outboundClient(processors.DefaultWebhookTimeout), s.config.Host)
	s.queueServer.Handle(queue.TaskTypeUserWebhook, webhookProcessor.HandleUser)
	s.queueServer.Handle(queue.TaskTypeSystemWebhook, webhookProcessor.HandleSystem)

	// Blocking & Muting
	blockingService := coreblocking.NewService(userRepo, blockingRepo, followingRepo, idGen)
	// Block→自動 unfollow 経路でも remote instance counter を更新 (#596)
	blockingService.SetInstanceRepo(instanceRepo)
	// block 時に保留中の follow request を双方向で取り消す。upstream
	// UserBlockingService.block の cancelRequest 相当。
	blockingService.SetFollowRequestCanceller(followingService)
	mutingService := coremuting.NewService(userRepo, mutingRepo, idGen)
	renoteMutingService := coremuting.NewRenoteService(userRepo, renoteMutingRepo, idGen)
	followingService.SetBlockingChecker(blockingService)
	reactionService.SetBlockingChecker(blockingService)
	notificationHook.SetMuteChecker(mutingService)

	// Polls
	pollService := corepoll.NewService(noteRepo, pollRepo, pollVoteRepo, followingRepo, idGen)
	pollService.SetNotificationHook(notificationHook)
	// note 著者にブロックされた user の投票を弾く (upstream youHaveBeenBlocked、#1538)。
	pollService.SetBlockingChecker(blockingService)
	// poll federation hook は deliverService が下で生成されるので、
	// `pollService.SetFederationHook(...)` の呼び出しは deliverService 構築後
	// (router.go:493 以降) に行う。下方の wire-up コメント参照。
	// pollEnded 通知は周期 ticker (60s) で expiresAt < NOW() AND notifiedAt
	// IS NULL を scan して著者 + ローカル投票者に発火する (#690)。Misskey TS
	// は BullMQ delayed job で実装しているが mk-go では queue infra を経由
	// せずに簡素な ticker で実現。partial index で空 scan のコストは最小。
	// **queue role でだけ回す (#2459)。** 常駐ワーカーは複数プロセス構成で
	// 同じ行を拾う。`ListExpiredUnnotified` → 通知 → `MarkNotified` の順で
	// ロックも CAS も無いので、server を N 台にすると同じ投票終了通知が N 回
	// 飛ぶ (Web Push も)。同ファイルの reaction flush / deliveryhealth /
	// retention aggregate は同じ理由で既にゲートされており、ここだけ漏れていた。
	if s.role.RunsQueue() {
		pollExpiryWorker := corepoll.NewExpiryWorker(pollRepo, pollVoteRepo, noteRepo, userRepo, notificationService, 60*time.Second, 100)
		s.startConstructionWorker(pollExpiryWorker.Run)
	}
	// pollVoted を note の stream topic に publish して subscribe 中の
	// frontend (note 詳細 / timeline) が reload なしで count を更新できる
	// ようにする (#690)。streamPubSub は下方で生成されるため遅延配線。

	// Drive storage (Phase 4.9: Meta に基づいて Local / S3 を切り替え)
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
	driveService := coredrive.NewService(driveFileRepo, driveFolderRepo, driveStorage, idGen)
	// storedInternal=true な行 (object storage 有効化より前 / TS 時代に保存された
	// ファイル) はローカル FS にあるので、削除経路がそちらに届くようにする。
	driveService.SetLocalStorage(localDriveStorage)
	// ファイル削除時の object storage 実体削除を queue に逃がす (#2325)。
	driveService.SetObjectDeleteEnqueuer(s.queueClient)
	// drive/files/show は moderator なら他人 / リモートユーザー所有の file
	// も返せるようにする (upstream Misskey の roleService.isModerator 経路
	// と一致)。リモート添付メディアの詳細閲覧に必要。
	driveService.SetRoleChecker(roleService)
	// 分割アップロード (#2313)。設定も backend (#2315) も meta から都度読むので、
	// admin の変更は再起動なしで効く。オブジェクトストレージ未使用の構成では
	// backend が MultipartStorage を満たさず、配線しても機能は無効のままになる。
	chunkedUploadSessionRepo := repository.NewChunkedUploadSessionRepository(s.db)
	driveService.SetChunkedUpload(chunkedUploadSessionRepo, func() coredrive.ChunkedUploadSettings {
		m, err := metaRepo.Fetch()
		if err != nil {
			return coredrive.ChunkedUploadSettings{}
		}
		return coredrive.ChunkedUploadSettingsFromMeta(m)
	})
	if _, ok := coredrive.ResolveStorage(driveStorage).(coredrive.MultipartStorage); !ok {
		// 起動時点の状態を一度だけ警告する。maxFileSize がリバースプロキシの
		// 上限を超えている構成では「設定上は許可されているのに必ず失敗する」
		// 状態になるため、回避手段が使えないことを明示しておく。
		// あとからオブジェクトストレージを有効にすれば再起動なしで使えるように
		// なるので、恒久的な無効化ではない (#2315)。
		slog.Warn("chunked upload is currently disabled: object storage is not configured",
			"maxFileSizeMb", s.config.MaxFileSize/(1024*1024))
	}

	// Image processing (Phase 4.8)
	imgProcessor := coredrive.NewDefaultImageProcessor()
	driveService.SetImageProcessor(imgProcessor)
	driveService.SetVideoProcessor(coredrive.NewFFmpegVideoProcessor(imgProcessor, nil))

	// Sensitive media detection (#44 / #406)。mk-go は backend に ML runtime
	// を抱えないので、operator が任意の互換 service を立てて URL を
	// `nsfwDetectorUrl` 設定に書く。URL が空なら detector=nil で自動付与は
	// 走らず、手動 isSensitive フラグのみ機能する (default)。動作モード
	// (Detection mode / Sensitivity / SetFlagAutomatically / EnableForVideos /
	// SilencedHosts) は admin の `meta` カラム経由で TS 互換に制御。
	if sensMeta, err := metaRepo.Fetch(); err == nil {
		sensCfg := coredrive.SensitiveConfig{
			Detection:            sensMeta.SensitiveMediaDetection,
			Sensitivity:          sensMeta.SensitiveMediaDetectionSensitivity,
			SetFlagAutomatically: sensMeta.SetSensitiveFlagAutomatically,
			EnableForVideos:      sensMeta.EnableSensitiveMediaDetectionForVideos,
			SilencedHosts:        sensMeta.MediaSilencedHosts,
		}
		var nsfwDetector coredrive.SensitiveDetector
		if url := s.config.NSFWDetectorURL; url != "" {
			timeout := s.config.NSFWDetectorTimeout
			if timeout <= 0 {
				timeout = coredrive.DefaultHTTPDetectorTimeout
			}
			nsfwDetector = coredrive.NewHTTPDetectorWithOptions(url, s.outboundClient(timeout), coredrive.HTTPDetectorOptions{
				AuthHeader: s.config.NSFWDetectorAuthHeader,
				Timeout:    timeout,
			})
		}
		driveService.SetSensitiveDetection(nsfwDetector, sensCfg)

		// 公式 sensitive-detector (upstream 2026.7.0 #17570)。接続設定は meta
		// 駆動 (コントロールパネルから変更可) なので live provider で渡す。
		// timeout は per-request に meta 値を context で適用するため、client
		// 自体の Timeout は掛けない (0 = transport 側 SSRF guard のみ)。
		driveService.SetOfficialSensitiveDetector(coredrive.NewOfficialDetector(s.outboundClient(0)))
		driveService.SetSensitiveSettingsProvider(func() coredrive.SensitiveSettings {
			m, err := metaRepo.Fetch()
			if err != nil || m == nil {
				return coredrive.SensitiveSettings{Config: sensCfg}
			}
			settings := coredrive.SensitiveSettings{
				Config: coredrive.SensitiveConfig{
					Detection:            m.SensitiveMediaDetection,
					Sensitivity:          m.SensitiveMediaDetectionSensitivity,
					SetFlagAutomatically: m.SetSensitiveFlagAutomatically,
					EnableForVideos:      m.EnableSensitiveMediaDetectionForVideos,
					SilencedHosts:        m.MediaSilencedHosts,
				},
				Official: coredrive.OfficialDetectorSettings{
					TimeoutMs:           m.SensitiveMediaDetectionTimeout,
					MaxImagesPerRequest: m.SensitiveMediaDetectionMaxImagesPerRequest,
				},
			}
			if m.SensitiveMediaDetectionAPIURL != nil {
				settings.Official.APIURL = *m.SensitiveMediaDetectionAPIURL
			}
			if m.SensitiveMediaDetectionAPIKey != nil {
				settings.Official.APIKey = *m.SensitiveMediaDetectionAPIKey
			}
			return settings
		})
	}

	// Export / Import workers (Phase 9.4): drive に保存するエクスポートと
	// drive から読み出すインポートを queue 経由で非同期処理する。
	exporter := coretransfer.NewExporter(coretransfer.ExporterDeps{
		UserRepo:         userRepo,
		NoteRepo:         noteRepo,
		PollRepo:         pollRepo,
		FollowingRepo:    followingRepo,
		BlockingRepo:     blockingRepo,
		MutingRepo:       mutingRepo,
		NoteFavoriteRepo: noteFavoriteRepo,
		AntennaRepo:      antennaRepo,
		ClipRepo:         clipRepo,
		ClipNoteRepo:     clipNoteRepo,
		UserListRepo:     userListRepo,
		Drive:            driveService,
		Notifier:         notificationService,
		IDGen:            idGen,
		// custom-emojis export: 全 local emoji を ListLocal で列挙し、各画像を
		// SSRF-safe client で download して zip 化する (#1217)。画像取得は最大
		// 60s/個、cap 8 MiB。
		EmojiRepo:         emojiRepo,
		EmojiImageFetcher: coretransfer.NewHTTPEmojiImageFetcher(s.outboundClient(60*time.Second), s.config.UserAgent, 0),
	})
	driveReader := coretransfer.NewRepoBackedDriveReader(driveFileRepo, driveStorage)
	// storedInternal=true な行 (object storage 有効化より前に保存されたファイル)
	// はローカル FS にあるので、import がそちらも読めるようにする (#2315)。
	driveReader.SetLocalStorage(localDriveStorage)
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
	exportProcessor := processors.NewExportProcessor(exporter)
	importProcessor := processors.NewImportProcessor(importer)
	s.queueServer.Handle(queue.TaskTypeExport, exportProcessor.Handle)
	s.queueServer.Handle(queue.TaskTypeImport, importProcessor.Handle)

	// Phase 9.9: admin/emoji/import-zip を非同期で処理するワーカー。
	// driveReader と driveService を再利用し ZIP 展開→Drive 保存→emoji 行作成。
	emojiImporter := coreemojiimport.NewImporter(coreemojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: emojiRepo,
		Drive:     driveReader,
		Uploader:  driveService,
		IDGen:     idGen,
		// zip インポートは publishEmoji* を通らないので、絵文字由来の
		// アバターデコレーション (#2975) のキャッシュはここで捨てる。
		DecorationCache: emojiDecorationResolver,
	})
	emojiImportProcessor := processors.NewImportCustomEmojisProcessor(emojiImporter)
	s.queueServer.Handle(queue.TaskTypeImportCustomEmojis, emojiImportProcessor.Handle)

	// Search (Phase 4.6)
	// 設定に従って provider を選択する。Meilisearch が設定されていれば
	// それを使い、そうでなければ SQL ILIKE フォールバック。
	searchService := coresearch.NewService(buildSearchProvider(s.config, noteRepo, followingRepo, idGen))
	noteCreateService.SetIndexHook(coresearch.NewNoteIndexHook(searchService))
	noteDeleteService.SetIndexHook(coresearch.NewNoteIndexHook(searchService))

	// ActivityPub
	apURLs := activitypub.NewURLBuilder(s.config.URL)
	apRenderer := activitypub.NewRenderer(apURLs)
	// Mention tag を AP Note に埋め込むための resolver。
	apRenderer.SetMentionResolver(corefederation.NewUserMentionResolver(userRepo, apURLs))
	apRenderer.SetFileResolver(driveFileRepo)
	apRenderer.SetEmojiResolver(emojiRepo)
	apRenderer.SetNoteResolver(noteRepo)
	apRenderer.SetPollResolver(pollRepo)
	// system actor の icon/banner fallback 用に meta.iconUrl / bannerUrl を都度解決する (#1969)。
	apRenderer.SetInstanceImageLookup(func() (string, string) {
		m, err := metaRepo.Fetch()
		if err != nil {
			return "", ""
		}
		icon, banner := "", ""
		if m.IconURL != nil {
			icon = *m.IconURL
		}
		if m.BannerURL != nil {
			banner = *m.BannerURL
		}
		return icon, banner
	})
	// actor の avatar/banner drive file の isSensitive / comment を解決して
	// AP icon/image に sensitive/name を付ける (#2031)。
	apRenderer.SetDriveFileMetaLookup(func(fileID string) (bool, *string, bool) {
		f, err := driveFileRepo.FindByID(fileID)
		if err != nil || f == nil {
			return false, nil, false
		}
		return f.IsSensitive, f.Comment, true
	})
	// config.URL は "https://example.com" 形式。ホスト部分だけ抽出して
	// apRenderer と webfinger 側で共有する (下の resolver 設定でも再利用)。
	localHost := ""
	if u, err := urlpkg.Parse(s.config.URL); err == nil {
		localHost = u.Host
		apRenderer.SetHost(localHost)
	}
	// AP outbound client: SSRF-safe transport を適用 (#323)。
	// config.AllowedPrivateNetworks で開発時の self-loop を許可できる。
	// config.Proxy / ProxyBypassHosts も #485 で wire 済み。
	apHTTPClient := s.outboundClient(30 * time.Second)
	apClient := activitypub.NewClient(apHTTPClient, s.config.UserAgent)
	// meta.allowExternalApRedirect が false なら AP fetch でのリダイレクトを拒否する。
	if m, err := metaRepo.Fetch(); err == nil && !m.AllowExternalApRedirect {
		apClient.DisableRedirect()
	}
	apFetcher := corefederation.NewAPFetcher(apClient)
	federationResolver := corefederation.NewResolver(userRepo, noteRepo, apURLs, apFetcher, idGen)
	publickeyRepo := repository.NewUserPublickeyRepository(s.db)
	federationResolver.SetPublickeyRepo(publickeyRepo)
	// FEP-521a Multikey 対応で remote actor の assertionMethod[] を parse して
	// user_publickey_extra に persist する (#1067 / #1070)。
	federationResolver.SetPublickeyExtraRepo(repository.NewUserPublickeyExtraRepository(s.db))
	federationResolver.SetPollRepo(pollRepo)
	// リモート actor の featured コレクション (ピン留め投稿) を取り込む (#2552)。
	// inbound の Add (processor 側) はピン留めされた瞬間にしか飛んで来ないので、
	// 観測より前からのピン留めはこちらでしか拾えない。
	federationResolver.SetPinningRepo(piningRepo, idGen)
	// AP vote (Note.name + inReplyTo to a poll) を local poll service に
	// 流して投票として記録する (#690)。これがないとリモートからの投票が
	// 通常 reply として扱われ frontend に DM のように表示される。
	federationResolver.SetPollVoter(pollService)
	federationResolver.SetEmojiRepo(emojiRepo)
	federationResolver.SetDriveFileRepo(driveFileRepo)
	// AP attachment dimension probe (#461) 用 outbound HTTP client。
	// SSRF-safe transport で内部 IP / cloud metadata エンドポイントへの
	// アクセスを拒否する。timeout は単発 image fetch なので apHTTPClient
	// (30s) より短めの 10s にする。
	imageProbeClient := s.outboundClient(10 * time.Second)
	federationResolver.SetImageProbeClient(imageProbeClient)
	federationProcessor := corefederation.NewProcessor(federationResolver, followingService, reactionService, noteDeleteService, userRepo, noteRepo)
	federationProcessor.SetLocalBaseURL(s.config.URL)

	// users/show 経由で host が指定されたリモートユーザーをローカル DB に
	// キャッシュが無くても解決できるようにする (#269)。webfinger で actor URI
	// を引いてから federationResolver.ResolveActor で User row を upsert する。
	// redirect 追跡必須のため apClient とは *http.Client を分離し、
	// apClient.DisableRedirect() の影響を受けないようにする。Timeout は
	// user-facing API 経由で呼ばれるので応答性優先で 10s に設定する。
	webfingerClient := activitypub.NewWebFingerClient(s.outboundClient(10*time.Second), s.config.UserAgent)
	userService.SetRemoteUserResolver(corefederation.NewRemoteUserResolver(
		webfingerClient, federationResolver, userRepo, localHost,
	))

	// Instance management (Phase 3 Step H)
	instanceService := coreinstance.NewService(instanceRepo, metaRepo, idGen)
	federationResolver.SetInstanceTracker(instanceService)
	// #1538: reactionAcceptance gate — role-gated emoji + media-silenced host。
	// instanceService 生成後 (= 本ブロック) で配線する (reactionService は line ~281
	// で先に作られるため、media-silence checker はここで遅延注入する)。
	reactionService.SetUserRolesProvider(roleService)
	reactionService.SetMediaSilenceChecker(instanceService)
	// **サイレンスしたホストからのフォローを承認制にする。** 未配線だと
	// 未施錠アカウントが承認なしでフォローされ、followers 限定ノートが
	// 配送される (upstream UserFollowingService の 4 つ目の OR 条件)。
	followingService.SetSilencedHostChecker(instanceService)
	// ホワイトリスト連合 (federation: specified) / blockedHosts に対する gate を
	// resolver の入口 (fetchActor / resolveNoteOnce / IngestNoteWithCreated) に
	// 適用する。deliver_service / inboxProcessor と同じ instanceService を共有。
	federationResolver.SetHostBlockChecker(instanceService)
	federationResolver.SetSilencedHostChecker(instanceService) // #2106 N14: silenced host の public note を home 降格
	// 新規 instance row 発見時に nodeinfo を取得して metadata を更新する。
	// admin/federation/refresh-remote-instance-metadata でも同じ fetcher を
	// 再利用して on-demand で再取得する (#351 フォロー)。
	metadataFetcher := coreinstance.NewFetchMetadataService(instanceRepo, apFetcher)
	instanceService.SetMetadataFetcher(metadataFetcher)

	// inbox worker から呼ばれる MarkRequestReceived を per-host で 1s buffer
	// に集約。同一 remote host の連続 inbox 受信で 10k UPDATE が 1 UPDATE に
	// 縮退する (#569)。
	instanceTouchBuffer := coreinstance.NewTouchBuffer(instanceService, time.Second)
	instanceTouchBuffer.Start(context.Background())
	s.registerShutdownHook(func(_ context.Context) { instanceTouchBuffer.Close() })

	// 署名方式の観測 (#2393)。inbound は同一 host から高頻度に来るので、
	// TouchBuffer と同じ縮退方式で per-host per系統 1 UPDATE にまとめる。
	//
	// flush 間隔を TouchBuffer の 1s より長い 60s にしているのは、こちらが表示用の
	// メタデータで秒単位の鮮度を要求しないため。1s のままだと連合先の数だけ毎秒
	// UPDATE が増え、instance touch の書き込み負荷を実質倍にしてしまう。相手の
	// 署名方式はそう頻繁に変わらないので、60s 窓で十分に追随できる。
	sigCapRepo := repository.NewInstanceSignatureCapabilityRepository(s.db)
	sigCapBuffer := coreinstance.NewCapabilityBuffer(sigCapRepo, time.Minute)
	sigCapBuffer.Start(context.Background())
	s.registerShutdownHook(func(_ context.Context) { sigCapBuffer.Close() })
	// 宣言 (assertionMethod の Ed25519) は actor resolve 経路で同期に書く。
	// inbound ほど高頻度ではないので buffer を挟まない。
	federationResolver.SetSignatureCapabilityDeclarer(sigCapRepo)

	// AP delivery: DeliverService + フック登録 + queue processor 登録
	deliverService := corefederation.NewDeliverService(s.queueClient, userRepo, followingRepo, keypairRepo, apURLs)
	deliverService.SetHostBlockChecker(instanceService)
	// FEP-521a Multikey 対応で recipient capable + sender Ed25519 鍵あり 経路で
	// Ed25519 sign を試行できるよう wire (#1067 / #1071)。
	deliverService.SetKeypairExtraRepo(keypairExtraRepo)
	// **未配線だと actor の `toot:suspended` を読まない** — 由来を持てない状態で
	// 自動凍結すると、モデレーターが解除しても次の refresh で無言で戻る (#2973)。
	federationResolver.SetSuspensionOriginRepo(suspensionOriginRepo)
	deliverService.SetPublickeyExtraRepo(repository.NewUserPublickeyExtraRepository(s.db))
	// test (#780) で queue を bypass して同期 deliver する hook を後付けで
	// 差し替えられるよう、Server から参照を保持する。
	s.deliverSvc = deliverService

	// poll federation hook: ローカル user が remote poll に投票したら AP Note
	// (with name + inReplyTo) を author の inbox に配信する (#690)。
	pollService.SetFederationHook(corefederation.NewPollDeliveryHook(apRenderer, deliverService, userRepo, apURLs, idGen))

	// Relay 連携 (#161): relay actor system account + relay.Service。
	// relay.Service は AP delivery を必要とするので deliverService 構築の
	// 直後でセットアップする。admin/relays ハンドラへ注入し、Accept/Reject
	// inbox 処理で status を更新できるように processor にも渡す。
	sysAcctSvc := coresystemaccount.NewService(userRepo, keypairRepo, repository.NewSystemAccountRepository(s.db), idGen)
	// FEP-521a Multikey 対応で system account も Ed25519 鍵対を併発行 (#1067 / #1068)。
	sysAcctSvc.SetKeypairExtraRepo(keypairExtraRepo)
	// #1704: list に remote user を追加したとき proxy account がフォローして連合
	// 投稿を受信できるようにする enqueuer。push / create-from-public で使う。
	listProxyFollow := coreuserlist.NewProxyFollower(sysAcctSvc, s.queueClient)
	relaySvc := corerelay.NewService(repository.NewRelayRepository(s.db), sysAcctSvc, apRenderer, deliverService, idGen)
	// relay 配送 activity に RsaSignature2017 LD-Signature を付与する (#1870)。
	// relay は downstream へ転送する際に元 HTTP-sig を保持できないので、LD-sig が
	// 無いと downstream が真正性を検証できず reject する。署名者は activity の
	// author (local user)。
	relaySvc.SetLdSigning(keypairRepo, ld.NewProcessor(), s.config.URL)

	// AP fetch のデフォルト signer に instance.actor を配線する (#419)。
	// IceShrimp.NET 等の authorized-fetch peer では未署名 GET が 401 で
	// 弾かれるため、apFetcher.FetchObject は signed GET を優先しに行く。
	// 署名失敗時は従来通り unsigned にフォールバック (deliver_service と
	// 同じ keyID 形式 = `<baseURL>/users/<id>#main-key`)。
	apFetcher.SetSigner(newInstanceActorSigner(sysAcctSvc, keypairRepo, apURLs))

	noteDeliveryHook := corefederation.NewNoteDeliveryHook(deliverService, apRenderer, apURLs, idGen, userRepo, noteRepo)
	noteDeliveryHook.SetRelayBroadcaster(relaySvc)
	noteCreateService.SetFederationHook(noteDeliveryHook)
	followingDeliveryHook := corefederation.NewFollowingDeliveryHook(deliverService, apRenderer, apURLs)
	followingService.SetFederationHook(followingDeliveryHook)
	// inbound Follow に対する Accept 返送は processor から直接呼ぶ (original
	// Follow の id を保持したまま相手に返すため、service 層を経由しない)。
	federationProcessor.SetInboundFollowAcceptor(followingDeliveryHook)
	reactionService.SetFederationHook(corefederation.NewReactionDeliveryHook(deliverService, apRenderer, apURLs, idGen, userRepo))
	// local user が remote user を (un)block した際に Block / Undo(Block) を
	// 相手 inbox へ配信する (#1560)。
	blockingService.SetFederationHook(corefederation.NewBlockingDeliveryHook(deliverService, userRepo, apRenderer))
	noteDeleteHook := corefederation.NewNoteDeleteDeliveryHook(deliverService, apRenderer, apURLs)
	noteDeleteHook.SetUserRepo(userRepo)
	noteDeleteHook.SetNoteRepo(noteRepo)
	noteDeleteService.SetFederationHook(noteDeleteHook)
	deliverProcessor := processors.NewDeliverProcessor(apClient)
	// 配信結果に応じて instance.isNotResponding を更新する
	deliverProcessor.SetResponseHook(instanceService)
	// suspend / block されたインスタンスへは dispatch 時に配送を止める (#1404)。
	// enqueue 時の isBlockedInbox を通り抜ける retry-backoff 中・積み残しジョブも弾く。
	deliverProcessor.SetDeliveryGate(instanceService)
	// FEP-521a Multikey 対応で host 単位の Ed25519 degrade flag を Redis に
	// 持つ (#1067 / #1071)。Ed25519 sign 失敗時 5min 同 host を RSA only に
	// 縮退する safety net。
	deliverProcessor.SetRedis(s.redis.Default)
	// **署名鍵は payload に載せず、配送時に引く。** queue の job は
	// `admin/queue/jobs` が moderator へ返すので、載せると鍵がそこから読める。
	deliverProcessor.SetSigningKeySource(processors.NewRepoSigningKeySource(keypairRepo, keypairExtraRepo))
	// Ed25519 署名の配送が 2xx を返した (= 同期的には拒否されなかった) ことを
	// 記録する (#2393)。
	deliverProcessor.SetSignatureCapabilityRecorder(sigCapBuffer)
	// deliverSuspendedSoftware: 対象インスタンスへの配送をスキップする
	if suspMeta, err := metaRepo.Fetch(); err == nil && len(suspMeta.DeliverSuspendedSoftware) > 0 {
		deliverProcessor.SetSuspendedChecker(
			corefederation.NewSuspendedChecker(suspMeta.DeliverSuspendedSoftware, instanceRepo),
		)
	}
	s.queueServer.Handle(queue.TaskTypeDeliver, deliverProcessor.Handle)

	// Inbox (#534 / #565): HTTP handler は body+signature 関連 header を
	// payload に詰めて 202 即返し。worker 側 (本 processor) で signature
	// 検証 + host block + instance touch + chart hook + Process を実行
	// する。これにより handler 同期の RSA-2048 verify (~1-2ms) が消えて
	// inbound throughput が ~2x に改善する (#565)。順序保証は各 activity
	// handler の冪等性で吸収する Misskey TS 互換戦略。
	inboxProcessor := processors.NewInboxProcessor(federationProcessor)
	// LD-Signature 検証で解決する著者に「リレー由来」の印を付ける (#2340)。
	inboxProcessor.SetRelayActorChecker(relaySvc)
	inboxProcessor.SetSignatureVerifier(federationResolver)
	inboxProcessor.SetHostBlockChecker(instanceService)
	inboxProcessor.SetInstanceTracker(instanceTouchBuffer)
	inboxProcessor.SetSignatureCapabilityRecorder(sigCapBuffer)
	// LD-Signature verifier (#1164 Phase D)。inbound activity body に signature
	// field があれば RsaSignature2017 + 2026.5.4 hardening を実行する。signature
	// 無し / verify pass の activity は素通し、verify fail は drop。
	inboxProcessor.SetLDSignatureVerifier(corefederation.NewLDSignatureVerifier(publickeyRepo))
	// 処理済み activity の再投函を弾く。署名の Date に窓を入れても、窓の内側は
	// まだ投げ直せる。**Default インスタンスを使う** — 2FA の replay guard と
	// 同じ用途 (短命の使用済みマーカー) で、timelines / reactions のような
	// 用途別インスタンスに載せる理由が無い。
	inboxProcessor.SetInboxReplayGuard(processors.NewRedisInboxReplayGuard(s.redis.Default))
	// chartHook は後段で SetChartHook 注入する (deliverProcessor と同じ pattern)。
	s.queueServer.Handle(queue.TaskTypeInbox, inboxProcessor.Handle)

	// Remote notes cleaning (issue #46): enqueue 契機は scheduler の cron
	// (RegisterCleanRemoteNotesJob, 毎日 04:00, TS 'cleanRemoteNotes' 相当,
	// #1563)。旧実装は 6h time.Ticker だった。Handle は常に登録し、processor が
	// cfg.Enabled で gate する (起動時 meta から構築)。
	// **毎回読む。** 起動時に固定すると、運営者が管理画面で止めてもプロセスを
	// 再起動するまで削除が走り続ける (不可逆な操作の唯一の停止手段)。
	cleanCfgFn := func() processors.CleanRemoteNotesConfig {
		cleanMeta, err := metaRepo.Fetch()
		if err != nil || cleanMeta == nil {
			// **読めなければ動かさない。** 判定材料が無いまま不可逆な削除を
			// 続けるより、次の cron まで待つほうが安全。
			return processors.CleanRemoteNotesConfig{}
		}
		return processors.CleanRemoteNotesConfig{
			Enabled:              cleanMeta.EnableRemoteNotesCleaning,
			ExpiryDays:           cleanMeta.RemoteNotesCleaningExpiryDaysForEachNotes,
			MaxProcessingMinutes: cleanMeta.RemoteNotesCleaningMaxProcessingDurationInMinutes,
		}
	}
	cleanProcessor := processors.NewCleanRemoteNotesProcessor(noteRepo, cleanCfgFn)
	cleanProcessor.SetCursorStore(processors.RedisCursorStore{
		Client: s.redis.Default,
		Key:    s.config.Redis.KeyPrefix() + processors.CleanRemoteNotesCursorKey,
	})
	s.queueServer.Handle(queue.TaskTypeCleanRemoteNotes, cleanProcessor.Handle)

	// リレー由来の孤児リモートユーザーの掃除 (#2340)。転送活動の LD-Signature
	// 検証は著者の公開鍵を DB に載せる必要があるため、その経路だけは揮発化
	// できない。後追いで回収する。対象は relay_observed_user に載っている行だけ。
	orphanUserProcessor := processors.NewOrphanUserCleanupProcessor(userRepo,
		func() processors.OrphanUserCleanerConfig {
			m, err := metaRepo.Fetch()
			if err != nil || m == nil {
				return processors.OrphanUserCleanerConfig{}
			}
			return processors.OrphanUserCleanerConfig{
				Enabled:   m.EnableRelayOrphanUserCleanup,
				GraceDays: m.RelayOrphanUserGraceDays,
			}
		})
	s.queueServer.Handle(queue.TaskTypeOrphanUserCleanup, orphanUserProcessor.Handle)

	// owner 無しのリモート添付の掃除 (#2722)。著者が materialize されていない
	// 添付は owner 無しで保存され (#2717)、ephemeral note が Redis の TTL で
	// 消えても drive_file の行は残り続ける。orphanUserCleanup が親 user を
	// 消して SET NULL になった行も同じ形。
	//
	// **gate は 2 つの meta の OR。** どちらの機能もこの形の行を作るので、
	// 片方だけ有効な構成で掃除が回らないと元の問題がそのまま残る。
	// 猶予と cutoff の組み立ては processor 側 (ID 生成方式は設定依存なので
	// generator をそのまま渡す)。
	//
	// cutoff 用の generator は**専用に作る**。`ulid` の monotonic reader は
	// 過去時刻で呼ぶと内部の基準時刻を巻き戻すので、採番と共有すると直後の
	// 単調性が 1 回崩れる。未知の方式なら主 idGen (:140) と同じく aidx へ
	// 倒す — **採番と cutoff で別の方式に分かれるほうが危険**で、その場合は
	// 「常に対象外」か「年齢を無視して対象」のどちらかに倒れる。
	cutoffGen, cutoffErr := id.NewGenerator(s.config.ID)
	if cutoffErr != nil {
		cutoffGen, _ = id.NewGenerator("aidx")
	}
	orphanAttachmentProcessor := processors.NewOrphanAttachmentCleanupProcessor(driveFileRepo, ephemeralStore,
		func() processors.OrphanAttachmentCleanerConfig {
			m, err := metaRepo.Fetch()
			if err != nil || m == nil {
				return processors.OrphanAttachmentCleanerConfig{}
			}
			return processors.OrphanAttachmentCleanerConfig{
				Enabled:      m.EnableEphemeralRelayNotes || m.EnableRelayOrphanUserCleanup,
				EphemeralTTL: time.Duration(m.EphemeralRelayNoteTTLMinutes) * time.Minute,
			}
		}, cutoffGen.Generate)
	s.queueServer.Handle(queue.TaskTypeOrphanAttachmentCleanup, orphanAttachmentProcessor.Handle)

	// Expired-mute prune (#1563 / #1603): scheduler の cron (*/5) が enqueue する。
	// user mute と channel mute の両方の期限切れ行を prune する。
	checkExpiredMutingsProcessor := processors.NewCheckExpiredMutingsProcessor(mutingRepo, channelMutingRepo)
	s.queueServer.Handle(queue.TaskTypeCheckExpiredMutings, checkExpiredMutingsProcessor.Handle)

	// Daily generic clean (#1563): scheduler の cron (0 0 * * *) が enqueue する。
	// user_ip 90 日 prune / 期限切れ role_assignment 削除 / reversi outdated game 削除 /
	// 未使用 antenna の deactivate (#1604, deactivateAntennaThreshold ミリ秒)。
	cleanGenericProcessor := processors.NewCleanProcessor(userIPRepo, roleAssignmentRepo, reversiRepo, idGen, antennaRepo, time.Duration(s.config.DeactivateAntennaThreshold)*time.Millisecond, repository.NewUserPendingRepository(s.db))
	// IP 照会の監査記録にも保持期間を掛ける (#3106)。**この行が無いと記録が永久に
	// 残り、`moderation_log` に IP を書くのと変わらなくなる。**
	cleanGenericProcessor.SetIPLookupLogPruner(repository.NewIPLookupLogRepository(s.db))
	s.queueServer.Handle(queue.TaskTypeClean, cleanGenericProcessor.Handle)

	// 分割アップロードセッションの GC (#2313): scheduler の cron (*/15) が
	// enqueue する。期限切れセッションの multipart upload を abort してから
	// 行を消す。日次 clean に相乗りさせないのは、未完了マルチパートアップロード
	// が課金対象で、TTL 60 分に対し 24 時間ぶん残るのを避けるため。
	chunkedUploadGCProcessor := processors.NewChunkedUploadGCProcessor(driveService)
	s.queueServer.Handle(queue.TaskTypeChunkedUploadGC, chunkedUploadGCProcessor.Handle)

	// object storage の実体削除 (#2325)。upstream Misskey と同じく専用 queue に
	// 逃がすことで、大量削除で API リクエストがブロックされるのと、storage 不調で
	// 実体が orphan になるのを防ぐ。cleanRemoteFiles は job 1 本の中でバッチ削除を
	// 回す (件数ぶん job を積むと Redis を圧迫するため)。
	objectStorageProcessor := processors.NewObjectStorageProcessor(driveStorage, localDriveStorage, driveFileRepo)
	s.queueServer.Handle(queue.TaskTypeObjectStorageDeleteFile, objectStorageProcessor.HandleDeleteFile)
	s.queueServer.Handle(queue.TaskTypeCleanRemoteFiles, objectStorageProcessor.HandleCleanRemoteFiles)

	// Reaction flush (issue #57): buffered writer 使用時は 30 秒ごとに flush。
	flushProcessor := processors.NewReactionFlushProcessor(reactionCountWriter)
	s.queueServer.Handle(queue.TaskTypeReactionFlush, flushProcessor.Handle)

	// Account cascade deletion (issue #187): admin/accounts/delete の後続
	// バックグラウンド処理。note / drive_file / following 関連行を掃除する。
	deleteAccountProcessor := processors.NewDeleteAccountProcessor(noteRepo, driveFileRepo, followingRepo)
	// #2230: local user の物理削除に userRepo を配線。未配線だと soft 削除止まりで行が残る。
	deleteAccountProcessor.SetUserRepo(userRepo)
	s.queueServer.Handle(queue.TaskTypeDeleteAccount, deleteAccountProcessor.Handle)

	// Per-pair Unfollow job (#587): admin/federation/remove-all-following
	// が enqueue するペアごとに Service.Unfollow を呼んで row 削除 + Reject
	// 配送を行う。Misskey TS の relationshipQueue 'unfollow' job 相当。
	unfollowProcessor := processors.NewUnfollowProcessor(followingService)
	s.queueServer.Handle(queue.TaskTypeUnfollow, unfollowProcessor.Handle)

	// Per-pair Follow / Block / Unblock job (#1605): 本家
	// RelationshipProcessorService の processFollow / processBlock /
	// processUnblock 相当。大量関係操作 (将来の AccountMove フォロワー移行
	// 等) の非同期バックプレッシャ surface。import は完了通知の
	// Applied/Skipped 集計を保つため同期適用を維持する (core/transfer)。
	followProcessor := processors.NewFollowProcessor(followingService)
	s.queueServer.Handle(queue.TaskTypeFollow, followProcessor.Handle)
	blockProcessor := processors.NewBlockProcessor(blockingService)
	s.queueServer.Handle(queue.TaskTypeBlock, blockProcessor.Handle)
	unblockProcessor := processors.NewUnblockProcessor(blockingService)
	s.queueServer.Handle(queue.TaskTypeUnblock, unblockProcessor.Handle)
	// buffering 有効起動時のみ 30s ごとに buffered reaction count を DB へ drain
	// する。30s ticker は upstream の日次 cron より高頻度で反映遅延を抑える
	// mk-go の運用。writer 型は起動時固定なので、実行時 OFF にしても ticker が
	// buffered writer を drain し続け滞留しない (#1780)。
	//
	// **queue role でだけ回す** (#2459)。`EnqueueReactionFlush` の
	// `WithUnique` TTL は 25s で ticker 間隔 30s より短いため、複数ノードが
	// 別タイミングで打つと重複排除をすり抜けて二重に積まれる。
	if s.role.RunsQueue() {
		if bufMeta, err := metaRepo.Fetch(); err == nil && bufMeta.EnableReactionsBuffering {
			s.startConstructionWorker(func(ctx context.Context) {
				s.reactionFlushWorker(ctx, s.queueClient.EnqueueReactionFlush)
			})
		}
	}

	// Charts (Phase 4 Step Q / 4.7) — 12 chart engines + management
	// service + hook adapter. The hooks must be wired before any
	// service that consumes them is invoked, so we construct the
	// bundle here, before the handler-binding section below.
	chartCharts := buildChartBundle(s.db)
	chartMgmt := chart.NewManagementService([]*chart.Chart{
		chartCharts.Notes,
		chartCharts.Users,
		chartCharts.Drive,
		chartCharts.Federation,
		chartCharts.Instance,
		chartCharts.ApRequest,
		chartCharts.ActiveUsers,
		chartCharts.PerUserNotes,
		chartCharts.PerUserDrive,
		chartCharts.PerUserFollowing,
		chartCharts.PerUserPv,
		chartCharts.PerUserReaction,
	}, 0)
	// Save の集約エラーを slog に出す。既定は no-op なので、配線しないと
	// 「どの group が書けなかったか」が本番でどこにも残らない (#2651)。
	//
	// **`slog.Warn` ではなく、いま解決した logger の Warn を渡す** (#2872)。
	// パッケージ関数を渡すと、ticker goroutine が実際に書く時点の
	// `slog.Default()` に出る。Chart 側と揃えて、配線した時点のものに固定する。
	chartMgmt.SetLogger(slog.Default().Warn)
	s.setChartManagement(chartMgmt)
	chartHooks := charthook.New(charthook.Config{
		Notes:            chartCharts.Notes,
		Users:            chartCharts.Users,
		Drive:            chartCharts.Drive,
		Federation:       chartCharts.Federation,
		Instance:         chartCharts.Instance,
		ApRequest:        chartCharts.ApRequest,
		ActiveUsers:      chartCharts.ActiveUsers,
		PerUserNotes:     chartCharts.PerUserNotes,
		PerUserDrive:     chartCharts.PerUserDrive,
		PerUserFollowing: chartCharts.PerUserFollowing,
		PerUserPv:        chartCharts.PerUserPv,
		PerUserReaction:  chartCharts.PerUserReaction,
		IDGen:            idGen,
	})
	// meta フラグでチャート生成を制御する。
	if m, err := metaRepo.Fetch(); err == nil {
		chartHooks.ChartsForRemoteUser = m.EnableChartsForRemoteUser
		chartHooks.ChartsForFederatedInst = m.EnableChartsForFederatedInstances
	}
	// 各サービスへ chart hook を注入する。Set* は nil 安全なので順序は不問。
	noteCreateService.SetChartHook(chartHooks)
	noteDeleteService.SetChartHook(chartHooks)
	followingService.SetChartHook(chartHooks)
	reactionService.SetChartHook(chartHooks)
	driveService.SetChartHook(chartHooks)
	federationResolver.SetChartHook(chartHooks)
	deliverProcessor.SetChartHook(chartHooks)
	inboxProcessor.SetChartHook(chartHooks)
	// 配送先ホストごとの健全性テレメトリ (#2461)。upstream は結果を
	// `isNotResponding` の真偽値にしか残さないので、mk-go 独自の観測。
	//
	// **queue role でだけ回す** (#2459)。記録するのは配送を実行するノードだけで、
	// Web ノードは admin endpoint から Redis を読むだけ。集計 goroutine を
	// Web ノードで回しても drain するものが無い。
	deliveryHealth := deliveryhealth.NewService(
		deliveryhealth.NewStore(s.redis.Default), deliveryhealth.DefaultMaxHosts, 0)
	// 受信側 (#2471)。送信側と同じ基盤で、Redis のキー空間だけを分ける。
	// 混ざると「配送できない host」と「受信を拒否した host」が同じ行に
	// 足し込まれ、どちらの話か分からなくなる。
	inboxHealth := deliveryhealth.NewService(
		deliveryhealth.NewStoreForDirection(s.redis.Default, deliveryhealth.DirectionInbound),
		deliveryhealth.DefaultMaxHosts, 0)
	if s.role.RunsQueue() {
		deliverProcessor.SetDeliveryTelemetry(deliveryHealth)
		deliveryHealth.Start(context.Background())
		s.registerShutdownHook(func(ctx context.Context) { deliveryHealth.Stop(ctx) })
		inboxProcessor.SetDeliveryTelemetry(inboxHealth)
		inboxHealth.Start(context.Background())
		s.registerShutdownHook(func(ctx context.Context) { inboxHealth.Stop(ctx) })
	}
	// federation 経由のリモートノート (Create / Announce) も PerUserNotesChart
	// 等の note 系 chart に +1 を記録するために配線する (#1156)。これが無いと
	// リモートユーザーのプロフィール「アクティビティ」タブの heatmap が空に
	// なり、削除 (-1) だけが note_delete_service 経由で記録されてマイナス側に
	// しか動かなくなる drop-in regression が発生する。
	federationProcessor.SetNoteChartHook(chartHooks)

	// Hashtag service: ノート作成 (local / federation 両経路) で hashtag table の
	// mentionedUsersCount / mentionedUserIds を更新する (#680)。/api/hashtags/list
	// 等の trends ranking はこの集計を見るので、未配線だと空集合のままになる。
	hashtagService := corehashtag.NewService(hashtagRepo, idGen)
	// meta.hiddenTags / meta.sensitiveWords を見て tag の record / featuring
	// を skip するため metaRepo を inject (upstream HashtagService.updateHashtags
	// Ranking と同 semantics、未配線だと sensitive tag が featured に出る
	// drop-in regression が再発するので必ず wire する)。
	hashtagService.SetMetaRepo(metaRepo)
	noteCreateService.SetHashtagHook(hashtagService)
	federationResolver.SetHashtagHook(hashtagService)
	// i/update の profile description 由来 user.tags 変更で hashtag attach 集計
	// (attachedUsersCount) を追従させる (#1362)。
	userService.SetUsertagHook(hashtagService)
	// graceful shutdown 経路で in-flight worker (#719 fire-and-forget) を ctx
	// 期限内に drain する (#727)。typical case では即返り、長時間動く worker は
	// ctx timeout で諦める (idempotent な RecordMention なので次回再カウント)。
	// #764: 旧版は Server.hashtagService field 経由で Shutdown() から呼ばれて
	// いたが、ctx-aware hook 化で field 不要になった。
	s.registerShutdownHook(func(ctx context.Context) {
		if err := hashtagService.Shutdown(ctx); err != nil {
			slog.Warn("hashtag service shutdown timed out", "err", err)
		}
	})

	// Chart cron processor: tickCharts (毎時) / resyncCharts (毎日) /
	// cleanCharts (毎日) を queue.Scheduler 経由で受け取る。Scheduler
	// 自体の cron 登録は server.Start() で行う。
	chartProcessor := processors.NewChartProcessor([]*chart.Chart{
		chartCharts.Notes,
		chartCharts.Users,
		chartCharts.Drive,
		chartCharts.Federation,
		chartCharts.Instance,
		chartCharts.ApRequest,
		chartCharts.ActiveUsers,
		chartCharts.PerUserNotes,
		chartCharts.PerUserDrive,
		chartCharts.PerUserFollowing,
		chartCharts.PerUserPv,
		chartCharts.PerUserReaction,
	})
	s.queueServer.Handle(queue.TaskTypeChartTick, chartProcessor.HandleTick)
	s.queueServer.Handle(queue.TaskTypeChartResync, chartProcessor.HandleResync)
	s.queueServer.Handle(queue.TaskTypeChartClean, chartProcessor.HandleClean)

	// Periodic remote-instance metadata refresh (#393)。
	// `metadataFetcher` は instance service 初期化時に作成済の
	// coreinstance.FetchMetadataService を流用する。
	instanceRefreshProc := processors.NewInstanceRefreshProcessor(instanceRepo, metadataFetcher, processors.InstanceRefreshConfig{})
	s.queueServer.Handle(queue.TaskTypeInstanceRefresh, instanceRefreshProc.Handle)

	// Daily retention aggregation (#421)。
	// 本家 AggregateRetentionProcessorService と同等で、retention_aggregation
	// テーブルに 1 日 1 行 cohort を追加し、過去 31 日分の data フィールドを
	// 更新する。/api/retention と admin overview の「定着率」heatmap が読む。
	retentionSvc := coreretention.NewService(userRepo, retentionRepo, idGen)
	retentionProc := processors.NewRetentionAggregateProcessor(retentionSvc)
	s.queueServer.Handle(queue.TaskTypeRetentionAggregate, retentionProc.Handle)

	// 起動時にも 1 回 aggregation を発火する。cron (0 0 * * *) を待つと
	// 新規デプロイ後最大 24h は heatmap が空のままになるので、その日の
	// cohort 行を即座に作って描画を始められるようにする (#421)。同じ
	// dateKey で 2 回目を Insert しても repository.ErrDuplicateKey で
	// silently 吸収されるので idempotent。DB 書き込みを startup hot path に
	// 乗せないよう goroutine で実行する。
	//
	// cron の担当と揃えて **queue role でだけ**発火する (#2459)。idempotent
	// なので複数ノードで走っても壊れないが、Web ノードを増やすたびに起動時の
	// DB 書き込みが増えるのは無駄。
	if s.role.RunsQueue() {
		go func() {
			if err := retentionSvc.Aggregate(context.Background()); err != nil {
				slog.Warn("retention aggregate at startup failed", "err", err)
			}
		}()
	}

	// Health check
	s.echo.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// Prometheus metrics: enableMetrics=true のときだけ公開する。
	// 認証は付かない (Prometheus 慣例) ので、operator は nginx / LB ACL で
	// access 制限する想定。詳細は docs/design/auto-scale-job-workers.md §6.1。
	if s.config.EnableMetrics {
		if err := wireMetricsEndpoint(s.echo, s.queueMetrics); err != nil {
			slog.Error("server: failed to register queue metrics", "err", err)
		}
	}

	// pprof: enablePprof=true のときだけ公開する。
	// ランタイムプロファイリング用。本番では絶対に有効化してはならない。
	if s.config.EnablePprof {
		pprofGroup := s.echo.Group("/debug/pprof")
		pprofGroup.GET("", func(c echo.Context) error {
			return c.Redirect(http.StatusMovedPermanently, "/debug/pprof/")
		})
		pprofGroup.GET("/", echo.WrapHandler(http.HandlerFunc(pprof.Index)))
		pprofGroup.GET("/cmdline", echo.WrapHandler(http.HandlerFunc(pprof.Cmdline)))
		pprofGroup.GET("/profile", echo.WrapHandler(http.HandlerFunc(pprof.Profile)))
		pprofGroup.GET("/symbol", echo.WrapHandler(http.HandlerFunc(pprof.Symbol)))
		pprofGroup.POST("/symbol", echo.WrapHandler(http.HandlerFunc(pprof.Symbol)))
		pprofGroup.GET("/trace", echo.WrapHandler(http.HandlerFunc(pprof.Trace)))
		pprofGroup.GET("/:name", func(c echo.Context) error {
			pprof.Handler(c.Param("name")).ServeHTTP(c.Response(), c.Request())
			return nil
		})
	}

	api := s.echo.Group("/api")

	// 本家 ApiCallService #sendApiError 互換の WWW-Authenticate 付与 (#1608)。
	// error envelope の kind を見て header を決めるため、応答を最初に
	// 仲介できる位置 (= group 最初の Use) に置く。後続 middleware / handler
	// が書く error 応答 (rate limit 429 / FST_ERR_CTP_* を含む) も全て
	// この writer を通る。

	// #2106 L56: upstream ApiServerService は /api scope の全応答に既定の
	// Cache-Control: private, max-age=0, must-revalidate を付与する。cacheSec endpoint は
	// handler 内で public, max-age=... に上書きする (handler の Set が後勝ち)。非 cacheSec の
	// allowGet GET を中間 proxy が誤キャッシュするのを防ぐ。
	api.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
			return next(c)
		}
	})

	// 本家 (Fastify) は JSON 応答の Content-Type を
	// `application/json; charset=utf-8` で返すが、Echo の c.JSON は charset を
	// 付けない。本家の e2e ヘルパ (test/utils.ts の api()) は Content-Type の
	// 完全一致で body を採るか捨てるかを決めており、この差だけで全レスポンスが
	// 空として扱われる。drop-in 互換のため本家に合わせる。
	api.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Before(func() {
				if c.Response().Header().Get(echo.HeaderContentType) == "application/json" {
					c.Response().Header().Set(echo.HeaderContentType, "application/json; charset=utf-8")
				}
			})
			return next(c)
		}
	})

	// Fastify互換のJSON body事前パース (#1609)。本家はbody parseが
	// endpoint handler (rate limit / 認証を含む) より先に走るため、
	// rate limiterより前に登録して malformed JSON を FST_ERR_CTP_* envelope
	// で先に弾く。
	api.Use(middleware.JSONBodyParse())

	// Rate limiter: Redisバックエンドのsliding window、Misskey TS互換。
	// auth.Authenticate()がグローバルミドルウェアとして先に実行されるため、
	// GetUser(c)で認証済みユーザーを取得できる。
	//
	// disableEndpointRateLimits=true のとき per-endpoint table を nil に
	// 落として全 endpoint で 429 を出さない (#560 / Misskey TS の
	// `NODE_ENV=development` 相当)。production では絶対に有効にしない。
	// 起動時の警告は config.resolve() 側で TestMode / EnablePprof と
	// 揃えて出している。
	rateLimitDefs := middleware.DefaultEndpointLimits
	if s.config.DisableEndpointRateLimits {
		rateLimitDefs = nil
	}
	rateLimiter := middleware.NewRedisRateLimiter(
		s.redis.Default,
		s.config.EnableIPRateLimit,
		rateLimitDefs,
	)
	// role policies の rateLimitFactor を反映できるよう roleService を inject
	// (#606 item 4)。trusted user に factor=2.0 等を割り当てて実効 Max を緩和
	// する運用に対応。
	rateLimiter.SetPolicyProvider(roleService)
	// 本家 RateLimiterService は NODE_ENV !== 'production' でレート制限を丸ごと
	// 無効化する。mk-go に NODE_ENV は無いので、本番では必ず false になる
	// TestMode を同じ用途で使う。有効なままだと本家 e2e の beforeAll (大量の
	// signup / 投稿) が 429 で落ちる。
	if s.config.TestMode {
		rateLimiter.Disable()
		slog.Warn("test mode: rate limiting is disabled")
	}
	api.Use(rateLimiter.Middleware())
	// 認証済みリクエストの IP を記録する (#3103)。**group の Use なので
	// `auth.Authenticate()` (global) より後に走る** — 逆だと UserContextKey が
	// まだ積まれておらず、常に何もしない middleware になる。
	api.Use(middleware.RecordClientIP(ipLogService))

	// Meta endpoint (public)
	metaHandler := meta.NewHandler(s.config, metaRepo)
	// **`requireSetup` をサーバー側の受け入れ条件と揃える (#3037)。**
	// `admin/accounts/create` の初回セットアップ窓は「`rootUserId` 未設定
	// **かつ**ローカル利用者 0」でしか開かないので、フロントの判定も同じに
	// する。揃えないと、`rootUserId` が NULL で利用者ありの DB で
	// **セットアップ画面が出続けて作成ボタンが必ず失敗する**。
	meta.SetLocalUserCounter(func() (int64, error) { return userRepo.CountLocalUsers() })
	metaHandler.SetAdRepo(repository.NewAdRepository(s.db))
	proxyAccountResolver := newProxyAccountResolver(repository.NewSystemAccountRepository(s.db), userRepo)
	metaHandler.SetProxyAccountResolver(proxyAccountResolver)
	// 分割アップロード (#2313) の能力告知。未対応構成では field ごと出さないので、
	// フロントエンドは undefined を見て従来の単発アップロードに倒れる。
	chunkedUploadCapability := func() (int64, bool) {
		settings, ok := driveService.ChunkedUploadCapability()
		return settings.ChunkSize, ok
	}
	metaHandler.SetChunkedUploadCapability(chunkedUploadCapability)
	api.POST("/meta", metaHandler.Meta)
	api.POST("/ping", metaHandler.Ping)

	// Frontend SPA shell — AP resource handlers fall back to this when the
	// request prefers HTML over application/activity+json, and the final
	// catch-all route at the bottom of RegisterRoutes uses the same handler.
	frontend := frontendHTML(s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability)

	// URL preview endpoint
	if previewMeta, err := metaRepo.Fetch(); err == nil {
		previewCfg := coreurlpreview.Config{
			Enabled:              previewMeta.URLPreviewEnabled,
			AllowRedirect:        previewMeta.URLPreviewAllowRedirect,
			TimeoutMs:            previewMeta.URLPreviewTimeout,
			MaxContentLength:     previewMeta.URLPreviewMaximumContentLength,
			RequireContentLength: previewMeta.URLPreviewRequireContentLength,
		}
		if previewMeta.URLPreviewUserAgent != nil {
			previewCfg.UserAgent = *previewMeta.URLPreviewUserAgent
		}
		if previewMeta.URLPreviewSummaryProxyURL != nil {
			previewCfg.SummaryProxyURL = *previewMeta.URLPreviewSummaryProxyURL
		}
		urlPreviewFetcher := coreurlpreview.NewFetcher(previewCfg, s.redis.Default, s.config.Redis.KeyPrefix(), s.config.AllowedPrivateNetworks, s.outboundOpts()...)
		// urlPreviewSensitiveList は admin が随時変更するため live provider で
		// 渡す (metaRepo は cached、update-meta で invalidate される)。
		urlPreviewFetcher.SetSensitiveListProvider(func() []string {
			m, err := metaRepo.Fetch()
			if err != nil || m == nil {
				return nil
			}
			return m.URLPreviewSensitiveList
		})
		urlHandler := apiurl.NewHandler(urlPreviewFetcher)
		s.echo.GET("/url", urlHandler.Preview)
	}

	// Test-only endpoints — TestMode=true のときだけ公開する。
	// Cypress の resetState コマンドが依存する /api/reset-db はここで登録する。
	// 本番で絶対に有効化してはならない (config.go 側で起動時 warning を出す)。
	if s.config.TestMode {
		testHandler := apitest.NewHandler(s.db, s.redis.Default, metaRepo, true)
		api.POST("/reset-db", testHandler.ResetDB)
		slog.Warn("test mode: /api/reset-db endpoint is registered", "url", s.config.URL)
	}

	// Stats endpoint (public) — チャートの集計済み値から取得
	notesChart := chartCharts.Notes
	usersChart := chartCharts.Users
	statsHandler := stats.NewHandler(s.db, notesChart, usersChart)
	api.POST("/stats", statsHandler.Stats)

	// listRelationRepos は embed user に viewer 視点の relation block を付与する
	// 共有 Repos。mute/renote-mute/following/federation/hashtags handler が
	// 共有する (#1957-a)。Apply は匿名 / self / 未配線で no-op。
	//
	// **`/users` / `/pinned-users` はこれを使わない** (#2791 で移設した先の
	// `usersHandler.viewerRelationRepos()` が同じ 6 つを組み立てる)。
	// `userrelation.Repos` にフィールドを足すときは**両方に配線が要る** —
	// 片方だけ更新すると explore と pinned-users から黙って欠ける。
	listRelationRepos := userrelation.Repos{
		Following:     followingRepo,
		Blocking:      blockingRepo,
		Muting:        mutingRepo,
		RenoteMuting:  renoteMutingRepo,
		FollowRequest: followRequestRepo,
		Memo:          repository.NewUserMemoRepository(s.db),
	}

	// CAPTCHA service — meta から有効な provider を選択して構築する。
	// meta 取得失敗時は captcha 無効として動作する (ログイン不能を避けるため)。
	// siteverify は SSRF-safe transport + forward proxy 経由にする (#638)。
	//
	// **起動時に meta を読めなくても service は作る (#3037)。** 以前は nil の
	// ままにしていたので、`SetCaptcha` も呼ばれず `reloadCaptcha` も
	// `captchaSvc == nil` で即 return し、**その後 meta が読めるようになっても
	// captcha が永久に無効**だった。しかも `/api/meta` は DB を読むので
	// フロントはウィジェットを描画し、運営者には ON に見える — この配線が
	// 塞ごうとしている状態そのもの。空の meta で作っておけば、次の
	// `metaUpdated` で provider が入る。
	captchaMeta, captchaMetaErr := metaRepo.Fetch()
	if captchaMetaErr != nil || captchaMeta == nil {
		slog.Warn("captcha: meta unavailable at startup; starting disabled and waiting for metaUpdated",
			"err", captchaMetaErr)
		captchaMeta = &model.Meta{}
	}
	captchaSvc := corecaptcha.NewServiceWithClient(captchaMeta, s.outboundClient(10*time.Second))

	// Signup (public)
	userPendingRepo := repository.NewUserPendingRepository(s.db)
	signupService.SetUserPendingRepo(userPendingRepo)
	// 承認制の登録を確認メールに乗せた経路 (#2571) で、申請の確定をアカウント
	// 作成と同じ tx に入れる (#2576)。
	signupService.SetSignupApplicationRepo(repository.NewSignupApplicationRepository(s.db))
	// PromotePending を transaction 化して partial failure rollback と
	// invitation ticket の SELECT FOR UPDATE ロックを有効化する (#600 item 2 + #604)。
	signupService.SetDB(s.db)
	signupTicketRepo := repository.NewRegistrationTicketRepository(s.db)
	signupService.SetTicketRepo(signupTicketRepo)
	signupHandler := apisignup.NewHandler(signupService, metaRepo, idGen)
	// #2673: signup も upstream と同じく実効 policy を返す (meta の base override /
	// role 集約 / server cap 込み)。素の default を返すと /api/i と食い違う。
	signupHandler.SetUserPolicyResolver(roleService)
	if captchaSvc != nil {
		signupHandler.SetCaptcha(captchaSvc)
	}
	// verifymail / truemail SaaS API への outbound を SSRF-safe transport +
	// forward proxy 経由にする (#638)。
	signupHandler.SetEmailValidationClient(s.outboundClient(10 * time.Second))
	// signupTicketRepo は repository.RegistrationTicketRepository で、
	// apisignup.TicketStore (FindByCode + MarkUsed) を superset として満たす。
	// 旧 gormTicketStore wrapper を直接 repo に置き換え (#610 item 1)。
	// 承認制の登録 (#2554 / #2555)。承認待ちを user 行として持たず、専用
	// テーブルに閉じ込める。TS へ切り替えても承認待ちが有効化されない。
	// signup (申請・登録) と admin (審査) の両方が同じインスタンスを見る。
	signupApplicationService := signupapplication.NewService(
		repository.NewSignupApplicationRepository(s.db), idGen)
	signupHandler.SetTicketStore(signupTicketRepo)
	// 承認制の登録 (#2554 / #2569)。本人性はクレームコードが担保するので、
	// 外部サーバーには一切依存しない。
	signupHandler.SetSignupApplications(signupApplicationService)
	// 申請が出されたことをモデレーターへ知らせる (#2987)。**審査 endpoint が
	// RequireModerator なので宛先は GetModerators と一致する。**
	signupApplicationService.SetReceivedNotifier(
		signupapplication.NewReceivedNotifier(notificationService, roleService))
	signupHandler.SetTestMode(s.config.TestMode)
	// emailRequiredForSignup フローの確認メール送信。常に sender を配線し、
	// closure 内で毎回 meta を読み直すことで admin UI の SMTP 設定変更が
	// 再起動なしに反映される (#1112)。smtpSecure も meta 経由で反映 (#1111)。
	signupHandler.SetEmailSender(s.config.URL, miscsmtp.SenderFromMeta(metaRepo, s.config.ProxySMTP))
	// captcha の実 provider が 1 つも無いときに申請 endpoint を守る署名付き
	// フォームトークン (#2806)。**captcha の代替ではない** — 位置づけは
	// core/signupform の doc を見ること。
	//
	// 鍵は instance_secret から取る (config 項目を増やさず、再起動とワーカー
	// 跨ぎで一貫させる)。**取れなかったら配線しない** — 弱い鍵に fallback する
	// と署名が意味を失うので、未配線として起動時に検出させるほうがよい。
	//
	// **nonce store は具象型のまま nil を見る。** interface に入れてから判定すると
	// typed-nil で必ず非 nil になり、「未配線」を検出できない。
	if nonces := signupform.NewRedisNonceStore(s.redis.Default); nonces == nil {
		slog.Error("signup form token: redis unavailable, form tokens disabled")
	} else if secret, err := repository.NewInstanceSecretRepository(s.db).
		GetOrCreate(repository.SignupFormTokenKey); err != nil {
		slog.Error("signup form token: secret unavailable, form tokens disabled", "err", err)
	} else {
		signupHandler.SetFormTokenIssuer(signupform.NewIssuer(secret, nonces))
	}
	api.POST("/signup", signupHandler.Signup)
	// 承認制の登録 (#2554 / #2556)。認証は不要 — 本人確認は MiAuth が担う。
	// 有効になっていない構成では 503 を返す。
	api.POST("/signup-application/form-token", signupHandler.ApplicationFormToken)
	api.POST("/signup-application/apply", signupHandler.ApplicationApply)
	api.POST("/signup-application/status", signupHandler.ApplicationStatus)
	api.POST("/signup-application/register", signupHandler.ApplicationRegister)
	api.POST("/signup-pending", signupHandler.SignupPending)

	// Username availability check (フロントエンドの signup フォームが呼ぶ)
	//
	// upstream username/available.ts は available = (既存 user 無し) && (used_usernames
	// 無し) && (preservedUsernames 非該当) の3条件で判定し、paramDef の
	// localUsernameSchema で format 検証する (#1551)。format 不正は paramDef レベルで
	// 弾かれるため INVALID_PARAM を返す。
	usedUsernameRepo := repository.NewUsedUsernameRepository(s.db)
	signupService.SetUsedUsernameRepo(usedUsernameRepo) // #2080: 削除済 username 再利用を弾く
	signupHandler.SetUsernameLookups(userRepo, usedUsernameRepo)
	api.POST("/username/available", signupHandler.UsernameAvailable)

	// Signin (Phase 6)
	signinHandler := apisignin.NewHandler(userRepo)
	if captchaSvc != nil {
		signinHandler.SetCaptcha(captchaSvc)
	}
	// **signin 側にも testMode が要る (#3037 レビュー 2 周目)。** upstream
	// `SigninApiService.ts:184` は signin の captcha 検証も
	// `process.env.NODE_ENV !== 'test'` で囲っている。signup 側にだけ
	// 配線されていたので、captcha を有効にした e2e 構成では signup だけ
	// 通って signin が落ちる。
	signinHandler.SetTestMode(s.config.TestMode)
	// IP logging (#3103)。**起動時に meta を読んで配線するかどうかを決めない** —
	// その形だと管理画面で有効にしても再起動するまで記録が始まらなかった (#3107)。
	// 記録するかどうかは iplog.Service が呼び出しのたびに meta から判断する。
	signinHandler.SetIPRecorder(ipLogService)
	// signin履歴レコード注入
	signinRepo := repository.NewSigninRepository(s.db)
	signinHandler.SetSigninRepo(signinRepo, idGen)
	signinHandler.SetLoginNotifier(notificationHook)
	// new-login 通知メール (#2454)。SenderFromMeta が per-call で meta を読み直すので
	// admin UI の SMTP 設定変更が再起動なしで効く。SMTP 未設定なら no-op。
	signinHandler.SetEmailSender(s.config.URL, miscsmtp.SenderFromMeta(metaRepo, s.config.ProxySMTP))
	signinHandler.SetMetaRepo(metaRepo)
	// アカウント作成時も signin 副作用 (履歴 / login 通知 / main publish) を通す (#1804)。
	signupHandler.SetSigninRecorder(signinHandler)
	api.POST("/signin", signinHandler.Signin)
	api.POST("/signin-flow", signinHandler.SigninFlow)
	api.POST("/signin-with-passkey", signinHandler.SigninWithPasskey)

	// パスワードリセット (認証不要)
	resetReqRepo := repository.NewPasswordResetRequestRepository(s.db)
	resetHandler := apiresetpassword.NewHandler(userRepo, resetReqRepo, idGen)
	resetHandler.SetServerURL(s.config.URL)
	// password reset の確認メール送信。配線パターンは signup と同じく
	// SenderFromMeta で per-call 再 Fetch、runtime 設定変更追従 (#1112)。
	resetHandler.SetMetaRepo(metaRepo)
	resetHandler.SetEmailSender(miscsmtp.SenderFromMeta(metaRepo, s.config.ProxySMTP))
	api.POST("/request-reset-password", resetHandler.RequestReset)
	api.POST("/reset-password", resetHandler.Reset)

	// WebAuthn / 2FA (issue #42)
	// userSecurityKeyRepo + WebAuthnService を構築して signin と /api/i に注入する。
	// Redis セッションは redis.default を流用する (用途別分離は不要)。
	userSecurityKeyRepo := repository.NewUserSecurityKeyRepository(s.db)
	webauthnSvc, webauthnErr := coretwofactor.NewWebAuthnService(s.config.URL, "Misskey", s.redis.Default)
	if webauthnErr != nil {
		slog.Warn("webauthn service unavailable", "err", webauthnErr)
	} else {
		signinHandler.SetWebAuthn(webauthnSvc, userSecurityKeyRepo)
	}

	// TOTP replay guard — accept した TOTP コードを Redis に SETNX で
	// 記録し、acceptance window 内 (~90s) の再利用を refuse する
	// (RFC 6238 §5.2)。mk-go が先行実装した hardening だが、upstream も
	// 2026.6.0 の UserAuthService.validateOtp で同等機構 (Redis SET NX EX、
	// TTL 90s) を入れたため現在は挙動が揃っている。
	// signin と /api/i の 2 経路で共通の guard を共有する。
	totpReplayGuard := coretwofactor.NewRedisReplayGuard(s.redis.Default)
	signinHandler.SetTOTPReplayGuard(totpReplayGuard)

	// Emojis endpoints (public)
	emojisHandler := apiemojis.NewHandler(emojiRepo)
	emojisHandler.SetExportEnqueuer(s.queueClient)
	api.POST("/emojis", emojisHandler.Emojis)
	api.GET("/emojis", emojisHandler.Emojis)
	api.POST("/emoji", emojisHandler.Emoji)
	api.GET("/emoji", emojisHandler.Emoji)

	// 7+ handlers across api/* pack notes via entity.PackNotes. Files /
	// MyReaction / Channel の post-pack 解決は notes 以外でも必要 (#426)。
	// 共通 resolver を 1 つだけ作って全 handler に注入する。
	noteFieldResolver := entity.NewNoteFieldResolver(driveFileRepo, driveFolderRepo, userRepo, reactionRepo, channelRepo, idGen)
	// viewer の poll vote から Poll.choices[i].IsVoted を埋める (#690)。
	// frontend の MkPoll.vue が `showResult = isVoted || closed` を初期化に
	// 使うため、これがないと「結果を見る」toggle が reload で揮発する。
	noteFieldResolver.SetPollVoteLookup(pollVoteRepo)

	// Notes endpoints
	notesHandler := notes.NewHandler(noteRepo, noteCreateService, noteDeleteService, noteQueryService, timelineService, reactionService, pollService, searchService, idGen)
	// first-page timeline 応答を per-viewer 短期キャッシュ (hit 時に DB + pack +
	// encode を skip)。opt-in (enableTimelineCache / MK_ENABLETIMELINECACHE)。
	// staleness trade-off があるため default off。
	if s.config.EnableTimelineCache {
		ttl := time.Duration(s.config.TimelineCacheTTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = 3 * time.Second
		}
		notesHandler.EnableTimelineJSONCache(ttl)
		slog.Info("timeline JSON cache enabled", "ttlSeconds", int(ttl.Seconds()))
	}
	notesHandler.SetDriveFileRepo(driveFileRepo)
	notesHandler.SetPollRepo(pollRepo)                     // #1538: polls/recommendation
	notesHandler.SetThreadMutingRepo(noteThreadMutingRepo) // #1538: thread-muting write
	notesHandler.SetNoteReactionRepo(reactionRepo)
	notesHandler.SetChannelRepo(channelRepo)
	notesHandler.SetChannelMutingRepo(channelMutingRepo)
	// home timeline で follow 中 channel の note を含める (#1686)。
	notesHandler.SetChannelFollowingRepo(channelFollowingRepo)
	// HTL の「返信先が followers 限定の投稿」ガード (upstream timeline.ts の noteFilter)。
	notesHandler.SetUserFollowingRepo(followingRepo)
	// notes/featured の engagement ランキング読み取り (#1687)。
	notesHandler.SetFeaturedRanking(featuredService)
	// timeline endpoint で muted user の note を除外する filter (#874)。
	// 未配線だと user mute は read 時に効かず、cache のみ DB 整合の状態
	// になる security/UX regression が残る。production では必ず wire する。
	notesHandler.SetMutingRepo(mutingRepo)
	// timeline endpoint で renote-muted user の pure renote を除外する
	// filter (#903)。未配線だと renote-mute は read 時に効かず、frontend
	// で消えるべき renote が表示され続ける。production では必ず wire する。
	notesHandler.SetRenoteMutingRepo(renoteMutingRepo)
	notesHandler.SetBlockingRepo(blockingRepo) // #1554: children/replies/renotes mute/block filter
	notesHandler.SetInstanceRepo(instanceRepo)
	notesHandler.SetMetaRepo(metaRepo) // #1783: list endpoint の blocked-host filter
	notesHandler.SetEmojiRepo(emojiRepo)
	notesHandler.SetReactionReader(reactionCountWriter)
	notesHandler.SetDriveFolderRepo(driveFolderRepo)
	notesHandler.SetUserRepo(userRepo)
	notesHandler.SetModeratorChecker(roleService) // #1538: moderator note delete
	notesHandler.SetUserListRepo(userListRepo)
	notesHandler.SetClipRepos(clipRepo, clipNoteRepo, clipFavoriteRepo) // #1554: notes/clips

	notehide.SetFollowingRepo(followingRepo)
	// LocalTimeline / GlobalTimeline / HybridTimeline で ltlAvailable /
	// gtlAvailable role policy を gate するために配線 (#1026)。匿名 viewer に
	// 対しては GetUserPolicies("") が base policies を返すので、admin が
	// meta.policies で全 user に対して TL を無効化できる。
	notesHandler.SetPolicyProvider(roleService)
	// **無条件に配線する。** 以前は Fetch 成功時だけ設定していたので、起動時に
	// DB が一時的に応答しないと空文字のままになり、`ugcVisibilityForVisitor` の
	// gate が**丸ごと消えていた** (空文字は "none" でも "local" でもないので
	// 素通し)。列は NOT NULL DEFAULT 'local' なので、失敗時も制限側の既定へ
	// 倒す (#2708)。
	// **毎回読む。** 起動時に焼き込むと、運営者が管理画面で締めても
	// プロセスを再起動するまで API に反映されない。
	notesHandler.SetUGCVisibilityLookup(func() string { return metaUGCVisibility(metaRepo) })
	if m, err := metaRepo.Fetch(); err == nil {
		if m.DeeplAuthKey != nil && *m.DeeplAuthKey != "" {
			notesHandler.SetTranslator(coretranslate.NewDeepL(*m.DeeplAuthKey, m.DeeplIsPro, s.outboundClient(10*time.Second)))
		}
	}
	api.POST("/notes/create", notesHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:notes"))
	api.POST("/notes/show", notesHandler.Show)
	api.POST("/notes/delete", notesHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:notes"))
	api.POST("/notes/renotes", notesHandler.Renotes)
	api.POST("/notes/replies", notesHandler.Replies)
	api.POST("/notes/children", notesHandler.Children)
	api.POST("/notes/conversation", notesHandler.Conversation)
	// upstream search.ts は requireCredential:false で匿名も handler に到達させ、
	// canSearchNotes policy を handler 内で検査して UNAVAILABLE を返す (#1783)。
	// route 側の RequireAuth + RequireRolePolicy は外す。
	api.POST("/notes/search", notesHandler.Search)
	api.POST("/notes/state", notesHandler.State, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/timeline", notesHandler.Timeline, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/local-timeline", notesHandler.LocalTimeline)
	api.POST("/notes/global-timeline", notesHandler.GlobalTimeline)
	api.POST("/notes/hybrid-timeline", notesHandler.HybridTimeline, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/reactions", notesHandler.Reactions)
	api.GET("/notes/reactions", notesHandler.Reactions)
	api.POST("/notes/reactions/create", notesHandler.ReactionsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:reactions"))
	api.POST("/notes/reactions/delete", notesHandler.ReactionsDelete, middleware.RequireAuth(), middleware.RequireScope("write:reactions"))
	api.POST("/notes/polls/vote", notesHandler.PollsVote, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:votes"))
	// Notes extra endpoints (Phase 6)
	notesHandler.SetFavoriteRepo(noteFavoriteRepo)
	// #1762: local note を favorite されたとき著者に myNoteFavorited1 を付与する
	// server-side achievement infra。claim-achievement と同じ profile.achievements
	// を更新する。
	achievementService := coreachievement.NewService(userService, notificationService)
	notesHandler.SetAchievementGranter(achievementService)
	api.POST("/notes/favorites/create", notesHandler.FavoritesCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:favorites"))
	api.POST("/notes/favorites/delete", notesHandler.FavoritesDelete, middleware.RequireAuth(), middleware.RequireScope("write:favorites"))
	api.POST("/notes/featured", notesHandler.Featured)
	api.GET("/notes/featured", notesHandler.Featured)
	api.POST("/notes/unrenote", notesHandler.Unrenote, middleware.RequireAuth(), middleware.RequireScope("write:notes"))
	api.POST("/notes/mentions", notesHandler.Mentions, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/user-list-timeline", notesHandler.UserListTimeline, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/notes/search-by-tag", notesHandler.SearchByTag)
	api.POST("/notes/clips", notesHandler.Clips)
	// canUseTranslator は upstream translate.ts 同様 handler 側で評価し、拒否時は
	// 503 UNAVAILABLE を返す (RequireRolePolicy の 403 ではない、#2010)。
	api.POST("/notes/translate", notesHandler.Translate,
		middleware.RequireAuth(),
		middleware.RequireScope("read:account"))
	api.POST("/notes/show-partial-bulk", notesHandler.ShowPartialBulk)
	// notes/drafts + notes/thread-muting + notes/polls/recommendation (実データ)
	noteDraftRepo := repository.NewNoteDraftRepository(s.db)
	notesHandler.SetDraftRepo(noteDraftRepo)
	notesHandler.SetScheduledNoteEnqueuer(s.queueClient)

	// PostScheduledNote processor (#1040): delayed queue 経由で draft を
	// note 化する。dependencies は既存 noteCreateService + userRepo + 上記
	// noteDraftRepo を共有する。
	postScheduledNoteProcessor := processors.NewPostScheduledNoteProcessor(noteDraftRepo, userRepo, noteCreateService)
	// idempotency lock を Redis SETNX で配線 (#1045 Phase 2-A)。queue の
	// at-least-once delivery で job が二重 fire しても重複 publish を防ぐ。
	postScheduledNoteProcessor.SetLock(processors.NewRedisScheduledNoteLock(
		s.redis.Default, s.config.Redis.KeyPrefix(), 0))
	// publish 結果通知 (#1045 Phase 2-B): scheduledNotePosted (= 成功) /
	// scheduledNotePostFailed (= 失敗) を user の通知 stream に発火する。
	postScheduledNoteProcessor.SetNotifier(notificationService)
	s.queueServer.Handle(queue.TaskTypePostScheduledNote, postScheduledNoteProcessor.Handle)
	api.POST("/notes/drafts/list", notesHandler.DraftsList, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("read:account"))
	api.POST("/notes/drafts/create", notesHandler.DraftsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/notes/drafts/update", notesHandler.DraftsUpdate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/notes/drafts/delete", notesHandler.DraftsDelete, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/notes/drafts/count", notesHandler.DraftsCount, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("read:account"))
	api.POST("/notes/thread-muting/create", notesHandler.ThreadMutingCreate, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/notes/thread-muting/delete", notesHandler.ThreadMutingDelete, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/notes/polls/recommendation", notesHandler.PollsRecommendation, middleware.RequireAuth(), middleware.RequireScope("read:account"))

	// Users endpoints
	usersHandler := users.NewHandler(userService, followingService, noteRepo, idGen)
	// /pinned-users は meta.pinnedUsers の acct を解決する (#2791 で router.go の
	// inline closure から移設)。
	usersHandler.SetMetaRepo(metaRepo)
	usersHandler.SetLocalHost(localHost)
	usersHandler.SetChartHook(chartHooks)
	// users/notes (withChannelNotes) の post-fetch filter でチャンネルミュートを効かせる。
	usersHandler.SetChannelMutingRepo(channelMutingRepo)
	// #2106 S3: 匿名 visitor への remote profile 露出を ugcVisibilityForVisitor で gate。
	// notes 側と同じ理由で無条件に配線する (#2708)。
	usersHandler.SetUGCVisibilityLookup(func() string { return metaUGCVisibility(metaRepo) })
	usersHandler.SetFeaturedRanking(featuredService) // #1687: users/featured-notes ランキング
	usersHandler.SetPiningRepo(piningRepo)
	usersHandler.SetFollowingRepo(followingRepo)
	usersHandler.SetBlockingRepo(blockingRepo)
	usersHandler.SetMutingRepo(mutingRepo)
	usersHandler.SetRolePolicyProvider(roleService) // #1550: create-from-public userListLimit/userEachUserListsLimit
	usersHandler.SetProxyFollow(listProxyFollow)    // #1704: create-from-public remote member proxy follow
	usersHandler.SetRenoteMutingRepo(renoteMutingRepo)
	usersHandler.SetFollowRequestRepo(followRequestRepo)
	usersHandler.SetInstanceRepo(instanceRepo)
	usersHandler.SetEmojiRepo(emojiRepo)
	usersHandler.SetReactionReader(reactionCountWriter)
	usersHandler.SetClipRepo(clipRepo)
	usersHandler.SetClipFavoriteRepo(clipFavoriteRepo) // #1562: users/clips の favoritedCount / isFavorited
	usersHandler.SetClipNoteRepo(clipNoteRepo)         // #2243: users/clips の notesCount 実カウント
	usersHandler.SetFlashRepo(flashRepo)
	usersHandler.SetGalleryRepo(repository.NewGalleryRepository(s.db))
	usersHandler.SetPageRepo(pageRepo)
	// users/pages の page も attachedFiles / eyeCatchingImage を解決する (#1662)。
	usersHandler.SetDriveFileRepo(driveFileRepo)
	usersHandler.SetNoteFieldResolver(noteFieldResolver)
	usersHandler.SetUserRepo(userRepo)
	usersHandler.SetNoteReactionRepo(reactionRepo)
	remoteStatsFetcher := corefederation.NewRemoteStatsFetcher(s.config.AllowedPrivateNetworks, s.config.UserAgent, s.outboundOpts()...)
	// **連合を切った相手へ取りに行かない。** この経路は未認証の
	// `/api/users/show` から呼ばれるので、放っておくと defederate した相手に
	// 「誰をいつ見たか」が漏れる。
	remoteStatsFetcher.SetHostAllowedChecker(instanceService.CanFetchOptionalRemoteData)
	usersHandler.SetRemoteStatsFetcher(&remoteStatsFetcherAdapter{
		fetcher: remoteStatsFetcher,
	})
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
	// upstream search-by-username-and-host.ts:13 は requireCredential:false かつ
	// requiredRolePolicy 無し (匿名・全 role に開放)。mk-go が独自に付けていた
	// RequireAuth + canSearchUsers gate を外して揃える (#1766)。
	api.POST("/users/search-by-username-and-host", usersHandler.SearchByUsernameAndHost)
	api.POST("/users/update-memo", usersHandler.UpdateMemo, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	usersHandler.SetAbuseRepo(repository.NewAbuseReportRepository(s.db))
	usersHandler.SetMemoRepo(repository.NewUserMemoRepository(s.db))
	usersHandler.SetUserListFavoriteRepo(userListFavoriteRepo)
	usersHandler.SetUserListRepo(userListRepo)
	// users/reactions で moderator/admin が remote / 非 public reactions を
	// 閲覧できるようにする (upstream の iAmModerator bypass)。
	usersHandler.SetModeratorChecker(roleService)
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

	// Account endpoints
	registryRepo := repository.NewRegistryRepository(s.db)
	iHandler := i.NewHandler(userService, idGen)
	iHandler.SetUserRepo(userRepo)
	iHandler.SetRoleProvider(roleService)
	iHandler.SetTOTPReplayGuard(totpReplayGuard)
	// 現在のパスワードを照合する i/* の照合失敗をアカウント単位で数える。
	// route ごとの limiter に置くと、token を持つだけの第三者が被害者の
	// i/regenerate-token を使い切れる (passwordguard の package doc)。
	passwordFailureGuard := passwordguard.NewRedisGuard(s.redis.Default)
	iHandler.SetPasswordFailureGuard(passwordFailureGuard)
	// upstream UserAuthService と同じテスト用バイパス。testMode 以外では無効。
	coretwofactor.SetTestMode(s.config.TestMode)
	iHandler.SetRegistryRepo(registryRepo)
	iHandler.SetMetaRepo(metaRepo)
	iHandler.SetServerURL(s.config.URL)
	// i/regenerate-token で旧 token を auth cache から即時削除する (#884)。
	// 未配線だと cache TTL 経過まで旧 token が auth 通過する security
	// regression が残るので production では必ず wire する。
	iHandler.SetAuthInvalidator(s.auth)
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
	// P4-6 (#166): i/authorized-apps, i/revoke-token, i/gallery/*, i/page-likes
	iHandler.SetAccessTokenRepo(repository.NewAccessTokenRepository(s.db))
	iHandler.SetGalleryRepo(repository.NewGalleryRepository(s.db))
	iHandler.SetPageLikeRepo(pageLikeRepo)
	// P4-6 followup (#174): i/move は federation resolver + deliverService を使って
	// 宛先 actor 解決 → alsoKnownAs 検証 → MovedTo 書き込み → Move activity 配送。
	accountMover := coremove.NewService(userRepo, followingRepo, apURLs, apRenderer, federationResolver, deliverService)
	// #2418: 移行時に旧アカウントのローカルフォロワーを移行先へ follow させる。
	// relationship queue (#2403) に載るので AP 配送を詰まらせない。
	accountMover.SetFollowQueue(s.queueClient)
	// proxy account はリスト購読のため機械的に follow しているだけなので
	// 引き継ぎ対象から外す。newProxyAccountResolver は username を返す別用途の
	// resolver なので、ここでは user id を引く専用のものを渡す。
	moveSystemAccountRepo := repository.NewSystemAccountRepository(s.db)
	accountMover.SetProxyAccountResolver(func() (string, bool) {
		sa, err := moveSystemAccountRepo.FindByType("proxy")
		if err != nil || sa == nil {
			return "", false
		}
		return sa.UserID, true
	})
	// #2419: フォロワー以外の関連データも移行先へ引き継ぐ。いずれも
	// 「旧側を消さずに新側を足す」片方向で、旧アカウントの関係は残す。
	accountMover.SetCarryOverRepos(blockingRepo, mutingRepo, userListRepo, idGen)
	accountMover.SetBlockQueue(s.queueClient)
	accountMover.SetRoleAssigner(roleService)
	accountMover.SetAntennaMover(antennaService)
	iHandler.SetAccountMover(accountMover)
	// #2414: リモートアカウントの移行を検知したら、同じ引き継ぎ処理を走らせる。
	// federation → core/move の一方向依存で循環しない。
	federationResolver.SetMoveProcessor(accountMover)
	// #2415: 移行直後 (相互確認済み + 2h 以内) だけ import の上限を緩和する。
	iHandler.SetMoveInValidator(accountMover)
	api.POST("/i", iHandler.Me, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/update", iHandler.Update, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/pin", iHandler.Pin, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/i/unpin", iHandler.Unpin, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/registry/get", iHandler.RegistryGet, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/set", iHandler.RegistrySet, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/registry/get-all", iHandler.RegistryGetAll, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/keys-with-type", iHandler.RegistryKeysWithType, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/remove", iHandler.RegistryRemove, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/change-password", iHandler.ChangePassword, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/delete-account", iHandler.DeleteAccount, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/favorites", iHandler.Favorites, middleware.RequireAuth(), middleware.RequireScope("read:favorites"))
	api.POST("/i/regenerate-token", iHandler.RegenerateToken, middleware.RequireAuth(), middleware.RequireSecure())
	iHandler.SetFavoriteRepo(noteFavoriteRepo)
	// Phase 7-2 (#244): /api/i の未読系フィールドを実クエリ化。
	iHandler.SetNotificationService(notificationService)
	iHandler.SetAchievementNotifier(notificationService)
	iHandler.SetFollowRequestRepo(followRequestRepo)
	iHandler.SetChatRepo(chatRepo)
	// Phase 7-2 follow-up (#271)
	iHandler.SetAntennaUnreadRepo(antennaNoteUnreadRepo)
	iHandler.SetChannelUnreadRepo(channelNoteUnreadRepo)
	// Phase 7-3 (#245): pinnedNoteIds / pinnedNotes / pinnedPageId / pinnedPage
	iHandler.SetPiningRepo(piningRepo)
	iHandler.SetNoteRepo(noteRepo)
	iHandler.SetPageRepo(pageRepo)
	iHandler.SetInstanceRepo(instanceRepo)
	iHandler.SetEmojiRepo(emojiRepo)
	iHandler.SetReactionReader(reactionCountWriter)
	avatarDecorationRepo := repository.NewAvatarDecorationRepository(s.db)
	iHandler.SetAvatarDecorationRepo(avatarDecorationRepo)
	// PackUserLite が avatarDecorations の各エントリに url を埋め込めるよう
	// 共有 catalog resolver を entity package に登録する (#521)。catalog は
	// admin 管理で低頻度更新のため 30s TTL の in-memory cache で十分。
	avatarDecorationResolver := avatardecoration.NewResolver(avatarDecorationRepo)
	entity.SetAvatarDecorationLookup(avatarDecorationResolver)
	entity.SetEmojiDecorationLookup(emojiDecorationResolver)
	// PackUserLite の canChat を role policy 由来 (= upstream
	// `chatAvailability === "available"`) に揃える (#988)。roleService 自体
	// が in-memory cache を持つ (#761) ので追加 DB 負荷は実質ゼロ。
	entity.SetCanChatLookup(corerole.NewCanChatLookup(roleService))
	// PackUserLite / PackUserDetailed の `badgeRoles` / `roles` field を
	// role assignment から populate (#1103)。旧 mk-go は両 field を空配列
	// hard-code していたため、admin が公開ロールを assign しても profile
	// に表示されなかった。roleService.GetUserRoles も in-memory cache 経由
	// (#761) で hot path コスト無し。
	entity.SetUserRolesLookup(corerole.NewUserRolesLookup(roleService))
	// remote user の badgeRoles を出すかは meta.showRoleBadgesOfRemoteUsers
	// 次第 (upstream)。CachedMetaRepository.Fetch は cache 経由で hot path
	// コスト無し。fetch 失敗時は false (= local-only の保守的 default) (#1653)。
	entity.SetShowRemoteBadgesLookup(func() bool {
		m, err := metaRepo.Fetch()
		return err == nil && m != nil && m.ShowRoleBadgesOfRemoteUsers
	})
	// local system account (relay.actor 等) の avatarUrl を meta.iconUrl に
	// する upstream の特例 (#1781)。IdenticonURL が '.' を含む local username の
	// fallback でのみ参照する。
	entity.SetInstanceIconURLLookup(func() string {
		m, err := metaRepo.Fetch()
		if err != nil || m == nil || m.IconURL == nil {
			return ""
		}
		return *m.IconURL
	})
	// PackDriveFile / IdenticonURL が remote origin の media URL (note 添付・
	// avatar・banner 等) を pack 時に media proxy 経由へ書き換えられるよう
	// context を登録する (#1529)。これが無いと remote の url/thumbnailUrl/
	// avatarUrl が raw のまま <img src> に乗り、閲覧者の IP が remote server
	// に漏れる。proxyRemoteFiles は meta 由来 (既定 true) で boot 時に snapshot
	// する。
	proxyRemoteFiles := true
	if m, err := metaRepo.Fetch(); err == nil {
		proxyRemoteFiles = m.ProxyRemoteFiles
	}

	mediaURLCtx := entity.NewMediaURLContext(
		s.config.URL,
		s.config.MediaProxy,
		s.mediaProxySecret,
		s.config.ExternalMediaProxyEnabled,
		proxyRemoteFiles,
	)
	// **毎回読む。** 起動時の値を焼き込むと、運営者が管理画面で切り替えても
	// プロセスを再起動するまで反映されない。これは閲覧者の IP がリモートへ
	// 漏れるかどうかを決める設定なので、締めたつもりで漏れ続ける形になる。
	mediaURLCtx.SetProxyRemoteFilesLookup(func() bool {
		m, err := metaRepo.Fetch()
		if err != nil || m == nil {
			// 読めないときは起動時の値に倒す (既定 true = プロキシ経由)。
			return proxyRemoteFiles
		}
		return m.ProxyRemoteFiles
	})
	// オブジェクトストレージの公開ドメインは instance ドメインと別なので、
	// これを教えないと自分が保存したファイルまで remote 判定されて media proxy
	// を経由してしまう (#2315)。設定は admin が変更しうるので都度引く。
	mediaURLCtx.SetOwnMediaBaseURLResolver(func() string {
		m, err := metaRepo.Fetch()
		if err != nil {
			return ""
		}
		return coredrive.PublicBaseURL(m)
	})
	entity.SetMediaURLContext(mediaURLCtx)
	// PackUserDetailed の isSilenced を role policy 由来に揃える (旧実装は
	// /api/i 以外で常に false 固定だった)。roleService.IsSilenced が
	// entity.SilencedLookup を構造的に満たすので直接 wire する。GetUserRoles は
	// in-memory cache (#761) だが IsSilenced 内の policy merge 自体は per-call
	// なので、UserDetailed pack は単一 user の detail 取得が主用途であり許容範囲。
	entity.SetSilencedLookup(roleService)
	iHandler.SetNoteFieldResolver(noteFieldResolver)
	// announcementRepoは後続で構築されるため SetupAdditional() 相当の順序依存があるが、
	// 現状 announcementRepo := ... の行がここより後にあるため下で wire する。

	// i/export-* and i/import-* (Phase 9.4)
	iHandler.SetTransferEnqueuer(s.queueClient)
	// #2230: i/delete-account の cascade 削除 (notes/drive/following purge) と AP Delete(actor)
	// 配信を admin/delete-account と同経路で配線する。未配線だと論理削除フラグだけ立って実削除が走らない。
	iHandler.SetDeleteAccountEnqueuer(s.queueClient)
	iHandler.SetAccountDeletionFederationHook(corefederation.NewUserModerationDeliveryHook(deliverService, apRenderer, userRepo))
	api.POST("/i/export-notes", iHandler.ExportNotes, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-following", iHandler.ExportFollowing, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-blocking", iHandler.ExportBlocking, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-mute", iHandler.ExportMute, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-favorites", iHandler.ExportFavorites, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-user-lists", iHandler.ExportUserLists, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-antennas", iHandler.ExportAntennas, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/export-clips", iHandler.ExportClips, middleware.RequireAuth(), middleware.RequireSecure())
	// i/import-* の canImport* role policy gate は #1020 で追加。policy=false
	// の user (default 全 false) は 403 ROLE_PERMISSION_DENIED を返す。
	api.POST("/i/import-following", iHandler.ImportFollowing,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportFollowing), middleware.RequireSecure())
	api.POST("/i/import-blocking", iHandler.ImportBlocking,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportBlocking), middleware.RequireSecure())
	api.POST("/i/import-muting", iHandler.ImportMuting,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportMuting), middleware.RequireSecure())
	api.POST("/i/import-user-lists", iHandler.ImportUserLists,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportUserLists), middleware.RequireSecure())
	api.POST("/i/import-antennas", iHandler.ImportAntennas,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanImportAntennas), middleware.RequireSecure())

	// i/webhooks/* — Webhook管理 (実データ)
	webhookHandler := apiwebhooks.NewHandler(webhookRepo, idGen)
	webhookHandler.SetDispatcher(webhookService)
	webhookHandler.SetRolePolicyProvider(roleService) // #1029: webhookLimit
	api.POST("/i/webhooks/create", webhookHandler.Create, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/webhooks/list", webhookHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/webhooks/show", webhookHandler.Show, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/webhooks/update", webhookHandler.Update, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/webhooks/delete", webhookHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/i/webhooks/test", webhookHandler.Test, middleware.RequireAuth(), middleware.RequireSecure(), middleware.RequireScope("read:account"))

	// Notifications endpoints
	notificationsHandler := notifications.NewHandler(notificationService, idGen)
	notificationsHandler.SetRepos(userRepo, noteRepo)
	notificationsHandler.SetQueryService(noteQueryService)
	notificationsHandler.SetFollowRequestRepo(followRequestRepo)
	notificationsHandler.SetInstanceRepo(instanceRepo)
	notificationsHandler.SetEmojiRepo(emojiRepo)
	// abuseReport 通知は read 時に権限を再確認する (#2868)。**未配線だと
	// fail-closed で abuseReport が誰にも返らない** ので、機能ごと死ぬ。
	notificationsHandler.SetModeratorChecker(roleService)
	// read 時 valid-notifier filter (now-muted notifier の除外) 用 (#1775)。
	notificationsHandler.SetMutingRepo(mutingRepo)
	notificationsHandler.SetTestNotifier(notificationHook)
	notificationsHandler.SetRoleLookup(roleNotifLookup)
	notificationsHandler.SetAbuseReportLookup(abuseNotifStates)
	notificationsHandler.SetEmojiApplicationLookup(emojiApplicationNotifLookup)
	notificationsHandler.SetSignupApplicationLookup(signupApplicationNotifLookup)
	// 通知に埋め込む note の files / channel / myReaction を埋める (#2735)。
	notificationsHandler.SetNoteFieldResolver(noteFieldResolver)
	// notifications/create の 'app' 通知で header/icon を token.name/iconUrl に
	// fallback するため access token repo を配線 (#1557)。
	notificationsHandler.SetAccessTokenRepo(repository.NewAccessTokenRepository(s.db))
	api.POST("/i/notifications", notificationsHandler.Show, middleware.RequireAuth(), middleware.RequireScope("read:notifications"))
	api.POST("/i/notifications-grouped", notificationsHandler.Grouped, middleware.RequireAuth(), middleware.RequireScope("read:notifications"))
	api.POST("/notifications/mark-all-as-read", notificationsHandler.MarkAllAsRead, middleware.RequireAuth(), middleware.RequireScope("write:notifications"))
	api.POST("/notifications/create", notificationsHandler.Create, middleware.RequireAuth(), middleware.RequireScope("write:notifications"))
	api.POST("/notifications/flush", notificationsHandler.Flush, middleware.RequireAuth(), middleware.RequireScope("write:notifications"))
	api.POST("/notifications/test-notification", notificationsHandler.TestNotification, middleware.RequireAuth(), middleware.RequireScope("write:notifications"))

	// i/* ハンドラ
	// claim-achievement / import-* は upstream で prohibitMoved:true (移行済
	// アカウントは YOUR_ACCOUNT_MOVED 403)。RequireNotMoved を配線する (#1555)。
	api.POST("/i/claim-achievement", iHandler.ClaimAchievement, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/i/apps", iHandler.Apps, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/authorized-apps", iHandler.AuthorizedApps, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/signin-history", iHandler.SigninHistory, middleware.RequireAuth(), middleware.RequireSecure())
	// upstream c07ce75281: サードパーティアプリが自身のアクセストークンを失効
	// できるよう secure を外した。**RequireAuth も付けない** — upstream は
	// requireCredential を外して endpoint 固有の CREDENTIAL_REQUIRED を投げるので、
	// 汎用 middleware に任せると error id が食い違う。認証と「自分のトークン以外は
	// 403」はどちらも handler 側で見る。
	api.POST("/i/revoke-token", iHandler.RevokeToken)
	api.POST("/i/update-email", iHandler.UpdateEmail, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/verify-email", iHandler.VerifyEmail)
	api.POST("/i/move", iHandler.Move, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireSecure())
	api.POST("/i/2fa/register", iHandler.TwoFARegister, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/done", iHandler.TwoFADone, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/unregister", iHandler.TwoFAUnregister, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/register-key", iHandler.TwoFARegisterKey, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/key-done", iHandler.TwoFAKeyDone, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/remove-key", iHandler.TwoFARemoveKey, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/update-key", iHandler.TwoFAUpdateKey, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/2fa/password-less", iHandler.TwoFAPasswordLess, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/i/gallery/likes", iHandler.GalleryLikes, middleware.RequireAuth(), middleware.RequireScope("read:gallery-likes"))
	api.POST("/i/gallery/posts", iHandler.GalleryPosts, middleware.RequireAuth(), middleware.RequireScope("read:gallery"))
	api.POST("/i/page-likes", iHandler.PageLikes, middleware.RequireAuth(), middleware.RequireScope("read:page-likes"))
	api.POST("/i/registry/get-detail", iHandler.RegistryGetDetail, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/keys", iHandler.RegistryKeys, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/i/registry/scopes-with-domain", iHandler.RegistryScopesWithDomain, middleware.RequireAuth(), middleware.RequireSecure())

	// i/export-* / i/import-* は上方 (line ~1245-1265) の explicit registration
	// が active (iHandler.Export* / Import* via transfer_handler.go) で、proper
	// な typed transfer.ExportXxx 定数と fileId 受け取りロジックを持つ。
	// かつて本箇所に generic for-loop による登録が同 path で重複していたが
	// (#1020 で気づいた pre-existing pattern)、それは fileId を受けず string
	// literal "following" 等を直接 type に渡す壊れた version で、Echo の
	// route override 経由で active 実装を蹴る悪影響があった。#1031 で削除済。

	// Hashtags endpoints (Phase 6)
	hashtagsHandler := apihashtags.NewHandler(s.db)
	hashtagsHandler.SetIDGen(idGen)                     // #2106 L25: 設定済 ID generator を共有 (毎回 aidx 生成を廃止)
	hashtagsHandler.SetRelationRepos(listRelationRepos) // #1957-a: hashtags/users の embed user に relation
	api.POST("/hashtags/list", hashtagsHandler.List)
	api.POST("/hashtags/search", hashtagsHandler.Search)
	api.POST("/hashtags/show", hashtagsHandler.Show)
	api.POST("/hashtags/trend", hashtagsHandler.Trend)
	// upstream Misskey の hashtags/trend は allowGet: true なので
	// frontend (misskeyApiGet) は GET で叩く。WidgetTrends がここを毎分
	// 呼ぶため GET でも応答できるよう同じ handler を 2 重 wire する。
	api.GET("/hashtags/trend", hashtagsHandler.Trend)
	api.POST("/hashtags/users", hashtagsHandler.Users)

	// Gallery endpoints (Phase 6)
	galleryHandler := apigallery.NewHandler(s.db, idGen)
	galleryHandler.SetRanking(featuredService) // #1548: like/unlike/featured ランキング
	galleryHandler.SetRoleChecker(roleService) // #1548: posts/delete モデレータ削除
	api.POST("/gallery/featured", galleryHandler.Featured)
	api.POST("/gallery/popular", galleryHandler.Popular)
	api.POST("/gallery/posts", galleryHandler.Posts)
	api.POST("/gallery/posts/create", galleryHandler.PostsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:gallery"))
	api.POST("/gallery/posts/show", galleryHandler.PostsShow)
	api.POST("/gallery/posts/delete", galleryHandler.PostsDelete, middleware.RequireAuth(), middleware.RequireScope("write:gallery"))
	api.POST("/gallery/posts/update", galleryHandler.PostsUpdate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:gallery"))
	api.POST("/gallery/posts/like", galleryHandler.PostsLike, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:gallery-likes"))
	api.POST("/gallery/posts/unlike", galleryHandler.PostsUnlike, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:gallery-likes"))

	// Blocking endpoints
	blockingHandler := blocking.NewHandler(blockingService, userRepo, idGen)
	// blocking/create・delete のレスポンスに viewer→blockee の relation block
	// (isBlocking 等) を載せるための共有 resolver 依存 (#1802)。
	blockingHandler.SetRelationRepos(userrelation.Repos{
		Following:     followingRepo,
		Blocking:      blockingRepo,
		Muting:        mutingRepo,
		RenoteMuting:  renoteMutingRepo,
		FollowRequest: followRequestRepo,
		Memo:          repository.NewUserMemoRepository(s.db),
	})
	blockingHandler.SetModeratorChecker(roleService) // #1985: blocking/list の count gate で moderator viewer 判定
	api.POST("/blocking/create", blockingHandler.Create, middleware.RequireAuth(), middleware.RequireScope("write:blocks"))
	api.POST("/blocking/delete", blockingHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:blocks"))
	api.POST("/blocking/list", blockingHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:blocks"))

	// Mute endpoints
	muteHandler := mute.NewHandler(mutingService, userRepo, idGen)
	muteHandler.SetRelationRepos(listRelationRepos)
	muteHandler.SetModeratorChecker(roleService) // #1985: mute/list の count gate で moderator viewer 判定
	api.POST("/mute/create", muteHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:mutes"))
	api.POST("/mute/delete", muteHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:mutes"))
	api.POST("/mute/list", muteHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:mutes"))

	// Renote mute endpoints
	renoteMuteHandler := renotemute.NewHandler(renoteMutingService, userRepo, idGen)
	renoteMuteHandler.SetRelationRepos(listRelationRepos)
	renoteMuteHandler.SetModeratorChecker(roleService) // #1985: renote-mute/list の count gate で moderator viewer 判定
	api.POST("/renote-mute/create", renoteMuteHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:mutes"))
	api.POST("/renote-mute/delete", renoteMuteHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:mutes"))
	api.POST("/renote-mute/list", renoteMuteHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:mutes"))

	// Drive endpoints
	driveHandler := drive.NewHandler(driveService, idGen)
	driveHandler.SetRepos(driveFileRepo, driveFolderRepo, noteRepo)
	driveHandler.SetUserRepo(userRepo)
	driveHandler.SetInstanceRepo(instanceRepo)
	driveHandler.SetEmojiRepo(emojiRepo)
	driveHandler.SetReactionReader(reactionCountWriter)
	driveHandler.SetNoteFieldResolver(noteFieldResolver)
	driveHandler.SetMetaRepo(metaRepo) // #2106 S4: enableIpLogging を尊重して requestIp/headers 保存を gate
	api.POST("/drive", driveHandler.Usage, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files", driveHandler.FilesList, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/create", driveHandler.FilesCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	// 分割アップロード (#2313)。mk-go 独自拡張。保護は create と同一
	// (RequireAuth + RequireNotMoved + write:drive) — append は
	// BodyLimitByPath の 1MiB 制限から外れるため、未認証で到達できる経路を
	// 増やさないことが前提になっている。
	api.POST("/drive/files/create-chunked/start", driveHandler.FilesCreateChunkedStart, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/create-chunked/append", driveHandler.FilesCreateChunkedAppend, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/create-chunked/finish", driveHandler.FilesCreateChunkedFinish, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/create-chunked/abort", driveHandler.FilesCreateChunkedAbort, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/show", driveHandler.FilesShow, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/update", driveHandler.FilesUpdate, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/delete", driveHandler.FilesDelete, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/find-by-hash", driveHandler.FilesFindByHash, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/folders/create", driveHandler.FoldersCreate, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/folders/show", driveHandler.FoldersShow, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/folders/update", driveHandler.FoldersUpdate, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/folders/delete", driveHandler.FoldersDelete, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/folders", driveHandler.FoldersList, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/folders/find", driveHandler.FoldersFind, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/find", driveHandler.FilesFind, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/check-existence", driveHandler.FilesCheckExistence, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/attached-notes", driveHandler.FilesAttachedNotes, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	// drive/files/attached-chat-messages は chat message を pack するため chat
	// handler 側で処理する (route 登録は chatHandler 構築後、#1218)。
	api.POST("/drive/files/upload-from-url", driveHandler.FilesUploadFromURL, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/move-bulk", driveHandler.FilesMoveBulk, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/stream", driveHandler.Stream, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	// Phase 7.4: drive/* — 全て実データハンドラに移行済み

	// Static file serving for LocalStorage / S3 backend. `storedInternal=true`
	// な行は driveStorage が S3 でも localDriveStorage に倒して提供する (#1414)。
	//
	// driveStorage がローカル (= object storage 非活性) の場合は primary と
	// local が同じローカル FS を指すため storedInternal 判定が無意味で、
	// ホットパスである `/files/:accessKey` の毎リクエスト DB クエリを省ける。
	// backend は動的に切り替わる (#2315) ので、この判定は配線時ではなく
	// filesHandler がリクエストごとに行う。
	s.echo.GET("/files/:accessKey", filesHandler(driveFileRepo, driveStorage, localDriveStorage))

	// Media proxy endpoint
	proxyAllowlist := coremediaproxy.NewDBAllowlistChecker(s.db)
	proxyService := coremediaproxy.NewService(
		s.config.URL, s.config.UserAgent, driveStorage,
		proxyAllowlist, s.mediaProxySecret,
		s.config.AllowedPrivateNetworks,
		s.outboundOpts()...,
	)
	// 画像処理の同時実行枠 (#3032)。0 なら mediaproxy 側の既定
	// (GOMAXPROCS / 2、最低 1)。**配線しないと設定キーが黙って効かない。**
	proxyService.SetCPUConcurrency(s.config.MediaProxyConcurrency)
	// Local drive file の thumbnail / webpublic 変種を proxy 側で再 encode
	// せず直接返せるようにする (#637 M1)。
	proxyService.SetDriveLookup(driveFileLookupAdapter{repo: driveFileRepo})
	// object storage 有効化より前に保存された storedInternal=true の実体は
	// ローカル FS にある。primary が空振りしたときの fallback に使う (#2315)。
	proxyService.SetLocalStorage(localDriveStorage)
	// 動画 still frame の生成は外部 service に委譲する (#637 M2 redesign)。
	// config.videoThumbnailGenerator が空のときは disabled (proxy は dummy
	// PNG を返す)。`unix:///path/socket` 形式で UDS deployment にも対応。
	// videoThumbnailGeneratorMode で wire を選択 ("post" 既定 / "get" は
	// Misskey TS 互換)。
	if s.config.VideoThumbnailGenerator != "" {
		proxyService.SetVideoThumbnailGeneratorWithMode(
			s.config.VideoThumbnailGenerator,
			s.config.VideoThumbnailGeneratorMode,
		)
	}
	proxyHandler := apiproxy.NewHandler(proxyService, s.config)
	s.echo.GET("/proxy/*", proxyHandler.Handle)

	// ActivityPub resource endpoints
	apHandler := ap.NewHandler(apRenderer, userService, noteQueryService, keypairRepo, idGen)
	apHandler.SetRemote(apFetcher, federationResolver)
	// ap/show の federation-allow gate (#1557)。instanceService が blocked /
	// federation policy を判定する。local host は port 無しの hostname
	// (config 起動時に validate 済で非空保証) を渡す — IsBlocked/IsAllowed は
	// hostname suffix match のため。
	apHandler.SetFederationGate(instanceService, s.config.Hostname)
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
		Memo:          repository.NewUserMemoRepository(s.db),
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
		s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability,
		ssrMetaDeps{
			User:         userRepo,
			Note:         noteRepo,
			Page:         pageRepo,
			Clip:         clipRepo,
			Flash:        flashRepo,
			Gallery:      repository.NewGalleryRepository(s.db),
			Drive:        driveFileRepo,
			Channel:      channelRepo,
			Reversi:      reversiRepo,
			Announcement: repository.NewAnnouncementRepository(s.db),
			IDGen:        idGen,
		},
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
	// ユーザーフィード (#2345)。upstream ClientServerService と同じく
	// /@:user.rss / .atom / .json を返す。
	feedHost := s.config.URL
	if pu, err := urlpkg.Parse(s.config.URL); err == nil && pu.Host != "" {
		feedHost = pu.Host
	}
	feedH := &feedHandler{
		baseURL:   s.config.URL,
		host:      feedHost,
		users:     feedUserResolver{repo: userRepo},
		notes:     noteRepo,
		parseTime: idGen.ParseTime,
		profiles: func(userID string) *model.UserProfile {
			p, err := userRepo.FindProfileByUserID(userID)
			if err != nil {
				return nil
			}
			return p
		},
		avatarURL: func(u *model.User) string {
			if u.AvatarURL != nil && *u.AvatarURL != "" {
				return *u.AvatarURL
			}
			return entity.IdenticonURL(u)
		},
		toHTML: func(text string) string {
			return mfm.ToHTML(mfm.Parse(text), feedHost)
		},
	}

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
	wellknownHandler := wellknown.NewHandler(apURLs, userService, s.config.Host, s.config.URL)
	// federation='none' のとき discovery を 403 で塞ぐため metaRepo を inject (#1924)。
	wellknownHandler.SetMetaRepo(metaRepo)
	// 非 API の Web リソース (#2345)。SPA catchall (s.echo.GET("/*")) より先に
	// 登録しないと index.html が 200 で返り、実装済みに見えて中身が HTML という
	// 分かりにくい壊れ方をする。
	webResHandler := newWebResourceHandler(s.config, metaRepo)
	s.echo.GET("/robots.txt", webResHandler.RobotsTxt)
	s.echo.GET("/opensearch.xml", webResHandler.OpenSearchXML)
	s.echo.GET("/api.json", s.OpenAPISpec)
	// Web Push の通知バッジ (#2345)。twemoji SVG を単色 PNG に焼いて返す。
	s.echo.GET("/twemoji-badge/*", newTwemojiBadgeHandler(frontendutil.TwemojiDir()).Serve)

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
		repository.NewAccessTokenRepository(s.db),
		idGen,
		s.config.URL,
		// 本家 OAuth2ProviderService は NODE_ENV === 'test' のとき http の
		// client_id を許可する (本番は https のみ)。TestMode を同じ用途で使う。
		s.config.TestMode,
		frontendConsentHTML(s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability),
	)
	// code 再利用で token を revoke したとき、auth cache に残った entry で
	// 通り続けないよう即時失効させる (RFC6749 §4.1.2)。
	oauthHandler.SetAuthInvalidator(s.auth)
	s.echo.GET("/oauth/authorize", oauthHandler.Authorize)
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

	nodeinfoHandler := nodeinfo.NewHandler(s.config)
	nodeinfoHandler.SetMetaRepo(metaRepo)
	nodeinfoHandler.SetUsageRepos(userRepo, noteRepo)
	// metadata.proxyAccountName 用に meta と同じ resolver を共有する (#1777)。
	nodeinfoHandler.SetProxyAccountResolver(proxyAccountResolver)
	s.echo.GET("/nodeinfo/2.1", nodeinfoHandler.Version2_1)
	// /.well-known/nodeinfo が 2.0 リンクも advertise するので 2.0 も配信する (#1777)。
	s.echo.GET("/nodeinfo/2.0", nodeinfoHandler.Version2_0)

	// Inbox endpoints
	inboxHandler := inbox.NewHandler(federationResolver, federationProcessor)
	inboxHandler.SetHostBlockChecker(instanceService)
	inboxHandler.SetInstanceTracker(instanceService)
	inboxHandler.SetSignatureCapabilityRecorder(sigCapBuffer)
	inboxHandler.SetChartHook(chartHooks)
	// inbox admission の host 署名+一致チェックに自ホストを配線する (#1949)。
	inboxHandler.SetExpectedHost(s.config.Host)
	// #534: signature 検証成功後の Process(body) を inbox queue に逃がす。
	// 未配線時は legacy 同期処理にフォールバック (テスト互換)。
	inboxHandler.SetEnqueuer(s.queueClient)
	// inbox の 64KB body limit は global pre-auth な middleware.BodyLimitByPath で適用する
	// (#1958 / #2075)。route-level に置くと global な auth.Authenticate の token 抽出 read が
	// 先に走って bypass されるため、ここでは limit を付けない。
	s.echo.POST("/inbox", inboxHandler.Inbox)
	s.echo.POST("/users/:id/inbox", inboxHandler.Inbox)

	// Federation endpoints
	federationHandler := apifederation.NewHandler(instanceService)
	federationHandler.SetFollowingRepo(followingRepo)
	federationHandler.SetUserRepo(userRepo)
	federationHandler.SetIDGen(idGen)
	federationHandler.SetResolver(federationResolver)
	federationHandler.SetRelationRepos(listRelationRepos) // #1957-a: followers/following/users の embed user に relation
	// moderationNote は公開エンドポイントで moderator にのみ返す (情報漏洩対策)。
	federationHandler.SetModeratorChecker(roleService)
	// instance 一覧 / show-instance の signatureCapability field (#2393)。
	federationHandler.SetSignatureCapabilityLookup(sigCapRepo)
	api.POST("/federation/instances", federationHandler.Instances)
	api.GET("/federation/instances", federationHandler.Instances)
	api.POST("/federation/show-instance", federationHandler.ShowInstance)
	api.POST("/federation/followers", federationHandler.Followers)
	api.POST("/federation/following", federationHandler.Following)
	api.POST("/federation/users", federationHandler.Users)
	// admin overview から `misskeyApiGet` で叩かれるため GET も登録 (#421)。
	api.POST("/federation/stats", federationHandler.Stats)
	api.GET("/federation/stats", federationHandler.Stats)
	// upstream 2026.9.1 で `requireCredential: true` / `kind: 'read:account'` /
	// 1 時間 30 回になった (それまでは未認証でも叩けた)。mk-go は #2106 で先に
	// RequireAuth を付けていたので、scope とレート制限 (ratelimit_defs.go) を足して
	// 揃える。RequireModerator にしないのは、frontend がリモートユーザーの
	// メニューで全ログインユーザーに出すため。
	api.POST("/federation/update-remote-user", federationHandler.UpdateRemoteUser, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	// Mastodon 互換のピア一覧 (#2245)。upstream は endpoints/ 配下ではなく
	// ApiServerService.ts で fastify に直接登録しているので、mk-go でも
	// endpoint 群とは別に /api/v1/... として生やす。
	api.GET("/v1/instance/peers", federationHandler.Peers)

	// Channels endpoints (Phase 4.2)
	channelsHandler := apichannels.NewHandler(channelService, idGen)
	channelsHandler.SetFavoriteRepo(channelFavoriteRepo)
	channelsHandler.SetMutingRepo(channelMutingRepo)
	channelsHandler.SetFollowingRepo(channelFollowingRepo)
	// channels/timeline の mute / block filter (#2345 の timelines 調査)。
	channelsHandler.SetMuteBlockRepos(mutingRepo, blockingRepo, noteRepo)
	channelsHandler.SetDriveFileRepo(driveFileRepo)
	// channels/create の canCreateChannel gate は #1020 で middleware に
	// 昇格 (handler 内 RolePolicyChecker → middleware.RequireRolePolicy)。
	api.POST("/channels/create", channelsHandler.Create,
		middleware.RequireAuth(), middleware.RequireNotMoved(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanCreateChannel), middleware.RequireScope("write:channels"))
	api.POST("/channels/show", channelsHandler.Show)
	api.POST("/channels/update", channelsHandler.Update, middleware.RequireAuth(), middleware.RequireScope("write:channels"))
	api.POST("/channels/follow", channelsHandler.Follow, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/unfollow", channelsHandler.Unfollow, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/followed", channelsHandler.Followed, middleware.RequireAuth(), middleware.RequireScope("read:channels"))
	api.POST("/channels/owned", channelsHandler.Owned, middleware.RequireAuth(), middleware.RequireScope("read:channels"))
	api.POST("/channels/featured", channelsHandler.Featured)
	api.POST("/channels/search", channelsHandler.Search)
	api.POST("/channels/timeline", channelsHandler.Timeline)
	channelsHandler.SetInstanceRepo(instanceRepo)
	channelsHandler.SetEmojiRepo(emojiRepo)
	channelsHandler.SetReactionReader(reactionCountWriter)
	channelsHandler.SetNoteFieldResolver(noteFieldResolver)
	channelsHandler.SetUserRepo(userRepo)
	channelsHandler.SetPinnedNoteRepo(noteRepo) // #1540: channels/show (detailed) の pinnedNotes 展開
	// pinnedNotes の可視性判定に使う利用者間フォロー。**上の
	// `SetFollowingRepo` (チャンネルのフォロー) とは別物。** 未配線だと
	// `CanSeeNote` が fail-closed に倒れ、followers 限定ノートが投稿者本人に
	// しか出なくなる (漏れる側ではないが、正当なピン留めも消える)。
	channelsHandler.SetUserFollowingRepo(followingRepo)
	channelsHandler.SetMetaRepo(metaRepo) // channels/timeline の blocked-host filter

	// Antennas endpoints (Phase 4.3)
	antennasHandler := antennas.NewHandler(antennaService, noteRepo, idGen)
	antennasHandler.SetInstanceRepo(instanceRepo)
	antennasHandler.SetEmojiRepo(emojiRepo)
	antennasHandler.SetReactionReader(reactionCountWriter)
	antennasHandler.SetNoteFieldResolver(noteFieldResolver)
	antennasHandler.SetUserRepo(userRepo)
	antennasHandler.SetQueryService(noteQueryService)                              // #1464: Notes の visibility filter (defense-in-depth)
	antennasHandler.SetMuteBlockRepos(mutingRepo, blockingRepo, channelMutingRepo) // #1544: Notes の mute/block/channel-mute filter
	antennasHandler.SetMetaRepo(metaRepo)                                          // #2106 N5: Notes の blocked-host filter
	api.POST("/antennas/create", antennasHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/antennas/show", antennasHandler.Show, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/antennas/update", antennasHandler.Update, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/antennas/delete", antennasHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/antennas/list", antennasHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/antennas/notes", antennasHandler.Notes, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/antennas/remove-note", antennasHandler.RemoveNote, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account")) // #2069 #17463

	// Clips endpoints (Phase 4.4)
	clipsHandler := clips.NewHandler(clipService, idGen)
	clipsHandler.SetFavoriteRepo(clipFavoriteRepo)
	clipsHandler.SetInstanceRepo(instanceRepo)
	clipsHandler.SetEmojiRepo(emojiRepo)
	clipsHandler.SetReactionReader(reactionCountWriter)
	clipsHandler.SetNoteFieldResolver(noteFieldResolver)
	clipsHandler.SetUserRepo(userRepo)
	clipsHandler.SetQueryService(noteQueryService)           // #1456: AddNote の visibility gate
	clipsHandler.SetMuteBlockRepos(mutingRepo, blockingRepo) // #1562: Notes の muted/blocked-user filter
	clipsHandler.SetMetaRepo(metaRepo)                       // #1562: Notes の blocked-host filter
	api.POST("/clips/create", clipsHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/clips/show", clipsHandler.Show, middleware.RequireScope("read:account"))
	api.POST("/clips/update", clipsHandler.Update, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/clips/delete", clipsHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/clips/list", clipsHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/clips/add-note", clipsHandler.AddNote, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/clips/remove-note", clipsHandler.RemoveNote, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:account"))
	api.POST("/clips/notes", clipsHandler.Notes, middleware.RequireScope("read:account"))

	// Pages endpoints (Phase 4.5)
	pagesHandler := pages.NewHandler(pageService, idGen)
	// page content の image block / eyeCatchingImageId から drive file を解決して
	// attachedFiles / eyeCatchingImage を埋める (#1662)。
	pagesHandler.SetDriveFileRepo(driveFileRepo)
	api.POST("/pages/create", pagesHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:pages"))
	api.POST("/pages/show", pagesHandler.Show)
	api.POST("/pages/update", pagesHandler.Update, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:pages"))
	api.POST("/pages/delete", pagesHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:pages"))
	api.POST("/pages/featured", pagesHandler.Featured)
	api.POST("/pages/like", pagesHandler.Like, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:page-likes"))
	api.POST("/pages/unlike", pagesHandler.Unlike, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:page-likes"))
	api.POST("/i/pages", pagesHandler.My, middleware.RequireAuth(), middleware.RequireScope("read:pages"))

	// Flash endpoints (Phase 4.5)
	flashHandler := apiflash.NewHandler(flashService, userRepo, idGen)
	flashHandler.SetRoleChecker(roleService) // #1548: flash/delete モデレータ削除
	api.POST("/flash/create", flashHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:flash"))
	api.POST("/flash/show", flashHandler.Show)
	api.POST("/flash/update", flashHandler.Update, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:flash"))
	api.POST("/flash/delete", flashHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:flash"))
	api.POST("/flash/featured", flashHandler.Featured)
	api.POST("/flash/search", flashHandler.Search)
	api.POST("/flash/like", flashHandler.Like, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:flash-likes"))
	api.POST("/flash/unlike", flashHandler.Unlike, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:flash-likes"))
	api.POST("/flash/my", flashHandler.My, middleware.RequireAuth(), middleware.RequireScope("read:flash"))
	api.POST("/flash/my-likes", flashHandler.MyLikes, middleware.RequireAuth(), middleware.RequireScope("read:flash-likes"))
	api.POST("/i/flashs", flashHandler.My, middleware.RequireAuth(), middleware.RequireScope("read:flash"))
	api.POST("/i/flashs/likes", flashHandler.MyLikes, middleware.RequireAuth(), middleware.RequireScope("read:flash-likes"))

	// Streaming (Phase 4.1 Step K)
	// 1. Redis pubsub bus (核となる publish/subscribe チャンネル)
	streamPubSub := event.NewPubSubService(s.redis.Pubsub, "stream:")
	streamBus := stream.NewEventPubSubBus(streamPubSub)

	// #1740: meta 更新の cross-worker cache invalidation。更新した worker は
	// internal:metaUpdated を publish し、各 worker は受信して自プロセスの
	// CachedMetaRepository を invalidate する。upstream の
	// globalEventService.publishInternalEvent('metaUpdated'/'policiesUpdated') 相当で、
	// update-meta / update-default-policies 等あらゆる meta 更新を 1 箇所で伝播する。
	//
	// /api/meta は serialize 済みレスポンスも持つ (#2649) ので、meta 行の
	// cache を落とす両方の経路 (自 worker の更新 / 他 worker からの受信) で
	// あわせて落とす。**どちらも meta 行の cache を先に落とすこと。** 逆順だと、
	// response cache を落とした直後に走った組み立てが古い meta 行を読んで
	// しまう (その窓は respCache の世代チェックでは塞げない)。hook 側は
	// CachedMetaRepository.Update が invalidate してから notifyUpdate する
	// 実装がその順序を保証しており、subscriber 側はここの並び順で保証する。
	internalPubSub := event.NewPubSubService(s.redis.Pubsub, "internal:")
	// captcha の provider 集合は起動時の meta スナップショットから組まれるので、
	// **ここで繋がないと管理画面で有効にしても再起動まで一切検証されない**。
	// `/api/meta` は DB を読むのでフロントは captcha を描画し、運営者からは
	// ON に見える。provider が 1 つも無いときの `Verify` は成功を返すため、
	// 有効化したつもりのまま `/api/signup` と `/api/signin` が素通りしていた。
	//
	// **自 worker の更新と他 worker からの受信の両方を通す。** 前者は hook、
	// 後者は subscriber で、どちらも同じ channel を経由する。
	reloadCaptcha := func() {
		if captchaSvc == nil {
			return
		}
		m, err := cachedMeta.Fetch()
		if err != nil || m == nil {
			// **読めなければ据え置く。** DB の瞬断で運営者の設定が消える側に
			// 倒さない。
			slog.Warn("captcha: reload skipped (meta unavailable)", "err", err)
			return
		}
		captchaSvc.Reload(m)
	}
	cachedMeta.SetInvalidationHook(func() {
		metaHandler.InvalidateResponseCache()
		reloadCaptcha()
		if err := internalPubSub.Publish(context.Background(), "metaUpdated", struct{}{}); err != nil {
			slog.Warn("meta: publish metaUpdated failed", "err", err)
		}
	})
	internalPubSub.Subscribe(context.Background(), "metaUpdated", func([]byte) {
		cachedMeta.Invalidate()
		metaHandler.InvalidateResponseCache()
		reloadCaptcha()
	})

	// #3037: ロール / ポリシーの cross-worker cache invalidation。更新した
	// worker は internal:rolesUpdated を publish し、各 worker は受信して自
	// プロセスのキャッシュを落とす。upstream の
	// `RoleService` が `internal` チャンネルの `roleUpdated` /
	// `userRoleAssigned` 等を購読しているのに相当する。
	//
	// **繋がないと剥奪が届かない。** `roleCacheTTL` は 5 分で、複数プロセス
	// 構成 (`MK_ONLY_SERVER` / `MK_ONLY_QUEUE`) は明示的にサポートされている。
	// 侵害された管理者のロールを剥奪しても、**剥奪操作を受け付けなかった側の
	// ノードでは最大 5 分間、管理 API が通り続ける**。
	//
	// **受信側は `InvalidateAllCachesLocally`。** 通常の invalidate を呼ぶと
	// そこから再び publish され、ワーカー同士が通知を投げ合って止まらなくなる。
	//
	// **自分の publish は受け流す (#3037)。** Redis の pub/sub は publish した
	// プロセス自身にも配る。自 worker は既に**精密な** invalidate を済ませて
	// いる (その user / その role だけ) のに、自分の通知で全消しに格上げされる
	// と、`admin/roles/assign` 1 回で `userRoleCache` も `rolesListCache` も
	// 全プラグインの policy cache も落ちる。`move_service` はアカウント移行
	// 1 回でロール数ぶん連射するので、そこが効く。
	rolesUpdatedSender := idGen.Generate(time.Now())
	roleService.SetInvalidationHook(func() {
		if err := internalPubSub.Publish(context.Background(), "rolesUpdated", rolesUpdatedSender); err != nil {
			slog.Warn("role: publish rolesUpdated failed", "err", err)
		}
	})
	internalPubSub.Subscribe(context.Background(), "rolesUpdated", func(payload []byte) {
		var from string
		if err := json.Unmarshal(payload, &from); err == nil && from == rolesUpdatedSender {
			return
		}
		roleService.InvalidateAllCachesLocally()
	})

	// #2752: antenna の cross-worker cache invalidation。書き込んだ worker が
	// internal:antennaUpdated を publish し、各 worker が自プロセスの
	// CachedAntennaRepository を invalidate する。upstream の
	// `antennaCreated` / `antennaUpdated` / `antennaDeleted` 内部イベントに相当
	// (upstream は 3 種に分けているが、mk-go の cache は「active な全件」しか
	// 持たないのでどれも全体 invalidate になる。分ける利得が無い)。
	//
	// **publish 側は書き込んだ worker 自身も invalidate 済み。** hook は
	// CachedAntennaRepository.afterWrite が invalidate してから呼ぶ。
	//
	// **この配線自体はテストが無い** (internal/server は CI カバレッジ 0% 例外で、
	// e2e にも antennaUpdated を見るものが無い)。channel 名の typo / hook の
	// 呼び忘れ / Subscribe の抜けは「他 worker に最大 1 分反映されない」形にしか
	// ならず、TTL が症状を覆い隠す。直上の metaUpdated も同じ posture なので
	// 既存パターンからの逸脱ではないが、**TTL を伸ばすならここを先に検証すること**。
	cachedAntenna.SetInvalidationHook(func() {
		if err := internalPubSub.Publish(context.Background(), "antennaUpdated", struct{}{}); err != nil {
			slog.Warn("antenna: publish antennaUpdated failed", "err", err)
		}
	})
	internalPubSub.Subscribe(context.Background(), "antennaUpdated", func([]byte) {
		cachedAntenna.Invalidate()
	})

	// pollVoted / reacted / unreacted / deleted を noteStream:<id> に publish
	// する共通 publisher。subNote / sn メッセージで購読しているクライアントへ
	// リアクション・削除イベントが流れる (#690 / #700)。pollService /
	// reactionService / noteDeleteService は streamPubSub 生成より前に作るため、
	// ここで後付け配線する。
	noteEventPub := stream.NewNoteEventPublisher(streamPubSub)
	pollService.SetEventPublisher(noteEventPub)
	reactionService.SetNoteStreamHook(&reactionNoteStreamAdapter{pub: noteEventPub})
	noteDeleteService.SetNoteStreamHook(&noteDeleteStreamAdapter{pub: noteEventPub})

	// 2. Channel registry: Misskey 互換のチャンネル名で各 factory を登録する
	streamRegistry := stream.NewRegistry()
	streamRegistry.RegisterCredentialed("homeTimeline", channels.NewHomeTimeline)
	streamRegistry.Register("localTimeline", channels.NewLocalTimeline)
	streamRegistry.Register("globalTimeline", channels.NewGlobalTimeline)
	streamRegistry.RegisterCredentialed("hybridTimeline", channels.NewHybridTimeline)
	streamRegistry.RegisterCredentialed("notifications", channels.NewNotifications)
	streamRegistry.RegisterCredentialed("main", channels.NewMain)
	streamRegistry.RegisterCredentialed("drive", channels.NewDrive)
	streamRegistry.Register("hashtag", channels.NewHashtag)
	streamRegistry.RegisterCredentialed("antenna", channels.NewAntennaFactory(antennaRepo).New)
	streamRegistry.Register("channel", channels.NewChannelTimeline)
	// userList は membership lookup を渡して per-member withReplies gate を有効化 (#2020)。
	streamRegistry.Register("userList", channels.NewUserListFactory(userListRepo).New)
	streamRegistry.Register("roleTimeline", channels.NewRoleTimelineFactory(roleService).New)
	streamRegistry.RegisterCredentialed("admin", channels.NewAdminFactory(roleService).New)
	// serverStats / queueStats は publisher を後段で構築するため、ここでは
	// 仮 register せず、publisher 生成後に登録する (1497 行付近)。

	// 3. Connection manager + 各 publisher の生成
	streamManager := stream.NewManager(streamRegistry, streamBus)
	// readNotificationメッセージをnotificationServiceに橋渡しする
	streamManager.SetNotificationReader(&notifReaderAdapter{svc: notificationService})
	// hardMutedWords (#787) 用の rules lookup を wire。connection 確立時に
	// 1 度だけ user_profile を引き、以降の publish では cache された rules を
	// 各 timeline channel が参照する。
	streamManager.SetHardMuteLookup(&hardMuteLookupAdapter{userRepo: userRepo})
	// followeeSnapshot (#1063): home/hybrid/local timeline channel が reply gate
	// に使う「followeeID -> withReplies」map を接続確立時に 1 回だけ fetch する。
	// 失敗 / anonymous は nil 返却で degrade — channel は 3 escape hatch のみで
	// 動く (= 旧来の "全 reply drop" よりは upstream 互換に近い)。
	streamManager.SetFollowingSnapshotLookup(&followingSnapshotAdapter{repo: followingRepo})
	// muteBlockSnapshot (#1711): main / notifications channel が notification /
	// mention 配信時に instance-mute / block / mute を gate するための snapshot を
	// 接続確立時に 1 回 fetch する。失敗 / anonymous は nil 返却で fail-open に
	// degrade — filter 無しで動き続ける。
	streamManager.SetMuteBlockSnapshotLookup(&muteBlockSnapshotAdapter{
		muting:        mutingRepo,
		blocking:      blockingRepo,
		renoteMuting:  renoteMutingRepo,
		channelMuting: channelMutingRepo,
		userRepo:      userRepo,
	})
	// subNote visibility gate (#1460 IDOR fix): handleSubNote が任意の noteID
	// で noteStream を subscribe させると、followers / specified note の
	// reacted / unreacted / pollVoted / deleted event が非対象 viewer に
	// 漏れる (HTTP 側 #1444 i/notifications IDOR の WS 版)。noteQueryService
	// の RequireVisible が NoteVisibilityChecker interface を自動 satisfy する。
	streamManager.SetNoteVisibilityChecker(noteQueryService)
	// onlineStatus の source。upstream と同じく WebSocket 接続中だけ
	// lastActiveDate を更新する (REST では更新しない、#2350)。
	streamManager.SetLastActiveRecorder(&lastActiveRecorderAdapter{userRepo: userRepo})
	// timeline channel (LTL/GTL/HTL) の ltlAvailable / gtlAvailable gate 用に role
	// policy provider を配線する (#1942)。REST timeline と同じく WS でも policy で
	// 無効化された timeline を subscribe させない (policy bypass 解消)。
	streamManager.SetPolicyProvider(roleService)
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
	streamManager.OnStreamRevoke(s.auth.InvalidateTokensForUser)
	// 2 回目の閉じ処理は tokenCache の TTL が切れた後に行う (DB を引いた直後に
	// 無効化 event を追い越して積まれた entry も、そこで確実に切れている)。
	streamManager.SetRevokeRecheckDelay(middleware.AuthCacheTTL + 5*time.Second)
	// WebSocket は接続時に 1 度しか認証しないので、tokenCache を落とすだけでは
	// 既存の接続が閉じない。失効を扱う handler へ revoke publisher を配る。
	// 自プロセスの Manager も渡す — publish が失敗しても、ここに居る接続は閉じる。
	streamRevokePublisher := stream.NewStreamRevokePublisher(streamPubSub, streamManager)
	iHandler.SetStreamRevoker(streamRevokePublisher)
	oauthHandler.SetStreamRevoker(streamRevokePublisher)
	iHandler.SetHardMutePublisher(&hardMutePublisherAdapter{pubsub: streamPubSub})
	// relation 変更 (#2400) の publisher を各 mutation 側へ配線する。7 系統すべてを
	// 繋がないと「一部の操作だけ反映されない」形の抜けになるので、まとめて置く。
	//   Muting          -> core/muting
	//   BlockingMe      -> core/blocking (通知先は blockee、向きに注意)
	//   RenoteMuting    -> core/muting の RenoteService
	//   MutedInstances  -> api/i の update
	//   MutingChannels  -> api/channels の mute
	//   following       -> core/following (Follow / Unfollow / AcceptRequest)
	relationReloadPublisher := &relationReloadPublisherAdapter{pubsub: streamPubSub}
	mutingService.SetRelationReloadPublisher(relationReloadPublisher)
	blockingService.SetRelationReloadPublisher(relationReloadPublisher)
	renoteMutingService.SetRelationReloadPublisher(relationReloadPublisher)
	followingService.SetRelationReloadPublisher(relationReloadPublisher)
	channelsHandler.SetRelationReloadPublisher(relationReloadPublisher)
	iHandler.SetRelationReloadPublisher(relationReloadPublisher)
	// 期限付き mute の**自然失効**も snapshot に反映させる (#2453)。上の 7 系統は
	// すべて利用者の操作が起点だが、期限切れは誰も操作しないので cron が唯一の
	// 通知点になる。ここを繋がないと「1 時間だけミュート」が接続中は解除されない。
	checkExpiredMutingsProcessor.SetRelationReloadPublisher(relationReloadPublisher)
	// profile 編集を remote followers に Update(Person) で配信する (#1560)。
	// actor endpoint と同じ shape にするため ed25519 keypair を、follower 以外の
	// relay subscriber にも届けるため relay broadcaster を配線する。
	profileUpdateHook := corefederation.NewProfileUpdateDeliveryHook(deliverService, apRenderer, userRepo, keypairRepo)
	profileUpdateHook.SetKeypairExtraRepo(keypairExtraRepo)
	profileUpdateHook.SetRelayBroadcaster(relaySvc)
	iHandler.SetProfileUpdateHook(profileUpdateHook)
	// note pin/unpin の Add/Remove(featured collection) を followers/relay へ配信 (#2024)。
	pinDeliveryHook := corefederation.NewPinDeliveryHook(deliverService, apRenderer, userRepo, noteRepo)
	pinDeliveryHook.SetRelayBroadcaster(relaySvc)
	iHandler.SetPinDeliveryHook(pinDeliveryHook)
	notePublisher := stream.NewNotePublisher(streamPubSub, idGen)
	notePublisher.SetEmojiLookup(emojiRepo)
	notePublisher.SetInstanceLookup(instanceRepo)
	notePublisher.SetReactionReader(reactionCountWriter)
	notePublisher.SetReactionPairReader(reactionRepo) // #2067: renote embed の pair cache を note_reaction から埋める
	notePublisher.SetFieldResolver(noteFieldResolver)
	notificationPublisher := stream.NewNotificationPublisher(streamPubSub)
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
	drivePublisher := stream.NewDrivePublisher(streamPubSub)
	reversiPublisher := stream.NewReversiGamePublisher(streamPubSub)
	mainStreamPublisher := stream.NewMainStreamPublisher(streamPubSub)
	// broadcast stream publisher (emoji / global announcement 等を全 connection へ、#2046/#2056)。
	broadcastPublisher := stream.NewBroadcastPublisher(streamPubSub)
	// #1549: report-abuse が各 moderator の adminStream:<id> へ newAbuseUserReport
	// を配信できるよう admin publisher + moderator lister を usersHandler に配線。
	usersHandler.SetAbuseReportFanout(roleService, stream.NewAdminStreamPublisher(streamPubSub))
	// 通報を通知欄にも残す (#2868)。**admin stream だけでは足りない** — あちらは
	// その瞬間に管理画面を開いている人にしか届かず、後から見返せない。
	usersHandler.SetAbuseReportInAppNotifier(notificationService)

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
	serverStatsPub := stream.NewServerStatsPublisher(streamPubSub, 0)
	if s.role.RunsServer() {
		serverStatsPub.Start()
		s.registerShutdownHook(func(_ context.Context) { serverStatsPub.Stop() })
	}
	streamRegistry.Register("serverStats", channels.NewServerStatsFactory(serverStatsPub))
	if s.queueInspector != nil {
		queueStatsPub := stream.NewQueueStatsPublisher(&queueStatsInspectorAdapter{inner: s.queueInspector}, streamPubSub, 0)
		if s.role.RunsServer() {
			queueStatsPub.Start()
			s.registerShutdownHook(func(_ context.Context) { queueStatsPub.Stop() })
		}
		streamRegistry.Register("queueStats", channels.NewQueueStatsFactory(queueStatsPub))
	} else {
		// queueInspector が無い起動 (テスト等) では fallback factory で空配列のみ返す
		streamRegistry.Register("queueStats", channels.NewQueueStats)
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
	notificationService.SetMainStreamPublisher(mainStreamPublisher)
	// readAllNotifications は upstream postReadAllNotifications と同じく
	// main stream と Web Push の両方へ送る。これが無いと SW 側の
	// readAllNotifications 分岐 (表示中の OS 通知を閉じる) が発火しない。
	notificationService.SetReadAllPusher(webPushService)
	notificationService.SetPacker(notificationPublisher)
	// ロール単位の通知 opt-out (#2898)。**配線しないと optOutNotificationTypes
	// を設定しても効かない** — Service は resolver 未設定なら全ての通知を通す。
	notificationService.SetPolicyResolver(roleService)
	driveService.SetStreamingPublisher(drivePublisher)
	// folder の folderCreated/folderUpdated/folderDeleted も同じ drive channel
	// publisher で配信する (#1564)。
	driveService.SetFolderStreamingPublisher(drivePublisher)
	driveService.SetMainStreamPublisher(mainStreamPublisher)
	// /drive/files/upload-from-url: SSRF-safe client で URL を download し、
	// drive へ保存後 urlUploadFinished を main stream に流す (#1217)。download は
	// 大きめになりうるので 60s timeout、cap は cfg.MaxFileSize に揃える。
	driveHandler.SetURLUploader(drive.NewURLUploader(
		s.outboundClient(60*time.Second), driveService, mainStreamPublisher,
		idGen, s.config.UserAgent, s.config.MaxFileSize,
	))
	followingService.SetMainStreamPublisher(mainStreamPublisher)
	userService.SetMainStreamPublisher(mainStreamPublisher)
	noteCreateService.SetMainStreamPublisher(mainStreamPublisher)
	signinHandler.SetMainStreamPublisher(mainStreamPublisher)
	iHandler.SetMainStreamPublisher(mainStreamPublisher)
	pagesHandler.SetMainStreamPublisher(mainStreamPublisher)
	pagesHandler.SetUserSource(userService)

	// 5. Reversi WebSocket channel (Phase 9.6) を登録する
	reversiService := corereversi.NewService(reversiRepo, reversiPublisher, s.redis.Default)
	streamRegistry.Register("reversiGame", channels.NewReversiGameFactory(reversiService).New)
	streamRegistry.RegisterCredentialed("reversi", channels.NewReversi)

	// 6. Chat WebSocket channels (Phase 9.8): chatRoom と chatUser を登録する
	chatPublisher := stream.NewChatPublisher(streamPubSub)
	chatService := corechat.NewService(chatRepo, idGen)
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
	// 1-on-1 DM で recipient が sender を block している場合に弾く
	// (upstream YOU_HAVE_BEEN_BLOCKED 互換)。
	chatService.SetBlockingRepo(blockingRepo)
	// React で custom emoji (:name:) reaction の存在検証に使う (#1541)。
	chatService.SetEmojiRepo(emojiRepo)
	// 1-on-1 DM の chatApproval bypass + 送信後 approval 挿入に使う (#1748)。
	chatService.SetApprovalRepo(repository.NewChatApprovalRepository(s.db))
	chatRoomFactory := channels.NewChatRoomFactory(chatService)
	streamRegistry.RegisterCredentialed("chatRoom", chatRoomFactory.New)
	streamRegistry.RegisterCredentialed("chatUser", channels.NewChatUserFactory(chatService).New)
	// Phase 9.7: federation processor / reversi handler に reversi 依存を注入。
	// FederationIDCache は本家 Misskey DB スキーマ互換のため DB カラムを持たず
	// Redis のみで session↔gameID の双方向 mapping を保持する。api handler と
	// federation inbox で同じインスタンスを共有する。
	reversiFedCache := corereversi.NewFederationIDCache(s.redis.Default)
	federationProcessor.SetReversi(reversiService, reversiRepo, idGen, reversiFedCache)
	federationProcessor.SetBlockingService(blockingService)
	federationProcessor.SetAbuseReportRepo(repository.NewAbuseReportRepository(s.db), idGen)
	// リモートからの通報 (AP Flag) もモデレーターの通知欄に出す (#2868)。
	// **配線しないと通報の出どころで通知の有無が変わる。**
	federationProcessor.SetAbuseReportNotification(roleService, notificationService)
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

	// Charts API endpoints (engines + hooks already wired earlier).
	// フロントエンドは `misskeyApiGet` (GET) でチャートを取得し、
	// `misskeyApi` (POST) も使う場面があるので両方受ける。POST のみ登録だと
	// GET が `api.Any("/*")` の catchall (= 200 + 空オブジェクト) に落ちて、
	// 受信側で `chart.pubActive[0]` 等が `undefined` 例外を起こす (#421)。
	chartsHandler := apicharts.NewHandler(chartCharts)
	// /retention は chart engine ではなく集計済みテーブルを読む (#2791 で
	// router.go の inline closure から移設)。
	chartsHandler.SetRetentionRepo(retentionRepo)
	chartMethods := []string{http.MethodGet, http.MethodPost}
	api.Match(chartMethods, "/charts/notes", chartsHandler.Notes)
	api.Match(chartMethods, "/charts/users", chartsHandler.Users)
	api.Match(chartMethods, "/charts/drive", chartsHandler.Drive)
	api.Match(chartMethods, "/charts/federation", chartsHandler.Federation)
	api.Match(chartMethods, "/charts/instance", chartsHandler.Instance)
	api.Match(chartMethods, "/charts/ap-request", chartsHandler.ApRequest)
	api.Match(chartMethods, "/charts/active-users", chartsHandler.ActiveUsers)
	api.Match(chartMethods, "/charts/user/notes", chartsHandler.UserNotes)
	api.Match(chartMethods, "/charts/user/drive", chartsHandler.UserDrive)
	api.Match(chartMethods, "/charts/user/following", chartsHandler.UserFollowing)
	api.Match(chartMethods, "/charts/user/pv", chartsHandler.UserPv)
	api.Match(chartMethods, "/charts/user/reactions", chartsHandler.UserReactions)

	// Following endpoints
	followingHandler := following.NewHandler(followingService, userService)
	followingHandler.SetIDGen(idGen)
	followingHandler.SetRelationRepos(listRelationRepos)
	api.POST("/following/create", followingHandler.Create, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:following"))
	api.POST("/following/delete", followingHandler.Delete, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/list", followingHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:following"))
	api.POST("/following/requests/list", followingHandler.ListRequests, middleware.RequireAuth(), middleware.RequireScope("read:following"))
	api.POST("/following/requests/accept", followingHandler.AcceptRequest, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/requests/reject", followingHandler.RejectRequest, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/requests/cancel", followingHandler.CancelRequest, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	// following 残り
	api.POST("/following/invalidate", followingHandler.Invalidate, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/update", followingHandler.UpdateFollow, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/update-all", followingHandler.UpdateFollowAll, middleware.RequireAuth(), middleware.RequireScope("write:following"))
	api.POST("/following/requests/sent", followingHandler.RequestsSent, middleware.RequireAuth(), middleware.RequireScope("read:following"))
	// channels 残り
	api.POST("/channels/favorite", channelsHandler.Favorite, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/unfavorite", channelsHandler.Unfavorite, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/mute/create", channelsHandler.MuteCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/mute/delete", channelsHandler.MuteDelete, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:channels"))
	api.POST("/channels/my-favorites", channelsHandler.MyFavorites, middleware.RequireAuth(), middleware.RequireScope("read:channels"))
	api.POST("/channels/mute/list", channelsHandler.MuteList, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("read:channels"))
	// clips 残り
	api.POST("/clips/favorite", clipsHandler.Favorite, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:clip-favorite"))
	api.POST("/clips/unfavorite", clipsHandler.Unfavorite, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:clip-favorite"))
	api.POST("/clips/my-favorites", clipsHandler.MyFavorites, middleware.RequireAuth(), middleware.RequireScope("read:clip-favorite"))
	// federation の残ルートは上で登録済 (federationHandler 初期化直後)

	// Public roles (Phase 6)
	rolesHandler := apiroles.NewHandler(roleService, idGen)
	rolesHandler.SetNotesQuery(repository.NewRoleNotesQuery(s.db))
	rolesHandler.SetNoteRepo(noteRepo) // #1630: notes の renote 入れ子 mute/block 検査
	rolesHandler.SetMetaRepo(metaRepo) // roles/notes の blocked-host filter
	rolesHandler.SetInstanceRepo(instanceRepo)
	rolesHandler.SetEmojiRepo(emojiRepo)
	rolesHandler.SetReactionReader(reactionCountWriter)
	rolesHandler.SetNoteFieldResolver(noteFieldResolver)
	rolesHandler.SetUserRepo(userRepo)
	rolesHandler.SetMuteBlockRepos(mutingRepo, blockingRepo, channelMutingRepo) // #1544: Notes の mute/block/channel-mute filter
	rolesHandler.SetRelationRepos(listRelationRepos)                            // #1973: roles/users の embed user に viewer-relation
	api.POST("/roles/list", rolesHandler.List, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/roles/show", rolesHandler.Show)
	api.POST("/roles/assignment-show", rolesHandler.AssignmentShow, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/roles/users", rolesHandler.Users)
	api.POST("/roles/notes", rolesHandler.Notes, middleware.RequireAuth(), middleware.RequireScope("read:account"))

	// User lists (Phase 6)
	userListHandler := apiuserlists.NewHandler(userListRepo, idGen)
	userListHandler.SetRolePolicyProvider(roleService)                                         // #1029: userListLimit / userEachUserListsLimit
	userListHandler.SetUserRepo(userRepo)                                                      // #1550: push NO_SUCH_USER check
	userListHandler.SetBlockingRepo(blockingRepo)                                              // #1550: push YOU_HAVE_BEEN_BLOCKED check
	userListHandler.SetFavoriteRepo(userListFavoriteRepo)                                      // #1550: show forPublic likedCount/isLiked
	userListHandler.SetProxyFollow(listProxyFollow)                                            // #1704: push remote member proxy follow
	userListHandler.SetUserListEventPublisher(stream.NewUserListStreamPublisher(streamPubSub)) // #1549: push/pull userAdded/userRemoved
	// users/lists/update-membership の withReplies 変更を userUpdated として流し、接続中の
	// userList channel snapshot を live 更新する (#2051)。
	usersHandler.SetUserListEventPublisher(stream.NewUserListStreamPublisher(streamPubSub))
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

	// Announcements (Phase 6)
	announcementRepo := repository.NewAnnouncementRepository(s.db)

	// Moderator-activity check (#1563): scheduler の cron (30 * * * *) が enqueue
	// する。全 moderator が 7 日非アクティブだと登録を招待制へ自動切替し、残 2 日で
	// 6h ごとに警告メール / announcement / SystemWebhook を送る。announcementRepo /
	// webhookService がここで揃うため本箇所で配線する。announcement は repo 直接
	// ではなく create + announcementCreated publish を対で行う Creator 経由
	// (#1606、本家 AnnouncementService.create 相当)。
	modActivitySvc := coremoderatoractivity.NewService(
		roleService,
		metaRepo,
		userRepo,
		coreannouncement.NewCreator(announcementRepo, idGen, mainStreamPublisher, broadcastPublisher),
		webhookService,
		idGen,
		miscsmtp.SubjectBodySenderFromMeta(metaRepo, s.config.ProxySMTP),
	)
	s.queueServer.Handle(queue.TaskTypeCheckModeratorsActivity,
		processors.NewCheckModeratorsActivityProcessor(modActivitySvc).Handle)

	// Phase 7-2 (#244): /api/i の hasUnreadAnnouncement / unreadAnnouncements 配線
	iHandler.SetAnnouncementRepo(announcementRepo)
	// self を MeDetailed へ昇格させる他 handler (users / hashtags 等) が、未読 /
	// policies / pinned を /api/i と同じ source で埋められるようにする。iHandler は
	// これらの依存を全て持っているので、Enricher として 1 度だけ登録する。
	// SetAnnouncementRepo などの依存配線が済んだ後に呼ぶこと。
	meself.SetEnricher(iHandler)
	announcementHandler := apiannouncements.NewHandler(announcementRepo, idGen)
	announcementHandler.SetMainStreamPublisher(mainStreamPublisher)
	announcementHandler.SetBroadcastPublisher(broadcastPublisher) // global announcementCreated (#2056)
	api.POST("/announcements", announcementHandler.List)
	api.POST("/i/read-announcement", announcementHandler.ReadAnnouncement, middleware.RequireAuth(), middleware.RequireScope("write:account"))

	// Admin endpoints (Phase 5)
	abuseReportRepo := repository.NewAbuseReportRepository(s.db)
	modLogRepo := repository.NewModerationLogRepository(s.db)
	recipientRepo := repository.NewAbuseReportNotificationRecipientRepository(s.db)
	// users/report-abuse 時に abuseReport system webhook を発火する (#1542)。
	// recipientRepo がここで揃うため本箇所で配線する。
	usersHandler.SetAbuseReportWebhook(webhookService, recipientRepo)
	adminHandler := apiadmin.NewHandler(signupService, roleService, metaRepo, userRepo, idGen)
	// モデレーターの suspend / unsuspend を local 由来として刻む (#2973)。
	adminHandler.SetSuspensionOriginRepo(suspensionOriginRepo)
	// catalog 更新を entity 側 packer に即時反映する (#2258)。TTL 任せだと
	// 作成直後に装着された decoration が lookup miss で silent drop される。
	adminHandler.SetAvatarDecorationInvalidator(avatarDecorationResolver)
	adminHandler.SetEmojiDecorationInvalidator(emojiDecorationResolver)
	// admin/ad/* の変更を /api/meta の response cache に反映する (#2649)。
	adminHandler.SetMetaResponseInvalidator(metaHandler)
	// admin/suspend-user / admin/unsuspend-user / admin/accounts/delete が
	// target user の全 token cache entry を即時 invalidate するために、
	// auth middleware を inject する (#965)。未配線時は 30 秒 cache TTL 待ち
	// で stale な user が auth 通過する security regression が残るので
	// production では必ず wire する。i/regenerate-token (#884) と同じく
	// AuthMiddleware が duck-typed で UserTokenInvalidator interface を
	// 満たしている。
	adminHandler.SetUserTokenInvalidator(s.auth)
	// 凍結・削除した利用者の WebSocket を閉じる (tokenCache の失効とは独立)。
	adminHandler.SetUserStreamRevoker(streamRevokePublisher)
	adminHandler.SetInstanceRepo(instanceRepo)
	adminHandler.SetDeliveryHealthProvider(deliveryHealth)
	adminHandler.SetInboxHealthProvider(inboxHealth)
	// IP からアカウントを引く口 (#3104)。**この行を落とすと admin/ip/accounts が
	// 500 を返す** — 空の結果は「その IP を使ったアカウントは無い」という誤った
	// 事実になり、調査の結論を反転させる。
	adminHandler.SetIPSearchRepo(repository.NewUserIPSearchRepository(s.db))
	// IP 照会の監査 (#3106)。**未配線だと照会が記録されないまま通る** — 照会は
	// 成立するので誰も気付けない。critical wiring 検査で落とす。
	ipLookupLogRepo := repository.NewIPLookupLogRepository(s.db)
	adminHandler.SetIPLookupAudit(iplookuplog.NewService(ipLookupLogRepo, idGen))
	adminHandler.SetIPLookupLogRepo(ipLookupLogRepo)
	// ドライブ使用量の集計 (#3053)。**この行を落とすと admin/drive/usage が
	// 500 を返す** — 0 バイトを返して「使っていない」と誤認させるよりよい。
	adminHandler.SetDriveUsageProvider(driveusage.NewService(
		repository.NewDriveUsageRepository(s.db), driveusage.DefaultTTL, driveusage.DefaultTopN))
	adminHandler.SetSignupApplicationReviewer(signupApplicationService)
	// カスタム絵文字の登録申請 (#2934)。承認までは emoji 行を作らず、
	// 専用テーブルに閉じ込める (signup_application と同じ形)。
	//
	// **承認時の emoji 生成は adminHandler が行う。** service に持たせると
	// admin/emoji/add と別経路になり、MIME の allowlist などの検証が申請
	// だけ抜ける。ここでは依存を逆向きに渡すだけ。
	// 通知の lookup が先に作った実体を使い回す (2 つ作らない)。
	emojiApplicationRepo := emojiApplicationRepoForNotif
	emojiApplicationService := emojiapplication.NewService(
		emojiApplicationRepo,
		emojiRepo,
		driveFileRepo,
		idGen,
		adminHandler,
		emojiapplication.NewResultNotifier(notificationService),
	)
	// 期間上限をロールから引く (#2958)。未配線だと上限が丸ごと効かなくなる
	// ので criticalWiring に載せてある。
	emojiApplicationService.SetPolicyProvider(roleService)
	// 申請が出されたことを**審査できる人**へ知らせる (#2987)。モデレーターでは
	// なく `canManageCustomEmojis` を持つ人 — 審査 endpoint の gate と揃える。
	emojiApplicationService.SetReceivedNotifier(
		emojiapplication.NewReceivedNotifier(notificationService, roleService))
	// 申請枠の手動リセット (#2962)。**未配線ならリセットは 500 で失敗する**
	// (`ErrQuotaResetUnavailable`) ので、戻したつもりで戻っていない状態にはならない。
	// ただし既存のリセットも読めなくなる = 過去に戻した枠が再び満杯に見えるので、
	// 配線の有無は静的ゲート (`emoji_application_gate_test.go`) と**起動時の
	// 自己診断 (`criticalWiring`) の両方**で見る。**片方では足りない** — 静的
	// ゲートは `nil` リテラルしか見ないので変数経由で渡す形を素通りし、自己診断は
	// 呼び出しを消した構成を起動するまで気付けない。
	emojiApplicationService.SetQuotaResetRepo(
		repository.NewEmojiApplicationQuotaResetRepository(s.db))
	emojiApplicationHandler := apiemojiapplications.NewHandler(
		emojiApplicationService, emojiApplicationRepo, driveFileRepo)
	// 「自分の申請」の画像プレビュー (#2989)。承認済みは `emojiId`、未承認の
	// リモート申請は `remoteHost + remoteName` で引く。**未配線だと preview が
	// `unknown` になる** — 「消えた」と断定しないので害は「理由が出ない」だけ。
	emojiApplicationHandler.SetEmojiLookup(emojiRepo)
	// **申請できる人をロールで絞る (#2934)。** canManageCustomEmojis を持つ人は
	// 申請ではなく直接登録できるので、この policy は「登録はできないが頼める人」。
	//
	// **第三者アプリのトークンは入れない (#3037)。** この 3 つは mk-go 独自
	// なので upstream の `kind` にあたる宣言が無く、`RequireScope` を配線
	// できない。upstream は「`kind` が無く、かつ資格情報を要する endpoint」に
	// 対し app token を一律で拒否する (`ApiCallService.ts:412-413`) ので、
	// 同じ側に倒す。本体の他の kind 無し endpoint は `RequireSecure` で
	// もっと強く塞がれており、抜けていたのはここだけ。
	api.POST("/emoji-application/create", emojiApplicationHandler.Create,
		middleware.RequireAuth(),
		middleware.RejectAppToken(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanRequestCustomEmojis))
	// **一覧と取り下げは policy で塞がない。** 後から policy を外された人が
	// 自分の申請を確認することも取り下げることもできなくなる。
	api.POST("/emoji-application/list-mine", emojiApplicationHandler.ListMine, middleware.RequireAuth(), middleware.RejectAppToken())
	api.POST("/emoji-application/cancel", emojiApplicationHandler.Cancel, middleware.RequireAuth(), middleware.RejectAppToken())
	adminHandler.SetEmojiApplicationReviewer(emojiApplicationService)
	adminHandler.SetEmojiApplicationRepo(emojiApplicationRepo)
	// 連合セルフ診断 (#2463)。migration 本数は起動時に数えず 0 を渡す
	// (server 側は既に migrate 済みで動いている前提。適用漏れの検出は
	// `misskey -doctor` の担当で、あちらは同梱ファイルを数えられる)。
	adminHandler.SetSelfCheckRunner(&selfCheckAdapter{
		checker: selfcheck.NewChecker(s.config.URL),
		deps:    selfcheck.LocalDeps{DB: s.db, Redis: s.redis.Default},
	})
	// admin/federation/update-instance の suspend / unsuspend を deliver hot path
	// の suspend 判定 cache へ TTL を待たず即時反映する (#1407 review)。
	adminHandler.SetInstanceSuspendCacheInvalidator(instanceService)
	// admin/show-user の signins field を実データで埋める (#1198)。signinRepo
	// は signin handler 配線時に既に構築済 (line ~1030)。
	adminHandler.SetSigninRepo(signinRepo)
	adminHandler.SetAbuseRepo(abuseReportRepo)
	adminHandler.SetAbuseForwarder(coreabuse.NewForwarder(abuseReportRepo, sysAcctSvc, apRenderer, deliverService))
	// suspend / delete-account / unsuspend 時に対象 local user の AP Delete /
	// Undo(Delete) を remote instances へ配信する (#1759)。
	adminHandler.SetUserModerationFederationHook(corefederation.NewUserModerationDeliveryHook(deliverService, apRenderer, userRepo))
	adminHandler.SetDeleteAccountEnqueuer(s.queueClient)
	adminHandler.SetServerURL(s.config.URL)
	adminHandler.SetConfigSetupPassword(s.config.SetupPassword)
	adminHandler.SetSMTPProxyURL(s.config.ProxySMTP)
	modLogService := coremodlog.New(modLogRepo, idGen)
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
	// admin/emoji/copy で remote 画像を local drive に保存するための fetcher
	// を wire する (#670)。outboundClient 経由なので SSRF / proxy / outgoing
	// address が他の outbound 経路と同じ設定で適用される。
	adminHandler.SetEmojiImageFetcher(apiadmin.NewEmojiImageFetcher(s.outboundClient(10*time.Second), driveService, s.config.UserAgent))
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
	adminHandler.SetAdRepo(repository.NewAdRepository(s.db))
	adminHandler.SetAvatarDecorationRepo(repository.NewAvatarDecorationRepository(s.db))
	adminHandler.SetInviteRepo(repository.NewRegistrationTicketRepository(s.db))
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
	api.POST("/admin/accounts/create", adminHandler.AccountsCreate)
	api.POST("/admin/show-user", adminHandler.ShowUser, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:show-user"))
	api.POST("/admin/show-users", adminHandler.ShowUsers, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:show-user"))
	api.POST("/admin/suspend-user", adminHandler.SuspendUser, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:suspend-user"))
	api.POST("/admin/unsuspend-user", adminHandler.UnsuspendUser, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:unsuspend-user"))
	api.POST("/admin/meta", adminHandler.AdminMeta, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:meta"))
	// mk-go 独自 (additive): 組み込み済みサーバープラグインの一覧 (#2497)。
	// プラグインの管理画面 (/admin/plugin/*) と権限を揃えてモデレーター以上。
	api.POST("/admin/server-plugins", adminHandler.ServerPlugins, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:meta"))
	api.POST("/admin/update-meta", adminHandler.UpdateMeta, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:meta"))
	api.POST("/admin/roles/create", adminHandler.RolesCreate, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/show", adminHandler.RolesShow, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:roles"))
	api.POST("/admin/roles/list", adminHandler.RolesList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:roles"))
	api.POST("/admin/roles/update", adminHandler.RolesUpdate, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/delete", adminHandler.RolesDelete, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/assign", adminHandler.RolesAssign, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/unassign", adminHandler.RolesUnassign, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/roles/assignment-show", adminHandler.RolesAssignmentShow, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:roles"))
	api.POST("/admin/roles/users", adminHandler.RolesUsers, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:roles"))
	api.POST("/admin/roles/update-default-policies", adminHandler.RolesUpdateDefaultPolicies, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:roles"))
	api.POST("/admin/abuse-user-reports", adminHandler.AbuseReports, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:abuse-user-reports"))
	api.POST("/admin/resolve-abuse-user-report", adminHandler.ResolveAbuseReport, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:resolve-abuse-user-report"))
	api.POST("/admin/show-moderation-logs", adminHandler.ShowModerationLogs, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:show-moderation-log"))
	// admin/emoji/* と admin/avatar-decorations/* は upstream Misskey TS の
	// ApiCallService.ts が requiredRolePolicy (canManageCustomEmojis /
	// canManageAvatarDecorations) のみで gate する (requireModerator/Admin
	// flag は設定されていない) ため、mk-go も RequireRolePolicy に揃える
	// (#1025)。admin role 持ちは HasRolePolicy 内で常に true → 自動 bypass、
	// それ以外は role で明示的に policy=true 必要。
	api.POST("/admin/emoji/add", adminHandler.EmojiAdd, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/update", adminHandler.EmojiUpdate, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/delete", adminHandler.EmojiDelete, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/list", adminHandler.EmojiList, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("read:admin:emoji"))
	api.POST("/admin/announcements/create", announcementHandler.AdminCreate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:announcements"))
	api.POST("/admin/announcements/update", announcementHandler.AdminUpdate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:announcements"))
	api.POST("/admin/announcements/delete", announcementHandler.AdminDelete, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:announcements"))
	api.POST("/admin/announcements/list", announcementHandler.AdminList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:announcements"))

	// Phase 7.5: admin/* 残りエンドポイント (ハンドラメソッド化)
	api.POST("/admin/delete-account", adminHandler.DeleteAccount, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:delete-account"))
	api.POST("/admin/delete-all-files-of-a-user", adminHandler.DeleteAllFilesOfUser, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:delete-all-files-of-a-user"))
	api.POST("/admin/reset-password", adminHandler.ResetPassword, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:reset-password"))
	api.POST("/admin/unset-mfa", adminHandler.UnsetMfa, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:unset-mfa"))
	api.POST("/admin/send-email", adminHandler.SendEmail, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:send-email"))
	api.POST("/admin/unset-user-avatar", adminHandler.UnsetUserAvatar, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:unset-user-avatar"))
	api.POST("/admin/unset-user-banner", adminHandler.UnsetUserBanner, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:unset-user-banner"))
	api.POST("/admin/update-user-note", adminHandler.UpdateUserNote, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:user-note"))
	api.POST("/admin/update-proxy-account", adminHandler.UpdateProxyAccount, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:account"))
	api.POST("/admin/forward-abuse-user-report", adminHandler.ForwardAbuseUserReport, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:resolve-abuse-user-report"))
	api.POST("/admin/update-abuse-user-report", adminHandler.UpdateAbuseUserReport, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:resolve-abuse-user-report"))
	api.POST("/admin/accounts/delete", adminHandler.AccountsDelete, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:account"))
	api.POST("/admin/accounts/find-by-email", adminHandler.AccountsFindByEmail, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:account"))
	api.POST("/admin/get-user-ips", adminHandler.GetUserIPs, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:user-ips"))
	// mk-go 独自 (#3104、親 #3066)。IP からローカルアカウントを引く。
	//
	// **`RequireModerator` と policy を併用する。** upstream の `admin/get-user-ips`
	// (上の行) は `requireAdmin` なので、同じ「利用者 ↔ IP の対応」に対して
	// モデレーターだけで開くと既存より緩い経路を新設することになる。policy の
	// 既定は false で、admin は policy を bypass するため、**既定の挙動は
	// upstream と同じ「管理者のみ」**になる。モデレーターへ開きたい運営者が
	// ロールで有効にする。scope は upstream の口と同じものを再利用する
	// (`internal/misc/permissions` は misskey-js と完全一致させる契約)。
	api.POST("/admin/ip/accounts", adminHandler.IPAccounts,
		middleware.RequireModerator(roleService),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory),
		middleware.RequireScope("read:admin:user-ips"))
	// 関連アカウント候補 (#3105)。**同じ policy と scope を再利用する** —
	// 扱うのは同じ「利用者 ↔ IP の対応」で、別 policy にすると片方だけ開けてしまう。
	api.POST("/admin/ip/related-accounts", adminHandler.IPRelatedAccounts,
		middleware.RequireModerator(roleService),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory),
		middleware.RequireScope("read:admin:user-ips"))
	// 照会の監査記録 (#3106)。**この応答自体が機密** — 照会に使った IP がそのまま
	// 入るので、照会と同じ 3 段で守る。
	api.POST("/admin/ip/lookup-log", adminHandler.IPLookupLog,
		middleware.RequireModerator(roleService),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanSearchIPHistory),
		middleware.RequireScope("read:admin:user-ips"))
	api.POST("/admin/get-index-stats", adminHandler.GetIndexStats, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:index-stats"))
	api.POST("/admin/get-table-stats", adminHandler.GetTableStats, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:table-stats"))
	api.POST("/admin/server-info", adminHandler.ServerInfo, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	// mk-go 独自 (#2395)。upstream に対応する endpoint は無いので scope も
	// server-info のものを流用する (admin UI 以外の consumer を想定しない)。
	api.POST("/admin/server-metrics", adminHandler.ServerMetrics, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	api.POST("/admin/captcha/current", adminHandler.CaptchaCurrent, middleware.RequireAdmin(roleService), middleware.RequireScope("read:admin:meta"))
	api.POST("/admin/captcha/save", adminHandler.CaptchaSave, middleware.RequireAdmin(roleService), middleware.RequireScope("write:admin:meta"))
	api.POST("/admin/ad/create", adminHandler.AdCreate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:ad"))
	api.POST("/admin/ad/delete", adminHandler.AdDelete, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:ad"))
	api.POST("/admin/ad/list", adminHandler.AdList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:ad"))
	api.POST("/admin/ad/update", adminHandler.AdUpdate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:ad"))
	api.POST("/admin/avatar-decorations/create", adminHandler.AvatarDecorationsCreate, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageAvatarDecorations), middleware.RequireScope("write:admin:avatar-decorations"))
	api.POST("/admin/avatar-decorations/delete", adminHandler.AvatarDecorationsDelete, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageAvatarDecorations), middleware.RequireScope("write:admin:avatar-decorations"))
	api.POST("/admin/avatar-decorations/list", adminHandler.AvatarDecorationsList, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageAvatarDecorations), middleware.RequireScope("read:admin:avatar-decorations"))
	api.POST("/admin/avatar-decorations/update", adminHandler.AvatarDecorationsUpdate, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageAvatarDecorations), middleware.RequireScope("write:admin:avatar-decorations"))
	api.POST("/admin/drive/clean-remote-files", adminHandler.DriveCleanRemoteFiles, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:drive"))
	api.POST("/admin/drive/cleanup", adminHandler.DriveCleanup, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:drive"))
	api.POST("/admin/drive/files", adminHandler.DriveFiles, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:drive"))
	api.POST("/admin/drive/show-file", adminHandler.DriveShowFile, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:drive"))
	// mk-go 独自 (#3053)。インスタンス全体のドライブ使用量と内訳。upstream は
	// per-user の `driveCapacityMb` しか持たず、合計を出す口が無い。
	api.POST("/admin/drive/usage", adminHandler.DriveUsage, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:drive"))
	api.POST("/admin/emoji/add-aliases-bulk", adminHandler.EmojiAddAliasesBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/copy", adminHandler.EmojiCopy, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	// mk-go 独自 (#2698)。リモート絵文字のインポート時に、AP では運ばれない
	// カテゴリ・エイリアス・センシティブを相手の REST API から取ってくる。
	// **作成はしない**ので read 系の scope で足りるが、絵文字管理権限に揃える
	// (取得先を相手に決められる = 外向き通信を起こす操作なので、誰でも叩けると
	// 探索に使える)。
	api.POST("/admin/emoji/fetch-remote-meta", adminHandler.EmojiFetchRemoteMeta, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("read:admin:emoji"))
	api.POST("/admin/emoji/delete-bulk", adminHandler.EmojiDeleteBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	// upstream 2026.9.0 は emoji import zip の堅牢化 (GHSA 経由) と同時に
	// requiredRolePolicy:'canManageCustomEmojis' から requireAdmin へ変えた。
	// fork frontend も iAmAdmin でインポート UI を出し分けるので backend も揃える。
	api.POST("/admin/emoji/import-zip", adminHandler.EmojiImportZip, middleware.RequireAdmin(roleService), middleware.RequireSecure())
	api.POST("/admin/emoji/list-remote", adminHandler.EmojiListRemote, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("read:admin:emoji"))
	// 絵文字の登録申請の審査 (#2934)。canManageCustomEmojis で gate する —
	// 承認は実質 admin/emoji/add と同じ操作なので、同じ権限を要求する。
	api.POST("/admin/emoji-application/list", adminHandler.EmojiApplicationList,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("read:admin:emoji"))
	api.POST("/admin/emoji-application/approve", adminHandler.EmojiApplicationApprove,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji-application/reject", adminHandler.EmojiApplicationReject,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("write:admin:emoji"))
	// 関連する過去の申請 (#2960)。**読むだけなので read scope**。
	api.POST("/admin/emoji-application/related", adminHandler.EmojiApplicationRelated,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("read:admin:emoji"))
	// ユーザーモデレーション画面の申請履歴と集計 (#2961)。**却下理由と
	// モデレーターへの補足が載るので、一般ユーザー向けには公開しない。**
	api.POST("/admin/emoji-application/list-by-user", adminHandler.EmojiApplicationListByUser,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("read:admin:emoji"))
	api.POST("/admin/emoji-application/user-summary", adminHandler.EmojiApplicationUserSummary,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("read:admin:emoji"))
	// 申請枠の手動リセット (#2962)。**書き込みなので write scope**。
	api.POST("/admin/emoji-application/reset-user-quota", adminHandler.EmojiApplicationResetUserQuota,
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis),
		middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/remove-aliases-bulk", adminHandler.EmojiRemoveAliasesBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/set-aliases-bulk", adminHandler.EmojiSetAliasesBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/set-category-bulk", adminHandler.EmojiSetCategoryBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/emoji/set-license-bulk", adminHandler.EmojiSetLicenseBulk, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("write:admin:emoji"))
	api.POST("/admin/federation/delete-all-files", adminHandler.FederationDeleteAllFiles, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	api.POST("/admin/federation/refresh-remote-instance-metadata", adminHandler.FederationRefreshRemoteInstanceMetadata, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	api.POST("/admin/federation/remove-all-following", adminHandler.FederationRemoveAllFollowing, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	api.POST("/admin/federation/update-instance", adminHandler.FederationUpdateInstance, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:federation"))
	// mk-go 独自 (#2461)。scope は **既存のものを再利用する**。
	// `internal/misc/permissions` は upstream misskey-js の一覧と完全一致させる
	// 契約で、RFC8414 の scopes_supported にもそのまま出るので、mk-go 固有の
	// scope を足せない。`admin/federation/*` に read 系の scope が無いため、
	// 同じく mk-go 独自の read endpoint である admin/server-metrics に倣って
	// read:admin:server-info を要求する。
	// mk-go 独自 (#2463)。scope は delivery-health と同じ理由で既存を再利用する。
	api.POST("/admin/self-check", adminHandler.SelfCheck, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	// mk-go 独自 (#2471)。送信側と同じ scope を再利用する。
	api.POST("/admin/federation/inbox-health", adminHandler.FederationInboxHealth, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	api.POST("/admin/federation/delivery-health", adminHandler.FederationDeliveryHealth, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:server-info"))
	api.POST("/admin/invite/create", adminHandler.InviteCreate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:invite-codes"))
	api.POST("/admin/invite/list", adminHandler.InviteList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:invite-codes"))
	// 承認制の登録の審査 (#2555)。mk-go 独自。scope は invite-codes を再利用する
	// — 承認はメール確認の経路で registration_ticket の発行につながるので管轄が同じで、
	// internal/misc/permissions は upstream misskey-js と完全一致させる契約が
	// あり mk-go 固有 scope を足せない。
	api.POST("/admin/signup-application/list", adminHandler.SignupApplicationList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:invite-codes"))
	api.POST("/admin/signup-application/approve", adminHandler.SignupApplicationApprove, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:invite-codes"))
	api.POST("/admin/signup-application/reject", adminHandler.SignupApplicationReject, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:invite-codes"))
	api.POST("/admin/promo/create", adminHandler.PromoCreate, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:promo"))
	api.POST("/admin/relays/add", adminHandler.RelaysAdd, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:relays"))
	api.POST("/admin/relays/list", adminHandler.RelaysList, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:relays"))
	api.POST("/admin/relays/remove", adminHandler.RelaysRemove, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:relays"))
	api.POST("/admin/system-webhook/create", adminHandler.SystemWebhookCreate, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/system-webhook/delete", adminHandler.SystemWebhookDelete, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/system-webhook/list", adminHandler.SystemWebhookList, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/system-webhook/show", adminHandler.SystemWebhookShow, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/system-webhook/test", adminHandler.SystemWebhookTest, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("read:admin:system-webhook"))
	api.POST("/admin/system-webhook/update", adminHandler.SystemWebhookUpdate, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:system-webhook"))
	api.POST("/admin/abuse-report/notification-recipient/create", adminHandler.AbuseReportNotificationRecipientCreate, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:abuse-report:notification-recipient"))
	api.POST("/admin/abuse-report/notification-recipient/delete", adminHandler.AbuseReportNotificationRecipientDelete, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:abuse-report:notification-recipient"))
	api.POST("/admin/abuse-report/notification-recipient/list", adminHandler.AbuseReportNotificationRecipientList, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("read:admin:abuse-report:notification-recipient"))
	api.POST("/admin/abuse-report/notification-recipient/show", adminHandler.AbuseReportNotificationRecipientShow, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("read:admin:abuse-report:notification-recipient"))
	api.POST("/admin/abuse-report/notification-recipient/update", adminHandler.AbuseReportNotificationRecipientUpdate, middleware.RequireModerator(roleService), middleware.RequireSecure(), middleware.RequireScope("write:admin:abuse-report:notification-recipient"))
	api.POST("/admin/queue/clear", adminHandler.QueueClear, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))
	api.POST("/admin/queue/deliver-delayed", adminHandler.QueueDeliverDelayed, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/inbox-delayed", adminHandler.QueueInboxDelayed, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/jobs", adminHandler.QueueJobs, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/promote-jobs", adminHandler.QueuePromoteJobs, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))
	api.POST("/admin/queue/pause", adminHandler.QueuePause, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))   // #2069 #17436
	api.POST("/admin/queue/resume", adminHandler.QueueResume, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue")) // #2069 #17436
	api.POST("/admin/queue/queue-stats", adminHandler.QueueQueueStats, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/queues", adminHandler.QueueQueues, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/remove-job", adminHandler.QueueRemoveJob, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))
	api.POST("/admin/queue/retry-job", adminHandler.QueueRetryJob, middleware.RequireModerator(roleService), middleware.RequireScope("write:admin:queue"))
	api.POST("/admin/queue/show-job", adminHandler.QueueShowJob, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/show-job-logs", adminHandler.QueueShowJobLogs, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))
	api.POST("/admin/queue/stats", adminHandler.QueueStats, middleware.RequireModerator(roleService), middleware.RequireScope("read:admin:queue"))

	// --- Phase 7.6b: chat/*, auth/*, ap/*, sw/*, reversi/*, bubble-game/*, misc ---

	// announcements/show — 個別アナウンスメント取得
	api.POST("/announcements/show", announcementHandler.Show)

	// auth/* — MiAuth/OAuth セッション
	authSessionRepo := repository.NewAuthSessionRepository(s.db)
	authHandler := apiauth.NewHandler(authSessionRepo, s.config, idGen)
	api.POST("/auth/session/generate", authHandler.SessionGenerate)
	api.POST("/auth/session/show", authHandler.SessionShow)
	api.POST("/auth/session/userkey", authHandler.SessionUserkey)
	api.POST("/auth/accept", authHandler.Accept, middleware.RequireAuth(), middleware.RequireSecure())

	// miauth/gen-token — アクセストークン直接生成
	api.POST("/miauth/gen-token", authHandler.GenToken, middleware.RequireAuth(), middleware.RequireSecure())
	// miauth/:session/check — 3rd-party client が session 紐付き token を取得する
	// (#1224)。token をまだ持たない client が叩くため RequireAuth は付けない。
	authHandler.SetUserRepo(userRepo)
	authHandler.SetTokenNotifier(notificationHook)
	api.POST("/miauth/:session/check", authHandler.MiAuthCheck)

	// app/* — アプリ管理API
	appHandler := apiapp.NewHandler(authSessionRepo, idGen)
	api.POST("/app/create", appHandler.Create)
	api.POST("/app/show", appHandler.Show)
	api.POST("/my/apps", appHandler.MyApps, middleware.RequireAuth(), middleware.RequireScope("read:account"))

	// ap/* — ActivityPub API lookup (実データ: ローカルオブジェクト解決)
	// /api/ap/notes は upstream Misskey にも存在しない vestigial route だった
	// ため #587 で削除済 (旧実装は常に空配列返却の stub)。
	api.POST("/ap/get", apHandler.APIGet, middleware.RequireAdmin(roleService), middleware.RequireScope("read:federation"))
	api.POST("/ap/show", apHandler.APIShow, middleware.RequireAuth(), middleware.RequireScope("read:account"))

	// sw/* — Service Worker push notifications (実データ)
	swHandler := apisw.NewHandler(swSubRepo, metaRepo, idGen)
	api.POST("/sw/register", swHandler.Register, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/sw/show-registration", swHandler.ShowRegistration, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/sw/update-registration", swHandler.UpdateRegistration, middleware.RequireAuth(), middleware.RequireSecure())
	api.POST("/sw/unregister", swHandler.Unregister)

	// reversi/* — オセロゲーム (実データ)
	reversiHandler := apireversi.NewHandler(reversiRepo, idGen)
	reversiHandler.SetService(reversiService)
	reversiHandler.SetFederation(s.config.URL, deliverService, reversiFedCache, userRepo)
	reversiHandler.SetStreamPublisher(reversiPublisher)
	// #417 P3: reversi 連合対応ホストのみ Invite を送る。Federation check
	// 用の HTTP client は SSRF-safe transport + timeout を明示的に渡す
	// (既存の webfingerClient と同じ設定を流用)。
	reversiHandler.SetFederationChecker(corereversi.NewFederationChecker(
		s.redis.Default,
		s.outboundClient(10*time.Second),
	))
	// #417 P3: /match の acct 引数で未キャッシュのリモートユーザーを
	// WebFinger 経由で取り込めるようにする。ここで webfingerClient /
	// federationResolver は既存の users/show 用と同じインスタンスを再利用。
	reversiHandler.SetRemoteUserLookup(corefederation.NewRemoteUserResolver(
		webfingerClient, federationResolver, userRepo, localHost,
	))
	// Service 側にも federation 一式を注入して state 変化時に Update / Leave を
	// 配信できるようにする (#417 P1)。fedCache も Service 側で
	// session→game 解決に必要なので忘れず設定する。
	reversiService.SetFederationCache(reversiFedCache)
	reversiService.SetFederationDeliverer(deliverService)
	reversiService.SetUserRepo(userRepo)
	reversiService.SetBaseURL(s.config.URL)
	// 連合 inbox 経由の invite 受信時にも local 被招待者の reversi stream に
	// `invited` を push する (#417 P2: リアルタイム招待)。
	federationProcessor.SetReversiStreamPublisher(reversiPublisher)
	api.POST("/reversi/games", reversiHandler.Games)
	api.POST("/reversi/invitations", reversiHandler.Invitations, middleware.RequireAuth(), middleware.RequireScope("read:account"))
	api.POST("/reversi/show-game", reversiHandler.ShowGame)
	api.POST("/reversi/match", reversiHandler.Match, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/reversi/cancel-match", reversiHandler.CancelMatch, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/reversi/surrender", reversiHandler.Surrender, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	api.POST("/reversi/verify", reversiHandler.Verify)

	// bubble-game/* — バブルゲーム (実データ)
	bubbleGameRepo := repository.NewBubbleGameRepository(s.db)
	bubbleGameHandler := apibubblegame.NewHandler(bubbleGameRepo, idGen)
	api.POST("/bubble-game/register", bubbleGameHandler.Register, middleware.RequireAuth(), middleware.RequireScope("write:account"))
	// ranking は upstream ranking.ts が allowGet:true。frontend drop-and-fusion.vue は
	// misskeyApiGet で GET を投げるため GET+POST を二重 wire する (hashtags/trend や
	// fetch-rss と同様、#1774)。
	api.POST("/bubble-game/ranking", bubbleGameHandler.Ranking)
	api.GET("/bubble-game/ranking", bubbleGameHandler.Ranking)

	// chat/* — Misskey v2026 チャット機能 (実データ)
	chatHandler := apichat.NewHandler(chatRepo, idGen)
	chatHandler.SetService(chatService)
	// drive/files/attached-chat-messages: chat message を pack するため chat
	// handler が処理する。file の owner-scope (moderator は任意 file 可) (#1218)。
	chatHandler.SetDriveFileRepo(driveFileRepo)
	chatHandler.SetModeratorChecker(roleService)
	chatHandler.SetUserRepo(userRepo)
	chatHandler.SetChatAvailabilityChecker(roleService) // #1796: recipient の chatAvailability(write) 検査
	chatHandler.SetModLog(modLogService)                // #1541: moderator による chat room 削除の監査ログ
	// chatRoomInvitationReceived 通知 (#1559): 招待作成 (local handler / AP service)
	// で発火し、entity 側で read 時に packed invitation へ解決する。lookup は
	// 招待削除済なら (nil,false)。chatHandler が PackInvitationByID を提供する
	// ため、notification packer (REST handler / stream) への配線はここで行う。
	chatHandler.SetInvitationNotifier(notificationHook)
	chatService.SetInvitationNotifier(notificationHook)
	notificationsHandler.SetChatInvitationLookup(chatHandler.PackInvitationByID)
	notificationPublisher.SetChatInvitationLookup(chatHandler.PackInvitationByID)
	api.POST("/drive/files/attached-chat-messages", chatHandler.AttachedChatMessages, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	// #1796: 全 chat endpoint に chatAvailability role-policy gate (upstream
	// checkChatAvailability(me.id, 'read'|'write')) を付与する。mode は upstream
	// の各 endpoint 呼び出しに一致させる (read = available|readonly、write =
	// available のみ)。RequireScope の scope (read/write:chat) とは一致しない
	// ものがある点に注意 (例: rooms/members は write:chat scope だが read availability)。
	chatRead := func() echo.MiddlewareFunc { return middleware.RequireChatAvailability(roleService, "read") }
	chatWrite := func() echo.MiddlewareFunc { return middleware.RequireChatAvailability(roleService, "write") }
	api.POST("/chat/rooms/create", chatHandler.RoomsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/show", chatHandler.RoomsShow, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/update", chatHandler.RoomsUpdate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/delete", chatHandler.RoomsDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/owned", chatHandler.RoomsOwned, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/joined", chatHandler.RoomsJoined, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/leave", chatHandler.RoomsLeave, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/mute", chatHandler.RoomsMute, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/unmute", chatHandler.RoomsUnmute, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/transfer-ownership", chatHandler.RoomsTransferOwnership, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/join", chatHandler.RoomsJoin, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/joining", chatHandler.RoomsJoining, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/members", chatHandler.RoomsMembers, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatRead())
	api.POST("/chat/rooms/invitations/create", chatHandler.InvitationsCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/delete", chatHandler.InvitationsDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/accept", chatHandler.InvitationsAccept, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/reject", chatHandler.InvitationsReject, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/ignore", chatHandler.InvitationsIgnore, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/invitations/inbox", chatHandler.InvitationsInbox, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/invitations/outbox", chatHandler.InvitationsOutbox, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/rooms/members/ban", chatHandler.MembersBan, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/rooms/members/update-membership", chatHandler.MembersUpdateMembership, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages", chatHandler.Messages, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/create", chatHandler.MessagesCreate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/create-to-user", chatHandler.MessagesCreateToUser, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/create-to-room", chatHandler.MessagesCreateToRoom, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/show", chatHandler.MessagesShow, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/update", chatHandler.MessagesUpdate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/delete", chatHandler.MessagesDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/read", chatHandler.MessagesRead, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatRead())
	api.POST("/chat/messages/search", chatHandler.MessagesSearch, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/user-timeline", chatHandler.UserTimeline, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/room-timeline", chatHandler.RoomTimeline, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/messages/react", chatHandler.ReactionsCreate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/unreact", chatHandler.ReactionsDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/reactions/create", chatHandler.ReactionsCreate, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/messages/reactions/delete", chatHandler.ReactionsDelete, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatWrite())
	api.POST("/chat/history", chatHandler.History, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())
	api.POST("/chat/read-all", chatHandler.ReadAll, middleware.RequireAuth(), middleware.RequireScope("write:chat"), chatRead())
	api.POST("/chat/unread-count", chatHandler.UnreadCount, middleware.RequireAuth(), middleware.RequireScope("read:chat"), chatRead())

	// --- Phase P3: 補助公開エンドポイント ---

	// get-online-users-count — オンラインユーザー数。フロントが
	// `misskeyApiGet` で GET 呼び出ししても catchall に落ちないよう
	// 両メソッドを受ける (#421)。POST のみだと `count` が undefined になり
	// admin overview の「NaN 人」表示の原因になっていた。
	api.GET("/get-online-users-count", usersHandler.OnlineCount)
	api.POST("/get-online-users-count", usersHandler.OnlineCount)

	// server-info (公開版) — サーバー情報
	// frontend の server-metric widget は `misskeyApiGet` で GET 呼び出し、
	// それ以外の MkVisitorDashboard 等は POST で呼ぶため両メソッドを登録。
	api.POST("/server-info", metaHandler.ServerInfo)
	api.GET("/server-info", metaHandler.ServerInfo)

	// endpoints — 登録済みAPIエンドポイント一覧
	// **Lister は closure で遅延評価する。** ここでの登録時点ではまだ全ルートが
	// 生えていないので、呼ばれたときに数える必要がある。
	endpointsHandler := endpoints.NewHandler(func() []string { return apiEndpointNames(s.echo) })
	api.POST("/endpoints", endpointsHandler.Endpoints)

	// endpoint — 指定 endpoint の情報を返す (#1695)。upstream endpoint.ts は
	// 未知 endpoint で null、既知なら { params: [{name,type}, ...] } を返す。
	// mk-go は per-endpoint の paramDef メタデータを保持しない (handler は
	// router.go の inline closure で param schema を構造化していない) ため、
	// param 名/型までは導出できない。ここでは response の shape (params は配列)
	// と未知 endpoint の null だけ upstream に揃える。params 内容の導出は
	// endpoint registry の新設が必要で本 endpoint の用途 (API introspection) に
	// 対して費用対効果が低いため非対応 (#1695 に明記)。
	api.POST("/endpoint", endpointsHandler.Endpoint)

	// retention — リテンション統計
	api.POST("/retention", chartsHandler.Retention)

	// get-avatar-decorations — アバターデコレーション全件取得
	avatarDecorationsHandler := avatardecorations.NewHandler(s.db, roleService.ExistingRoleIDSet)
	api.POST("/get-avatar-decorations", avatarDecorationsHandler.Get)

	// email-address/available — メールアドレスの利用可否チェック (public)
	//
	// upstream email-address/available.ts は requireCredential:false の public
	// endpoint で、EmailService.validateEmailForAccount が reason を
	// null|'used'|'format'|'disposable'|'mx'|'smtp'|'banned'|'network'|'blacklist'
	// で返す (#1551)。mk-go は外部依存の active validation (mx/smtp/disposable) は
	// 行わず、format → used (emailVerified=true 一致) → banned の順で判定する
	// (upstream の enableActiveEmailValidation=false 相当)。
	signupHandler.SetEmailAvailabilityDB(s.db)
	api.POST("/email-address/available", signupHandler.EmailAvailable)

	// promo/read — プロモノートの既読マーク (認証必須)
	promoHandler := promo.NewHandler(noteRepo, promoReadRepo, idGen)
	api.POST("/promo/read", promoHandler.Read,
		middleware.RequireAuth(), middleware.RequireScope("write:account"))

	// invite/* — 招待コード user-scope 4 endpoint。canInvite role policy gate
	// (#1020) + inviteLimit / inviteLimitCycle / inviteExpirationTime (#1029
	// PR-2) は handler 側で enforcement。handler 抽出後の unit test は
	// internal/api/invite/handler_test.go を参照。
	inviteHandler := apiinvite.NewHandler(signupTicketRepo, idGen)
	inviteHandler.SetRolePolicyProvider(roleService)
	// invite/list が createdBy / usedBy を UserLite で解決するため (#1776)。
	inviteHandler.SetUserRepo(userRepo)
	// モデレーターが他人の / createdById が NULL の招待を消せるようにする (#2812)。
	// 未配線なら bypass が効かないだけなので `recordCriticalWiring` には載せない
	// (緩む側ではなく厳しい側に倒れる)。この行が消えていないことは
	// `TestInviteModeratorCheckerIsWired` が見る。
	inviteHandler.SetModeratorChecker(roleService)
	api.POST("/invite/create", inviteHandler.Create,
		middleware.RequireAuth(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanInvite), middleware.RequireScope("write:invite-codes"))

	// upstream は invite/list・delete・limit にも create と同じ
	// requiredRolePolicy:'canInvite' を付ける。canInvite 非保持ユーザーが招待コード
	// 一覧/削除/残数取得を実行できないよう gate を揃える (#1776)。
	api.POST("/invite/list", inviteHandler.List,
		middleware.RequireAuth(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanInvite), middleware.RequireScope("read:invite-codes"))
	api.POST("/invite/delete", inviteHandler.Delete,
		middleware.RequireAuth(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanInvite), middleware.RequireScope("write:invite-codes"))
	api.POST("/invite/limit", inviteHandler.Limit,
		middleware.RequireAuth(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanInvite), middleware.RequireScope("read:invite-codes"))

	// notes (plain) — bulk note lookup by noteIds
	api.POST("/notes", notesHandler.BulkShow)

	// export-custom-emojis — 全 local custom emoji を zip (画像 + meta.json) に
	// export する非同期ジョブを enqueue する (#1217)。upstream 同様 endpoint は
	// 即 204 を返し、生成完了後に exportCompleted 通知 + drive file で受け取る。
	api.POST("/export-custom-emojis", emojisHandler.ExportCustomEmojis,
		middleware.RequireAuth(), middleware.RequireSecure())

	// fetch-external-resources — external JSON resource を hash 検証付きで取得
	// (#1222)。外部 theme/plugin 等を integrity-check して返す。SSRF-safe client
	// 経由、upstream requireCredential 相当で RequireAuth を要求する。
	fetchExternalHandler := apifetchexternal.New(s.outboundClient(apifetchexternal.FetchTimeout), s.config.UserAgent)
	api.POST("/fetch-external-resources", fetchExternalHandler.Fetch, middleware.RequireAuth(), middleware.RequireSecure())

	// fetch-rss — RSS / Atom feed プロキシ。frontend RSS / RSSTicker ウィジェット
	// が GET で叩く実装になっているため Misskey TS と同じく allowGet 相当で
	// GET / POST の両方を受け付ける。
	fetchRSSHandler := apifetchrss.New(s.outboundClient(apifetchrss.FetchTimeout), s.config.UserAgent)
	api.GET("/fetch-rss", fetchRSSHandler.Fetch)
	api.POST("/fetch-rss", fetchRSSHandler.Fetch)

	// page-push — page scriptが任意のeventをpage所有者のmainに emit する。
	api.POST("/page-push", pagesHandler.PagePush, middleware.RequireAuth(), middleware.RequireSecure())

	// v2/admin/emoji/list — v2はページネーション情報付きオブジェクトを返す専用ハンドラ
	api.POST("/v2/admin/emoji/list", adminHandler.EmojiListV2, middleware.RequireRolePolicy(roleService, corerole.PolicyCanManageCustomEmojis), middleware.RequireScope("read:admin:emoji"))

	// --- その他の残りエンドポイント ---
	// test — paramDef の検証挙動そのものを確認するための endpoint
	// (upstream test.ts、#1551)。validate 済の ps をそのまま echo する。
	//
	//   - required:['required'] が未指定なら 400
	//   - `id` は format:'misskey:id' なので空文字は 400
	//   - default 付きの param はキー省略時のみ既定値。`nullableDefault: null`
	//     は明示 null として通す (default で潰さない)
	echoTestHandler := apitest.NewEchoHandler()
	api.POST("/test", echoTestHandler.Test)

	// プラグインのルート (#2478)。catchall より前に登録する。
	// Echo の radix tree は静的パスを wildcard より優先するので順序に依存
	// しないが、「catchall の後ろに実ルートがある」形は読み手を惑わせる。
	registeredPlugins := plugins
	// nodeinfo に載せるのは **宣言したものだけ**。
	var peered []string
	for _, def := range registeredPlugins {
		if def.Peered && pluginEnabled(s.config.Plugins[def.Name]) {
			peered = append(peered, def.Name)
		}
	}
	nodeinfoHandler.SetPeeredPlugins(peered)

	// プラグイン同士の通信 (#2537)。**署名・解決・ブロック判定は本体のものを
	// そのまま使う** — inbox と同じ厳しさにするため、専用の実装を作らない。
	s.peerDeps = &pluginPeerDeps{
		selfHost: s.config.Host,
		client:   s.outboundClient(peerTimeout),
		resolver: federationResolver,
		keyCache: activitypub.NewPublicKeyCache(256),
		blocker:  instanceService,
		signer:   newInstanceActorSigner(sysAcctSvc, keypairRepo, apURLs),
		// **こちらが持っているものだけ覚える。** 相手の nodeinfo は相手が
		// 書いた値なので、そのまま持つと外から中身を膨らませられる。
		remote:  newNodeInfoPeerLister(s.outboundClient(peerLookupTimeout), peered),
		idGen:   idGen,
		limiter: newPeerRateLimiter(),
		// 送信はキューに積む (#2819)。**プロセス内の time.Sleep だと再起動を
		// またげず、デプロイのたびに送信中のものが消える。**
		enqueuer: s.queueClient,
	}

	if err := s.setupPlugins(api, registeredPlugins, openPluginStorage); err != nil {
		s.pluginSetupErr = err
		return
	}
	// 消したプラグインのデータが残っていないかを知らせる (#2479)。
	// 自動では消さないので、残っていること自体を見えるようにする。
	s.warnOrphanPluginData(context.Background(), registeredPlugins)
	// admin/server-plugins (#2497) が返すスナップショットと残存データ検査。
	adminHandler.SetServerPlugins(serverPluginInfos(registeredPlugins, s.config.Plugins))
	adminHandler.SetPluginOrphanSchemas(s.pluginOrphanSchemaLister(registeredPlugins))

	// API catchall。
	//
	// GET は upstream ApiServerService の `fastify.get('/*')` と同じく 404
	// UNKNOWN_API_ENDPOINT を返す。200 を返すと SPA catchall と区別が付かず、
	// 「/api 配下なのに HTML が返る」状態をクライアントが検出できない。
	//
	// GET 以外は意図的に 200 + 空オブジェクトのまま。未登録エンドポイントへの
	// 404 は Misskey 公式フロントの一部ページで例外を投げてしまうため、
	// 実装が出揃うまで pass-through にしている。実装漏れは warn ログで検知する。
	api.Any("/*", apiCatchall)

	// フロントエンドアセット配信
	// dev モードなら Vite dev server へプロキシ、そうでなければビルド済みアセットを
	// 静的配信する (無ければ 404。dev server へは流さない)。
	//
	// dev モードではビルド成果物の有無を**見ない** (#2477)。見てしまうと、
	// 以前のビルドが残っているだけで dev server に繋がらず HMR に入れない。
	registerFrontendAssets(s.echo, s.config, "/vite", frontendutil.FrontendDir(), viteDevServerURL)

	// embed 専用バンドル配信 (#2389)。通常の SPA とは別 build なので別ディレクトリ・
	// 別 prefix になる (upstream ClientServerService の `/embed_vite/` と同じ)。
	registerFrontendAssets(s.echo, s.config, "/embed_vite", frontendutil.FrontendEmbedDir(), viteEmbedDevServerURL)

	// フロントエンド配布アセット (locales, fonts等) + リポジトリアセット (ai.png等)
	// Echo は同一パスに Static を 2 回登録すると上書きされるため、
	// frontendDistDir → repoAssetsDir の順にフォールバックするハンドラを使う
	frontendDistDir := frontendutil.FrontendDistDir()
	repoAssetsDir := frontendutil.RepoAssetsDir()
	s.echo.GET("/assets/*", frontendutil.AssetsHandler(frontendDistDir, repoAssetsDir))

	// twemoji SVG配信 + 30 day Cache-Control (upstream `ms('30 days')` 同等)。
	twemojiDir := frontendutil.TwemojiDir()
	if _, err := os.Stat(twemojiDir); err == nil {
		serveStaticAssetDir(s.echo, "/twemoji", twemojiDir)
	}

	// fluent-emoji PNG配信 (実績バッジ等で参照される)。upstream Misskey TS の
	// /fluent-emoji/:hex.png 経路 (ClientServerService.ts) と互換、+ 30 day
	// Cache-Control (上記 twemoji と同 helper)。
	fluentEmojiDir := frontendutil.FluentEmojiDir()
	if _, err := os.Stat(fluentEmojiDir); err == nil {
		serveStaticAssetDir(s.echo, "/fluent-emoji", fluentEmojiDir)
	}

	// client-assets配信 (バブルゲーム、フラッシュ等のフロントエンドアセット)
	clientAssetsDir := frontendutil.ClientAssetsDir()
	if _, err := os.Stat(clientAssetsDir); err == nil {
		s.echo.Static("/client-assets", clientAssetsDir)
	}

	// 静的アセット配信 (favicon, splash, icons等)
	staticDir := frontendutil.StaticDir()
	if _, err := os.Stat(staticDir); err == nil {
		s.echo.Static("/static-assets", staticDir)
		// 埋め込み先サイトが読み込むローダー。upstream も staticAssets から
		// 配っている (#2389)。
		s.echo.File("/embed.js", filepath.Join(staticDir, "embed.js"))
		s.echo.File("/favicon.ico", filepath.Join(staticDir, "favicon.ico"))
		s.echo.File("/apple-touch-icon.png", filepath.Join(staticDir, "apple-touch-icon.png"))
		// /robots.txt はここでは配らない。upstream は meta を見て動的生成する
		// (ugcVisibilityForVisitor で Disallow が変わる) ので、上の
		// webResourceHandler に一本化する。静的ファイルを後から登録すると
		// そちらが後勝ちして、実体が無い環境で 404 になっていた (#2345)。
	}

	// identicon — ユーザーアイコン自動生成 (meta.enableIdenticonGeneration が
	// false なら 1x1 透明 PNG にフォールバック)
	s.echo.GET("/identicon/:x", identiconHandler(metaRepo))

	// /avatar/@:acct — frontend の MkMention.vue が <img src="/avatar/@user@host">
	// で直接読みに来るので、対応する handler が無いと mention chip が
	// アイコン無しで描画される (#462)。upstream Misskey TS の同名 endpoint
	// と互換: 見つかった user の avatarUrl にリダイレクトし、未設定なら
	// identicon、未存在なら /static-assets/user-unknown.png にフォールバック。
	s.echo.GET("/avatar/@:acct", avatarHandler(userRepo, localHost))

	// /emoji/:path — frontend の MkCustomEmoji が `:emojiUrl` プロップを
	// 受け取らないとき (典型: 通知 reaction icon) に <img src="/emoji/<name>@<host>.webp">
	// で直接 fetch するので、handler が無いと SPA catchall に流れて画像が
	// 読めず fallback 画像になる (#468)。upstream Misskey TS の
	// ServerService.ts:153 と互換に: name + host を抜いて emoji table を
	// lookup → publicUrl に 302 redirect、見つからなければ ?fallback クエリ
	// 付きなら /static-assets/emoji-unknown.png、無ければ 404。
	s.echo.GET("/emoji/:path", emojiRedirectHandler(emojiRepo))

	// manifest.json — PWA用
	s.echo.GET("/manifest.json", manifestJSON(s.config, metaRepo))

	// Service Worker (sw.js) — Misskey frontend は GET /sw.js を登録しにくる。
	// SPA catchall より前に登録しないと text/html にフォールバックしてしまい
	// ブラウザが "unsupported MIME type" エラーで SW 登録を拒否する。
	swDistDir := frontendutil.SwDistDir()
	if _, err := os.Stat(filepath.Join(swDistDir, "sw.js")); err == nil {
		s.echo.File("/sw.js", filepath.Join(swDistDir, "sw.js"))
	}

	// 埋め込みページ (#2389)。SPA catchall より**前**に登録する。後だと
	// /embed/* が通常の SPA シェルに落ちて、iframe の中にフルアプリが出る
	// (エラーにならないので気付きにくい壊れ方になる)。
	//
	// pack は viewer なしで行う。埋め込みは認証を伴わないので、閲覧者依存の
	// フィールドを解決してはいけない。可視性の判断は各 handler の
	// PackForEmbed 経由で既存の CanSee / HideEmbeds に委ねる。
	embed := newEmbedHandlers(s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability, EmbedDeps{
		NoteRepo: noteRepo,
		UserRepo: userRepo,
		ClipRepo: clipRepo,
		PackNote: func(c echo.Context, n *model.Note) (any, error) {
			return notesHandler.PackForEmbed(c.Request().Context(), n), nil
		},
		PackUser: func(_ echo.Context, u *model.User) (any, error) {
			// upstream の userEntityService.pack(user) 相当 (= UserLite)。
			return entity.PackUserLite(u), nil
		},
		PackClip: func(_ echo.Context, cl *model.Clip) (any, error) {
			return clipsHandler.PackForEmbed(cl), nil
		},
	})
	s.echo.GET("/embed/notes/:note", embed.Note)
	s.echo.GET("/embed/user-timeline/:user", embed.User)
	s.echo.GET("/embed/clips/:clip", embed.Clip)
	s.echo.GET("/embed/*", embed.Fallback)

	// Frontend HTML shell — SPA catchall (最後に登録)
	// CSP violation の受け口 (#2425)。**CSP が off のときは生やさない。**
	// 認証不要の POST なので、使わない構成で晒す理由が無い。
	if mode := s.config.FrontendContentSecurityPolicy; mode == CSPModeReportOnly || mode == CSPModeEnforce {
		s.echo.POST(CSPReportPath, cspReportHandler())
	}

	s.echo.GET("/*", frontend)

	// #2682: 未配線でも動いてしまい、かつ**認証や一回性の保証が無効になる**
	// 依存だけを起動時に検査する。消費側は fallback を残したまま (テストが
	// 未配線で大量に組み立てるため)、本番で fallback が発火する状態を
	// リクエスト時の無言の劣化ではなく起動時の失敗にする。棚卸しは #2674。
	//
	// **意図的に載せていないもの** (再監査で漏れと読まれないように):
	//   - admin.configSetupPassword: 空が正当な既定値なので `!= ""` では
	//     大半の構成で起動できなくなる。検査するなら s.config との値一致に
	//     なり、他と shape が違う (#2682 review L-6)
	//   - inbox の hostBlockChecker (processor 側と handler 側の 2 つ):
	//     連合ブロックは moderation の tier で #2683。ただし同じ checker が
	//     allowlist 運用と FederationDisabled も担うので、未配線だと閉じた
	//     構成が黙って開く。#2683 では優先して扱う
	//   - captcha (signup / signin): 配線が起動時の metaRepo.Fetch() 1 回に
	//     依存しており、**一時的な DB エラーでプロセス生存中ずっと captcha
	//     無しになる**。ここに載せないのは、captcha を使わない構成では nil が
	//     正当な状態で、単純な `!= nil` では大半の instance が起動しなく
	//     なるため。meta 側で provider が有効なときだけ要求する形にする必要が
	//     あり、他と検査の shape が違う (#2682 review M-D、別 issue)
	//   - pluginPeerDeps.selfHost: struct literal 経由の 2 つ目の消費者。
	//     config.resolve が空 host を弾くので実際には空にならない
	//     (#2682 review L-5)
	//   - #2791 で router.go から移設した endpoint の setter 6 本
	//     (SetUsernameLookups / SetEmailAvailabilityDB / SetMetaRepo /
	//     SetLocalHost / SetRetentionRepo / SetExportEnqueuer): 移設前は
	//     closure の字句捕捉だったので落とすとコンパイルエラーだったが、今は
	//     消してもビルドが通る。**ここに載せないのは、いずれも「認証や一回性の
	//     保証」ではないため** — 未配線で起きるのは空配列 / 0 / 「使えない」で、
	//     いちばん重いのが SetEmailAvailabilityDB の「登録済みアドレスを
	//     available:true と答える」。ただし `user_profile.email` に unique
	//     制約は無く、重複チェックはこの endpoint だけが持つので、劣化しても
	//     防げるのは事前の案内だけという点で他の項目と tier が違う。
	//     移設で新しく生まれた面なので、棚卸し (#2674) の対象には入れる
	s.recordCriticalWiring([]criticalWiring{
		{"users.remoteStatsFederationGate", remoteStatsFetcher.HasHostAllowedChecker(),
			"連合を切った相手へもプロフィール表示のたびに統計を取りに行き、誰をいつ見たかが漏れる"},
		{"peer.blocker", s.peerDeps.HasBlocker(),
			"プラグイン間通信でブロックリストと連合ポリシーが効かなくなる"},
		{"inbox.signatureVerifier", inboxProcessor.HasSignatureVerifier(),
			"HTTP 署名検証・連合ブロック・actor 一致・replay 判定が同時に飛ぶ"},
		{"signup.ticketConsumption", signupService.HasTicketConsumption(),
			"招待 ticket の行ロックが飛び、1 枚から複数アカウントを作る窓が開く"},
		{"signin.totpReplayGuard", signinHandler.HasTOTPReplayGuard(),
			"有効な TOTP コードを window 内で再利用できる"},
		{"signin.metaRepo", signinHandler.HasMetaRepo(),
			"new-login メールの instance lang fallback が空のまま進み、profile.lang 未設定時に併記ではなく英語だけになる"},
		{"resetpassword.metaRepo", resetHandler.HasMetaRepo(),
			"パスワードリセットメールの instance lang fallback が空のまま進む"},
		{"instance.hostBoundNodeinfoFetcher", metadataFetcher.HasHostBoundFetcher(),
			"nodeinfo の取得が redirect の飛び先に縛られなくなり、任意の host が返した JSON を instance 行へ書き戻せる"},
		{"federation.prohibitedWords", federationResolver.HasProhibitedWordsSource(),
			"リモートから届くノートに禁止語のフィルタが掛からなくなる (ローカル投稿にだけ効く状態)"},
		{"federation.noteDeleteHook.userRepo", noteDeleteHook.HasUserRepo(),
			"DM / フォロワー限定ノートの Delete が宛先に届かず、相手サーバーに残り続ける"},
		{"federation.noteDeleteHook.noteRepo", noteDeleteHook.HasNoteRepo(),
			"フォローしていないリモート user が renote / reply したノートを消しても、相手サーバーに Delete が届かない"},
		{"federation.localBaseURL", federationProcessor.HasLocalBaseURL(),
			"relay の Accept / Reject が全て落ち、ローカル URI の判定も外れて自ホスト宛の object がリモート扱いになる"},
		{"inbox.expectedHost", inboxHandler.HasExpectedHost(),
			"署名対象の host ヘッダ検査と自ホスト一致検査が飛ぶ"},
		{"inbox.enqueuer", inboxHandler.HasEnqueuer(),
			"同期 fallback へ落ち、actor 一致検査・LD-Signature 検証・replay guard を通らない"},
		{"i.totpReplayGuard", iHandler.HasTOTPReplayGuard(),
			"2FA gate を持つ 8 endpoint と i/2fa/done で有効な TOTP コードを再利用でき、バックアップコードの単回予約も効かなくなる"},
		{"i.authInvalidator", iHandler.HasAuthInvalidator(),
			"revoke-token / regenerate-token / delete-account / update の後も古い token が auth cache の TTL のあいだ通る"},
		{"admin.userTokenInvalidator", adminHandler.HasUserTokenInvalidator(),
			"凍結・削除した利用者の session が auth cache の TTL のあいだ生き残る"},
		{"i.accessTokenRepo", iHandler.HasAccessTokenRepo(),
			"i/revoke-token が成功と同じ 204 を返したまま token を消さない (恒久)"},
		{"oauth.authInvalidator", oauthHandler.HasAuthInvalidator(),
			"authorization code 再利用を検出して失効させた token が TTL のあいだ通る"},
		{"i.streamRevoker", iHandler.HasStreamRevoker(),
			"regenerate-token / revoke-token / delete-account の後も、失効した token で張られた WebSocket が通知・DM を受け取り続ける"},
		{"admin.userStreamRevoker", adminHandler.HasUserStreamRevoker(),
			"凍結・削除した利用者の WebSocket が通知・DM・フォロワー限定投稿を受け取り続ける"},
		{"oauth.streamRevoker", oauthHandler.HasStreamRevoker(),
			"authorization code 再利用で失効させた token の WebSocket が開いたまま残る"},
		{"signup.applicationSettlement", signupService.HasApplicationSettlement(),
			"承認済み申請の行ロックが飛び、1 承認から複数アカウントを作る窓が開く"},
		{"signup.formTokens", signupHandler.HasFormTokens(),
			"captcha を 1 つも設定していないインスタンスで、申請 endpoint の唯一の簡易チェックが素通しになる"},
		{"inbox.replayGuard", inboxProcessor.HasInboxReplayGuard(),
			"処理済み activity の再投函を検出できない (isReplay が常に false)"},
		{"postScheduledNote.lock", postScheduledNoteProcessor.HasLock(),
			"job が二度 fire したとき予約投稿が 2 回 publish される"},
		{"emojiApplication.policyProvider", emojiApplicationService.HasPolicyProvider(),
			"カスタム絵文字申請の日次・週次・月次の上限が丸ごと効かなくなる"},
		{"emojiApplication.quotaResetRepo", emojiApplicationService.HasQuotaResetRepo(),
			"申請枠の手動リセットが常に失敗し、過去に戻した枠も再び満杯に見える"},
		{"admin.emojiImageFetcher", adminHandler.HasEmojiImageFetcher(),
			"承認した絵文字と admin/emoji/add で登録した絵文字 / admin/emoji/update で差し替えた絵文字が申請者 / 操作者 / 相手サーバーのファイルを参照し続け、消された時点で壊れる"},

		// ここから可視性・権限・上限 (#2683)。認証ほど鋭くはないが、いずれも
		// 利用者から見えない形で制限が外れる。
		{"notes.policyProvider", notesHandler.HasPolicyProvider(),
			"timeline の可視性 gate (ltlAvailable / gtlAvailable) に加え canSearchNotes・canUseTranslator・noteDraftLimit が同時に素通しになる"},
		{"pages.driveFileRepo", pagesHandler.HasDriveFileRepo(),
			"eyecatching image の所有者チェックが素通しになり、他人の drive ファイルを貼れる (IDOR)"},
		{"invite.rolePolicyProvider", inviteHandler.HasRolePolicyProvider(),
			"policy が空になり招待の上限が無制限になる"},
		{"signup.userPolicies", signupHandler.HasUserPolicyResolver(),
			"素の default policy を返すので、meta の base policy override も server cap も反映されない"},
		{"reaction.userRolesProvider", reactionService.HasUserRolesProvider(),
			"ロール限定カスタム絵文字を誰でも使える"},
		{"drive.roleChecker", driveService.HasRoleChecker(),
			"MIME 許可リスト・NSFW 強制・ファイルサイズ上限・容量上限が同時に無効になる"},
		{"move.carryOverRepos", accountMover.HasCarryOverRepos(),
			"移行先アカウントにブロック / ミュート / リストが引き継がれない"},
		{"note.blockingRepo", noteCreateService.HasBlockingRepo(),
			"自分をブロックしている相手への返信・引用が通る"},
		{"note.metaRepo", noteCreateService.HasMetaRepo(),
			"meta のセンシティブワードと禁止ワードが両方とも効かない"},
		{"note.silencingProvider", noteCreateService.HasSilencingProvider(),
			"silenced な利用者の public 投稿が home へ降格されない"},
		{"stream.hardMuteLookup", streamManager.HasHardMuteLookup(),
			"配信ごとのハードミュート filter が無効になる"},
		{"stream.muteBlockLookup", streamManager.HasMuteBlockSnapshotLookup(),
			"配信ごとのミュート / ブロック filter が無効になる (fail-open)"},
		{"stream.policyProvider", streamManager.HasPolicyProvider(),
			"streaming の ltlAvailable / gtlAvailable gate が無効になる (fail-open)"},
		{"inboxProcessor.hostBlockChecker", inboxProcessor.HasHostBlockChecker(),
			"ブロック済み host / 許可外 host からの activity を受け入れる"},
		{"inbox.hostBlockChecker", inboxHandler.HasHostBlockChecker(),
			"federation: none 設定でも inbox が受け付ける"},
		{"deliver.hostBlockChecker", deliverService.HasHostBlockChecker(),
			"ブロック済み host へも配送する (outbound の defederation が効かない)"},
		{"resolver.hostBlockChecker", federationResolver.HasHostBlockChecker(),
			"federation 設定を無視してリモートを解決する"},
		{"streamChannels.chatRoom.service", chatRoomFactory.HasService(),
			"chatRoom channel の購読でメンバーシップ検査が skip され、誰でも任意の room を購読できる"},
		{"note.driveFileRepo", noteCreateService.HasDriveFileRepo(),
			"他人の drive file ID を notes/create の fileIds に指定して自分の note に貼れる (IDOR)"},
		{"antenna.rolePolicyProvider", antennaService.HasRolePolicyProvider(),
			"antennaLimit が効かずアンテナを無制限に作れる"},
		{"clip.rolePolicyProvider", clipService.HasRolePolicyProvider(),
			"clipLimit と noteEachClipsLimit が両方とも効かない"},
		{"webhooks.rolePolicyProvider", webhookHandler.HasRolePolicyProvider(),
			"webhookLimit が効かず webhook を無制限に作れる"},
		{"userlists.rolePolicyProvider", userListHandler.HasRolePolicyProvider(),
			"userListLimit と userEachUserListsLimit が両方とも効かない"},
		{"users.rolePolicyProvider", usersHandler.HasRolePolicyProvider(),
			"users/lists/create-from-public の userListLimit / userEachUserListsLimit が効かない"},

		// ここから #2708。#2683 で「既存 entry と基準上区別できない」まで取り込み、
		// 残していた同クラスの依存。
		{"notes.driveFileRepo", notesHandler.HasDriveFileRepo(),
			"下書きの添付で他人の drive ファイル ID を指定でき、かつ全 note の files が空になる (昇格時は note.driveFileRepo が checkFileIDs で弾くので実体は漏れない)"},
		{"chat.driveFileRepo", chatHandler.HasDriveFileRepo(),
			"他人の drive ファイルをチャットに添付できる (IDOR)"},
		{"ap.hostBlockChecker", apHandler.HasHostBlockChecker(),
			"federation: none でも Person を AP serve し、ap/show の FEDERATION_NOT_ALLOWED gate も skip される"},
		{"deliverProcessor.deliveryGate", deliverProcessor.HasDeliveryGate(),
			"block / federation mode は enqueue 時チェックの第 2 の関門が外れ (積み残しと retry 中が漏れる)、suspend は enqueue 側に検査が無いので新規ジョブごと配送し続ける"},
		{"admin.suspensionOriginRepo", adminHandler.HasSuspensionOriginRepo(),
			"モデレーターの suspend / unsuspend が由来を刻まないので、解除が次の actor refresh で無言で戻る (#2973)"},
		{"resolver.suspensionOriginRepo", federationResolver.HasSuspensionOriginRepo(),
			"リモート actor の凍結を読まなくなる (#2951 が効かない)。読んだとしても由来を刻めないので、モデレーターが解除しても次の refresh で無言で戻る"},
		{"deliverProcessor.signingKeySource", deliverProcessor.HasSigningKeySource(),
			"署名鍵は payload に載せず配送時に引くので、未配線だと全ての AP 配送が POST されないまま失敗する (連合が止まる)"},
		{"notes.metaRepo", notesHandler.HasMetaRepo(),
			"blocked-host の note が post-fetch 経路で漏れる"},
		{"antennas.metaRepo", antennasHandler.HasMetaRepo(),
			"blocked-host の note が antenna で漏れる"},
		{"clips.metaRepo", clipsHandler.HasMetaRepo(),
			"blocked-host の note が clip で漏れる"},
		{"users.metaRepo", usersHandler.HasMetaRepo(),
			"blocked-host の note が users/notes・users/reactions・users/featured-notes で漏れる"},
		{"roles.metaRepo", rolesHandler.HasMetaRepo(),
			"blocked-host の note が roles/notes で漏れる"},
		{"channels.metaRepo", channelsHandler.HasMetaRepo(),
			"blocked-host の note が channels/timeline で漏れる"},
		{"reaction.mediaSilenceChecker", reactionService.HasMediaSilenceChecker(),
			"media-silenced な host からのカスタム絵文字リアクションがそのまま出る"},
		{"inbox.blockingService", federationProcessor.HasBlockingService(),
			"受信した Block / Undo(Block) が無視され、reversi の Invite からブロック判定が飛ぶ"},
		{"following.silencedHostChecker", followingService.HasSilencedHostChecker(),
			"サイレンスしたホストからのフォローが承認なしで通り、followers 限定ノートが配送される"},
		{"resolver.silencedHostChecker", federationResolver.HasSilencedHostChecker(),
			"silenced instance の remote public note が home へ降格されず public timeline に出る"},
		{"following.blockingChecker", followingService.HasBlockingChecker(),
			"ブロック関係を無視してフォローが成立する (自分がブロックした相手・自分をブロックしている相手の両方。後者は inbox の Follow も通す)"},
		{"admin.ipLookupAudit", adminHandler.HasIPLookupAudit(),
			"IP 照会が監査に残らない (照会そのものは成立するので誰も気付けない)"},
		{"blocking.followRequestCanceller", blockingService.HasFollowRequestCanceller(),
			"block しても保留中の follow request が双方向に残り、block 中に承認されるとフォロー関係が成立する"},
		{"reaction.blockingChecker", reactionService.HasBlockingChecker(),
			"自分をブロックしている相手の投稿にリアクションできる (IsBlocked(target.UserID, user.ID))"},
		{"poll.blockingChecker", pollService.HasBlockingChecker(),
			"自分をブロックしている相手のアンケートに投票できる (IsBlocked(target.UserID, user.ID))"},
		{"chat.blockingRepo", chatService.HasBlockingRepo(),
			"自分をブロックしている相手へ DM を送れる (Exists(toUserID, fromUserID))"},
		{"notes.blockingRepo", notesHandler.HasBlockingRepo(),
			"ブロック / ミュート / チャンネルミュートの post-fetch filter が丸ごと skip される"},
		{"notes.mutingRepo", notesHandler.HasMutingRepo(),
			"post-fetch filter のミュート集合と notes/polls/recommendation の除外集合が空になる (timeline の SQL は UseMutingSubquery で残る)"},
		{"notes.channelMutingRepo", notesHandler.HasChannelMutingRepo(),
			"チャンネルミュートが一切効かない (別ソースが無いので in-memory・SQL・user-list timeline・HTL の全経路)"},
		{"notes.userFollowingRepo", notesHandler.HasUserFollowingRepo(),
			"followers 限定投稿への返信が非フォロワーの HTL / STL に出る"},
		{"i.roleProvider", iHandler.HasRoleProvider(),
			"alwaysMarkNsfw の自己解除防止・wordMuteLimit・canUpdateBioMedia・i/import-antennas の antennaLimit が同時に外れ、avatarDecorationLimit は policy を無視して 1 固定になり、canUseEmojiAsAvatarDecoration も既定の true 固定になる (ロールで絵文字デコレーションを止められなくなる)"},
		{"notes.ugcVisibility", notesHandler.HasUGCVisibility(),
			"匿名 visitor への note 露出を ugcVisibilityForVisitor で gate できない"},
		{"users.ugcVisibility", usersHandler.HasUGCVisibility(),
			"匿名 visitor への remote profile 露出を ugcVisibilityForVisitor で gate できない"},

		// ここから #2709 review。上と収載基準が同じなのに落ちていた分
		// (無条件配線で、nil が空集合として素通しされる = 緩い側へ倒れる)。
		{"antennas.muteBlockRepos", antennasHandler.HasMuteBlockRepos(),
			"antennas/notes でミュート / ブロック / チャンネルミュートの filter が消える (呼び出し元の gate も無い)"},
		{"clips.muteBlockRepos", clipsHandler.HasMuteBlockRepos(),
			"clips/notes でミュート / ブロックの filter が消える"},
		{"users.channelMutingRepo", usersHandler.HasChannelMutingRepo(),
			"users/notes (withChannelNotes) の post-fetch filter でチャンネルミュートが効かない"},
		{"users.mutingRepo", usersHandler.HasMutingRepo(),
			"users/notes の post-fetch filter のミュート集合が空になる"},
		{"users.blockingRepo", usersHandler.HasBlockingRepo(),
			"isBlockedByTarget が常に false になり、被ブロック中の相手の users/notes・reactions・featured-notes が読める"},
		{"notes.renoteMutingRepo", notesHandler.HasRenoteMutingRepo(),
			"renote ミュートが read 時に一切効かない"},
	})
}

// selfCheckAdapter runs the federation / dependency self-checks for the
// admin endpoint (#2463).
//
// **検査の宛先は config の `url` に固定する。** 自ホストは loopback / private IP
// に解決されうるので検査用 client は SSRF ガードを通さない。宛先を外から
// 与えられる口を作らないことが、その安全性の前提になっている。
type selfCheckAdapter struct {
	checker *selfcheck.Checker
	deps    selfcheck.LocalDeps
}

func (a *selfCheckAdapter) RunSelfCheck(ctx context.Context) selfcheck.Report {
	return selfcheck.Run(ctx, a.checker, a.deps)
}

// notifReaderAdapter bridges stream.NotificationReader to
// core/notification.Service.MarkAllAsRead.
type notifReaderAdapter struct {
	svc *corenotification.Service
}

func (a *notifReaderAdapter) ReadAll(userID string) error {
	// WebSocket の readNotification は upstream Connection.onReadNotification と
	// 同じく暗黙既読なので force を立てない (#2831)。
	return a.svc.MarkAllAsRead(context.Background(), userID, false)
}

// hardMuteLookupAdapter bridges UserRepository to stream.HardMuteRulesLookup
// so the streaming Manager can attach the persisted hardMutedWords (#787) at
// connection setup. Returns nil on lookup failure / empty rule set so the
// streaming filter degrades to no-op rather than dropping the connection.
// lastActiveRecorderAdapter bumps user.lastActiveDate for a live WebSocket
// connection (upstream StreamingApiServerService の updateLastActiveDate 相当)。
// 書き込みは goroutine に逃がして接続処理を待たせない。
type lastActiveRecorderAdapter struct {
	userRepo repository.UserRepository
}

func (a *lastActiveRecorderAdapter) RecordActive(userID string) {
	if a.userRepo == nil || userID == "" {
		return
	}
	go func() {
		if err := a.userRepo.UpdateUser(userID, map[string]any{"lastActiveDate": time.Now()}); err != nil {
			slog.Debug("streaming: lastActiveDate update failed", "userId", userID, "err", err)
		}
	}()
}

// sensitiveChannelLookup answers "is this channel sensitive?" for antenna
// filtering. note は channel を埋めて持たないので channelId から都度引く。
type sensitiveChannelLookup struct {
	repo repository.ChannelRepository
}

func (l *sensitiveChannelLookup) IsSensitiveChannel(channelID string) bool {
	if l.repo == nil || channelID == "" {
		return false
	}
	ch, err := l.repo.FindByID(channelID)
	if err != nil || ch == nil {
		return false
	}
	return ch.IsSensitive
}

type hardMuteLookupAdapter struct {
	userRepo repository.UserRepository
}

func (a *hardMuteLookupAdapter) HardMutedWordsForUser(userID string) []byte {
	if a.userRepo == nil || userID == "" {
		return nil
	}
	profile, err := a.userRepo.FindProfileByUserID(userID)
	if err != nil || profile == nil || len(profile.HardMutedWords) == 0 {
		return nil
	}
	return []byte(profile.HardMutedWords)
}

// followingSnapshotAdapter bridges FollowingRepository to
// stream.FollowingSnapshotLookup so the streaming Manager can snapshot the
// viewer's followee list at connection setup (#1063). 戻り値は followeeID →
// withReplies の map で、home/hybrid/local timeline の reply gate に使う。
//
// 依存: `repository.FollowingRepository.ListFollowing(userID, limit, offset)`
// (`internal/repository/following.go`)。既存 method なので追加配線は不要。
//
// fetch は ListFollowing をページネーションで全件読みだす。フォロー数 N が
// 巨大な場合 (10K+) のメモリ / DB 負荷はあるが、upstream Misskey TS も同等
// な map を connection ごとに保持しているので drop-in 互換性回復のためには
// 必要なコスト。pageSize は upstream の channel-following-service と同じく
// 200 / 上限は 10000 で cap し、それ以上 follow している power user は
// snapshot から外れて escape hatch のみ動作する degrade に倒す (= 旧来の
// "全 reply drop" よりは upstream 寄り)。
//
// 改善案 (本 PR scope outside): 同一 user の WebSocket 再接続ごとに毎回
// 全 followee を fetch するので、tab 多重・短期再接続の多い production
// では DB 負荷になる。per-user LRU cache を Manager に持たせれば
// connection accept のコストを下げられる。invalidation は Following 変更
// の subscriber (#791 と同じパターン) で粒度を合わせる想定。
type followingSnapshotAdapter struct {
	repo repository.FollowingRepository
}

func (a *followingSnapshotAdapter) FollowingSnapshotForUser(userID string) map[string]bool {
	if a.repo == nil || userID == "" {
		return nil
	}
	const pageSize = 200
	const maxEntries = 10000
	snap := make(map[string]bool)
	offset := 0
	for {
		rows, err := a.repo.ListFollowing(userID, pageSize, offset)
		if err != nil {
			return snap
		}
		if len(rows) == 0 {
			return snap
		}
		for _, r := range rows {
			snap[r.FolloweeID] = r.WithReplies
			if len(snap) >= maxEntries {
				return snap
			}
		}
		if len(rows) < pageSize {
			return snap
		}
		offset += pageSize
	}
}

// muteBlockSnapshotAdapter bridges the muting / blocking / renote-muting /
// channel-muting repositories + user profile to stream.MuteBlockSnapshotLookup
// so the streaming Manager can snapshot the viewer's mute/block relationships
// at connection setup (#1711). The main / notifications channels consult it to
// drop notifications / mentions involving muted instances, muted users, users
// blocking the viewer, renote-muted authors, or muted channels.
//
// 各 set fetch は best-effort: 個別の repo error は空 set に degrade させ、
// snapshot 全体は常に non-nil で返す (= 取得できた範囲だけ filter する)。
// followingSnapshotAdapter と同じく接続ごとに 1 回読みだす connection-time
// snapshot だが、変更時は SubscribeRelationReload (上の配線を参照) が
// 同じ lookup を呼び直して接続中の snapshot を差し替える (#2400)。
type muteBlockSnapshotAdapter struct {
	muting        repository.MutingRepository
	blocking      repository.BlockingRepository
	renoteMuting  repository.RenoteMutingRepository
	channelMuting repository.ChannelMutingRepository
	userRepo      repository.UserRepository
}

func (a *muteBlockSnapshotAdapter) MuteBlockSnapshotForUser(userID string) *stream.MuteBlockSnapshot {
	if userID == "" {
		return nil
	}
	snap := &stream.MuteBlockSnapshot{
		Muting:         map[string]struct{}{},
		BlockingMe:     map[string]struct{}{},
		RenoteMuting:   map[string]struct{}{},
		MutedInstances: map[string]struct{}{},
		MutingChannels: map[string]struct{}{},
	}
	if a.muting != nil {
		if ids, err := a.muting.ListMuteeIDs(userID); err == nil {
			for _, id := range ids {
				snap.Muting[id] = struct{}{}
			}
		}
	}
	if a.blocking != nil {
		if ids, err := a.blocking.ListBlockerIDs(userID); err == nil {
			for _, id := range ids {
				snap.BlockingMe[id] = struct{}{}
			}
		}
	}
	if a.renoteMuting != nil {
		if ids, err := a.renoteMuting.ListMuteeIDs(userID); err == nil {
			for _, id := range ids {
				snap.RenoteMuting[id] = struct{}{}
			}
		}
	}
	if a.channelMuting != nil {
		if rows, err := a.channelMuting.ListByUser(userID); err == nil {
			for _, r := range rows {
				snap.MutingChannels[r.ChannelID] = struct{}{}
			}
		}
	}
	if a.userRepo != nil {
		if profile, err := a.userRepo.FindProfileByUserID(userID); err == nil && profile != nil && len(profile.MutedInstances) > 0 {
			var hosts []string
			if json.Unmarshal(profile.MutedInstances, &hosts) == nil {
				for _, h := range hosts {
					if h != "" {
						// host は AP canonical の lowercase で比較する
						// (timeline filter / loadMutedInstances と統一)。
						snap.MutedInstances[strings.ToLower(h)] = struct{}{}
					}
				}
			}
		}
	}
	return snap
}

// relationReloadPublisherAdapter bridges PubSubService to the various
// RelationReloadPublisher interfaces (core/muting, core/blocking,
// core/following, api/channels, api/i) so relation changes reach every active
// streaming connection of the user (#2400).
//
// mutation 側は「誰の」「どちらの snapshot が」変わったかだけを知り、pubsub の
// 存在を知らない。hardMutePublisherAdapter (#791) と同じ形。
//
// 失敗は log のみで握る。realtime nudge が届かなくても API 応答は成功させる
// (client は次の再接続で新しい snapshot を取る)。
type relationReloadPublisherAdapter struct {
	pubsub *event.PubSubService
}

func (a *relationReloadPublisherAdapter) publish(userID string, scope stream.RelationScope) {
	if a.pubsub == nil || userID == "" {
		return
	}
	payload := stream.RelationReloadPayload{UserID: userID, Scope: scope}
	if err := a.pubsub.Publish(context.Background(), stream.RelationReloadTopic, payload); err != nil {
		slog.Warn("router: relation reload publish failed",
			"userID", userID, "scope", scope, "err", err)
	}
}

func (a *relationReloadPublisherAdapter) PublishMuteBlockReload(userID string) {
	a.publish(userID, stream.RelationScopeMuteBlock)
}

func (a *relationReloadPublisherAdapter) PublishFollowingReload(userID string) {
	a.publish(userID, stream.RelationScopeFollowing)
}

// hardMutePublisherAdapter bridges PubSubService to i.HardMutePublisher so
// i/update can flush a wordmute reload event to every active streaming
// connection of the user (#791). Failures are logged but never bubbled — the
// API response should not fail just because the realtime nudge couldn't be
// delivered (clients still pick up the new rules at the next reconnect).
type hardMutePublisherAdapter struct {
	pubsub *event.PubSubService
}

func (a *hardMutePublisherAdapter) PublishHardMuteReload(userID string) {
	if a.pubsub == nil || userID == "" {
		return
	}
	payload := stream.WordMuteReloadPayload{UserID: userID}
	if err := a.pubsub.Publish(context.Background(), stream.WordMuteReloadTopic, payload); err != nil {
		slog.Warn("router: wordmute reload publish failed", "userID", userID, "err", err)
	}
}

// reactionNoteStreamAdapter bridges core/reaction.NoteStreamHook to the
// shared stream.NoteEventPublisher. Misskey TS upstream の wire format に
// 揃え、`{type: "reacted"|"unreacted", body: {reaction, emoji?, userId}}`
// を `noteStream:<noteID>` に publish する (#700)。
type reactionNoteStreamAdapter struct {
	pub *stream.NoteEventPublisher
}

func (a *reactionNoteStreamAdapter) OnReacted(noteID, userID, reaction string, emoji *corereaction.NoteStreamEmoji) {
	body := map[string]any{
		"reaction": reaction,
		"userId":   userID,
		"emoji":    nil,
	}
	if emoji != nil {
		body["emoji"] = map[string]any{"name": emoji.Name, "url": emoji.URL}
	}
	a.pub.PublishToNoteStream(noteID, "reacted", body)
}

func (a *reactionNoteStreamAdapter) OnUnreacted(noteID, userID, reaction string) {
	a.pub.PublishToNoteStream(noteID, "unreacted", map[string]any{
		"reaction": reaction,
		"userId":   userID,
	})
}

// noteDeleteStreamAdapter bridges core/note.DeleteNoteStreamHook to the
// shared stream.NoteEventPublisher (#700)。
type noteDeleteStreamAdapter struct {
	pub *stream.NoteEventPublisher
}

func (a *noteDeleteStreamAdapter) OnNoteDeleted(noteID string, deletedAt time.Time) {
	a.pub.PublishToNoteStream(noteID, "deleted", map[string]any{
		"deletedAt": deletedAt.UTC().Format(time.RFC3339Nano),
	})
}

// instanceActorSigner is the SignerProvider used by APFetcher to sign
// outgoing GETs with the instance.actor system account (#419)。
//
// 初回 Signer() で systemaccount.Fetch + keypair lookup + RSA PEM parse を
// やってから、結果 (parse 済みの *PrivateKey) を mutex 配下にキャッシュする。
// 一度成功した key は process lifetime 中に変わらない前提で無期限保持し、
// 再起動で再ロードされる。
//
// 失敗側はキャッシュしない: transient な DB glitch で起動直後に load が
// コケた時、その後ずっと unsigned-only に張り付くことを避けるため、失敗
// を `signerLoadBackoff` だけ抑制してから再試行する (#419 Devin review)。
type instanceActorSigner struct {
	sysAcct *coresystemaccount.Service
	keypair repository.UserKeypairRepository
	urls    *activitypub.URLBuilder

	mu          sync.RWMutex
	cachedKey   *activitypub.PrivateKey
	lastFailure time.Time
}

// signerLoadBackoff は load() 失敗後の再試行抑制間隔。DB が一時的に死んだ
// 時に Signer() の per-fetch で同じクエリを叩き続けないようにする。
const signerLoadBackoff = 30 * time.Second

func newInstanceActorSigner(
	svc *coresystemaccount.Service,
	kp repository.UserKeypairRepository,
	urls *activitypub.URLBuilder,
) *instanceActorSigner {
	return &instanceActorSigner{sysAcct: svc, keypair: kp, urls: urls}
}

// Signer returns the parsed instance.actor PrivateKey. corefederation.ErrNoSigner
// を返した場合 APFetcher は unsigned-only モードで継続する。
//
// double-checked locking で hot path (cache hit) を read-lock のみにし、
// 同時 fetch が DB query 待ちで直列化しないようにする (#419 Devin review)。
// slog.Warn は lock 解除後に呼んで、log handler が遅い時に他の goroutine を
// 詰まらせない (#419 Devin review)。
func (s *instanceActorSigner) Signer() (*activitypub.PrivateKey, error) {
	// Fast path: 既に load 済みなら read-lock だけで返す
	s.mu.RLock()
	if k := s.cachedKey; k != nil {
		s.mu.RUnlock()
		return k, nil
	}
	s.mu.RUnlock()

	// Slow path: load を試みる
	var (
		loaded  *activitypub.PrivateKey
		loadLog func() // nil 以外なら lock 解除後に呼ぶ Warn ログ
	)
	s.mu.Lock()
	switch {
	case s.cachedKey != nil:
		// double-check: lock 取得待ちの間に他 goroutine が load を成功させた
		loaded = s.cachedKey
	case !s.lastFailure.IsZero() && time.Since(s.lastFailure) < signerLoadBackoff:
		// 直近の失敗から十分時間が経っていない → DB spam 抑制で skip
	default:
		key, logFn := s.loadLocked()
		loadLog = logFn
		if key != nil {
			s.cachedKey = key
			s.lastFailure = time.Time{}
			loaded = key
		} else {
			s.lastFailure = time.Now()
		}
	}
	s.mu.Unlock()

	if loadLog != nil {
		loadLog()
	}
	if loaded != nil {
		return loaded, nil
	}
	return nil, corefederation.ErrNoSigner
}

// remoteStatsFetcherAdapter は corefederation.RemoteStatsFetcher を
// users.RemoteStatsFetcher interface に橋渡しする (#943)。layered architecture
// (api → core → repository) を保つため users package が federation を直接 import
// しない設計を維持する。
type remoteStatsFetcherAdapter struct {
	fetcher *corefederation.RemoteStatsFetcher
}

func (a *remoteStatsFetcherAdapter) Fetch(ctx context.Context, host, username string) *users.RemoteUserStatsView {
	stats := a.fetcher.Fetch(ctx, host, username)
	if stats == nil {
		return nil
	}
	return &users.RemoteUserStatsView{
		NotesCount:     stats.NotesCount,
		FollowersCount: stats.FollowersCount,
		FollowingCount: stats.FollowingCount,
	}
}

// loadLocked performs the actual systemaccount + keypair + PEM parse. mu を
// 既に保持している前提。Warn ログ呼び出しは caller (Signer) が lock 解除後
// に走らせるよう関数値で返す (sync 化されたログハンドラ呼び出しが他の
// goroutine をブロックしないため, #419 Devin review)。
func (s *instanceActorSigner) loadLocked() (*activitypub.PrivateKey, func()) {
	user, err := s.sysAcct.Fetch("instance")
	if err != nil || user == nil {
		return nil, func() {
			slog.Warn("instance.actor signer: system account fetch failed; AP fetches will fall back to unsigned",
				"err", err)
		}
	}
	kp, err := s.keypair.FindByUserID(user.ID)
	if err != nil || kp == nil || kp.PrivateKey == "" {
		return nil, func() {
			slog.Warn("instance.actor signer: keypair lookup failed; AP fetches will fall back to unsigned",
				"userId", user.ID, "err", err)
		}
	}
	key, err := activitypub.NewPrivateKey(s.urls.UserKeyURI(user.ID), kp.PrivateKey)
	if err != nil {
		return nil, func() {
			slog.Warn("instance.actor signer: PEM parse failed; AP fetches will fall back to unsigned",
				"userId", user.ID, "err", err)
		}
	}
	return key, nil
}
