// Package metrics exposes Prometheus metrics for the job queue subsystem.
//
// 設計合意は docs/design/auto-scale-job-workers.md §6.1 を参照。本パッケージ
// は #1122 の最小スコープ (controller / Resize 配線前) なので以下の役割分担:
//
//   - mk_job_workers_active{queue} (gauge): scrape 時に driver.WorkerCount を
//     都度 read。auto-scale で動的変化する将来も追従できる。mkq driver では
//     handler が閾値を超えて戻ってこない worker を除いた**生存数**を返す
//     (#2657)。goroutine 数ではない。
//   - mk_job_handlers_abandoned{queue} (gauge) /
//     mk_job_handler_abandonments_total{queue} (counter): dispatcher が
//     待つのをやめた handler の数 (mkq driver のみ、他は常に 0)。**gauge が
//     0 に戻らないなら handler が本当に戻ってこない経路を踏んでいる** —
//     その goroutine は DB 接続などを掴んだまま残る (#2658)。
//   - mk_job_workers_quarantined{queue} (gauge): 閾値超過で pool の外に
//     退けてある worker 数 (mkq driver のみ、他は常に 0)。**0 でない状態が
//     続いていたら handler がブロックしている。** #2657 の本番障害はこれが
//     可視化されておらず 1 日以上気付けなかった。
//   - mk_job_queue_pending{queue} (gauge): scrape 時に Inspector.GetQueueInfo
//     を呼ぶ。中身は Redis ZCARD 相当で軽量。
//   - mk_job_dispatch_wait_seconds{queue} (histogram): declare のみ。
//     enqueue → dispatch hook は #1124 mkqdriver Resize PR で配線予定。
//   - mk_job_processing_seconds{queue,status} (histogram): declare のみ。
//     handler 前後の observation hook は #1124 で配線予定。
//   - mk_job_scale_events_total{queue,direction} (counter): declare のみ。
//     scale event 発火は #1125 controller wiring PR で配線予定。
//
// 5 metric 全部を一度に declare しておくことで、後続 PR が hook を追加するとき
// metric 名 / label 体系の議論を再度しなくて済む (前方互換 contract を本 PR で
// 確定する)。
package metrics

