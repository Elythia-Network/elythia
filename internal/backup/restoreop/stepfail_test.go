package restoreop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/maintmode"
)

// failOnce fails writing the step to once, and reports when it did.
type failOnce struct {
	to    string
	once  sync.Once
	fired chan struct{}
}

func newFailOnce(to string) *failOnce { return &failOnce{to: to, fired: make(chan struct{})} }

func (f *failOnce) hook(to string) error {
	if to != f.to {
		return nil
	}
	var err error
	f.once.Do(func() {
		err = errors.New("redis: write failed")
		close(f.fired)
	})
	return err
}

func (f *failOnce) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.fired:
	case <-time.After(10 * time.Second):
		t.Fatalf("writing %s did not fail", f.to)
	}
}

// TestStepNotWritten checks that a step that cannot be written stops the
// operation before anything outside depends on it, and that the same run
// then recovers from the step that was written, without a restart (#3463
// (6c) M-2, and H-1 of the third review).
func TestStepNotWritten(t *testing.T) {
	cases := []struct {
		name      string
		to        string
		failCheck bool
		holdConn  bool
		final     string
		// cleaned is how often Redis was cleaned in all: a step that was not
		// written must not be followed by the cleanup it guards.
		cleaned int
	}{
		{"switched", StepSwitched, false, false, StepReverted, 1},
		{"resuming", StepResuming, false, false, StepReverted, 2},
		{"reverting", StepReverting, true, false, StepReverted, 2},
		{"aborting", StepAborting, false, true, StepAborted, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, func(o *Options) { o.HeldTimeout = 100 * time.Millisecond })
			e.process("p1")
			if c.failCheck {
				e.db.fail["check"] = errors.New("doctor failed")
			}
			rec, _ := e.prepare()
			if c.holdConn {
				e.db.mu.Lock()
				e.db.conns[testDB] = 1
				e.db.mu.Unlock()
			}
			f := newFailOnce(c.to)
			e.m.failStep = f.hook
			require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
			f.wait(t)
			e.db.mu.Lock()
			e.db.conns[testDB] = 0
			e.db.mu.Unlock()
			e.waitStep(rec.OpID, c.final)
			assert.Equal(t, "current", e.db.content(testDB))
			require.Eventually(t, func() bool { return e.maintenance() == nil }, 5*time.Second, 5*time.Millisecond)
			require.Eventually(t, func() bool { return !e.slot.isHeld() }, 5*time.Second, 5*time.Millisecond)
			e.mu.Lock()
			defer e.mu.Unlock()
			assert.Equal(t, c.cleaned, e.cleaned)
			assert.Empty(t, e.notes)
		})
	}
}

// TestStepNotWritten_RedisGone checks that an operation whose record cannot
// be read either keeps the judge and the slot, notifies once, and finishes
// when Redis is back.
func TestStepNotWritten_RedisGone(t *testing.T) {
	var clock atomic.Int64
	start := time.Now()
	e := newEnv(t, func(o *Options) {
		o.Now = func() time.Time { return start.Add(time.Duration(clock.Load())) }
	})
	e.process("p1")
	rec, _ := e.prepare()
	var failed atomic.Bool
	e.m.failStep = func(to string) error {
		if to == StepSwitched && failed.CompareAndSwap(false, true) {
			e.mr.SetError("LOADING Redis is loading the dataset in memory")
			return errors.New("redis: write failed")
		}
		return nil
	}
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	require.Eventually(t, func() bool {
		// 書けなかった後だけ時計を進める (それまでは held の待ちの期限に効く)。
		if failed.Load() {
			clock.Add(int64(stalledNotifyAfter))
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.notes) == 1
	}, 5*time.Second, 5*time.Millisecond)
	assert.True(t, e.slot.isHeld(), "the slot is kept while it waits")
	e.mr.SetError("")
	e.waitStep(rec.OpID, StepReverted)
	require.Eventually(t, func() bool { return e.maintenance() == nil }, 5*time.Second, 5*time.Millisecond)
	e.mu.Lock()
	defer e.mu.Unlock()
	assert.Len(t, e.notes, 1, "notified once")
	assert.Contains(t, e.notes[0], "Redis does not answer")
}

// TestRecover_ResumingWithNamesNotSwapped checks that a resuming record whose
// database names are not as after the switch is left to an operator: the
// record may lag behind a revert that already put the previous one back.
func TestRecover_ResumingWithNamesNotSwapped(t *testing.T) {
	names := map[string]string{testDB: "current", testDB + "_rolled_back_1": "restored"}
	e, rec := crashed(t, StepResuming, names, maintmode.PhaseHeld, "")
	e.waitStep(rec.OpID, StepUnknown)
	assert.Equal(t, names, e.db.names())
	st := e.maintenance()
	require.NotNil(t, st)
	assert.Equal(t, maintmode.PhaseHeld, st.Phase, "not resumed")
}
