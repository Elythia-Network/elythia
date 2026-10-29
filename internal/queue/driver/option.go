package driver

import "time"

// EnqueueOptions captures driver-neutral parameters for enqueueing a
// single task. Concrete drivers translate these into native options.
//
// Zero-valued fields mean "use the driver default":
//   - Queue ""           : driver decides (often the default queue name)
//   - MaxRetry 0         : driver default retry count
//   - UniqueTTL 0        : no unique-key dedup
//   - ProcessIn 0        : process immediately
//   - KeepFailed 0       : driver default (= no automatic retention)
//   - KeepCompleted 0    : driver default (= no automatic retention)
//   - KeepCompletedAge 0 : driver default (= no age-based prune)
//   - KeepFailedAge 0    : driver default (= no age-based prune)
type EnqueueOptions struct {
	Queue     string
	MaxRetry  int
	UniqueTTL time.Duration
	ProcessIn time.Duration

	// KeepFailed bounds the size of the failed ZSET for this task's
	// queue. mkq translates this to `mkq.WithKeepFailed(n)`.
	//
	// KeepFailedSet は MaxRetry / MaxRetrySet と同じ「明示 0 vs default」
	// 区別 pattern を踏襲して提供するが、driver translation 上は両者とも
	// 「unlimited 蓄積 (= driver default)」になり挙動は同じ
	// (mkqdriver/option.go の `> 0` guard 参照)。operator が `<queue>JobKeepFailed:
	// 0` と明示的に opt-out したこと自体は Set=true として記録される
	// (= documented intent としての semantic を保つ)。
	KeepFailed    int
	KeepFailedSet bool

	// KeepCompleted bounds the size of the completed ZSET, mirroring
	// KeepFailed's semantics for the completed bucket. mkq translates
	// this to `mkq.WithKeepCompleted(n)`. BullMQ の
	// `removeOnComplete: N` 互換 (#1193)。
	//
	// KeepCompletedSet も KeepFailedSet と同じ「明示 0 vs default」
	// 区別 pattern。
	KeepCompleted    int
	KeepCompletedSet bool

	// KeepCompletedAge drops completed jobs older than this duration
	// (BullMQ `removeOnComplete: {age: <seconds>}` 互換)。mkq
	// translates to `mkq.WithKeepCompletedAge(age)`。0 = age-based
	// prune 無し (driver default を踏ませる)。
	//
	// Combine with KeepCompleted to cap by both count and age in
	// the same way BullMQ TS's `{age, count}` object form works
	// (BullMQ TS の `removeOnComplete: {age: 7d, count: 30}` 互換)。
	KeepCompletedAge time.Duration

	// KeepFailedAge is the failed-side analogue of KeepCompletedAge
	// (BullMQ `removeOnFail: {age: <seconds>}` 互換)。0 = age-based
	// prune 無し。
	KeepFailedAge time.Duration

	// MaxRetrySet distinguishes "MaxRetry left at default" from
	// "MaxRetry explicitly set to 0". MaxRetry=0 means no retries, which
	// differs from "use default", so callers like the cleanRemoteNotes
	// job that want zero retries must opt in explicitly.
	MaxRetrySet bool

	// BackoffType / BackoffDelay describe the retry backoff strategy.
	// BackoffType is "" (driver default), "fixed", "exponential", or
	// "custom"; BackoffDelay is the base delay (ignored for "custom").
	// mkq translates these to mkq.FixedBackoff / mkq.ExponentialBackoff /
	// mkq.CustomBackoff (the last defers delay computation to a worker-
	// registered strategy).
	//
	// 未設定の mkq は backoff 無し (= 遅延0で即時 retry) になるため、落ちて
	// いる配送先を連打し delayed bucket にも滞在しない。federation queue
	// (deliver / inbox) は必ず設定して hammering を防ぐ (#1405)。
	BackoffType  string
	BackoffDelay time.Duration

	// VerbatimJSONPayload is the caller's promise that the payload is a JSON
	// object written the way encoding/json writes it (no whitespace between
	// tokens, <, > and & escaped, valid UTF-8, nested no deeper than
	// MaxFramedJSONDepth). A driver that embeds payloads as JSON
	// (CompactJobPayloads) may then skip its own check. Set it with
	// WithVerbatimJSONPayload.
	//
	// 検査を飛ばすだけで、埋め込みに失敗したときの以前の形への切り替えは残る。
	// 約束が破れていても job は落ちないが、本文が json.Marshal に書き直されて
	// バイト列が変わりうるので、約束できる payload にだけ付ける。
	VerbatimJSONPayload bool
}

// EnqueueOption mutates EnqueueOptions; pass via Client.Enqueue.
type EnqueueOption func(*EnqueueOptions)

// WithQueue routes the task to the named queue.
func WithQueue(name string) EnqueueOption {
	return func(o *EnqueueOptions) { o.Queue = name }
}

// WithMaxRetry sets the maximum number of retries for the task. Use
// zero to disable retries entirely. Leaving it unset falls back to the
// driver default, which for mkq is "no retry" — deliver / inbox
// therefore get an explicit TS-compatible default from
// internal/server.buildPolicy (#1411).
func WithMaxRetry(n int) EnqueueOption {
	return func(o *EnqueueOptions) {
		o.MaxRetry = n
		o.MaxRetrySet = true
	}
}

// WithUnique sets a uniqueness TTL: the driver suppresses duplicate
// enqueues with the same task type + payload within the window.
func WithUnique(ttl time.Duration) EnqueueOption {
	return func(o *EnqueueOptions) { o.UniqueTTL = ttl }
}

