package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// fakeMaintenanceSupervisor is a Supervisor over DB-less generations that
// follows the maintenance state of a miniredis.
func fakeMaintenanceSupervisor(t *testing.T, f *fakeGenerations) (*Supervisor, *maintmode.Store) {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	return fakeMaintenanceSupervisorAsIs(t, f)
}

// fakeMaintenanceSupervisorAsIs is fakeMaintenanceSupervisor keeping the
// process role environment of the caller.
func fakeMaintenanceSupervisorAsIs(t *testing.T, f *fakeGenerations) (*Supervisor, *maintmode.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	store := maintmode.NewStore(rc, nil, "u:")
	deps := f.deps(5 * time.Second)
	deps.Maintenance = store
	deps.ProcessID = "p1"
	deps.MaintenancePoll = 20 * time.Millisecond
	sup, err := NewSupervisor(&config.Config{}, nil, deps)
	require.NoError(t, err)
	sup.maint.start()
	t.Cleanup(func() { sup.maint.stop(context.Background()) })
	return sup, store
}

func report(t *testing.T, store *maintmode.Store) maintmode.ProcReport {
	t.Helper()
	reps, err := store.Reports(context.Background())
	require.NoError(t, err)
	require.Len(t, reps, 1)
	return reps[0]
}

// TestMaintenance_ReportStaysFreshWhileStopping checks that the process keeps
// refreshing its report while it is busy stopping the generation, so the
// report (TTL 10s) does not expire and look like completion (#3463 review
// H2).
func TestMaintenance_ReportStaysFreshWhileStopping(t *testing.T) {
	f := newFakeGenerations(t)
	block := make(chan struct{})
	f.closeDB = func(*gorm.DB) error { <-block; return nil }
	sup, store := fakeMaintenanceSupervisor(t, f)
	require.NoError(t, store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	require.Eventually(t, func() bool { return sup.maint.procState() == maintmode.ProcDraining },
		5*time.Second, 10*time.Millisecond)

	// DB を閉じる所で止まっている間も、報告の時刻が進む。
	first := report(t, store).UpdatedAt
	require.Eventually(t, func() bool {
		r := report(t, store)
		return r.State == maintmode.ProcDraining && r.UpdatedAt.After(first)
	}, 2*time.Second, 10*time.Millisecond, "止めている間に報告が更新されない")
	close(block)
	require.Eventually(t, func() bool { return report(t, store).State == maintmode.ProcDrained },
		5*time.Second, 10*time.Millisecond)
}

// TestMaintenance_FailedResumeBacksOff checks the retry interval of a
// generation that cannot be rebuilt: it doubles from resumeRetryMin up to
// resumeRetryMax, and no attempt is made before it elapses (#3463 review M3).
func TestMaintenance_FailedResumeBacksOff(t *testing.T) {
	f := newFakeGenerations(t)
	sup, store := fakeMaintenanceSupervisor(t, f)
	require.NoError(t, store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	require.Eventually(t, func() bool { return sup.maint.procState() == maintmode.ProcDrained },
		5*time.Second, 10*time.Millisecond)

	var opens atomic.Int32
	failing := sup.deps
	failing.OpenDB = func(*config.Config) (*gorm.DB, error) {
		opens.Add(1)
		return nil, errors.New("database is unreachable")
	}
	sup.maint.stop(context.Background())
	sup.deps = failing
	require.NoError(t, store.Clear(context.Background(), ""))

	m := &sup.maint
	m.reconcile(context.Background())
	require.Equal(t, maintmode.ProcResumeFailed, m.procState())
	assert.EqualValues(t, 1, opens.Load())
	assert.Equal(t, resumeRetryMin, m.retryWait)
	// 間隔が過ぎるまでは試さない。
	m.reconcile(context.Background())
	assert.EqualValues(t, 1, opens.Load(), "間隔を待たずに作り直しを試した")

	want := []time.Duration{2 * resumeRetryMin, 4 * resumeRetryMin, 8 * resumeRetryMin, resumeRetryMax, resumeRetryMax}
	for i, w := range want {
		m.mu.Lock()
		m.retryAt = time.Now().Add(-time.Millisecond)
		m.mu.Unlock()
		m.reconcile(context.Background())
		assert.EqualValues(t, i+2, opens.Load())
		assert.Equal(t, w, m.retryWait, "attempt %d", i+2)
	}
}

// TestMaintenance_QueueOnlyNodeServesHealthzOnly checks that a queue-only
// node does not start serving pages or static files in maintenance (#3463
// review L4).
func TestMaintenance_QueueOnlyNodeServesHealthzOnly(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "1")
	f := newFakeGenerations(t)
	sup, store := fakeMaintenanceSupervisorAsIs(t, f)
	require.NoError(t, store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	require.Eventually(t, func() bool { return sup.maint.procState() == maintmode.ProcDrained },
		5*time.Second, 10*time.Millisecond)
	code, body := getBody(t, sup, "/healthz")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `"role":"queue"`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	sup.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"), "配送ノードがメンテナンスの画面を配った")
}

// orderHook records, in order, the state reads and the report writes a
// Supervisor sends to Redis.
type orderHook struct {
	mu  sync.Mutex
	ops []string
}

func (h *orderHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *orderHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "hgetall" && strings.HasSuffix(fmt.Sprint(cmd.Args()[1]), "elythia:maintenance") {
			h.add("read-state")
		}
		return next(ctx, cmd)
	}
}
func (h *orderHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, c := range cmds {
			if c.Name() == "hset" && strings.Contains(fmt.Sprint(c.Args()[1]), ":proc:") {
				h.add("report")
				break
			}
		}
		return next(ctx, cmds)
	}
}
func (h *orderHook) add(op string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ops = append(h.ops, op)
}

