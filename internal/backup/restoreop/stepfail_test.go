package restoreop

import (
	"context"
	"errors"
	"sync"
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
// operation before anything outside depends on it, and that the recovery
// then finishes it from the step that was written (#3463 (6c) M-2).
func TestStepNotWritten(t *testing.T) {
	cases := []struct {
		name      string
		to        string
		failCheck bool
		holdConn  bool
		left      string // the step left for the recovery
		final     string
		content   string
		// cleaned is how often Redis was cleaned before the stop.
		cleaned int
	}{
		{"switched", StepSwitched, false, false, StepEntering, StepReverted, "current", 0},
		{"resuming", StepResuming, false, false, StepSwitched, StepReverted, "current", 1},
		{"reverting", StepReverting, true, false, StepSwitched, StepReverted, "current", 1},
		{"aborting", StepAborting, false, true, StepEntering, StepAborted, "current", 0},
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
			// 外への作用の前で止まっているので、待っている goroutine は無い。
			require.Eventually(t, func() bool { return !e.slot.isHeld() }, 5*time.Second, 5*time.Millisecond)
			got, err := e.store.Get(context.Background(), rec.OpID)
			require.NoError(t, err)
			assert.Equal(t, c.left, got.Step)
			st := e.maintenance()
			require.NotNil(t, st, "the maintenance stays for the recovery")
			assert.NotEqual(t, maintmode.PhaseResuming, st.Phase, "the processes are not let back")
			e.mu.Lock()
			assert.Equal(t, c.cleaned, e.cleaned, "nothing else is done after the step could not be written")
			e.mu.Unlock()
			e.stop()
			e.db.mu.Lock()
			e.db.conns[testDB] = 0
			delete(e.db.fail, "check")
			e.db.mu.Unlock()
			e.start()
			e.waitStep(rec.OpID, c.final)
			assert.Equal(t, c.content, e.db.content(testDB))
			require.Eventually(t, func() bool { return e.maintenance() == nil }, 5*time.Second, 5*time.Millisecond)
		})
	}
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