import (
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// standardQueues is the label set every pull-mode gauge is emitted for,
// and the set that gets a zero-valued series at startup (Prometheus
// convention: zero-init avoids "no data" gaps in graphs).
//
// **`internal/queue` のキュー定数と揃える。** ここから漏れたキューは
// /metrics に一切出ない。relationship (#2403) / objectStorage (#2325) /
// maintenance は長く漏れていた。表示だけの問題に見えていたが、
// relationship は詰まり検出の対象キューでもあるため
// mk_job_workers_quarantined が出ず、#2657 で足した監視がそのキューだけ
// 効かない状態になっていたので揃えた (系列が 3 つ増える)。
var standardQueues = []string{
	queue.QueueName,
	queue.InboxQueueName,
	queue.RelationshipQueueName,
	queue.ExportQueueName,
	queue.PushQueueName,
	queue.WebhookQueueName,
	queue.ObjectStorageQueueName,
	queue.MaintenanceQueueName,
}

// Metrics bundles every Prometheus collector owned by the queue subsystem.
// Construct with New, optionally call BindDriver to wire the pull-based
// gauges, then Register against a prometheus.Registry.
//
// 設計判断: prometheus.DefaultRegisterer を直接触らず、Server スコープの
// Registry を新規作成して inject する方式 (testing / 多重 instance 起動時に
// global 汚染を避けるため、`testutil` で使う sub-Registry pattern と整合)。
type Metrics struct {
	// pullCollector holds workers_active / queue_pending which are read
	// from the driver at scrape time. nil until BindDriver is called.
	pullCollector *driverCollector

	// Push-mode collectors below. Histograms / counters are observed
	// inline by call sites (driver hooks / controller). HistogramVec /
	// CounterVec keep their accumulated values until next scrape.
	DispatchWaitSeconds *prometheus.HistogramVec
	ProcessingSeconds   *prometheus.HistogramVec
	ScaleEventsTotal    *prometheus.CounterVec

	// scrapeErrs is the (queue, kind) → cumulative count map. Lives on
	// Metrics (not driverCollector) so accumulated counts persist across
	// BindDriver re-bind. driverCollector holds a pointer to this map so
	// Inc + emit both touch the same series.
	scrapeErrs sync.Map // map[scrapeErrKey]*atomic.Uint64
}

// ScrapeErrorCount returns the cumulative count of /metrics scrape failures
// for the (queue, kind) pair, or 0 if no errors have been observed.
// Persisted on Metrics so re-binding the driver via BindDriver does not
// reset the count.
func (m *Metrics) ScrapeErrorCount(queue, kind string) uint64 {
	return loadScrapeError(&m.scrapeErrs, scrapeErrKey{queue: queue, kind: kind})
}

// New constructs the Metrics bundle (no registration, no driver binding).
//
// Histogram buckets are chosen for typical mk-go job latencies:
//   - dispatch_wait: 1ms..16s (covers normal idle dispatch through queue backed-up)
//   - processing: 10ms..160s (covers fast Redis touches through slow federation deliver)
func New() *Metrics {
	m := &Metrics{
		DispatchWaitSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "mk",
			Subsystem: "job",
			Name:      "dispatch_wait_seconds",
			Help:      "Seconds a job waited between enqueue and worker dispatch.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 4, 8), // 1ms .. ~16s
		}, []string{"queue"}),
		ProcessingSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "mk",
			Subsystem: "job",
			Name:      "processing_seconds",
			Help:      "Seconds a worker spent processing a job.",
			Buckets:   prometheus.ExponentialBuckets(0.01, 4, 8), // 10ms .. ~160s
		}, []string{"queue", "status"}),
		ScaleEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "mk",
			Subsystem: "job",
			Name:      "scale_events_total",
			Help:      "Total auto-scale events triggered, by queue and direction (up/down).",
		}, []string{"queue", "direction"}),
	}
	// Pre-init zero-valued series for push-mode metrics so Prometheus
	// dashboards show a continuous line instead of "no data". Pull-mode
	// metrics (workers_active / queue_pending / scrape_errors_total) don't
	// need pre-init because driverCollector.Collect always emits a value for
	// every standardQueues entry on each scrape.
	for _, q := range standardQueues {
		m.DispatchWaitSeconds.WithLabelValues(q)
		m.ProcessingSeconds.WithLabelValues(q, "success")
		m.ProcessingSeconds.WithLabelValues(q, "failure")
		m.ScaleEventsTotal.WithLabelValues(q, "up")
		m.ScaleEventsTotal.WithLabelValues(q, "down")
	}
	return m
}

// BindDriver wires the pull-based gauges (workers_active / queue_pending) to
// the given driver.Driver. Calling BindDriver multiple times replaces the
// previous binding (= last caller wins). Call order vs Register is free —
// the new driverCollector owns its own scrape error state internally so
// scrape errors are emitted from within the same Collect goroutine as the
// gauges (= no race vs a separately-registered CounterVec; #1136 follow-up).
//
// Gauges are evaluated lazily on each scrape, so a non-running driver
// (Server not yet Start()ed) shows zero without erroring (driver.WorkerCount
// returns 0 in that case).
func (m *Metrics) BindDriver(d driver.Driver) {
	if d == nil {
		m.pullCollector = nil
		return
	}
	m.pullCollector = &driverCollector{driver: d, scrapeErrs: &m.scrapeErrs}
}

