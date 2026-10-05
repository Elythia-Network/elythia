package queue

import (
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// MaintenanceQueueName is the queue used for low-priority maintenance
// jobs (chart tick / resync / clean etc). Kept separate from the
// latency-sensitive deliver queue so a long aggregation does not stall
// federation traffic.
const MaintenanceQueueName = "maintenance"

// Scheduler is the driver-neutral facade over driver.Scheduler. It
// registers cron-style entries once at startup and continuously
// enqueues tasks per their schedule onto the standard worker queues.
type Scheduler struct {
	inner driver.Scheduler
}

// **cron の時刻はどの TZ でも UTC ではない。** Register は cron 式をそのまま
// driver へ渡し、TZ を指定していない。mkq (既定) は `time.Local` で解釈する
// ので**プロセスの TZ** に従う (`docker-compose.yml` は `TZ: Asia/Tokyo` を
// 設定しているので既定構成では JST)。
// 以下の各 job のコメントに書いてある時刻は「その TZ での時刻」を指す。
//
// NewScheduler wraps the driver's scheduler.
func NewScheduler(d driver.Driver) *Scheduler {
	return &Scheduler{inner: d.Scheduler()}
}

// ScheduledJobRetention is how long completed / failed iterations of a
// cron job are kept, matching upstream QueueService (7 days).
const ScheduledJobRetention = 7 * 24 * time.Hour

// register is the single entry point for every cron registration, so a
// new job cannot be added without retention.
//
// **定期ジョブの completed / failed は保持期間を付けないと無期限に溜まる**
// (本番の maintenance で完了 57,614 件・失敗 9,319 件、最古は運用開始日)。
// upstream は `upsertJobScheduler` の template に `removeOnComplete` /
// `removeOnFail` を `{ age: 7 日 }` で置いている。mkq v1.2.0 で scheduler にも
// 指定できるようになった (mkq#109)。
//
// **件数ではなく期限で指定する。** mkq の CHANGELOG のとおり、件数指定は
// 溜まった分を 1 回の Lua で全部消そうとして Redis を止めうる。期限指定は
// 1 回の完了につき最大 1000 件ずつ刈るので、溜まった分も数回で掃ける。
// upstream が件数を使わないのは、キュー全体で数えるため高頻度の job が
// 低頻度の job の記録を押し出すから (QueueService.ts のコメント)。
func (s *Scheduler) register(cronspec, taskType string, payload []byte, opts ...driver.EnqueueOption) error {
	opts = append(opts,
		driver.WithKeepCompletedAge(ScheduledJobRetention),
		driver.WithKeepFailedAge(ScheduledJobRetention),
	)
	return s.inner.Register(cronspec, taskType, payload, opts...)
}

// RegisterChartJobs registers the 3 chart-related cron jobs.
//
//   - tickCharts : 毎時 55 分 (TS Misskey と同一 cron pattern)
//   - resyncCharts: 毎日 00:00
//   - cleanCharts : 毎日 00:00
//
// 重複 enqueue 防止のため Unique TTL を cron 周期と合わせる。前回の
// 同種ジョブが処理中のまま次回 cron が発火しても、TTL 内であれば
// driver が重複 enqueue を弾く。
func (s *Scheduler) RegisterChartJobs() error {
	jobs := []struct {
		cron      string
		taskType  string
		uniqueTTL time.Duration
	}{
		{"55 * * * *", TaskTypeChartTick, 1 * time.Hour},
		{"0 0 * * *", TaskTypeChartResync, 24 * time.Hour},
		{"0 0 * * *", TaskTypeChartClean, 24 * time.Hour},
	}
	for _, j := range jobs {
		if err := s.register(j.cron, j.taskType, nil,
			driver.WithQueue(MaintenanceQueueName),
			driver.WithMaxRetry(0),
			driver.WithUnique(j.uniqueTTL),
		); err != nil {
			return err
		}
	}
	return nil
}

// RegisterInstanceRefreshJob registers the daily remote-instance metadata
// refresh cron (#393) at 03:00. The actual walk + fetch is implemented
// by processors.InstanceRefreshProcessor.
func (s *Scheduler) RegisterInstanceRefreshJob() error {
	return s.register("0 3 * * *", TaskTypeInstanceRefresh, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(24*time.Hour),
	)
}

// RegisterRetentionJob registers the daily retention aggregation cron
// (#421) at 00:00. The actual computation is implemented by
// processors.RetentionAggregateProcessor.
func (s *Scheduler) RegisterRetentionJob() error {
	return s.register("0 0 * * *", TaskTypeRetentionAggregate, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(24*time.Hour),
	)
}

// RegisterCheckExpiredMutingsJob registers the expired-mute prune cron
// (#1563) every 5 minutes, mirroring upstream `checkExpiredMutings`. The
// prune is implemented by processors.CheckExpiredMutingsProcessor.
func (s *Scheduler) RegisterCheckExpiredMutingsJob() error {
	return s.register("*/5 * * * *", TaskTypeCheckExpiredMutings, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(5*time.Minute),
	)
}

// RegisterResendQuoteRequestsJob registers the per-minute resend of pending
// QuoteRequests (#3238)。送り直しの予定は 1 分単位なので毎分回す。
func (s *Scheduler) RegisterResendQuoteRequestsJob() error {
	return s.register("* * * * *", TaskTypeResendQuoteRequests, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(time.Minute),
	)
}

// RegisterCleanJob registers the daily generic clean cron (#1563) at 00:00,
// mirroring upstream `clean`. user_ip 90 日 prune / 期限切れ
// role_assignment 削除 / reversi outdated game 削除を processors.CleanProcessor
// が行う。
func (s *Scheduler) RegisterCleanJob() error {
	return s.register("0 0 * * *", TaskTypeClean, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(24*time.Hour),
	)
}

// RegisterChunkedUploadGCJob registers the chunked upload session GC cron
// (#2313) every 15 minutes. 期限切れセッションの multipart upload を abort して
// 行を消す。頻度を上げているのは、オブジェクトストレージが未完了の
// マルチパートアップロードにも課金するため (詳細は TaskTypeChunkedUploadGC)。
func (s *Scheduler) RegisterChunkedUploadGCJob() error {
	return s.register("*/15 * * * *", TaskTypeChunkedUploadGC, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(15*time.Minute),
	)
}

// RegisterPluginJob registers a plugin's cron entry (#2478).
//
// **プラグイン専用のキュー (`plugin:<名前>`) に載せる (#2818)。** 以前は
// maintenance に相乗りしていたが、1 つのプラグインの cron が詰まると本体の
// 定期処理まで止まる。以下は maintenance に載せていた頃の理由で、配送
// (deliver) のレイテンシに影響させないという目的はそのまま専用キューが果たす。
//
// 旧: maintenance キューに載せるのは、プラグインの定期処理が配送 (deliver) の
// レイテンシに影響しないようにするため。プラグインの処理時間は mk-go 側から
// 見積もれないので、遅い処理が来ても連合が詰まらない側に置く。
//
// Unique TTL を設けないのは cron 周期が任意だから。周期より長い TTL を勝手に
// 決めると発火を落とすことになる。重複が困る処理はプラグイン側で冪等に書く
// (トランザクションを跨げない制約と同じ理由で、いずれにせよ冪等性は要る)。
func (s *Scheduler) RegisterPluginJob(cron string, plugin, job string, payload []byte) error {
	// **maintenance ではなくプラグイン専用のキューへ (#2818)。** 相乗りだと、
	// 1 つのプラグインの cron が詰まったときに本体の定期処理まで止まる。
	return s.register(cron, PluginTaskType(plugin, job), payload,
		driver.WithQueue(PluginQueueName(plugin)),
		driver.WithMaxRetry(0),
	)
}

// RegisterOrphanUserCleanupJob registers the daily cleanup of relay-derived
// orphan remote users (#2340) at 05:00.
//
// cleanRemoteNotes (04:00) の後に置くのは、ノートが先に消えた方が「ノート 0 件」
// の条件に合致する行が増えるため。enable gate は processor 側で meta を見るので
// cron は無条件登録する。
func (s *Scheduler) RegisterOrphanUserCleanupJob() error {
	return s.register("0 5 * * *", TaskTypeOrphanUserCleanup, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(24*time.Hour),
	)
}

// RegisterOrphanAttachmentCleanupJob registers the daily cleanup of owner-less
// remote link attachments (#2722) at 05:30.
//
// orphanUserCleanup (05:00) の後に置く。あちらが親 user を消すと
// ON DELETE SET NULL で owner 無しの行が増えるため。**ただし同じ日のうちに
// 回収される保証は無い** — orphanUserCleanup も 200 行 x 最大 1000 バッチ x
// 500ms pause の構成なので、backlog があれば 30 分では終わらない。その場合
// 回収は翌日に回る (どちらも冪等なので問題は無い)。
//
// enable gate は processor 側で meta を見るので cron は無条件登録する。
func (s *Scheduler) RegisterOrphanAttachmentCleanupJob() error {
	return s.register("30 5 * * *", TaskTypeOrphanAttachmentCleanup, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(24*time.Hour),
	)
}

// RegisterCleanRemoteNotesJob registers the daily remote-note cleanup cron
// (#1563) at 04:00, mirroring upstream `cleanRemoteNotes`. 旧実装は
// router.go の 6h time.Ticker だったが、TS の systemQueue cron に揃える。
// enable gate は processor 側で meta を見るので cron は無条件登録する。
func (s *Scheduler) RegisterCleanRemoteNotesJob() error {
	return s.register("0 4 * * *", TaskTypeCleanRemoteNotes, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(24*time.Hour),
	)
}

// RegisterCheckModeratorsActivityJob registers the hourly moderator-activity
// cron (#1563) at minute 30, mirroring upstream `checkModeratorsActivity`.
// processors.CheckModeratorsActivityProcessor が inactivity 判定と通知を行う。
// remaining.hours%6 の de-dup が崩れないよう 1h cadence を保つ (ticker 不可)。
func (s *Scheduler) RegisterCheckModeratorsActivityJob() error {
	return s.register("30 * * * *", TaskTypeCheckModeratorsActivity, nil,
		driver.WithQueue(MaintenanceQueueName),
		driver.WithMaxRetry(0),
		driver.WithUnique(1*time.Hour),
	)
}

// PruneUnregistered removes the cron schedules left in Redis that this
// process did not register (e.g. a job moved to another queue or renamed),
// returning them as "<queue>/<scheduleID>". Call it after every Register*.
func (s *Scheduler) PruneUnregistered() ([]string, error) {
	return s.inner.PruneUnregistered()
}

// Start launches the scheduler in the background. Returns immediately.
func (s *Scheduler) Start() error { return s.inner.Start() }

// Shutdown stops the scheduler and releases its connection.
func (s *Scheduler) Shutdown() { s.inner.Shutdown() }
