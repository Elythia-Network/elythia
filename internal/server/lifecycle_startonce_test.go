package server

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	queuemetrics "github.com/elythia-network/elythia/internal/queue/metrics"
)

// eventLog is a log of what the fakes of a test started and stopped, safe
// for concurrent use.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *eventLog) count(e string) int {
	n := 0
	for _, got := range l.all() {
		if got == e {
			n++
		}
	}
	return n
}

// startOnceDriver is a queue driver whose worker and scheduler record their
// Start and Shutdown in an eventLog. The auto-scale controller runs against
// the embedded scriptableDriver, which counts its Resize calls (the
// controller resizes every queue it manages when it starts).
type startOnceDriver struct {
	*scriptableDriver
	srv *startOnceQueueServer
	sch *startOnceScheduler
}

func (d *startOnceDriver) Server() driver.Server       { return d.srv }
func (d *startOnceDriver) Scheduler() driver.Scheduler { return d.sch }

type startOnceQueueServer struct {
	log      *eventLog
	startErr error
}

func (s *startOnceQueueServer) Handle(string, driver.HandlerFunc) {}
func (s *startOnceQueueServer) Start() error {
	s.log.add("queue start")
	return s.startErr
}
func (s *startOnceQueueServer) Shutdown() { s.log.add("queue shutdown") }

type startOnceScheduler struct{ log *eventLog }

func (s *startOnceScheduler) Register(string, string, []byte, ...driver.EnqueueOption) error {
	return nil
}
func (s *startOnceScheduler) PruneUnregistered() ([]string, error) { return nil, nil }
func (s *startOnceScheduler) Start() error {
	s.log.add("scheduler start")
	return nil
}
func (s *startOnceScheduler) Shutdown() { s.log.add("scheduler shutdown") }

// startOnceFixture is a Server literal of role whose queue worker, scheduler,
// auto-scale controller, chart save loop and one part of the list ("part")
// can be observed.
type startOnceFixture struct {
	srv *Server
	log *eventLog
	drv *startOnceDriver
}

func newStartOnceFixture(role config.ProcessRole) *startOnceFixture {
	log := &eventLog{}
	d := &startOnceDriver{
		scriptableDriver: newScriptableDriver(nil),
		srv:              &startOnceQueueServer{log: log},
		sch:              &startOnceScheduler{log: log},
	}
	s := &Server{
		echo:           echo.New(),
		config:         &config.Config{JobQueueAutoScale: true},
		role:           role,
		queueDriver:    d,
		queueServer:    queue.NewServer(d),
		queueScheduler: queue.NewScheduler(d),
		queueMetrics:   queuemetrics.New(),
		chartMgmt:      chart.NewManagementService(nil, time.Hour),
	}
	s.addPartWithStart("part", nil, func() { log.add("part start") }, func(context.Context) { log.add("part stop") })
	return &startOnceFixture{srv: s, log: log, drv: d}
}

// chartRunning reports whether the chart save loop of the fixture is running.
// ManagementService has no getter, so it tries to start the loop: Start fails
// only while the loop runs. A loop the check started is stopped again.
func (f *startOnceFixture) chartRunning(t *testing.T) bool {
	t.Helper()
	if err := f.srv.chartMgmt.Start(context.Background()); err != nil {
		return true
	}
	f.srv.chartMgmt.Stop(context.Background())
	return false
}

// autoscaleStarted reports whether the auto-scale controller was started.
func (f *startOnceFixture) autoscaleStarted() bool {
	return f.drv.resizes.Load() > 0
}

// TestServer_StartAfterStopStartsNothing checks that StartBackground and
// StartBackgroundForTest called after StopBackground start nothing, neither
// the parts nor the queue worker, auto-scale, scheduler or chart, and return
// errGenerationStopped (#3709).
//
// #3709より前は、一覧は起動しない(startAllがstoppedを見る)が、queueとchartは止めた
// 世代のまま起動し、成功を返していた。
func TestServer_StartAfterStopStartsNothing(t *testing.T) {
	for name, start := range map[string]func(*Server) error{
		"StartBackground":        (*Server).StartBackground,
		"StartBackgroundForTest": (*Server).StartBackgroundForTest,
	} {
		t.Run(name, func(t *testing.T) {
			f := newStartOnceFixture(config.RoleBoth)
			require.NoError(t, f.srv.StopBackground(context.Background()))
			stopped := f.log.all()

			err := start(f.srv)
			require.ErrorIs(t, err, errGenerationStopped)
			assert.Equal(t, stopped, f.log.all(), "a start after the stop started something")
			assert.Nil(t, f.srv.autoscale, "a start after the stop started the auto-scale controller")
			assert.False(t, f.autoscaleStarted(), "a start after the stop started the auto-scale controller")
			assert.False(t, f.chartRunning(t), "a start after the stop started the chart save loop")
			// 2回目も同じ結果を返し、起動しない。
			require.ErrorIs(t, start(f.srv), errGenerationStopped)
			assert.Equal(t, stopped, f.log.all())
		})
	}
}

