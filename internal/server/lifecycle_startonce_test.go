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

// autoscaleStopped reports whether the goroutines of the auto-scale
// controller the start left in the Server have exited (true when there is
// none), waiting for them at most a few seconds.
func (f *startOnceFixture) autoscaleStopped() bool {
	r := f.srv.autoscale
	if r == nil {
		return true
	}
	exited := make(chan struct{})
	go func() { r.wg.Wait(); close(exited) }()
	select {
	case <-exited:
		return true
	case <-time.After(3 * time.Second):
		r.Stop(context.Background())
		return false
	}
}

// TestServer_StartBackgroundForTestIgnoresTheRole pins what the parameter of
// the shared start selects (#3709): StartBackgroundForTest starts the queue
// worker whatever the role and never the chart save loop; StartBackground
// starts the queue only for a role that runs it, and the chart always. What
// either started, StopBackground stops (#3713).
func TestServer_StartBackgroundForTestIgnoresTheRole(t *testing.T) {
	t.Run("StartBackgroundForTest", func(t *testing.T) {
		f := newStartOnceFixture(config.RoleServer)
		require.NoError(t, f.srv.StartBackgroundForTest())
		assert.Equal(t, []string{"part start", "queue start", "scheduler start"}, f.log.all())
		assert.True(t, f.autoscaleStarted(), "StartBackgroundForTest did not start the auto-scale controller")
		assert.False(t, f.chartRunning(t), "StartBackgroundForTest started the chart save loop")
		// #3713より前は、server roleでStartBackgroundForTestが起動したworkerとschedulerを
		// StopBackgroundが止めず、ここで手で止めていた。
		require.NoError(t, f.srv.StopBackground(context.Background()))
		assert.Equal(t, []string{"part start", "queue start", "scheduler start", "scheduler shutdown", "queue shutdown", "part stop"},
			f.log.all(), "StopBackground did not stop what StartBackgroundForTest started")
		assert.True(t, f.autoscaleStopped(), "StopBackground left the auto-scale controller running")
	})
	t.Run("StartBackground", func(t *testing.T) {
		f := newStartOnceFixture(config.RoleServer)
		require.NoError(t, f.srv.StartBackground())
		assert.Equal(t, []string{"part start"}, f.log.all())
		assert.False(t, f.autoscaleStarted(), "StartBackground started the auto-scale controller on a server role")
		assert.True(t, f.chartRunning(t), "StartBackground did not start the chart save loop")
		require.NoError(t, f.srv.StopBackground(context.Background()))
		// queueを起動していないserver roleの世代では、今までどおりworkerとschedulerの
		// Shutdownを呼ばない。
		assert.Equal(t, []string{"part start", "part stop"}, f.log.all(),
			"StopBackground shut down the queue that StartBackground did not start on a server role")
	})
}

