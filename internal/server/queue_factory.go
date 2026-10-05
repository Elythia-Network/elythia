package server

import (
	"context"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
)

// buildQueueDriver constructs the queue driver selected by config.
//
// mkq connects to Redis via mkq.NewClient (PING + SCRIPT LOAD); failures
// bubble up so the server fails to start rather than running with a queue
// that cannot be reached.
//
// `<queue>JobConcurrency` / `<queue>JobPerSec` / `<queue>JobMaxAttempts`
// 系の config は queue_factory が driver Config に流して runtime に反映
// する。deliver / inbox / relationship の 3 queue に対して有効
// (#495 / #534 / #2403)。
func buildQueueDriver(ctx context.Context, cfg *config.Config, pluginQueues []string) (driver.Driver, error) {
	totalConcurrency := 16
	if cfg.DeliverJobConcurrency != nil && *cfg.DeliverJobConcurrency > 0 {
		totalConcurrency = *cfg.DeliverJobConcurrency
	}

	queueConcurrency := perQueueConcurrencyFromConfig(cfg)
	queueRateLimits := perQueueRatesFromConfig(cfg)

	// 空 string は config.resolveJobQueueDriver で "mkq" に正規化されている
	// 想定だが、queue_factory が直接 *config.Config を受けるテスト経由など
	// 正規化されない経路もあるので、ここでも "" → "mkq" に倒す。
	// mkq は唯一の driver だが、config 側の検証が落ちたときに別の driver 名で
	// 黙って mkq を起動しないよう、既知の名前だけを受ける形は残す (#2985)。
	switch cfg.JobQueueDriver {
	case "mkq", "":
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return mkqdriver.New(dialCtx, mkqConfig(cfg, totalConcurrency, queueConcurrency, queueRateLimits, pluginQueues))
	default:
		return nil, fmt.Errorf("server: unknown jobQueueDriver %q", cfg.JobQueueDriver)
	}
}

// mkqConfig assembles the mkq driver configuration from the operator config.
//
// **buildQueueDriver から切り出してある理由**: あちらは Redis へ実接続するので
// テストから呼べず、config から driver へ値が渡っているかを確かめる手段が
// 無くなる。`queueStuckWorkerSeconds` (#2657) と
// `queueHandlerDeadlineSeconds` (#2658) は機構を止める唯一の手段なので、
// 配線が落ちたら気付けるようにしておく。
func mkqConfig(cfg *config.Config, totalConcurrency int, queueConcurrency, queueRateLimits map[string]int, pluginQueues []string) mkqdriver.Config {
	// **既定の一覧を書き換えず足すだけにする。** ここで本体のキューを
	// 列挙し直すと、キューを増やしたときに片側更新になる。
	var names []string
	if len(pluginQueues) > 0 {
		names = append(append(names, mkqdriver.QueueNames...), pluginQueues...)
	}
	return mkqdriver.Config{
		Redis:            mkqdriver.BuildRedisOptions(cfg.RedisForJobQueue),
		QueueNames:       names,
		Concurrency:      totalConcurrency,
		QueueConcurrency: queueConcurrency,
		QueueRateLimits:  queueRateLimits,
		IdlePollInterval: time.Duration(cfg.QueueIdlePollSeconds) * time.Second,
		StuckWorkerAfter: stuckWorkerAfter(cfg),
		HandlerDeadline:  handlerDeadline(cfg),
	}
}

// stuckWorkerAfter converts queueStuckWorkerSeconds into the driver's
// duration form. 0 keeps the per-queue defaults; a negative value is
// preserved as "disabled" rather than clamped, so operators can switch
// quarantine off by editing config and restarting, without waiting for a
// patched binary (#2657). 設定の hot reload は無いので再起動は要る。
//
// **これだけでは supervisor は止まらない。** 期限 (#2658) が有効なら漏れの
// 予算を適用するために起動し続ける。完全に止めるなら
// queueHandlerDeadlineSeconds にも負値が要る。
func stuckWorkerAfter(cfg *config.Config) time.Duration {
	if cfg.QueueStuckWorkerSeconds < 0 {
		return -1
	}
	return time.Duration(cfg.QueueStuckWorkerSeconds) * time.Second
}

// handlerDeadline converts queueHandlerDeadlineSeconds into the driver's
// duration form. Negative is preserved as "disabled" rather than clamped,
// for the same reason as stuckWorkerAfter (#2658).
func handlerDeadline(cfg *config.Config) time.Duration {
	if cfg.QueueHandlerDeadlineSeconds < 0 {
		return -1
	}
	return time.Duration(cfg.QueueHandlerDeadlineSeconds) * time.Second
}

