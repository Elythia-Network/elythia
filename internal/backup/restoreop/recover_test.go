package restoreop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/maintmode"
)

// crashed leaves an operation at step with the given database names and
// maintenance phase ("" for none), as a killed service would, and starts a
// new manager on it.
func crashed(t *testing.T, step string, names map[string]string, phase string, aside string) (*env, *Record) {
	t.Helper()
	e := newEnv(t)
	e.stop()
	e.process("p1")
	ctx := context.Background()
	now := time.Now()
	rec := &Record{
		OpID: "restore-crashed", Step: step, GenerationID: "20261010T120000Z", Database: testDB,
		Target: testDB + "_restore_20261010120000", Before: testDB + "_before_restore_20261010120000",
		Aside: aside, UserID: "u1", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, e.store.create(ctx, rec))
	e.db.mu.Lock()
	e.db.dbs = map[string]string{}
	for k, v := range names {
		e.db.dbs[k] = v
	}
	e.db.mu.Unlock()
	if phase != "" {
		require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: rec.OpID, Kind: maintmode.KindRestore, Phase: phase}))
	}
	e.start()
	return e, rec
}

const (
	tgt    = testDB + "_restore_20261010120000"
	before = testDB + "_before_restore_20261010120000"
)

func TestRecover(t *testing.T) {
	notSwapped := map[string]string{testDB: "current", tgt: "restored"}
	swapped := map[string]string{testDB: "restored", before: "current"}

	t.Run("preparing drops the new database", func(t *testing.T) {
		e, rec := crashed(t, StepPreparing, notSwapped, "", "")
		e.waitStep(rec.OpID, StepFailed)
		assert.Equal(t, map[string]string{testDB: "current"}, e.db.names())
	})
	t.Run("ready drops the new database", func(t *testing.T) {
		e, rec := crashed(t, StepReady, notSwapped, "", "")
		e.waitStep(rec.OpID, StepAborted)
		assert.Equal(t, map[string]string{testDB: "current"}, e.db.names())
	})
	t.Run("aborting in the maintenance resumes on the current database", func(t *testing.T) {
		e, rec := crashed(t, StepAborting, notSwapped, maintmode.PhaseEntering, "")
		e.waitStep(rec.OpID, StepAborted)
		assert.Equal(t, map[string]string{testDB: "current"}, e.db.names())
		require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
	})
	t.Run("entering before the switch resumes and drops", func(t *testing.T) {
		e, rec := crashed(t, StepEntering, notSwapped, maintmode.PhaseHeld, "")
		e.waitStep(rec.OpID, StepAborted)
		assert.Equal(t, map[string]string{testDB: "current"}, e.db.names())
		require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
	})
	t.Run("entering before Enter drops", func(t *testing.T) {
		e, rec := crashed(t, StepEntering, notSwapped, "", "")
		e.waitStep(rec.OpID, StepAborted)
		assert.Equal(t, map[string]string{testDB: "current"}, e.db.names())
		assert.Nil(t, e.maintenance())
	})
	t.Run("entering after the switch puts the previous back", func(t *testing.T) {
		// 名前の入れ替えの後、switched を書く前に落ちた。
		e, rec := crashed(t, StepEntering, swapped, maintmode.PhaseHeld, "")
		got := e.waitStep(rec.OpID, StepReverted)
		names := e.db.names()
		assert.Equal(t, "current", names[testDB])
		assert.Equal(t, "restored", names[got.Aside])
		require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
	})
	t.Run("switched puts the previous back", func(t *testing.T) {
		e, rec := crashed(t, StepSwitched, swapped, maintmode.PhaseHeld, "")
		e.waitStep(rec.OpID, StepReverted)
		assert.Equal(t, "current", e.db.content(testDB))
	})
	t.Run("switched with the names not matching gives up", func(t *testing.T) {
		e, rec := crashed(t, StepSwitched, notSwapped, maintmode.PhaseHeld, "")
		e.waitStep(rec.OpID, StepUnknown)
		assert.Equal(t, notSwapped, e.db.names(), "nothing is touched")
		assert.NotNil(t, e.maintenance())
	})
	t.Run("resuming with the maintenance over is done", func(t *testing.T) {
		e, rec := crashed(t, StepResuming, swapped, "", "")
		e.waitStep(rec.OpID, StepDone)
		assert.Equal(t, swapped, e.db.names())
	})
	t.Run("resuming waits for the processes", func(t *testing.T) {
		e, rec := crashed(t, StepResuming, swapped, maintmode.PhaseResuming, "")
		e.waitStep(rec.OpID, StepDone)
		assert.Equal(t, swapped, e.db.names())
		assert.Nil(t, e.maintenance())
	})
	t.Run("reverting before the swap back", func(t *testing.T) {
		e, rec := crashed(t, StepReverting, swapped, maintmode.PhaseHeld, testDB+"_rolled_back_1")
		e.waitStep(rec.OpID, StepReverted)
		assert.Equal(t, map[string]string{testDB: "current", testDB + "_rolled_back_1": "restored"}, e.db.names())
	})
	t.Run("reverting after the swap back", func(t *testing.T) {
		names := map[string]string{testDB: "current", testDB + "_rolled_back_1": "restored"}
		e, rec := crashed(t, StepReverting, names, maintmode.PhaseHeld, testDB+"_rolled_back_1")
		e.waitStep(rec.OpID, StepReverted)
		assert.Equal(t, names, e.db.names())
		assert.Equal(t, 0, e.db.switches, "it does not swap again")
		require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
	})
	t.Run("unrecognised names give up", func(t *testing.T) {
		weird := map[string]string{testDB: "x", tgt: "y", before: "z"}
		e, rec := crashed(t, StepEntering, weird, maintmode.PhaseHeld, "")
		e.waitStep(rec.OpID, StepUnknown)
		assert.Equal(t, weird, e.db.names())
		assert.Empty(t, e.db.drops)
	})
	t.Run("pg_database unreadable gives up", func(t *testing.T) {
		e := newEnv(t)
		e.stop()
		e.db.fail["existing"] = errors.New("down")
		ctx := context.Background()
		require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-x", Step: StepSwitched, Database: testDB, Target: tgt, Before: before}))
		e.start()
		e.waitStep("restore-x", StepUnknown)
		assert.Empty(t, e.db.drops)
	})
	t.Run("reverting with the maintenance gone gives up", func(t *testing.T) {
		// 状態が消えている = プロセスは戻した DB で動いている。入れ替えると書き込みを失う。
		e, rec := crashed(t, StepSwitched, swapped, "", "")
		e.waitStep(rec.OpID, StepUnknown)
		assert.Equal(t, swapped, e.db.names())
	})
}

