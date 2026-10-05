package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// fakeMetricsDriver is a minimal driver.Driver used only by the /metrics
// wiring tests. Other methods panic so unintended calls surface fast.
type fakeMetricsDriver struct {
	workers   map[string]int
	pending   map[string]int
	pendErr   map[string]error
	inspector *fakeMetricsInspector
}

func newFakeMetricsDriver(workers, pending map[string]int) *fakeMetricsDriver {
	d := &fakeMetricsDriver{workers: workers, pending: pending}
	d.inspector = &fakeMetricsInspector{parent: d}
	return d
}

func (f *fakeMetricsDriver) Client() driver.Client {
	panic("fakeMetricsDriver: Client() not implemented")
}
func (f *fakeMetricsDriver) Server() driver.Server {
	panic("fakeMetricsDriver: Server() not implemented")
}
func (f *fakeMetricsDriver) Inspector() driver.Inspector { return f.inspector }
func (f *fakeMetricsDriver) Scheduler() driver.Scheduler {
	panic("fakeMetricsDriver: Scheduler() not implemented")
}
func (f *fakeMetricsDriver) Close() error { return nil }
func (f *fakeMetricsDriver) WorkerCount(qname string) int {
	if v, ok := f.workers[qname]; ok {
		return v
	}
	return 0
}
func (f *fakeMetricsDriver) Resize(qname string, n int) error {
	if f.workers == nil {
		f.workers = map[string]int{}
	}
	f.workers[qname] = n
	return nil
}

type fakeMetricsInspector struct {
	parent *fakeMetricsDriver
}

func (i *fakeMetricsInspector) Queues() ([]string, error) { return nil, nil }
func (i *fakeMetricsInspector) GetQueueInfo(qname string) (*driver.InspectorInfo, error) {
	if err, ok := i.parent.pendErr[qname]; ok {
		return nil, err
	}
	pending := i.parent.pending[qname]
	return &driver.InspectorInfo{Queue: qname, Pending: pending}, nil
}

// DispatchableCount mirrors GetQueueInfo. これらの fake は Prometheus 経路の
// 検証用で autoscaler は通らないが、driver.Inspector を満たすために要る。
func (i *fakeMetricsInspector) DispatchableCount(qname string) (int, error) {
	if err, ok := i.parent.pendErr[qname]; ok {
		return 0, err
	}
	return i.parent.pending[qname], nil
}

func (i *fakeMetricsInspector) DeleteTask(qname, taskID string) error {
	return nil
}
func (i *fakeMetricsInspector) DeleteAllPendingTasks(qname string) (int, error) {
	return 0, nil
}
func (i *fakeMetricsInspector) RunTask(qname, taskID string) error { return nil }
func (i *fakeMetricsInspector) PauseQueue(qname string) error      { return nil }
func (i *fakeMetricsInspector) UnpauseQueue(qname string) error    { return nil }
func (i *fakeMetricsInspector) ListPendingTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) ListActiveTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) ListCompletedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) ListFailedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) ListScheduledTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) ListRetryTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) ListDelayedTasks(qname string, page, pageSize int) ([]*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) GetTaskInfo(qname, taskID string) (*driver.TaskSummary, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) GetTaskLogs(string, string, int64, int64) ([]string, int64, error) {
	return nil, 0, nil
}

func (i *fakeMetricsInspector) QueueMetrics(qname, kind string) (*driver.MetricsResult, error) {
	return nil, nil
}
func (i *fakeMetricsInspector) Close() error { return nil }

