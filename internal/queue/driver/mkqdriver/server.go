package mkqdriver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shiroha-a/mkq"

	"github.com/shiroha-a/mk/internal/queue/driver"
)

// Server runs a per-queue worker pool, dispatching jobs to handlers
// registered via Handle(taskType, fn). The dispatch keys off the
// framedPayload.Type field rather than mkq's BullMQ Job.name — that
// field is not overridable by mkq's schedule API, so framing the
// payload is the only consistent way to recover the task type for
// both ad-hoc and scheduled jobs.
//
// Pool architecture (#1124, per ADR §5.1): for each queue we hold a
// `workerPool` containing N `mkq.Worker` instances, each constructed
// with `WithConcurrency(1)`. Adding workers = scale-up; stopping
// workers = scale-down. This sidesteps mkq's lack of a runtime Resize
// API.
//
// Stop semantics: mkq.Worker.Stop **cancels** the in-flight job's
// context (= ctx.Done fires for the handler) and waits for the worker
// goroutine to exit cleanly. It does NOT wait for the in-flight job to
// complete naturally. Cancelled jobs are not lost: BullMQ re-locks
// them on next pickup so a different Worker (in the same or another
// pool) will retry. This trade-off prevents scale-down from blocking
// for minutes on slow remote inboxes.
//
// Locking model:
//   - s.mu protects s.started, s.handlers, and the s.pools map (not
//     individual pool state).
//   - Each pool owns its own pool.mu for the workers slice. Resize
//     takes pool.mu and HOLDS IT across Worker.Stop calls. Stop is
//     normally fast (ms-order cancellation, not job completion) so this
//     is acceptable; different queues use independent pool.mu so
//     cross-queue Resize is parallel. **ms で終わらない場合がある**:
//     ctx を見ない handler を抱えた Worker への Stop は stopWorkerTimeout
//     (30 秒) まで返らない。閾値未満の busy Worker を縮小で選んだとき、
//     および Resize(q, 0) / Shutdown が quarantine (= 戻ってこないと
//     分かっている Worker) を畳むときに起きる。その間そのキューの
//     WorkerCount は待たされる (= Prometheus scrape も)。
//   - workerPool.shutdown flag (set under pool.mu in shutdownLocked)
//     guards against the Resize-after-Shutdown race: once Shutdown
//     marks a pool, subsequent Resize calls on that captured pool
//     return ErrResizeAfterShutdown instead of spawning leaked
//     Workers.
//
// After Start() returns, the dispatch fast-path runs **without** taking
// s.mu — the handler snapshot is captured at Start time and frozen for
// the lifetime of the server. Handle calls after Start panic, so the
// freeze is maintained.
type Server struct {
	driver      *Driver
	concurrency int
	// maxMetricsPoints は mkq.WithJobMetrics の引数。0 だと metrics
	// 書き込みを無効化、正値で BullMQ-spec の metrics LIST に書き込み
	// が走る。
	maxMetricsPoints int
	// idlePollInterval は mkq.WithIdlePollInterval の引数。0 なら
	// defaultIdlePollInterval を使う。
	idlePollInterval time.Duration

	// perQueueConcurrent / perQueueRate are runtime tuning overrides
	// keyed by queue name (#495). Missing / zero entries fall back to
	// the shared per-queue defaults (`concurrency` for the worker pool,
	// no rate limiter respectively).
	perQueueConcurrent map[string]int
	perQueueRate       map[string]int

	// observer is the optional per-job timing hook (#2277). nil のときは
	// dispatch path で clock も触らない。
	observer driver.Observer

	// stuckAfter / superviseInterval configure the wedged-worker
	// supervisor (#2657). Zero applies the package defaults;
	// stuckAfter < 0 disables quarantine. **supervisor 自体は止まらない** —
	// handlerDeadline が有効なら漏れの予算を適用するために起動する
	// (startSupervisor)。
	stuckAfter        time.Duration
	superviseInterval time.Duration
	// handlerDeadline bounds how long the dispatcher waits for one job's
	// handler (#2658). Zero applies defaultHandlerDeadline; a negative
	// value disables the bound. Task types in handlerDeadlines override it.
	handlerDeadline time.Duration
	// abandoned counts handlers the dispatcher gave up on, per queue.
	abandoned abandonCounters
	// nanos is the monotonic clock the pools use for liveness. nil means
	// monotonicNanos.
	nanos func() int64

	mu       sync.Mutex
	handlers map[string]driver.HandlerFunc
	pools    map[string]*workerPool
	started  bool
	// superviseCancel stops the supervisor goroutine; superviseDone is
	// closed by that goroutine on exit. Both nil while not running.
	superviseCancel context.CancelFunc
	superviseDone   chan struct{}
}

// defaultStuckAfter is the threshold applied to queues whose jobs have a
// bounded duration.
//
// **これは「健全な job の上限」ではなく「これを超えたら勘定に入れない」。**
// safehttp が切るのは 1 リクエスト 10 秒であって 1 job ではない。inbox の
// handler は resolver 経由で最大 resolveRecursionLimit (256) 段の逐次 fetch を
// 回しうるので、理論上の worst case は 40 分を超える。job 単位の期限は
// #2658 で別途入れる。
//
// 30 分にしてあるのは、この機構の狙いが**恒久的に戻ってこない Worker の
// 回収**だからで、短くしても得るものが少ないため。#2657 の本番障害は 1 日以上
// 戻ってこなかったので、30 分で気付けば十分に速い。逆に短くすると、たまたま
// 遅い job のたびに Worker を差し替えることになり、実効の並列度が設定値を
// 超えた状態が定常化する (allowedRosterLocked のコメント参照)。
const defaultStuckAfter = 30 * time.Minute

// defaultStuckAfterByQueue maps a queue to its stuck-worker threshold.
// A zero entry (or an absent one) disables liveness tracking for that
// queue.
//
// **バッチ系を外してあるのは、閾値超過が正常だから。** 隔離は job を殺さない
// が、代わりの Worker を立てるので、常時閾値を超えるキューでは実効の並列度が
// 設定値を超え続け、Redis 接続も余分に食う (allowedRosterLocked 参照)。
// 具体的には:
//
//   - objectStorage の cleanRemoteFiles は 1 job で最大 10000 バッチ x 500ms の
//     ページングを回す (それだけで 83 分)
//   - export は 1 job でアカウント全体をページングして書き出す
//   - maintenance は cron 由来の後始末をまとめて回す
//
// これらは「戻ってこない」のではなく「長い」ので、この機構の対象ではない。
// 代わりに job 側へ期限を付ける話が #2658。
//
// **追跡対象にも長い job は残る。** deleteAccount は maintenance ではなく
// deliver に載っており (queue.go の EnqueueDeleteAccount)、100 件ごとに 250ms
// 空けるので 70 万ノート規模で 30 分を超える。inbox も resolver の再帰で
// 理論上 40 分を超えうる。どちらも隔離されるだけで job は cancel されないため
// 実害は「一時的に Worker が 1 本増える」に留まるが、閾値を短くすると
// その状態が定常化する。
//
// 既定一覧に無いキューも追跡しない。job の長さを想定できない
// ものに閾値を当てても誤検知しか生まない。必要なら
// queueStuckWorkerSeconds で全キューに一律の値を入れられる。
var defaultStuckAfterByQueue = map[string]time.Duration{
	"inbox":        defaultStuckAfter,
	"deliver":      defaultStuckAfter,
	"relationship": defaultStuckAfter,
	"push":         defaultStuckAfter,
	"webhook":      defaultStuckAfter,
}

// defaultStuckAfterForQueue returns the built-in threshold for qname.
func defaultStuckAfterForQueue(qname string) time.Duration {
	return defaultStuckAfterByQueue[qname]
}

// defaultSuperviseInterval is how often the supervisor re-checks every
// pool. 30 秒あたりで見れば十分で、Redis も一切触らない (handler 出入りの
// atomic を読むだけ) ので間隔を詰める理由が無い。
const defaultSuperviseInterval = 30 * time.Second

