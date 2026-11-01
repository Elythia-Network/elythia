package server

import (
	"context"
	"net/http"
	"net/http/pprof"
	urlpkg "net/url"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/activitypub/ld"
	apiannouncements "github.com/elythia-network/elythia/internal/api/announcements"
	apibubblegame "github.com/elythia-network/elythia/internal/api/bubblegame"
	apicharts "github.com/elythia-network/elythia/internal/api/charts"
	apichat "github.com/elythia-network/elythia/internal/api/chat"
	"github.com/elythia-network/elythia/internal/api/drive"
	apiemojis "github.com/elythia-network/elythia/internal/api/emojis"
	apifetchexternal "github.com/elythia-network/elythia/internal/api/fetchexternal"
	apifetchrss "github.com/elythia-network/elythia/internal/api/fetchrss"
	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/api/notes"
	apiproxy "github.com/elythia-network/elythia/internal/api/proxy"
	apiresetpassword "github.com/elythia-network/elythia/internal/api/resetpassword"
	apireversi "github.com/elythia-network/elythia/internal/api/reversi"
	apisignin "github.com/elythia-network/elythia/internal/api/signin"
	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	"github.com/elythia-network/elythia/internal/api/stats"
	apisw "github.com/elythia-network/elythia/internal/api/sw"
	apitest "github.com/elythia-network/elythia/internal/api/test"
	apiurl "github.com/elythia-network/elythia/internal/api/url"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	"github.com/elythia-network/elythia/internal/config"
	coreannouncement "github.com/elythia-network/elythia/internal/core/announcement"
	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	"github.com/elythia-network/elythia/internal/core/avatardecoration"
	corecaptcha "github.com/elythia-network/elythia/internal/core/captcha"
	corechannel "github.com/elythia-network/elythia/internal/core/channel"
	"github.com/elythia-network/elythia/internal/core/chart/charthook"
	"github.com/elythia-network/elythia/internal/core/dbhealth"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/driveusage"
	coreemojiimport "github.com/elythia-network/elythia/internal/core/emojiimport"
	"github.com/elythia-network/elythia/internal/core/event"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/fedrule"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/core/iplog"
	coremediaproxy "github.com/elythia-network/elythia/internal/core/mediaproxy"
	coremoderatoractivity "github.com/elythia-network/elythia/internal/core/moderatoractivity"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	corepluginaccount "github.com/elythia-network/elythia/internal/core/pluginaccount"
	corepluginnotify "github.com/elythia-network/elythia/internal/core/pluginnotify"
	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/core/pluginsecret/pgrepo"
	corepoll "github.com/elythia-network/elythia/internal/core/poll"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corerelay "github.com/elythia-network/elythia/internal/core/relay"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	coresearch "github.com/elythia-network/elythia/internal/core/search"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/core/signupform"
	coretimeline "github.com/elythia-network/elythia/internal/core/timeline"
	coretransfer "github.com/elythia-network/elythia/internal/core/transfer"
	coretranslate "github.com/elythia-network/elythia/internal/core/translate"
	coretwofactor "github.com/elythia-network/elythia/internal/core/twofactor"
	coreurlpreview "github.com/elythia-network/elythia/internal/core/urlpreview"
	"github.com/elythia-network/elythia/internal/core/userpack"
	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
	corewebpush "github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/plugin"
	"github.com/labstack/echo/v4"
)

