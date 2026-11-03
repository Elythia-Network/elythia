package driver

import (
	"context"
	"encoding/json"
	"time"
)

// HandlerFunc processes a single task. Returning an error wrapping
// ErrSkipRetry tells the driver not to retry even if attempts remain.
type HandlerFunc func(ctx context.Context, t Task) error

// Client enqueues tasks. Drivers translate the (taskType, payload,
// opts) tuple into their native enqueue call.
type Client interface {
	Enqueue(ctx context.Context, taskType string, payload []byte, opts ...EnqueueOption) error
	Close() error
}

// Server is the worker-side runtime: register handlers, then Start
// to begin consuming jobs. Shutdown stops accepting new jobs and
// waits for in-flight ones.
type Server interface {
	Handle(taskType string, h HandlerFunc)
	Start() error
	Shutdown()
}

// InspectorInfo is the per-queue summary returned by Inspector.GetQueueInfo.
// Field names are the driver-neutral counts the admin/queue endpoints map
// onto the BullMQ shape the frontend expects.
type InspectorInfo struct {
	Queue     string
	Size      int
	Active    int
	Pending   int
	Completed int
	Failed    int
	// Delayed is the number of jobs that will not run right away: the
	// delayed bucket (scheduled enqueues and retry-backoff waiters alike)
	// plus the registered repeat schedules. A repeat schedule is counted
	// twice, because its next run also sits in the delayed bucket.
	//
	// 予定と再試行待ちを数え分けない (#3962)。数え分けには delayed の全件を
	// 本文まで読む必要があり、使う側 (admin/queue と stream の queueStats) は
	// どちらも合計しか出さないため。
	Delayed int
	// IsPaused reports whether the queue is paused (BullMQ meta.paused)。
	// upstream admin/queue は queueInfo.isPaused でボタンを切り替える (#17436)。
	IsPaused bool
	// QualifiedName is BullMQ's `<keyPrefix>:<name>` (queue-keys.js の
	// getQueueQualifiedName)。admin UI がそのまま表示する。prefix は driver の
	// 設定なのでここで組む — 上位が "bull" を決め打ちすると、prefix を変えた
	// 構成で嘘を表示する (#2689)。持たない driver は空でよい。
	QualifiedName string
}

// MetricsResult is the BullMQ-compatible per-minute history of
// finalised jobs (completed or failed) for a queue. Drivers that do
// not natively track time-series data return Data nil and only
// populate Count from the cumulative bucket size — admin UIs that
// only display the chart will simply render empty in that case.
//
// Data ordering follows BullMQ's `Queue.getMetrics` contract: newest
// minute first. Entries are int64 to keep the field representable on
// 32-bit platforms despite the underlying minute counts staying small.
type MetricsResult struct {
	Count int64
	Data  []int64
}

// MetricsKindCompleted / MetricsKindFailed name the only kinds
// Inspector.QueueMetrics accepts. Other strings return an error.
const (
	MetricsKindCompleted = "completed"
	MetricsKindFailed    = "failed"
)

// TaskSummary is the driver-neutral projection of a task used by the
// admin/queue list APIs.
type TaskSummary struct {
	ID            string
	Queue         string
	Type          string
	State         string
	Payload       []byte
	Retried       int
	MaxRetry      int
	LastErr       string
	LastFailedAt  time.Time
	NextProcessAt time.Time
	EnqueuedAt    time.Time
	ScheduledAt   time.Time
	// ProcessedAt is when the job was last dispatched to a worker (Bull
	// job.processedOn). Populated for finished jobs; zero for never-run jobs.
	ProcessedAt time.Time
	CompletedAt time.Time
	// ProcessedBy is the worker name that most recently dequeued the job
	// (BullMQ job.processedBy / mkq `pb`). Empty for never-run jobs and for
	// drivers without the concept. Surfaced as the upstream optional
	// QueueJob.processedBy field.
	ProcessedBy string

	// 以下は BullMQ の job HASH をそのまま運ぶ (#2689)。admin の job 詳細は
	// upstream の packJobData と同じく **保存されている値をそのまま出す**のが
	// 正しく、driver 側で組み立て直すと知らない key が黙って消える。
	// 持たない driver は空のまま。

	// Opts is the raw BullMQ `opts` JSON (attempts / backoff /
	// removeOnComplete / removeOnFail / priority など)。
	Opts json.RawMessage
	// Delay is the BullMQ `delay` HASH field in milliseconds.
	Delay int64
	// Stacktrace is the BullMQ `stacktrace` array. 試行ごとに 1 要素積まれる。
	Stacktrace []string
	// ReturnValue is the raw BullMQ `returnvalue` JSON. 完了していない job は空。
	ReturnValue json.RawMessage
	// Progress is the raw BullMQ `progress` JSON (数値 / 文字列 / object)。
	Progress json.RawMessage
	// AttemptsAt holds the start time of every failed attempt, oldest first,
	// in unix milliseconds.
	//
	// **BullMQ には無い。** BullMQ は per-attempt の時刻を残さないので、
	// 再試行を時系列に並べたい admin UI は置く時刻を持てない (upstream
	// Misskey の job 詳細が試行を `at ?` と出しているのはこれが理由)。
	// mkq が拡張として記録する (#2692)。持たない driver / 記録前に失敗した
	// job では空。
	AttemptsAt []int64
}