// workerPool owns the mkq.Worker instances for one queue. Each Worker
// is started with WithConcurrency(1), so |workers| equals the effective
// concurrency for the queue. Resize mutates the slice while holding mu
// and issues Worker.Stop calls under the same mu (Stop is ms-order
// cancellation, not job completion, so holding the lock is acceptable).
//
// shutdown is set true under mu by shutdownLocked. Once set, all
// subsequent resizeLocked calls return ErrResizeAfterShutdown so a
// caller that captured the pool pointer before Server.Shutdown ran
// cannot spawn leaked Workers on a pool that is no longer owned by
// any Server.
//
// Roster と quarantine の 2 本立てにしている理由 (#2657): 1 Worker =
// 1 dispatcher goroutine なので、handler から戻らなくなった Worker は
// 二度と awaitMarker に到達せず、キューの capacity を無言で 1 本削る。
// 本番で inbox の 4 本すべてがこの状態になり、autoscale は帳簿上の
// len(workers) を見て「4 本健全」と誤認し続けた。詰まった Worker は
// roster から quarantine へ退避して勘定から外し、代わりを spawn する。
type workerPool struct {
	queue   *mkq.Queue[framedPayload]
	handler mkq.Handler[framedPayload]
	// optsBase is the WorkerOption slice common to every Worker in this
	// pool (rate limit, job metrics, etc.). WithConcurrency(1) is added
	// per-worker in Resize, not here.
	optsBase []mkq.WorkerOption
	// name is the queue name, used for log attribution only.
	name string
	// stuckAfter is how long a handler may run before its Worker stops
	// counting towards the queue's worker budget. <= 0 disables liveness
	// tracking for this pool.
	stuckAfter time.Duration
	// nanos is the monotonic clock, injectable for tests. Never nil after
	// Start.
	nanos func() int64

	mu sync.Mutex
	// workers is the live roster: the Workers that count towards
	// WorkerCount and that Resize grows / shrinks.
	workers []*workerHandle
	// quarantine holds Workers evicted from the roster because their
	// handler overran stuckAfter. They are NOT stopped on eviction —
	// see quarantineStuckLocked for why — and are dropped once they
	// return to idle.
	quarantine []*workerHandle
	// desired is the roster size last requested via Resize / Start. The
	// supervisor restores the roster to this size after evictions.
	desired int
	// baseConcurrency is the queue's configured worker count, fixed at
	// Start. The quarantine headroom is sized from this.
	baseConcurrency int
	// peakDesired is the high-water mark of desired. The quarantine
	// allowance is anchored to it — see allowedRosterLocked.
	peakDesired int
	// abandoned is the Server-owned counter of handlers the dispatcher gave
	// up on for this queue (#2658). Those goroutines leak just like a
	// quarantined Worker's, so they count against the same budget.
	abandoned *abandonCounters
	// seq numbers Workers for log attribution.
	seq uint64
	// capLogged / lastCapLogNanos throttle the over-budget Error log. The
	// bool is separate because 0 is a legal monotonic reading, so it cannot
	// double as "never logged".
	capLogged       bool
	lastCapLogNanos int64
	shutdown        bool
}

// reportCapEvery throttles the Error log emitted while the pool cannot
// reach its configured worker count.
const reportCapEvery = 5 * time.Minute

// quarantineHeadroom is the floor for how many quarantined Workers a pool
// tolerates beyond its configured count. See allowedRosterLocked; the
// Redis connection budget for this allowance is reserved up front in
// workerPoolSize.
const quarantineHeadroom = 4

// workerHandle pairs one mkq.Worker with the liveness bookkeeping the
// pool needs to tell a live dispatcher from a wedged one.
//
// mkq exposes no "is this dispatcher running" API (checked against
// v1.0.6), but the handler is ours: wrapping it per Worker is enough to
// see whether a Worker is inside a job and for how long.
//
// **その計測で足りることは本番で確認した** (#2657)。`bull:inbox:active` に
// inbox の job が 4 件、掴まれたまま残っていた (08-22 の観測で 18-28 時間、
// 08-23 の再測で 31-41 時間)。決め手は job HASH の `stc` (stalledCount) が
// **未設定**だったこと — 一度も stalled-check に回収されていない、つまり
// lock が途切れず延長され続けたということ (同じプロセスで stalled-check が
// 現に他の job を回収していたので、checker が止まっていた可能性は無い)。mkq の heartbeat goroutine は
// `processJob` の寿命に紐づくので、これは `processJob` から戻っていない
// = handler の中にいる、を意味する。mkq 内部でも Redis でもない。
//
// (lock の PTTL でも同じ推論はできるが、heartbeat が lockDuration/2 ごとに
// 張り直すので PTTL は 15-30 秒を巡回するだけで、スナップショット 1 点では
// 継続を示せない。`stc` の方が直接的。)
type workerHandle struct {
	w *mkq.Worker
	// busySinceNanos holds the monotonic reading of the moment the
	// currently running handler was entered, or 0 while the Worker is
	// idle. Workers are created with WithConcurrency(1) so at most one
	// job is in flight per handle and a single word is exact.
	//
	// 壁時計ではなく単調時間を入れる。NTP が前方に飛ぶと壁時計差では
	// 全 Worker が同時に閾値超過に見える。
	busySinceNanos atomic.Int64
	// completed counts handler returns. Monotonic, and the only reliable
	// way to prove a quarantined Worker is alive: mkq chains jobs through
	// the prefetch path (moveToFinished が次の job を返し、dispatchLoop が
	// awaitMarker を経ずにそのまま handler へ渡す) ので、忙しいキューでは
	// idle の窓がマイクロ秒しかなく、supervisor の 30 秒周期ではまず
	// 捉えられない。「今 idle か」ではなく「隔離後に 1 件でも完了したか」で
	// 見る。
	completed atomic.Uint64

	// 以下は pool.mu の下でのみ読み書きする。
	// quarantinedAt is the monotonic reading of the eviction.
	quarantinedAt int64
	// completedAtQuarantine snapshots completed at eviction time.
	completedAtQuarantine uint64
	// protected marks a handle just returned from quarantine. It is not
	// eligible as a scale-down victim while it holds a job, so proving a
	// Worker alive never costs that Worker its next job.
	protected bool
	// completedAtReinstate snapshots completed when protection was granted.
	completedAtReinstate uint64
	// seq is a per-pool monotonic id for log attribution.
	seq uint64
}

// busyFor reports how long the handle has been inside its handler, or 0
// when it is idle.
func (h *workerHandle) busyFor(nowNanos int64) time.Duration {
	v := h.busySinceNanos.Load()
	if v == 0 {
		return 0
	}
	d := time.Duration(nowNanos - v)
	if d < 0 {
		return 0
	}
	return d
}

// idle reports whether the handle is currently between jobs.
//
// **「job を持っていない」ではない。** handler から戻った直後の dispatcher は
// まだ finalise を回しており、次の job を moveToActive で掴んでから handler に
// 入り直す。その窓でも idle と読める。停止判断をこれ単独で下さないこと。
func (h *workerHandle) idle() bool { return h.busySinceNanos.Load() == 0 }

// provenLive reports whether the handle completed at least one job since
// it was quarantined, which proves the dispatcher is running.
func (h *workerHandle) provenLive() bool {
	return h.completed.Load() != h.completedAtQuarantine
}

// processStart anchors the monotonic clock used for liveness. time.Since
// reads the monotonic clock, so elapsed values are immune to wall-clock
// steps.
var processStart = time.Now()

// monotonicNanos is the default nanos source for a pool.
func monotonicNanos() int64 { return int64(time.Since(processStart)) }

// ErrResizeAfterShutdown is returned by Resize when the pool's owning
// Server has already shut down. Callers (auto-scale controller) should
// treat this as "stop trying to scale, the server is gone".
var ErrResizeAfterShutdown = errors.New("mkqdriver: pool already shut down")