// TestMaintenance_StartingProcessReportsBeforeReadingTheState checks #3463
// review M1': a starting process writes its "starting" report before it
// reads the state and opens the database, so a maintenance entered in
// between waits for it instead of becoming held.
func TestMaintenance_StartingProcessReportsBeforeReadingTheState(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	hook := &orderHook{}
	rc.AddHook(hook)
	store := maintmode.NewStore(rc, nil, "u:")

	f := newFakeGenerations(t)
	deps := f.deps(5 * time.Second)
	opening := make(chan struct{})
	proceed := make(chan struct{})
	open := deps.OpenDB
	deps.OpenDB = func(cfg *config.Config) (*gorm.DB, error) {
		close(opening)
		<-proceed
		return open(cfg)
	}
	deps.Maintenance = store
	deps.ProcessID = "p1"
	deps.MaintenancePoll = 20 * time.Millisecond
	built := make(chan *Supervisor, 1)
	go func() {
		sup, err := NewSupervisor(&config.Config{}, nil, deps)
		assert.NoError(t, err)
		built <- sup
	}()
	<-opening
	// DB を開いている最中のプロセスは「起動中」と見える。
	assert.Equal(t, maintmode.ProcStarting, report(t, store).State)
	hook.mu.Lock()
	ops := append([]string(nil), hook.ops...)
	hook.mu.Unlock()
	require.GreaterOrEqual(t, len(ops), 2)
	assert.Equal(t, []string{"report", "read-state"}, ops[:2], "状態を読んでから起動中を報告している")
	close(proceed)
	sup := <-built
	t.Cleanup(func() { sup.maint.stop(context.Background()) })
	assert.Equal(t, maintmode.ProcRunning, report(t, store).State)

	// 止めたら報告を消し、heartbeat も止まる (消した報告が書き戻されない)。
	sup.maint.stop(context.Background())
	assertNoReport(t, store)
}

// assertNoReport checks that no report is left, and that none comes back
// (the heartbeat has stopped).
func assertNoReport(t *testing.T, store *maintmode.Store) {
	t.Helper()
	reps, err := store.Reports(context.Background())
	require.NoError(t, err)
	assert.Empty(t, reps)
	time.Sleep(100 * time.Millisecond)
	reps, err = store.Reports(context.Background())
	require.NoError(t, err)
	assert.Empty(t, reps, "消した報告を heartbeat が書き戻した")
}