// perQueueConcurrencyFromConfig flattens the deliver/inbox/relationship
// concurrency knobs into a queue-name → worker-count map.
func perQueueConcurrencyFromConfig(cfg *config.Config) map[string]int {
	out := map[string]int{}
	if cfg.DeliverJobConcurrency != nil && *cfg.DeliverJobConcurrency > 0 {
		out[queue.QueueName] = *cfg.DeliverJobConcurrency
	}
	if cfg.InboxJobConcurrency != nil && *cfg.InboxJobConcurrency > 0 {
		out[queue.InboxQueueName] = *cfg.InboxJobConcurrency
	}
	if cfg.RelationshipJobConcurrency != nil && *cfg.RelationshipJobConcurrency > 0 {
		out[queue.RelationshipQueueName] = *cfg.RelationshipJobConcurrency
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// perQueueRatesFromConfig builds the queue-name → tasks/sec map applied to
// the driver's rate limiter (mkq.WithRateLimit).
func perQueueRatesFromConfig(cfg *config.Config) map[string]int {
	out := map[string]int{}
	if cfg.DeliverJobPerSec != nil && *cfg.DeliverJobPerSec > 0 {
		out[queue.QueueName] = *cfg.DeliverJobPerSec
	}
	if cfg.InboxJobPerSec != nil && *cfg.InboxJobPerSec > 0 {
		out[queue.InboxQueueName] = *cfg.InboxJobPerSec
	}
	if cfg.RelationshipJobPerSec != nil && *cfg.RelationshipJobPerSec > 0 {
		out[queue.RelationshipQueueName] = *cfg.RelationshipJobPerSec
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// defaultDeliverJobMaxAttempts / defaultInboxJobMaxAttempts are the total
// number of tries (initial + retries) applied to the deliver / inbox queues
// when the operator hasn't set `<queue>JobMaxAttempts`. They mirror Misskey
// TS QueueService.ts (`deliverJobMaxAttempts ?? 12` / `inboxJobMaxAttempts
// ?? 8`) so delivery to a temporarily-down remote retries instead of failing
// on the first attempt.
//
// mkq の attempts=0 は「リトライ無し」であり、ここでデフォルトを当てないと
// 落ちた配送先への retry-backoff (#1406) がそもそも発火しない。drop-in 互換の
// ためにも TS と同じ既定値を使う (#1411)。
const (
	defaultDeliverJobMaxAttempts = 12
	defaultInboxJobMaxAttempts   = 8
)

// defaultKeepFailed bounds the failed bucket retention applied to inbox /
// deliver enqueue when operator が config で明示しない場合の安全側 default。
// 1000 件あれば admin UI の Errored Instances panel (host aggregation で
// top-N 表示) は確実に賄え、redis memory pressure (1 job ≈ 1KB × 2 queue
// = 2MB) も問題にならない。`<queue>JobKeepFailed: 0` で operator が明示
// 解除可能 (= 蓄積無制限の従来挙動)。
const defaultKeepFailed = 1000

// defaultKeepCompleted は completed bucket retention の最大件数 default。
// Misskey TS upstream の QueueService.ts が全 enqueue で `removeOnComplete:
// {age: 7d, count: 30}` を指定しているのに合わせて 30 件に揃える。BullMQ
// default の「無制限保持」が UDS 観測で実害 (inbox completed 15K+ 蓄積) を
// 出していたためここで切る (#1193)。`<queue>JobKeepCompleted: 0` で
// operator が明示解除可能 (= TS と挙動を意図的にずらしたい場合)。
const defaultKeepCompleted = 30

// defaultKeepCompletedAge / defaultKeepFailedAge は age-based retention の
// default。BullMQ TS upstream と同じ 7 日。count 上限と組み合わせて
// 「7 日 以内 かつ 最新 N 件」を保持する (BullMQ TS の object form 互換)。
const (
	defaultKeepCompletedAge = 7 * 24 * time.Hour
	defaultKeepFailedAge    = 7 * 24 * time.Hour
)

// applyClientPolicies copies enqueue-side defaults onto the queue.Client.
// Called once at server construction so EnqueueDeliver / EnqueueInbox can
// pre-pend driver options when callers don't override.
//
// Policy 設定は deliver / inbox / export / push / webhook の application
// queue 全てに対して適用する。Misskey TS upstream の QueueService.ts が
// 全 enqueue helper で `removeOnComplete: {age: 7d, count: 30}` を指定して
// いる規約に合わせるため (#1193)。**cron 発火の job はこの Policy を通らない**
// (maintenance と、プラグインの cron)。そちらは `queue.Scheduler.register` が
// upstream の system queue と同じ 7 日の期限を付ける (mkq v1.2.0 / mkq#109)。
// **maintenance には Policy が無い** ので、cron 以外で maintenance に積む job
// (`EnqueueCleanRemoteNotes` / `EnqueueReactionFlush`) には retention が付かない。
// ただし期限による刈り込みは、job が入る側の集合 (completed / failed) 全体が
// 対象なので、cron の job が**完了**するたびに completed 側のそれらも 7 日で
// 刈られる。failed 側が刈られるのは cron の job が**失敗**したときだけ。
func applyClientPolicies(c *queue.Client, cfg *config.Config) {
	c.SetPolicy(queue.QueueName, buildPolicy(cfg.DeliverJobMaxAttempts, defaultDeliverJobMaxAttempts, cfg.DeliverJobKeepFailed, cfg.DeliverJobKeepCompleted))
	c.SetPolicy(queue.InboxQueueName, buildPolicy(cfg.InboxJobMaxAttempts, defaultInboxJobMaxAttempts, cfg.InboxJobKeepFailed, cfg.InboxJobKeepCompleted))
	// export / push / webhook 用の YAML key は意図的に増やさない (TS と
	// 同じく一律の default を踏ませれば十分)。tuning 用 hook が必要に
	// なったら deliver/inbox と同じ pattern で *KeepCompleted / *KeepFailed
	// を追加すれば対応可能。
	c.SetPolicy(queue.ExportQueueName, defaultPolicy())
	c.SetPolicy(queue.PushQueueName, defaultPolicy())
	c.SetPolicy(queue.WebhookQueueName, defaultPolicy())
	// objectStorage も retention を効かせる。一括削除で completed が一気に
	// 積み上がるので、上限が無いと Redis を圧迫する (#2325)。
	c.SetPolicy(queue.ObjectStorageQueueName, defaultPolicy())
	// relationship も同様。移行時の一括 follow で completed が一気に積み上がる
	// ので retention が要る (#2403)。
	c.SetPolicy(queue.RelationshipQueueName, defaultPolicy())
	// **プラグインのキューは接頭辞で 1 回だけ登録する。** `plugin:<名前>` は
	// 運営者が入れたプラグインの数だけ増えるので、名前ごとに登録させると
	// 登録し忘れたものだけ retention が効かない。`PolicyFor` が接頭辞で
	// 引き当てる。
	c.SetPolicy(queue.PluginQueuePrefix, defaultPolicy())
}

// defaultPolicy returns the queue.Policy applied when no operator config
// override exists for the queue. すべての retention default (KeepFailed /
// KeepCompleted / KeepCompletedAge / KeepFailedAge) を含む。tuning hook
// が無い queue (export / push / webhook) で applyClientPolicies が一律
// 適用するために存在する。MaxAttempts は Policy.MaxAttempts を参照しない
// queue 群なので default 0 (= 適用しない) を渡す。
func defaultPolicy() queue.Policy {
	return buildPolicy(nil, 0, nil, nil)
}

// buildPolicy assembles a queue.Policy from optional config pointers,
// applying default values when the operator hasn't specified them.
//
// MaxAttempts は KeepFailed / KeepCompleted と同じ 3 状態設計:
//   - nil          → defaultMaxAttempts を適用 (Misskey TS の `?? N` 相当)
//   - 明示 0        → 0 (operator opt-out = リトライ無し)
//   - 明示 N (>0)   → N
//
// defaultMaxAttempts=0 を渡せば「未指定でも適用しない」(export/push/webhook 用)。
// mkq の attempts=0 はリトライ無しのため、deliver/inbox では必ず非ゼロの
// default を渡して落ちた配送先への retry-backoff を発火させる (#1411)。
//
// KeepFailed / KeepCompleted は nil で「default 適用」、明示 0 で「retention
// 無し (operator opt-out)」、明示 N で「N 件まで保持」の 3 状態を表す。
// Age 値 (KeepCompletedAge / KeepFailedAge) は operator が tuning する用途が
// 薄いため YAML key を増やさず TS と同じ 7d 固定で適用する。
func buildPolicy(maxAttempts *int, defaultMaxAttempts int, keepFailed *int, keepCompleted *int) queue.Policy {
	p := queue.Policy{}
	if maxAttempts != nil {
		// 明示値はそのまま尊重する (0 = operator opt-out)。
		p.MaxAttempts = *maxAttempts
	} else {
		p.MaxAttempts = defaultMaxAttempts
	}
	if keepFailed != nil {
		p.KeepFailed = *keepFailed
	} else {
		p.KeepFailed = defaultKeepFailed
	}
	if keepCompleted != nil {
		p.KeepCompleted = *keepCompleted
	} else {
		p.KeepCompleted = defaultKeepCompleted
	}
	p.KeepCompletedAge = defaultKeepCompletedAge
	p.KeepFailedAge = defaultKeepFailedAge
	return p
}
