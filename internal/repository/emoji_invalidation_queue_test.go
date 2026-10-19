package repository_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/repository"
)

// publishRecorder records what an EmojiInvalidationQueue publishes.
type publishRecorder struct {
	mu        sync.Mutex
	published []repository.EmojiCacheInvalidation
}

func (r *publishRecorder) publish(ev repository.EmojiCacheInvalidation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, ev)
}

func (r *publishRecorder) result() []repository.EmojiCacheInvalidation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]repository.EmojiCacheInvalidation(nil), r.published...)
}

func ids(id string) repository.EmojiCacheInvalidation {
	return repository.EmojiCacheInvalidation{IDs: []string{id}}
}

// stopReturns calls q.Stop(ctx) and reports whether it returned within
// limit. It does not touch t, so it can be called from any goroutine.
func stopReturns(q *repository.EmojiInvalidationQueue, ctx context.Context, limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		q.Stop(ctx)
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

// stopWithin calls q.Stop(ctx) and fails the test when it does not return
// within limit. Call it from the test goroutine only.
func stopWithin(t *testing.T, q *repository.EmojiInvalidationQueue, ctx context.Context, limit time.Duration) {
	t.Helper()
	if !stopReturns(q, ctx, limit) {
		t.Fatalf("Stop did not return within %s", limit)
	}
}

// TestEmojiInvalidationQueue_StartPublishesWhatWasEnqueuedBefore checks that
// invalidations enqueued before Start are kept and published once the queue
// has started, and that nothing is published before Start (#3705).
func TestEmojiInvalidationQueue_StartPublishesWhatWasEnqueuedBefore(t *testing.T) {
	var rec publishRecorder
	q := repository.NewEmojiInvalidationQueue(8, rec.publish)
	q.Enqueue(ids("a"))
	q.Enqueue(ids("b"))
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, rec.result(), "the queue published before Start")

	q.Start(context.Background())
	t.Cleanup(func() { q.Stop(context.Background()) })
	assert.Eventually(t, func() bool { return len(rec.result()) == 2 }, 2*time.Second, 5*time.Millisecond,
		"what was enqueued before Start was not published after it")
	assert.Equal(t, []repository.EmojiCacheInvalidation{ids("a"), ids("b")}, rec.result())

	// 起動の後に積んだ分も送る。
	q.Enqueue(ids("c"))
	assert.Eventually(t, func() bool { return len(rec.result()) == 3 }, 2*time.Second, 5*time.Millisecond,
		"what was enqueued after Start was not published")
}

// TestEmojiInvalidationQueue_StopWaitsForRunToDrain checks that Stop after
// Start cancels Run and returns only after Run has published what is pending
// (#3705).
func TestEmojiInvalidationQueue_StopWaitsForRunToDrain(t *testing.T) {
	entered := make(chan struct{}, 8)
	gate := make(chan struct{})
	var rec publishRecorder
	q := repository.NewEmojiInvalidationQueue(8, func(ev repository.EmojiCacheInvalidation) {
		entered <- struct{}{}
		<-gate
		rec.publish(ev)
	})
	q.Start(context.Background())
	q.Enqueue(ids("a"))
	select {
	case <-entered: // Runがaのpublishで止まっている
	case <-time.After(2 * time.Second):
		t.Fatal("Run never started publishing")
	}
	q.Enqueue(ids("b"))

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		q.Stop(context.Background())
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while Run was still publishing")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after Run drained")
	}
	assert.Equal(t, []repository.EmojiCacheInvalidation{ids("a"), ids("b")}, rec.result(),
		"Stop returned before Run published what was pending")
}

