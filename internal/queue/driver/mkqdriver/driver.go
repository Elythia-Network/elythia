package mkqdriver

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/elythia-network/mkq"
	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// QueueNames is the set of logical queues mkqdriver pre-defines at
// startup. New queue names introduced in mk-go must be added here so
// the corresponding mkq.Queue handle is created and a worker is
// spawned for it.
//
// **この一覧を変えたら `frontend/packages/misskey-js/src/consts.ts` の
// `queueTypes` も合わせること。**
// 管理画面のジョブキュータブは API 応答ではなくその定数から生成されるため、
// ずれると存在しない queue のタブが常時ゼロ表示になり、実在する queue が
// 画面から見えなくなる (#2323)。upstream との対応表は
// docs/divergence.md の「4-3. job queue の構成差分」。
var QueueNames = []string{
	"deliver",
	"inbox",
	"push",
	"export",
	"webhook",
	"maintenance",
	"objectStorage",
	"relationship",
}

// Config is the per-driver configuration the constructor consumes.
type Config struct {
	// Redis is forwarded to mkq.Config.Redis. Construct it via
	// BuildRedisOptions.
	Redis redis.UniversalOptions

	// KeyPrefix overrides BullMQ's "bull" default. Empty string keeps
	// the BullMQ default — recommended unless the same Redis hosts
	// multiple BullMQ deployments.
	KeyPrefix string

	// Concurrency is a fallback worker budget used only for queues that
	// have neither an entry in defaultQueueConcurrency nor an explicit
	// QueueConcurrency override (e.g. operator-defined custom queues).
	// Known queues use the per-queue hot-tuned defaults instead (see
	// defaultQueueConcurrency), so this rarely takes effect. Zero falls
	// back to 16.
	//
	// 旧挙動: total / len(queues) の均等割りだったが、それだと federation の
	// hot queue (inbox / deliver) が starve するため per-queue default に
	// 置き換えた。Concurrency は未知 queue 用の保険に降格。
	Concurrency int

	// QueueNames overrides the set of queues to pre-define. Nil/empty
	// keeps the package default (QueueNames).
	QueueNames []string

	// MaxMetricsDataPoints caps the number of one-minute buckets the
	// per-queue completed / failed metrics list keeps. The zero value
	// applies `defaultMaxMetricsDataPoints` (= MetricsTime.ONE_WEEK in
	// BullMQ TS, what Misskey TS upstream uses), so admin UIs see
	// drop-in compatible chart data when sharing a Redis with a TS
	// Misskey instance. Set to a negative value to opt out entirely;
	// no BullMQ metrics keys are written in that case (mkq's default
	// behaviour pre-v1.0.1).
	MaxMetricsDataPoints int

	// IdlePollInterval is the floor of how long an idle worker waits on the
	// marker key before re-checking. 0 applies mkq's own default.
	//
	// mkq v1.0.4 以降、空振りのたびに待ちが倍になり 30 秒で頭打ちになる。
	// つまりこれは初回の待ちで、アイドルが続けば実際の間隔はもっと長い。
	//
	// **ジョブ取得の遅さには繋がらない** (marker push で起きるため)。
	// 停止の速さにも影響しない (Stop が marker を突いて起こす)。
	IdlePollInterval time.Duration

	// QueueConcurrency overrides the per-queue worker pool size for the
	// named queue. Zero / missing entries fall back to the per-queue
	// hot-tuned defaults (defaultQueueConcurrency). Used by config keys
	// like `deliverJobConcurrency` / `inboxJobConcurrency` to tune the
	// hot queues independently from the rest (#495).
	QueueConcurrency map[string]int

	// QueueRateLimits caps each named queue at N tasks/sec via
	// mkq.WithRateLimit. Zero / missing entries disable the limiter
	// for that queue.
	QueueRateLimits map[string]int

	// StuckWorkerAfter is how long one job's handler may run before its
	// Worker stops counting towards the queue's worker budget and is
	// replaced (#2657). Zero applies the per-queue table
	// (defaultStuckAfterByQueue); a positive value overrides it for every
	// queue; a negative value disables quarantine (but not the supervisor —
	// HandlerDeadline keeps it running to apply the leak budget).
	//
	// 隔離した Worker と放棄した handler (#2658) は**同じ枠**を食い、
	// 枠に達するとそのキューの roster を 0 まで縮めて止める。
	//
	// 進行中の job は cancel されない。Worker を勘定から外して代わりを立てる
	// だけで、元の Worker は job を完走でき、完走したら pool に戻る。ただし
	// **代わりを立てる分だけ実効の並列度は上がる**
	// (上限は「到達した最大 worker 数 + max(設定値, 4)」。autoscale が無効なら
	// 設定値 + max(設定値, 4)、有効なら max(設定値, maxWorkers) + max(設定値, 4)。
	// peak は設定値から始まる単調非減少値なので maxWorkers を下回らない)。
	// 常時この閾値を超えるキューに短い値を当てると、その状態が定常化する。
	// 既定でバッチ系のキューを対象外にしているのはそのため。
	StuckWorkerAfter time.Duration

	// HandlerDeadline bounds how long the dispatcher waits for one job's
	// handler before giving up on it and moving on (#2658). Zero applies
	// defaultHandlerDeadline; a negative value disables the bound.
	//
	// **超過しても handler は止まらない** (Go では goroutine を殺せない)。
	// 止まるのは待つのをやめる側で、handler の goroutine はそのまま走り続ける。
	// job は失敗して retry に回るので、放棄した job と retry が同時に走りうる。
	// 1 job が分単位かかるのが正常な batch 系 task type は
	// handlerDeadlines で対象外にしてある。
	HandlerDeadline time.Duration

	// SuperviseInterval is how often every pool is re-checked for wedged
	// Workers. Zero applies defaultSuperviseInterval; a negative value
	// disables the periodic check (Resize still reconciles).
	SuperviseInterval time.Duration
}