func (s *Server) setupRoutes(plugins []plugin.Definition, openPluginStorage pluginStorageOpener) {
	idGen, err := id.NewGenerator(s.config.ID)
	if err != nil {
		idGen, _ = id.NewGenerator("aidx")
	}

	// Streaming の Redis pubsub と publisher (#3676)。どれも Redis の pubsub にしか
	// 依存せず、作っても接続も購読も始めない (Redis に繋ぐのは最初の Subscribe)。
	// なので受け手より先に作り、受け手を作った直後に渡す。
	// **購読を始める位置は下の stream の節のまま。** 早めると、handler ができる前に
	// 届いた event が、まだ配線していない service を触る。
	streamPubSub := event.NewPubSubService(s.redis.Pubsub, "stream:")
	s.streamPubSub = streamPubSub
	internalPubSub := event.NewPubSubService(s.redis.Pubsub, "internal:")
	s.internalPubSub = internalPubSub
	// pollVoted / reacted / unreacted / deleted を noteStream:<id> に publish
	// する共通 publisher。subNote / sn メッセージで購読しているクライアントへ
	// リアクション・削除イベントが流れる (#690 / #700)。
	//
	// 下の 3 つと userListStreamPublisher は buildStream へ移さず、ここで作る (#3692)。
	// どれも buildStream を呼ぶ位置 (#3692 では stream の節、#3827 からは buildHandlers の
	// 直前) より前にある受け手へ渡す依存 (3 つは core の service・連合の processor・handler、
	// userListStreamPublisher は #3692 の時点の usersHandler) で、それより前に要る。
	// buildStream をここで呼び別名も置くと、manager と note / notification の
	// publisher (setter の受け手) まで先頭へ移り、後から入れる辺が 96 から 117 に
	// 増える (docs/architecture.md 4.1 の数え方)。#3676 でここに置いた reversi の
	// publisher は、Packer を構築子の引数で受けるので entityPacker の後で作る (#3827)。
	noteEventPub := stream.NewNoteEventPublisher(streamPubSub)
	// relation 変更 (#2400) の publisher。7 系統すべてを繋がないと「一部の操作だけ
	// 反映されない」形の抜けになる。
	//   Muting          -> core/muting
	//   BlockingMe      -> core/blocking (通知先は blockee、向きに注意)
	//   RenoteMuting    -> core/muting の RenoteService
	//   MutedInstances  -> api/i の update
	//   MutingChannels  -> api/channels の mute
	//   following       -> core/following (Follow / Unfollow / AcceptRequest)
	// 加えて、期限付き mute の自然失効を cron (checkExpiredMutingsProcessor) が通知する。
	relationReloadPublisher := &relationReloadPublisherAdapter{pubsub: streamPubSub}
	drivePublisher := stream.NewDrivePublisher(streamPubSub)
	mainStreamPublisher := stream.NewMainStreamPublisher(streamPubSub)
	// users/lists/update-membership の withReplies 変更 (#2051) と、users/lists/push /
	// pull の userAdded / userRemoved (#1549) を流す publisher。前は 2 つの handler の
	// setter の引数でそれぞれ作っていた。持つのは pubsub だけで書き換える口も無いので、
	// 1 つを共有しても動きは同じ (#3692)。
	userListStreamPublisher := stream.NewUserListStreamPublisher(streamPubSub)

	// Repositories (#3684)。作るのは buildRepos (wiring_repos.go) で、ここでは
	// 今までと同じ名前の別名を置くだけにする。以降の部分は書き換えない
	// (wiring-check などのゲートが照合する文字列を変えないため)。repository は作っても
	// DB に触らないので、作る位置が下の EnsureInitial などより前に移っても、DB の文の
	// 順は変わらない。
	// 部品の一覧 (#3699) への登録は s.addStartCloser などの値から作る登録か、値を持たない
	// s.addWorker で行う (docs/architecture.md 4.3)。構築では登録するだけで、起動は
	// StartBackground が一覧の順に行う (#3703)。キャッシュの一覧 (#3721) への登録は
	// s.addCache で行う。
	w := &wiring{repos: s.buildRepos()}
	userRepo := w.repos.user
	noteRepo := w.repos.note
	cachedMeta := w.repos.cachedMeta
	var metaRepo repository.MetaRepository = cachedMeta
	pollRepo := w.repos.poll
	followingRepo := w.repos.following
	followRequestRepo := w.repos.followRequest
	userRelationRepo := w.repos.userRelation
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
	keypairExtraRepo := w.repos.keypairExtra
	suspensionOriginRepo := w.repos.suspensionOrigin
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
	roleRepo := w.repos.role
	swSubRepo := w.repos.swSub
	noteFavoriteRepo := w.repos.noteFavorite
	noteThreadMutingRepo := w.repos.noteThreadMuting
	userListRepo := w.repos.userList
	webhookRepo := w.repos.webhook
	systemWebhookRepo := w.repos.systemWebhook
	reversiRepo := w.repos.reversi
	userIPRepo := w.repos.userIP
	chatRepo := w.repos.chat
	channelFavoriteRepo := w.repos.channelFavorite
	channelMutingRepo := w.repos.channelMuting
	clipFavoriteRepo := w.repos.clipFavorite
	userListFavoriteRepo := w.repos.userListFavorite
	retentionRepo := w.repos.retention
	channelNoteUnreadRepo := w.repos.channelNoteUnread
	antennaNoteUnreadRepo := w.repos.antennaNoteUnread
	chunkedUploadSessionRepo := w.repos.chunkedUploadSession
	publickeyRepo := w.repos.publickey
	goneInstanceRepo := w.repos.goneInstance
	userPendingRepo := w.repos.userPending
	signupTicketRepo := w.repos.signupTicket
	usedUsernameRepo := w.repos.usedUsername
	signinRepo := w.repos.signin
	noteDraftRepo := w.repos.noteDraft
	avatarDecorationRepo := w.repos.avatarDecoration
	announcementRepo := w.repos.announcement
	recipientRepo := w.repos.recipient
	ipLookupLogRepo := w.repos.ipLookupLog
	moveSystemAccountRepo := w.repos.systemAccount
	systemAccountRepo := w.repos.systemAccount
	noteUnreadRepo := w.repos.noteUnread
	abuseReportRepoForNotif := w.repos.abuseReport
	emojiApplicationRepoForNotif := w.repos.emojiApplication
	signupApplicationRepoForNotif := w.repos.signupApplication
	signupApplicationRepo := w.repos.signupApplication
	fedRuleRepo := w.repos.fedRule
	sigCapRepo := w.repos.sigCap
	quoteAuthorizationRepo := w.repos.quoteAuthorization
	resetReqRepo := w.repos.resetReq
	userSecurityKeyRepo := w.repos.userSecurityKey
	registryRepo := w.repos.registry
	bubbleVersusRecordRepo := w.repos.bubbleVersusRecord
	abuseReportRepo := w.repos.abuseReport
	userMemoRepo := w.repos.userMemo
	galleryRepo := w.repos.gallery
	accessTokenRepo := w.repos.accessToken
	publickeyExtraRepo := w.repos.publickeyExtra
	adRepo := w.repos.ad

	// Seed the singleton meta row on first boot so that fresh installs
	// can run /api/admin/accounts/create (initial setup) without tripping
	// over a missing meta row.
	if err := metaRepo.EnsureInitial(idGen.Generate(time.Now())); err != nil {
		slog.Error("failed to ensure initial meta row", "err", err)
	}
	// 絵文字由来のアバターデコレーション (#2975) の解決に使う。**catalog と同じ形の
	// TTL cache だが、載せるのはローカルかつ非センシティブなものだけ**で、その
	// 条件がそのまま「表示してよいか」の判定になっている。絵文字をセンシティブに
	// したり消したりすると、後始末なしに次の TTL で全プロフィールから消える。
	//
	// **`emojiRepo` より後・絵文字を変える配線より前に置く。** zip インポータと
	// admin handler の両方がこの invalidator を受け取る。
	emojiDecorationResolver := avatardecoration.NewEmojiResolver(emojiRepo)
	// mk-go は #2378 以前、ローカルユーザーの RSA 秘密鍵を PKCS#1 で発行していた。
	// mk-go 自身は両形式を読めるが **Misskey TS は PKCS#8 しか読めない** ので、
	// そのまま TS へ引き渡すとそのユーザーの送信側の連合が全滅する (受信は動くので
	// 片方向だけ静かに壊れる)。既存インスタンスを救うため起動時に変換する。
	// TS へ引き渡すこと (復路) は保証しなくなった (#3191) が、鍵の形式は本家と揃えておく。
	//
	// 変換するのは PEM のエンコーディングだけで鍵そのものは変わらない。公開鍵も
	// 鍵 ID も不変なので連合相手から見て何も変わらない。冪等なので毎回走ってよく、
	// 対象が無ければ 1 クエリで終わる。失敗は警告だけで起動継続。
	if converted, err := keypairRepo.NormalizePrivateKeysToPKCS8(); err != nil {
		slog.Warn("user_keypair PKCS#8 正規化に失敗", "err", err)
	} else if converted > 0 {
		slog.Info("user_keypair を PKCS#8 に正規化した", "converted", converted)
	}
	// instance.{followersCount,followingCount} は following service の
	// adjustInstanceCountsForFollowing (Follow/Unfollow/AcceptRequest) で
	// incremental に維持される。block による解除も following.Service の
	// UnfollowForBlock を通るので同じ経路に乗る
	// (admin/overview の federation pie chart の data source)。起動時の
	// backfill は、再起動時点での following テーブルとの整合性回復 +
	// direct DB 改変や過去の counter drift への安全網として残す (#421)。
	// 失敗は警告だけで起動継続。
	if err := instanceRepo.RecomputeFollowCounts(); err != nil {
		slog.Warn("instance follow-counts recompute failed", "err", err)
	}
	// 認証済みリクエストの IP を記録する (#3103)。**meta は呼び出しのたびに読む**
	// ので、`enableIpLogging` の切り替えが再起動なしで効く (#3107)。重複排除の窓は
	// upstream の `userIpHistories` に合わせて 1 時間。
	ipLogService := iplog.NewService(userIPRepo, metaRepo, iplog.DedupeWindow)

	// Charts (Phase 4 Step Q / 4.7) — 12 chart engines + hook adapter (#3676)。
	// どちらも DB の handle と idGen にしか依存せず、作っても goroutine も起動しない
	// (定期の保存は下の chartMgmt、instance の件数のバッファも下)。なので受け手より
	// 先に作り、受け手を作った直後に SetChartHook で渡す。
	// 下の meta のフラグの写し (cachedMeta の読み出し) も一緒に前へ移した。最初の読み
	// 出しなので DB を読みうるが、元の位置との間に DB の文が無いので、PostgreSQL の
	// 文の順は変わらない。
	chartCharts := buildChartBundle(s.db)
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
		chartHooks.StatsForFederatedInst = m.EnableStatsForFederatedInstances
	}
	// 上の 3 つは起動時の写しで、読めなかったときの代わり。本家は meta をその場で
	// 読むので、admin での切り替えが再起動を待たずに効くよう毎回 cachedMeta を
	// 読ませる (#3330)。
	chartHooks.SetMetaSource(metaRepo.Fetch)

	// role.Service (#3825)。entity の Packer と共有の userpack の Packer が依存し、
	// buildCore で作る部品の構築子がその 2 つを受け取るので、buildCore より前に作る。
	// 作るだけで DB にも Redis にも触らない。setter は下の元の位置のまま。
	roleService := s.buildRole(w, idGen)

	// entity の Packer の依存 (#3788、#3790)。投稿を包む部品 (webpush、
	// NoteFieldResolver、handler) へ渡すので、それらを作るより前に組み立てる。
	// どれも作るだけで、DB に触るのは metaRepo.Fetch だけ (上で既に読んで
	// キャッシュにある)。
	// PackUserLite が avatarDecorations の各エントリに url を埋め込めるよう
	// 共有 catalog resolver を entity package に登録する (#521)。catalog は
	// admin 管理で低頻度更新のため 30s TTL の in-memory cache で十分。
	avatarDecorationResolver := avatardecoration.NewResolver(avatarDecorationRepo)
	// PackUserLite の canChat を role policy 由来 (= upstream
	// `chatAvailability === "available"`) に揃える (#988)。roleService 自体
	// が in-memory cache を持つ (#761) ので追加 DB 負荷は実質ゼロ。
	canChatLookup := corerole.NewCanChatLookup(roleService)
	// PackUserLite / PackUserDetailed の `badgeRoles` / `roles` field を
	// role assignment から populate (#1103)。旧 mk-go は両 field を空配列
	// hard-code していたため、admin が公開ロールを assign しても profile
	// に表示されなかった。roleService.GetUserRoles も in-memory cache 経由
	// (#761) で hot path コスト無し。
	userRolesLookup := corerole.NewUserRolesLookup(roleService)
	// remote user の badgeRoles を出すかは meta.showRoleBadgesOfRemoteUsers
	// 次第 (upstream)。CachedMetaRepository.Fetch は cache 経由で hot path
	// コスト無し。fetch 失敗時は false (= local-only の保守的 default) (#1653)。
	showRemoteBadges := func() bool {
		m, err := metaRepo.Fetch()
		return err == nil && m != nil && m.ShowRoleBadgesOfRemoteUsers
	}
	// local system account (relay.actor 等) の avatarUrl を meta.iconUrl に
	// する upstream の特例 (#1781)。IdenticonURL が '.' を含む local username の
	// fallback でのみ参照する。
	instanceIconURL := func() string {
		m, err := metaRepo.Fetch()
		if err != nil || m == nil || m.IconURL == nil {
			return ""
		}
		return *m.IconURL
	}
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
	// PackUserDetailed の isSilenced を role policy 由来に揃える (旧実装は
	// /api/i 以外で常に false 固定だった)。roleService.IsSilenced が
	// entity.SilencedLookup を構造的に満たすので直接 wire する。GetUserRoles は
	// in-memory cache (#761) だが IsSilenced 内の policy merge 自体は per-call
	// なので、UserDetailed pack は単一 user の detail 取得が主用途であり許容範囲。
	entityPackerDeps := entity.PackerDeps{
		AvatarDecoration: avatarDecorationResolver,
		EmojiDecoration:  emojiDecorationResolver,
		CanChat:          canChatLookup,
		UserRoles:        userRolesLookup,
		ShowRemoteBadges: showRemoteBadges,
		InstanceIconURL:  instanceIconURL,
		Silenced:         roleService,
	}
	// 部品へ渡す Packer (#3790)。組み立てごとに作り、プロセスの状態は書き換えない
	// (#3800 で既定の Packer と Set* を本番から消した)。メディアのプロキシ
	// (mediaURLCtx) は構築子の必須の引数で渡す (#3834)。無いとリモートのメディアの
	// URL を生のまま出し、閲覧者の IP がリモートへ漏れる。
	entityPacker := entity.NewPacker(mediaURLCtx, entityPackerDeps)
	// reversi の対局の publisher。招待者 (UserLite) を包む Packer は構築子の必須の引数
	// (#3794、#3827)。#3676 から setupRoutes の先頭で作っていたが、Packer を渡すために
	// ここへ移した。作るだけで Redis に繋がず、使う側 (連合の processor・reversi の
	// service と handler) はどれもここより後にある。
	reversiPublisher := stream.NewReversiGamePublisher(streamPubSub, entityPacker)

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
		Memo:          userMemoRepo,
		Relation:      userRelationRepo,
	}
	// follow / unfollow の Webhook は本家と同じく、フォローした側から見た
	// UserDetailedNotMe で相手を送る。followed は UserLite (#3269)。
	// main stream の同じイベントと blocking/create・delete の応答も同じ packer で
	// 組む (#3330)。ピン留めと移行先を埋める側 (users の handler) は後で組み立てる
	// ので、そこで SetDetailExtras する。
	sharedUserPacker := userpack.New(entityPacker, userpack.Lookups{
		Instances:  instanceRepo,
		Emojis:     emojiRepo,
		Profiles:   userRepo,
		Relations:  listRelationRepos,
		Moderators: roleService,
	}, idGen)

	// Core services (#3686)。作るのは buildCore (wiring_core.go) で、ここでは今までと
	// 同じ名前の別名を置くだけにする。移したのは、作るだけで DB にも Redis にも外部にも
	// 触らず、goroutine も shutdown hook も持たず、依存が repository と先に作った core
	// だけのもの。roleService は上の buildRole で作って引数で渡す (#3825)。**残った setter は、
	// 下の元の位置のまま呼ぶ** (#3825 で構築子の引数に置き換えたものを除く)。
	// 構築の途中で動く処理から見える配線を変えないため (docs/architecture.md 4.1)。
	// 副作用のあるものと、stream・handler・連合の部品に依存するものは、下の元の位置で作る。
	// **別名は変数 coreSvcs から取る。** w.core から取ると、別名を代入より前に書いても
	// コンパイルが通り、ゼロ値 (nil) を黙って受け取る (#3686 のレビュー)。
	coreSvcs := s.buildCore(w, idGen, roleService, streamPubSub, entityPacker, sharedUserPacker)
	w.core = coreSvcs
	featuredService := coreSvcs.featured
	hashtagService := coreSvcs.hashtag
	modLogService := coreSvcs.modLog
	reversiFedCache := coreSvcs.reversiFedCache
	totpReplayGuard := coreSvcs.totpReplayGuard
	webPushService := coreSvcs.webPush
	instanceService := coreSvcs.instance
	signupService := coreSvcs.signup
	noteCreateService := coreSvcs.noteCreate
	noteDeleteService := coreSvcs.noteDelete
	noteQueryService := coreSvcs.noteQuery
	userService := coreSvcs.user
	followingService := coreSvcs.following
	timelineFanoutHook := coreSvcs.timelineFanoutHook
	ephemeralStore := coreSvcs.ephemeralStore
	timelineService := coreSvcs.timeline
	channelService := coreSvcs.channel
	antennaService := coreSvcs.antenna
	clipService := coreSvcs.clip
	reactionService := coreSvcs.reaction
	notificationService := coreSvcs.notification
	notificationHook := coreSvcs.notificationHook
	webhookService := coreSvcs.webhook
	webhookNoteHook := coreSvcs.webhookNoteHook
	webhookReactionHook := coreSvcs.webhookReactionHook
	webhookFollowingHook := coreSvcs.webhookFollowingHook
	blockingService := coreSvcs.blocking
	mutingService := coreSvcs.muting
	renoteMutingService := coreSvcs.renoteMuting
	pollService := coreSvcs.poll
	localDriveStorage := coreSvcs.localDriveStorage
	driveStorage := coreSvcs.driveStorage
	driveService := coreSvcs.drive
	imgProcessor := coreSvcs.imgProcessor
	driveReader := coreSvcs.driveReader
	sysAcctSvc := coreSvcs.sysAcct
	listProxyFollow := coreSvcs.listProxyFollow
	retentionSvc := coreSvcs.retention
	achievementService := coreSvcs.achievement
	proxyAllowlist := coreSvcs.proxyAllowlist
	abuseInAppNotifier := coreSvcs.abuseInAppNotifier

	// 以下は #3676 で受け手より先に作るようにした部品のうち、buildCore へ移さなかった
	// もの (passwordFailureGuard・proxyAccountResolver・dbHealth) と、buildCore へ移した
	// 部品 (featuredService・hashtagService など) への setter。inboxSoftwareChecker は
	// 下の buildFederation で作る (#3688)。
	// 部品はどれも作るだけで DB にも Redis にも触らず、goroutine も起動しない。
	// 受け手への setter の呼び出しは元の位置のまま。
	//
	// meta.hiddenTags / meta.sensitiveWords を見て tag の record / featuring
	// を skip するため metaRepo を inject (upstream HashtagService.updateHashtags
	// Ranking と同 semantics、未配線だと sensitive tag が featured に出る
	// drop-in regression が再発するので必ず wire する)。
	hashtagService.SetMetaRepo(metaRepo)
	// 現在のパスワードを照合する i/ 配下の API の照合失敗をアカウント単位で数える。
	// route ごとの limiter に置くと、token を持つだけの第三者が被害者の
	// i/regenerate-token を使い切れる (passwordguard の package doc)。
	passwordFailureGuard := passwordguard.NewRedisGuard(s.redis.Default)
	// proxy account の username。meta / nodeinfo の handler と、frontend の HTML が共有する。
	proxyAccountResolver := newProxyAccountResolver(systemAccountRepo, userRepo)
	// DB の健全性 (#3095)。統計はプライマリから読み、1 分キャッシュする。
	dbHealth := dbhealth.NewService(s.db, s.config.DBReplications && len(s.config.DBSlaves) > 0)
	// ドライブ使用量の集計 (#3053)。キャッシュの一覧に登録するので (#3738)、管理画面の
	// handler に渡す所より前に作る。
	driveUsage := driveusage.NewService(repository.NewDriveUsageRepository(s.db), driveusage.DefaultTTL, driveusage.DefaultTopN)
	// 連合の部品 (#3688)。作るのは buildFederation (wiring_fed.go) で、ここでは今までと
	// 同じ名前の別名を置く。呼ぶ位置は、移した部品の中で最初に作っていた
	// inboxSoftwareChecker の位置。移した部品は作るだけで副作用を持たないので、
	// apURLs や deliverService を作る位置が前へ移っても、DB と Redis の文の順は
	// 変わらない。setter はそれぞれ元の位置のまま。
	// **別名は変数 fedSvcs から取る。** w.fed から取ると、別名を代入より前に書いても
	// コンパイルが通り、ゼロ値を黙って受け取る (docs/architecture.md 4.2)。
	fedSvcs := s.buildFederation(w, coreSvcs, idGen)
	w.fed = fedSvcs
	inboxSoftwareChecker := fedSvcs.inboxSoftwareChecker
	apURLs := fedSvcs.apURLs
	apRenderer := fedSvcs.apRenderer
	deliverService := fedSvcs.deliver
	relaySvc := fedSvcs.relay
	noteDeliveryHook := fedSvcs.noteDeliveryHook
	followingDeliveryHook := fedSvcs.followingDeliveryHook
	quoteRequestDeliveryHook := fedSvcs.quoteRequestDeliveryHook
	noteDeleteHook := fedSvcs.noteDeleteHook
	deliveryBreaker := fedSvcs.deliveryBreaker
	userModerationDeliveryHook := fedSvcs.userModerationDeliveryHook
	profileUpdateHook := fedSvcs.profileUpdateHook
	pinDeliveryHook := fedSvcs.pinDeliveryHook

	// Core services
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
	// ActivityPub 配信のためにローカルユーザーは RSA 鍵対を必要とする。
	signupService.SetKeypairRepo(keypairRepo)
	// FEP-521a Multikey 対応で Ed25519 鍵対も併発行 (#1067 / #1068)。
	signupService.SetKeypairExtraRepo(keypairExtraRepo)
	noteCreateService.SetMainStreamPublisher(mainStreamPublisher)
	noteCreateService.SetChartHook(chartHooks)
	// Phase 7-1 follow-up (#254): 本家互換の残存 error 検出ロジック用依存を注入。
	// blockingRepo・driveFileRepo・metaRepo と silencing (#1024) は、buildCore で
	// 構築子に渡す (#3825)。
	noteCreateService.SetChannelRepo(channelRepo)
	// mentionLimit を role policy から引く (#2321)。未配線だと
	// DefaultMentionLimit 固定になり、ロールで設定した値が効かない。
	noteCreateService.SetRolePolicyProvider(roleService)
	// reply/mention の main-stream event をスレッドミュート済 recipient に出さない (#1954)。
	noteCreateService.SetThreadMutingRepo(noteThreadMutingRepo)
	noteDeleteService.SetUserRepo(userRepo) // #1538: resolve author for moderator delete
	noteDeleteService.SetNoteStreamHook(&noteDeleteStreamAdapter{pub: noteEventPub})
	noteDeleteService.SetChartHook(chartHooks)
	noteQueryService.SetFavoriteRepo(noteFavoriteRepo)
	noteQueryService.SetThreadMutingRepo(noteThreadMutingRepo)
	userService.SetMainStreamPublisher(mainStreamPublisher)
	// PinNote の上限を role policy `pinLimit` で override 可能にする (#1029)。
	userService.SetRolePolicyProvider(roleService)
	// /api/i/update の avatarId / bannerId 経路 (#467) で drive_file の
	// 所有権検証 + URL コピーが必要なため、driveFileRepo を配線する。
	userService.SetDriveFileRepository(driveFileRepo)
	// users/search-by-username-and-host で host=<self-hostname> 入力時に
	// local 限定にする upstream 互換 remap (#1064) のため、自 hostname を
	// Service に渡す。空文字なら remap は "." 一致のみになる。
	userService.SetSelfHostname(s.config.Hostname)
	followingService.SetRelationReloadPublisher(relationReloadPublisher)
	followingService.SetMainStreamPublisher(mainStreamPublisher)
	followingService.SetChartHook(chartHooks)
	// remote instance の followersCount / followingCount を Follow/Unfollow 時に
	// incremental 更新する (#596)。未配線でも本機能には影響しないが、admin
	// dashboard の federation pie chart が起動直後以外で 0 に偏る。
	followingService.SetInstanceRepo(instanceRepo)
	// 本家と同じく meta.enableStatsForFederatedInstances が false なら集計列を
	// 動かさない (#3330)。cachedMeta なので follow ごとの DB 往復は無い。
	followingService.SetInstanceStatsGate(corefollowing.MetaInstanceStatsGate(metaRepo))

	// Timeline services (Redis-backed fanout)
	// 引用先が DB 側にある (= リレー由来でない投稿を引用している) ケースを
	// 埋められるようにする (#2397)。未配線だと Redis 側の引用先しか復元できず、
	// 残りが「削除された投稿」として描画される。
	ephemeralStore.SetNoteLookup(noteRepo)
	// timeline の hydrate が DB に無い ID を Redis から補えるようにする。
	// store 自体は常に配線し、機能の有効・無効は取り込み側で判定する
	// (無効化しても既に置かれているぶんは TTL 切れまで読めた方がよい)。
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
	// channels/update の moderator bypass (#1540): owner でなくても moderator
	// なら他人の channel を編集できる upstream 互換のため roleService を inject。
	channelService.SetModeratorChecker(roleService)
	// Phase 7-2 follow-up (#271): channel note 着信時に follower ごとの
	// unread row を作成して /api/i の hasUnreadChannel に反映する。
	channelService.SetUnreadRepo(channelNoteUnreadRepo)
	noteCreateService.SetChannelHook(corechannel.NewNoteCreateHook(channelService))

	// Antennas (Phase 4.3)
	antennaService.SetFollowingRepo(followingRepo)
	antennaService.SetUserListRepo(userListRepo)
	// excludeNotesInSensitiveChannel の判定 (upstream AntennaService:117)。
	antennaService.SetSensitiveChannelLookup(&sensitiveChannelLookup{repo: channelRepo})
	// 宙吊り ID の除去は「本当に DB に無いか」を primary で確かめてから行う
	// (#2719)。レプリカを見ると複製前の行を消してしまう。
	antennaService.SetPrimaryNoteExistence(noteRepo)
	// Phase 7-2 follow-up (#271): antenna 着信時に所有者の unread row を
	// 作成して /api/i の hasUnreadAntenna に反映する。
	antennaService.SetUnreadRepo(antennaNoteUnreadRepo)
	noteCreateService.SetAntennaHook(coreantenna.NewNoteCreateHook(antennaService))

	// Clips (Phase 4.4) の rolePolicyProvider と streamPublisher は、buildCore で構築子に
	// 渡す (#3825)。

	noteCreateService.SetFeaturedRanking(featuredService)

	// Reactions
	reactionService.SetFeaturedRanking(featuredService)
	reactionService.SetNoteStreamHook(&reactionNoteStreamAdapter{pub: noteEventPub})
	reactionService.SetChartHook(chartHooks)

	// Reactions buffering (issue #57): meta.enableReactionsBuffering が true なら
	// Redis にバッファし、30秒ごとに DB へ flush する。
	//
	// **書き込みのたびに設定に従う (#3663)。** 以前は起動時の値で writer の型を
	// 選んでいたので、管理画面で切り替えても作り直すまで効かなかった (#1780)。
	// 切り替えは下の metaUpdated の購読で行う。読めなければ無効で始める
	// (従来と同じ)。
	reactionBufferingAtStart := false
	if bufMeta, err := metaRepo.Fetch(); err == nil {
		reactionBufferingAtStart = bufMeta.EnableReactionsBuffering
	}
	reactionCountWriter := corereaction.NewSwitchableWriter(s.redis.Default, noteRepo, reactionBufferingAtStart)
	s.reactionCountWriter = reactionCountWriter
	reactionService.SetCountWriter(reactionCountWriter)

	// Notifications (Redis Streams)
	notificationService.SetNoteUnreadRepo(noteUnreadRepo)
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
		// 未対応の件数 (#3200)。取れなければ件数だけ出さない — 通知そのものは
		// 状態が引けているので落とさない。
		if n, err := abuseReportRepoForNotif.CountUnresolved(); err == nil {
			out.UnresolvedCount = &n
		} else {
			slog.Warn("notification: unresolved abuse report count failed", "err", err)
		}
		return out, true
	}
	followingService.SetNotificationHook(notificationHook)
	reactionService.SetNotificationHook(notificationHook)

	// Web Push (Phase 9.3): 通知作成後にサブスクライバーへpush配信する。
	notificationHook.SetWebPushPublisher(webPushService)
	webPushNotePacker := corewebpush.NewNoteRepoPacker(noteRepo, idGen, followingRepo, driveFileRepo, entityPacker)
	webPushUserPacker := corewebpush.NewUserRepoPacker(userRepo, entityPacker)
	notificationHook.SetPackers(webPushUserPacker, webPushNotePacker)
	// 1 件の通知の stream と Web Push が、ノートの読み込みを共有する (#3611)。
	notificationHook.SetNoteLoader(noteRepo.FindByIDWithRelations)
	// push は送る直前に、ノートがまだあるかだけを primary で確かめる (#3611)。
	notificationHook.SetNoteExistenceChecker(noteRepo)
	webPushCache := corewebpush.NewSubscriptionCache(swSubRepo, s.redis.Default)
	// Web Push delivery: webpush-go の HTTPClient field に SSRF-safe client
	// を注入して FCM / Mozilla / Apple endpoint への配送も outbound 政策
	// (forward proxy / outgoingAddress 等) に従わせる (#638)。
	webPushSender := processors.LibraryWebPushSender{HTTPClient: s.outboundClient(30 * time.Second)}
	webPushProcessor := processors.NewWebPushProcessor(
		webPushCache, swSubRepo, metaRepo, webPushSender, s.config.URL,
	)

	// Webhook (Phase 9.5): ユーザー/システムwebhookの配信を非同期処理する。
	// reply/mention webhook をスレッドミュート済 recipient に出さない (#1965)。
	webhookNoteHook.SetThreadMutingRepo(noteThreadMutingRepo)
	// note payload の embed (renote/reply/引用先) を受信者の可視性で gate する。
	webhookNoteHook.SetFollowingRepo(followingRepo)
	noteCreateService.SetWebhookHook(webhookNoteHook)
	webhookReactionHook.SetFollowingRepo(followingRepo)
	reactionService.SetWebhookHook(webhookReactionHook)
	followingService.SetWebhookHook(webhookFollowingHook)
	webhookSignupHook := corewebhook.NewSignupHook(webhookService, entityPacker) // #3823: Packer は構築子の必須の引数
	signupService.SetWebhookHook(webhookSignupHook)
	// Webhook delivery: SSRF-safe transport + forward proxy 経由で user-supplied
	// URL に POST する (#638)。Timeout は processors 側の DefaultWebhookTimeout
	// に揃える。
	webhookProcessor := processors.NewWebhookProcessor(webhookRepo, systemWebhookRepo, s.outboundClient(processors.DefaultWebhookTimeout), s.config.Host)

	// Blocking & Muting
	blockingService.SetRelationReloadPublisher(relationReloadPublisher)
	mutingService.SetRelationReloadPublisher(relationReloadPublisher)
	renoteMutingService.SetRelationReloadPublisher(relationReloadPublisher)
	followingService.SetBlockingChecker(blockingService)
	notificationHook.SetMuteChecker(mutingService)

	// Polls
	// pollVoted を note の stream topic に publish して subscribe 中の
	// frontend (note 詳細 / timeline) が reload なしで count を更新できる
	// ようにする (#690)。
	pollService.SetEventPublisher(noteEventPub)
	pollService.SetNotificationHook(notificationHook)
	// poll federation hook の deliverService は buildFederation で作る (#3688)。
	// `pollService.SetFederationHook(...)` の呼び出しは、setter を元の位置に残す
	// 規則 (docs/architecture.md 4.1) により、下方の wire-up の位置のまま。
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
		s.addWorker("pollExpiryWorker", pollExpiryWorker.Run)
	}

	// Drive storage (Phase 4.9: Meta に基づいて Local / S3 を切り替え)
	driveService.SetStreamingPublisher(drivePublisher)
	// folder の folderCreated/folderUpdated/folderDeleted も同じ drive channel
	// publisher で配信する (#1564)。
	driveService.SetFolderStreamingPublisher(drivePublisher)
	driveService.SetMainStreamPublisher(mainStreamPublisher)
	driveService.SetChartHook(chartHooks)
	// storedInternal=true な行 (object storage 有効化より前 / TS 時代に保存された
	// ファイル) はローカル FS にあるので、削除経路がそちらに届くようにする。
	driveService.SetLocalStorage(localDriveStorage)
	// ファイル削除時の object storage 実体削除を queue に逃がす (#2325)。
	driveService.SetObjectDeleteEnqueuer(s.queueClient)
	// 分割アップロード (#2313)。設定も backend (#2315) も meta から都度読むので、
	// admin の変更は再起動なしで効く。オブジェクトストレージ未使用の構成では
	// backend が MultipartStorage を満たさず、配線しても機能は無効のままになる。
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
	// storedInternal=true な行 (object storage 有効化より前に保存されたファイル)
	// はローカル FS にあるので、import がそちらも読めるようにする (#2315)。
	driveReader.SetLocalStorage(localDriveStorage)
	exportProcessor := processors.NewExportProcessor(exporter)
	// queue の processor (#3690)。作るのは buildQueue (wiring_queue.go) で、ここでは
	// 今までと同じ名前の別名を置く。呼ぶ位置は、移した processor の中で最初に作って
	// いた importProcessor の位置。移した processor は作るだけで副作用を持たないので、
	// cleanProcessor や objectStorageProcessor を作る位置が前へ移っても、DB と Redis の
	// 文の順は変わらない。setter はそれぞれ元の位置のまま。outbound client を使うもの
	// (webPush・webhook) と、setupRoutes で作る部品に依存するもの (export・deliver・
	// inbox など) は、下の元の位置で作る。
	// **別名は変数 queueSvcs から取る。** w.queue から取ると、別名を代入より前に書いても
	// コンパイルが通り、ゼロ値を黙って受け取る (docs/architecture.md 4.2)。
	queueSvcs := s.buildQueue(w, coreSvcs, idGen)
	w.queue = queueSvcs
	importProcessor := queueSvcs.importProcessor
	cleanProcessor := queueSvcs.cleanProcessor
	orphanUserProcessor := queueSvcs.orphanUserProcessor
	orphanAttachmentProcessor := queueSvcs.orphanAttachmentProcessor
	checkExpiredMutingsProcessor := queueSvcs.checkExpiredMutingsProcessor
	archiveAnnouncementsProc := queueSvcs.archiveAnnouncementsProc
	cleanGenericProcessor := queueSvcs.cleanGenericProcessor
	chunkedUploadGCProcessor := queueSvcs.chunkedUploadGCProcessor
	objectStorageProcessor := queueSvcs.objectStorageProcessor
	unfollowProcessor := queueSvcs.unfollowProcessor
	followProcessor := queueSvcs.followProcessor
	blockProcessor := queueSvcs.blockProcessor
	unblockProcessor := queueSvcs.unblockProcessor
	retentionProc := queueSvcs.retentionProc

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

	// Search (Phase 4.6)
	// 設定に従って provider を選択する。Meilisearch が設定されていれば
	// それを使い、そうでなければ SQL ILIKE フォールバック。
	searchService := coresearch.NewService(buildSearchProvider(s.config, noteRepo, followingRepo, idGen))
	noteCreateService.SetIndexHook(coresearch.NewNoteIndexHook(searchService))
	noteDeleteService.SetIndexHook(coresearch.NewNoteIndexHook(searchService))

	// ActivityPub。apURLs と apRenderer は buildFederation で作る (#3688)。
	// actor の `published` (アカウントの作成日時) を ID から出す (#3465)。
	apRenderer.SetIDGenerator(idGen)
	// Mention tag を AP Note に埋め込むための resolver。連合OFF/PENDING利用者は
	// tagとして外部へ列挙しない。
	mentionResolver := corefederation.NewUserMentionResolver(userRepo, apURLs)
	mentionResolver.SetLocalFederationObserver(coreSvcs.localFederation)
	apRenderer.SetMentionResolver(mentionResolver)
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
		// `@user@<自ホスト>` をローカルの利用者として解決する (#3330)。
		// 既定ポートは剥がす (本家の config.host は `new URL().host`)。
		noteCreateService.SetLocalHost(idnhost.HostPort(u))
	}
	// AP outbound client: SSRF-safe transport を適用 (#323)。
	// config.AllowedPrivateNetworks で開発時の self-loop を許可できる。
	// config.Proxy / ProxyBypassHosts も #485 で wire 済み。
	apHTTPClient := s.outboundClient(30 * time.Second)
	apClient := activitypub.NewClient(apHTTPClient, s.config.UserAgent)
	// meta.allowExternalApRedirect は外向きの取得には使わない。upstream と同じく、
	// 自分が返す外部への 3xx を止める設定 (apHandler.SetExternalRedirectPolicy)。
	// instance actor の秘密鍵は世代に 1 つだけ持ち、AP の取得とプラグインの peer の両方へ
	// 同じ値を渡す (#3732)。2 つ作ると同じ鍵のキャッシュが 2 つになり、片方を捨てても
	// 意味が無い。受け手 (apFetcher) より先に作る (後から入れる辺を増やさない、4.1)。
	instanceSigner := newInstanceActorSigner(sysAcctSvc, keypairRepo, apURLs)
	apFetcher := corefederation.NewAPFetcher(apClient)
	// AP attachment の先頭取得 (#461 / #3243) 用 outbound HTTP client。
	// SSRF-safe transport で内部 IP / cloud metadata エンドポイントへの
	// アクセスを拒否する。timeout は先頭だけの単発 fetch なので apHTTPClient
	// (30s) より短めの 10s にする。
	attachmentProbeClient := s.outboundClient(10 * time.Second)
	// 構築子で渡す依存 (#3833):
	//   - ホワイトリスト連合 (federation: specified) / blockedHosts に対する gate を
	//     resolver の入口 (fetchActor / resolveNoteOnce / IngestNoteWithCreated) に
	//     適用する。deliver_service / inboxProcessor と同じ instanceService を共有する。
	//     meta.prohibitedWords と meta.sensitiveWords も同じ instanceService から読む
	//   - 受信を拒否するソフトウェア (#3362) のホストの投稿・actor も、リノートや返信の
	//     連鎖、ap/show で取り込まない (#3563)。inbox と同じく判定のたびに meta を読む。
	//     メンションの解決 (remoteUserResolver) は構築子で同じ checker を受け取る
	//   - silenced host の public note を home 降格 (#2106 N14)
	//   - 受信した note の mentionLimit を投稿者 (リモート) の role policy から引く (#3330)
	//   - 凍結の由来の記録先。無いと actor の `toot:suspended` を読まない — 由来を持てない
	//     状態で自動凍結すると、モデレーターが解除しても次の refresh で無言で戻る (#2973)
	//   - 世代の停止で待つ、新しいノートの chart hook の箱 (#3661)
	federationResolver := corefederation.NewResolver(userRepo, noteRepo, apURLs, apFetcher, idGen,
		instanceService, inboxSoftwareChecker, instanceService, roleService, suspensionOriginRepo, s.background)
	federationResolver.SetChartHook(chartHooks)
	// resolver が新しく作った note (inbound Create、返信元・引用・Announce の
	// 対象など) は resolver 側で数える (#3330)。processor は Announce の renote
	// 行だけを数えるので、こちらも配線しないと Create が一切数えられない。
	federationResolver.SetNoteChartHook(chartHooks)
	federationResolver.SetPublickeyRepo(publickeyRepo)
	// FEP-521a Multikey 対応で remote actor の assertionMethod[] を parse して
	// user_publickey_extra に persist する (#1067 / #1070)。
	federationResolver.SetPublickeyExtraRepo(publickeyExtraRepo)
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
	federationResolver.SetAttachmentProbeClient(attachmentProbeClient)
	// 自ホストの base URL (inbound の object がローカルユーザーを指すかの判定と、relay の
	// Accept / Reject の検査)、Block / Undo(Block) と reversi の Invite のブロック判定に使う
	// blockingService、世代の停止で待つ受信後の hook の箱 (#3661) は構築子で渡す (#3833)。
	federationProcessor := corefederation.NewProcessor(federationResolver, followingService, reactionService, noteDeleteService, userRepo, noteRepo,
		s.config.URL, blockingService, s.background)
	// federation 経由のリモートノート (Create / Announce) も PerUserNotesChart
	// 等の note 系 chart に +1 を記録するために配線する (#1156)。これが無いと
	// リモートユーザーのプロフィール「アクティビティ」タブの heatmap が空に
	// なり、削除 (-1) だけが note_delete_service 経由で記録されてマイナス側に
	// しか動かなくなる drop-in regression が発生する。
	federationProcessor.SetNoteChartHook(chartHooks)
	// 連合 inbox 経由の invite 受信時にも local 被招待者の reversi stream に
	// `invited` を push する (#417 P2: リアルタイム招待)。
	federationProcessor.SetReversiStreamPublisher(reversiPublisher)
	// instance actor に見せなかった Announce の対象を、actor をフォローして
	// いるローカルの利用者の鍵で取り直す (#3916)。連合に出していない利用者の鍵は
	// 使わない (coreSvcs.localFederation で判定)。
	federationProcessor.SetFollowerFetchKeys(corefederation.NewLocalFollowerKeys(followingRepo, userRepo, keypairRepo, blockingRepo,
		apURLs, coreSvcs.localFederation))

	// users/show 経由で host が指定されたリモートユーザーをローカル DB に
	// キャッシュが無くても解決できるようにする (#269)。webfinger で actor URI
	// を引いてから federationResolver.ResolveActor で User row を upsert する。
	// redirect 追跡必須のため apClient とは *http.Client を分離する。Timeout は
	// user-facing API 経由で呼ばれるので応答性優先で 10s に設定する。
	webfingerClient := activitypub.NewWebFingerClient(s.outboundClient(10*time.Second), s.config.UserAgent)
	// notes/create の DB に無いリモートの利用者へのメンションも同じ経路で取りに行く
	// (本家 NoteCreateService.extractMentionedUsers → RemoteUserResolveService、#3330)。
	// 連合しないホストには WebFinger を投げない (hostBlocker は下の
	// remoteUserResolver.SetHostBlockChecker で渡す)。
	//
	// **1 つの instance を共有する。** 同じ acct の解決・再同期を singleflight で
	// まとめるので、users/show と notes/create (と reversi) が別々に持つと、
	// 同じ acct への WebFinger が経路の数だけ並ぶ。
	//
	// 受信を拒否するソフトウェア (#3362) のホストの利用者も、ドメインブロックと同じく
	// WebFinger を投げる前に止める (#3563)。構築子の必須の引数 (#3823)。
	remoteUserResolver := corefederation.NewRemoteUserResolver(
		webfingerClient, federationResolver, userRepo, localHost, inboxSoftwareChecker,
	)
	userService.SetRemoteUserResolver(remoteUserResolver)
	noteCreateService.SetRemoteUserResolver(remoteUserResolver)

	// Instance management (Phase 3 Step H)。instanceService は先頭近くで作る (#3676)。
	// goneSuspended になった時刻を残す (#3067)。消えたインスタンスとのフォロー関係を
	// 片付ける候補を「いつから消えているか」で並べる。
	instanceService.SetGoneRecorder(goneInstanceRepo)
	federationResolver.SetInstanceTracker(instanceService)
	remoteUserResolver.SetHostBlockChecker(instanceService)
	federationResolver.SetMediaSilencedHostChecker(instanceService) // #3218: media silenced host の添付をセンシティブに
	// 連合のルール (#3090)。ホスト単位の設定 (上の hostBlocker / silenced) に
	// 追加の層として重ねる。activity のルールは inbox の署名検証の後
	// (dispatchActivity の入口)、投稿のルールは取り込みの全経路で評価する。
	fedRuleHits := fedrule.NewHitStore(s.redis.Default)
	fedRuleService := fedrule.NewService(fedRuleRepo, fedRuleHits, idGen.ParseTime)
	federationResolver.SetRuleEvaluator(fedRuleService)
	federationProcessor.SetRuleEvaluator(fedRuleService)
	// 起動はStartBackgroundで行う(#3703)。起動せずに止めても、Stopは停止の印を立てるだけで
	// 済む。止め方はStartの中で作ったctxを切るだけで、goroutineは待たない(#3717)。
	s.addStartStopper(context.Background(), "fedRuleHits", fedRuleHits)
	// 新規 instance row 発見時に nodeinfo を取得して metadata を更新する。
	// admin/federation/refresh-remote-instance-metadata でも同じ fetcher を
	// 再利用して on-demand で再取得する (#351 フォロー)。apFetcher は redirect の
	// 飛び先を返す取得 (FetchJSONWithFinalURL) を持ち、構築子が型で求める (#3833)。
	metadataFetcher := coreinstance.NewFetchMetadataService(instanceRepo, apFetcher)
	instanceService.SetMetadataFetcher(metadataFetcher)
	// 受信と配送のたびに、24 時間より古ければ nodeinfo を取り直す (#3564)。
	// 本家の fetchInstanceMetadata と同じく、プロセスをまたいだ重複は Redis の
	// ロックで防ぐ。取り直しは受信や配送のジョブとは切り離して走る。終了時は
	// 新しい取り直しを止め、走っているものは短く待つだけにする (後に並ぶ hook の
	// 時間を使い切らないため)。
	metadataRefresher := coreinstance.NewMetadataRefresher(instanceRepo, metadataFetcher,
		coreinstance.NewRedisRefreshLock(s.redis.Default))
	s.addCloser("metadataRefresher", metadataRefresher)

	// inbox worker から呼ばれる MarkRequestReceived を per-host で 1s buffer
	// に集約。同一 remote host の連続 inbox 受信で 10k UPDATE が 1 UPDATE に
	// 縮退する (#569)。
	instanceTouchBuffer := coreinstance.NewTouchBuffer(instanceService, time.Second)
	s.addStartCloser(context.Background(), "instanceTouchBuffer", instanceTouchBuffer)

	// 署名方式の観測 (#2393)。inbound は同一 host から高頻度に来るので、
	// TouchBuffer と同じ縮退方式で per-host per系統 1 UPDATE にまとめる。
	//
	// flush 間隔を TouchBuffer の 1s より長い 60s にしているのは、こちらが表示用の
	// メタデータで秒単位の鮮度を要求しないため。1s のままだと連合先の数だけ毎秒
	// UPDATE が増え、instance touch の書き込み負荷を実質倍にしてしまう。相手の
	// 署名方式はそう頻繁に変わらないので、60s 窓で十分に追随できる。
	sigCapBuffer := coreinstance.NewCapabilityBuffer(sigCapRepo, time.Minute)
	s.addStartCloser(context.Background(), "sigCapBuffer", sigCapBuffer)
	// 宣言 (assertionMethod の Ed25519) は actor resolve 経路で同期に書く。
	// inbound ほど高頻度ではないので buffer を挟まない。
	federationResolver.SetSignatureCapabilityDeclarer(sigCapRepo)

	// AP delivery: DeliverService + フック登録 + queue processor 登録。
	// deliverService と配送の hook は buildFederation で作る (#3688)。host のブロックの
	// 判定は構築子で渡す (#3833)。
	// FEP-521a Multikey 対応で recipient capable + sender Ed25519 鍵あり 経路で
	// Ed25519 sign を試行できるよう wire (#1067 / #1071)。
	deliverService.SetKeypairExtraRepo(keypairExtraRepo)
	deliverService.SetPublickeyExtraRepo(publickeyExtraRepo)
	// test (#780) で queue を bypass して同期 deliver する hook を後付けで
	// 差し替えられるよう、Server から参照を保持する。
	s.deliverSvc = deliverService

	// poll federation hook: ローカル user が remote poll に投票したら AP Note
	// (with name + inReplyTo) を author の inbox に配信する (#690)。
	pollService.SetFederationHook(corefederation.NewPollDeliveryHook(apRenderer, deliverService, userRepo, apURLs, idGen))

	// Relay 連携 (#161): relay actor system account + relay.Service。
	// relay.Service は buildFederation で作る (#3688)。admin/relays ハンドラへ注入し、
	// Accept/Reject inbox 処理で status を更新できるように processor にも渡す。
	// FEP-521a Multikey 対応で system account も Ed25519 鍵対を併発行 (#1067 / #1068)。
	sysAcctSvc.SetKeypairExtraRepo(keypairExtraRepo)
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
	apFetcher.SetSigner(instanceSigner)

	noteDeliveryHook.SetRelayBroadcaster(relaySvc)
	noteCreateService.SetFederationHook(noteDeliveryHook)
	followingService.SetFederationHook(followingDeliveryHook)
	// inbound Follow に対する Accept 返送は processor から直接呼ぶ (original
	// Follow の id を保持したまま相手に返すため、service 層を経由しない)。
	federationProcessor.SetInboundFollowAcceptor(followingDeliveryHook)
	// FEP-044f: リモートからの QuoteRequest に答え、承認の実体を配る (#3234)。
	quoteRequestHandler := corefederation.NewQuoteRequestHandler(corefederation.QuoteRequestDeps{
		Notes:     noteRepo,
		Users:     userRepo,
		Blocks:    blockingService,
		Follows:   followingService,
		Approvals: quoteAuthorizationRepo,
		FetchNote: federationResolver.FetchNoteForVerification,
		Respond:   quoteRequestDeliveryHook,
		URLs:      apURLs,
		IDGen:     idGen,
	})
	federationProcessor.SetQuoteRequestHandler(quoteRequestHandler)
	// こちらの引用の承認を取りに行き (ローカル同士は自分で発行)、承認を
	// quoteAuthorization として配る (#3234 段階 3)。
	quoteOutbox := corefederation.NewQuoteOutbox(quoteRequestHandler,
		repository.NewNoteQuoteRequestRepository(s.db), quoteAuthorizationRepo, noteDeliveryHook)
	noteDeliveryHook.SetQuoteOutbox(quoteOutbox)
	federationProcessor.SetQuoteAnswerHandler(quoteOutbox)
	apRenderer.SetQuoteApprovalResolver(quoteOutbox.ApprovalURI)
	// 保留のまま残った QuoteRequest を毎分確かめて送り直す (#3238)。
	resendQuoteRequestsProcessor := processors.NewResendQuoteRequestsProcessor(quoteOutbox)
	// ブロックした相手の引用に出していた承認を取り消す (#3234 段階 4)。
	blockingService.SetQuoteRevoker(corefederation.NewQuoteRevoker(quoteAuthorizationRepo, userRepo, noteRepo,
		quoteRequestDeliveryHook, noteDeliveryHook, apURLs))
	reactionService.SetFederationHook(corefederation.NewReactionDeliveryHook(deliverService, apRenderer, apURLs, idGen, userRepo))
	// local user が remote user を (un)block した際に Block / Undo(Block) を
	// 相手 inbox へ配信する (#1560)。
	blockingService.SetFederationHook(corefederation.NewBlockingDeliveryHook(deliverService, userRepo, apRenderer))
	noteDeleteService.SetFederationHook(noteDeleteHook)
	// suspend / block されたインスタンスへは dispatch 時に配送を止める (#1404。第 2 引数)。
	// enqueue 時の isBlockedInbox を通り抜ける retry-backoff 中・積み残しジョブも弾く。
	// **署名鍵は payload に載せず、配送時に引く (第 3 引数)。** queue の job は
	// `admin/queue/jobs` が moderator へ返すので、載せると鍵がそこから読める。
	// deliverSuspendedSoftware: 対象インスタンスへの配送をスキップする (第 4 引数)。設定は
	// 判定のたびに meta から読むので、常に配線する (#3558。以前は起動時に設定が
	// 空なら配線せず、後から設定しても再起動するまで効かなかった)。
	// 配送に成功した相手の nodeinfo を取り直す (#3564。第 5・第 6 引数)。本家と同じく
	// enableStatsForFederatedInstances が有効なときだけ。
	deliverProcessor := processors.NewDeliverProcessor(apClient,
		instanceService,
		processors.NewRepoSigningKeySource(keypairRepo, keypairExtraRepo),
		corefederation.NewSuspendedChecker(metaRepo, instanceRepo),
		metadataRefresher, corefollowing.MetaInstanceStatsGate(metaRepo))
	deliverProcessor.SetChartHook(chartHooks)
	// 配信結果に応じて instance.isNotResponding を更新する
	deliverProcessor.SetResponseHook(instanceService)
	// FEP-521a Multikey 対応で host 単位の Ed25519 degrade flag を Redis に
	// 持つ (#1067 / #1071)。Ed25519 sign 失敗時 5min 同 host を RSA only に
	// 縮退する safety net。
	deliverProcessor.SetRedis(s.redis.Default)
	// Ed25519 署名の配送が 2xx を返した (= 同期的には拒否されなかった) ことを
	// 記録する (#2393)。
	deliverProcessor.SetSignatureCapabilityRecorder(sigCapBuffer)

	// Inbox (#534 / #565): HTTP handler は body+signature 関連 header を
	// payload に詰めて 202 即返し。worker 側 (本 processor) で signature
	// 検証 + host block + instance touch + chart hook + Process を実行
	// する。これにより handler 同期の RSA-2048 verify (~1-2ms) が消えて
	// inbound throughput が ~2x に改善する (#565)。順序保証は各 activity
	// handler の冪等性で吸収する Misskey TS 互換戦略。
	// 第 4 引数は受信を拒否するソフトウェア (#3362)。設定は判定のたびに meta から読む。
	// 第 5 引数で、受信のたびに相手の nodeinfo を取り直す (#3564)。受信を拒否した相手も
	// 取り直し、拒否の判定を新しい版で行えるようにする。
	// 第 6 引数は処理済み activity の再投函を弾く guard。署名の Date に窓を入れても、
	// 窓の内側はまだ投げ直せる。**Default インスタンスを使う** — 2FA の replay guard と
	// 同じ用途 (短命の使用済みマーカー) で、timelines / reactions のような
	// 用途別インスタンスに載せる理由が無い。
	inboxProcessor := processors.NewInboxProcessor(federationProcessor,
		federationResolver,
		instanceService,
		inboxSoftwareChecker,
		metadataRefresher,
		processors.NewRedisInboxReplayGuard(s.redis.Default))
	inboxProcessor.SetChartHook(chartHooks)
	// LD-Signature 検証で解決する著者に「リレー由来」の印を付ける (#2340)。
	inboxProcessor.SetRelayActorChecker(relaySvc)
	inboxProcessor.SetInstanceTracker(instanceTouchBuffer)
	inboxProcessor.SetSignatureCapabilityRecorder(sigCapBuffer)
	// LD-Signature verifier (#1164 Phase D)。inbound activity body に signature
	// field があれば RsaSignature2017 + 2026.5.4 hardening を実行する。signature
	// 無し / verify pass の activity は素通し、verify fail は drop。
	inboxProcessor.SetLDSignatureVerifier(corefederation.NewLDSignatureVerifier(publickeyRepo))

	// 定期と後続のジョブの processor、chart の管理と cron、配送と受信の健全性、インスタンスの
	// メタデータの取り直し、起動時の retention の集計 (#3901 で関数へ移した。呼ぶ位置と中の文の
	// 順は元のまま。部品を一覧へ登録する順も変わらない。作った部品は同じ名前で受ける)。
	flushProcessor, deleteAccountProcessor, deliveryHealth, inboxHealth, chartProcessor, instanceRefreshProc := s.wireBackgroundJobs(backgroundJobsDeps{
		relationReloadPublisher:      relationReloadPublisher,
		userRepo:                     userRepo,
		noteRepo:                     noteRepo,
		cachedMeta:                   cachedMeta,
		followingRepo:                followingRepo,
		driveFileRepo:                driveFileRepo,
		instanceRepo:                 instanceRepo,
		pageRepo:                     pageRepo,
		bubbleVersusRecordRepo:       bubbleVersusRecordRepo,
		chartCharts:                  chartCharts,
		chartHooks:                   chartHooks,
		hashtagService:               hashtagService,
		noteCreateService:            noteCreateService,
		userService:                  userService,
		retentionSvc:                 retentionSvc,
		deliveryBreaker:              deliveryBreaker,
		reactionCountWriter:          reactionCountWriter,
		webPushCache:                 webPushCache,
		cleanProcessor:               cleanProcessor,
		checkExpiredMutingsProcessor: checkExpiredMutingsProcessor,
		cleanGenericProcessor:        cleanGenericProcessor,
		federationResolver:           federationResolver,
		metadataFetcher:              metadataFetcher,
		deliverProcessor:             deliverProcessor,
		inboxProcessor:               inboxProcessor,
	})

	// Health check
	s.echo.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	// メンテナンスの画面が「終わった」を知るための口 (#3463)。メンテナンス中は
	// Supervisor のメンテナンスのハンドラーが同じパスに maintenance: true を返す。
	s.echo.GET(maintmode.StatusPath, maintenanceStatusOff)

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

	// POST / Match で登録する endpoint は、route の認証・権限の middleware の
	// 後ろに body が object かの検査が付く (requireObjectBody、#3330)。
	api := apiRoutes{s.echo.Group("/api")}

	// 本家 ApiCallService #sendApiError 互換の WWW-Authenticate 付与 (#1608)。
	// error envelope の kind を見て header を決めるため、応答を最初に
	// 仲介できる位置 (= group 最初の Use) に置く。後続 middleware / handler
	// が書く error 応答 (rate limit 429 / FST_ERR_CTP_* を含む) も全て
	// この writer を通る。

	// #2106 L56: upstream ApiServerService は /api scope の全応答に既定の
	// Cache-Control: private, max-age=0, must-revalidate を付与する。cacheSec endpoint は
	// 直後の middleware.APICacheControl が、資格情報の無い GET の成功応答だけ
	// public, max-age=... に上書きする (表は middleware.APICacheSec、#3593)。非 cacheSec の
	// allowGet GET を中間 proxy が誤キャッシュするのを防ぐ。
	api.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
			return next(c)
		}
	})
	// 本家で cacheSec を持つ endpoint は、資格情報の無い GET の成功応答だけ
	// public にする。表は middleware.APICacheSec の 1 か所 (#3593)。
	api.Use(middleware.APICacheControl())

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
	// read:accountを持たないapp tokenは、kindも資格情報の要求も無いendpoint
	// (notes/showなど)では匿名として扱う。rate limiterとIPの記録は利用者で
	// 数えたいので、その後ろに置く。kindを持つendpointではRequireScopeなどの
	// gateが利用者を戻す。
	api.Use(middleware.HideAppUserWithoutRead())
	// 本家のparamDefで引数を検証する。本家かElythiaの宣言のあるendpointは、
	// テストのモードでだけ登録するreset-dbを除いてすべて本家と同じ400で拒否し
	// (#3742、#3890で宣言から決める形にした。paramEnforcedEndpoints)、reset-dbでは
	// 応答を変えずに、handlerと判定が分かれた件数を数える(#3677)。検証そのものは
	// routeのmiddlewareの末尾(withParamShadow)で走る。
	if mw := s.paramShadowMiddleware(); mw != nil {
		api.Use(mw)
	}

	// Meta endpoint (public)
	metaHandler := meta.NewHandler(s.config, metaRepo)
	// **`requireSetup` をサーバー側の受け入れ条件と揃える (#3037)。**
	// `admin/accounts/create` の初回セットアップ窓は「`rootUserId` 未設定
	// **かつ**ローカル利用者 0」でしか開かないので、フロントの判定も同じに
	// する。揃えないと、`rootUserId` が NULL で利用者ありの DB で
	// **セットアップ画面が出続けて作成ボタンが必ず失敗する**。
	// 初回セットアップの判定は admin/accounts/create と同じ数え方にする。
	// プラグインが管理するアカウント (#3468) とシステムアカウントは数えない。
	// /api/meta と、frontend の shell に埋める meta (通常・OAuth の同意画面・
	// permalink の SSR・埋め込み) の全部へ同じ値を渡す (#3782)。
	setupDetector := meta.NewSetupDetector(func() (int64, error) { return userRepo.CountLocalUsersForSetup() })
	metaHandler.SetSetupDetector(setupDetector)
	metaHandler.SetAdRepo(adRepo)
	metaHandler.SetProxyAccountResolver(proxyAccountResolver)
	// 分割アップロード (#2313) の能力告知。未対応構成では field ごと出さないので、
	// フロントエンドは undefined を見て従来の単発アップロードに倒れる。
	chunkedUploadCapability := func() (int64, bool) {
		settings, ok := driveService.ChunkedUploadCapability()
		return settings.ChunkSize, ok
	}
	metaHandler.SetChunkedUploadCapability(chunkedUploadCapability)

	// Frontend SPA shell — AP resource handlers fall back to this when the
	// request prefers HTML over application/activity+json, and the final
	// catch-all route at the bottom of RegisterRoutes uses the same handler.
	frontend := frontendHTML(s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability, setupDetector)

	// URL preview endpoint
	//
	// **設定は毎回読む。** 起動時の値で固定すると、管理画面で URL プレビューを
	// 無効にしても (外向きの取得を止める緊急の手段)、再起動まで取りに行き続けた。
	// upstream の UrlPreviewService もその都度 meta を見る。metaRepo は cache 付きで、
	// update-meta で無効化される。
	urlPreviewFetcher := coreurlpreview.NewFetcher(coreurlpreview.Config{}, s.config.AllowedPrivateNetworks, s.outboundOpts()...)
	// 同じ相手への接続を他の外向きの経路と使い回す (#3591)。
	urlPreviewFetcher.SetTransport(s.outboundTransport())
	urlPreviewFetcher.SetConfigProvider(func() (coreurlpreview.Config, error) {
		m, err := metaRepo.Fetch()
		if err != nil {
			return coreurlpreview.Config{}, err
		}
		return urlPreviewConfig(m), nil
	})
	// urlPreviewSensitiveList は admin が随時変更するため live provider で
	// 渡す (metaRepo は cached、update-meta で invalidate される)。
	urlPreviewFetcher.SetSensitiveListProvider(func() []string {
		m, err := metaRepo.Fetch()
		if err != nil || m == nil {
			return nil
		}
		return m.URLPreviewSensitiveList
	})
	urlHandler := apiurl.NewHandler(urlPreviewFetcher, entityPacker) // #3798: プレビューの画像を media proxy へ通す
	// **接続元ごとの上限を付ける。** `/url` は認証不要で、1 回ごとにサーバーから
	// 外向きの取得を 1 本起こす。`/api` の外なので API の rate limit は掛からない。
	s.echo.GET("/url", urlHandler.Preview, middleware.RequireIPRateLimit(rateLimiter, middleware.URLPreviewRateLimitKey))

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
	signupService.SetUserPendingRepo(userPendingRepo)
	// パスワードをハッシュする5つの部品へ、設定のbcryptCostから作ったHasherを渡す(#3780)。
	signupService.SetPasswordHasher(s.passwordHasher)
	// **入れ忘れると黙って劣化する依存は、構築子の必須の引数で渡す (#3823)。**
	signupHandler := apisignup.NewHandler(signupService, metaRepo, idGen,
		s.background, // #3661: 世代の停止で待つ
		entityPacker, // #3792: 作成直後の利用者を MeDetailed で包む
		// #2673: signup も upstream と同じく実効 policy を返す (meta の base override /
		// role 集約 / server cap 込み)。素の default を返すと /api/i と食い違う。
		roleService,
	)
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
	// テーブルに閉じ込める。user 行にすると、user を読む全ての経路に「承認待ちを
	// 除く」条件が要る (当初は TS へ切り替えても有効化されないことも理由だったが、
	// 復路は保証しなくなった。#3191)。
	// signup (申請・登録) と admin (審査) の両方が同じインスタンスを見る。
	signupApplicationService := signupapplication.NewService(
		signupApplicationRepo, idGen)
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

	// Username availability check (フロントエンドの signup フォームが呼ぶ)
	//
	// upstream username/available.ts は available = (既存 user 無し) && (used_usernames
	// 無し) && (preservedUsernames 非該当) の3条件で判定し、paramDef の
	// localUsernameSchema で format 検証する (#1551)。format 不正は paramDef レベルで
	// 弾かれるため INVALID_PARAM を返す。
	signupService.SetUsedUsernameRepo(usedUsernameRepo) // #2080: 削除済 username 再利用を弾く
	signupHandler.SetUsernameLookups(userRepo, usedUsernameRepo)

	// Signin (Phase 6)
	signinHandler := apisignin.NewHandler(userRepo,
		s.background, // #3661: 世代の停止で待つ
		// new-login メールの instance lang fallback に使う。
		metaRepo,
		// 有効な TOTP コードを window 内で再利用させない (RFC 6238 §5.2)。
		totpReplayGuard,
	)
	signinHandler.SetMainStreamPublisher(mainStreamPublisher)
	if captchaSvc != nil {
		signinHandler.SetCaptcha(captchaSvc)
	}
	// **signin 側にも testMode が要る (#3037 レビュー 2 周目)。** upstream
	// `SigninApiService.ts:184` は signin の captcha 検証も
	// `process.env.NODE_ENV !== 'test'` で囲っている。signup 側にだけ
	// 配線されていたので、captcha を有効にした e2e 構成では signup だけ
	// 通って signin が落ちる。
	signinHandler.SetTestMode(s.config.TestMode)
	signinHandler.SetPasswordHasher(s.passwordHasher)
	// IP logging (#3103)。**起動時に meta を読んで配線するかどうかを決めない** —
	// その形だと管理画面で有効にしても再起動するまで記録が始まらなかった (#3107)。
	// 記録するかどうかは iplog.Service が呼び出しのたびに meta から判断する。
	signinHandler.SetIPRecorder(ipLogService)
	// signin履歴レコード注入
	signinHandler.SetSigninRepo(signinRepo, idGen)
	signinHandler.SetLoginNotifier(notificationHook)
	// new-login 通知メール (#2454)。SenderFromMeta が per-call で meta を読み直すので
	// admin UI の SMTP 設定変更が再起動なしで効く。SMTP 未設定なら no-op。
	signinHandler.SetEmailSender(s.config.URL, miscsmtp.SenderFromMeta(metaRepo, s.config.ProxySMTP))
	// アカウント作成時も signin 副作用 (履歴 / login 通知 / main publish) を通す (#1804)。
	signupHandler.SetSigninRecorder(signinHandler)

	// パスワードリセット (認証不要)
	resetHandler := apiresetpassword.NewHandler(userRepo, resetReqRepo, idGen,
		s.background, // #3661: 世代の停止で待つ
		metaRepo,     // リセットのメールの instance lang fallback に使う
	)
	resetHandler.SetServerURL(s.config.URL)
	resetHandler.SetPasswordHasher(s.passwordHasher)
	// password reset の確認メール送信。配線パターンは signup と同じく
	// SenderFromMeta で per-call 再 Fetch、runtime 設定変更追従 (#1112)。
	resetHandler.SetEmailSender(miscsmtp.SenderFromMeta(metaRepo, s.config.ProxySMTP))

	// WebAuthn / 2FA (issue #42)
	// WebAuthnService を構築し、userSecurityKeyRepo (buildRepos で作る) と一緒に
	// signin と /api/i に注入する。
	// Redis セッションは redis.default を流用する (用途別分離は不要)。
	webauthnSvc, webauthnErr := coretwofactor.NewWebAuthnService(s.config.URL, config.DisplayName, s.redis.Default)
	if webauthnErr != nil {
		slog.Warn("webauthn service unavailable", "err", webauthnErr)
	} else {
		signinHandler.SetWebAuthn(webauthnSvc, userSecurityKeyRepo)
	}

	// Emojis endpoints (public)
	emojisHandler := apiemojis.NewHandler(emojiRepo)
	emojisHandler.SetExportEnqueuer(s.queueClient)

	// api/* の多くの handler が渡された Packer の PackNotes で投稿を包む。Files /
	// MyReaction / Channel の post-pack 解決は notes 以外でも必要 (#426)。
	// 共通 resolver を 1 つだけ作って全 handler に注入する。添付ファイルも投稿の
	// 本体と同じ Packer で包むよう、entityPacker を渡す (#3790)。
	noteFieldResolver := entity.NewNoteFieldResolver(entityPacker, driveFileRepo, driveFolderRepo, userRepo, reactionRepo, channelRepo, idGen)
	// viewer の poll vote から Poll.choices[i].IsVoted を埋める (#690)。
	// frontend の MkPoll.vue が `showResult = isVoted || closed` を初期化に
	// 使うため、これがないと「結果を見る」toggle が reload で揮発する。
	noteFieldResolver.SetPollVoteLookup(pollVoteRepo)

	// 投稿を返す handler が、閲覧者の見てはいけない中身を隠すのに使う (#3786)。
	// 匿名 viewer へ pack する note を ugcVisibilityForVisitor='none' で hide する
	// (upstream NoteEntityService.shouldHideNote)。管理画面の変更を再起動なしで
	// 反映するよう、起動時の値ではなく都度引く。notes の handler と、下の
	// buildHandlers で作る handler へ渡す。notes の handler の構築子で受けるため、
	// notesHandler より前で作る (#3831)。NewHider は値を作るだけで、DB にも Redis
	// にも触らない。
	noteHider := notehide.NewHider(followingRepo, func() string { return metaUGCVisibility(metaRepo) })

	// Notes endpoints
	// **入れ忘れると黙って劣化する依存は、構築子の必須の引数で渡す (#3831)。** 以前は
	// 下で setter を呼び、recordCriticalWiring が起動時に入れ忘れを見ていた。
	//   - noteHider と entityPacker: 投稿を返すときに、閲覧者の見てはいけない中身を
	//     隠し、メディアの URL をプロキシへ書き換える (#3786 / #3790)
	//   - roleService: LocalTimeline / GlobalTimeline / HybridTimeline で
	//     ltlAvailable / gtlAvailable role policy を gate する (#1026)。匿名 viewer に
	//     対しては GetUserPolicies("") が base policies を返すので、admin が
	//     meta.policies で全 user に対して TL を無効化できる
	//   - ugcVisibilityForVisitor の lookup: **無条件に配線する。** 以前は Fetch
	//     成功時だけ設定していたので、起動時に DB が一時的に応答しないと空文字の
	//     ままになり、gate が**丸ごと消えていた** (空文字は "none" でも "local" でも
	//     ないので素通し)。列は NOT NULL DEFAULT 'local' なので、失敗時も制限側の
	//     既定へ倒す (#2708)。**毎回読む。** 起動時に焼き込むと、運営者が管理画面で
	//     締めてもプロセスを再起動するまで API に反映されない
	//   - driveFileRepo: 添付ファイルの解決と、下書きの添付の所有権検査
	//   - metaRepo: list endpoint の blocked-host filter (#1783)
	//   - blockingRepo: children/replies/renotes の mute/block filter (#1554)
	//   - mutingRepo: timeline endpoint で muted user の note を除外する filter (#874)。
	//     渡さないと user mute は read 時に効かず、cache のみ DB 整合の状態になる
	//   - renoteMutingRepo: timeline endpoint で renote-muted user の pure renote を
	//     除外する filter (#903)。渡さないと renote-mute は read 時に効かず、frontend
	//     で消えるべき renote が表示され続ける
	//   - channelMutingRepo: ミュートしたチャンネルの投稿を除く
	//   - followingRepo: HTL の「返信先が followers 限定の投稿」ガード (upstream
	//     timeline.ts の noteFilter)
	notesHandler := notes.NewHandler(noteRepo, noteCreateService, noteDeleteService, noteQueryService, timelineService, reactionService, pollService, searchService, idGen,
		noteHider, entityPacker, roleService, func() string { return metaUGCVisibility(metaRepo) },
		driveFileRepo, metaRepo, blockingRepo, mutingRepo, renoteMutingRepo, channelMutingRepo, followingRepo)
	notesHandler.SetLocalFederationObserver(coreSvcs.localFederation)
	notesHandler.SetPiningRepo(piningRepo)
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
	notesHandler.SetPollRepo(pollRepo)                     // #1538: polls/recommendation
	notesHandler.SetThreadMutingRepo(noteThreadMutingRepo) // #1538: thread-muting write
	notesHandler.SetNoteReactionRepo(reactionRepo)
	notesHandler.SetChannelRepo(channelRepo)
	// home timeline で follow 中 channel の note を含める (#1686)。
	notesHandler.SetChannelFollowingRepo(channelFollowingRepo)
	// notes/featured の engagement ランキング読み取り (#1687)。
	notesHandler.SetFeaturedRanking(featuredService)
	notesHandler.SetInstanceRepo(instanceRepo)
	notesHandler.SetEmojiRepo(emojiRepo)
	notesHandler.SetReactionReader(reactionCountWriter)
	notesHandler.SetDriveFolderRepo(driveFolderRepo)
	notesHandler.SetUserRepo(userRepo)
	notesHandler.SetModeratorChecker(roleService) // #1538: moderator note delete
	notesHandler.SetUserListRepo(userListRepo)
	notesHandler.SetClipRepos(clipRepo, clipNoteRepo, clipFavoriteRepo) // #1554: notes/clips

	// **キーと Pro は翻訳のたびに読む (#3663)。** 起動時の値で翻訳器を作ると、
	// 管理画面でキーを入れても外しても、プロセスか世代を作り直すまで変わら
	// なかった。metaRepo はキャッシュだが、`internal:metaUpdated` で全プロセスの
	// 分が捨てられる。
	notesHandler.SetTranslator(coretranslate.NewMetaDeepL(func() (string, bool, error) {
		m, err := metaRepo.Fetch()
		if err != nil {
			return "", false, err
		}
		if m.DeeplAuthKey == nil {
			return "", false, nil
		}
		return *m.DeeplAuthKey, m.DeeplIsPro, nil
	}, s.outboundClient(10*time.Second)))
	// Notes extra endpoints (Phase 6)
	notesHandler.SetFavoriteRepo(noteFavoriteRepo)
	notesHandler.SetAchievementGranter(achievementService)
	// notes/drafts + notes/thread-muting + notes/polls/recommendation (実データ)
	notesHandler.SetDraftRepo(noteDraftRepo)
	notesHandler.SetScheduledNoteEnqueuer(s.queueClient)

	// PostScheduledNote processor (#1040): delayed queue 経由で draft を
	// note 化する。dependencies は既存 noteCreateService + userRepo +
	// noteDraftRepo を共有する。#3690 で buildQueue へ移したが、Redis の lock を
	// 構築子で受けるため #3829 でここへ戻した。
	// idempotency lock を Redis SETNX で配線 (#1045 Phase 2-A)。queue の
	// at-least-once delivery で job が二重 fire しても重複 publish を防ぐ。
	postScheduledNoteProcessor := processors.NewPostScheduledNoteProcessor(noteDraftRepo, userRepo, noteCreateService,
		processors.NewRedisScheduledNoteLock(s.redis.Default, s.config.Redis.KeyPrefix(), 0))
	// publish 結果通知 (#1045 Phase 2-B): scheduledNotePosted (= 成功) /
	// scheduledNotePostFailed (= 失敗) を user の通知 stream に発火する。
	postScheduledNoteProcessor.SetNotifier(notificationService)

	// API の handler (#3694)。作るのは buildHandlers (wiring_handlers.go) で、ここでは
	// 今までと同じ名前の別名を置く。呼ぶ位置は、移した handler の中で最初に作っていた
	// usersHandler の位置。移した handler は作るだけで副作用を持たないので、作る位置が
	// 前へ移っても、DB と Redis の文の順は変わらない。setter とルートの登録はそれぞれ
	// 元の位置のまま。ここより前で作る handler (meta・notes など) と、setupRoutes で作る
	// 部品・stream の manager・outbound client に依存する handler、ここで作ると setter で後から
	// 入れる辺が増える handler (admin など) は、元の位置で作る (docs/architecture.md 4.2)。
	// **別名は変数 hSvcs から取る。** w.handlers から取ると、別名を代入より前に書いても
	// コンパイルが通り、ゼロ値を黙って受け取る (docs/architecture.md 4.2)。
	// **入れ忘れると黙って劣化する依存は、構築子の必須の引数で渡す (#3817)。** 以前は
	// 下で setter を呼び、recordCriticalWiring が起動時に入れ忘れを見ていた。ここで
	// 渡す値は、どれもこの位置より前で作ってある。通報と申請の受付の通知は、
	// abuseNotifStates・roleService・signupApplicationNotifLookup のどれかが欠けると
	// 一覧に出なくなる (fail-closed)。
	//
	// stream の部品は buildHandlers より前に作る (#3827)。i と oauth の handler が、
	// buildStream で作る revoke publisher を構築子の引数で受けるため。buildStream が
	// 使うのは先頭で作る streamPubSub と idGen と、下で渡す Packer と 4 つの lookup だけで、
	// 中の構築子はどれも値を作って返すだけ (Redis に繋がず、購読も goroutine も始めない)。
	// 購読 (Subscribe…) と setter、w.stream への代入と別名 (revoke publisher を除く) は、
	// 下の stream の節のまま。
	//
	// manager の配信ごとの filter と、note / notification の publisher の Packer は、
	// どれも無いと黙って劣化するので、構築子の必須の引数で渡す (#3827)。
	//   - hardMutedWords (#787) の rules lookup。connection 確立時に 1 度だけ
	//     user_profile を引き、以降の publish では cache された rules を各 timeline
	//     channel が参照する
	//   - muteBlockSnapshot (#1711): main / notifications channel が notification /
	//     mention 配信時に instance-mute / block / mute を gate するための snapshot を
	//     接続確立時に 1 回 fetch する。失敗 / anonymous は nil 返却で fail-open に
	//     degrade — filter 無しで動き続ける
	//   - timeline channel (LTL/GTL/HTL) の ltlAvailable / gtlAvailable gate 用の role
	//     policy provider (#1942)。REST timeline と同じく WS でも policy で無効化された
	//     timeline を subscribe させない (policy bypass 解消)
	//   - 匿名接続への meta.ugcVisibilityForVisitor (upstream NoteStreamingHidingService /
	//     Connection の noteUpdated)。REST と同じ metaUGCVisibility を毎回引く (cachedMeta
	//     なので event ごとの DB 往復は無い)。起動時に焼き込むと、管理画面で締めても
	//     再起動まで streaming に効かない
	streamSvcs := buildStream(streamPubSub, idGen, entityPacker,
		&hardMuteLookupAdapter{userRepo: userRepo},
		&muteBlockSnapshotAdapter{
			muting:        mutingRepo,
			blocking:      blockingRepo,
			renoteMuting:  renoteMutingRepo,
			channelMuting: channelMutingRepo,
			userRepo:      userRepo,
		},
		roleService,
		func() string { return metaUGCVisibility(metaRepo) })
	// 失効を扱う handler (i・oauth・admin) へ配る revoke publisher。別名は変数 streamSvcs
	// から取る。i と oauth の handler が構築子の引数で受けるので、ほかの別名 (下の
	// stream の節) より前に置く (#3827)。
	streamRevokePublisher := streamSvcs.streamRevokePublisher
	// i/2fa/register の仮の秘密の期限と i/2fa/done の失敗回数を数える guard。i の
	// handler が構築子の引数で受ける (#3827)。作るだけで Redis には触らない。
	// buildHandlers の中で s.redis を読むと、Redis の無い Server で組み立てるテスト
	// (TestBuildHandlersFillsEveryField) が落ちるので、ここで作って渡す。
	twoFAPendingGuard := coretwofactor.NewRedisPendingGuard(s.redis.Default)
	hSvcs := s.buildHandlers(w, coreSvcs, fedSvcs, idGen, noteHider, entityPacker, sharedUserPacker, metaRepo,
		abuseNotifStates, abuseReportRepoForNotif.CountUnresolved, signupApplicationNotifLookup,
		twoFAPendingGuard, streamRevokePublisher)
	w.handlers = hSvcs
	s.builtHandlers = hSvcs
	usersHandler := hSvcs.users
	iHandler := hSvcs.i
	webhookHandler := hSvcs.webhook
	notificationsHandler := hSvcs.notifications
	hashtagsHandler := hSvcs.hashtags
	galleryHandler := hSvcs.gallery
	blockingHandler := hSvcs.blocking
	muteHandler := hSvcs.mute
	renoteMuteHandler := hSvcs.renoteMute
	driveHandler := hSvcs.drive
	apHandler := hSvcs.ap
	wellknownHandler := hSvcs.wellknown
	nodeinfoHandler := hSvcs.nodeinfo
	federationHandler := hSvcs.federation
	channelsHandler := hSvcs.channels
	antennasHandler := hSvcs.antennas
	clipsHandler := hSvcs.clips
	pagesHandler := hSvcs.pages
	flashHandler := hSvcs.flash
	followingHandler := hSvcs.following
	rolesHandler := hSvcs.roles
	userListHandler := hSvcs.userList
	authHandler := hSvcs.auth
	appHandler := hSvcs.app
	bubbleGameHandler := hSvcs.bubbleGame
	endpointsHandler := hSvcs.endpoints
	avatarDecorationsHandler := hSvcs.avatarDecorations
	promoHandler := hSvcs.promo
	inviteHandler := hSvcs.invite
	echoTestHandler := hSvcs.echoTest

	// users と i (自分のアカウント) の API の配線 (#3901 で関数へ移した。呼ぶ位置と中の文の順は
	// 元のまま。作った部品は同じ名前で受ける)。
	remoteStatsFetcher, accountMover := s.wireAccounts(accountsDeps{
		idGen:                      idGen,
		streamPubSub:               streamPubSub,
		relationReloadPublisher:    relationReloadPublisher,
		mainStreamPublisher:        mainStreamPublisher,
		userListStreamPublisher:    userListStreamPublisher,
		userRepo:                   userRepo,
		noteRepo:                   noteRepo,
		metaRepo:                   metaRepo,
		followingRepo:              followingRepo,
		followRequestRepo:          followRequestRepo,
		piningRepo:                 piningRepo,
		reactionRepo:               reactionRepo,
		emojiRepo:                  emojiRepo,
		blockingRepo:               blockingRepo,
		mutingRepo:                 mutingRepo,
		renoteMutingRepo:           renoteMutingRepo,
		driveFileRepo:              driveFileRepo,
		instanceRepo:               instanceRepo,
		antennaRepo:                antennaRepo,
		clipRepo:                   clipRepo,
		clipNoteRepo:               clipNoteRepo,
		pageRepo:                   pageRepo,
		pageLikeRepo:               pageLikeRepo,
		flashRepo:                  flashRepo,
		noteFavoriteRepo:           noteFavoriteRepo,
		userListRepo:               userListRepo,
		chatRepo:                   chatRepo,
		clipFavoriteRepo:           clipFavoriteRepo,
		userListFavoriteRepo:       userListFavoriteRepo,
		channelNoteUnreadRepo:      channelNoteUnreadRepo,
		antennaNoteUnreadRepo:      antennaNoteUnreadRepo,
		signinRepo:                 signinRepo,
		avatarDecorationRepo:       avatarDecorationRepo,
		moveSystemAccountRepo:      moveSystemAccountRepo,
		userSecurityKeyRepo:        userSecurityKeyRepo,
		registryRepo:               registryRepo,
		abuseReportRepo:            abuseReportRepo,
		userMemoRepo:               userMemoRepo,
		galleryRepo:                galleryRepo,
		chartHooks:                 chartHooks,
		roleService:                roleService,
		sharedUserPacker:           sharedUserPacker,
		coreSvcs:                   coreSvcs,
		featuredService:            featuredService,
		instanceService:            instanceService,
		followingService:           followingService,
		antennaService:             antennaService,
		notificationService:        notificationService,
		webhookService:             webhookService,
		driveReader:                driveReader,
		listProxyFollow:            listProxyFollow,
		passwordFailureGuard:       passwordFailureGuard,
		apURLs:                     apURLs,
		apRenderer:                 apRenderer,
		deliverService:             deliverService,
		userModerationDeliveryHook: userModerationDeliveryHook,
		reactionCountWriter:        reactionCountWriter,
		localHost:                  localHost,
		federationResolver:         federationResolver,
		federationProcessor:        federationProcessor,
		webauthnSvc:                webauthnSvc,
		noteFieldResolver:          noteFieldResolver,
		usersHandler:               usersHandler,
		iHandler:                   iHandler,
		webhookHandler:             webhookHandler,
	})

	// Notifications endpoints
	notificationsHandler.SetRepos(userRepo, noteRepo)
	notificationsHandler.SetQueryService(noteQueryService)
	notificationsHandler.SetFollowRequestRepo(followRequestRepo)
	notificationsHandler.SetInstanceRepo(instanceRepo)
	notificationsHandler.SetEmojiRepo(emojiRepo)
	// read 時 valid-notifier filter (now-muted notifier の除外) 用 (#1775)。
	notificationsHandler.SetMutingRepo(mutingRepo)
	notificationsHandler.SetTestNotifier(notificationHook)
	notificationsHandler.SetRoleLookup(roleNotifLookup)
	notificationsHandler.SetEmojiApplicationLookup(emojiApplicationNotifLookup)
	// 通知に埋め込む note の files / channel / myReaction を埋める (#2735)。
	notificationsHandler.SetNoteFieldResolver(noteFieldResolver)
	// notifications/create の 'app' 通知で header/icon を token.name/iconUrl に
	// fallback するため access token repo を配線 (#1557)。
	notificationsHandler.SetAccessTokenRepo(accessTokenRepo)

	// i/export-* / i/import-* は上方 (line ~1245-1265) の explicit registration
	// が active (iHandler.Export* / Import* via transfer_handler.go) で、proper
	// な typed transfer.ExportXxx 定数と fileId 受け取りロジックを持つ。
	// かつて本箇所に generic for-loop による登録が同 path で重複していたが
	// (#1020 で気づいた pre-existing pattern)、それは fileId を受けず string
	// literal "following" 等を直接 type に渡す壊れた version で、Echo の
	// route override 経由で active 実装を蹴る悪影響があった。#1031 で削除済。

	// Hashtags endpoints (Phase 6)
	hashtagsHandler.SetIDGen(idGen)                     // #2106 L25: 設定済 ID generator を共有 (毎回 aidx 生成を廃止)
	hashtagsHandler.SetRelationRepos(listRelationRepos) // #1957-a: hashtags/users の embed user に relation

	// Gallery endpoints (Phase 6)
	galleryHandler.SetRanking(featuredService) // #1548: like/unlike/featured ランキング
	galleryHandler.SetRoleChecker(roleService) // #1548: posts/delete モデレータ削除

	// Blocking endpoints
	// blocking/create・delete のレスポンスに viewer→blockee の relation block
	// (isBlocking 等) を載せるための共有 resolver 依存 (#1802)。
	blockingHandler.SetRelationRepos(userrelation.Repos{
		Following:     followingRepo,
		Blocking:      blockingRepo,
		Muting:        mutingRepo,
		RenoteMuting:  renoteMutingRepo,
		FollowRequest: followRequestRepo,
		Memo:          userMemoRepo,
		Relation:      userRelationRepo,
	})
	blockingHandler.SetModeratorChecker(roleService) // #1985: blocking/list の count gate で moderator viewer 判定

	// Mute endpoints
	muteHandler.SetRelationRepos(listRelationRepos)
	muteHandler.SetModeratorChecker(roleService) // #1985: mute/list の count gate で moderator viewer 判定

	// Renote mute endpoints
	renoteMuteHandler.SetRelationRepos(listRelationRepos)
	renoteMuteHandler.SetModeratorChecker(roleService) // #1985: renote-mute/list の count gate で moderator viewer 判定

	// Drive endpoints
	// /drive/files/upload-from-url: SSRF-safe client で URL を download し、
	// drive へ保存後 urlUploadFinished を main stream に流す (#1217)。download は
	// 大きめになりうるので 60s timeout、cap は cfg.MaxFileSize に揃える。
	driveURLUploader := drive.NewURLUploader(
		s.outboundClient(60*time.Second), driveService, mainStreamPublisher,
		idGen, s.config.UserAgent, s.config.MaxFileSize,
		entityPacker, // #3796: urlUploadFinished のファイルを包む
	)
	notesHandler.SetLocalFederationObserver(coreSvcs.localFederation)
	driveHandler.SetURLUploader(driveURLUploader)
	driveHandler.SetRepos(driveFileRepo, driveFolderRepo, noteRepo)
	driveHandler.SetUserRepo(userRepo)
	driveHandler.SetInstanceRepo(instanceRepo)
	driveHandler.SetEmojiRepo(emojiRepo)
	driveHandler.SetReactionReader(reactionCountWriter)
	driveHandler.SetNoteFieldResolver(noteFieldResolver)
	driveHandler.SetMetaRepo(metaRepo) // #2106 S4: enableIpLogging を尊重して requestIp/headers 保存を gate
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
	proxyService := coremediaproxy.NewService(
		s.config.URL, s.config.UserAgent, driveStorage,
		proxyAllowlist, s.mediaProxySecret,
		s.config.AllowedPrivateNetworks,
		s.outboundOpts()...,
	)
	// 画像処理の同時実行枠 (#3032)。0 なら mediaproxy 側の既定
	// (GOMAXPROCS / 2、最低 1)。**配線しないと設定キーが黙って効かない。**
	proxyService.SetCPUConcurrency(s.config.MediaProxyConcurrency)
	// 同じ相手への接続を他の外向きの経路と使い回す (#3591)。
	proxyService.SetTransport(s.outboundTransport())
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

	// ActivityPub のリソースと、同じ path の SSR・フィード、discovery、OAuth、inbox の登録と、
	// 連合の API の配線 (#3901 で関数へ移した。呼ぶ位置と中の文の順は元のまま。作った
	// handler は同じ名前で受ける)。
	ssrMeta, feedH, twemojiBadge, oauthHandler, inboxHandler := s.wireFederationRoutes(federationRoutesDeps{
		idGen:                   idGen,
		userRepo:                userRepo,
		noteRepo:                noteRepo,
		metaRepo:                metaRepo,
		instanceRepo:            instanceRepo,
		followingRepo:           followingRepo,
		followRequestRepo:       followRequestRepo,
		userRelationRepo:        userRelationRepo,
		piningRepo:              piningRepo,
		blockingRepo:            blockingRepo,
		mutingRepo:              mutingRepo,
		renoteMutingRepo:        renoteMutingRepo,
		driveFileRepo:           driveFileRepo,
		keypairExtraRepo:        keypairExtraRepo,
		channelRepo:             channelRepo,
		clipRepo:                clipRepo,
		pageRepo:                pageRepo,
		flashRepo:               flashRepo,
		reversiRepo:             reversiRepo,
		announcementRepo:        announcementRepo,
		sigCapRepo:              sigCapRepo,
		quoteAuthorizationRepo:  quoteAuthorizationRepo,
		userMemoRepo:            userMemoRepo,
		galleryRepo:             galleryRepo,
		accessTokenRepo:         accessTokenRepo,
		chartHooks:              chartHooks,
		roleService:             roleService,
		entityPacker:            entityPacker,
		coreSvcs:                coreSvcs,
		instanceService:         instanceService,
		proxyAccountResolver:    proxyAccountResolver,
		apFetcher:               apFetcher,
		federationResolver:      federationResolver,
		federationProcessor:     federationProcessor,
		sigCapBuffer:            sigCapBuffer,
		rateLimiter:             rateLimiter,
		setupDetector:           setupDetector,
		chunkedUploadCapability: chunkedUploadCapability,
		frontend:                frontend,
		streamRevokePublisher:   streamRevokePublisher,
		apHandler:               apHandler,
		wellknownHandler:        wellknownHandler,
		nodeinfoHandler:         nodeinfoHandler,
		federationHandler:       federationHandler,
	})

	// Channels endpoints (Phase 4.2)
	channelsHandler.SetRelationReloadPublisher(relationReloadPublisher)
	channelsHandler.SetFavoriteRepo(channelFavoriteRepo)
	channelsHandler.SetMutingRepo(channelMutingRepo)
	channelsHandler.SetFollowingRepo(channelFollowingRepo)
	// channels/timeline の mute / block filter (#2345 の timelines 調査)。
	channelsHandler.SetMuteBlockRepos(mutingRepo, blockingRepo, noteRepo)
	channelsHandler.SetDriveFileRepo(driveFileRepo)
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

	// Antennas endpoints (Phase 4.3)
	antennasHandler.SetInstanceRepo(instanceRepo)
	antennasHandler.SetEmojiRepo(emojiRepo)
	antennasHandler.SetReactionReader(reactionCountWriter)
	antennasHandler.SetNoteFieldResolver(noteFieldResolver)
	antennasHandler.SetUserRepo(userRepo)
	antennasHandler.SetQueryService(noteQueryService) // #1464: Notes の visibility filter (defense-in-depth)

	// Clips endpoints (Phase 4.4)
	clipsHandler.SetFavoriteRepo(clipFavoriteRepo)
	clipsHandler.SetInstanceRepo(instanceRepo)
	clipsHandler.SetEmojiRepo(emojiRepo)
	clipsHandler.SetReactionReader(reactionCountWriter)
	clipsHandler.SetNoteFieldResolver(noteFieldResolver)
	clipsHandler.SetUserRepo(userRepo)
	clipsHandler.SetQueryService(noteQueryService) // #1456: AddNote の visibility gate
	clipsHandler.SetNoteRepo(noteRepo)             // #1630 / #3818: Notes の renote 入れ子の mute/block filter

	// Pages endpoints (Phase 4.5)
	pagesHandler.SetMainStreamPublisher(mainStreamPublisher)

	// Flash endpoints (Phase 4.5)
	flashHandler.SetRoleChecker(roleService) // #1548: flash/delete モデレータ削除

	// Streaming (Phase 4.1 Step K)
	// 1. Redis pubsub bus (核となる publish/subscribe チャンネル)。streamPubSub と
	// internalPubSub は、受け手へ早く渡せるよう setupRoutes の先頭で作っている (#3676)。
	// bus・registry・manager と、ここより後で渡す publisher は buildStream
	// (wiring_stream.go) で作る (#3692)。buildStream は buildHandlers の直前で呼ぶ
	// (#3827)。ここでは今までと同じ名前の別名を置くだけにし、以降の部分は書き換えない。
	// どれも作るだけで Redis に繋がず、購読も始めない。setter と購読 (Subscribe…)、
	// s.streamManager への代入は元の位置のまま。
	w.stream = streamSvcs
	streamRegistry := streamSvcs.streamRegistry
	streamManager := streamSvcs.streamManager
	notePublisher := streamSvcs.notePublisher
	notificationPublisher := streamSvcs.notificationPublisher
	broadcastPublisher := streamSvcs.broadcastPublisher
	chatPublisher := streamSvcs.chatPublisher

	// cross-worker のキャッシュの無効化と、キャッシュの一覧への登録 (#3901 で関数へ移した。
	// 呼ぶ位置と中の文の順は元のまま)。
	s.wireCacheInvalidation(cacheInvalidationDeps{
		idGen:                    idGen,
		internalPubSub:           internalPubSub,
		cachedMeta:               cachedMeta,
		emojiRepo:                emojiRepo,
		cachedAntenna:            cachedAntenna,
		emojiDecorationResolver:  emojiDecorationResolver,
		ipLogService:             ipLogService,
		roleService:              roleService,
		avatarDecorationResolver: avatarDecorationResolver,
		instanceService:          instanceService,
		dbHealth:                 dbHealth,
		driveUsage:               driveUsage,
		deliverService:           deliverService,
		relaySvc:                 relaySvc,
		reactionCountWriter:      reactionCountWriter,
		webPushCache:             webPushCache,
		instanceSigner:           instanceSigner,
		apFetcher:                apFetcher,
		federationResolver:       federationResolver,
		fedRuleService:           fedRuleService,
		deliverProcessor:         deliverProcessor,
		metaHandler:              metaHandler,
		statsHandler:             statsHandler,
		captchaSvc:               captchaSvc,
		notesHandler:             notesHandler,
		usersHandler:             usersHandler,
		nodeinfoHandler:          nodeinfoHandler,
		channelsHandler:          channelsHandler,
		antennasHandler:          antennasHandler,
		clipsHandler:             clipsHandler,
		notePublisher:            notePublisher,
	})

	// 取り消したリノート / 削除した引用の通知を消す (#3201)。**配線しないと build も
	// テストも通ったまま** 未読件数に中身の無い通知が数えられ続ける。
	noteDeleteService.SetNotificationHook(notificationHook)

	// ストリーミングの接続の管理と publisher、reversi・バブルゲームの対戦・chat の service
	// とチャンネル、/streaming の登録 (#3901 で関数へ移した。呼ぶ位置と中の文の順は元のまま。
	// 購読を始める位置も変わらない。作った service は同じ名前で受ける)。
	abuseCreatedNotifier, serverStatsPub, reversiService, bubbleVersusPublisher, bubbleVersusService, chatService, chatRoomFactory := s.wireStreaming(streamingDeps{
		idGen:                        idGen,
		streamPubSub:                 streamPubSub,
		mainStreamPublisher:          mainStreamPublisher,
		userRepo:                     userRepo,
		noteRepo:                     noteRepo,
		metaRepo:                     metaRepo,
		followingRepo:                followingRepo,
		piningRepo:                   piningRepo,
		reactionRepo:                 reactionRepo,
		emojiRepo:                    emojiRepo,
		blockingRepo:                 blockingRepo,
		keypairExtraRepo:             keypairExtraRepo,
		instanceRepo:                 instanceRepo,
		antennaRepo:                  antennaRepo,
		clipRepo:                     clipRepo,
		userListRepo:                 userListRepo,
		reversiRepo:                  reversiRepo,
		chatRepo:                     chatRepo,
		bubbleVersusRecordRepo:       bubbleVersusRecordRepo,
		abuseReportRepo:              abuseReportRepo,
		roleService:                  roleService,
		entityPacker:                 entityPacker,
		reversiPublisher:             reversiPublisher,
		sharedUserPacker:             sharedUserPacker,
		reversiFedCache:              reversiFedCache,
		webPushService:               webPushService,
		noteCreateService:            noteCreateService,
		noteQueryService:             noteQueryService,
		userService:                  userService,
		timelineFanoutHook:           timelineFanoutHook,
		ephemeralStore:               ephemeralStore,
		antennaService:               antennaService,
		clipService:                  clipService,
		reactionService:              reactionService,
		notificationService:          notificationService,
		notificationHook:             notificationHook,
		blockingService:              blockingService,
		mutingService:                mutingService,
		pollService:                  pollService,
		abuseInAppNotifier:           abuseInAppNotifier,
		apURLs:                       apURLs,
		apRenderer:                   apRenderer,
		deliverService:               deliverService,
		relaySvc:                     relaySvc,
		profileUpdateHook:            profileUpdateHook,
		pinDeliveryHook:              pinDeliveryHook,
		reactionCountWriter:          reactionCountWriter,
		roleNotifLookup:              roleNotifLookup,
		emojiApplicationNotifLookup:  emojiApplicationNotifLookup,
		signupApplicationNotifLookup: signupApplicationNotifLookup,
		abuseNotifLookup:             abuseNotifLookup,
		federationResolver:           federationResolver,
		federationProcessor:          federationProcessor,
		noteFieldResolver:            noteFieldResolver,
		notesHandler:                 notesHandler,
		usersHandler:                 usersHandler,
		iHandler:                     iHandler,
		clipsHandler:                 clipsHandler,
		pagesHandler:                 pagesHandler,
		streamRegistry:               streamRegistry,
		streamManager:                streamManager,
		notePublisher:                notePublisher,
		notificationPublisher:        notificationPublisher,
		chatPublisher:                chatPublisher,
	})

	// Charts API endpoints (engines + hooks already wired earlier).
	// フロントエンドは `misskeyApiGet` (GET) でチャートを取得し、
	// `misskeyApi` (POST) も使う場面があるので両方受ける。POST のみ登録だと
	// GET が `api.Any("/*")` の catchall (= 200 + 空オブジェクト) に落ちて、
	// 受信側で `chart.pubActive[0]` 等が `undefined` 例外を起こす (#421)。
	chartsHandler := apicharts.NewHandler(chartCharts)
	// /retention は chart engine ではなく集計済みテーブルを読む (#2791 で
	// router.go の inline closure から移設)。
	chartsHandler.SetRetentionRepo(retentionRepo)

	// Following endpoints
	followingHandler.SetIDGen(idGen)
	// federation の残ルートは上で登録済 (federationHandler 初期化直後)

	// Public roles (Phase 6)
	rolesHandler.SetRelationCache(s.relationCache)
	rolesHandler.SetNotesQuery(repository.NewRoleNotesQuery(s.db))
	rolesHandler.SetNoteRepo(noteRepo) // #1630: notes の renote 入れ子 mute/block 検査
	rolesHandler.SetInstanceRepo(instanceRepo)
	rolesHandler.SetEmojiRepo(emojiRepo)
	rolesHandler.SetReactionReader(reactionCountWriter)
	rolesHandler.SetNoteFieldResolver(noteFieldResolver)
	rolesHandler.SetUserRepo(userRepo)
	rolesHandler.SetMuteBlockRepos(mutingRepo, blockingRepo, channelMutingRepo) // #1544: Notes の mute/block/channel-mute filter

	// User lists (Phase 6)
	userListHandler.SetUserRepo(userRepo)                              // #1550: push NO_SUCH_USER check
	userListHandler.SetBlockingRepo(blockingRepo)                      // #1550: push YOU_HAVE_BEEN_BLOCKED check
	userListHandler.SetFavoriteRepo(userListFavoriteRepo)              // #1550: show forPublic likedCount/isLiked
	userListHandler.SetProxyFollow(listProxyFollow)                    // #1704: push remote member proxy follow
	userListHandler.SetUserListEventPublisher(userListStreamPublisher) // #1549: push/pull userAdded/userRemoved

	// Announcements (Phase 6)

	// Moderator-activity check (#1563): scheduler の cron (30 * * * *) が enqueue
	// する。全 moderator が 7 日非アクティブだと登録を招待制へ自動切替し、残 2 日で
	// 6h ごとに警告メール / announcement / SystemWebhook を送る。announcementRepo /
	// webhookService がここで揃うため本箇所で配線する。announcement は repo 直接
	// ではなく create + announcementCreated publish を対で行う Creator 経由
	// (#1606、本家 AnnouncementService.create 相当)。
	announcementCreator := coreannouncement.NewCreator(announcementRepo, idGen, mainStreamPublisher, broadcastPublisher,
		entityPacker) // #3796: announcementCreated の本文を包む
	modActivitySvc := coremoderatoractivity.NewService(
		roleService,
		metaRepo,
		userRepo,
		announcementCreator,
		webhookService,
		idGen,
		miscsmtp.SubjectBodySenderFromMeta(metaRepo, s.config.ProxySMTP),
	)
	checkModeratorsActivityProcessor := processors.NewCheckModeratorsActivityProcessor(modActivitySvc)

	// queue の handler の登録 (#3690)。組み込みの job 種別とそれを受ける processor の
	// 対応は builtinQueueHandlers (wiring_queue.go) の一覧にだけ書き、ここで 1 回
	// 登録する。登録は driver の map に入れるだけで、読まれるのは queueServer.Start
	// (New の後) で map を写すときだけなので、上に散らばっていた 30 か所の登録を
	// ここへ集めても動きは変わらない。processor への setter はすべてこれより前にある。
	// 置く位置は、最後の登録 (moderator の活動) があった位置。プラグインの登録
	// (setupPlugins) はこれより後のまま。
	s.registerQueueHandlers(builtinQueueHandlers(queueHandlerProcs{
		webPush:                 webPushProcessor,
		webhook:                 webhookProcessor,
		dataExport:              exportProcessor,
		dataImport:              importProcessor,
		emojiImport:             emojiImportProcessor,
		resendQuoteRequests:     resendQuoteRequestsProcessor,
		deliver:                 deliverProcessor,
		inbox:                   inboxProcessor,
		cleanRemoteNotes:        cleanProcessor,
		orphanUserCleanup:       orphanUserProcessor,
		orphanAttachmentCleanup: orphanAttachmentProcessor,
		checkExpiredMutings:     checkExpiredMutingsProcessor,
		archiveAnnouncements:    archiveAnnouncementsProc,
		clean:                   cleanGenericProcessor,
		chunkedUploadGC:         chunkedUploadGCProcessor,
		objectStorage:           objectStorageProcessor,
		reactionFlush:           flushProcessor,
		deleteAccount:           deleteAccountProcessor,
		unfollow:                unfollowProcessor,
		follow:                  followProcessor,
		block:                   blockProcessor,
		unblock:                 unblockProcessor,
		chart:                   chartProcessor,
		instanceRefresh:         instanceRefreshProc,
		retentionAggregate:      retentionProc,
		postScheduledNote:       postScheduledNoteProcessor,
		checkModeratorsActivity: checkModeratorsActivityProcessor,
	}))

	// Phase 7-2 (#244): /api/i の hasUnreadAnnouncement / unreadAnnouncements 配線
	iHandler.SetAnnouncementRepo(announcementRepo)
	// self を MeDetailed へ昇格させる他 handler (users / hashtags 等) が、未読 /
	// policies / pinned を /api/i と同じ source で埋められるようにする。iHandler は
	// これらの依存を全て持っているので、Enricher として包んだ Packer を 1 つ作り、
	// 昇格させる handler へ渡す (#3784)。admin は下で作るので、作った直後に渡す。
	// 渡し忘れても補完が抜けるだけで落ちないので、配線のテスト
	// (self_packer_wiring_test.go) が handler ごとに見る。
	selfPacker := meself.NewPacker(iHandler)
	usersHandler.SetSelfPacker(selfPacker)
	hashtagsHandler.SetSelfPacker(selfPacker)
	federationHandler.SetSelfPacker(selfPacker)
	pagesHandler.SetSelfPacker(selfPacker)
	rolesHandler.SetSelfPacker(selfPacker)
	announcementHandler := apiannouncements.NewHandler(announcementRepo, idGen, entityPacker) // #3796
	announcementHandler.SetMainStreamPublisher(mainStreamPublisher)
	announcementHandler.SetBroadcastPublisher(broadcastPublisher) // global announcementCreated (#2056)

	// 管理画面の API と、DB のバックアップの管理画面 (#3901 で関数へ移した。呼ぶ位置と
	// 中の文の順は元のまま。作った handler と service は同じ名前で受ける)。
	adminHandler, emojiApplicationService, emojiApplicationHandler, backupHandler := s.wireAdmin(adminDeps{
		idGen:                        idGen,
		internalPubSub:               internalPubSub,
		userRepo:                     userRepo,
		noteRepo:                     noteRepo,
		metaRepo:                     metaRepo,
		followingRepo:                followingRepo,
		followRequestRepo:            followRequestRepo,
		emojiRepo:                    emojiRepo,
		driveFileRepo:                driveFileRepo,
		suspensionOriginRepo:         suspensionOriginRepo,
		instanceRepo:                 instanceRepo,
		systemWebhookRepo:            systemWebhookRepo,
		userIPRepo:                   userIPRepo,
		goneInstanceRepo:             goneInstanceRepo,
		signupTicketRepo:             signupTicketRepo,
		signinRepo:                   signinRepo,
		avatarDecorationRepo:         avatarDecorationRepo,
		recipientRepo:                recipientRepo,
		ipLookupLogRepo:              ipLookupLogRepo,
		emojiApplicationRepoForNotif: emojiApplicationRepoForNotif,
		fedRuleRepo:                  fedRuleRepo,
		sigCapRepo:                   sigCapRepo,
		userSecurityKeyRepo:          userSecurityKeyRepo,
		abuseReportRepo:              abuseReportRepo,
		adRepo:                       adRepo,
		emojiDecorationResolver:      emojiDecorationResolver,
		roleService:                  roleService,
		avatarDecorationResolver:     avatarDecorationResolver,
		entityPacker:                 entityPacker,
		sharedUserPacker:             sharedUserPacker,
		modLogService:                modLogService,
		totpReplayGuard:              totpReplayGuard,
		instanceService:              instanceService,
		signupService:                signupService,
		noteDeleteService:            noteDeleteService,
		userService:                  userService,
		followingService:             followingService,
		notificationService:          notificationService,
		webhookService:               webhookService,
		localDriveStorage:            localDriveStorage,
		driveStorage:                 driveStorage,
		driveService:                 driveService,
		sysAcctSvc:                   sysAcctSvc,
		passwordFailureGuard:         passwordFailureGuard,
		dbHealth:                     dbHealth,
		driveUsage:                   driveUsage,
		apRenderer:                   apRenderer,
		deliverService:               deliverService,
		relaySvc:                     relaySvc,
		deliveryBreaker:              deliveryBreaker,
		userModerationDeliveryHook:   userModerationDeliveryHook,
		apFetcher:                    apFetcher,
		fedRuleHits:                  fedRuleHits,
		fedRuleService:               fedRuleService,
		metadataFetcher:              metadataFetcher,
		deliverProcessor:             deliverProcessor,
		deliveryHealth:               deliveryHealth,
		inboxHealth:                  inboxHealth,
		metaHandler:                  metaHandler,
		signupApplicationService:     signupApplicationService,
		webauthnSvc:                  webauthnSvc,
		streamRevokePublisher:        streamRevokePublisher,
		usersHandler:                 usersHandler,
		galleryHandler:               galleryHandler,
		flashHandler:                 flashHandler,
		broadcastPublisher:           broadcastPublisher,
		abuseCreatedNotifier:         abuseCreatedNotifier,
		selfPacker:                   selfPacker,
		announcementHandler:          announcementHandler,
	})

	// --- Phase 7.6b: chat/*, auth/*, ap/*, sw/*, reversi/*, bubble-game/*, misc ---

	// auth/* — MiAuth/OAuth セッション
	// miauth/:session/check — 3rd-party client が session 紐付き token を取得する
	// (#1224)。token をまだ持たない client が叩くため RequireAuth は付けない。
	authHandler.SetUserRepo(userRepo)
	authHandler.SetTokenNotifier(notificationHook)

	// sw/* — Service Worker push notifications (実データ)
	swHandler := apisw.NewHandler(swSubRepo, metaRepo, idGen, webPushCache)

	// reversi/* — オセロゲーム (実データ)
	reversiHandler := apireversi.NewHandler(reversiRepo, idGen, entityPacker,
		sharedUserPacker) // #3330: invitations の招待者の instance・絵文字
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
	// WebFinger 経由で取り込めるようにする。users/show / notes/create と同じ
	// resolver を共有する (連合しないホストの skip もこれで効く)。
	reversiHandler.SetRemoteUserLookup(remoteUserResolver)
	// Service 側にも federation 一式を注入して state 変化時に Update / Leave を
	// 配信できるようにする (#417 P1)。fedCache も Service 側で
	// session→game 解決に必要なので忘れず設定する。
	reversiService.SetFederationCache(reversiFedCache)
	reversiService.SetFederationDeliverer(deliverService)
	reversiService.SetUserRepo(userRepo)
	reversiService.SetBaseURL(s.config.URL)

	// bubble-game/versus/* — 1:1 の対戦 (mk-go 独自、#3230)
	bubbleVersusHandler := apibubblegame.NewVersusHandler(bubbleVersusService, userRepo, entityPacker)
	// 対戦の記録 (#3232)。見せる範囲は handler の canView が決める。
	bubbleVersusHandler.SetRecords(bubbleVersusRecordRepo, blockingService)

	// chat/* — Misskey v2026 チャット機能 (実データ)
	chatHandler := apichat.NewHandler(chatRepo, idGen, entityPacker,
		// drive/files/attached-chat-messages: chat message を pack するため chat
		// handler が処理する。file の owner-scope (moderator は任意 file 可) (#1218)。
		driveFileRepo,
		sharedUserPacker, // #3438: UserLite の instance・絵文字を応答ごとにまとめて埋める
	)
	chatHandler.SetService(chatService)
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

	// --- Phase P3: 補助公開エンドポイント ---

	// email-address/available — メールアドレスの利用可否チェック (public)
	//
	// upstream email-address/available.ts は requireCredential:false の public
	// endpoint で、EmailService.validateEmailForAccount が reason を
	// null|'used'|'format'|'disposable'|'mx'|'smtp'|'banned'|'network'|'blacklist'
	// で返す (#1551)。mk-go は外部依存の active validation (mx/smtp/disposable) は
	// 行わず、format → used (emailVerified=true 一致) → banned の順で判定する
	// (upstream の enableActiveEmailValidation=false 相当)。
	signupHandler.SetEmailAvailabilityDB(s.db)

	// invite/* — 招待コード user-scope 4 endpoint。canInvite role policy gate
	// (#1020) + inviteLimit / inviteLimitCycle / inviteExpirationTime (#1029
	// PR-2) は handler 側で enforcement。handler 抽出後の unit test は
	// internal/api/invite/handler_test.go を参照。
	// invite/list が createdBy / usedBy を UserLite で解決するため (#1776)。
	inviteHandler.SetUserRepo(userRepo)

	// fetch-external-resources — external JSON resource を hash 検証付きで取得
	// (#1222)。外部 theme/plugin 等を integrity-check して返す。SSRF-safe client
	// 経由、upstream requireCredential 相当で RequireAuth を要求する。
	fetchExternalHandler := apifetchexternal.New(s.outboundClient(apifetchexternal.FetchTimeout), s.config.UserAgent)

	// fetch-rss — RSS / Atom feed プロキシ。frontend RSS / RSSTicker ウィジェット
	// が GET で叩く実装になっているため Misskey TS と同じく allowGet 相当で
	// GET / POST の両方を受け付ける。
	fetchRSSHandler := apifetchrss.New(s.outboundClient(apifetchrss.FetchTimeout), s.config.UserAgent)

	// /api のルートを登録する (#3697)。登録は領域ごとの routes_<領域>.go の関数にあり、
	// 引数には setupRoutes の同じ名前の変数をそのまま渡す (docs/architecture.md 4.2)。
	// ルートの行は移す前と同じ文字列なので、行を照合するゲートの名前がここで繋がる。
	//
	// **すべての api.Use より後で、プラグインの準備に失敗したときの return より前に呼ぶ。**
	// echo の group は、Use より前に登録したルートに middleware を付けない。return の後に
	// 置くと、その経路で /api のルートが無いまま止まる。
	//
	// **ルートの行の引数は、ここで呼んだ時点で評価される。** 元は handler を作った直後に
	// 登録していたので、渡す変数をここまでの間に書き換えると、登録が受け取る値が変わる。
	// 書き換えていないこと、ここで 1 回ずつ呼ぶことは TestSetupRoutesCallsEveryRouteRegistrarOnce
	// が見る。test mode の /api/reset-db (handler を if の中で作る) と catchall
	// (プラグインのルートより後) はここに入れず、元の位置に残した。
	registerMetaRoutes(api, metaHandler, statsHandler, endpointsHandler, echoTestHandler)
	registerAuthRoutes(api, signupHandler, signinHandler, resetHandler, authHandler, appHandler, inviteHandler, roleService)
	registerEmojisRoutes(api, emojisHandler, emojiApplicationHandler, avatarDecorationsHandler, roleService)
	registerNotesRoutes(api, notesHandler, promoHandler)
	registerUsersRoutes(api, usersHandler, userListHandler, roleService)
	registerIRoutes(api, iHandler, webhookHandler, notificationsHandler, pagesHandler, flashHandler, announcementHandler, roleService, rateLimiter)
	registerNotificationsRoutes(api, notificationsHandler, swHandler)
	registerHashtagsRoutes(api, hashtagsHandler)
	registerGalleryRoutes(api, galleryHandler)
	registerRelationsRoutes(api, blockingHandler, muteHandler, renoteMuteHandler, followingHandler)
	registerDriveRoutes(api, driveHandler, chatHandler)
	registerFederationRoutes(api, federationHandler, apHandler, roleService)
	registerChannelsRoutes(api, channelsHandler, roleService)
	registerAntennasRoutes(api, antennasHandler)
	registerClipsRoutes(api, clipsHandler)
	registerPagesRoutes(api, pagesHandler)
	registerFlashRoutes(api, flashHandler)
	registerChartsRoutes(api, chartsHandler)
	registerRolesRoutes(api, rolesHandler)
	registerAnnouncementsRoutes(api, announcementHandler)
	registerAdminRoutes(api, adminHandler, announcementHandler, backupHandler, roleService)
	registerGamesRoutes(api, reversiHandler, bubbleGameHandler, bubbleVersusHandler)
	registerChatRoutes(api, chatHandler, roleService)
	registerFetchRoutes(api, fetchExternalHandler, fetchRSSHandler)

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
	// 相手のプラグイン一覧はキャッシュの一覧に登録するので (#3776)、変数に取り出す。
	pluginPeers := newNodeInfoPeerLister(s.outboundClient(peerLookupTimeout), peered)
	// 依存は構築子の必須の引数で渡す (#3827)。instanceService はブロックリストと連合の
	// ポリシーの判定。
	s.peerDeps = newPluginPeerDeps(
		s.config.Host,
		s.outboundClient(peerTimeout),
		federationResolver,
		activitypub.NewPublicKeyCache(256),
		instanceService,
		instanceSigner,
		// **こちらが持っているものだけ覚える。** 相手の nodeinfo は相手が
		// 書いた値なので、そのまま持つと外から中身を膨らませられる。
		pluginPeers,
		idGen,
		// 受け口のレート制限。全プラグインで 1 つを共有する。
		newPeerRateLimiter(),
		// 送信はキューに積む (#2819)。**プロセス内の time.Sleep だと再起動を
		// またげず、デプロイのたびに送信中のものが消える。**
		s.queueClient,
	)

	// 外部とファイルの系統 (#3776)。RSS と peer のプラグイン一覧はここより前の登録の
	// 並びの後で作るので、ここで登録する。5 つとも外から取った値か、ファイルを描いた
	// 値だけを持ち、ほかのキャッシュの値を含まないので、捨てる順の制約は無い。
	// プラグインの準備の return より前に置く (失敗しても一覧は揃える)。
	s.addCache("urlPreview", cacheFromExternal, urlPreviewFetcher)
	s.addCache("remoteStats", cacheFromExternal, remoteStatsFetcher)
	s.addCache("fetchRSS", cacheFromExternal, fetchRSSHandler)
	s.addCache("pluginPeers", cacheFromExternal, pluginPeers)
	s.addCache("twemojiBadge", cacheFromFile, twemojiBadge)
	s.crossProcessCaches.urlPreview = urlPreviewFetcher
	s.crossProcessCaches.remoteStats = remoteStatsFetcher
	s.crossProcessCaches.fetchRSS = fetchRSSHandler
	s.crossProcessCaches.pluginPeers = pluginPeers
	s.crossProcessCaches.twemojiBadge = twemojiBadge

	// プラグインが管理するアカウント (#3468)。作成は signup、画像はドライブ、
	// プロフィールはプロセス内の i/update、削除は admin/delete-account と同じ
	// 流れを通す。経路ごとに別の実装を持たないため。
	pluginAccountSvc := corepluginaccount.NewService(userRepo, repository.NewPluginAccountRepository(s.db), signupService)
	pluginAccountSvc.SetUploader(driveService)
	pluginAccountSvc.SetProfileUpdater(pluginProfileUpdater(&pluginAPI{echo: s.echo, userRepo: userRepo, host: requestHostFor(s.config.URL)}))
	pluginAccountSvc.SetDeleter(adminHandler)
	s.pluginAccounts = pluginAccountSvc
	// プラグインの秘密の値 (#3470)。鍵の長さは config で検査済みなので、ここで
	// 失敗するのは本体の不具合。起動を止める (プラグイン登録の失敗と同じ扱い)。
	pluginSecretSvc, err := pluginsecret.New(pgrepo.New(repository.NewPluginSecretRepository(s.db)), s.config.PluginSecretKey)
	if err != nil {
		s.pluginSetupErr = err
		return
	}
	s.pluginSecrets = pluginSecretSvc

	// プラグインの route は本家に無い経路なので、body の検査を付けない素の group を渡す。
	if err := s.setupPlugins(api.Group, registeredPlugins, openPluginStorage); err != nil {
		s.pluginSetupErr = err
		return
	}
	// 管理するアカウントへの通知をプラグインへ届ける (#3469)。**ロールを問わず
	// 配線する** — 通知とチャットはどのプロセスでも作られる。受け取るプラグインが
	// 居なければ nil なので、通知を作るたびの宛先の確認も走らない。
	// queueClient が nil のまま interface に入れると nil にならないので、ここで分ける。
	if d := newPluginNotificationDispatcher(registeredPlugins, s.config.Plugins, s.queueClient, corepluginnotify.Deps{
		Users: userRepo, Mutes: mutingService, Blocks: blockingRepo,
	}); d != nil {
		notificationHook.SetCreatedObserver(d)
		chatService.SetMessageObserver(d)
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

	// フロントエンドの配信と SPA の catchall (#3901 で関数へ移した。呼ぶ位置と中の文の順は
	// 元のまま。プラグインの準備に失敗したときの return より後)。
	s.wireFrontendRoutes(frontendRoutesDeps{
		userRepo:                userRepo,
		noteRepo:                noteRepo,
		metaRepo:                metaRepo,
		emojiRepo:               emojiRepo,
		clipRepo:                clipRepo,
		entityPacker:            entityPacker,
		proxyAccountResolver:    proxyAccountResolver,
		localHost:               localHost,
		setupDetector:           setupDetector,
		chunkedUploadCapability: chunkedUploadCapability,
		frontend:                frontend,
		notesHandler:            notesHandler,
		clipsHandler:            clipsHandler,
	})

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
	s.builtCore = coreParts{
		blockingService:     blockingService,
		reactionService:     reactionService,
		signupService:       signupService,
		clipService:         clipService,
		antennaService:      antennaService,
		pollService:         pollService,
		modLogService:       modLogService,
		notificationService: notificationService,

		noteCreateService:    noteCreateService,
		followingService:     followingService,
		driveService:         driveService,
		userService:          userService,
		webhookNoteHook:      webhookNoteHook,
		webhookReactionHook:  webhookReactionHook,
		webhookFollowingHook: webhookFollowingHook,
	}
	s.builtRouteParts = routeParts{
		admin:            adminHandler,
		signup:           signupHandler,
		signin:           signinHandler,
		resetPassword:    resetHandler,
		oauth:            oauthHandler,
		emojiApplication: emojiApplicationService,
		abuseCreated:     abuseCreatedNotifier,

		chat:                  chatHandler,
		chatService:           chatService,
		reversi:               reversiHandler,
		bubbleVersus:          bubbleVersusHandler,
		bubbleVersusPublisher: bubbleVersusPublisher,
		announcements:         announcementHandler,
		announcementCreator:   announcementCreator,
		url:                   urlHandler,
		ssrMeta:               ssrMeta,
		driveURLUploader:      driveURLUploader,
		webhookSignup:         webhookSignupHook,
		accountMover:          accountMover,
		remoteStatsFetcher:    remoteStatsFetcher,
		remoteUserResolver:    remoteUserResolver,

		streamManager:         streamManager,
		notePublisher:         notePublisher,
		notificationPublisher: notificationPublisher,
		serverStatsPub:        serverStatsPub,
		reversiPublisher:      reversiPublisher,

		feed: feedH,
		peer: s.peerDeps,

		deleteAccountProcessor:     deleteAccountProcessor,
		postScheduledNoteProcessor: postScheduledNoteProcessor,
		deliverProcessor:           deliverProcessor,
		inboxProcessor:             inboxProcessor,

		notes: notesHandler,

		metadataFetcher: metadataFetcher,
		deliverService:  deliverService,
		noteDeleteHook:  noteDeleteHook,
		inboxHandler:    inboxHandler,

		federationProcessor: federationProcessor,
		federationResolver:  federationResolver,

		sharedUserPacker:  sharedUserPacker,
		webPushNotePacker: webPushNotePacker,
		webPushUserPacker: webPushUserPacker,
		noteFieldResolver: noteFieldResolver,

		proxyService:   proxyService,
		proxyAllowlist: coreSvcs.proxyAllowlist,
	}
	s.recordCriticalWiring([]criticalWiring{
		// 投稿を包む部品の Packer (#3790)。無い部品はリモートのメディアの URL を生のまま出す。
		{"webPushNote.entityPacker", webPushNotePacker.HasEntityPacker(), entityPackerMissingEffect},
		// 利用者の詳細を包む部品 (#3792)。sharedUserPacker は follow 系の webhook と
		// main stream、blocking の応答、pages の pageEvent、一覧を包む handler などが共有する。
		{"sharedUserPacker.entityPacker", sharedUserPacker.HasEntityPacker(), entityPackerMissingEffect},
		// UserLite を包む部品 (#3794)。
		{"webPushUser.entityPacker", webPushUserPacker.HasEntityPacker(), entityPackerMissingEffect},
		{"signup.formTokens", signupHandler.HasFormTokens(),
			"captcha を 1 つも設定していないインスタンスで、申請 endpoint の唯一の簡易チェックが素通しになる"},

		// ここから可視性・権限・上限 (#2683)。認証ほど鋭くはないが、いずれも
		// 利用者から見えない形で制限が外れる。
		{"streamChannels.chatRoom.service", chatRoomFactory.HasService(),
			"chatRoom channel の購読でメンバーシップ検査が skip され、誰でも任意の room を購読できる"},

		// ここから #2708。#2683 で「既存 entry と基準上区別できない」まで取り込み、
		// 残していた同クラスの依存。
		// #3330: follow 系の main stream / Webhook と blocking/create・delete の
		// 利用者を本家の UserDetailedNotMe に揃える共有の packer の、ピン留めと移行先を
		// 埋める側 (users の handler)。packer そのものを following と webhook へ渡すのは、
		// #3825 から構築子の必須の引数。
		{"userPacker.detailExtras", sharedUserPacker.HasDetailExtras(),
			"follow 系の main stream / Webhook と blocking/create・delete で pinnedNotes などが空、movedTo / alsoKnownAs が null のまま返る"},
		{"following.meUpdatedPublisher", followingService.HasMeUpdatedPublisher(),
			"フォロー申請の作成・承認・取り消しで meUpdated が流れず、受け取った申請の印がリロードまで変わらない"},
		// #3330: 一覧・単体の UserDetailed のピン留め・移行先を users/show と同じ規則で埋める。
		{"userpack.detailExtrasMany", sharedUserPacker.HasDetailExtrasMany(),
			"共有 packer で組む一覧 (federation・following・roles・admin) の pinnedNotes などが空、movedTo / alsoKnownAs が null のまま返る"},
		{"following.blockingChecker", followingService.HasBlockingChecker(),
			"ブロック関係を無視してフォローが成立する (自分がブロックした相手・自分をブロックしている相手の両方。後者は inbox の Follow も通す)"},
		{"following.followingEstablisher", followingService.HasFollowingEstablisher(),
			"フォローの行の作成・削除と、申請の削除・カウントの増減が別々に書かれ、途中で失敗するとフォローの行だけが残る (再試行も既にフォロー済みで止まる)、承認で申請だけが消える、解除で行だけが消えてカウントと配送が欠ける (#3911、#3369)"},
	})
}

