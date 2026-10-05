package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
)

// fakeDriver is a minimal driver.Driver implementation that returns
// canned values for WorkerCount and routes Inspector calls to a backing
// fakeInspector. Other Driver methods panic; tests should not exercise them.
type fakeDriver struct {
	workers   map[string]int
	inspector *fakeInspector
}

func (f *fakeDriver) Client() driver.Client       { panic("fakeDriver: Client() not implemented") }
func (f *fakeDriver) Server() driver.Server       { panic("fakeDriver: Server() not implemented") }
func (f *fakeDriver) Inspector() driver.Inspector { return f.inspector }
func (f *fakeDriver) Scheduler() driver.Scheduler { panic("fakeDriver: Scheduler() not implemented") }
func (f *fakeDriver) Close() error                { return nil }
func (f *fakeDriver) WorkerCount(qname string) int {
	if v, ok := f.workers[qname]; ok {
		return v
	}
	return 0
}
func (f *fakeDriver) Resize(qname string, n int) error {
	if f.workers == nil {
		f.workers = map[string]int{}
	}
	f.workers[qname] = n
	return nil
}

type fakeInspector struct {
	pendingByQueue map[string]int
	errByQueue     map[string]error
}

func (i *fakeInspector) Queues() ([]string, error) { return nil, nil }
func (i *fakeInspector) GetQueueInfo(qname string) (*driver.InspectorInfo, error) {
	if err, ok := i.errByQueue[qname]; ok {
		return nil, err
	}
	pending, ok := i.pendingByQueue[qname]
	if !ok {
		return &driver.InspectorInfo{Queue: qname}, nil
	}
	return &driver.InspectorInfo{Queue: qname, Pending: pending}, nil
}

// DispatchableCount mirrors GetQueueInfo. この fake は Prometheus 経路の検証用で
// autoscaler は通らないが、driver.Inspector を満たすために要る。
func (i *fakeInspector) DispatchableCount(qname string) (int, error) {
	if err, ok := i.errByQueue[qname]; ok {
		return 0, err
	}
	return i.pendingByQueue[qname], nil
}

func (i *fakeInspector) DeleteTask(qname, taskID string) error           { return nil }
func (i *fakeInspector) DeleteAllPendingTasks(qname string) (int, error) { return 0, nil }
func (i *fakeInspector) PauseQueue(qname string) error                   { return nil }
func (i *fakeInspector) UnpauseQueue(qname string) error                 { return nil }
func (i *fakeInspector) RunTask(qname, taskID string) error              { return nil }
func (i *fakeInspector) ListPendingTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeInspector) ListActiveTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeInspector) ListCompletedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeInspector) ListFailedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeInspector) ListScheduledTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeInspector) ListRetryTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeInspector) ListDelayedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeInspector) GetTaskInfo(qname, taskID string) (*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeInspector) GetTaskLogs(string, string, int64, int64) ([]string, int64, error) {
	return nil, 0, nil
}

func (i *fakeInspector) QueueMetrics(qname, kind string) (*driver.MetricsResult, error) {
	return nil, nil
}
func (i *fakeInspector) Close() error { return nil }

// TestNew_PreInitializesZeroSeries verifies that every standard queue label
// has a zero-valued series after New (for push-mode metrics), so Prometheus
// graphs don't show "no data" gaps before the first observation. The pull-
// mode metrics (workers_active / queue_pending) are emitted by Collect at
// scrape time so they don't need pre-init here.
func TestNew_PreInitializesZeroSeries(t *testing.T) {
	m := New()
	for _, q := range standardQueues {
		assert.Equal(t, 0.0, testutil.ToFloat64(m.ScaleEventsTotal.WithLabelValues(q, "up")))
		assert.Equal(t, 0.0, testutil.ToFloat64(m.ScaleEventsTotal.WithLabelValues(q, "down")))
	}
}

// TestRegister_AttachesAllCollectors verifies that Register succeeds against
// a fresh Registry and registering twice fails (Prometheus contract).
func TestRegister_AttachesAllCollectors(t *testing.T) {
	r := prometheus.NewRegistry()
	m := New()
	require.NoError(t, m.Register(r))

	// 2 度目の登録は AlreadyRegisteredError を返す。
	err := m.Register(r)
	require.Error(t, err)
	var already prometheus.AlreadyRegisteredError
	assert.True(t, errors.As(err, &already), "expected AlreadyRegisteredError, got %T", err)
}

