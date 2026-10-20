package fedrule

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStopTestStore returns a HitStore on an in-process Redis, so the write
// loop runs without a Redis server.
func newStopTestStore(t *testing.T) *HitStore {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewHitStore(rdb)
}

// runningLoops counts the goroutines that run the write loop of a HitStore.
func runningLoops() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "fedrule.(*HitStore).Start.func1(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// requireNoLoop waits until no write loop of a HitStore is running.
func requireNoLoop(t *testing.T, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return runningLoops() == 0 }, 5*time.Second, 10*time.Millisecond, msg)
}

// assertStaysQueued records a hit and checks that no loop takes it off the
// queue or writes it.
func assertStaysQueued(t *testing.T, s *HitStore, ruleID string) {
	t.Helper()
	s.Record(Hit{RuleID: ruleID, At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, s.queue, 1, "a loop took the hit off the queue")
	samples, err := s.Samples(context.Background(), ruleID, 0)
	require.NoError(t, err)
	assert.Empty(t, samples, "a loop wrote the hit")
}

// TestHitStore_StopStopsTheLoop checks that Stop ends the write loop Start
// started, without the ctx given to Start being cancelled (#3717), and that
// hits recorded after it are no longer written.
func TestHitStore_StopStopsTheLoop(t *testing.T) {
	requireNoLoop(t, "前提: ほかのテストのloopが残っていない")
	s := newStopTestStore(t)
	s.Start(context.Background())
	s.Record(Hit{RuleID: "before", At: time.Now()})
	require.Eventually(t, func() bool {
		got, _ := s.Samples(context.Background(), "before", 0)
		return len(got) == 1
	}, 5*time.Second, 10*time.Millisecond, "the loop did not write a hit before Stop")
	require.Equal(t, 1, runningLoops())

	s.Stop(context.Background())
	requireNoLoop(t, "Stop did not end the loop")
	assertStaysQueued(t, s, "after")
	// 2回目の停止は何もしない。
	s.Stop(context.Background())
}

// TestHitStore_StopEndsEveryLoop checks that Stop ends every loop when Start
// was called more than once.
func TestHitStore_StopEndsEveryLoop(t *testing.T) {
	requireNoLoop(t, "前提: ほかのテストのloopが残っていない")
	s := newStopTestStore(t)
	s.Start(context.Background())
	s.Start(context.Background())
	require.Equal(t, 2, runningLoops())
	s.Stop(context.Background())
	requireNoLoop(t, "Stop did not end every loop")
}

// TestHitStore_StartAfterStopStartsNothing checks that Stop before Start is
// safe and keeps the later Start from starting a loop, as cancelling the ctx
// of Start did before #3717.
func TestHitStore_StartAfterStopStartsNothing(t *testing.T) {
	requireNoLoop(t, "前提: ほかのテストのloopが残っていない")
	s := newStopTestStore(t)
	s.Stop(context.Background())
	s.Start(context.Background())
	assert.Zero(t, runningLoops(), "Start after Stop started a loop")
	assertStaysQueued(t, s, "afterStop")
}

// TestHitStore_ConcurrentStartAndStop checks, with -race, that Start and
// Stop called at the same time leave no loop running, whichever comes first.
func TestHitStore_ConcurrentStartAndStop(t *testing.T) {
	requireNoLoop(t, "前提: ほかのテストのloopが残っていない")
	for range 50 {
		s := newStopTestStore(t)
		var wg sync.WaitGroup
		wg.Go(func() { s.Start(context.Background()) })
		wg.Go(func() { s.Stop(context.Background()) })
		wg.Wait()
		requireNoLoop(t, "a loop outlived a concurrent Stop")
	}
}

// TestHitStore_StopWithoutRedis checks that Stop is a no-op on a nil HitStore
// and on one without Redis.
func TestHitStore_StopWithoutRedis(t *testing.T) {
	requireNoLoop(t, "前提: ほかのテストのloopが残っていない")
	var s *HitStore
	s.Stop(context.Background())
	s = NewHitStore(nil)
	s.Stop(context.Background())
	s.Start(context.Background())
	assert.Zero(t, runningLoops())
}