// TestEmojiInvalidationQueue_StopIsBoundedByItsCtx checks that Stop after
// Start returns when its ctx is done, even if Run is still publishing.
func TestEmojiInvalidationQueue_StopIsBoundedByItsCtx(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	q := repository.NewEmojiInvalidationQueue(8, func(repository.EmojiCacheInvalidation) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	t.Cleanup(func() { close(release) })
	q.Start(context.Background())
	q.Enqueue(ids("a"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never started publishing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stopWithin(t, q, ctx, 2*time.Second)
}

// TestEmojiInvalidationQueue_StopWithoutStartPublishesPending checks that
// Stop on a queue that was never started does not wait for a Run that does
// not exist, and publishes what is pending synchronously before it returns
// (#3705).
func TestEmojiInvalidationQueue_StopWithoutStartPublishesPending(t *testing.T) {
	var rec publishRecorder
	q := repository.NewEmojiInvalidationQueue(8, rec.publish)
	q.Enqueue(ids("a"))
	q.Enqueue(ids("b"))
	// 期限の無いctxで止める(構築に失敗した世代の後始末と同じ)。Runを待つと返らない。
	stopWithin(t, q, context.Background(), 2*time.Second)
	assert.Equal(t, []repository.EmojiCacheInvalidation{ids("a"), ids("b")}, rec.result(),
		"Stop without Start did not publish what was pending before it returned")
}

// TestEmojiInvalidationQueue_StopWithoutStartIsBoundedByTheDrainTimeout
// checks that the synchronous drain of Stop without Start stops at the drain
// timeout.
func TestEmojiInvalidationQueue_StopWithoutStartIsBoundedByTheDrainTimeout(t *testing.T) {
	var rec publishRecorder
	q := repository.NewEmojiInvalidationQueue(8, func(ev repository.EmojiCacheInvalidation) {
		rec.publish(ev)
		time.Sleep(40 * time.Millisecond)
	})
	repository.SetEmojiInvalidationDrainTimeout(q, 60*time.Millisecond)
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		q.Enqueue(ids(id))
	}
	stopWithin(t, q, context.Background(), 2*time.Second)
	n := len(rec.result())
	assert.Positive(t, n)
	assert.Less(t, n, 6, "the drain of Stop without Start is bounded by the drain timeout")
}

// TestEmojiInvalidationQueue_StopWithoutStartIsBoundedByItsCtx checks that
// the synchronous drain of Stop without Start also stops when the shutdown
// ctx is done, before the drain timeout: an already-done ctx publishes
// nothing, and a short one stops the drain early.
func TestEmojiInvalidationQueue_StopWithoutStartIsBoundedByItsCtx(t *testing.T) {
	newQueue := func(rec *publishRecorder) *repository.EmojiInvalidationQueue {
		q := repository.NewEmojiInvalidationQueue(8, func(ev repository.EmojiCacheInvalidation) {
			rec.publish(ev)
			time.Sleep(40 * time.Millisecond)
		})
		for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
			q.Enqueue(ids(id))
		}
		return q
	}
	t.Run("ctx already done", func(t *testing.T) {
		var rec publishRecorder
		q := newQueue(&rec)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stopWithin(t, q, ctx, 2*time.Second)
		assert.Empty(t, rec.result(), "Stop without Start published after its ctx was done")
	})
	t.Run("ctx done during the drain", func(t *testing.T) {
		var rec publishRecorder
		q := newQueue(&rec)
		// 締め切り(既定の2秒)より先にctxが切れる。ctxを見なければ6件とも送る。
		// 送った件数の下限は見ない。遅い環境では、送り始める前にctxが切れて0件になりうる。
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		stopWithin(t, q, ctx, 2*time.Second)
		assert.Less(t, len(rec.result()), 6, "the drain of Stop without Start did not stop when its ctx was done")
	})
}

// TestEmojiInvalidationQueue_StartAndStopTwice checks that a second Start
// starts no second Run (each invalidation is published once) and that a
// second Stop does nothing.
func TestEmojiInvalidationQueue_StartAndStopTwice(t *testing.T) {
	var rec publishRecorder
	q := repository.NewEmojiInvalidationQueue(8, rec.publish)
	q.Start(context.Background())
	q.Start(context.Background())
	for _, id := range []string{"a", "b", "c", "d"} {
		q.Enqueue(ids(id))
	}
	stopWithin(t, q, context.Background(), 2*time.Second)
	want := []repository.EmojiCacheInvalidation{ids("a"), ids("b"), ids("c"), ids("d")}
	assert.Equal(t, want, rec.result())

	// 2回目のStopは何もしない。止めた後に積んだ分は送らない。2回目のStartが
	// 2つ目のRunを立てていれば、1つ目は止まらずに残り、ここで積んだ分を送る。
	q.Enqueue(ids("e"))
	stopWithin(t, q, context.Background(), 2*time.Second)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, want, rec.result(), "a sender kept running after Stop, or a second Stop published again")
}

// TestEmojiInvalidationQueue_SecondStopWithoutStartDoesNothing checks that
// a second Stop on a queue that was never started does not publish what was
// enqueued after the first one.
func TestEmojiInvalidationQueue_SecondStopWithoutStartDoesNothing(t *testing.T) {
	var rec publishRecorder
	q := repository.NewEmojiInvalidationQueue(8, rec.publish)
	q.Enqueue(ids("a"))
	stopWithin(t, q, context.Background(), 2*time.Second)
	q.Enqueue(ids("b"))
	stopWithin(t, q, context.Background(), 2*time.Second)
	assert.Equal(t, []repository.EmojiCacheInvalidation{ids("a")}, rec.result(), "a second Stop published again")
}

// TestEmojiInvalidationQueue_StartAfterStopStartsNothing checks that Start
// after Stop does not run Run: nothing would stop it (#3705).
func TestEmojiInvalidationQueue_StartAfterStopStartsNothing(t *testing.T) {
	var rec publishRecorder
	q := repository.NewEmojiInvalidationQueue(8, rec.publish)
	stopWithin(t, q, context.Background(), 2*time.Second)
	q.Start(context.Background())
	q.Enqueue(ids("a"))
	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, rec.result(), "Start after Stop ran the sender")
}

// TestEmojiInvalidationQueue_ConcurrentStartAndStop runs Start, Stop and
// Enqueue concurrently (for -race) and checks that Stop always returns and
// that no sender is left running after it.
func TestEmojiInvalidationQueue_ConcurrentStartAndStop(t *testing.T) {
	for range 50 {
		var rec publishRecorder
		q := repository.NewEmojiInvalidationQueue(8, rec.publish)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(3)
			go func() { defer wg.Done(); q.Start(context.Background()) }()
			go func() { defer wg.Done(); q.Enqueue(ids("x")) }()
			go func() {
				defer wg.Done()
				// t.Fatalfはテストのgoroutineからしか呼べないので、ここではErrorfで報告する。
				if !stopReturns(q, context.Background(), 2*time.Second) {
					t.Errorf("Stop did not return within 2s")
				}
			}()
		}
		wg.Wait()
		n := len(rec.result())
		// 止めた後は、積んでも送られない(送るgoroutineが残っていない)。
		q.Enqueue(ids("after"))
		time.Sleep(time.Millisecond)
		require.Len(t, rec.result(), n, "a sender kept running after Stop")
	}
}
