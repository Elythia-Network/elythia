package server

import (
	"bytes"
	"context"
	"net/http"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// partsStartedByStartBackground lists the parts that have a start: the ones
// StartBackground starts and construction alone does not (#3703). The other
// parts of the list only have a stop.
//
// 部品を足して起動を持たせたら、こことdocs/architecture.md 4.3の起動の表を一緒に直す。
var partsStartedByStartBackground = map[string]bool{
	"pollExpiryWorker":            true,
	"fedRuleHits":                 true,
	"instanceTouchBuffer":         true,
	"sigCapBuffer":                true,
	"reactionFlushWorker":         true,
	"instanceCounterBuffer":       true,
	"deliveryHealth":              true,
	"inboxHealth":                 true,
	"retentionAggregateAtStartup": true,
	"userCacheLink":               true,
	"serverStatsPub":              true,
	"queueStatsPub":               true,
}

// wrapStarts replaces the start of every part of s that has one with a start
// that records the part's name and then calls the original start. It returns
// a function that reads what was recorded.
func wrapStarts(s *Server) func() []string {
	var mu sync.Mutex
	var started []string
	for i := range s.lifecycle.parts {
		p := s.lifecycle.parts[i]
		if p.start == nil {
			continue
		}
		s.lifecycle.parts[i].start = func() {
			mu.Lock()
			started = append(started, p.name)
			mu.Unlock()
			p.start()
		}
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), started...)
	}
}

// partGoroutineMarkers names, for each long-running goroutine a part starts,
// a function on its stack. retentionAggregateAtStartup is not here: it runs
// once and returns (TestNewServer_ConstructionStartsNoPart sees it in the DB).
var partGoroutineMarkers = map[string]string{
	"instanceTouchBuffer":   "instance.(*TouchBuffer).runLoop(",
	"sigCapBuffer":          "instance.(*CapabilityBuffer).runLoop(",
	"instanceCounterBuffer": "instance.(*CounterBuffer).runLoop(",
	// deliveryHealthとinboxHealthは同じ型なので、goroutineは2つになる。
	"deliveryHealth+inboxHealth": "deliveryhealth.(*Service).loop(",
	"serverStatsPub":             "stream.(*ServerStatsPublisher).loop(",
	"queueStatsPub":              "stream.(*QueueStatsPublisher).loop(",
	"fedRuleHits":                "fedrule.(*HitStore).Start.func",
	// pollExpiryWorker・reactionFlushWorker・userCacheLinkの3つ。
	"workers": "server.(*Server).startConstructionWorker.func",
}

// countPartGoroutines counts the goroutines of this process whose stack has
// each marker of partGoroutineMarkers.
func countPartGoroutines(t *testing.T) map[string]int {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, pprof.Lookup("goroutine").WriteTo(&buf, 2))
	counts := map[string]int{}
	for _, g := range strings.Split(buf.String(), "\n\n") {
		for key, marker := range partGoroutineMarkers {
			if strings.Contains(g, marker) {
				counts[key]++
			}
		}
	}
	return counts
}