// defaultMaxMetricsDataPoints mirrors BullMQ TS's
// `MetricsTime.ONE_WEEK = 10080` and is the value Misskey TS upstream
// applies via baseWorkerOptions. Keeping the same retention here
// preserves chart history across drop-in TS↔mk-go swaps that share
// the Redis instance.
const defaultMaxMetricsDataPoints = 10080

// Driver bundles the Client / Server / Inspector / Scheduler that
// share one *mkq.Client instance. New connects + script-loads against
// Redis; close the Driver to release the connection pool.
type Driver struct {
	client *mkq.Client
	cfg    Config
	queues map[string]*mkq.Queue[framedPayload]
	// queueNames holds the Define'd queue names in configured order. Used by
	// Inspector.Queues() so the admin dashboard lists exactly the queues
	// mk-go manages (drop-in 時に旧 Misskey TS が共有 registry に残した foreign
	// queue 名を含めない、#1393)。
	queueNames []string

	// rdb is a side-channel redis client used by the Inspector for
	// ad-hoc reads that mkq's public API does not expose (currently
	// the per-queue `repeat` ZSET — see Inspector.GetQueueInfo). mkq's
	// embedded *redis.UniversalClient is unexported, so duplicating
	// the connection here keeps mkqdriver decoupled from mkq internals.
	// keyPrefix mirrors mkq.Config.KeyPrefix with the BullMQ default
	// ("bull") substituted for empty strings, so callers can build keys
	// without re-checking config.
	rdb       redis.UniversalClient
	keyPrefix string

	// Sub-components are constructed lazily on first access.
	mu      sync.Mutex
	dClient *Client
	dServer *Server
	dIns    *Inspector
	dSched  *Scheduler
	closed  bool
}