// Handle registers a handler for the given task type. Must be called
// before Start; calls after Start panic. The dispatch fast-path
// captures a snapshot of the handlers map at Start, so the registry
// is frozen for the worker's lifetime.
func (s *Server) Handle(taskType string, h driver.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		panic("mkqdriver: Handle called after Start")
	}
	s.handlers[taskType] = h
}

// Start initialises one workerPool per pre-defined queue and fills it
// with the configured initial concurrency. Returns once all per-queue
// pools are populated.
//
// Failure mode: if any per-queue spawn fails partway, every Worker
// started so far is stopped before bubbling the error up. Stop is
// invoked **without** holding s.mu so the in-flight job draining is not
// blocked.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("mkqdriver: Start called twice")
	}
	handlersSnapshot := maps.Clone(s.handlers)
	if handlersSnapshot == nil {
		handlersSnapshot = map[string]driver.HandlerFunc{}
	}
	s.started = true
	s.mu.Unlock()

	// Iterate queue names in deterministic order so partial-startup
	// error messages are reproducible across runs (Go map iteration
	// is randomized).
	names := make([]string, 0, len(s.driver.queues))
	for n := range s.driver.queues {
		names = append(names, n)
	}
	sort.Strings(names)

	pools := make(map[string]*workerPool, len(names))
	for _, name := range names {
		concurrency := s.concurrency
		if v, ok := s.perQueueConcurrent[name]; ok && v > 0 {
			concurrency = v
		}
		var optsBase []mkq.WorkerOption
		if rl, ok := s.perQueueRate[name]; ok && rl > 0 {
			// **リミッタは queue 単位で効く。** pool 内に N Worker いても
			// 合計は rl のまま。実体は BullMQ 互換の `bull:<queue>:limiter`
			// という queue ごとに 1 本の Redis キーで、全 Worker がそれを
			// INCR するため。
			//
			// ここには以前「per-Worker なので合計 rate は rl × N」という
			// コメントと、その値を出す slog.Warn があったが**誤り**だった
			// (#2640)。operator にその値を信じさせると、狙ったレートを
			// worker 数で割って設定してしまい配送が 1/N に絞られる。
			// `TestServer_RateLimit_IsPerQueueNotPerWorker` が worker 1 と
			// 16 で実効レートが変わらないことを固定している。
			optsBase = append(optsBase, mkq.WithRateLimit(rl, time.Second))
		}
		if s.maxMetricsPoints > 0 {
			// BullMQ-compatible per-queue metrics opt-in。
			optsBase = append(optsBase, mkq.WithJobMetrics(s.maxMetricsPoints))
		}
		if s.idlePollInterval > 0 {
			// アイドル時の空振りポーリングを間引きたい運用者向けの opt-in。
			//
			// **ジョブ取得は遅くならない。** worker は BZPOPMIN で marker key を
			// 待っており、ジョブが積まれた時点で Lua 側が marker を push するので
			// ミリ秒で起きる (interval 30 秒でも取得 18.9ms を実測)。この値は
			// 「marker を取り逃した場合に気づくまで」の上限でしかない。
			//
			// **既定を延ばさないのは shutdown が延びるため。** 発行済みの
			// BZPOPMIN は ctx キャンセルで中断できないので、停止には最大
			// interval だけかかる (実測: 1 秒で 0.6 秒、5 秒で 4.6 秒)。
			// mkq 既定の 100ms は go-redis が 1 秒へ切り上げるため実効 1 秒で、
			// worker 44 個の構成ではアイドル時に Redis へ毎秒 774 コマンド撃つ
			// (本番実測。Misskey TS は毎秒 21.5)。Redis の CPU は 1 コアの
			// 1.2% なので、再起動の速さと引き換えにする価値は薄いと判断した。
			optsBase = append(optsBase, mkq.WithIdlePollInterval(s.idlePollInterval))
		}
		// custom backoff (`{type:"custom"}`) で enqueue された job の retry delay
		// を算出する strategy を全 Worker に登録する。deliver / inbox は
		// Misskey TS httpRelatedBackoff で enqueue されるので、worker 側で同じ
		// 式を再現して drop-in 一致させる (#1406, mkq#67)。built-in backoff
		// (fixed / exponential) の job には影響しない。
		optsBase = append(optsBase, mkq.WithBackoffStrategy(func(attemptsMade int) time.Duration {
			return httpRelatedBackoff(attemptsMade, nil)
		}))

		pool := &workerPool{
			queue:           s.driver.queues[name],
			handler:         newDispatchHandler(handlersSnapshot, name, s.observer, s.handlerDeadline, &s.abandoned),
			optsBase:        optsBase,
			name:            name,
			stuckAfter:      s.resolvedStuckAfter(name),
			nanos:           s.clock(),
			baseConcurrency: concurrency,
			abandoned:       &s.abandoned,
		}
		// `*Locked` 名 method の convention で pool.mu を保持。新規構築直後の
		// pool で外部参照は無いため race は無いが、convention 一貫性のため。
		pool.mu.Lock()
		spawnErr := pool.resizeLocked(concurrency)
		if spawnErr != nil {
			pool.shutdownLocked()
		}
		pool.mu.Unlock()
		if spawnErr != nil {
			// partial 既存 pools を cleanup してから error 返却。
			for _, p := range pools {
				p.mu.Lock()
				p.shutdownLocked()
				p.mu.Unlock()
			}
			s.mu.Lock()
			s.started = false
			s.mu.Unlock()
			return fmt.Errorf("mkqdriver: start workers for %q (after %d queues started successfully): %w",
				name, len(pools), spawnErr)
		}
		pools[name] = pool
	}

	s.mu.Lock()
	s.pools = pools
	s.mu.Unlock()
	s.startSupervisor()
	return nil
}

// clock returns the configured monotonic nanos source.
func (s *Server) clock() func() int64 {
	if s.nanos != nil {
		return s.nanos
	}
	return monotonicNanos
}

// resolvedStuckAfter returns the stuck threshold for qname. A positive
// Config.StuckWorkerAfter overrides every queue; a negative one disables
// liveness tracking everywhere; zero falls back to the per-queue table.
//
// 判定は stuckAfterForQueue に集約する。Redis pool のサイズ計算
// (workerPoolSize) が同じ規則で「どのキューが追跡対象か」を数えるので、
// 二重実装にすると確保する接続数と実際の挙動がずれる。
func (s *Server) resolvedStuckAfter(qname string) time.Duration {
	return stuckAfterForQueue(qname, s.stuckAfter)
}

// startSupervisor launches the goroutine that periodically reconciles
// every pool. Called at the end of Start.
//
// **autoscale とは独立に回す必要がある。** jobQueueAutoScale は opt-in で、
// 無効なら Resize は一度も呼ばれない。詰まりの回収を Resize 経路にだけ
// 置くと、既定構成では capacity が減ったまま二度と戻らない。
func (s *Server) startSupervisor() {
	interval := s.superviseInterval
	if interval == 0 {
		interval = defaultSuperviseInterval
	}
	if interval < 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.mu.Lock()
	// Shutdown が stopSupervisor を済ませた後にここへ来ると、誰も止めない
	// goroutine が残る。Shutdown は pools を nil にするので、それを見て降りる。
	if s.pools == nil {
		s.mu.Unlock()
		cancel()
		close(done)
		return
	}
	// **期限が有効なら隔離が無効でも supervisor が要る。** 漏れの予算
	// (allowedRosterLocked) を適用するのは reconcileLocked で、それを定期的に
	// 回すのは supervisor だけ。隔離だけを条件にすると、隔離対象外のキュー
	// (maintenance / export / objectStorage) や queueStuckWorkerSeconds: -1 で
	// 放棄が積もっても誰も予算を見ず、漏れが無制限になる
	// (実測: 12 秒で 58 件、頭打ちにならない)。
	tracked := s.handlerDeadline >= 0
	if !tracked {
		for _, p := range s.pools {
			if p.stuckAfter > 0 {
				tracked = true
				break
			}
		}
	}
	if !tracked {
		s.mu.Unlock()
		cancel()
		close(done)
		return
	}
	s.superviseCancel = cancel
	s.superviseDone = done
	s.mu.Unlock()
	go s.supervise(ctx, interval, done)
}