// TestRecover_WaitsForTheJobSlot checks that the recovery waits for a job
// that took the slot first (the daemon verifies the newest generation at
// start) instead of giving up.
func TestRecover_WaitsForTheJobSlot(t *testing.T) {
	e := newEnv(t)
	e.stop()
	ctx := context.Background()
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-slot", Step: StepReady, Database: testDB, Target: tgt, Before: before}))
	e.db.mu.Lock()
	e.db.dbs[tgt] = "restored"
	e.db.mu.Unlock()
	e.slot = &slot{}
	release, err := e.slot.acquire()
	require.NoError(t, err)
	busy := e.slot
	e.opts.Acquire = busy.acquire
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.m = New(e.opts)
	e.m.Start(e.ctx)
	t.Cleanup(func() { e.cancel(); e.m.Wait() })
	time.Sleep(30 * time.Millisecond)
	got, err := e.store.Get(ctx, "restore-slot")
	require.NoError(t, err)
	assert.Equal(t, StepReady, got.Step, "waits while the slot is busy")
	release()
	e.waitStep("restore-slot", StepAborted)
	assert.NotContains(t, e.db.names(), tgt)
}

// TestRecover_MaintenanceUnreadable checks that the recovery gives up instead
// of dropping the new database when it cannot tell whether its maintenance
// is still on (dropping and finishing would leave that maintenance with no
// operation to end it).
func TestRecover_MaintenanceUnreadable(t *testing.T) {
	for _, step := range []string{StepAborting, StepEntering} {
		t.Run(step, func(t *testing.T) {
			var h *hookMaint
			e := newEnv(t, withHook(&h))
			e.stop()
			ctx := context.Background()
			require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-unread", Step: step, Database: testDB, Target: tgt, Before: before}))
			e.db.mu.Lock()
			e.db.dbs[tgt] = "restored"
			e.db.mu.Unlock()
			h.failGet = errors.New("redis down")
			e.start()
			e.waitStep("restore-unread", StepUnknown)
			assert.Contains(t, e.db.names(), tgt)
		})
	}
}

