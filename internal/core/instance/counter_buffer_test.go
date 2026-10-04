package instance_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/instance"
)

type counterCall struct {
	host   string
	column string
	delta  int
}

type recordingCounterTarget struct {
	mu    sync.Mutex
	calls []counterCall
	err   error
}

func (r *recordingCounterTarget) IncrementCount(host, column string, delta int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, counterCall{host: host, column: column, delta: delta})
	return r.err
}

func (r *recordingCounterTarget) snapshot() []counterCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]counterCall(nil), r.calls...)
}

// TestCounterBuffer_CoalescesPerHostAndColumn pins that increments are summed
// per (host, column) and written once per flush.
func TestCounterBuffer_CoalescesPerHostAndColumn(t *testing.T) {
	target := &recordingCounterTarget{}
	buf := instance.NewCounterBuffer(target, time.Hour)

	for range 5 {
		buf.Add("a.example", instance.CounterNotesCount, 1)
	}
	buf.Add("a.example", instance.CounterNotesCount, -2)
	buf.Add("a.example", instance.CounterUsersCount, 1)
	buf.Add("b.example", instance.CounterNotesCount, 1)
	buf.FlushNow()

	assert.ElementsMatch(t, []counterCall{
		{host: "a.example", column: "notesCount", delta: 3},
		{host: "a.example", column: "usersCount", delta: 1},
		{host: "b.example", column: "notesCount", delta: 1},
	}, target.snapshot())

	// flush 済みの値は次の flush で再送しない。
	buf.FlushNow()
	assert.Len(t, target.snapshot(), 3)
}

// TestCounterBuffer_SkipsNoOps pins the inputs that must never reach the
// database: a net-zero window, empty hosts, zero deltas and unknown columns
// (the column name is interpolated into SQL).
func TestCounterBuffer_SkipsNoOps(t *testing.T) {
	target := &recordingCounterTarget{}
	buf := instance.NewCounterBuffer(target, time.Hour)

	buf.Add("a.example", instance.CounterNotesCount, 1)
	buf.Add("a.example", instance.CounterNotesCount, -1)
	buf.Add("", instance.CounterNotesCount, 1)
	buf.Add("a.example", instance.CounterUsersCount, 0)
	buf.Add("a.example", "followersCount", 1)
	buf.Add("a.example", `"notesCount" = 0; --`, 1)
	buf.FlushNow()

	assert.Empty(t, target.snapshot())
}

// TestCounterBuffer_FlushErrorDropsWindow pins that a failed write is logged
// and not retried (best-effort statistics, like upstream's CollapsedQueue).
func TestCounterBuffer_FlushErrorDropsWindow(t *testing.T) {
	target := &recordingCounterTarget{err: errors.New("db down")}
	buf := instance.NewCounterBuffer(target, time.Hour)

	buf.Add("a.example", instance.CounterNotesCount, 2)
	buf.FlushNow()
	buf.FlushNow()

	assert.Equal(t, []counterCall{{host: "a.example", column: "notesCount", delta: 2}}, target.snapshot())
}

// TestCounterBuffer_CloseWithoutStartFlushes pins that Close before Start
// still writes the pending window and does not block.
func TestCounterBuffer_CloseWithoutStartFlushes(t *testing.T) {
	target := &recordingCounterTarget{}
	buf := instance.NewCounterBuffer(target, 0)

	buf.Add("a.example", instance.CounterUsersCount, 1)
	buf.Close()

	assert.Equal(t, []counterCall{{host: "a.example", column: "usersCount", delta: 1}}, target.snapshot())
}

// TestCounterBuffer_BackgroundFlushAndClose pins the ticker flush, the final
// flush on Close, and that Start and Close are idempotent.
func TestCounterBuffer_BackgroundFlushAndClose(t *testing.T) {
	target := &recordingCounterTarget{}
	buf := instance.NewCounterBuffer(target, 10*time.Millisecond)
	buf.Start(context.Background())
	buf.Start(context.Background())

	buf.Add("a.example", instance.CounterNotesCount, 1)
	require.Eventually(t, func() bool { return len(target.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)

	buf.Add("b.example", instance.CounterNotesCount, 1)
	buf.Close()
	buf.Close()
	assert.Len(t, target.snapshot(), 2, "Close must flush the remaining window")
}

// TestCounterBuffer_ContextCancelFlushes pins the final flush when the
// context passed to Start is cancelled.
func TestCounterBuffer_ContextCancelFlushes(t *testing.T) {
	target := &recordingCounterTarget{}
	buf := instance.NewCounterBuffer(target, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	buf.Start(ctx)

	buf.Add("a.example", instance.CounterNotesCount, 4)
	cancel()
	require.Eventually(t, func() bool { return len(target.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, 4, target.snapshot()[0].delta)
	buf.Close()
}
