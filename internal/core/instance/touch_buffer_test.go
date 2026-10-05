package instance_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingTarget struct {
	mu    sync.Mutex
	hosts []string
	calls atomic.Int64
}

func (r *recordingTarget) MarkRequestReceived(host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.Add(1)
	r.hosts = append(r.hosts, host)
	return nil
}

// 同一 host を 1000 回 MarkRequestReceived しても、flush 1 回で 1 call
// に縮退すること。これが #569 のメイン期待効果。
func TestTouchBuffer_CoalescesSameHost(t *testing.T) {
	target := &recordingTarget{}
	buf := instance.NewTouchBuffer(target, 50*time.Millisecond)

	for range 1000 {
		require.NoError(t, buf.MarkRequestReceived("remote.example"))
	}
	buf.FlushNow()

	assert.Equal(t, int64(1), target.calls.Load(), "1000 calls must coalesce to 1 underlying MarkRequestReceived")
}

// 異なる host は別 entry として保持され、flush 時にすべて適用される。
func TestTouchBuffer_PreservesDistinctHosts(t *testing.T) {
	target := &recordingTarget{}
	buf := instance.NewTouchBuffer(target, time.Hour) // bg flush 起こさない

	hosts := []string{"a.example", "b.example", "c.example"}
	for _, h := range hosts {
		require.NoError(t, buf.MarkRequestReceived(h))
	}
	buf.FlushNow()
	assert.Equal(t, int64(3), target.calls.Load())
	assert.ElementsMatch(t, hosts, target.hosts)
}

// 空 host は no-op。
func TestTouchBuffer_EmptyHostNoOp(t *testing.T) {
	target := &recordingTarget{}
	buf := instance.NewTouchBuffer(target, time.Hour)
	require.NoError(t, buf.MarkRequestReceived(""))
	buf.FlushNow()
	assert.Zero(t, target.calls.Load())
}

// Start で起動した bg goroutine は Close で確実に停止する。
func TestTouchBuffer_StartStop(t *testing.T) {
	target := &recordingTarget{}
	buf := instance.NewTouchBuffer(target, 10*time.Millisecond)
	buf.Start(context.Background())
	require.NoError(t, buf.MarkRequestReceived("h"))
	time.Sleep(40 * time.Millisecond)
	buf.Close()
	assert.GreaterOrEqual(t, target.calls.Load(), int64(1))
}

// Start を呼ばずに Close しても deadlock せず、pending は best-effort flush
// される (#580 Devin BUG-1 対応)。
func TestTouchBuffer_CloseWithoutStartDoesNotDeadlock(t *testing.T) {
	target := &recordingTarget{}
	buf := instance.NewTouchBuffer(target, time.Hour)
	require.NoError(t, buf.MarkRequestReceived("h"))

	done := make(chan struct{})
	go func() {
		buf.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close should not block when Start was never called")
	}
	assert.Equal(t, int64(1), target.calls.Load(),
		"Close should still flush pending entries best-effort")
}

// Start を 2 度呼んでも bg goroutine は 1 個だけ。
func TestTouchBuffer_StartIsIdempotent(t *testing.T) {
	target := &recordingTarget{}
	buf := instance.NewTouchBuffer(target, 10*time.Millisecond)
	buf.Start(context.Background())
	buf.Start(context.Background())
	require.NoError(t, buf.MarkRequestReceived("h"))
	time.Sleep(40 * time.Millisecond)
	buf.Close()
	// goroutine 1 つ分の挙動で済むことを confirm (count >= 1)。
	assert.GreaterOrEqual(t, target.calls.Load(), int64(1))
}

// Context cancel で bg flush + return すること。
func TestTouchBuffer_StopsOnContextCancel(t *testing.T) {
	target := &recordingTarget{}
	buf := instance.NewTouchBuffer(target, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	buf.Start(ctx)
	require.NoError(t, buf.MarkRequestReceived("h"))
	cancel()
	// goroutine 終了を待つため Close する。Close は既に stop しているので
	// すぐ return する想定。
	done := make(chan struct{})
	go func() {
		buf.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close should return quickly after context cancel")
	}
	assert.GreaterOrEqual(t, target.calls.Load(), int64(1))
}