// supervise reconciles every pool on a fixed interval so wedged Workers
// are evicted and replaced even when nothing calls Resize.
func (s *Server) supervise(ctx context.Context, interval time.Duration, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		pools := make([]*workerPool, 0, len(s.pools))
		for _, p := range s.pools {
			pools = append(pools, p)
		}
		s.mu.Unlock()
		for _, p := range pools {
			// Shutdown は stopSupervisor の完了を待つ。pool 数だけ
			// reconcile を回し切ってからでは待ちが積み上がるので、
			// cancel されたら残りを捨てて降りる。
			if ctx.Err() != nil {
				return
			}
			p.mu.Lock()
			err := p.reconcileLocked()
			name := p.name
			p.mu.Unlock()
			// Shutdown と競合した pool は ErrResizeAfterShutdown を返す。
			// 正常な停止手順なので黙って飛ばす。
			if err != nil && !errors.Is(err, ErrResizeAfterShutdown) {
				slog.Warn("mkqdriver: supervisor could not restore the worker pool",
					"queue", name, "err", err)
			}
		}
	}
}

// stopSupervisor cancels the supervisor goroutine and waits for it to
// exit, so Shutdown cannot race a reconcile that spawns Workers.
func (s *Server) stopSupervisor() {
	s.mu.Lock()
	cancel, done := s.superviseCancel, s.superviseDone
	s.superviseCancel, s.superviseDone = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Shutdown stops every worker in every pool. Calls after the first are
// no-ops. Pools are shut down in parallel so the wall-clock latency is
// max(per-pool shutdown time) rather than the sum.
//
// Stop is fast (cancel + goroutine join, not job completion) so per
// pool shutdown is ms-order; the parallelism is mostly insurance
// against pathological cases (many queues × many Workers each).
func (s *Server) Shutdown() {
	// supervisor を先に止める。止めずに畳むと、pools を外した直後に
	// supervisor が捕捉済みの pool へ reconcile を走らせ、誰も所有して
	// いない Worker を spawn しうる (shutdownLocked のガードで弾かれるが、
	// 先に止めるほうが順序として明確)。
	s.stopSupervisor()

	s.mu.Lock()
	toShutdown := s.pools
	s.pools = nil
	s.started = false
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, p := range toShutdown {
		wg.Add(1)
		go func(p *workerPool) {
			defer wg.Done()
			p.mu.Lock()
			defer p.mu.Unlock()
			p.shutdownLocked()
		}(p)
	}
	wg.Wait()
}

// Resize changes the worker count for qname at runtime. Implements the
// Driver.Resize contract (see driver.Driver interface). Concurrent
// Resize calls on the same qname serialise via the pool's internal
// mutex; calls on different qnames proceed in parallel.
//
// n == 0 is allowed and means "stop all workers for this queue" (= the
// queue is paused). n < 0 returns an error. There is no hard upper
// bound — the auto-scale controller (#1125) enforces maxWorkers
// upstream.
func (s *Server) Resize(qname string, n int) error {
	if n < 0 {
		return fmt.Errorf("mkqdriver: Resize: n must be >= 0, got %d", n)
	}
	s.mu.Lock()
	pool, ok := s.pools[qname]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("mkqdriver: Resize: unknown queue %q", qname)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.resizeLocked(n)
}

// workerCount reports the number of **live** Workers for qname, or 0 if
// no pool exists (Server not started, queue unknown). Called by
// Driver.WorkerCount.
//
// 生存数であって帳簿上の本数ではない (#2657)。詰まった Worker は roster に
// 残っていても除外され、quarantine 済みのものは roster にそもそもいない。
// Resize は reconcileLocked 経由で必ず先に quarantine するので、
// 「WorkerCount が 2 を返したので Resize(3) したら実は 4 本あって
// scale-down された」という食い違いは起きない。
func (s *Server) workerCount(qname string) int {
	s.mu.Lock()
	pool, ok := s.pools[qname]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.liveCountLocked()
}

// QuarantinedWorkerCount reports how many Workers for qname are held in
// quarantine — evicted from the roster because their handler overran
// StuckWorkerAfter, and not yet observed returning from it.
//
// 診断用。0 でない状態が続いているなら handler がどこかでブロックして
// いるので、その job type を疑う (#2657 の本番例は inbox の AP 処理が
// ctx を見ずに止まっていた)。
func (s *Server) QuarantinedWorkerCount(qname string) int {
	s.mu.Lock()
	pool, ok := s.pools[qname]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return len(pool.quarantine)
}

// resizeLocked records the requested roster size and reconciles the
// pool. Caller must hold pool.mu.
//
// Post-Shutdown guard: returns ErrResizeAfterShutdown if shutdownLocked
// has already run on this pool. Prevents the captured-pool race where
// Resize spawns Workers on a pool no longer owned by any Server.
func (p *workerPool) resizeLocked(n int) error {
	if p.shutdown {
		return ErrResizeAfterShutdown
	}
	p.desired = n
	if n > p.peakDesired {
		p.peakDesired = n
	}
	return p.reconcileLocked()
}

// reconcileLocked restores the pool to its configured worker count.
// Caller must hold pool.mu.
//
// 順序に意味がある:
//
//  1. 閾値を超えた Worker を roster から quarantine へ退避する。**無条件**。
//     これで roster は常に「生存している Worker だけ」になり、
//     len(p.workers) と WorkerCount の勘定が一致する
//  2. quarantine のうち「隔離後に 1 件でも完了した」= 生きていると証明
//     されたものを roster へ戻す
//  3. roster を目標数に合わせる
//
// Resize / Start / supervisor はすべてここを通るので、勘定の一貫性は
// この関数だけを読めば分かる。
func (p *workerPool) reconcileLocked() error {
	if p.shutdown {
		return ErrResizeAfterShutdown
	}
	p.quarantineStuckLocked()
	p.reinstateProvenLiveLocked()
	p.clearProtectionLocked()
	return p.adjustLocked()
}

// clearProtectionLocked drops the reinstatement grace once the handle has
// finished the job it was holding when it came back. Caller must hold
// pool.mu.
//
// **idle かどうかで判定しない。** それは reinstateProvenLiveLocked が退けた
// のと同じ罠で、忙しいキューでは job と job の間の idle がマイクロ秒しか
// 無いため supervisor の周期ではまず捉えられない。解除条件をそれにすると
// 庇いが外れず、roster が目標を超えたまま何時間も張り付く (実測: 設定 16 /
// maxWorkers 128 で roster 144 に固定)。完了カウンタなら job 1 件で外れる。
func (p *workerPool) clearProtectionLocked() {
	for _, h := range p.workers {
		if h.protected && h.completed.Load() != h.completedAtReinstate {
			h.protected = false
		}
	}
}

// quarantineStuckLocked moves every roster Worker whose handler has run
// longer than stuckAfter into p.quarantine. Caller must hold pool.mu.
//
// **停止しない。** 閾値超過は詰まりの証明ではない。ここで Stop すると
// in-flight job が cancel されて retry に回り、閾値より長い job は永久に
// 完了しなくなる。隔離した Worker は job をそのまま完走でき、完走した
// 事実は completed カウンタで観測できる (reinstateProvenLiveLocked)。
//
// **数を絞らない。** 絞ると詰まった Worker が roster に残り、
// len(p.workers) と生存数がずれて「増やせと言ったのに縮む」が起きる。
// 総数の上限は roster 側 (allowedRosterLocked) で掛ける。
func (p *workerPool) quarantineStuckLocked() {
	if p.stuckAfter <= 0 {
		return
	}
	now := p.nanos()
	var live, moved []*workerHandle
	for _, h := range p.workers {
		if h.busyFor(now) >= p.stuckAfter {
			h.quarantinedAt = now
			h.completedAtQuarantine = h.completed.Load()
			moved = append(moved, h)
			continue
		}
		live = append(live, h)
	}
	if len(moved) == 0 {
		return
	}
	p.workers = live
	p.quarantine = append(p.quarantine, moved...)
	for _, h := range moved {
		// 「詰まった」と断定しない。長い batch job (export の巨大アカウント、
		// objectStorage の bulk cleanup) でも同じ閾値を踏みうる。
		slog.Warn("mkqdriver: handler exceeded the stuck-worker threshold; the worker no longer counts towards the queue budget and has been replaced",
			"queue", p.name, "worker", h.seq,
			"busyFor", h.busyFor(now).Truncate(time.Millisecond).String(),
			"threshold", p.stuckAfter.String(),
			"quarantined", len(p.quarantine))
	}
}

// reinstateProvenLiveLocked returns quarantined Workers that have
// completed a job since eviction back to the roster. Caller must hold
// pool.mu.
//
// **「今 idle か」で判定しない。** mkq は moveToFinished が返す prefetch を
// そのまま次の handler に渡すので、忙しいキューでは job と job の間の idle が
// マイクロ秒しかなく、supervisor の周期ではまず捉えられない。それを release
// 条件にすると、遅かっただけの健全な Worker が永久に隔離されたまま総数の
// 枠を食い潰す。完了カウンタなら取りこぼさない。
//
// 戻すだけで停止しないのも意図的。復帰時に握っていた job は原則そのまま
// 完走する (protected が外れるのはその job を終えてから)。庇いは 1 件ぶんなので
// その後も余剰なら通常の scale-down 対象に戻るし、**漏れの予算に当たっての
// 縮小では庇い自体が無視される** (adjustLocked)。
func (p *workerPool) reinstateProvenLiveLocked() {
	if len(p.quarantine) == 0 {
		return
	}
	now := p.nanos()
	var stillWedged, back []*workerHandle
	for _, h := range p.quarantine {
		if h.provenLive() {
			back = append(back, h)
			continue
		}
		stillWedged = append(stillWedged, h)
	}
	if len(back) == 0 {
		return
	}
	p.quarantine = stillWedged
	for _, h := range back {
		// 次の縮小で真っ先に殺されないよう庇う。roster の末尾に付くので、
		// 庇わないと splitForRemoval の末尾優先で最初の victim になる。
		// 庇いは「いま持っている job を 1 件終えるまで」(clearProtectionLocked)。
		h.protected = true
		h.completedAtReinstate = h.completed.Load()
	}
	p.workers = append(p.workers, back...)
	for _, h := range back {
		slog.Info("mkqdriver: worker finished the job that overran the threshold; returned to the pool",
			"queue", p.name, "worker", h.seq,
			"quarantinedFor", time.Duration(now-h.quarantinedAt).Truncate(time.Millisecond).String(),
			"quarantined", len(p.quarantine))
	}
}

// allowedRosterLocked returns the roster size the pool may actually run,
// which is p.desired unless what the queue has leaked — quarantined Workers
// plus abandoned handlers — has grown past the tolerated headroom. Caller
// must hold pool.mu.
//
// **上限が要る。** 隔離した Worker の goroutine は止められない (handler が
// 戻らない以上、Stop は stopWorkerTimeout だけ待たされたうえで goroutine を
// 残して返る) ので、代わりを無制限に立てると handler を確実に詰まらせる job が
// 1 種類あるだけで goroutine と Redis 接続が際限なく増える。それを食い潰すと
// `resource temporarily unavailable` を自分で再現することになる。#2657 の
// 直前にも同じエラーが出ていたが、**それが詰まりの原因だという証拠は無い**
// (valkey のエラーを一度も出していないプロセスで同じ症状が全部再現する。
// #2659 / docs/docker-uds.md)。
//
// **上限の基準を desired にしない。** desired は autoscale が動かす値で、
// しかも autoscale の目標は「この関数が返した生存数 + step」から決まる。
// 基準が desired に依存すると、
//
//	allowed = desired - (定数)  →  desired' = allowed + step
//	→ step < 定数 のあいだ allowed が毎 tick 減り続ける
//
// という帰還ループになり、**scale-up 要求のたびに roster が縮む**。
// desired=16 / 隔離 20 なら 3 tick で 0、隔離 42 なら 20 tick かけて 0 まで
// 落ちるのを実測した。
//
// 基準を peakDesired (単調非減少) にすると上限 C が desired から独立するので、
// autoscale の反復は `allowed' = min(allowed + step, C)` となって **C まで
// 上がって収束する**。単調なので autoscale の伸長を頭打ちにもしない。
func (p *workerPool) allowedRosterLocked() int {
	// **stuckAfter で早期 return しない。** 隔離を切っていても期限 (#2658) は
	// 動くので、放棄だけが起きるキューがある — maintenance / export /
	// objectStorage は既定で隔離の対象外だし、queueStuckWorkerSeconds: -1 でも
	// そうなる。そこで早期 return すると放棄ぶんが予算に入らず、
	// leakedLocked のコメントが言う「goroutine と DB 接続だけが際限なく
	// 増える」状態がそのまま残る (実測: 6 秒で 28 件、頭打ちにならない)。
	// 何も漏れていなければ ceiling は desired を上回るので、平常時の挙動は
	// 早期 return と同じ。
	ceiling := p.peakDesired + quarantineHeadroomFor(p.baseConcurrency) - p.leakedLocked()
	if ceiling < 0 {
		ceiling = 0
	}
	return min(p.desired, ceiling)
}

// leakedLocked counts everything this queue has leaked and cannot reclaim:
// quarantined Workers plus handlers the dispatcher abandoned. Caller must
// hold pool.mu.
//
// **放棄した handler も数える。** 数えないと上限が機能しない。放棄すると
// dispatcher が戻り、その worker は job を処理するので #2658 の provenLive が
// 隔離から roster に戻す = quarantine が空になる。quarantine だけで上限を
// 測っていると、handler を確実に詰まらせる job がある限り「隔離 → 期限で
// 放棄 → 復帰 → また隔離」を回り続け、**goroutine と DB 接続だけが際限なく
// 増える**。DB pool を食い潰すとキューだけでなくプロセス全体が止まる。
func (p *workerPool) leakedLocked() int {
	// 隔離が無効なキューでは quarantine が常に空で、放棄ぶんだけが積まれる。
	n := len(p.quarantine)
	if p.abandoned != nil {
		current, _ := p.abandoned.snapshot(p.name)
		n += current
	}
	return n
}

// quarantineHeadroomFor returns how much leakage — quarantined Workers plus
// abandoned handlers — a pool with the given configured worker count
// tolerates before it starts winding down. The argument is the configured
// count (workerPool.baseConcurrency), not the current roster target.
func quarantineHeadroomFor(configured int) int {
	return max(configured, quarantineHeadroom)
}

// reportCapLocked logs, at most once per reportCapEvery, that the pool
// cannot reach its configured worker count. Caller must hold pool.mu.
func (p *workerPool) reportCapLocked(allowed int) {
	now := p.nanos()
	if p.capLogged && time.Duration(now-p.lastCapLogNanos) < reportCapEvery {
		return
	}
	p.capLogged = true
	p.lastCapLogNanos = now
	oldest := time.Duration(0)
	for _, h := range p.quarantine {
		if d := time.Duration(now - h.quarantinedAt); d > oldest {
			oldest = d
		}
	}
	abandonedNow := 0
	if p.abandoned != nil {
		abandonedNow, _ = p.abandoned.snapshot(p.name)
	}
	slog.Error("mkqdriver: too many workers are held outside the pool; it can no longer run the requested worker count",
		"queue", p.name, "configured", p.baseConcurrency, "requested", p.desired, "running", allowed,
		"quarantined", len(p.quarantine), "abandonedHandlers", abandonedNow,
		"oldestQuarantinedFor", oldest.Truncate(time.Second).String(),
		"hint", "job handlers are not returning; check mk_job_workers_quarantined vs mk_job_handlers_abandoned to see which budget is consumed, then fix the handler (queueStuckWorkerSeconds / queueHandlerDeadlineSeconds only move the thresholds)")
}

// adjustLocked grows or shrinks the roster to the allowed size. Caller
// must hold pool.mu. On scale-up new Workers are spawned via mkq.Process;
// on scale-down the victims picked by splitForRemoval are stopped
// concurrently (Stop = cancel in-flight, not wait for completion — see
// Server type doc).
//
// **要求どおりの本数が止まるとは限らない。** autoscale 由来の縮小では、
// job を持ったまま庇われている Worker を victim にしないので、次の reconcile へ
// 持ち越されることがある。漏れの予算に当たっての縮小では庇いを無視するので
// 要求どおり止まる (honourProtection)。
func (p *workerPool) adjustLocked() error {
	n := p.allowedRosterLocked()
	if n < p.desired {
		p.reportCapLocked(n)
	}
	if p.desired == 0 {
		// 「このキューを止める」という明示の要求。隔離中の Worker も畳む。
		// 本当に詰まっていれば Stop は stopWorkerTimeout まで返らないが、
		// 止めろと言われている以上そこは待つ。roster と一緒に 1 回で止めて、
		// pool.mu を握ったままの待ちが 2 本直列にならないようにする。
		toStop := append(append([]*workerHandle(nil), p.workers...), p.quarantine...)
		p.workers, p.quarantine = nil, nil
		stopHandles(toStop)
		return nil
	}
	current := len(p.workers)
	switch {
	case n >= current:
		if n == current {
			return nil
		}
		// scale-up: 不足分の Worker を新規 spawn
		needed := n - current
		spawned := make([]*workerHandle, 0, needed)
		for i := 0; i < needed; i++ {
			opts := append([]mkq.WorkerOption{mkq.WithConcurrency(1)}, p.optsBase...)
			h := &workerHandle{seq: p.seq}
			w, err := mkq.Process(p.queue, p.trackedHandler(h), opts...)
			if err != nil {
				// 既に spawn した Worker は停止して回復
				stopHandles(spawned)
				return fmt.Errorf("mkqdriver: spawn worker: %w", err)
			}
			h.w = w
			p.seq++
			spawned = append(spawned, h)
		}
		p.workers = append(p.workers, spawned...)
		return nil
	default: // n < current
		surplus := current - n
		// **上限に当たっての縮小では庇いを無視する。** 庇いは「復帰させた
		// Worker の job を守る」ためのもので、予算超過はそれより優先される。
		// 庇いを効かせたままだと、放棄した handler が積もって allowed=0 に
		// なっても roster を縮められない (復帰 -> 庇われる -> victim 候補から
		// 外れる、が回り続ける)。実測でその状態を確認している: allowed=0 の
		// まま roster=4 が維持され、goroutine が deadline ごとに増え続けた。
		honourProtection := n >= p.desired
		maxBusy := surplus
		if honourProtection {
			// 庇っている Worker のぶんは他の Worker から取り立てない。庇う対象を
			// victim から外すだけだと、代わりに別の Worker の job が cancel される
			// ので、隔離が job を殺さないという性質が結局崩れる。idle は費用ゼロ
			// なので、そちらは上限に関係なく畳んでよい。
			protectedBusy := 0
			for _, h := range p.workers {
				if h.protected && !h.idle() {
					protectedBusy++
				}
			}
			maxBusy = max(surplus-protectedBusy, 0)
		}
		keep, toStop := splitForRemoval(p.workers, surplus, maxBusy, honourProtection)
		p.workers = keep
		stopHandles(toStop)
		return nil
	}
}

// trackedHandler wraps the pool's dispatch handler so entry and exit are
// visible to the liveness bookkeeping. One wrapper per Worker — sharing
// a single closure would make "which Worker is busy" unanswerable, which
// is the whole point.
func (p *workerPool) trackedHandler(h *workerHandle) mkq.Handler[framedPayload] {
	base := p.handler
	nanos := p.nanos
	return func(ctx context.Context, job *mkq.Job[framedPayload]) (any, error) {
		// 0 は idle を表す番兵なので、単調時間が 0 を返しても busy と
		// 区別できるよう 1 に丸める (プロセス起動直後に起きうる)。
		t := nanos()
		if t == 0 {
			t = 1
		}
		h.busySinceNanos.Store(t)
		defer func() {
			// 完了を先に数える。busy を落としてから数えると、その隙間で
			// reconcile が走ったときに「idle だが完了は未計上」に見える。
			h.completed.Add(1)
			h.busySinceNanos.Store(0)
		}()
		return base(ctx, job)
	}
}

// splitForRemoval picks up to k scale-down victims out of ws and returns
// (kept, removed). kept preserves the original order. At most maxBusy of
// the victims hold a job. When honourProtection is set, Workers protected
// while holding a job are never chosen at all, so fewer than k may be
// removed; the caller clears it when the shrink is forced by the leak
// budget rather than requested by autoscale.
//
// 優先順位:
//
//	0: idle (in-flight job が無いので停止しても何も失わない)
//	1: 処理中 (停止すると job が cancel され retry になる)
//	-: 処理中かつ庇われている = 選ばない
//
// 同 rank 内は末尾優先で、従来の scale-down 挙動を保つ。
//
// **末尾固定をやめた理由** (#2657): 末尾は「直前に autoscale が足した
// Worker」で、詰まりが起きている最中はそこだけが健全という状態になる。
// 本番の inbox で 4 本すべてが handler に入ったまま戻らなくなり、autoscale が
// 作った 5 本目を毎回 6 秒で殺し続けた。
//
// **庇う対象を除く理由**: 隔離から戻したばかりの Worker は roster の末尾に
// 付くので、庇わないと末尾優先でそれが最初の victim になる。「隔離は job を
// 殺さない」という設計なのに、生きていると分かった直後にその job を
// cancel することになる。忙しいキューでは roster に idle が 1 本も無いため、
// 単に「今回は見送る」だけでは次の reconcile で同じことが起きる。
//
// 閾値超過の Worker はここには来ない。reconcileLocked が adjustLocked より
// 先に quarantine へ退避しているので、roster に残っているのは生存分だけ。
func splitForRemoval(ws []*workerHandle, k, maxBusy int, honourProtection bool) (kept, removed []*workerHandle) {
	if k <= 0 {
		return ws, nil
	}
	type candidate struct {
		idx  int
		busy bool
	}
	cands := make([]candidate, 0, len(ws))
	for i, h := range ws {
		busy := !h.idle()
		if busy && h.protected && honourProtection {
			continue
		}
		cands = append(cands, candidate{idx: i, busy: busy})
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].busy != cands[b].busy {
			return !cands[a].busy
		}
		// 同 rank は末尾優先 (従来の scale-down 挙動を保つ)。
		return cands[a].idx > cands[b].idx
	})
	doomed := make(map[int]struct{}, k)
	busyTaken := 0
	for _, c := range cands {
		if len(doomed) == k {
			break
		}
		if c.busy {
			if busyTaken >= maxBusy {
				continue
			}
			busyTaken++
		}
		doomed[c.idx] = struct{}{}
	}
	kept = make([]*workerHandle, 0, len(ws)-len(doomed))
	removed = make([]*workerHandle, 0, len(doomed))
	for i, h := range ws {
		if _, ok := doomed[i]; ok {
			removed = append(removed, h)
			continue
		}
		kept = append(kept, h)
	}
	return kept, removed
}

