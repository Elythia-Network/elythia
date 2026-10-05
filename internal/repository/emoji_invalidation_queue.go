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
}

// defaultEmojiInvalidationDrainTimeout bounds how long Run keeps publishing
// pending invalidations after ctx is done.
const defaultEmojiInvalidationDrainTimeout = 2 * time.Second

// NewEmojiInvalidationQueue creates a queue holding up to size pending
// invalidations. publish is called from the Run goroutine only.
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