// TestBindDriver_CollectsAtScrapeTime verifies that workers_active and
// queue_pending values are read from the driver each scrape (= reflect
// driver state at /metrics call time, not at BindDriver time).
func TestBindDriver_CollectsAtScrapeTime(t *testing.T) {
	d := &fakeDriver{
		workers: map[string]int{"deliver": 32, "inbox": 16},
		inspector: &fakeInspector{
			pendingByQueue: map[string]int{"deliver": 100, "inbox": 5},
		},
	}

	m := New()
	m.BindDriver(d)
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))

	body := scrape(t, r)
	assert.Contains(t, body, `mk_job_workers_active{queue="deliver"} 32`)
	assert.Contains(t, body, `mk_job_workers_active{queue="inbox"} 16`)
	assert.Contains(t, body, `mk_job_queue_pending{queue="deliver"} 100`)
	assert.Contains(t, body, `mk_job_queue_pending{queue="inbox"} 5`)

	// Driver の WorkerCount を後から変更 → 再 scrape で反映される。
	d.workers["deliver"] = 64
	body = scrape(t, r)
	assert.Contains(t, body, `mk_job_workers_active{queue="deliver"} 64`)
}

// TestBindDriver_NilIsNoOp verifies that BindDriver(nil) does not panic and
// leaves the pull collector unset (= /metrics still works but omits
// workers_active / queue_pending series).
func TestBindDriver_NilIsNoOp(t *testing.T) {
	m := New()
	m.BindDriver(nil)
	assert.Nil(t, m.pullCollector)
}

// TestBindDriver_InspectorErrorReturnsZero verifies that scrape time errors
// from Inspector.GetQueueInfo are swallowed at the gauge layer (reported as
// 0 to keep /metrics responsive) AND that mk_job_scrape_errors_total counts
// each failure so operators can alert on the rate.
func TestBindDriver_InspectorErrorReturnsZero(t *testing.T) {
	d := &fakeDriver{
		workers: map[string]int{"deliver": 8},
		inspector: &fakeInspector{
			errByQueue: map[string]error{"deliver": errors.New("redis timeout")},
		},
	}
	m := New()
	m.BindDriver(d)
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))

	// First scrape: gauge degrades to 0, scrape_errors increments to 1.
	body := scrape(t, r)
	assert.Contains(t, body, `mk_job_workers_active{queue="deliver"} 8`)
	assert.Contains(t, body, `mk_job_queue_pending{queue="deliver"} 0`)
	// 同 scrape の body 内にも error counter が反映されていることを assert
	// (MustNewConstMetric 経路なので scrape goroutine 内で値を capture)。
	assert.Contains(t, body, `mk_job_scrape_errors_total{kind="queue_pending",queue="deliver"} 1`)
	assert.Equal(t, uint64(1), m.ScrapeErrorCount("deliver", "queue_pending"))

	// Second scrape: counter increments again, body reflects the new value.
	body = scrape(t, r)
	assert.Contains(t, body, `mk_job_scrape_errors_total{kind="queue_pending",queue="deliver"} 2`)
	assert.Equal(t, uint64(2), m.ScrapeErrorCount("deliver", "queue_pending"))

	// Successful queues do not increment scrape_errors.
	assert.Equal(t, uint64(0), m.ScrapeErrorCount("inbox", "queue_pending"))
}

// TestBindDriver_ReplacesPreviousBinding verifies that calling BindDriver
// twice replaces the previous binding (= last caller wins), so re-wiring on
// driver swap behaves predictably.
func TestBindDriver_ReplacesPreviousBinding(t *testing.T) {
	m := New()
	first := &fakeDriver{workers: map[string]int{"deliver": 1}, inspector: &fakeInspector{}}
	second := &fakeDriver{workers: map[string]int{"deliver": 99}, inspector: &fakeInspector{}}
	m.BindDriver(first)
	m.BindDriver(second)

	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))
	body := scrape(t, r)
	assert.Contains(t, body, `mk_job_workers_active{queue="deliver"} 99`)
}

// TestBindDriver_PreservesScrapeErrorsAcrossRebind verifies that the
// accumulated scrape_errors_total counter survives a BindDriver re-bind
// (driver swap mid-life), because scrapeErrs lives on Metrics not the
// per-bind driverCollector. Without this, an operator hot-swapping
// drivers (via config reload) would silently lose
// alerting history reachable via Metrics.ScrapeErrorCount (#1136
// follow-up).
//
// Note: prometheus.Registry doesn't unregister the old Collector on
// re-bind — callers needing the new driver visible via /metrics scrape
// body must also re-Register. This test only guards the persistence of
// `ScrapeErrorCount` accessor, which is the in-process API used for
// programmatic monitoring (e.g. an internal admin endpoint or
// integration test).
func TestBindDriver_PreservesScrapeErrorsAcrossRebind(t *testing.T) {
	m := New()
	failing := &fakeDriver{
		workers: map[string]int{"deliver": 1},
		inspector: &fakeInspector{
			errByQueue: map[string]error{"deliver": errors.New("redis timeout")},
		},
	}
	m.BindDriver(failing)
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))
	scrape(t, r) // increments count to 1
	require.Equal(t, uint64(1), m.ScrapeErrorCount("deliver", "queue_pending"))

	// Re-bind to a different driver — accumulated count must be preserved
	// because the new driverCollector points to the SAME m.scrapeErrs map.
	healthy := &fakeDriver{workers: map[string]int{"deliver": 5}, inspector: &fakeInspector{}}
	m.BindDriver(healthy)
	assert.Equal(t, uint64(1), m.ScrapeErrorCount("deliver", "queue_pending"),
		"scrape error count must persist across BindDriver re-bind")
}