// liveCountLocked counts roster Workers that are not over the stuck
// threshold. Caller must hold pool.mu.
//
// reconcileLocked を通った直後は roster にほぼ閾値超過の Worker はいない
// (quarantine したあとに戻した分だけは再判定していないので残りうる)。
// いずれにせよ WorkerCount が過大申告しないようここで数え直す。
func (p *workerPool) liveCountLocked() int {
	if p.stuckAfter <= 0 {
		return len(p.workers)
	}
	now := p.nanos()
	n := 0
	for _, h := range p.workers {
		if h.busyFor(now) < p.stuckAfter {
			n++
		}
	}
	return n
}

// shutdownLocked stops every Worker in the pool — roster and quarantine
// alike — and marks the pool as shut down (= subsequent Resize calls
// return ErrResizeAfterShutdown). Caller must hold pool.mu. Used by
// Server.Shutdown.
func (p *workerPool) shutdownLocked() {
	p.shutdown = true
	// quarantine も止める。詰まったままの Worker への Stop は
	// stopWorkerTimeout で頭打ちになるだけだが、閾値を超えただけで
	// 実際には動いていた Worker はここで畳まれる。
	stopHandles(append(append([]*workerHandle(nil), p.workers...), p.quarantine...))
	p.workers = nil
	p.quarantine = nil
}