// TestWireMetricsEndpoint_RegistersPullAndPushSeries exercises the full
// helper used by router.setupRoutes (= wireMetricsEndpoint → buildMetricsHandler).
// Verifies both push-mode declarations and pull-mode values from the driver
// appear in the /metrics response, catching regressions in:
//   - missing BindDriver call in router.go
//   - missing collector in metrics.Register
//   - wrong endpoint path or handler wrapper
func TestWireMetricsEndpoint_RegistersPullAndPushSeries(t *testing.T) {
	d := newFakeMetricsDriver(
		map[string]int{"deliver": 32, "inbox": 16},
		map[string]int{"deliver": 100, "inbox": 5},
	)

	e := echo.New()
	require.NoError(t, wireMetricsEndpoint(e, newQueueMetrics(d)))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	bodyStr := string(body)

	// Pull-mode metrics (= BindDriver wired correctly, driverCollector
	// emits a value for every standardQueues entry)
	assert.Contains(t, bodyStr, `mk_job_workers_active{queue="deliver"} 32`)
	assert.Contains(t, bodyStr, `mk_job_workers_active{queue="inbox"} 16`)
	assert.Contains(t, bodyStr, `mk_job_queue_pending{queue="deliver"} 100`)
	assert.Contains(t, bodyStr, `mk_job_queue_pending{queue="inbox"} 5`)

	// Push-mode metric declarations (HELP/TYPE = collector Registered)
	for _, want := range []string{
		"# TYPE mk_job_dispatch_wait_seconds histogram",
		"# TYPE mk_job_processing_seconds histogram",
		"# TYPE mk_job_scale_events_total counter",
		"# TYPE mk_job_scrape_errors_total counter",
	} {
		assert.Contains(t, bodyStr, want)
	}
}

// TestWireMetricsEndpoint_OpenMetricsFormat verifies that an Accept header
// requesting OpenMetrics format triggers the OpenMetrics text variant
// (rather than the default Prometheus text format). Confirms the
// `EnableOpenMetrics: true` option on promhttp.HandlerOpts is actually
// honoured end-to-end.
func TestWireMetricsEndpoint_OpenMetricsFormat(t *testing.T) {
	d := newFakeMetricsDriver(map[string]int{}, map[string]int{})

	e := echo.New()
	require.NoError(t, wireMetricsEndpoint(e, newQueueMetrics(d)))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	contentType := rec.Header().Get("Content-Type")
	assert.Contains(t, contentType, "application/openmetrics-text",
		"OpenMetrics scraper Accept should return OpenMetrics-text content-type, got %q", contentType)

	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	// OpenMetrics 仕様 (RFC) で body 末尾は `# EOF\n` で終わる。
	assert.Contains(t, string(body), "# EOF")
}

// TestBuildMetricsHandler_ReportsScrapeErrorOnInspectorFailure verifies that
// a failing Inspector triggers mk_job_scrape_errors_total{kind="queue_pending"}
// on the same /metrics response (= scrape error is observable in the very
// scrape that experienced the error, no extra round-trip needed). The
// "observable in the same scrape" invariant only holds because driverCollector
// emits scrape_errors_total via MustNewConstMetric from within its own
// Collect goroutine; a previous design with a separately-registered
// CounterVec lost the synchronization (Prometheus Registry.Gather runs
// Collect calls concurrently and reads Counter.Write later in the main
// goroutine, so a peer Collector's Inc could land after serialization).
// #1136 follow-up commit "Eliminate metrics scrape race".
func TestBuildMetricsHandler_ReportsScrapeErrorOnInspectorFailure(t *testing.T) {
	d := newFakeMetricsDriver(map[string]int{"deliver": 8}, map[string]int{})
	d.pendErr = map[string]error{"deliver": errors.New("redis timeout")}

	handler, err := buildMetricsHandler(newQueueMetrics(d))
	require.NoError(t, err)

	// First scrape: gauge degrades to 0, counter increments to 1.
	body := scrapeHandler(t, handler)
	assert.Contains(t, body, `mk_job_workers_active{queue="deliver"} 8`)
	assert.Contains(t, body, `mk_job_queue_pending{queue="deliver"} 0`)
	assert.Contains(t, body, `mk_job_scrape_errors_total{kind="queue_pending",queue="deliver"} 1`)

	// Second scrape: counter increments again.
	body = scrapeHandler(t, handler)
	assert.Contains(t, body, `mk_job_scrape_errors_total{kind="queue_pending",queue="deliver"} 2`)
}

func scrapeHandler(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}