// Inspector exposes admin-level queue introspection used by the
// admin/queue endpoints.
type Inspector interface {
	Queues() ([]string, error)
	GetQueueInfo(qname string) (*InspectorInfo, error)

	// DispatchableCount returns how many jobs a worker could dequeue from
	// the named queue right now: the pending (wait) count, or 0 while the
	// queue is paused. It skips the rest of the summary.
	//
	// **pause 中は 0 を返す (#3166)。** BullMQ 6 (mkq v1.1.0) では pause しても
	// ジョブは wait に残るので、InspectorInfo.Pending には backlog が見える。
	// オートスケーラがそれを深さと読むと、ジョブを取れないキューの worker を
	// 最大数まで増やし、pause が続く限り減らさない。だから Pending とは
	// pause 中だけ値が違う。
	//
	// **オートスケーラは 1Hz x 管理キュー数で回るので、集計 API を使わせない。**
	// GetQueueInfo は admin パネル向けに wait/active/delayed/completed/failed の
	// 全カウント + repeat の ZCARD + paused 判定を 1 回で引く。オートスケーラが
	// 使うのは Pending 1 個だけで、残りは捨てている。実測でこれがアイドル時の
	// Redis コマンドの 6 割 (毎秒 90 前後) を占めていた (#2605。当時は delayed
	// 全件の ZRANGE + N x HGETALL も含んでいた。#3962 で外した)。
	DispatchableCount(qname string) (int, error)

	DeleteTask(qname, taskID string) error
	DeleteAllPendingTasks(qname string) (int, error)
	RunTask(qname, taskID string) error

	// PauseQueue / UnpauseQueue pause and resume a queue (BullMQ
	// Queue.pause()/resume()、upstream admin/queue/pause・resume #17436)。
	// paused 中は worker が job を fetch しない。enqueue された job は drop
	// されず resume で再開される (driver の保証)。
	PauseQueue(qname string) error
	UnpauseQueue(qname string) error

	ListPendingTasks(qname string, page, pageSize int) ([]*TaskSummary, error)
	ListActiveTasks(qname string, page, pageSize int) ([]*TaskSummary, error)
	ListScheduledTasks(qname string, page, pageSize int) ([]*TaskSummary, error)
	ListRetryTasks(qname string, page, pageSize int) ([]*TaskSummary, error)
	// ListDelayedTasks returns the whole delayed bucket (scheduled and
	// retry-backoff jobs together), latest fire time first.
	//
	// upstream の admin/queue/jobs は Bull の state 名 `delayed` を受け取り、
	// delayed ZSET を新しい順に返す。Scheduled / Retry を後から混ぜると、retry で
	// delayed に戻った job の予定時刻 (ZSET の score) を HASH から復元できず、
	// 並びが崩れる (#3167)。
	ListDelayedTasks(qname string, page, pageSize int) ([]*TaskSummary, error)
	// ListCompletedTasks / ListFailedTasks return finished jobs retained in
	// the completed / failed buckets. Drivers without finished-job retention
	// return an empty slice.
	ListCompletedTasks(qname string, page, pageSize int) ([]*TaskSummary, error)
	ListFailedTasks(qname string, page, pageSize int) ([]*TaskSummary, error)

	GetTaskInfo(qname, taskID string) (*TaskSummary, error)

	// GetTaskLogs returns the log lines recorded against a job, oldest
	// first, and the total number stored.
	//
	// upstream の admin/queue/show-job-logs は `queue.getJobLogs(jobId).logs`
	// をそのまま返す。持たない driver は空を返してよい (存在しない job と
	// log 0 件の job は BullMQ も区別しない)。
	GetTaskLogs(qname, taskID string, start, end int64) ([]string, int64, error)

	// QueueMetrics returns the BullMQ-spec per-minute history for the
	// given queue. kind must be MetricsKindCompleted or
	// MetricsKindFailed; other values return an error. Drivers without
	// native time-series support return MetricsResult.Data == nil but
	// still populate Count from the cumulative bucket cardinality.
	QueueMetrics(qname, kind string) (*MetricsResult, error)

	Close() error
}