// TestServer_StartAfterFailedConstructionStartsNothing checks that the
// rollback of a failed construction (cleanupConstruction) marks the
// generation stopped, so a start called afterwards starts nothing (#3709).
func TestServer_StartAfterFailedConstructionStartsNothing(t *testing.T) {
	f := newStartOnceFixture(config.RoleBoth)
	// 後始末はqueueのdriverをnilに戻すので、autoscaleを起動するとnilのdriverでpanicする。
	// 印を見ない退行を、panicではなく失敗として出すために切っておく。
	f.srv.config.JobQueueAutoScale = false
	queueServer := f.srv.queueServer
	f.srv.cleanupConstruction()
	// 後始末はdriverをnilに戻すが、queueServerは残る。印を見ないと、ここから起動する。
	require.Same(t, queueServer, f.srv.queueServer)
	cleaned := f.log.all()

	require.ErrorIs(t, f.srv.StartBackground(), errGenerationStopped)
	assert.Equal(t, cleaned, f.log.all(), "a start after the rollback started something")
	assert.False(t, f.chartRunning(t), "a start after the rollback started the chart save loop")
}

// TestServer_StartBackgroundAndForTestShareTheOnce checks that
// StartBackground and StartBackgroundForTest go through the same once
// (#3709): whichever is called first starts the generation, and the other
// starts nothing and returns the result of the first.
func TestServer_StartBackgroundAndForTestShareTheOnce(t *testing.T) {
	startBackground := (*Server).StartBackground
	startForTest := (*Server).StartBackgroundForTest
	for _, tc := range []struct {
		name          string
		first, second func(*Server) error
		// chart reports whether the first start runs the chart save loop.
		chart bool
	}{
		{name: "StartBackground first", first: startBackground, second: startForTest, chart: true},
		{name: "StartBackgroundForTest first", first: startForTest, second: startBackground, chart: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStartOnceFixture(config.RoleBoth)
			require.NoError(t, tc.first(f.srv))
			require.NoError(t, tc.second(f.srv))
			assert.Equal(t, []string{"part start", "queue start", "scheduler start"}, f.log.all(),
				"the second start started something again")
			assert.Equal(t, tc.chart, f.chartRunning(t), "the second start changed whether the chart runs")
			require.NoError(t, f.srv.StopBackground(context.Background()))
		})
		t.Run(tc.name+" failing", func(t *testing.T) {
			f := newStartOnceFixture(config.RoleBoth)
			failure := errors.New("queue start failed")
			f.drv.srv.startErr = failure
			first := tc.first(f.srv)
			require.ErrorIs(t, first, failure)
			second := tc.second(f.srv)
			assert.Same(t, first, second, "the second start did not return the result of the first")
			assert.Equal(t, 1, f.log.count("queue start"), "the second start started the queue again")
			require.NoError(t, f.srv.StopBackground(context.Background()))
		})
	}
}