// TestPullCollector_ConcurrentScrapesIncrementAtomically verifies that
// simultaneous Prometheus scrapers (= multi-pod monitoring / replicated
// Prometheus servers) increment scrape_errors_total without lost updates.
// The sync.Map + *atomic.Uint64 pattern relies on standard-library
// contracts; this is a wiring smoke test (#1136 follow-up review #4).
func TestPullCollector_ConcurrentScrapesIncrementAtomically(t *testing.T) {
	d := &fakeDriver{
		workers: map[string]int{"deliver": 1},
		inspector: &fakeInspector{
			errByQueue: map[string]error{"deliver": errors.New("redis timeout")},
		},
	}
	m := New()
	m.BindDriver(d)
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))

	const goroutines = 8
	const scrapesPerGoroutine = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < scrapesPerGoroutine; j++ {
				scrape(t, r)
			}
		}()
	}
	wg.Wait()
	// 各 scrape で deliver の Inspector が 1 回 fail → 8 * 25 = 200 回 increment。
	assert.Equal(t, uint64(goroutines*scrapesPerGoroutine), m.ScrapeErrorCount("deliver", "queue_pending"))
}

// TestObserveHistograms verifies that DispatchWaitSeconds and ProcessingSeconds
// accept observations and record counts. Buckets are not enumerated exhaustively
// here; we only verify the count side moves so future hook PRs (#1124) can
// rely on the histogram being functional.
func TestObserveHistograms(t *testing.T) {
	m := New()
	m.DispatchWaitSeconds.WithLabelValues("deliver").Observe(0.005)
	m.ProcessingSeconds.WithLabelValues("deliver", "success").Observe(0.5)
	m.ProcessingSeconds.WithLabelValues("deliver", "failure").Observe(1.5)

	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))
	body := scrape(t, r)
	assert.Contains(t, body, `mk_job_dispatch_wait_seconds_count{queue="deliver"} 1`)
	assert.Contains(t, body, `mk_job_processing_seconds_count{queue="deliver",status="success"} 1`)
	assert.Contains(t, body, `mk_job_processing_seconds_count{queue="deliver",status="failure"} 1`)
}

// TestScaleEventsTotalCounter verifies that the counter can be incremented
// (sub-issue #1125 will wire actual increments from the controller).
func TestScaleEventsTotalCounter(t *testing.T) {
	m := New()
	m.ScaleEventsTotal.WithLabelValues("deliver", "up").Inc()
	m.ScaleEventsTotal.WithLabelValues("deliver", "up").Inc()
	m.ScaleEventsTotal.WithLabelValues("deliver", "down").Inc()
	assert.Equal(t, 2.0, testutil.ToFloat64(m.ScaleEventsTotal.WithLabelValues("deliver", "up")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ScaleEventsTotal.WithLabelValues("deliver", "down")))
}

// scrape calls the registry's HTTP handler and returns the response body as
// a string. Used to assert text-format output rather than scanning collectors
// directly (matches how Prometheus actually consumes the endpoint).
func scrape(t *testing.T, r *prometheus.Registry) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(r, promhttp.HandlerOpts{}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

// quarantineFakeDriver adds the optional quarantine surface to fakeDriver.
type quarantineFakeDriver struct {
	*fakeDriver
	quarantined map[string]int
}

func (d *quarantineFakeDriver) QuarantinedWorkerCount(qname string) int {
	return d.quarantined[qname]
}

// **mkqdriver.Driver がこの面を満たすことを固定する。** quarantineReporter は
// 構造的インターフェースなので、Driver 側のメソッド名が変わっても型検査は
// 通り、mk_job_workers_quarantined が黙って常時 0 になる (#2657 で足した
// 監視が無言で死ぬ)。
var _ quarantineReporter = (*mkqdriver.Driver)(nil)

func TestCollect_QuarantinedGauge(t *testing.T) {
	m := New()
	m.BindDriver(&quarantineFakeDriver{
		fakeDriver:  &fakeDriver{workers: map[string]int{"inbox": 4}, inspector: &fakeInspector{}},
		quarantined: map[string]int{"inbox": 3},
	})
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))

	body := scrape(t, r)
	assert.Contains(t, body, `mk_job_workers_quarantined{queue="inbox"} 3`)
	assert.Contains(t, body, `mk_job_workers_quarantined{queue="deliver"} 0`)
}