// New connects to Redis, preloads mkq's vendored Lua scripts, and
// pre-defines the configured queues. The returned Driver owns the
// underlying *mkq.Client and must be Close'd to release resources.
//
// The supplied context bounds the connection setup phase only; once
// New returns, the connection is shared by the driver's sub-services
// for the rest of their lifetime.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	names := cfg.QueueNames
	if len(names) == 0 {
		names = QueueNames
	}

	// worker client の Redis pool を per-queue concurrency 合計で自動サイジング
	// する。mkq は worker 毎に BZPopMin で blocking 接続を 1 つ保持するため、
	// operator が poolSize を明示していない (= 0) ままだと go-redis default
	// (10×GOMAXPROCS) が小コア機で worker 総数を下回り、接続待ちで dispatch が
	// 詰まる (worker.go が "pool < concurrency+8" を warn する)。明示設定があれば
	// 尊重する。side-channel rdb は低頻度なので default pool のままにする。
	workerRedis := cfg.Redis
	if workerRedis.PoolSize == 0 {
		workerRedis.PoolSize = workerPoolSize(names, cfg.QueueConcurrency, cfg.StuckWorkerAfter)
	}

	mkqCfg := mkq.Config{Redis: workerRedis, KeyPrefix: cfg.KeyPrefix}
	client, err := mkq.NewClient(ctx, mkqCfg)
	if err != nil {
		return nil, fmt.Errorf("mkqdriver: connect: %w", err)
	}

	// side-channel redis client (see Driver.rdb doc). PING is verified
	// implicitly via mkq.NewClient above against the same Addrs, so a
	// second probe here is redundant — let the first ZCARD surface
	// connectivity errors instead.
	rdb := redis.NewUniversalClient(&cfg.Redis)
	keyPrefix := cfg.KeyPrefix
	if keyPrefix == "" {
		keyPrefix = defaultKeyPrefix
	}
	queues := make(map[string]*mkq.Queue[framedPayload], len(names))
	for _, n := range names {
		queues[n] = mkq.Define[framedPayload](client, n)
	}

	return &Driver{
		client:     client,
		cfg:        cfg,
		queues:     queues,
		queueNames: names,
		rdb:        rdb,
		keyPrefix:  keyPrefix,
	}, nil
}

// defaultKeyPrefix mirrors mkq's BullMQ-compatible default. Duplicated
// here (rather than imported from mkq) because mkq exposes the constant
// only via Config field semantics, not as a public symbol.
const defaultKeyPrefix = "bull"

// repeatKey returns the BullMQ ZSET key that holds the "next-fire"
// schedule for repeat jobs on the named queue. Layout: `{prefix}:{queue}:repeat`.
func (d *Driver) repeatKey(queue string) string {
	return d.keyPrefix + ":" + queue + ":repeat"
}

// waitKey returns the BullMQ `wait` LIST key for the named queue.
// Inspector.DispatchableCount reads its length directly instead of going
// through the aggregate Counts path.
func (d *Driver) waitKey(queue string) string {
	return d.keyPrefix + ":" + queue + ":wait"
}

// metaKey returns the BullMQ `meta` HASH key for the named queue. The
// `paused` field on it is the pause flag (mkq's Queue.IsPaused reads the
// same field).
func (d *Driver) metaKey(queue string) string {
	return d.keyPrefix + ":" + queue + ":meta"
}

// queueFor returns the pre-defined queue for the given name, or nil.
func (d *Driver) queueFor(name string) *mkq.Queue[framedPayload] {
	return d.queues[name]
}

// Client returns the lazily-constructed driver.Client.
func (d *Driver) Client() driver.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dClient == nil {
		d.dClient = &Client{driver: d}
	}
	return d.dClient
}

// defaultQueueConcurrency maps each logical queue to its default worker pool
// size when the operator hasn't set an explicit <queue>JobConcurrency.
//
// mkq は per-queue 専用 worker pool を持つ (BullMQ と同じ構造)。total budget を
// queue 数で均等割りすると federation の hot queue (inbox / deliver) が starve
// する (例: total 16 / 6 queue = 2 worker)。queue-bench で inbox=2 は inbound
// drain が明確に劣ることを確認したため、Misskey TS QueueProcessorService の
// per-queue default を基準に hot queue を厚く、低頻度 queue を薄く配分する。
//
// worker 数 ≒ Redis 接続数 (mkq は worker 毎に BZPopMin で blocking 接続を 1 つ
// 保持し、worker.go が "recommended pool = concurrency + 8" を warn する) なので、
// 接続数を抑えるため deliver は upstream の 128 ではなく 16 に留める (outbound
// bench で 2 worker でも BullMQ の 6.6x スループットがあり Go 側は余力十分)。
// 合計 16+16+4+4+4+4+2+2 = 52 worker。worker Redis pool は New() が
// workerPoolSize() で自動的にこの合計 + poolHeadroom に合わせる。operator は
// <queue>JobConcurrency でいつでも上書きできる (pool も追従する)。
var defaultQueueConcurrency = map[string]int{
	"inbox":       16, // = Misskey inboxJobConcurrency ?? 16
	"deliver":     16, // upstream は 128 だが Go の per-worker 効率を踏まえ抑制
	"webhook":     4,
	"push":        4,
	"export":      2,
	"maintenance": 2,
	// upstream は 16。実体削除は S3 への I/O 待ちが主で 1 worker あたりの
	// 効率が良く、bulk cleanup は job 1 本が内部でバッチ削除を回すため
	// (= job 数で並列度を稼ぐ設計ではない)、deliver と同じ理由で抑える。
	"objectStorage": 4,
	// upstream は relationshipJobConcurrency ?? 16。relationship job は DB
	// bound (following 行 + カウンタ + stream publish) で、外向き HTTP は
	// deliver へ再 enqueue されるだけ。db.MaxOpenConns の既定 25 を HTTP 経路と
	// 共有するため、16 を DB bound な queue に割くと Web 側のテールレイテンシに
	// 響く。deliver からの隔離 (#2403 の目的) は 4 で達成できる。足りなければ
	// operator が relationshipJobConcurrency で上げられる。
	// deliver を 128→16 に抑えているのは Go の per-worker 効率が理由で、
	// こちらとは根拠が違う。
	"relationship": 4,
}

