package paramshadow

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// maxKeys bounds the distinct divergence lines kept between two summaries.
// The keys come from the paramDefs and the handlers' literals, so the bound
// is not expected to be reached; past it, new keys are counted in one line
// per endpoint and kind with HandlerParam / HandlerCode set to overflowKey.
const maxKeys = 4096

const overflowKey = "(overflow)"

// divergenceKey is one line of the summary.
type divergenceKey struct {
	endpoint, kind, schemaPath, handlerParam, handlerCode string
	handlerStatus                                         int
}

// Recorder counts the events in memory and writes them with slog: one line
// per distinct divergence and one summary line, every interval (started by
// the first event after it has passed, off the request when SetAsync was
// called) and on Flush. Each summary covers the events
// since the previous one, so the totals of a period are the sums of its
// lines (also across processes).
type Recorder struct {
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time

	mu          sync.Mutex
	since       time.Time
	divergences map[divergenceKey]int64
	agreed      int64
	skipped     map[string]int64

	// next is the UnixNano after which the next event writes the summary.
	next atomic.Int64

	// async runs the periodic summary off the request; see SetAsync.
	async func(func()) bool
}

// SetAsync makes Record hand the periodic summary to run instead of writing
// it on the request's goroutine. run reports whether it started the
// function; when it did not (the server generation is stopping), Record
// writes the summary itself. Call it before the Recorder is used.
func (r *Recorder) SetAsync(run func(func()) bool) { r.async = run }

// DefaultInterval is the summary interval NewRecorder uses for a
// non-positive interval.
const DefaultInterval = 10 * time.Minute

// NewRecorder returns a Recorder that writes to logger every interval
// (DefaultInterval when interval is not positive).
func NewRecorder(logger *slog.Logger, interval time.Duration) *Recorder {
	if interval <= 0 {
		interval = DefaultInterval
	}
	r := &Recorder{logger: logger, interval: interval, now: time.Now}
	r.reset(r.now())
	return r
}

func (r *Recorder) reset(now time.Time) {
	r.since = now
	r.divergences = map[divergenceKey]int64{}
	r.agreed = 0
	r.skipped = map[string]int64{}
	r.next.Store(now.Add(r.interval).UnixNano())
}

// Record counts e, then writes the summary when the interval has passed.
func (r *Recorder) Record(e Event) {
	r.mu.Lock()
	switch e.Kind {
	case KindAgreed:
		r.agreed++
	case KindSkipped:
		r.skipped[e.Skip]++
	default:
		k := divergenceKey{
			endpoint: e.Endpoint, kind: e.Kind, schemaPath: e.SchemaPath,
			handlerParam: e.HandlerParam, handlerCode: e.HandlerCode, handlerStatus: e.HandlerStatus,
		}
		if _, ok := r.divergences[k]; !ok && len(r.divergences) >= maxKeys {
			k = divergenceKey{endpoint: e.Endpoint, kind: e.Kind, handlerParam: overflowKey, handlerCode: overflowKey}
		}
		r.divergences[k]++
	}
	r.mu.Unlock()

	now := r.now()
	next := r.next.Load()
	// 間隔を過ぎた後の最初の1件だけが書く(CompareAndSwapに勝った1件)。
	if now.UnixNano() < next || !r.next.CompareAndSwap(next, now.Add(r.interval).UnixNano()) {
		return
	}
	// 要約は最大で数千行になりうるので、リクエストの処理の中では書かない
	// (#3677のレビューL-2)。
	if r.async != nil && r.async(r.Flush) {
		return
	}
	r.Flush()
}

// Flush writes what was counted since the previous summary and starts a new
// period. It writes nothing when nothing was counted.
func (r *Recorder) Flush() {
	r.mu.Lock()
	now := r.now()
	since, divergences, agreed, skipped := r.since, r.divergences, r.agreed, r.skipped
	r.reset(now)
	r.mu.Unlock()

	var diverged, skippedTotal int64
	for _, n := range divergences {
		diverged += n
	}
	for _, n := range skipped {
		skippedTotal += n
	}
	if diverged+agreed+skippedTotal == 0 {
		return
	}

	keys := make([]divergenceKey, 0, len(divergences))
	for k := range divergences {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b divergenceKey) int {
		return cmp.Or(cmp.Compare(a.endpoint, b.endpoint), cmp.Compare(a.kind, b.kind),
			cmp.Compare(a.schemaPath, b.schemaPath), cmp.Compare(a.handlerParam, b.handlerParam),
			cmp.Compare(a.handlerCode, b.handlerCode), cmp.Compare(a.handlerStatus, b.handlerStatus))
	})
	for _, k := range keys {
		r.logger.Info("paramshadow divergence",
			"endpoint", k.endpoint,
			"kind", k.kind,
			"schemaPath", k.schemaPath,
			"handlerParam", k.handlerParam,
			"handlerStatus", k.handlerStatus,
			"handlerCode", k.handlerCode,
			"count", divergences[k],
		)
	}

	reasons := make([]string, 0, len(skipped))
	for reason := range skipped {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	skipAttrs := make([]any, 0, len(reasons))
	for _, reason := range reasons {
		skipAttrs = append(skipAttrs, slog.Int64(reason, skipped[reason]))
	}
	r.logger.Info("paramshadow summary",
		"since", since.UTC().Format(time.RFC3339),
		"seconds", int64(now.Sub(since).Seconds()),
		"agreed", agreed,
		"diverged", diverged,
		"skipped", skippedTotal,
		slog.Group("skippedBy", skipAttrs...),
	)
}

// Close writes what was counted since the previous summary, like Flush, when
// the server generation stops. The ctx is not used.
func (r *Recorder) Close(context.Context) { r.Flush() }
