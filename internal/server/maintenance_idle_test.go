package server

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// reportCounter counts the report writes a Supervisor sends to Redis, the
// state reads, and the judge lock scripts.
type reportCounter struct {
	n          atomic.Int32
	stateReads atomic.Int32
	judgeLocks atomic.Int32
}

func (h *reportCounter) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *reportCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		switch {
		case cmd.Name() == "hgetall" && len(args) > 1 && fmt.Sprint(args[1]) == "u:elythia:maintenance":
			h.stateReads.Add(1)
		case cmd.Name() == "evalsha" || cmd.Name() == "eval":
			for _, a := range args {
				if fmt.Sprint(a) == "u:elythia:maintenance:judge" {
					h.judgeLocks.Add(1)
					break
				}
			}
		}
		return err
	}
}
func (h *reportCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, c := range cmds {
			if c.Name() == "hset" && strings.Contains(fmt.Sprint(c.Args()[1]), ":proc:") {
				h.n.Add(1)
				break
			}
		}
		return next(ctx, cmds)
	}
}

// idleRig is a Supervisor (and judge) over DB-less generations whose idle
// interval differs from the interval during a maintenance (#3592).
type idleRig struct {
	sup     *Supervisor
	store   *maintmode.Store
	mr      *miniredis.Miniredis
	rc      *redis.Client
	reports *reportCounter
}

func newIdleRig(t *testing.T, poll, idle time.Duration) *idleRig {
	t.Helper()
	return newIdleRigWith(t, poll, idle, nil)
}

// newIdleRigWith is newIdleRig with prepare run on the tracker before its
// watch loop starts.
func newIdleRigWith(t *testing.T, poll, idle time.Duration, prepare func(*maintTracker)) *idleRig {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	reports := &reportCounter{}
	rc.AddHook(reports)
	store := maintmode.NewStore(rc, nil, "u:")
	f := newFakeGenerations(t)
	counter := &dbCounter{f: f}
	counter.wrapClose(nil)
	deps := f.deps(5 * time.Second)
	deps.Maintenance = store
	deps.MaintenanceCounter = counter
	deps.ProcessID = "p1"
	deps.MaintenancePoll = poll
	deps.MaintenanceIdlePoll = idle
	sup, err := NewSupervisor(&config.Config{}, nil, deps)
	require.NoError(t, err)
	if prepare != nil {
		prepare(&sup.maint)
	}
	sup.maint.start()
	t.Cleanup(func() { sup.maint.stop(context.Background()) })
	return &idleRig{sup: sup, store: store, mr: mr, rc: rc, reports: reports}
}

// enterSilently writes a maintenance state without publishing the hint, as
// if the hint had been lost.
func (r *idleRig) enterSilently(t *testing.T) {
	t.Helper()
	require.NoError(t, r.rc.HSet(context.Background(), r.store.StateKey(),
		"phase", maintmode.PhaseEntering, "opId", "op").Err())
}

func TestMaintenance_IntervalFollowsTheState(t *testing.T) {
	rig := newIdleRig(t, 20*time.Millisecond, time.Hour)
	m := &rig.sup.maint
	require.Eventually(t, func() bool { return m.procState() == maintmode.ProcRunning }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, time.Hour, m.interval(), "running without a maintenance is idle")

	m.state.Store(&maintmode.State{Phase: maintmode.PhaseResuming})
	assert.Equal(t, 20*time.Millisecond, m.interval(), "an active state, even resuming, is polled quickly")
	m.state.Store(nil)
	for _, proc := range []string{maintmode.ProcStarting, maintmode.ProcDraining, maintmode.ProcDrained,
		maintmode.ProcResuming, maintmode.ProcResumeFailed} {
		m.mu.Lock()
		saved := m.proc
		m.proc = proc
		m.mu.Unlock()
		assert.Equal(t, 20*time.Millisecond, m.interval(), proc)
		m.mu.Lock()
		m.proc = saved
		m.mu.Unlock()
	}
}

// TestMaintenance_IdleProcessNoticesTheHintAtOnce checks that a process
// polling at the idle interval still enters at once through the pubsub
// hint, and that its judge is woken too, so the maintenance becomes held
// without waiting for the idle interval.
func TestMaintenance_IdleProcessNoticesTheHintAtOnce(t *testing.T) {
	rig := newIdleRig(t, 20*time.Millisecond, time.Hour)
	require.Eventually(t, func() bool { return rig.sup.maint.procState() == maintmode.ProcRunning },
		5*time.Second, 10*time.Millisecond)
	// 判定役が最初の判定を終え、待機の周期 (1 時間) で眠るのを待つ。
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, rig.store.Enter(context.Background(), maintmode.State{OpID: "op"}))
	require.Eventually(t, func() bool {
		st := stateOf(t, rig.store)
		return st != nil && st.Phase == maintmode.PhaseHeld
	}, 5*time.Second, 10*time.Millisecond, "the idle judge was not woken by the hint")
	assert.Equal(t, maintmode.ProcDrained, rig.sup.maint.procState())
}