// unknownQueueConcurrency is applied to any queue absent from
// defaultQueueConcurrency (e.g. operator-defined custom queues).
const unknownQueueConcurrency = 2

// poolHeadroom is the spare Redis connection budget added on top of the
// per-queue worker total so enqueue / Inspector-via-Client ops are not
// starved while every BZPopMin worker holds a connection. Matches mkq's
// own per-worker "concurrency + 8" recommendation applied once to the
// shared pool.
const poolHeadroom = 8

// workerPoolSize sizes the worker Redis connection pool for the resolved
// per-queue concurrency. mkq holds one blocking BZPopMin connection per
// worker, so the pool must cover every worker simultaneously or dispatch
// stalls on connection acquisition. The headroom / floor policy lives in
// poolSizeForWorkers; the floor is go-redis' own default (10 × GOMAXPROCS).
//
// stuckAfter is Config.StuckWorkerAfter and decides which queues track
// worker liveness. **追跡するキューは許容ぶんの接続を先に確保しておく。**
// 隔離された Worker は awaitMarker に戻って BZPopMin 接続を握り続けるので、
// 代わりを立てた分だけ実接続が増える。あとから足せない (pool サイズは
// 起動時に決まる) ので、上限ぶんをここで見込む。go-redis の PoolSize は
// 上限であって事前確保ではない (MinIdleConns=0) ため、広げても普段の
// コストは無い。
func workerPoolSize(queues []string, override map[string]int, stuckAfter time.Duration) int {
	conc := resolveQueueConcurrency(queues, override)
	sum := 0
	for name, c := range conc {
		sum += c
		if stuckAfterForQueue(name, stuckAfter) > 0 {
			sum += quarantineHeadroomFor(c)
		}
	}
	return poolSizeForWorkers(sum, 10*runtime.GOMAXPROCS(0))
}

// stuckAfterForQueue resolves the effective liveness threshold for qname
// from the driver-level setting, mirroring Server.resolvedStuckAfter.
func stuckAfterForQueue(qname string, configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	if configured < 0 {
		return 0
	}
	return defaultStuckAfterForQueue(qname)
}

// poolSizeForWorkers returns the Redis pool size for workerSum BZPopMin
// workers: workerSum + poolHeadroom, floored at floor.
//
// floor is go-redis' own default (10 × GOMAXPROCS) so auto-sizing never
// shrinks the pool below what an unset PoolSize would have produced. That
// preserves enqueue-burst headroom on multi-core hosts and leaves room for
// jobQueueAutoScale to grow workers past the static defaults (auto-scale
// resizes workers but not the connection pool). Heavy auto-scale deployments
// should still set redisForJobQueue.poolSize explicitly to cover maxWorkers.
// Connections are created lazily (MinIdleConns=0) so a higher ceiling costs
// nothing until load demands it.
//
// floor is passed in (rather than read from runtime here) so the headroom +
// floor policy is unit-testable independently of the host core count.
func poolSizeForWorkers(workerSum, floor int) int {
	return max(workerSum+poolHeadroom, floor)
}

// queueDefaultConcurrency returns the hot-tuned default pool size for a queue.
func queueDefaultConcurrency(name string) int {
	if c, ok := defaultQueueConcurrency[name]; ok {
		return c
	}
	return unknownQueueConcurrency
}