// TestNewServer_ConstructionStartsNoPart checks, on a server built by
// newServer with both roles, that construction starts none of the twelve
// parts that have a start, and that StartBackground starts them (#3703).
// It looks at what the parts do, not at the list: the goroutines they run
// and the row the retention worker writes.
//
// 一覧の起動を差し替えて見る検査(TestNewServer_ShutdownStopsThePartsInTheirOrder)
// では、一覧を通らずに構築の途中で起動する書き方を落とせない。ここではgoroutineと
// DBを見る。
func TestNewServer_ConstructionStartsNoPart(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db, clients, _, cfg := constructionFixture(t)
	restoreProcessGlobals(t)
	require.NoError(t, db.Exec(`DELETE FROM retention_aggregation`).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM retention_aggregation`) })
	retentionRows := func() int64 {
		var n int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM retention_aggregation`).Scan(&n).Error)
		return n
	}

	before := countPartGoroutines(t)
	srv, err := newServer(cfg, db, clients, nil, noopStorage)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.StopBackground(context.Background()) })
	require.Equal(t, config.RoleBoth, srv.role)
	// 構築の途中で起動していれば、ここまでにgoroutineが立っている。retentionのworkerは
	// 起動すればすぐに行を書くので、少し待ってから見る。
	time.Sleep(300 * time.Millisecond)
	// 増えていないことを見る。前のテストの残りのgoroutineが抜けて減るのは許す。
	afterConstruction := countPartGoroutines(t)
	for key := range partGoroutineMarkers {
		assert.LessOrEqualf(t, afterConstruction[key], before[key], "construction started the goroutine of %s", key)
	}
	assert.Zero(t, retentionRows(), "construction ran retentionAggregateAtStartup")

	require.NoError(t, srv.StartBackground())
	want := map[string]int{}
	for key, n := range before {
		want[key] = n
	}
	for key, n := range map[string]int{
		"instanceTouchBuffer": 1, "sigCapBuffer": 1, "instanceCounterBuffer": 1,
		"deliveryHealth+inboxHealth": 2, "serverStatsPub": 1, "queueStatsPub": 1,
		"fedRuleHits": 1, "workers": 3,
	} {
		want[key] += n
	}
	assert.Eventually(t, func() bool {
		got := countPartGoroutines(t)
		for key, n := range want {
			if got[key] < n {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "StartBackground did not start the goroutines of the parts")
	assert.Eventually(t, func() bool { return retentionRows() == 1 }, 5*time.Second, 20*time.Millisecond,
		"StartBackground did not run retentionAggregateAtStartup")
}

// TestSupervisor_FailedStartStopsEveryPart makes StartBackground fail in the
// middle of the parts (a start that panics) on a generation the Supervisor
// builds, and checks that the Supervisor stops the generation: every part is
// stopped in its order, the ones never started included, without waiting for
// a worker that never ran, and the generation's DB connections and Redis
// subscriptions go away (#3703).
func TestSupervisor_FailedStartStopsEveryPart(t *testing.T) {
	env := newLifecycleEnv(t)
	deps := env.deps()
	var mu sync.Mutex
	var built *Server
	var rec lifecycleRecorder
	var listed []string
	failStart := false
	deps.NewServer = func(cfg *config.Config, db *gorm.DB, rc *cache.RedisClients) (*Server, error) {
		srv, err := newServer(cfg, db, rc, nil, noopStorage)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if failStart {
			built = srv
			listed = srv.lifecycle.names()
			rec.wrap(srv)
			// 一覧の中ほどの部品の起動をpanicさせる。後の部品(workerを含む)は起動しない。
			for i := range srv.lifecycle.parts {
				if srv.lifecycle.parts[i].name == "deliveryHealth" {
					srv.lifecycle.parts[i].start = func() { panic("start failed") }
				}
			}
		}
		return srv, nil
	}
	sup, err := NewSupervisor(env.cfg, env.redis, deps)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sup.Shutdown(context.Background()) })
	rep := sup.stopGeneration(http.HandlerFunc(unavailable))
	require.NoError(t, rep.Err)
	env.requireNoBackends(t)

	mu.Lock()
	failStart = true
	mu.Unlock()
	started := time.Now()
	err = sup.startGeneration()
	elapsed := time.Since(started)
	require.ErrorIs(t, err, errLifecycleStart)
	assert.ErrorContains(t, err, `part "deliveryHealth" panicked`)
	// 起動していないworkerの停止がdoneを待つと、DrainTimeout(5秒)まで固まる。
	assert.Less(t, elapsed, 3*time.Second, "stopping the generation waited for a part that never started")

	mu.Lock()
	require.NotNil(t, built)
	require.Contains(t, listed, "userCacheLink", "前提: 起動しなかったworkerが一覧にある")
	mu.Unlock()
	called, _ := rec.result()
	assert.Equal(t, listed, called, "the failed generation did not stop every part in its order")
	sup.genMu.Lock()
	assert.Nil(t, sup.gen, "the failed generation was kept")
	sup.genMu.Unlock()
	assert.Equal(t, http.StatusServiceUnavailable, serveStatus(t, sup, http.MethodGet, "/healthz"))
	env.requireNoBackends(t)
	env.requireNoPatterns(t, "the failed generation left its Redis subscriptions")
}

// startRecordingDriver is a driver.Driver whose worker records its Start in
// a log shared with the parts of a test. Other Driver methods are not used by
// StartBackground of a server without a scheduler and panic.
type startRecordingDriver struct {
	driver.Driver
	srv *startRecordingServer
}

func (d *startRecordingDriver) Server() driver.Server { return d.srv }
func (d *startRecordingDriver) Close() error          { return nil }

type startRecordingServer struct{ log *[]string }

func (s *startRecordingServer) Handle(string, driver.HandlerFunc) {}
func (s *startRecordingServer) Start() error {
	*s.log = append(*s.log, "queue")
	return nil
}
func (s *startRecordingServer) Shutdown() {}

// newStartOrderServer returns a server of both roles whose queue worker and
// two parts, a and b, record their start in the returned log. When
// panicking is set, the start of a panics.
func newStartOrderServer(panicking bool) (*Server, *[]string) {
	log := &[]string{}
	d := &startRecordingDriver{srv: &startRecordingServer{log: log}}
	s := &Server{
		echo: echo.New(), config: &config.Config{}, role: config.RoleBoth,
		queueDriver: d, queueServer: queue.NewServer(d),
	}
	s.addPartWithStart("a", nil, func() {
		if panicking {
			panic("start failed")
		}
		*log = append(*log, "a")
	}, func(context.Context) {})
	s.addPartWithStart("b", nil, func() { *log = append(*log, "b") }, func(context.Context) {})
	return s, log
}

// TestServer_StartBackgroundStartsThePartsBeforeTheQueue pins the start order
// of StartBackground and StartBackgroundForTest: the parts of the list, in
// list order, before the queue worker (#3703). The worker's jobs write to the
// buffers and the other parts, so the parts that flush them run first.
func TestServer_StartBackgroundStartsThePartsBeforeTheQueue(t *testing.T) {
	for name, start := range map[string]func(*Server) error{
		"StartBackground":        (*Server).StartBackground,
		"StartBackgroundForTest": (*Server).StartBackgroundForTest,
	} {
		t.Run(name, func(t *testing.T) {
			s, log := newStartOrderServer(false)
			require.NoError(t, start(s))
			assert.Equal(t, []string{"a", "b", "queue"}, *log)
			require.NoError(t, s.StopBackground(context.Background()))
		})
	}
}

// TestServer_FailedPartStartDoesNotStartTheQueue checks that when the start of
// a part panics, StartBackground returns the error without starting the parts
// after it or the queue worker (#3703).
func TestServer_FailedPartStartDoesNotStartTheQueue(t *testing.T) {
	s, log := newStartOrderServer(true)
	err := s.StartBackground()
	require.ErrorIs(t, err, errLifecycleStart)
	assert.Empty(t, *log, "a part after the failed one, or the queue worker, was started")
	require.NoError(t, s.StopBackground(context.Background()))
}