// stopWorkerTimeout bounds how long stopWorker waits for one Worker.
//
// **上限が無いと、起こしを取りこぼした 1 本が起動全体を止める。** dispatcher は
// BZPOPMIN で最大 30 秒 park するが、発行済みの読み取りは ctx キャンセルで
// 中断できない。mkq は Stop で専用 key を突いて起こすので通常は ms で返る。
// ここはその起こしが届かなかった場合の保険で、30 秒は park の上限に合わせて
// ある (それを過ぎれば dispatcher は自力で目を覚ます)。
//
// 実際に無制限で踏んだ: Resize が inbox を 16 → 4 に縮める過程で Stop が
// 返らず、Server.Start が完了せず HTTP の listen まで到達しなかった。
const stopWorkerTimeout = 30 * time.Second

// stopWorker wraps mkq.Worker.Stop with structured error logging. Stop
// failures here are non-fatal (the goroutine still exits via runCancel)
// but operators should be alerted via logs because frequent failures
// usually indicate Redis connectivity issues that need investigation.
func stopWorker(w *mkq.Worker) {
	ctx, cancel := context.WithTimeout(context.Background(), stopWorkerTimeout)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		slog.Warn("mkqdriver: worker stop error", "err", err)
	}
}

// stopHandles stops every Worker in hs concurrently.
//
// **逐次に止めない。** 1 本あたりの待ちは通常 ms だが、起こしを取りこぼすと
// stopWorkerTimeout まで伸びる。逐次だとそれが本数分積み上がる (16 本なら
// 最悪 8 分)。並列なら 1 回分で済む。
//
// h.w が nil の handle は spawn 途中で mkq.Process が失敗した分なので飛ばす。
func stopHandles(hs []*workerHandle) {
	var wg sync.WaitGroup
	for _, h := range hs {
		if h == nil || h.w == nil {
			continue
		}
		wg.Add(1)
		go func(w *mkq.Worker) {
			defer wg.Done()
			stopWorker(w)
		}(h.w)
	}
	wg.Wait()
}