// resolveQueueConcurrency builds the per-queue worker pool sizes from the
// hot-tuned defaults, overlaid with explicit <queue>JobConcurrency overrides
// (override > 0 wins). The result covers every registered queue so the Server
// never falls back to Concurrency for a known queue.
func resolveQueueConcurrency(queues []string, override map[string]int) map[string]int {
	out := make(map[string]int, len(queues))
	for _, name := range queues {
		out[name] = queueDefaultConcurrency(name)
	}
	for name, v := range override {
		if v > 0 {
			out[name] = v
		}
	}
	return out
}

// ResolveQueueConcurrency exposes the effective per-queue worker counts for
// diagnostics (#2469).
//
// **算出は resolveQueueConcurrency をそのまま使う。** 実効値を出すために別実装を
// 起こすと、表示と実際が食い違ったときにどちらが正しいか分からなくなる。
func ResolveQueueConcurrency(queues []string, override map[string]int) map[string]int {
	return resolveQueueConcurrency(queues, override)
}

// StuckWorkerThreshold exposes the effective per-queue stuck-worker
// threshold for diagnostics (#2657). stuckAfter is
// Config.StuckWorkerAfter; pass 0 for the built-in per-queue defaults.
// A zero result means the queue is not tracked.
func StuckWorkerThreshold(qname string, stuckAfter time.Duration) time.Duration {
	return stuckAfterForQueue(qname, stuckAfter)
}

// HandlerDeadlineFor exposes the effective per-task-type handler deadline
// for diagnostics (#2658). configured is Config.HandlerDeadline; pass 0 for
// the built-in default. A zero result means the task type is exempt.
func HandlerDeadlineFor(taskType string, configured time.Duration) time.Duration {
	return handlerDeadlineFor(taskType, configured)
}

// QuarantineHeadroomFor exposes how many workers a pool of the given size
// may hold outside the roster before it stops replacing them (#2657), so
// diagnostics can state the real worker ceiling.
func QuarantineHeadroomFor(desired int) int {
	return quarantineHeadroomFor(desired)
}

// WorkerPoolSize exposes the auto-sized Redis pool for diagnostics (#2469).
// stuckAfter is Config.StuckWorkerAfter; pass 0 for the built-in per-queue
// defaults.
func WorkerPoolSize(queues []string, override map[string]int, stuckAfter time.Duration) int {
	return workerPoolSize(queues, override, stuckAfter)
}

// Server returns the lazily-constructed driver.Server.
//
// Per-queue worker pool sizes come from defaultQueueConcurrency (hot-tuned
// toward inbox / deliver) overlaid with explicit <queue>JobConcurrency
// overrides. cfg.Concurrency only seeds the fallback used for queues without
// a per-queue value (rare; all registered queues have a default).
func (d *Driver) Server() driver.Server {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dServer == nil {
		total := d.cfg.Concurrency
		if total <= 0 {
			total = 16
		}
		queues := len(d.queues)
		if queues < 1 {
			queues = 1
		}
		// fallback (未知 queue 用): 旧来の均等割りを保険として残す。
		perQueue := total / queues
		if perQueue < 1 {
			perQueue = 1
		}
		queueNames := make([]string, 0, len(d.queues))
		for name := range d.queues {
			queueNames = append(queueNames, name)
		}
		metricsPoints := d.cfg.MaxMetricsDataPoints
		if metricsPoints == 0 {
			metricsPoints = defaultMaxMetricsDataPoints
		}
		d.dServer = &Server{
			driver:             d,
			concurrency:        perQueue,
			maxMetricsPoints:   metricsPoints,
			idlePollInterval:   d.cfg.IdlePollInterval,
			perQueueConcurrent: resolveQueueConcurrency(queueNames, d.cfg.QueueConcurrency),
			perQueueRate:       d.cfg.QueueRateLimits,
			stuckAfter:         d.cfg.StuckWorkerAfter,
			handlerDeadline:    d.cfg.HandlerDeadline,
			superviseInterval:  d.cfg.SuperviseInterval,
			handlers:           make(map[string]driver.HandlerFunc),
		}
	}
	return d.dServer
}