// TestServer_StopBackgroundStopsTheQueueByRoleOrStart pins when
// StopBackground shuts the queue worker and the scheduler down (#3713): when
// the role runs the queue, started or not (as before #3713), and when the
// start started the worker, whatever the role, even when the start failed
// after it.
func TestServer_StopBackgroundStopsTheQueueByRoleOrStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		role config.ProcessRole
		// start starts the generation (nil: never started) and checks its result.
		start func(t *testing.T, f *startOnceFixture)
		// shutdowns is how many times the stop shuts the worker and the scheduler down.
		shutdowns int
	}{
		{
			name: "server role started for the test",
			role: config.RoleServer,
			start: func(t *testing.T, f *startOnceFixture) {
				require.NoError(t, f.srv.StartBackgroundForTest())
			},
			shutdowns: 1,
		},
		{
			name: "server role whose auto-scale failed to start",
			role: config.RoleServer,
			start: func(t *testing.T, f *startOnceFixture) {
				// workerの起動の後、autoscaleの起動で失敗させる(下限×queueの数が全体の上限を超える)。
				minWorkers, maxGlobal := 2, 1
				f.srv.config.MinWorkers = &minWorkers
				f.srv.config.MaxWorkersGlobal = &maxGlobal
				err := f.srv.StartBackgroundForTest()
				require.ErrorContains(t, err, "start autoscale")
				require.Equal(t, []string{"part start", "queue start"}, f.log.all(),
					"premise: the worker started and the start failed after it")
			},
			shutdowns: 1,
		},
		{
			name: "server role whose worker failed to start",
			role: config.RoleServer,
			start: func(t *testing.T, f *startOnceFixture) {
				failure := errors.New("queue start failed")
				f.drv.srv.startErr = failure
				require.ErrorIs(t, f.srv.StartBackgroundForTest(), failure)
			},
			// 起動に失敗したworkerは、起動していないものとして扱う(roleの判定だけが残る)。
			shutdowns: 0,
		},
		{name: "server role never started", role: config.RoleServer, shutdowns: 0},
		{name: "both roles never started", role: config.RoleBoth, shutdowns: 1},
		{name: "queue role never started", role: config.RoleQueue, shutdowns: 1},
		{
			name: "both roles started",
			role: config.RoleBoth,
			start: func(t *testing.T, f *startOnceFixture) {
				require.NoError(t, f.srv.StartBackground())
			},
			shutdowns: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStartOnceFixture(tc.role)
			if tc.start != nil {
				tc.start(t, f)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, f.srv.StopBackground(ctx))
			assert.Equal(t, tc.shutdowns, f.log.count("queue shutdown"), "shutdowns of the queue worker")
			assert.Equal(t, tc.shutdowns, f.log.count("scheduler shutdown"), "shutdowns of the scheduler")
			// 失敗したStartも"queue start"を記録するので、止めるはずの組だけ順を見る。
			if at := slices.Index(f.log.all(), "queue start"); at >= 0 && tc.shutdowns > 0 {
				assert.Contains(t, f.log.all()[at+1:], "queue shutdown", "the queue worker was not shut down after it started")
			}
			assert.True(t, f.autoscaleStopped(), "StopBackground left the auto-scale controller running")
		})
	}
}

// TestServer_StopQueueOfANeverStartedServerRoleDoesNotWait checks that the
// stop of a server-role generation that never started the queue still
// returns without waiting, even with an expired ctx, and without shutting
// the worker or the scheduler down (#3713 keeps it as before).
func TestServer_StopQueueOfANeverStartedServerRoleDoesNotWait(t *testing.T) {
	f := newStartOnceFixture(config.RoleServer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 待ちのselectは準備のできたcaseを無作為に選ぶので、何度か試す。
	for range 50 {
		done, stopped := f.srv.stopQueue(ctx)
		require.True(t, stopped, "a generation with nothing to stop was taken as not stopped in time")
		<-done
	}
	assert.Empty(t, f.log.all(), "the stop of a never-started server role shut the queue down")
}

// TestServer_ConcurrentStartAndStopLeaveNothingRunning runs StartBackground
// (or StartBackgroundForTest) and StopBackground at the same time, many
// times, and checks that whatever the start started was stopped after it:
// the part, the queue worker and scheduler, the auto-scale controller and
// the chart save loop (#3709). Run it with -race: the start and the stop
// share the fields of the Server (the auto-scale runner, and queueStarted,
// which the stop of a server role reads (#3713)).
//
// 起動と停止の順を守る印が無いと、停止が止め終わった後(または止めている途中)に
// 起動したworkerが残る。
func TestServer_ConcurrentStartAndStopLeaveNothingRunning(t *testing.T) {
	const rounds = 300
	for _, tc := range []struct {
		name   string
		role   config.ProcessRole
		starts []func(*Server) error
	}{
		{name: "both roles", role: config.RoleBoth, starts: []func(*Server) error{(*Server).StartBackground, (*Server).StartBackgroundForTest}},
		// server roleではStartBackgroundForTestだけがworkerを起動し、停止はqueueStartedを
		// 読んで止める(#3713)。RoleBothではroleの判定で短絡し、queueStartedを読まない。
		{name: "server role", role: config.RoleServer, starts: []func(*Server) error{(*Server).StartBackgroundForTest}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startedFirst := 0
			for i := range rounds {
				f := newStartOnceFixture(tc.role)
				start := tc.starts[i%len(tc.starts)]
				// 片方の順に偏らないよう、組ごとに先に譲る側を替える。譲らないと、GOMAXPROCS=1で
				// -raceを付けないときに、どの組でも起動が先に走る(実測で300組のうち300組)。
				startYields := (i/len(tc.starts))%2 == 0
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
					// 起動が勝った組では、queueのworkerを起動している。起動していなければ、下の
					// workerとschedulerの停止の検査が空振りする。
					assert.Contains(t, events, "queue start", "round %d: a start that won did not start the queue", i)
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
		})
	}
}