// TestServer_StartBackgroundForTestIgnoresTheRole pins what the parameter of
// the shared start selects (#3709): StartBackgroundForTest starts the queue
// worker whatever the role and never the chart save loop; StartBackground
// starts the queue only for a role that runs it, and the chart always.
func TestServer_StartBackgroundForTestIgnoresTheRole(t *testing.T) {
	t.Run("StartBackgroundForTest", func(t *testing.T) {
		f := newStartOnceFixture(config.RoleServer)
		require.NoError(t, f.srv.StartBackgroundForTest())
		assert.Equal(t, []string{"part start", "queue start", "scheduler start"}, f.log.all())
		assert.True(t, f.autoscaleStarted(), "StartBackgroundForTest did not start the auto-scale controller")
		assert.False(t, f.chartRunning(t), "StartBackgroundForTest started the chart save loop")
		// stopQueueはworkerとschedulerの停止をroleで守るので、server roleで
		// StartBackgroundForTestが起動したものはStopBackgroundでは止まらない
		// (#3709より前から同じ)。ここでは手で止める。
		f.srv.autoscale.Stop(context.Background())
		f.srv.queueServer.Shutdown()
		f.srv.queueScheduler.Shutdown()
		require.NoError(t, f.srv.StopBackground(context.Background()))
	})
	t.Run("StartBackground", func(t *testing.T) {
		f := newStartOnceFixture(config.RoleServer)
		require.NoError(t, f.srv.StartBackground())
		assert.Equal(t, []string{"part start"}, f.log.all())
		assert.False(t, f.autoscaleStarted(), "StartBackground started the auto-scale controller on a server role")
		assert.True(t, f.chartRunning(t), "StartBackground did not start the chart save loop")
		require.NoError(t, f.srv.StopBackground(context.Background()))
	})
}

// TestServer_ConcurrentStartAndStopLeaveNothingRunning runs StartBackground
// (or StartBackgroundForTest) and StopBackground at the same time, many
// times, and checks that whatever the start started was stopped after it:
// the part, the queue worker and scheduler, the auto-scale controller and
// the chart save loop (#3709). Run it with -race: the start and the stop
// share the fields of the Server (the auto-scale runner).
//
// 起動と停止の順を守る印が無いと、停止が止め終わった後(または止めている途中)に
// 起動したworkerが残る。
func TestServer_ConcurrentStartAndStopLeaveNothingRunning(t *testing.T) {
	const rounds = 300
	starts := []func(*Server) error{(*Server).StartBackground, (*Server).StartBackgroundForTest}
	startedFirst := 0
	for i := range rounds {
		f := newStartOnceFixture(config.RoleBoth)
		start := starts[i%len(starts)]
		// 片方の順に偏らないよう、組ごとに先に譲る側を替える。譲らないと、GOMAXPROCS=1で
		// -raceを付けないときに、どの組でも起動が先に走る(実測で300組のうち300組)。
		startYields := (i/len(starts))%2 == 0
		var wg sync.WaitGroup
		var startErr error
		ready := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-ready
			if startYields {
				runtime.Gosched()
			}
			startErr = start(f.srv)
		}()
		go func() {
			defer wg.Done()
			<-ready
			if !startYields {
				runtime.Gosched()
			}
			assert.NoError(t, f.srv.StopBackground(context.Background()))
		}()
		close(ready)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: the start and the stop did not return (deadlock)", i)
		}

		events := f.log.all()
		if startErr != nil {
			require.ErrorIs(t, startErr, errGenerationStopped, "round %d", i)
			assert.NotContains(t, events, "part start", "round %d: a start that lost to the stop started a part", i)
			assert.NotContains(t, events, "queue start", "round %d: a start that lost to the stop started the queue", i)
			assert.False(t, f.autoscaleStarted(), "round %d: a start that lost to the stop started the auto-scale controller", i)
		} else {
			startedFirst++
		}
		for _, pair := range [][2]string{
			{"part start", "part stop"},
			{"queue start", "queue shutdown"},
			{"scheduler start", "scheduler shutdown"},
		} {
			at := slices.Index(events, pair[0])
			if at < 0 {
				continue
			}
			assert.Containsf(t, events[at+1:], pair[1], "round %d: %q was not stopped after it started: %v", i, pair[0], events)
		}
		if r := f.srv.autoscale; r != nil {
			stopped := make(chan struct{})
			go func() { r.wg.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				r.Stop(context.Background())
				t.Errorf("round %d: the auto-scale controller was left running", i)
			}
		}
		if f.chartRunning(t) {
			f.srv.chartMgmt.Stop(context.Background())
			t.Errorf("round %d: the chart save loop was left running", i)
		}
		if t.Failed() {
			return
		}
	}
	// 両方の順が起きていなければ、片方の順の検査は空振りしている。
	t.Logf("the start ran before the stop in %d of %d rounds", startedFirst, rounds)
	assert.Positive(t, startedFirst, "the start never ran before the stop")
	assert.Less(t, startedFirst, rounds, "the stop never ran before the start")
}
