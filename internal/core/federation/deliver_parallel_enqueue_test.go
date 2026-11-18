package federation_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// concurrencyEnqueuer records every EnqueueDeliver call and the largest
// number of calls that were in flight at once.
type concurrencyEnqueuer struct {
	stubEnqueuer
	hold time.Duration
	// failInbox は、積むと fail で失敗させる inbox。ほかは hold だけ待って成功する。
	failInbox string
	fail      error
	// panicInbox は、積むと panic させる inbox。
	panicInbox string
	attempts   atomic.Int64
	inFlight   atomic.Int64
	maxMu      sync.Mutex
	max        int64
	countMu    sync.Mutex
	counts     map[string]int
}

func (e *concurrencyEnqueuer) EnqueueDeliver(payload queue.DeliverPayload, _ ...driver.EnqueueOption) error {
	e.attempts.Add(1)
	n := e.inFlight.Add(1)
	defer e.inFlight.Add(-1)
	e.maxMu.Lock()
	e.max = max(e.max, n)
	e.maxMu.Unlock()
	if payload.Inbox == e.panicInbox {
		panic("enqueuer exploded")
	}
	if payload.Inbox == e.failInbox {
		return e.fail
	}
	time.Sleep(e.hold)
	e.countMu.Lock()
	defer e.countMu.Unlock()
	if e.counts == nil {
		e.counts = map[string]int{}
	}
	e.counts[payload.Inbox]++
	return nil
}

func newConcurrencyDeliverService(t *testing.T, enq *concurrencyEnqueuer) *federation.DeliverService {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	svc := federation.NewDeliverService(enq, userRepo, testutil.NewMockFollowingRepository(), keypairRepo, activitypub.NewURLBuilder("https://example.com"), nil)
	installLocalSigner(t, userRepo, keypairRepo)
	return svc
}

func manyInboxes(n int) []string {
	inboxes := make([]string, 0, n)
	for i := range n {
		inboxes = append(inboxes, fmt.Sprintf("https://remote%d.example/inbox", i))
	}
	return inboxes
}

// TestDeliverActivity_ManyInboxesEnqueueEachOnceInParallel pins #4219: a
// delivery to many inboxes enqueues them in parallel, with a bounded number
// in flight, and still enqueues every inbox exactly once.
func TestDeliverActivity_ManyInboxesEnqueueEachOnceInParallel(t *testing.T) {
	enq := &concurrencyEnqueuer{hold: time.Millisecond}
	svc := newConcurrencyDeliverService(t, enq)
	inboxes := manyInboxes(200)
	// 重複も混ぜて、並べても1回ずつしか積まないことを見る。
	withDup := append(append([]string(nil), inboxes...), inboxes[:50]...)

	require.NoError(t, svc.DeliverActivity("alice", []byte(`{"type":"Create"}`), withDup))

	assert.Len(t, enq.counts, len(inboxes))
	for _, inbox := range inboxes {
		assert.Equal(t, 1, enq.counts[inbox], inbox)
	}
	assert.EqualValues(t, len(inboxes), enq.attempts.Load())
	assert.Greater(t, enq.max, int64(1), "many inboxes must be enqueued in parallel")
	assert.LessOrEqual(t, enq.max, int64(16), "parallel enqueues must stay bounded")
}

// TestDeliverActivity_EightInboxesEnqueueInParallel pins the threshold of
// #4219: eight jobs are already enqueued in parallel.
func TestDeliverActivity_EightInboxesEnqueueInParallel(t *testing.T) {
	enq := &concurrencyEnqueuer{hold: 5 * time.Millisecond}
	svc := newConcurrencyDeliverService(t, enq)

	require.NoError(t, svc.DeliverActivity("alice", []byte(`{"type":"Create"}`), manyInboxes(8)))

	assert.Len(t, enq.counts, 8)
	assert.Greater(t, enq.max, int64(1))
}

// TestDeliverActivity_FewInboxesEnqueueOneAtATime pins that a delivery to a
// handful of inboxes stays sequential (#4219).
func TestDeliverActivity_FewInboxesEnqueueOneAtATime(t *testing.T) {
	enq := &concurrencyEnqueuer{hold: time.Millisecond}
	svc := newConcurrencyDeliverService(t, enq)

	require.NoError(t, svc.DeliverActivity("alice", []byte(`{"type":"Create"}`), manyInboxes(7)))

	assert.Len(t, enq.counts, 7)
	assert.EqualValues(t, 1, enq.max)
}

// TestDeliverActivity_ParallelEnqueueStopsOnFailure pins that a failing
// enqueue stops the rest, as the sequential loop did (#4219): once one inbox
// fails, the other workers take no further inboxes.
func TestDeliverActivity_ParallelEnqueueStopsOnFailure(t *testing.T) {
	inboxes := manyInboxes(400)
	// 最初に取られる inbox をすぐに失敗させる。ほかの worker は、手元の1件を
	// 積み終えたところで止まるはず。止まらなければ、全件を積む。
	enq := &concurrencyEnqueuer{hold: time.Millisecond, failInbox: inboxes[0], fail: errors.New("redis down")}
	svc := newConcurrencyDeliverService(t, enq)

	err := svc.DeliverActivity("alice", []byte(`{"type":"Create"}`), inboxes)

	require.ErrorContains(t, err, "redis down")
	require.ErrorContains(t, err, "enqueue deliver to "+inboxes[0])
	assert.Less(t, enq.attempts.Load(), int64(len(inboxes)), "a failure must stop the remaining enqueues")
}

// TestDeliverActivity_ParallelEnqueuePanicReachesCaller pins that a panic in
// a parallel enqueue is re-raised on the caller's goroutine, as in the
// sequential loop, so the caller's recover still logs it (#4219). Before the
// re-raise, every worker has stopped, and the value names the inbox and the
// worker's stack.
func TestDeliverActivity_ParallelEnqueuePanicReachesCaller(t *testing.T) {
	inboxes := manyInboxes(50)
	enq := &concurrencyEnqueuer{hold: time.Millisecond, panicInbox: inboxes[3]}
	svc := newConcurrencyDeliverService(t, enq)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = svc.DeliverActivity("alice", []byte(`{"type":"Create"}`), inboxes)
	}()

	require.NotNil(t, recovered, "the panic must reach the caller")
	msg := fmt.Sprint(recovered)
	assert.Contains(t, msg, "enqueuer exploded")
	assert.Contains(t, msg, "enqueue deliver to "+inboxes[3])
	assert.Contains(t, msg, "concurrencyEnqueuer).EnqueueDeliver", "the worker's stack must be kept")
	assert.Zero(t, enq.inFlight.Load(), "every worker must have stopped")
}
