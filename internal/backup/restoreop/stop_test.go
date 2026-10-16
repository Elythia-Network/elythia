package restoreop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/maintmode"
)

// stopper stops the service at the n-th call of one point, as if it were
// shut down while that call was running.
type stopper struct {
	mu    sync.Mutex
	at    string
	n     int
	calls map[string]int
	stop  func()
	fired chan struct{}
}

func (s *stopper) hit(point string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[point]++
	if s.at != point || s.calls[point] != s.n {
		return nil
	}
	s.at = ""
	s.stop()
	close(s.fired)
	return context.Canceled
}

// stoppedEnv starts an operation, confirms it, and stops the service at the
// n-th call of point ("enter", "switch", "clean", "check", "resume").
func stoppedEnv(t *testing.T, point string, n int, mutate ...func(*env)) (*env, *Record) {
	t.Helper()
	s := &stopper{at: point, n: n, calls: map[string]int{}, fired: make(chan struct{})}
	var h *hookMaint
	e := newEnv(t, withHook(&h), func(o *Options) {
		clean, check := o.CleanRedis, o.Check
		o.CleanRedis = func(ctx context.Context) error {
			if err := s.hit("clean"); err != nil {
				return err
			}
			return clean(ctx)
		}
		o.Check = func(ctx context.Context) error {
			if err := s.hit("check"); err != nil {
				return err
			}
			return check(ctx)
		}
	})
	s.stop = e.cancel
	h.afterEnter = func() error { return s.hit("enter") }
	h.failSet = func(phase string) error {
		if phase == maintmode.PhaseResuming {
			return s.hit("resume")
		}
		return nil
	}
	e.db.switchErr = func(int) error { return s.hit("switch") }
	for _, f := range mutate {
		f(e)
	}
	e.process("p1")
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	select {
	case <-s.fired:
	case <-time.After(10 * time.Second):
		t.Fatalf("the service was not stopped at %s", point)
	}
	e.stop()
	return e, rec
}

// assertLeftForRecovery checks that a stopped service neither moved the
// operation to a final step nor gave up.
func assertLeftForRecovery(t *testing.T, e *env, opID string) {
	t.Helper()
	got, err := e.store.Get(context.Background(), opID)
	require.NoError(t, err)
	assert.False(t, Final(got.Step), "the step stays at %s for the recovery", got.Step)
	e.mu.Lock()
	defer e.mu.Unlock()
	assert.Empty(t, e.notes, "stopping is not a failure to report")
}

func TestStoppedMidway_RecoveryFinishesTheOperation(t *testing.T) {
	cases := []struct {
		name  string
		point string
		n     int
		final string
		// content of the server's database at the end
		content string
	}{
		{"after entering the maintenance", "enter", 1, StepAborted, "current"},
		{"while switching the names", "switch", 1, StepAborted, "current"},
		{"while cleaning redis", "clean", 1, StepReverted, "current"},
		{"while checking the restored database", "check", 1, StepReverted, "current"},
		{"while resuming", "resume", 1, StepDone, "restored:20261010T120000Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, rec := stoppedEnv(t, c.point, c.n)
			assertLeftForRecovery(t, e, rec.OpID)
			e.start()
			e.waitStep(rec.OpID, c.final)
			assert.Equal(t, c.content, e.db.content(testDB))
			e.waitMaintenanceGone(5 * time.Second)
			e.mu.Lock()
			defer e.mu.Unlock()
			assert.Empty(t, e.notes)
		})
	}
}

// TestStoppedMidway_WhileReverting stops the service while it puts the
// previous database back after a failed check.
func TestStoppedMidway_WhileReverting(t *testing.T) {
	failCheck := func(e *env) { e.db.fail["check"] = assert.AnError }
	for _, c := range []struct {
		name  string
		point string
		n     int
	}{
		{"while swapping back", "switch", 2},
		{"while cleaning redis after swapping back", "clean", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, rec := stoppedEnv(t, c.point, c.n, failCheck)
			assertLeftForRecovery(t, e, rec.OpID)
			e.start()
			e.waitStep(rec.OpID, StepReverted)
			assert.Equal(t, "current", e.db.content(testDB))
			e.waitMaintenanceGone(5 * time.Second)
			e.mu.Lock()
			defer e.mu.Unlock()
			assert.Empty(t, e.notes)
		})
	}
}