// defaultHandlerDeadline bounds how long one job's handler may run before
// the dispatcher stops waiting for it (#2658).
//
// **1 時間は「健全な job の上限」ではなく「これを超えたら worker を諦めさせない
// ための線」。** inbox の理論上の worst case (resolver 256 段 x safehttp 10 秒 =
// 43 分) を上回る値として選んだ。#2657 の隔離 (30 分) より後に発火するのは
// 意図的で、隔離が先に capacity を戻し、こちらが後から worker 本体を回収する。
const defaultHandlerDeadline = time.Hour

// abandonGrace is how long the dispatcher waits after the deadline has
// cancelled the handler's context before giving up on it and leaking its
// goroutine. 親 ctx のキャンセルはここに入らない (runBounded 参照)。
//
// ctx を見る handler は cancel の直後に戻るので、この猶予があれば goroutine を
// 捨てずに済む。捨てることになるのは ctx を無視する経路だけ。
const abandonGrace = 5 * time.Second

// graceFor caps the unwind grace at the deadline itself, so a short deadline
// does not spend far longer waiting for the unwind than for the work.
func graceFor(deadline time.Duration) time.Duration {
	return min(abandonGrace, deadline)
}

// handlerDeadlines overrides defaultHandlerDeadline per task type. A zero
// entry means "no deadline".
//
// **1 job が分単位〜時間単位かかるのが正常な batch 系は期限を持たない。**
// ここで期限を切ると job が失敗して retry に回り、**永久に完了しなくなる**。
// 具体的には objectStorage:cleanRemoteFiles が最大 10000 バッチ x 500ms
// (それだけで 83 分)、maintenance:deleteAccount が 100 件ごとに 250ms 空ける
// 設計、export / import は 1 job でアカウント全体をページングする。
//
// **その代わり、これらは何にも守られない。** #2657 の隔離が見るのは
// defaultStuckAfterByQueue のキューだけで、ここに挙げた task type のうち
// deliver に載る maintenance:deleteAccount 以外は export / objectStorage /
// maintenance のいずれかに載っており、そちらは隔離の対象外。つまり
// export の handler が戻らなくなったら、その worker は失われたままになる。
// job 側に進捗を持たせて分割する以外に手が無いので、現時点では受け入れる。
//
// **task type ごとにするのはキュー単位では粗すぎるため。**
// maintenance:deleteAccount は maintenance ではなく deliver キューに載って
// いるので、キュー単位だと ap:deliver (10 秒級) と同じ扱いになってしまう。
//
// chart:resync / chart:clean はここに**入れない**。resync は chart:tick と
// 同じ runTick を major=true で呼ぶだけで、tick 側は対象なのに resync だけ
// 外すのは筋が通らない。clean も chart ごとに Clean を呼ぶだけ。
//
// **`internal/queue` の定数を import しない。** driver は queue の下位
// パッケージなので、親を import すると親が driver を import できなくなる
// (今は無いが、既定 driver を組み立てる関数を queue 側に置いた瞬間に循環する)。
// 代わりに値を直書きし、定数との一致は internal test が固定している
// (TestHandlerDeadlines_MatchTaskTypeConstants)。task type の値は BullMQ の
// job name として wire に出るので、そもそも自由に変えられない。
var handlerDeadlines = map[string]time.Duration{
	"objectStorage:cleanRemoteFiles":      0,
	"maintenance:cleanRemoteNotes":        0,
	"maintenance:orphanUserCleanup":       0,
	"maintenance:orphanAttachmentCleanup": 0,
	"maintenance:deleteAccount":           0,
	"maintenance:clean":                   0,
	"maintenance:retentionAggregate":      0,
	"export":                              0,
	"import":                              0,
	"importCustomEmojis":                  0,
}

// handlerDeadlineFor returns the deadline for taskType, or 0 when the task
// type is exempt.
func handlerDeadlineFor(taskType string, configured time.Duration) time.Duration {
	if configured < 0 {
		return 0
	}
	if d, ok := handlerDeadlines[taskType]; ok {
		return d
	}
	if configured > 0 {
		return configured
	}
	return defaultHandlerDeadline
}

// ErrHandlerAbandoned is returned to mkq when the dispatcher gives up
// waiting for a handler. The job fails and follows the normal retry path;
// the handler's goroutine keeps running and is never reclaimed.
var ErrHandlerAbandoned = errors.New("mkqdriver: handler did not return before its deadline; the dispatcher moved on and the handler's goroutine keeps running")

