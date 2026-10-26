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

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/activitypub/ld"
	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/api/adminbackup"
	apiannouncements "github.com/elythia-network/elythia/internal/api/announcements"
	apibubblegame "github.com/elythia-network/elythia/internal/api/bubblegame"
	apicharts "github.com/elythia-network/elythia/internal/api/charts"
	apichat "github.com/elythia-network/elythia/internal/api/chat"
	"github.com/elythia-network/elythia/internal/api/drive"
	apiemojiapplications "github.com/elythia-network/elythia/internal/api/emojiapplications"
	apiemojis "github.com/elythia-network/elythia/internal/api/emojis"
	apifetchexternal "github.com/elythia-network/elythia/internal/api/fetchexternal"
	apifetchrss "github.com/elythia-network/elythia/internal/api/fetchrss"
	"github.com/elythia-network/elythia/internal/api/inbox"
	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/api/notes"
	"github.com/elythia-network/elythia/internal/api/oauth"
	apiproxy "github.com/elythia-network/elythia/internal/api/proxy"
	apiresetpassword "github.com/elythia-network/elythia/internal/api/resetpassword"
	apireversi "github.com/elythia-network/elythia/internal/api/reversi"
	apisignin "github.com/elythia-network/elythia/internal/api/signin"
	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	"github.com/elythia-network/elythia/internal/api/stats"
	"github.com/elythia-network/elythia/internal/api/streaming"
	apisw "github.com/elythia-network/elythia/internal/api/sw"
	apitest "github.com/elythia-network/elythia/internal/api/test"
	apiurl "github.com/elythia-network/elythia/internal/api/url"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	"github.com/elythia-network/elythia/internal/api/users"
	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/config"
	coreabuse "github.com/elythia-network/elythia/internal/core/abuse"
	coreannouncement "github.com/elythia-network/elythia/internal/core/announcement"
	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	"github.com/elythia-network/elythia/internal/core/avatardecoration"
	corebubbleversus "github.com/elythia-network/elythia/internal/core/bubbleversus"
	"github.com/elythia-network/elythia/internal/core/cachebus"
	corecaptcha "github.com/elythia-network/elythia/internal/core/captcha"
	corechannel "github.com/elythia-network/elythia/internal/core/channel"
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/core/chart/charthook"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/core/dbhealth"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/driveusage"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	coreemojiimport "github.com/elythia-network/elythia/internal/core/emojiimport"
	"github.com/elythia-network/elythia/internal/core/emojimeta"
	coreephemeral "github.com/elythia-network/elythia/internal/core/ephemeral"
	"github.com/elythia-network/elythia/internal/core/event"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/fedrule"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/core/gonecleanup"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/core/iplog"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	coremediaproxy "github.com/elythia-network/elythia/internal/core/mediaproxy"
	coremodlog "github.com/elythia-network/elythia/internal/core/moderationlog"
	coremoderatoractivity "github.com/elythia-network/elythia/internal/core/moderatoractivity"
	coremove "github.com/elythia-network/elythia/internal/core/move"
	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	corepluginaccount "github.com/elythia-network/elythia/internal/core/pluginaccount"
	corepluginnotify "github.com/elythia-network/elythia/internal/core/pluginnotify"
	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/core/pluginsecret/pgrepo"
	corepoll "github.com/elythia-network/elythia/internal/core/poll"
	"github.com/elythia-network/elythia/internal/core/procstats"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corerelationcache "github.com/elythia-network/elythia/internal/core/relationcache"
	corerelay "github.com/elythia-network/elythia/internal/core/relay"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	coresearch "github.com/elythia-network/elythia/internal/core/search"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/core/signupform"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	coresystemaccount "github.com/elythia-network/elythia/internal/core/systemaccount"
	coretimeline "github.com/elythia-network/elythia/internal/core/timeline"
	coretransfer "github.com/elythia-network/elythia/internal/core/transfer"
	coretranslate "github.com/elythia-network/elythia/internal/core/translate"
	coretwofactor "github.com/elythia-network/elythia/internal/core/twofactor"
	coreurlpreview "github.com/elythia-network/elythia/internal/core/urlpreview"
	"github.com/elythia-network/elythia/internal/core/userpack"
	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
	corewebpush "github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/stream/channels"
	"github.com/elythia-network/elythia/plugin"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"log/slog"
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

	// Remote notes cleaning (issue #46)。processor と、設定を毎回 meta から読む
	// 関数は buildQueue で作る (#3690)。cursor の setter はここ。
	cleanProcessor.SetCursorStore(processors.RedisCursorStore{
		Client: s.redis.Default,
		Key:    s.config.Redis.KeyPrefix() + processors.CleanRemoteNotesCursorKey,
	})

	// Expired-mute prune (#1563 / #1603)。processor は buildQueue で作る (#3690)。
	// 期限付き mute の**自然失効**も snapshot に反映させる (#2453)。relationReloadPublisher
	// の他の配線はすべて利用者の操作が起点だが、期限切れは誰も操作しないので cron が唯一の
	// 通知点になる。ここを繋がないと「1 時間だけミュート」が接続中は解除されない。
	checkExpiredMutingsProcessor.SetRelationReloadPublisher(relationReloadPublisher)

	// Daily generic clean (#1563)。processor は buildQueue で作る (#3690)。
	// 下の SetIPLookupLogPruner は、buildRepos の ipLookupLogRepo と同じ種類の
	// repository をここで作っている (#3684 でまとめなかった)。ゲート
	// (TestIPLookupLogRetentionIsWired) が、nil を渡せないよう構築の式そのものを
	// 照合しているため。db しか持たないので、2 つあっても動きは同じ。
	// IP 照会の監査記録にも保持期間を掛ける (#3106)。**この行が無いと記録が永久に
	// 残り、`moderation_log` に IP を書くのと変わらなくなる。**
	cleanGenericProcessor.SetIPLookupLogPruner(repository.NewIPLookupLogRepository(s.db))
	// 対戦の記録は終局から 30 日で消す (#3232)。
	cleanGenericProcessor.SetBubbleVersusRecordPruner(bubbleVersusRecordRepo)

	// Reaction flush (issue #57): buffered writer 使用時は 30 秒ごとに flush。
	flushProcessor := processors.NewReactionFlushProcessor(reactionCountWriter)

	// Account cascade deletion (issue #187): admin/accounts/delete の後続
	// バックグラウンド処理。note / drive_file / following 関連行を掃除する。
	// #3690 で buildQueue へ移したが、setupRoutes で作る webPushCache を構築子で受ける
	// ため #3829 でここへ戻した。
	// pageRepo (#3293): ページを 1 件ずつ消して、参照するノートの pageCount を減らす。
	// webPushCache (#3207): 消した sw_subscription が購読キャッシュに残って push され
	// 続けないよう、Web Push の配送が読むのと同じキャッシュを渡す。
	deleteAccountProcessor := processors.NewDeleteAccountProcessor(noteRepo, driveFileRepo, followingRepo, pageRepo, webPushCache)
	// #2230: local user の物理削除に userRepo を配線。未配線だと soft 削除止まりで行が残る。
	deleteAccountProcessor.SetUserRepo(userRepo)

	// 30s ごとに buffered reaction count を DB へ drain する。30s ticker は
	// upstream の日次 cron より高頻度で反映遅延を抑える mk-go の運用。
	//
	// **ticker は設定に関わらず回し、積むかは毎回 writer に聞く (#3663)。**
	// 有効の間と、無効にしてから `BufferDrainWindow` の間だけ積む。起動時の
	// 値で ticker の有無を決めると、無効で起動してから有効にしたときに誰も
	// flush しない。
	//
	// **ticker は全部の役割で回し、tick ごとに meta から設定を読み直す。**
	// web だけのプロセスも、通知を取りこぼしたまま溜め続けたり、窓を過ぎて
	// 移さずに書いたりしないよう、meta のキャッシュの期限で揃える
	// (reactionFlushTick)。止め方は他の構築時の worker と同じ
	// (addWorker の停止の hook)。
	//
	// **flush を積むのは queue role だけ** (#2459)。`EnqueueReactionFlush` の
	// `WithUnique` TTL は 25s で ticker 間隔 30s より短いため、複数ノードが
	// 別タイミングで打つと重複排除をすり抜けて二重に積まれる。
	var enqueueReactionFlush func() error
	if s.role.RunsQueue() {
		enqueueReactionFlush = s.queueClient.EnqueueReactionFlush
	}
	s.addWorker("reactionFlushWorker", func(ctx context.Context) {
		s.reactionFlushWorker(ctx, func() error {
			return reactionFlushTick(cachedMeta, reactionCountWriter, enqueueReactionFlush)
		})
	})

	// Charts (Phase 4 Step Q / 4.7) — management service. The 12 chart
	// engines (chartCharts) and the hook adapter (chartHooks) are built
	// right after the repositories so that each consumer gets the hook
	// as soon as it is constructed (#3676).
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
	// instance の notesCount / usersCount (#3330)。本家と同じく chart hook の
	// enableStatsForFederatedInstances の分岐の中で動かす。notesCount の加算は
	// inbox の最頻経路なので、本家の CollapsedQueue と同じく窓で合算してから書く。
	//
	// chartHooksはrepositoryの直後で作るが、このバッファは起動と停止を持つ部品(#3699)として
	// 一覧に登録するので、部品の登録順(この順で起動し、FIFOで止まる)を変えないようここに
	// 残す(#3676)。起動はStartBackgroundで行う(#3703)。chartHooks のフィールドに入れるだけなので setter の辺ではない。
	instanceCounterBuffer := coreinstance.NewCounterBuffer(instanceRepo, 30*time.Second)
	s.addStartCloser(context.Background(), "instanceCounterBuffer", instanceCounterBuffer)
	chartHooks.InstanceCounter = instanceCounterBuffer
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
	// 落ちた配送先へのブレーカーと 429 の間隔 (#3048)。状態は Redis に置くので、
	// queue ノードが複数あっても判断を共有する。管理画面 (Web ノード) からも
	// 同じ状態を読み、手で閉じる。deliveryBreaker は buildFederation で作る (#3688)。
	if s.role.RunsQueue() {
		if deliveryBreaker != nil {
			deliverProcessor.SetDeliveryBreaker(deliveryBreaker)
		}
		deliverProcessor.SetDeliveryTelemetry(deliveryHealth)
		s.addStartStopper(context.Background(), "deliveryHealth", deliveryHealth)
		inboxProcessor.SetDeliveryTelemetry(inboxHealth)
		s.addStartStopper(context.Background(), "inboxHealth", inboxHealth)
	}

	noteCreateService.SetHashtagHook(hashtagService)
	federationResolver.SetHashtagHook(hashtagService)
	// i/update の profile description 由来 user.tags 変更で hashtag attach 集計
	// (attachedUsersCount) を追従させる (#1362)。
	userService.SetUsertagHook(hashtagService)
	// 停止は Close(ctx) で、in-flight worker を ctx 期限内に drain し、時間切れを
	// 警告に出す (#727、#3717。理由は Close のコメント)。
	// #764: 旧版は Server.hashtagService field 経由で Shutdown() から呼ばれて
	// いたが、ctx-aware hook 化で field 不要になった。
	s.addCloser("hashtagService", hashtagService)

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

	// Periodic remote-instance metadata refresh (#393)。
	// `metadataFetcher` は instance service 初期化時に作成済の
	// coreinstance.FetchMetadataService を流用する。
	instanceRefreshProc := processors.NewInstanceRefreshProcessor(instanceRepo, metadataFetcher, processors.InstanceRefreshConfig{})

	// 起動時にも 1 回 retention の aggregation (#421) を発火する。cron (0 0 * * *) を待つと
	// 新規デプロイ後最大 24h は heatmap が空のままになるので、その日の
	// cohort 行を即座に作って描画を始められるようにする (#421)。同じ
	// dateKey で 2 回目を Insert しても repository.ErrDuplicateKey で
	// silently 吸収されるので idempotent。DB 書き込みを startup hot path に
	// 乗せないよう goroutine で実行する。
	//
	// cron の担当と揃えて **queue role でだけ**発火する (#2459)。idempotent
	// なので複数ノードで走っても壊れないが、Web ノードを増やすたびに起動時の
	// DB 書き込みが増えるのは無駄。
	//
	// **addWorker で走らせる** (#3463)。素の goroutine だと、
	// メンテナンスで世代を止めて DB のプールを閉じた後も走り続け、閉じたプールへ
	// 書きに行く (実測で "sql: database is closed" が出た)。shutdown hook が
	// 終わるのを待つので、DB を閉じる前に終わる。
	if s.role.RunsQueue() {
		s.addWorker("retentionAggregateAtStartup", func(ctx context.Context) {
			if err := retentionSvc.Aggregate(ctx); err != nil {
				slog.Warn("retention aggregate at startup failed", "err", err)
			}
		})
	}

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
	// 本家のparamDefで引数を検証する。本家に揃えたendpoint(clips/*、charts/*、hashtags/*、antennas/*、flash/*、gallery/*、following/*、channels/*、blocking/*、mute/*、renote-mute/*、roles/*、invite/*、notifications/*、app/*、auth/*、miauth/gen-token、my/apps、sw/*、announcements、promo/read、email-address/available、username/available、ap/*、bubble-game/ranking、bubble-game/register、federation/instancesを除くfederation/*、pages/*、page-push、参照だけのトップレベルのendpoint、users、users/*、drive、drive/*、chat/*、reversi/matchを除くreversi/*)では本家と
	// 同じ400で拒否し(#3742)、それ以外では応答を変えずに、handlerと判定が
	// 分かれた件数を数える(#3677)。検証そのものはrouteのmiddlewareの末尾
	// (withParamShadow)で走る。
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
	// プラグインが管理するアカウント (#3468) は数えない。
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
	// enableReactionsBuffering も同じ 2 つの経路で読み直す (#3663)。切り替えの
	// 途中のバッファの扱いは corereaction.SwitchableWriter に書いてある。
	reloadReactionBuffering := func() {
		m, err := cachedMeta.Fetch()
		if err != nil || m == nil {
			// 読めなければ据え置く (captcha と同じ)。
			slog.Warn("reactions: buffering reload skipped (meta unavailable)", "err", err)
			return
		}
		reactionCountWriter.SetBuffering(m.EnableReactionsBuffering)
	}
	// meta の系統のキャッシュを一覧に登録する (#3721)。捨てる順は下の hook と購読と
	// 同じで、meta 行を先に捨てる (逆だと、間に来たリクエストが古い行から組み直す)。
	s.addCache("cachedMeta", cacheFromDB, cachedMeta)
	s.addCache("metaResponse", cacheFromDB, metaHandler)
	s.addCache("nodeinfo", cacheFromDB, nodeinfoHandler)
	// captchaとリアクションの切り替えはmetaから読み直す項目として登録する(#3777)。
	// 捨てて空にするとcaptchaが無効と同じになるので、捨てずに読み直す。hookと購読と
	// 同じく、meta行を捨てた後に読み直す。
	s.addCache("captcha", cacheFromDB, reloadOnDrop(reloadCaptcha))
	s.addCache("reactionBuffering", cacheFromDB, reloadOnDrop(reloadReactionBuffering))
	cachedMeta.SetInvalidationHook(func() {
		metaHandler.InvalidateResponseCache()
		// /nodeinfo の応答 (10 分) も meta から組むので、同じ順序で捨てる (#3663)。
		nodeinfoHandler.InvalidateCache()
		reloadCaptcha()
		reloadReactionBuffering()
		if err := internalPubSub.Publish(context.Background(), "metaUpdated", struct{}{}); err != nil {
			slog.Warn("meta: publish metaUpdated failed", "err", err)
		}
	})
	internalPubSub.Subscribe(context.Background(), "metaUpdated", func(payload []byte) {
		cachedMeta.Invalidate()
		metaHandler.InvalidateResponseCache()
		nodeinfoHandler.InvalidateCache()
		reloadCaptcha()
		reloadReactionBuffering()
		if hook := s.metaUpdatedHandled.Load(); hook != nil {
			(*hook)(payload)
		}
	})
	// 連合のルール (#3090) は受信の経路でスナップショットを持つので、管理画面で
	// 変えたら他のプロセスにも読み直させる (繋がないと最大 5 分古いルールで
	// 評価し続ける)。
	internalPubSub.Subscribe(context.Background(), "federationRulesUpdated", func([]byte) {
		fedRuleService.Invalidate()
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

	// #3607: 閲覧者ごとの関係のキャッシュの cross-worker invalidation。
	// 関係の repository に書いたプロセスは自分のキャッシュを捨ててから
	// internal:relationCacheInvalidated を publish し、他のプロセスは受けて捨てる。
	// 連合の受信 (Block / Follow / Undo / Accept / Reject) は MK_ONLY_QUEUE の
	// プロセスで書かれ、タイムラインは API のプロセスで読まれるので、繋がないと
	// ミュート・ブロックが TTL (1 分) の間効かない。自分の publish は受け流す。
	corerelationcache.Connect(s.relationCache, internalPubSub, idGen.Generate(time.Now()))
	notesHandler.SetRelationCache(s.relationCache)
	usersHandler.SetRelationCache(s.relationCache)
	antennasHandler.SetRelationCache(s.relationCache)
	channelsHandler.SetRelationCache(s.relationCache)
	clipsHandler.SetRelationCache(s.relationCache)

	// #3664: インスタンスの停止判定の cross-worker invalidation。停止の操作は
	// Web のプロセスで受け、配送の判定は MK_ONLY_QUEUE のプロセスで走るので、
	// 繋がないと停止したはずの相手へ最大 5 分 (判定のキャッシュの TTL) 配送が続く。
	// 自プロセスの判定は publish の前に捨て済みで、自分の publish は受け流す。
	cachebus.ConnectInstanceSuspension(instanceService, internalPubSub, idGen.Generate(time.Now()))
	// #3664: relay の一覧 (承認済みのもの) も同じ。Accept / Reject は連合の受信で、
	// 追加・削除は管理画面で書かれ、ノートを relay へ配るのは投稿を受けたプロセス。
	cachebus.ConnectRelays(relaySvc, internalPubSub, idGen.Generate(time.Now()))
	// #3664: 認証の tokenCache の cross-worker invalidation。捨てた利用者の ID だけを
	// 伝え、他のプロセスはその利用者の entry を捨てる (token は流さない)。
	cachebus.ConnectAuth(s.auth, internalPubSub, idGen.Generate(time.Now()))
	// 利用者と関係の系統のキャッシュを一覧に登録する (#3723)。3 つとも newServer で
	// 作る。tokenCache は利用者を DB から読み (native token は FindByToken、app token は
	// FindByHashOrToken の Preload)、利用者のキャッシュを通らないので、捨てる順の制約は無い。
	s.addCache("userCache", cacheFromDB, s.userCache)
	s.addCache("relationCache", cacheFromDB, s.relationCache)
	s.addCache("authTokens", cacheFromDB, s.auth)
	// ロールの系統のキャッシュ (#3725)。利用者のロール・ロールの一覧と、プラグインの
	// policy の結果を持つ。**利用者のキャッシュの後に登録する。** 条件付きロールは
	// 利用者の行 (CachedUserRepository) から評価して利用者のロールのキャッシュに積むので、
	// 先に捨てると、間に来たリクエストが古い利用者の行で評価した結果を積む。meta の
	// base policy は評価のたびに meta のキャッシュから読み、ロールのキャッシュには持た
	// ないので、meta の系統との間に順の制約は無い。
	s.addCache("roles", cacheFromDB, roleService)

	// #3383: 絵文字の (name, host) cache の cross-worker invalidation。
	// リモート絵文字の作成・更新は連合の受信 (MK_ONLY_QUEUE のプロセス) で
	// 起き、それを表示に使うのは API のプロセスなので、繋がないと作られた
	// 絵文字が負キャッシュの TTL (1 分) の間、更新・削除が正の TTL (10 分) の
	// 間反映されない。
	//
	// **書き込んだプロセスは自分で精密に落とし済み**なので、自分の publish は
	// 受け流す (rolesUpdated と同じ)。受信側は ApplyRemoteInvalidation で落とし、
	// hook を呼ばないので通知の投げ合いにならない。
	//
	// **publish は書き込みの経路から切り離す。** upsertEmojis は絵文字 1 つごとに
	// Create / UpdateFields を呼ぶので、同期で publish すると Redis が詰まった
	// ときに受信が「絵文字数 x timeout」止まる。queue が溢れたら全体の
	// invalidation に畳む (黙って捨てない)。publish 自体の失敗は TTL が上限になる。
	emojiCacheSender := idGen.Generate(time.Now())
	type emojiCacheMessage struct {
		From string                            `json:"from"`
		Inv  repository.EmojiCacheInvalidation `json:"inv"`
	}
	emojiInvalidationQueue := repository.NewEmojiInvalidationQueue(1024, func(inv repository.EmojiCacheInvalidation) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := internalPubSub.Publish(ctx, cachebus.TopicEmoji, emojiCacheMessage{From: emojiCacheSender, Inv: inv}); err != nil {
			slog.Warn("emoji: publish emojiCacheInvalidated failed", "err", err)
		}
	})
	// 停止時は Run が溜まっている分を最大 2 秒 (送信中の 1 件の timeout を足すと最悪約 4 秒)
	// かけて送ってから返る。プロセスが
	// 先に終わると送れないので、shutdown の deadline の範囲でその完了を待つ(Stop)。
	// Run が返った後の書き込み (停止中にまだ走っている受信ジョブ) は送られず、
	// TTL が上限になる。
	//
	// RunはStartBackgroundで起動する(#3705)。hookは今の位置で繋ぐので、構築から
	// 起動までに積まれた分は溜めておき、起動の後に送る。
	s.addStartStopper(context.Background(), "emojiInvalidationQueue", emojiInvalidationQueue)
	emojiRepo.SetInvalidationHook(emojiInvalidationQueue.Enqueue)
	// 受けた側は絵文字のキャッシュを捨て、ローカルの絵文字に関わる通知では絵文字
	// 由来のデコレーションの map も、その後で捨てる (#3664。順の理由は
	// SubscribeEmojiCache)。**読めない通知と受け側の溢れでは全体を落とす。**
	// 何を落とすべきか分からないまま捨てると、TTL の間古い絵文字を出し続ける。
	cachebus.SubscribeEmojiCache(emojiRepo, emojiDecorationResolver, internalPubSub, emojiCacheSender)

	// #3664: アバターのデコレーション (catalog と絵文字由来の 2 種) の
	// cross-worker invalidation。管理画面の操作を受けたプロセスだけが捨てると、
	// 他のプロセスは TTL (30 秒) の間、作ったばかりのデコレーションを装着から落とす。
	cachebus.ConnectAvatarDecorations(avatarDecorationResolver, emojiDecorationResolver, internalPubSub, idGen.Generate(time.Now()))
	// 絵文字とデコレーションの系統のキャッシュを一覧に登録する (#3727)。**絵文字の後に
	// 絵文字由来のデコレーションを登録する。** emojiDecorations は絵文字のキャッシュの
	// 一覧 (ListLocal) から作り直すので、先に捨てると、間に来たリクエストが古い一覧から
	// 作り直した map を積む (SubscribeEmojiCache と同じ理由)。catalog は DB から読む。
	s.addCache("emojis", cacheFromDB, emojiRepo)
	s.addCache("emojiDecorations", cacheFromDB, emojiDecorationResolver)
	s.addCache("avatarDecorations", cacheFromDB, avatarDecorationResolver)
	// 連合の系統のうち、インスタンスの判定・relay の一覧・連合のルール (#3729)。3 つとも
	// DB を直接読み、meta は判定のたびに cachedMeta から読むだけで持たないので、ほかの
	// 系統との間に捨てる順の制約は無い。
	s.addCache("instances", cacheFromDB, instanceService)
	s.addCache("relays", cacheFromDB, relaySvc)
	s.addCache("fedRules", cacheFromDB, fedRuleService)
	// 連合の系統の残り (#3732)。instance actor の秘密鍵は system account の利用者を
	// CachedUserRepository から引くので、userCache の後に置く (ここは後ろなので満たす)。
	// ほかの 3 つは DB を直接読む。
	s.addCache("deliverEd25519", cacheFromDB, deliverService)
	s.addCache("federationPublicKeys", cacheFromDB, federationResolver)
	s.addCache("deliverSigningKeys", cacheFromDB, deliverProcessor)
	s.addCache("instanceActorSigner", cacheFromDB, instanceSigner)
	// 管理画面と統計の系統 (#3738)。4 つとも DB を直接読むか、DB へ書いたことの印を
	// 持つだけで、ほかのキャッシュの値を含まないので、捨てる順の制約は無い。投稿・
	// タイムライン・通知の系統より後に、それらの元になるキャッシュを置かないため、
	// その前に置く。
	s.addCache("driveUsage", cacheFromDB, driveUsage)
	s.addCache("dbHealth", cacheFromDB, dbHealth)
	s.addCache("statsCounts", cacheFromDB, statsHandler)
	s.addCache("ipLogWindow", cacheFromDB, ipLogService)
	// 投稿・タイムライン・通知の系統 (#3736)。アンテナ・push 購読・注目ノートの順位は
	// DB (順位は Redis) を直接読む。**ストリームの直前のノートとタイムラインの JSON は、
	// pack の元になるキャッシュより後に置く。** どちらも pack した利用者・ロール・絵文字・
	// デコレーション・インスタンスを含むので、先に捨てると、間に来た pack が古い元から
	// 作り直して積む。この後に登録する外部とファイルの系統 (#3776) は、pack の元に
	// ならない。
	// タイムラインの JSON は opt-in で、有効にしたときだけ登録する。
	s.addCache("antennas", cacheFromDB, cachedAntenna)
	s.addCache("pushSubscriptions", cacheFromDB, webPushCache)
	s.addCache("featuredNotes", cacheFromDB, notesHandler.FeaturedRankingCache())
	s.addCache("notePublisher", cacheFromDB, notePublisher)
	if tc := notesHandler.TimelineJSONCache(); tc != nil {
		s.addCache("timelineJSON", cacheFromDB, tc)
	}

	// #3664: 利用者のキャッシュ (CachedUserRepository) の cross-worker invalidation。
	// queue のプロセスがリモートの利用者を更新 (凍結の反映、Update の受信) しても、
	// 繋がないと Web のプロセスは TTL (5 分) の間古い行を返す。書き込みの頻度が
	// 高いので、publish は書き込みの経路から切り離し、溜まった id を 1 通に
	// まとめて送る (理由と量は cachebus.userQueue)。停止時は溜まっている分を
	// 最大 2 秒かけて送ってから止まる (絵文字と同じ)。
	s.crossProcessCaches = crossProcessCaches{
		instances: instanceService, relays: relaySvc, decorations: avatarDecorationResolver,
		emojiDecorations: emojiDecorationResolver, emojis: emojiRepo, fedRules: fedRuleService,
		deliver: deliverService, resolver: federationResolver, deliverProcessor: deliverProcessor,
		instanceSigner: instanceSigner, fetcherSigner: apFetcher.SignerProvider(),
		antennas: cachedAntenna, pushSubscriptions: webPushCache, notes: notesHandler,
		notePublisher: notePublisher, driveUsage: driveUsage, dbHealth: dbHealth, stats: statsHandler,
		ipLog: ipLogService, captcha: captchaSvc,
	}

	userCacheLink := cachebus.ConnectUsers(s.userCache, internalPubSub, idGen.Generate(time.Now()))
	s.addWorker("userCacheLink", userCacheLink.Run)

	// 取り消したリノート / 削除した引用の通知を消す (#3201)。**配線しないと build も
	// テストも通ったまま** 未読件数に中身の無い通知が数えられ続ける。
	noteDeleteService.SetNotificationHook(notificationHook)

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
	adminHandler.SetSelfCheckRunner(&selfCheckAdapter{
		checker: selfcheck.NewChecker(s.config.URL),
		deps:    selfcheck.LocalDeps{DB: s.db, Redis: s.redis.Default, DBHealth: dbHealth.Report},
	})
	// 連合先との疎通の診断 (#3055)。**通信は SSRF-safe な outboundClient だけ**で、
	// 上の self-check の client (ガード無し) とは繋がない。自ホストは断る。
	adminHandler.SetRemoteChecker(s.newRemoteCheckAdapter(remoteCheckSources{
		fetcher:   apFetcher,
		instances: instanceService,
		users:     repository.NewRemoteUserSampler(s.db),
		sigCaps:   sigCapRepo,
		degraded:  deliverProcessor.Ed25519Degraded,
		delivery:  deliveryHealth,
		breaker:   deliveryBreaker,
	}), corefederation.NormalizeGateHost(s.config.URL))
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
	// 目印でもあり、backupGuard が同じ条件をもう一度見る (付け忘れても閉じる)。
	// reauth-challenge はパスキーの challenge を出すだけの前段なので、再認証は
	// 課さず、それ以外の条件を handler の中で見る。
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
	backupGuard := backupHandler.Guard()
	// 画面からのリストア (#3463 (6c)) は daemon へ転送する。再認証 (backupGuard) は
	// 付けない。付けると TOTP のコードを本体と daemon で二重に消費して落ちる。再認証を
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
	registerIRoutes(api, iHandler, webhookHandler, notificationsHandler, pagesHandler, flashHandler, announcementHandler, roleService)
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
	registerAdminRoutes(api, adminHandler, announcementHandler, backupHandler, roleService, backupGuard)
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
	s.echo.GET("/avatar/@:acct", avatarHandler(entityPacker, userRepo, localHost))

	// /emoji/:path — frontend の MkCustomEmoji が `:emojiUrl` プロップを
	// 受け取らないとき (典型: 通知 reaction icon) に <img src="/emoji/<name>@<host>.webp">
	// で直接 fetch するので、handler が無いと SPA catchall に流れて画像が
	// 読めず fallback 画像になる (#468)。upstream Misskey TS の
	// ServerService.ts:153 と互換に: name + host を抜いて emoji table を
	// lookup → publicUrl に 302 redirect、見つからなければ ?fallback クエリ
	// 付きなら /static-assets/emoji-unknown.png、無ければ 404。
	s.echo.GET("/emoji/:path", emojiRedirectHandler(entityPacker, emojiRepo))

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
	embed := newEmbedHandlers(s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability, setupDetector, EmbedDeps{
		NoteRepo: noteRepo,
		UserRepo: userRepo,
		ClipRepo: clipRepo,
		PackNote: func(c echo.Context, n *model.Note) (any, error) {
			return notesHandler.PackForEmbed(c.Request().Context(), n), nil
		},
		PackUser: func(_ echo.Context, u *model.User) (any, error) {
			// upstream の userEntityService.pack(user) 相当 (= UserLite)。
			return entityPacker.PackUserLite(u), nil
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
	// background は更新の goroutine を起動する世代の箱 (#3661)。接続の close
	// handler からも呼ばれるので、世代の停止で待たないと閉じた DB へ書きに行く。
	background *bgwork.Group
}

func (a *lastActiveRecorderAdapter) RecordActive(userID string) {
	if a.userRepo == nil || userID == "" {
		return
	}
	a.background.Go(func(context.Context) {
		if err := a.userRepo.UpdateUser(userID, map[string]any{"lastActiveDate": time.Now()}); err != nil {
			slog.Debug("streaming: lastActiveDate update failed", "userId", userID, "err", err)
		}
	})
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
// outgoing GETs with the instance.actor system account (#419), and the signer
// of the plugin peers. One value per generation is shared by both (#3732).
//
// 初回 Signer() で systemaccount.Fetch + keypair lookup + RSA PEM parse を
// やってから、結果 (parse 済みの *PrivateKey) を mutex 配下にキャッシュする。
// 一度成功した key は期限なしで保持する。世代の作り直しか、キャッシュの一覧の
// DropLocal (#3732) で捨てられ、次の Signer() で読み直す。
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

// DropLocal drops the cached instance actor key and the backoff after a failed
// load (#3732), so the next Signer reads the key again. It is what the cache
// list calls (docs/architecture.md 4.4). 読み込みは mu を握ったまま行うので、
// DropLocal はそれが終わるのを待ってから捨てる (世代の番号は要らない)。
//
// 失敗の backoff も捨てる。ほかの系統の backoff (相手への取得の失敗) と違い、
// これは自分の DB の読み込みの失敗で、DB を戻した後にはもう当てはまらない。残すと、
// 戻す直前の失敗から 30 秒の間、署名なしの取得が続く。
func (s *instanceActorSigner) DropLocal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cachedKey = nil
	s.lastFailure = time.Time{}
}

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