// TestRecover_RevertedWhileResumingKeepsRedis checks that a recovery which
// finds the previous database back and the processes already resuming does
// not clean Redis again: they write to it on the previous database.
func TestRecover_RevertedWhileResumingKeepsRedis(t *testing.T) {
	names := map[string]string{testDB: "current", testDB + "_rolled_back_1": "restored"}
	e, rec := crashed(t, StepReverting, names, maintmode.PhaseResuming, testDB+"_rolled_back_1")
	e.waitStep(rec.OpID, StepReverted)
	e.mu.Lock()
	defer e.mu.Unlock()
	assert.Zero(t, e.cleaned)
}

// TestRecover_RereadsAfterTheSlot checks that the recovery reads the record
// again once it has the job slot: an operation this run prepared right after
// the start may have finished while the recovery waited, and must not be
// recovered (its database dropped) from the stale copy.
func TestRecover_RereadsAfterTheSlot(t *testing.T) {
	e := newEnv(t)
	e.stop()
	ctx := context.Background()
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-live", Step: StepReady, Database: testDB, Target: tgt, Before: before}))
	e.db.mu.Lock()
	e.db.dbs[tgt] = "restored"
	e.db.mu.Unlock()
	busy := &slot{}
	release, err := busy.acquire()
	require.NoError(t, err)
	e.slot = busy
	e.opts.Acquire = busy.acquire
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.m = New(e.opts)
	e.m.Start(e.ctx)
	t.Cleanup(func() { e.cancel(); e.m.Wait() })
	time.Sleep(30 * time.Millisecond)
	// 待っている間に、その操作が終わった (この実行の確定で done になった)。
	require.NoError(t, e.store.setStep(ctx, "restore-live", StepReady, StepDone, time.Now()))
	release()
	require.Eventually(t, func() bool { return !busy.isHeld() }, time.Second, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	e.stop()
	assert.Empty(t, e.db.drops, "a finished operation is not recovered")
	got, err := e.store.Get(ctx, "restore-live")
	require.NoError(t, err)
	assert.Equal(t, StepDone, got.Step)
}

// TestRecover_GivesBackTheReservedSlot checks that StartHolding gives the
// slot back whether or not there is something to recover, and that the
// recovery then does not take the slot again.
func TestRecover_GivesBackTheReservedSlot(t *testing.T) {
	for _, unfinished := range []bool{false, true} {
		e := newEnv(t)
		e.stop()
		if unfinished {
			require.NoError(t, e.store.create(context.Background(), &Record{OpID: "restore-held", Step: StepReady, Database: testDB, Target: tgt, Before: before}))
		}
		busy := &slot{}
		release, err := busy.acquire()
		require.NoError(t, err)
		e.opts.Acquire = func() (func(), error) {
			t.Error("the recovery takes the slot again")
			return nil, errors.New("busy")
		}
		e.ctx, e.cancel = context.WithCancel(context.Background())
		e.m = New(e.opts)
		e.m.StartHolding(e.ctx, release)
		t.Cleanup(func() { e.cancel(); e.m.Wait() })
		require.Eventually(t, func() bool { return !busy.isHeld() }, 5*time.Second, 5*time.Millisecond, "unfinished=%v", unfinished)
		if unfinished {
			e.waitStep("restore-held", StepAborted)
		}
	}
}

// TestGiveUpAdvice checks that the notification an operator gets when an
// operation stops names the steps that fit whether the maintenance is still
// on: with it gone, the processes serve the database, so it must not just
// say to rename it.
func TestGiveUpAdvice(t *testing.T) {
	notSwapped := map[string]string{testDB: "current", tgt: "restored"}
	swapped := map[string]string{testDB: "restored", before: "current"}
	note := func(t *testing.T, e *env) string {
		t.Helper()
		// 通知は unknown を書いた後に送るので、届くのを待つ。
		var n []string
		require.Eventually(t, func() bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			n = append([]string(nil), e.notes...)
			return len(n) > 0
		}, 5*time.Second, 5*time.Millisecond)
		require.Len(t, n, 1)
		return n[0]
	}
	t.Run("maintenance on", func(t *testing.T) {
		e, rec := crashed(t, StepSwitched, notSwapped, maintmode.PhaseHeld, "")
		e.waitStep(rec.OpID, StepUnknown)
		n := note(t, e)
		assert.Contains(t, n, "left the maintenance on")
		assert.Contains(t, n, "elythia backup restore -rollback")
		assert.NotContains(t, n, "elythia maintenance enter")
	})
	t.Run("maintenance gone", func(t *testing.T) {
		e, rec := crashed(t, StepSwitched, swapped, "", "")
		e.waitStep(rec.OpID, StepUnknown)
		n := note(t, e)
		assert.Contains(t, n, "The maintenance is not on")
		assert.Contains(t, n, "enter the maintenance first (elythia maintenance enter)")
		assert.NotContains(t, n, "left the maintenance on")
	})
	t.Run("maintenance unreadable", func(t *testing.T) {
		var h *hookMaint
		e := newEnv(t, withHook(&h))
		e.stop()
		require.NoError(t, e.store.create(context.Background(), &Record{OpID: "restore-unread", Step: StepSwitched, Database: testDB, Target: tgt, Before: before}))
		e.db.mu.Lock()
		e.db.dbs = map[string]string{testDB: "restored", before: "current"}
		e.db.mu.Unlock()
		h.failGet = errors.New("redis down")
		e.start()
		e.waitStep("restore-unread", StepUnknown)
		n := note(t, e)
		assert.Contains(t, n, "could not read the maintenance state")
		assert.Contains(t, n, "elythia maintenance status")
	})
}