// newDispatchHandler builds the mkq handler closure that demuxes
// incoming jobs to the registered driver.HandlerFunc. The handlers
// map is captured by reference; callers must guarantee it is not
// mutated after the closure is created (Server.Start enforces this
// by snapshotting via maps.Clone before the call).
//
// driver.ErrSkipRetry → mkq.ErrUnrecoverable and driver.DelayError →
// mkq.Delay conversions live here (nativeError) so processors can keep
// their existing %w-wrap idiom unchanged.
func newDispatchHandler(handlers map[string]driver.HandlerFunc, qname string, obs driver.Observer, deadline time.Duration, ab abandonSink) mkq.Handler[framedPayload] {
	return func(ctx context.Context, job *mkq.Job[framedPayload]) (any, error) {
		taskType := job.Data.Type
		h := handlers[taskType]
		if h == nil {
			// 未登録 task type は ErrSkipRetry 相当 (再 enqueue しても処理者が
			// いないので無限ループになる)。
			return nil, fmt.Errorf("mkqdriver: no handler for %q: %w", taskType, mkq.ErrUnrecoverable)
		}
		run := func(c context.Context) error {
			d := handlerDeadlineFor(taskType, deadline)
			return runBounded(c, h, mkqTask{taskType: taskType, payload: job.Data.Body},
				d, graceFor(d),
				abandonInfo{queue: qname, taskType: taskType, jobID: job.ID}, ab)
		}
		// observer 未配線なら clock も触らない (hot path、#2277)。
		if obs == nil {
			return nil, nativeError(run(ctx))
		}
		// dispatch wait は **初回試行のみ** 観測する。job.Timestamp は BullMQ の
		// 作成時刻で attempt ごとに更新されないため、retry 分を含めると backoff
		// 待ち (意図的な遅延) が「詰まり」として混ざり、p95 が数十秒に化ける。
		// 見たいのは「enqueue された job がどれだけ待たされてから最初に拾われたか」
		// = 混雑度なので、AttemptsMade == 0 に限定する。
		//
		// **取り出されたのが初めてのときに限る (AttemptsStarted)。** 遅延で戻した
		// job (driver.DelayError、#3048) は試行回数を消費しないので AttemptsMade は
		// 0 のまま戻ってくる。そちらだけで見ると、意図して待たせた時間を混雑として
		// 数える。
		if job.AttemptsMade == 0 && job.AttemptsStarted <= 1 && !job.Timestamp.IsZero() {
			obs.ObserveDispatchWait(qname, time.Since(job.Timestamp))
		}
		started := time.Now()
		err := run(ctx)
		// **遅延の要求は観測しない。** 失敗として数えると、落ちた配送先を待たせて
		// いるだけで失敗率が上がって見える。成功として数えると、送っていないのに
		// ms 級の処理が「成功」として処理時間の分布に混ざる。
		var delayed *driver.DelayError
		if !errors.As(err, &delayed) {
			obs.ObserveProcessing(qname, time.Since(started), err != nil)
		}
		return nil, nativeError(err)
	}
}

// nativeError maps the driver sentinels a handler returned to mkq's
// native ones. A DelayError takes precedence over ErrSkipRetry, as in mkq.
func nativeError(err error) error {
	if err == nil {
		return nil
	}
	var delayed *driver.DelayError
	if errors.As(err, &delayed) {
		return fmt.Errorf("%w: %w", err, mkq.Delay(delayed.Delay))
	}
	if errors.Is(err, driver.ErrSkipRetry) {
		return fmt.Errorf("%w: %w", err, mkq.ErrUnrecoverable)
	}
	return err
}

// abandonCounters tracks abandoned handlers per queue. The zero value is
// ready to use; it is a field of Server so its lifetime matches the pools.
type abandonCounters struct {
	mu      sync.Mutex
	total   map[string]uint64
	current map[string]int
}

func (a *abandonCounters) handlerAbandoned(queue string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.total == nil {
		a.total = map[string]uint64{}
		a.current = map[string]int{}
	}
	a.total[queue]++
	a.current[queue]++
}

func (a *abandonCounters) handlerReturnedLate(queue string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current[queue] > 0 {
		a.current[queue]--
	}
}

// snapshot returns (currently abandoned, abandoned in total) for queue.
func (a *abandonCounters) snapshot(queue string) (int, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current[queue], a.total[queue]
}

// AbandonedHandlerCount reports how many handlers for qname the dispatcher
// gave up on and that have still not returned, plus the cumulative total.
//
// 診断用。current が増えたまま減らないなら、handler が本当に戻ってこない
// 経路を踏んでいる (goroutine と DB 接続を掴んだまま)。#2658。
func (s *Server) AbandonedHandlerCount(qname string) (current int, total uint64) {
	return s.abandoned.snapshot(qname)
}

// abandonInfo identifies a job for the abandonment log.
type abandonInfo struct {
	queue    string
	taskType string
	jobID    string
}

// abandonSink counts handlers the dispatcher gave up on. nil is allowed.
type abandonSink interface {
	handlerAbandoned(queue string)
	handlerReturnedLate(queue string)
}

// runBounded invokes h and gives up on it once its context is done and the
// grace period has elapsed.
//
// **goroutine を別に切るのは、それ以外に dispatcher を返す方法が無いから。**
// Go では goroutine を殺せないので、戻ってこない handler を「諦める」には
// 呼び出し側が待つのをやめるしかない。放棄した goroutine はそのまま走り続け、
// DB 接続などを掴んだままになる。それでも worker 一式を失うより軽い
// (#2657 の隔離は dispatcher + handler + Redis 接続をまとめて失っていた)。
//
// deadline <= 0 なら従来どおり同期で呼ぶ。batch 系の task type と、
// 期限を明示的に無効化した構成がこれに当たる。
func runBounded(ctx context.Context, h driver.HandlerFunc, task driver.Task, deadline, grace time.Duration, info abandonInfo, ab abandonSink) error {
	if deadline <= 0 {
		return h(ctx, task)
	}
	hctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		// **ここで recover しないとプロセスが落ちる。** mkq の runHandler は
		// handler の panic を recover して job を failed にするが、その defer は
		// 別 goroutine の panic を拾えない。
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("mkqdriver: handler panic: %v\n%s", r, debug.Stack())
			}
		}()
		done <- h(hctx, task)
	}()

	// **諦めるのは期限を過ぎたときだけ。** 親 ctx のキャンセル (scale-down /
	// shutdown / lock 喪失) では諦めない。mk-go の deliver / inbox /
	// relationship processor は `Handle(_ context.Context, ...)` で ctx を
	// 捨てており、cancel されても止まらない。そこで諦めると job は failed →
	// retry に回る一方で元の実行は完走するので、**scale-down のたびに配送が
	// 二重になる**。親キャンセルの側は mkq の Worker.Stop が
	// stopWorkerTimeout で頭打ちにしており、そこは #2658 以前から変わらない。
	//
	// ctx を見る handler は cancel された時点で戻ってくるので、下の select が
	// そのまま拾う (期限まで待たされることはない)。
	t := time.NewTimer(deadline + grace)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
	}

	if ab != nil {
		ab.handlerAbandoned(info.queue)
		// 遅れて戻ってきたら現存数を戻す。放棄した goroutine が生き続けて
		// いるのか、単に遅かっただけなのかを gauge で見分けられるようにする。
		go func() {
			<-done
			ab.handlerReturnedLate(info.queue)
		}()
	}
	// cause を見ないと「期限が短すぎる」と読み違える。縮小で止められた
	// worker の handler が ctx を無視した場合も、同じ経路でここに来る。
	slog.Warn("mkqdriver: giving up on a handler; its goroutine is abandoned and keeps running (see cause: a deadline means the job ran too long, a cancellation means the worker was being stopped)",
		"queue", info.queue, "taskType", info.taskType, "jobID", info.jobID,
		"elapsed", time.Since(started).Truncate(time.Millisecond).String(),
		"deadline", deadline.String(), "grace", grace.String(),
		"cause", context.Cause(hctx))
	return fmt.Errorf("%w (task %s, job %s, %s)", ErrHandlerAbandoned, info.taskType, info.jobID,
		time.Since(started).Truncate(time.Millisecond))
}

// SetObserver wires the per-job timing hook. Must be called before Start;
// Start snapshots the observer into each queue's dispatch closure.
func (s *Server) SetObserver(obs driver.Observer) {
	s.mu.Lock()
	s.observer = obs
	s.mu.Unlock()
}