// Register attaches all owned collectors to r. Returns the first error to
// occur; callers should treat any error as a startup failure so the operator
// notices duplicate registration (= bug). Idempotent registration is NOT
// supported by prometheus.Registry, so call this once per Registry.
func (m *Metrics) Register(r prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		m.DispatchWaitSeconds,
		m.ProcessingSeconds,
		m.ScaleEventsTotal,
	}
	if m.pullCollector != nil {
		collectors = append(collectors, m.pullCollector)
	}
	for _, c := range collectors {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// driverCollector implements prometheus.Collector for the pull-based metrics
// (workers_active / queue_pending / scrape_errors_total). All 3 metrics
// share a single Collector so Describe / Collect execute one pass over the
// queue list, AND scrape errors are accumulated + emitted from within the
// same Collect goroutine — emitting via MustNewConstMetric captures the
// counter value synchronously, avoiding the race that existed when
// scrape_errors_total was a separate CounterVec (its Counter.Write was
// called LATER by prometheus.Registry.Gather's main goroutine, after a
// peer Collector's Inc had already mutated the counter — off-by-one
// observation in the same scrape; #1136 follow-up).
//
// At scrape time, Collect calls driver.WorkerCount and Inspector.GetQueueInfo
// for every standardQueues entry. Errors from Inspector are swallowed at the
// gauge layer (reported as 0 to keep /metrics responsive under transient
// Redis failures) BUT the failure is also counted via
// `mk_job_scrape_errors_total{kind="queue_pending"}` so operators can alert
// on the failure rate without having to watch the gauge for "suspicious zeros".
type driverCollector struct {
	driver driver.Driver

	// scrapeErrs is a pointer to the Metrics-owned map so accumulated
	// counts persist across BindDriver re-bind (driver swap). sync.Map +
	// *atomic.Uint64 lets concurrent scrapes (= simultaneous Prometheus
	// scrapers) increment safely without blocking each other.
	scrapeErrs *sync.Map // map[scrapeErrKey]*atomic.Uint64
}

// scrapeErrKey identifies a single scrape-error counter series. kind is the
// upstream label ("queue_pending" for inspector failures; future kinds may
// cover other pull sources).
type scrapeErrKey struct {
	queue, kind string
}

// incScrapeError increments the cumulative count for (queue, kind) on the
// given sync.Map. Safe for concurrent invocation.
func incScrapeError(m *sync.Map, key scrapeErrKey) {
	v, _ := m.LoadOrStore(key, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

// loadScrapeError returns the cumulative count for (queue, kind), 0 if the
// counter has never been incremented.
func loadScrapeError(m *sync.Map, key scrapeErrKey) uint64 {
	v, ok := m.Load(key)
	if !ok {
		return 0
	}
	return v.(*atomic.Uint64).Load()
}

var (
	// workersActiveDesc reports per-queue worker pool size on the mkq
	// backend. 詰まった worker は除外して数える (#2657。仕組みは
	// docs/design/auto-scale-job-workers.md の §5.1.1、metric 一覧は §6.1)。
	workersActiveDesc = prometheus.NewDesc(
		"mk_job_workers_active",
		"Number of workers per queue able to take work.",
		[]string{"queue"}, nil,
	)
	handlersAbandonedDesc = prometheus.NewDesc(
		"mk_job_handlers_abandoned",
		"Number of job handlers per queue the dispatcher gave up on that have still not returned (mkq backend only; always 0 elsewhere).",
		[]string{"queue"}, nil,
	)
	// **`mk_job_handlers_abandoned_total` にしない。** OpenMetrics encoder は
	// counter の family 名から `_total` を落とすので、gauge の
	// `mk_job_handlers_abandoned` と family 名が衝突し、同じ family に
	// `# TYPE` が 2 行出る (spec 違反)。`/metrics` は
	// `EnableOpenMetrics: true` なので実際に踏む。
	handlerAbandonmentsTotalDesc = prometheus.NewDesc(
		"mk_job_handler_abandonments_total",
		"Cumulative number of job handlers per queue the dispatcher gave up on (mkq backend only; always 0 elsewhere).",
		[]string{"queue"}, nil,
	)
	workersQuarantinedDesc = prometheus.NewDesc(
		"mk_job_workers_quarantined",
		"Number of workers per queue held outside the pool because their handler overran the stuck-worker threshold (mkq backend only; always 0 elsewhere).",
		[]string{"queue"}, nil,
	)
	queuePendingDesc = prometheus.NewDesc(
		"mk_job_queue_pending",
		"Number of pending jobs per queue (Redis ZCARD).",
		[]string{"queue"}, nil,
	)
	scrapeErrorsDesc = prometheus.NewDesc(
		"mk_job_scrape_errors_total",
		"Total Prometheus scrape errors per queue, by source kind (e.g. queue_pending = Inspector.GetQueueInfo failed). A non-zero rate here means the corresponding gauge is reporting stale/zero values; alert on this rather than only on the gauge values themselves.",
		[]string{"queue", "kind"}, nil,
	)
)

func (c *driverCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- workersActiveDesc
	ch <- workersQuarantinedDesc
	ch <- handlersAbandonedDesc
	ch <- handlerAbandonmentsTotalDesc
	ch <- queuePendingDesc
	ch <- scrapeErrorsDesc
}

// quarantineReporter is the optional driver-side surface behind
// mk_job_workers_quarantined. Only mkqdriver implements it; a driver
// without that state leaves the gauge at 0.
//
// **driver.Driver に足さない。** 動的な pool を持たない driver では常に 0 を
// 返すだけの stub になり、「対応した」ように見える面を増やしてしまう。
type quarantineReporter interface {
	QuarantinedWorkerCount(qname string) int
}

// abandonReporter is the optional driver-side surface behind
// mk_job_handlers_abandoned{,_total} (#2658). Same rationale as
// quarantineReporter for keeping it off driver.Driver.
type abandonReporter interface {
	AbandonedHandlerCount(qname string) (int, uint64)
}

// abandonedFor returns the driver's abandoned-handler counts for qname, or
// zeroes when the driver does not track them.
func abandonedFor(d driver.Driver, qname string) (int, uint64) {
	if r, ok := d.(abandonReporter); ok {
		return r.AbandonedHandlerCount(qname)
	}
	return 0, 0
}

// quarantinedFor returns the driver's quarantined worker count for qname,
// or 0 when the driver does not track it.
//
// **Driver 側だけを見る。** d.Server() は nil のとき Server を作る遅延
// コンストラクタなので、scrape のたびに副作用を起こすことになる。
func quarantinedFor(d driver.Driver, qname string) int {
	if r, ok := d.(quarantineReporter); ok {
		return r.QuarantinedWorkerCount(qname)
	}
	return 0
}

func (c *driverCollector) Collect(ch chan<- prometheus.Metric) {
	inspector := c.driver.Inspector()
	for _, q := range standardQueues {
		ch <- prometheus.MustNewConstMetric(
			workersActiveDesc, prometheus.GaugeValue,
			float64(c.driver.WorkerCount(q)), q,
		)
		ch <- prometheus.MustNewConstMetric(
			workersQuarantinedDesc, prometheus.GaugeValue,
			float64(quarantinedFor(c.driver, q)), q,
		)
		abandonedNow, abandonedTotal := abandonedFor(c.driver, q)
		ch <- prometheus.MustNewConstMetric(
			handlersAbandonedDesc, prometheus.GaugeValue, float64(abandonedNow), q,
		)
		ch <- prometheus.MustNewConstMetric(
			handlerAbandonmentsTotalDesc, prometheus.CounterValue, float64(abandonedTotal), q,
		)
		var pending int
		info, err := inspector.GetQueueInfo(q)
		switch {
		case err != nil:
			incScrapeError(c.scrapeErrs, scrapeErrKey{queue: q, kind: "queue_pending"})
		case info != nil:
			pending = info.Pending
		}
		ch <- prometheus.MustNewConstMetric(
			queuePendingDesc, prometheus.GaugeValue,
			float64(pending), q,
		)
	}
	// Emit scrape error counters AFTER all Inc calls above so the value
	// reflects this scrape's failures. MustNewConstMetric captures the
	// count synchronously within this goroutine — no race with the main
	// Gather goroutine reading via Counter.Write later. Zero series are
	// emitted for every standard queue so dashboards see a continuous line.
	for _, q := range standardQueues {
		ch <- prometheus.MustNewConstMetric(
			scrapeErrorsDesc, prometheus.CounterValue,
			float64(loadScrapeError(c.scrapeErrs, scrapeErrKey{queue: q, kind: "queue_pending"})),
			q, "queue_pending",
		)
	}
}