// Inspector returns the lazily-constructed driver.Inspector.
func (d *Driver) Inspector() driver.Inspector {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dIns == nil {
		d.dIns = &Inspector{driver: d}
	}
	return d.dIns
}

// Scheduler returns the lazily-constructed driver.Scheduler.
func (d *Driver) Scheduler() driver.Scheduler {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dSched == nil {
		d.dSched = &Scheduler{driver: d}
	}
	return d.dSched
}

// Resize delegates to the Server's WorkerPool layer to grow / shrink
// the worker count for qname at runtime.
//
// Before Server() has been called there is no Server object at all, so
// this returns driver.ErrResizeNotSupported. **After Server() but before
// Server.Start the pool map is still empty**, so the call reaches
// Server.Resize and comes back as `unknown queue %q` instead — callers
// that want a working Resize must wait for Server.Start, and must not
// use ErrResizeNotSupported to detect "not started yet" (#2985).
//
// Concrete implementation lives in server.go alongside the pool itself
// (server.go's locking model is the source of truth for pool state).
func (d *Driver) Resize(qname string, n int) error {
	d.mu.Lock()
	srv := d.dServer
	d.mu.Unlock()
	if srv == nil {
		return driver.ErrResizeNotSupported
	}
	return srv.Resize(qname, n)
}

// WorkerCount returns the number of Workers for qname that can currently
// take work. Reads the live pool state via Server.workerCount so it
// reflects both runtime Resize operations (#1124) and Workers held
// outside the pool because their handler overran the stuck threshold
// (#2657). Before Server() has been called or before Server.Start
// completes, the underlying pool map is nil and we return 0.
//
// Lock ordering: d.mu (this function) → s.mu (workerCount) → pool.mu
// (workerCount inner). All acquisitions are short and the nested order
// is consistent across Resize / WorkerCount, so no deadlock risk.
func (d *Driver) WorkerCount(qname string) int {
	d.mu.Lock()
	srv := d.dServer
	d.mu.Unlock()
	if srv == nil {
		return 0
	}
	return srv.workerCount(qname)
}

// AbandonedHandlerCount reports how many handlers for qname the dispatcher
// gave up on and that have still not returned, plus the cumulative total
// (#2658). Returns zeroes before Server.Start.
func (d *Driver) AbandonedHandlerCount(qname string) (current int, total uint64) {
	d.mu.Lock()
	srv := d.dServer
	d.mu.Unlock()
	if srv == nil {
		return 0, 0
	}
	return srv.AbandonedHandlerCount(qname)
}

// QuarantinedWorkerCount reports how many Workers for qname are held
// outside the pool because their handler overran the stuck threshold
// (#2657). Returns 0 before Server.Start.
//
// Backs the `mk_job_workers_quarantined` gauge. **0 でない状態が続いて
// いたら handler がどこかでブロックしている。** 本番でこれが可視化されて
// いなかったため、inbox の詰まりに 1 日以上気付けなかった。
func (d *Driver) QuarantinedWorkerCount(qname string) int {
	d.mu.Lock()
	srv := d.dServer
	d.mu.Unlock()
	if srv == nil {
		return 0
	}
	return srv.QuarantinedWorkerCount(qname)
}

// Close stops the worker (if started) and releases the underlying
// *mkq.Client. Idempotent: subsequent calls are no-ops, so layered
// shutdown hooks can double-close safely.
func (d *Driver) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	srv := d.dServer
	d.mu.Unlock()

	if srv != nil {
		srv.Shutdown()
	}
	// closed フラグを true にしてから cleanup を始めているため (上の
	// idempotent ガード)、途中で early return すると残りのリソースが
	// 永久リークする。client / rdb の両方を必ず Close し、エラーは
	// errors.Join で集約して返す。
	var clientErr, rdbErr error
	if err := d.client.Close(); err != nil {
		clientErr = fmt.Errorf("mkqdriver: close client: %w", err)
	}
	if d.rdb != nil {
		// 副 Redis client は GetQueueInfo の ZCARD 専用。失敗しても
		// 主要シャットダウン経路を blocking させる必要はないが、リーク
		// 防止のため errors.Join 経由で呼び出し側に返しておく。
		if err := d.rdb.Close(); err != nil {
			rdbErr = fmt.Errorf("mkqdriver: close rdb: %w", err)
		}
	}
	return errors.Join(clientErr, rdbErr)
}