// TestRecover_GivesUpWhenItCannotTell covers the recoveries that cannot
// decide and must leave the operation to an operator untouched.
func TestRecover_GivesUpWhenItCannotTell(t *testing.T) {
	swapped := map[string]string{testDB: "restored", before: "current"}
	cases := []struct {
		name  string
		step  string
		names map[string]string
		phase string
		aside string
		hook  func(h *hookMaint)
	}{
		{"resuming with the maintenance unreadable", StepResuming, swapped, maintmode.PhaseResuming, "",
			func(h *hookMaint) { h.failGet = errors.New("redis down") }},
		{"resuming that cannot resume", StepResuming, swapped, maintmode.PhaseHeld, "",
			func(h *hookMaint) {
				h.failSet = func(string) error { return errors.New("cannot write") }
			}},
		{"reverting with names of neither side", StepReverting, map[string]string{testDB: "x"}, maintmode.PhaseHeld, testDB + "_rolled_back_1", nil},
		{"reverting with the maintenance unreadable", StepReverting, map[string]string{testDB: "current", testDB + "_rolled_back_1": "restored"}, maintmode.PhaseHeld, testDB + "_rolled_back_1",
			func(h *hookMaint) { h.failGet = errors.New("redis down") }},
		{"an unknown step", "bogus", swapped, maintmode.PhaseHeld, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h *hookMaint
			e := newEnv(t, withHook(&h))
			e.stop()
			ctx := context.Background()
			require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-x", Step: c.step, Database: testDB, Target: tgt, Before: before, Aside: c.aside}))
			e.db.mu.Lock()
			e.db.dbs = map[string]string{}
			for k, v := range c.names {
				e.db.dbs[k] = v
			}
			e.db.mu.Unlock()
			require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "restore-x", Kind: maintmode.KindRestore, Phase: c.phase}))
			if c.hook != nil {
				c.hook(h)
			}
			e.start()
			e.waitStep("restore-x", StepUnknown)
			assert.Equal(t, c.names, e.db.names(), "nothing is renamed")
			assert.Empty(t, e.db.drops)
		})
	}
}

// TestCanStart checks the reasons an operation cannot start, which the
// control API asks before it spends a re-authentication.
func TestCanStart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	require.NoError(t, e.m.CanStart(ctx))
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-left", Step: StepReady, Database: testDB, Target: tgt, Before: before}))
	assert.ErrorIs(t, e.m.CanStart(ctx), ErrBusy)

	r := newEnv(t, func(o *Options) { o.Replicas = true })
	assert.ErrorIs(t, r.m.CanStart(ctx), ErrReplicas)
	require.NoError(t, r.maint.Enter(ctx, maintmode.State{OpID: "m1", Kind: maintmode.KindManual}))
	assert.ErrorIs(t, r.m.CanStart(ctx), ErrMaintenanceActive)
	assert.ErrorIs(t, r.m.NoMaintenance(ctx), ErrMaintenanceActive)

	var h *hookMaint
	f := newEnv(t, withHook(&h))
	h.failGet = errors.New("redis down")
	assert.ErrorContains(t, f.m.CanStart(ctx), "redis down")

	// 予定の取得や検証が枠を持っている間は、再認証の前に断る。
	busy := false
	b := newEnv(t, func(o *Options) { o.SlotBusy = func() bool { return busy } })
	require.NoError(t, b.m.CanStart(ctx))
	busy = true
	assert.ErrorIs(t, b.m.CanStart(ctx), ErrBusy)
}

