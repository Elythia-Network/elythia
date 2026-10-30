package server

import (
	"context"
	"time"

	"log/slog"

	apicharts "github.com/elythia-network/elythia/internal/api/charts"
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/core/chart/charthook"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/hashtag"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/core/note"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/core/retention"
	"github.com/elythia-network/elythia/internal/core/user"
	corewebpush "github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/repository"
)

// backgroundJobsDeps holds the parts wireBackgroundJobs receives from
// setupRoutes (#3901). setupRoutes sets every field from the variable of the
// same name; TestSetupRoutesSectionsReceiveSameNamedVariables checks that no
// field is left out or filled from a different variable.
type backgroundJobsDeps struct {
	relationReloadPublisher      *relationReloadPublisherAdapter
	userRepo                     repository.UserRepository
	noteRepo                     repository.NoteRepository
	cachedMeta                   *repository.CachedMetaRepository
	followingRepo                repository.FollowingRepository
	driveFileRepo                repository.DriveFileRepository
	instanceRepo                 repository.InstanceRepository
	pageRepo                     repository.PageRepository
	bubbleVersusRecordRepo       repository.BubbleVersusRepository
	chartCharts                  apicharts.Charts
	chartHooks                   *charthook.Hooks
	hashtagService               *hashtag.Service
	noteCreateService            *note.CreateService
	userService                  *user.Service
	retentionSvc                 *retention.Service
	deliveryBreaker              *deliveryhealth.Breaker
	reactionCountWriter          *corereaction.SwitchableWriter
	webPushCache                 *corewebpush.SubscriptionCache
	cleanProcessor               *processors.CleanRemoteNotesProcessor
	checkExpiredMutingsProcessor *processors.CheckExpiredMutingsProcessor
	cleanGenericProcessor        *processors.CleanProcessor
	federationResolver           *corefederation.Resolver
	metadataFetcher              *coreinstance.FetchMetadataService
	deliverProcessor             *processors.DeliverProcessor
	inboxProcessor               *processors.InboxProcessor
}

// wireBackgroundJobs wires the queue processors of the periodic and
// follow-up jobs (remote note cleaning, expired mutes, the daily clean, the
// reaction flush, account deletion), the chart management service with its
// cron processor and the delivery / inbox health services, the remote
// instance metadata refresh, and the retention aggregation at startup.
// setupRoutes calls it where these statements used to be, so the parts are
// added to the start / stop list in the same order (docs/architecture.md
// 4.3), and receives the parts it returns under the same names. It binds
// every field of d to the name setupRoutes uses, so the moved statements read
// the same as before (#3901).
func (s *Server) wireBackgroundJobs(d backgroundJobsDeps) (*processors.ReactionFlushProcessor, *processors.DeleteAccountProcessor, *deliveryhealth.Service, *deliveryhealth.Service, *processors.ChartProcessor, *processors.InstanceRefreshProcessor) {
	relationReloadPublisher := d.relationReloadPublisher
	userRepo := d.userRepo
	noteRepo := d.noteRepo
	cachedMeta := d.cachedMeta
	followingRepo := d.followingRepo
	driveFileRepo := d.driveFileRepo
	instanceRepo := d.instanceRepo
	pageRepo := d.pageRepo
	bubbleVersusRecordRepo := d.bubbleVersusRecordRepo
	chartCharts := d.chartCharts
	chartHooks := d.chartHooks
	hashtagService := d.hashtagService
	noteCreateService := d.noteCreateService
	userService := d.userService
	retentionSvc := d.retentionSvc
	deliveryBreaker := d.deliveryBreaker
	reactionCountWriter := d.reactionCountWriter
	webPushCache := d.webPushCache
	cleanProcessor := d.cleanProcessor
	checkExpiredMutingsProcessor := d.checkExpiredMutingsProcessor
	cleanGenericProcessor := d.cleanGenericProcessor
	federationResolver := d.federationResolver
	metadataFetcher := d.metadataFetcher
	deliverProcessor := d.deliverProcessor
	inboxProcessor := d.inboxProcessor

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
	return flushProcessor, deleteAccountProcessor, deliveryHealth, inboxHealth, chartProcessor, instanceRefreshProc
}