// crossProcessCaches holds the caches whose invalidations travel between
// processes (#3664), for the wiring test.
type crossProcessCaches struct {
	instances   *coreinstance.Service
	relays      *corerelay.Service
	decorations *avatardecoration.Resolver
	// emojiDecorations と emojis は、絵文字由来のデコレーションの配線を見るため。
	emojiDecorations *avatardecoration.EmojiResolver
	emojis           *repository.CachedEmojiRepository
	// fedRules は連合のルール (#3090)。internal:federationRulesUpdated で読み直させる。
	// キャッシュの一覧のテストが、登録した値と同じかを見るため (#3729)。
	fedRules *fedrule.Service
	// deliver / resolver / deliverProcessor / instanceSigner は連合の系統の残りの
	// キャッシュ (#3732)。キャッシュの一覧のテストが、登録した値と同じかを見るため。
	// fetcherSigner は apFetcher が持つ署名の口で、instanceSigner と同じ値を渡して
	// いることをテストが確かめる。
	deliver          *corefederation.DeliverService
	resolver         *corefederation.Resolver
	deliverProcessor *processors.DeliverProcessor
	instanceSigner   *instanceActorSigner
	fetcherSigner    corefederation.SignerProvider
	// antennas / pushSubscriptions / notes / notePublisher は投稿・タイムライン・
	// 通知の系統のキャッシュ (#3736)。キャッシュの一覧のテストが、登録した値と
	// 同じかを見るため。注目ノートの順位とタイムラインの JSON は notes から引く。
	antennas          *repository.CachedAntennaRepository
	pushSubscriptions *corewebpush.SubscriptionCache
	notes             *notes.Handler
	notePublisher     *stream.NotePublisher
	// driveUsage / dbHealth / stats / ipLog は管理画面と統計の系統のキャッシュ
	// (#3738)。キャッシュの一覧のテストが、登録した値と同じかを見るため。
	driveUsage *driveusage.Service
	dbHealth   *dbhealth.Service
	stats      *stats.Handler
	ipLog      *iplog.Service
	// urlPreview / remoteStats / fetchRSS / pluginPeers / twemojiBadge は外部と
	// ファイルの系統のキャッシュ (#3776)。キャッシュの一覧のテストが、登録した値と
	// 同じかを見るため。RSS と peer のプラグイン一覧は setupRoutes で s.crossProcessCaches
	// を組む位置より後で作るので、5 つとも一覧に登録する所でフィールドに入れる。
	urlPreview   *coreurlpreview.Fetcher
	remoteStats  *corefederation.RemoteStatsFetcher
	fetchRSS     *apifetchrss.Handler
	pluginPeers  *nodeInfoPeerLister
	twemojiBadge *twemojiBadgeHandler
	// captchaは一覧の読み直しの項目(#3777)が読み直すproviderの組。キャッシュの
	// 一覧のテストが、捨てた後に新しい設定で検証することを見るため。
	captcha *corecaptcha.Service
}