// TestNewSupervisor_FailedStartRemovesItsReport checks that a process that
// fails to start after announcing itself does not leave a starting report
// behind: it removes the report, and the heartbeat stops. starting の報告が
// 残ると、判定役は TTL が切れるまでメンテナンスを held にしない。
func TestNewSupervisor_FailedStartRemovesItsReport(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	for name, breakIt := range map[string]func(*fakeGenerations, *miniredis.Miniredis){
		// 状態のキーを別の型にして、読むのを失敗させる。
		"reading the state": func(_ *fakeGenerations, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set("u:elythia:maintenance", "not a hash"))
		},
		"building the generation": func(f *fakeGenerations, _ *miniredis.Miniredis) {
			f.openErr = errors.New("db down")
		},
	} {
		t.Run(name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rc.Close() })
			store := maintmode.NewStore(rc, nil, "u:")
			f := newFakeGenerations(t)
			breakIt(f, mr)
			deps := f.deps(time.Second)
			deps.Maintenance = store
			deps.ProcessID = "p1"
			deps.MaintenancePoll = 20 * time.Millisecond
			_, err := NewSupervisor(&config.Config{}, nil, deps)
			require.Error(t, err)
			assertNoReport(t, store)
		})
	}
}

// dbCounter answers, like pg_stat_activity, one session for every generation
// whose DB pool has not finished closing.
type dbCounter struct {
	f      *fakeGenerations
	closed atomic.Int32
}

// wrapClose makes f.closeDB count the pools that finished closing.
func (c *dbCounter) wrapClose(block <-chan struct{}) {
	c.f.closeDB = func(*gorm.DB) error {
		if block != nil {
			<-block
		}
		c.closed.Add(1)
		return nil
	}
}

func (c *dbCounter) Sessions(context.Context) ([]maintmode.Blocker, error) {
	var out []maintmode.Blocker
	for i := c.closed.Load(); i < c.f.built.Load(); i++ {
		out = append(out, maintmode.Blocker{Server: "primary"})
	}
	return out, nil
}

func (c *dbCounter) Terminate(context.Context, maintmode.Blocker) error { return nil }

func stateOf(t *testing.T, store *maintmode.Store) *maintmode.State {
	t.Helper()
	st, err := store.Get(context.Background())
	require.NoError(t, err)
	return st
}

// stepUntil steps j until the state reaches phase, or fails after a second.
func stepUntil(t *testing.T, j *maintmode.Judge, store *maintmode.Store, phase string) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, _ = j.Step(context.Background())
		st := stateOf(t, store)
		return st != nil && st.Phase == phase
	}, 5*time.Second, 10*time.Millisecond, "state did not reach %s", phase)
}

// neverHeld steps j for a while and checks that the state stays entering.
func neverHeld(t *testing.T, j *maintmode.Judge, store *maintmode.Store, why string) {
	t.Helper()
	for range 20 {
		_, _ = j.Step(context.Background())
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, maintmode.PhaseEntering, stateOf(t, store).Phase, why)
}