// WithProcessIn delays processing of the task by the given duration.
func WithProcessIn(d time.Duration) EnqueueOption {
	return func(o *EnqueueOptions) { o.ProcessIn = d }
}

// WithVerbatimJSONPayload marks the payload as a JSON object written the
// way encoding/json writes it (EnqueueOptions.VerbatimJSONPayload).
//
// 1 つの投稿を inbox の数だけ積む配送では、同じ検査を宛先ごとに繰り返す
// (73KB の本文で 1 件あたり約 140µs、#3614)。payload を自分で組み立てる側は
// 形を知っているので、検査を省けるよう伝える。
func WithVerbatimJSONPayload() EnqueueOption {
	return func(o *EnqueueOptions) { o.VerbatimJSONPayload = true }
}

// Backoff type constants. "fixed" / "exponential" are BullMQ built-ins
// (computed driver-side from BackoffDelay); "custom" defers the delay
// computation to a worker-registered strategy (BullMQ's
// settings.backoffStrategy path) and ignores BackoffDelay.
const (
	BackoffFixed       = "fixed"
	BackoffExponential = "exponential"
	BackoffCustom      = "custom"
)

// WithBackoff sets the retry backoff strategy. typ is "fixed",
// "exponential", or "custom"; delay is the base delay (ignored for
// "custom"). Without a backoff mkq retries immediately, so federation
// queues must set one to avoid hammering unreachable hosts (#1405).
func WithBackoff(typ string, delay time.Duration) EnqueueOption {
	return func(o *EnqueueOptions) {
		o.BackoffType = typ
		o.BackoffDelay = delay
	}
}

// WithFederationBackoff applies the retry backoff used by the deliver /
// inbox queues: BullMQ "custom" strategy, whose delay is computed by the
// worker-registered Misskey httpRelatedBackoff ((2^n-1)*60s, capped at 8h,
// +0..20% jitter). enqueue 側は `{type:"custom"}` だけを保存し、実際の delay
// は mkqdriver の WithBackoffStrategy が算出する。これで TS と drop-in 一致
// する (#1405 / #1406, mkq#67)。
func WithFederationBackoff() EnqueueOption {
	return func(o *EnqueueOptions) {
		o.BackoffType = BackoffCustom
	}
}

// WithKeepFailed bounds the size of the failed bucket / ZSET for this
// task's queue: when the failed bucket exceeds n entries, the oldest
// ones are pruned automatically. n==0 explicitly disables retention
// (= unlimited accumulation, matches the historical behaviour).
//
// 主な使い道: inbox / deliver 等の連合 queue で transient failure が
// 永続蓄積するのを防ぐ。BullMQ の `removeOnFail: N` と意味同等。
//
// mkqdriver は `n > 0` のときだけ `mkq.WithKeepFailed(n)` を AddJob に
// 渡す。注意: mkq native の `WithKeepFailed(0)` は **「即時削除」**
// (BullMQ `removeOnFail: true` 相当) で driver-neutral semantic と
// 真逆なので、driver translation 側で 0 は skip する。
func WithKeepFailed(n int) EnqueueOption {
	return func(o *EnqueueOptions) {
		o.KeepFailed = n
		o.KeepFailedSet = true
	}
}

// WithKeepCompleted bounds the size of the completed bucket / ZSET for
// this task's queue, mirroring WithKeepFailed's semantics for completed
// jobs. n==0 explicitly disables retention (= unlimited accumulation).
//
// 主な使い道: BullMQ default の「completed job 無制限保持」を防ぐ。
// inbox / deliver / db / push / webhook queue の completed bookkeeping
// が時間と共に redis memory を蝕む実害があるため (#1193 で UDS 観測)。
// BullMQ の `removeOnComplete: N` と意味同等。
//
// mkqdriver は `n > 0` のときだけ `mkq.WithKeepCompleted(n)` を AddJob
// に渡す。注意: mkq native の `WithKeepCompleted(0)` は **「即時削除」**
// (BullMQ `removeOnComplete: true` 相当) で driver-neutral semantic と
// 真逆なので、driver translation 側で 0 は skip する。
func WithKeepCompleted(n int) EnqueueOption {
	return func(o *EnqueueOptions) {
		o.KeepCompleted = n
		o.KeepCompletedSet = true
	}
}

// WithKeepCompletedAge drops completed jobs older than age, mirroring
// mkq's `WithKeepCompletedAge` (BullMQ `removeOnComplete: {age: <sec>}`
// 互換)。WithKeepCompleted と併用すると count / age の両条件で prune
// される (BullMQ TS の `removeOnComplete: {age: 7d, count: 30}` 互換)。
//
// mkqdriver は `age > 0` のときだけ `mkq.WithKeepCompletedAge(age)` を
// AddJob に渡す。
func WithKeepCompletedAge(age time.Duration) EnqueueOption {
	return func(o *EnqueueOptions) { o.KeepCompletedAge = age }
}

// WithKeepFailedAge is the failed-side analogue of WithKeepCompletedAge
// (BullMQ `removeOnFail: {age: <sec>}` 互換)。WithKeepFailed と併用
// すると count / age の両条件で prune される。
func WithKeepFailedAge(age time.Duration) EnqueueOption {
	return func(o *EnqueueOptions) { o.KeepFailedAge = age }
}

// ApplyEnqueueOptions folds the variadic options into a single
// EnqueueOptions. Drivers call this to obtain a populated struct
// before mapping to their native API.
func ApplyEnqueueOptions(opts []EnqueueOption) EnqueueOptions {
	var o EnqueueOptions
	for _, fn := range opts {
		fn(&o)
	}
	return o
}
