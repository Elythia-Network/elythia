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
		e.waitMaintenanceGone(5*time.Second, "the maintenance it wrote is ended")
		assert.Equal(t, "current", e.db.content(testDB))
	})
	t.Run("state written late", func(t *testing.T) {
		// Enter の書き込みが、失敗を返した後で届く。読み直しで決めると、自分の entering
		// が残ったまま aborted になる。
		var h *hookMaint
		e := newEnv(t, withHook(&h))
		e.process("p1")
		rec, _ := e.prepare()
		h.mu.Lock()
		h.failEnter = errors.New("i/o timeout")
		h.late = &maintmode.State{OpID: rec.OpID, Kind: maintmode.KindRestore}
		h.mu.Unlock()
		require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
		e.waitStep(rec.OpID, StepAborted)
		e.waitMaintenanceGone(5 * time.Second)
	})
	t.Run("state cannot be cleared", func(t *testing.T) {
		var h *hookMaint
		e := newEnv(t, withHook(&h))
		e.process("p1")
		rec, _ := e.prepare()
		h.afterEnter = func() error {
			h.mu.Lock()
			h.failClear = errors.New("redis down")
			h.mu.Unlock()
			return errors.New("i/o timeout reading the reply")
		}
		require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
		e.waitStep(rec.OpID, StepUnknown)
		assert.Contains(t, e.db.names(), rec.Target, "nothing is dropped while it cannot tell")
	})
}