// TestMaintenance_HeldFollowsTheDatabaseNotTheShutdownOrder checks P8 of the
// second design: whatever a process that shuts down does with its report
// (removes it after closing the DB, removes it before, or dies without
// removing it), the maintenance is held only once its DB pool is closed.
// 以前は報告の順序が判定を左右し、レビューのたびに順序の穴が出た。
func TestMaintenance_HeldFollowsTheDatabaseNotTheShutdownOrder(t *testing.T) {
	for _, order := range []string{"removed after closing", "removed before closing", "dies without removing"} {
		t.Run(order, func(t *testing.T) {
			t.Setenv(config.EnvOnlyServer, "")
			t.Setenv(config.EnvOnlyQueue, "")
			mr := miniredis.RunT(t)
			rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rc.Close() })
			store := maintmode.NewStore(rc, nil, "u:")
			f := newFakeGenerations(t)
			counter := &dbCounter{f: f}
			block := make(chan struct{})
			counter.wrapClose(block)
			deps := f.deps(5 * time.Second)
			deps.Maintenance = store
			deps.ProcessID = "p1"
			deps.MaintenancePoll = 20 * time.Millisecond
			sup, err := NewSupervisor(&config.Config{}, nil, deps)
			require.NoError(t, err)
			// 監視は動かさない。DB を閉じるのは Shutdown だけにする。
			judge := &maintmode.Judge{Store: store, Counter: counter, ID: "judge", DrainTimeout: time.Hour}
			require.NoError(t, store.Enter(context.Background(), maintmode.State{OpID: "op"}))

			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- sup.Shutdown(context.Background()) }()
			switch order {
			case "removed before closing":
				// 報告だけが先に消えた (DB はまだ閉じていない)。
				require.Eventually(t, func() bool { return f.closed.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
				sup.maint.leave()
			case "dies without removing":
				// heartbeat が止まり、報告が TTL で消えた。
				require.Eventually(t, func() bool { return f.closed.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
				sup.maint.hbCancel()
				<-sup.maint.hbDone
				sup.maint.hbCancel = nil
				mr.FastForward(maintmode.ProcTTL + time.Second)
			}
			neverHeld(t, judge, store, "DB を閉じ終える前に held にした")
			close(block)
			require.NoError(t, <-shutdownDone)
			stepUntil(t, judge, store, maintmode.PhaseHeld)
			reps, err := store.Reports(context.Background())
			require.NoError(t, err)
			assert.Empty(t, reps, "止めたプロセスの報告が残っている")
		})
	}
}

// TestMaintenance_SupervisorsJudgeOneAtATime checks that the Supervisors are
// the judges: one of them takes the lock, moves the maintenance to held once
// both let go, clears it once both serve again, and the other takes over
// when the judge stops.
func TestMaintenance_SupervisorsJudgeOneAtATime(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	store := maintmode.NewStore(rc, nil, "u:")
	sups := map[string]*Supervisor{}
	for _, id := range []string{"a", "b"} {
		f := newFakeGenerations(t)
		counter := &dbCounter{f: f}
		counter.wrapClose(nil)
		deps := f.deps(5 * time.Second)
		deps.Maintenance = store
		deps.MaintenanceCounter = counter
		deps.ProcessID = id
		deps.MaintenancePoll = 20 * time.Millisecond
		sup, err := NewSupervisor(&config.Config{}, nil, deps)
		require.NoError(t, err)
		sup.maint.start()
		t.Cleanup(func() { sup.maint.stop(context.Background()) })
		sups[id] = sup
	}
	ctx := context.Background()
	require.NoError(t, store.Enter(ctx, maintmode.State{OpID: "op"}))
	require.Eventually(t, func() bool { return stateOf(t, store).Phase == maintmode.PhaseHeld },
		5*time.Second, 10*time.Millisecond, "Supervisor が held にしない")
	for id, sup := range sups {
		assert.Equal(t, maintmode.ProcDrained, sup.maint.procState(), id)
	}
	holder, err := store.Judge(ctx)
	require.NoError(t, err)
	require.Contains(t, []string{"a", "b"}, holder)

	require.NoError(t, store.SetPhase(ctx, "op", maintmode.PhaseResuming))
	require.Eventually(t, func() bool { return stateOf(t, store) == nil },
		5*time.Second, 10*time.Millisecond, "全員が戻っても判定役が消さない")
	for id, sup := range sups {
		assert.Equal(t, maintmode.ProcRunning, sup.maint.procState(), id)
	}

	// 判定役が止まったら、もう片方が引き継ぐ。
	require.NoError(t, store.Enter(ctx, maintmode.State{OpID: "op2"}))
	require.Eventually(t, func() bool { h, _ := store.Judge(ctx); return h != "" }, 5*time.Second, 10*time.Millisecond)
	first, _ := store.Judge(ctx)
	sups[first].maint.stop(ctx)
	other := "a"
	if first == "a" {
		other = "b"
	}
	require.Eventually(t, func() bool { h, _ := store.Judge(ctx); return h == other },
		5*time.Second, 10*time.Millisecond, "止めた判定役のロックを、もう片方が引き継がない")
}
