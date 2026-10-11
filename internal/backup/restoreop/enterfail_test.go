package restoreop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnterFailedAfterWriting checks that a failed Enter whose state was
// written anyway is ended before the operation is marked aborted (M-1).
func TestEnterFailedAfterWriting(t *testing.T) {
	t.Run("state written", func(t *testing.T) {
		var h *hookMaint
		e := newEnv(t, withHook(&h))
		e.process("p1")
		rec, _ := e.prepare()
		h.afterEnter = func() error { return errors.New("i/o timeout reading the reply") }
		require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
		e.waitStep(rec.OpID, StepAborted)
		require.Eventually(t, func() bool { return e.maintenance() == nil }, 5*time.Second, 5*time.Millisecond,
			"the maintenance it wrote is ended")
		assert.Equal(t, "current", e.db.content(testDB))
	})
	t.Run("state unreadable", func(t *testing.T) {
		var h *hookMaint
		e := newEnv(t, withHook(&h))
		e.process("p1")
		rec, _ := e.prepare()
		h.afterEnter = func() error {
			h.mu.Lock()
			h.failGet = errors.New("redis down")
			h.mu.Unlock()
			return errors.New("i/o timeout reading the reply")
		}
		require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
		e.waitStep(rec.OpID, StepUnknown)
		assert.Contains(t, e.db.names(), rec.Target, "nothing is dropped while it cannot tell")
	})
}