// TestRecover_ReadsTheMaintenanceAgain checks that the recovery reads the
// maintenance state again a few times instead of handing a short Redis
// outage to an operator, and still gives up when it stays unreadable.
func TestRecover_ReadsTheMaintenanceAgain(t *testing.T) {
	notSwapped := map[string]string{testDB: "current", tgt: "restored"}
	swapped := map[string]string{testDB: "restored", before: "current"}
	for _, c := range []struct {
		name  string
		step  string
		names map[string]string
		fails int
		final string
	}{
		{"aborting, a short outage", StepAborting, notSwapped, maintenanceReadAttempts - 1, StepAborted},
		{"entering, a short outage", StepEntering, notSwapped, maintenanceReadAttempts - 1, StepAborted},
		{"resuming, a short outage", StepResuming, swapped, maintenanceReadAttempts - 1, StepDone},
		{"aborting, unreadable", StepAborting, notSwapped, maintenanceReadAttempts, StepUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			var h *hookMaint
			e := newEnv(t, withHook(&h))
			e.stop()
			e.process("p1")
			ctx := context.Background()
			require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-x", Step: c.step, Database: testDB, Target: tgt, Before: before}))
			e.db.mu.Lock()
			e.db.dbs = map[string]string{}
			for k, v := range c.names {
				e.db.dbs[k] = v
			}
			e.db.mu.Unlock()
			require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "restore-x", Kind: maintmode.KindRestore, Phase: maintmode.PhaseHeld}))
			h.mu.Lock()
			h.failGets = c.fails
			h.mu.Unlock()
			e.start()
			e.waitStep("restore-x", c.final)
		})
	}
}

// TestRecover_KeepsReadingTheRecord checks that the recovery reads the
// unfinished record again until Redis answers, giving back the reserved
// slot meanwhile so that backups go on (#3463 (6c) L-4).
func TestRecover_KeepsReadingTheRecord(t *testing.T) {
	e := newEnv(t)
	e.stop()
	ctx := context.Background()
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-left", Step: StepAborting, Database: testDB, Target: tgt, Before: before}))
	e.db.mu.Lock()
	e.db.dbs[tgt] = "restored"
	e.db.mu.Unlock()
	reserved := &slot{}
	release, err := reserved.acquire()
	require.NoError(t, err)
	e.mr.SetError("LOADING Redis is loading the dataset in memory")
	e.slot = &slot{}
	e.opts.Acquire = e.slot.acquire
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.m = New(e.opts)
	e.m.StartHolding(e.ctx, release)
	t.Cleanup(func() { e.cancel(); e.m.Wait() })
	require.Eventually(t, func() bool { return !reserved.isHeld() }, 5*time.Second, 5*time.Millisecond,
		"the reserved slot is given back while the record cannot be read")
	e.mr.SetError("")
	e.waitStep("restore-left", StepAborted)
	assert.NotContains(t, e.db.names(), tgt)
}

// TestRecover_RereadsRightAfterTheSlot finishes the operation between the
// recovery's read and its taking the slot: it must read again after the
// slot, not act on the copy it read before.
func TestRecover_RereadsRightAfterTheSlot(t *testing.T) {
	e := newEnv(t)
	e.stop()
	ctx := context.Background()
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-live", Step: StepReady, Database: testDB, Target: tgt, Before: before}))
	e.db.mu.Lock()
	e.db.dbs[tgt] = "restored"
	e.db.mu.Unlock()
	s := &slot{}
	e.slot = s
	e.opts.Acquire = func() (func(), error) {
		// 枠を取る直前に、この実行の操作として終わった。
		require.NoError(t, e.store.setStep(ctx, "restore-live", StepReady, StepDone, time.Now()))
		return s.acquire()
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.m = New(e.opts)
	e.m.Start(e.ctx)
	e.m.Wait()
	e.cancel()
	assert.Empty(t, e.db.drops, "a finished operation is not recovered")
	assert.False(t, s.isHeld())
}