// TestStoppedMidway_WhileWaiting stops the service while it waits for the
// processes, first to let go of the database and then to serve again.
func TestStoppedMidway_WhileWaiting(t *testing.T) {
	t.Run("for held", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.HeldTimeout = time.Hour })
		e.process("p1")
		rec, _ := e.prepare()
		e.db.mu.Lock()
		e.db.conns[testDB] = 1
		e.db.mu.Unlock()
		require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
		e.waitMaintenance(func(st *maintmode.State) bool { return st != nil }, 5*time.Second)
		e.stop()
		assertLeftForRecovery(t, e, rec.OpID)
		e.db.mu.Lock()
		e.db.conns[testDB] = 0
		e.db.mu.Unlock()
		e.start()
		e.waitStep(rec.OpID, StepAborted)
		assert.Equal(t, map[string]string{testDB: "current", "postgres": "maint"}, e.db.names())
		e.waitMaintenanceGone(5 * time.Second)
	})
	t.Run("for the processes to serve again", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.ResumeTimeout = time.Hour })
		p := e.process("p1")
		p.setFailBack(true)
		rec, _ := e.prepare()
		require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
		e.waitStep(rec.OpID, StepResuming)
		e.stop()
		assertLeftForRecovery(t, e, rec.OpID)
		p.setFailBack(false)
		e.start()
		e.waitStep(rec.OpID, StepDone)
		assert.Equal(t, "restored:20261010T120000Z", e.db.content(testDB))
	})
}

// TestStoppedMidway_WhileAborting stops the service while it waits for the
// processes to come back on the current database after giving up waiting
// for held: the new database must not be dropped yet, or the recovery would
// lose track of an operation that still holds the maintenance.
func TestStoppedMidway_WhileAborting(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ResumeTimeout = time.Hour; o.HeldTimeout = 50 * time.Millisecond })
	p := e.process("p1")
	rec, _ := e.prepare()
	e.db.mu.Lock()
	e.db.conns[testDB] = 1
	e.db.mu.Unlock()
	p.setFailBack(true)
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	e.waitStep(rec.OpID, StepAborting)
	e.waitMaintenance(func(st *maintmode.State) bool { return st != nil && st.Phase == maintmode.PhaseResuming }, 5*time.Second)
	e.stop()
	assertLeftForRecovery(t, e, rec.OpID)
	assert.Contains(t, e.db.names(), rec.Target, "dropped only after the processes are back")
	e.db.mu.Lock()
	e.db.conns[testDB] = 0
	e.db.mu.Unlock()
	p.setFailBack(false)
	e.start()
	e.waitStep(rec.OpID, StepAborted)
	assert.NotContains(t, e.db.names(), rec.Target)
	e.waitMaintenanceGone(5 * time.Second)
}

// TestStoppedMidway_WhileRecovering stops the service again while the
// recovery waits for the processes to come back: the new database stays and
// the next start finishes the operation.
func TestStoppedMidway_WhileRecovering(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ResumeTimeout = time.Hour })
	e.stop()
	ctx := context.Background()
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-again", Step: StepAborting, Database: testDB, Target: tgt, Before: before}))
	e.db.mu.Lock()
	e.db.dbs[tgt] = "restored"
	e.db.mu.Unlock()
	require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "restore-again", Kind: maintmode.KindRestore, Phase: maintmode.PhaseHeld}))
	p := e.process("p1")
	p.setFailBack(true)
	e.start()
	e.waitMaintenance(func(st *maintmode.State) bool { return st != nil && st.Phase == maintmode.PhaseResuming }, 5*time.Second)
	e.stop()
	assertLeftForRecovery(t, e, "restore-again")
	assert.Contains(t, e.db.names(), tgt)
	p.setFailBack(false)
	e.start()
	e.waitStep("restore-again", StepAborted)
	assert.NotContains(t, e.db.names(), tgt)
}