// TestMaintenance_MissedHintWaitsForTheIdleInterval checks that without the
// hint an idle process notices the maintenance only at the idle interval
// (here an hour, so not at all during the test).
func TestMaintenance_MissedHintWaitsForTheIdleInterval(t *testing.T) {
	rig := newIdleRig(t, 20*time.Millisecond, time.Hour)
	require.Eventually(t, func() bool { return rig.sup.maint.procState() == maintmode.ProcRunning },
		5*time.Second, 10*time.Millisecond)
	// 起動時の 1 回、監視の最初の読み直し、判定役の最初の判定 (読んでロックを
	// 解く) が終わるのを、回数で待つ。sleep で待つと重い CI で間に合わない。
	require.Eventually(t, func() bool {
		return rig.reports.stateReads.Load() >= 3 && rig.reports.judgeLocks.Load() >= 1
	}, 5*time.Second, time.Millisecond, "the first reads never happened")
	reads := rig.reports.stateReads.Load()
	rig.enterSilently(t)
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, maintmode.ProcRunning, rig.sup.maint.procState(), "an idle process re-read the state at the short interval")
	assert.Equal(t, reads, rig.reports.stateReads.Load(), "the state was re-read while idle")
}

// TestMaintenance_MissedHintIsNoticedWithinTheIdleInterval checks the bound
// of a lost hint: the process re-reads the state at the idle interval, and
// once it sees the state, its judge moves the maintenance to held.
func TestMaintenance_MissedHintIsNoticedWithinTheIdleInterval(t *testing.T) {
	rig := newIdleRig(t, 20*time.Millisecond, 200*time.Millisecond)
	require.Eventually(t, func() bool { return rig.sup.maint.procState() == maintmode.ProcRunning },
		5*time.Second, 10*time.Millisecond)
	rig.enterSilently(t)
	require.Eventually(t, func() bool {
		st := stateOf(t, rig.store)
		return st != nil && st.Phase == maintmode.PhaseHeld
	}, 5*time.Second, 10*time.Millisecond, "a lost hint was never caught up by the idle poll")
	assert.Equal(t, maintmode.ProcDrained, rig.sup.maint.procState())
}

// TestMaintenance_IdleReportIsWrittenOncePerInterval checks that the watch
// loop and the heartbeat do not both write the report on every interval
// (#3592: it was written twice every 2 seconds).
func TestMaintenance_IdleReportIsWrittenOncePerInterval(t *testing.T) {
	const idle = 100 * time.Millisecond
	rig := newIdleRig(t, 20*time.Millisecond, idle)
	require.Eventually(t, func() bool { return rig.sup.maint.procState() == maintmode.ProcRunning },
		5*time.Second, 10*time.Millisecond)
	time.Sleep(2 * idle)
	before := rig.reports.n.Load()
	const window = 20 * idle
	time.Sleep(window)
	n := rig.reports.n.Load() - before
	// 1 周期に 1 回なら約 20 回。二重に書くと約 40 回になる。下限は、負荷の
	// 高い CI で周期が延びても割らないよう低めに取る (書かなくなる回帰を拾えれば
	// よい)。
	assert.LessOrEqual(t, n, int32(28), "the report is written more than once per interval")
	assert.GreaterOrEqual(t, n, int32(4), "the report is not refreshed every interval")
	assert.Equal(t, maintmode.ProcRunning, report(t, rig.store).State)
}

// TestMaintenance_FailedReportIsRetriedAtTheShortInterval checks #3592
// review L-1: after a report write fails, the next refresh writes again
// without waiting for the idle interval, and the interval stays short until
// a write succeeds. 待機中の周期のままだと、ProcTTL の間に 2 回ほどしか試せない。
func TestMaintenance_FailedReportIsRetriedAtTheShortInterval(t *testing.T) {
	rig := newIdleRig(t, 20*time.Millisecond, time.Hour)
	m := &rig.sup.maint
	require.Eventually(t, func() bool { return m.procState() == maintmode.ProcRunning }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	ctx := context.Background()
	require.Equal(t, time.Hour, m.interval())

	rig.mr.SetError("LOADING the server is loading")
	m.report(ctx)
	rig.mr.SetError("")
	assert.Equal(t, 20*time.Millisecond, m.interval(), "a failed write keeps the idle interval")

	before := report(t, rig.store).UpdatedAt
	m.refresh(ctx)
	assert.True(t, report(t, rig.store).UpdatedAt.After(before), "the refresh after a failed write did not write")
	assert.Equal(t, time.Hour, m.interval(), "a successful write did not restore the idle interval")
}

// TestMaintenance_LostSubscriptionPollsAtTheShortInterval checks that while
// the pubsub subscription is gone the state is re-read at the short
// interval even when idle: no hint can arrive meanwhile.
func TestMaintenance_LostSubscriptionPollsAtTheShortInterval(t *testing.T) {
	rig := newIdleRigWith(t, 20*time.Millisecond, time.Hour, func(m *maintTracker) {
		m.subscribe = func(context.Context) <-chan struct{} {
			ch := make(chan struct{})
			close(ch)
			return ch
		}
	})
	require.Eventually(t, func() bool { return rig.sup.maint.procState() == maintmode.ProcRunning },
		5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	rig.enterSilently(t)
	require.Eventually(t, func() bool { return rig.sup.maint.procState() == maintmode.ProcDrained },
		5*time.Second, 10*time.Millisecond, "without a subscription the idle process waited the idle interval")
}
