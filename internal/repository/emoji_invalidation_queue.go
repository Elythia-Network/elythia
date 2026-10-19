package repository

import (
	"context"
	"sync"
	"time"
)

// EmojiInvalidationQueue hands emoji cache invalidations to a single sender
// goroutine, so a slow or unreachable pub/sub Redis never blocks the write
// path (e.g. inbox processing creating remote emojis one by one).
//
// Enqueue never blocks and never drops an invalidation silently: when size
// invalidations are already pending, they are collapsed into one
// EmojiCacheInvalidation{All: true}, which covers whatever was dropped.
//
// Start runs the sender (Run) in a goroutine and Stop stops it (#3705).
// Invalidations enqueued before Start are kept and published once it has
// started.
type EmojiInvalidationQueue struct {
	mu      sync.Mutex
	pending []EmojiCacheInvalidation
	size    int
	// notify は Enqueue が送信側を起こすのに使う (容量 1)。送信側は起きたら
	// pending が空になるまで出すので、起こし損ねは起きない。
	notify  chan struct{}
	publish func(EmojiCacheInvalidation)
	// drainTimeout は停止時に溜まっている分を送り切るのに使う時間の上限。
	drainTimeout time.Duration

	// lifeMuはStartとStopの状態(started/stopped/cancel/done)を守る。
	// 止めた後のStartは何もしない。止める人の居ないgoroutineを残さないため(#3705)。
	lifeMu  sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// defaultEmojiInvalidationDrainTimeout bounds how long Run keeps publishing
// pending invalidations after ctx is done.
const defaultEmojiInvalidationDrainTimeout = 2 * time.Second

// NewEmojiInvalidationQueue creates a queue holding up to size pending
// invalidations. publish is called from the Run goroutine only, or by Stop
// when the queue was never started (then no Run goroutine exists).
func NewEmojiInvalidationQueue(size int, publish func(EmojiCacheInvalidation)) *EmojiInvalidationQueue {
	if size <= 0 {
		size = 1
	}
	return &EmojiInvalidationQueue{
		size:         size,
		notify:       make(chan struct{}, 1),
		publish:      publish,
		drainTimeout: defaultEmojiInvalidationDrainTimeout,
	}
}

// Enqueue queues ev for publishing without blocking.
func (q *EmojiInvalidationQueue) Enqueue(ev EmojiCacheInvalidation) {
	q.mu.Lock()
	if len(q.pending) > 0 && q.pending[0].All {
		// 先頭の All が後ろに積むものをすべて包含するので、積まない。
	} else if len(q.pending) >= q.size {
		// 溢れた分は捨てずに「全体を落とす」へ畳む。溜まっていたものも ev も
		// All が包含する。
		q.pending = append(q.pending[:0], EmojiCacheInvalidation{All: true})
	} else {
		q.pending = append(q.pending, ev)
	}
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Start runs Run in a goroutine with a ctx derived from ctx, which Stop
// cancels. Only the first call starts anything, and none does once Stop has
// been called.
func (q *EmojiInvalidationQueue) Start(ctx context.Context) {
	q.lifeMu.Lock()
	defer q.lifeMu.Unlock()
	if q.started || q.stopped {
		return
	}
	q.started = true
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	q.cancel, q.done = cancel, done
	go func() {
		defer close(done)
		q.Run(runCtx)
	}()
}

// Stop stops the queue. When Start has run, Stop cancels Run's ctx and waits
// until Run has published what is pending (up to the drain timeout) and
// returned, or until ctx is done. When Start has not run, it publishes what
// is pending synchronously, until the drain timeout or until ctx is done,
// whichever comes first (a publish already running is not interrupted), and
// returns without waiting for Run. Only the first call does anything; Start
// after Stop starts nothing. Invalidations enqueued after Stop are not
// published.
//
// 起動していないときにdoneを待つと、doneは閉じないので停止のctxの期限まで固まる
// (構築に失敗した世代の後始末はcontext.Background()で止めるので、永久に返らない)。
// 溜まった分を同期で送るのは、インスタンスのバッファが起動していないCloseで同期で
// flushするのと同じ扱い(#3705)。停止のctxも見るのは、停止の関数は停止のctxの期限で
// 返る約束だから(lifecyclePart.stop)。送信中の1件は、publishの側のtimeoutまで待つ。
func (q *EmojiInvalidationQueue) Stop(ctx context.Context) {
	q.lifeMu.Lock()
	if q.stopped {
		q.lifeMu.Unlock()
		return
	}
	q.stopped = true
	started, cancel, done := q.started, q.cancel, q.done
	q.lifeMu.Unlock()
	if !started {
		q.drainNow(ctx, time.Now().Add(q.drainTimeout))
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Run publishes queued invalidations until ctx is done. On ctx done it keeps
// publishing what is still pending for up to the drain timeout before it
// returns; invalidations enqueued after Run has returned are not published.
func (q *EmojiInvalidationQueue) Run(ctx context.Context) {
	for {
		// 停止を先に見る。停止中の受信ジョブが積んだ分も、ここで送り切る。
		if ctx.Err() != nil {
			q.publishPending(ctx, time.Now().Add(q.drainTimeout))
			return
		}
		select {
		case <-ctx.Done():
			continue
		case <-q.notify:
		}
		q.publishPending(ctx, time.Time{})
	}
}

// publishPending publishes until nothing is pending. With a zero deadline it
// also stops as soon as ctx is done; with a non-zero deadline (draining on
// stop) it stops at the deadline instead.
//
// **通常の送信でも停止を見る。** 見ないと、停止中も受信ジョブが書き込みを続けて
// いる間 (shutdown hook は queue server の停止より先に走る) pending が空にならず、
// Run が先頭の停止の確認へ戻れない。停止処理は Run を待つので、10 秒の予算を
// 使い切り、後ろの chart の保存や HTTP の停止が期限切れになる (#3383 のレビューで
// 実測)。停止に気付いたら抜けて、Run の先頭で締め切りのある送り切りへ移る。
func (q *EmojiInvalidationQueue) publishPending(ctx context.Context, deadline time.Time) {
	for {
		if deadline.IsZero() {
			if ctx.Err() != nil {
				return
			}
		} else if !time.Now().Before(deadline) {
			return
		}
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.mu.Unlock()
			return
		}
		ev := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()
		q.publish(ev)
	}
}

// drainNow publishes what is pending until nothing is, deadline passes or
// ctx is done, checking both before each publish. Stop uses it when Run was
// never started.
func (q *EmojiInvalidationQueue) drainNow(ctx context.Context, deadline time.Time) {
	for ctx.Err() == nil && time.Now().Before(deadline) {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.mu.Unlock()
			return
		}
		ev := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()
		q.publish(ev)
	}
}