func TestCollect_QuarantinedGaugeZeroWithoutSupport(t *testing.T) {
	// quarantine を追跡しない driver では常に 0。scrape が落ちたり
	// gauge ごと消えたりしないこと。
	m := New()
	m.BindDriver(&fakeDriver{workers: map[string]int{"inbox": 4}, inspector: &fakeInspector{}})
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))

	body := scrape(t, r)
	assert.Contains(t, body, `mk_job_workers_quarantined{queue="inbox"} 0`)
}

// TestStandardQueues_CoversEveryQueue pins the label set against the queue
// constants. **ここから漏れたキューは /metrics に一切出ない**ので、
// キューを足したときに監視だけ置き去りになるのを防ぐ。
func TestStandardQueues_CoversEveryQueue(t *testing.T) {
	want := []string{
		queue.QueueName,
		queue.InboxQueueName,
		queue.RelationshipQueueName,
		queue.ExportQueueName,
		queue.PushQueueName,
		queue.WebhookQueueName,
		queue.ObjectStorageQueueName,
		queue.MaintenanceQueueName,
	}
	assert.ElementsMatch(t, want, standardQueues)
	// driver が実際に立てるキュー一覧と一致していること。片方だけ増えると
	// 監視が置き去りになる。
	assert.ElementsMatch(t, mkqdriver.QueueNames, standardQueues,
		"every queue the driver spawns workers for must get a metrics series")
}

// **mkqdriver.Driver がこの面を満たすことを固定する。** 構造的インターフェース
// なのでメソッド名を変えても型検査は通り、gauge が黙って常時 0 になる (#2658)。
var _ abandonReporter = (*mkqdriver.Driver)(nil)

type abandonFakeDriver struct {
	*fakeDriver
	current map[string]int
	total   map[string]uint64
}

func (d *abandonFakeDriver) AbandonedHandlerCount(qname string) (int, uint64) {
	return d.current[qname], d.total[qname]
}

func TestCollect_AbandonedHandlerGauges(t *testing.T) {
	m := New()
	m.BindDriver(&abandonFakeDriver{
		fakeDriver: &fakeDriver{workers: map[string]int{"inbox": 4}, inspector: &fakeInspector{}},
		current:    map[string]int{"inbox": 2},
		total:      map[string]uint64{"inbox": 7},
	})
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))

	body := scrape(t, r)
	assert.Contains(t, body, `mk_job_handlers_abandoned{queue="inbox"} 2`)
	assert.Contains(t, body, `mk_job_handler_abandonments_total{queue="inbox"} 7`)
	assert.Contains(t, body, `mk_job_handlers_abandoned{queue="deliver"} 0`)
}

func TestCollect_AbandonedGaugesZeroWithoutSupport(t *testing.T) {
	m := New()
	m.BindDriver(&fakeDriver{workers: map[string]int{"inbox": 4}, inspector: &fakeInspector{}})
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))

	body := scrape(t, r)
	assert.Contains(t, body, `mk_job_handlers_abandoned{queue="inbox"} 0`)
	assert.Contains(t, body, `mk_job_handler_abandonments_total{queue="inbox"} 0`)
}

// **gauge と counter が同じ family 名に潰れないことを固定する。**
// OpenMetrics encoder は counter の family 名から `_total` を落とすので、
// `mk_job_handlers_abandoned` + `..._total` の組は同じ family に `# TYPE` を
// 2 行出してしまう (spec 違反)。`/metrics` は EnableOpenMetrics なので実害あり。
func TestCollect_AbandonedMetricFamiliesDoNotCollide(t *testing.T) {
	m := New()
	m.BindDriver(&abandonFakeDriver{
		fakeDriver: &fakeDriver{workers: map[string]int{"inbox": 1}, inspector: &fakeInspector{}},
		current:    map[string]int{"inbox": 1},
		total:      map[string]uint64{"inbox": 1},
	})
	r := prometheus.NewRegistry()
	require.NoError(t, m.Register(r))

	// **Gather() では検出できない。** family 名を落とすのは OpenMetrics の
	// encoder なので、実際にその形式でレンダリングして `# TYPE` の重複を見る。
	srv := httptest.NewServer(promhttp.HandlerFor(r, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/openmetrics-text;version=1.0.0")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	types := map[string]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		types[strings.Fields(line)[2]]++
	}
	require.NotEmpty(t, types)
	for name, n := range types {
		assert.Equal(t, 1, n, "metric family %q must declare its type once", name)
	}
	assert.Contains(t, types, "mk_job_handlers_abandoned")
	assert.Contains(t, types, "mk_job_handler_abandonments")
}