// Scheduler registers cron-driven recurring tasks. cronspec follows
// the underlying driver's cron syntax (mkq accepts standard 5-field cron
// expressions and the `@daily` style descriptors).
type Scheduler interface {
	Register(cronspec, taskType string, payload []byte, opts ...EnqueueOption) error
	// PruneUnregistered removes the schedules stored for the driver's
	// queues that this process did not try to Register, and returns them
	// as "<queue>/<scheduleID>". It does nothing when nothing was
	// registered, and returns an error without removing anything when any
	// registration failed.
	//
	// **登録先や名前を変えた cron の旧スケジューラを消すため (#3173)。** 消さないと
	// 永久に発火し続ける — #2818 でプラグインの cron を専用キューへ移したとき、
	// maintenance 側の 6 本が残って二重実行と handler 無しの失敗を続けていた。
	// 全ての Register の後に 1 回呼ぶこと。
	PruneUnregistered() ([]string, error)
	Start() error
	Shutdown()
}

// Driver bundles the per-deployment queue driver factory. A single
// Driver instance owns the underlying connection / runtime; callers
// obtain Client / Server / Inspector / Scheduler from it.
//
// Close releases the resources owned by the driver. Sub-components
// (Client, Inspector etc.) do not need to be Close'd individually
// when the parent Driver is Closed.
type Driver interface {
	Client() Client
	Server() Server
	Inspector() Inspector
	Scheduler() Scheduler
	Close() error

	// WorkerCount returns the number of workers currently **able to take
	// work** for qname. Drivers that share a single worker pool across
	// queues return the pool-wide Concurrency for every qname. Drivers
	// that have not started their Server yet return 0.
	//
	// Used by the Prometheus metrics layer (`mk_job_workers_active`) and
	// by the auto-scale controller (#1120 tracker) to read the current
	// pool size when computing scale decisions.
	//
	// **mkq driver では帳簿上の本数ではない。** handler が閾値を超えて戻って
	// こない worker を除外して数える (#2657)。詰まった worker を健全として
	// 数えると autoscale の scale-up 閾値 (本数 x 4) だけが上がり、実際に
	// 働ける worker が 0 本でも scale-up しない。
	//
	// mkq driver では Resize も同じ勘定で動くので、「WorkerCount が返した値
	// + n」を Resize に渡すと n 本増える。ただし総数の上限に当たっている
	// ときは増えない (その場合 Error ログが出る。5 分に 1 回まで間引かれる)。
	// 縮める側も、隔離から戻したばかりで job を持っている worker は止めない
	// ので要求より少なく止まることがある。いずれも次の tick で収束し、
	// WorkerCount は常に実際の本数を返す。
	WorkerCount(qname string) int

	// Resize changes the worker pool size for qname to n at runtime.
	// Returns ErrResizeNotSupported when there is no pool to resize
	// (mkq before Server.Start). Otherwise:
	//
	//   - n > current: spawn up to (n - current) new worker goroutines.
	//   - n < current: stop up to (current - n) workers. **In-flight jobs are
	//     cancelled, not awaited** — the job's context fires and BullMQ
	//     re-locks it for the next pickup, so scale-down cannot block for
	//     minutes on a slow remote inbox
	//     (`TestServer_ResizeDown_CancelsInFlight`).
	//   - n == current: no-op, returns nil.
	//
	// Resize is intended for the auto-scale controller (#1120 tracker
	// PR #1125 wiring). Concurrent Resize calls on the same qname
	// serialise internally; callers do not need to gate themselves.
	//
	// Negative n returns an error. n is clamped to [0, server-defined-max]
	// per the driver's implementation (mkqdriver has no hard ceiling
	// because the auto-scale controller enforces it upstream).
	Resize(qname string, n int) error
}

// Observer receives per-job timing observations from a driver's dispatch
// path. Wiring is optional: drivers that have no observer configured must
// behave exactly as before (no allocation, no clock reads on the hot path).
//
// driver パッケージが Prometheus や admin API を知らずに済むよう、
// 観測は interface で外に出す。実装は internal/queue/runtimestats と
// internal/queue/metrics 側が持つ (#2277)。
type Observer interface {
	// ObserveDispatchWait reports the gap between enqueue and the worker
	// picking the job up. Drivers that cannot determine the enqueue time
	// skip this call entirely.
	ObserveDispatchWait(queue string, d time.Duration)
	// ObserveProcessing reports handler wall time and whether it failed.
	ObserveProcessing(queue string, d time.Duration, failed bool)
}
