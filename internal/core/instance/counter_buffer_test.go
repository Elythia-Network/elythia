package instance_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/instance"
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

// TestCounterBuffer_WritesThroughAfterClose checks that an increment added
// after Close is written at once instead of being left in a pending map
// nothing flushes any more (#3662).
func TestCounterBuffer_WritesThroughAfterClose(t *testing.T) {
	target := &recordingCounterTarget{}
	buf := instance.NewCounterBuffer(target, time.Hour)
	buf.Start(context.Background())
	buf.Close()
	buf.Add("late.example", instance.CounterNotesCount, 3)
	buf.Add("late.example", instance.CounterNotesCount, 0)
	assert.Equal(t, []counterCall{{host: "late.example", column: "notesCount", delta: 3}}, target.snapshot())
}

// TestCounterBuffer_WritesThroughAfterContextCancel checks the same once the
// flush loop has stopped because its context was canceled.
func TestCounterBuffer_WritesThroughAfterContextCancel(t *testing.T) {
	target := &recordingCounterTarget{}
	buf := instance.NewCounterBuffer(target, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	buf.Start(ctx)
	buf.Add("first.example", instance.CounterUsersCount, 1)
	cancel()
	require.Eventually(t, func() bool { return len(target.snapshot()) == 1 }, 2*time.Second, 5*time.Millisecond)
	buf.Add("late.example", instance.CounterUsersCount, 1)
	assert.Len(t, target.snapshot(), 2)
}

// blockingCounterTarget blocks its first write until release is closed.
type blockingCounterTarget struct {
	recordingCounterTarget
	first   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (r *blockingCounterTarget) IncrementCount(host, column string, delta int) error {
	if r.first.CompareAndSwap(false, true) {
		close(r.entered)
		<-r.release
	}
	return r.recordingCounterTarget.IncrementCount(host, column, delta)
}

// TestCounterBuffer_AddDuringTheLastFlushIsCountedOnce checks that an
// increment added while the last flush is still writing is written exactly
// once: at once, and not also left in the pending map (#3662). 加算なので、
// 二重に書くと値が狂う。
func TestCounterBuffer_AddDuringTheLastFlushIsCountedOnce(t *testing.T) {
	target := &blockingCounterTarget{entered: make(chan struct{}), release: make(chan struct{})}
	buf := instance.NewCounterBuffer(target, time.Hour)
	buf.Start(context.Background())
	buf.Add("x.example", instance.CounterNotesCount, 3)
	closed := make(chan struct{})
	go func() {
		buf.Close()
		close(closed)
	}()
	<-target.entered // 最後の flush が 3 を書いている途中
	buf.Add("x.example", instance.CounterNotesCount, 2)
	close(target.release)
	<-closed
	// 閉じた後の pending に何か残っていれば、ここで書かれて合計がずれる。
	buf.FlushNow()
	sum := 0
	for _, c := range target.snapshot() {
		sum += c.delta
	}
	assert.Equal(t, 5, sum, "calls: %+v", target.snapshot())
}
