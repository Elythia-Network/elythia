package cachebus

import (
	"context"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/repository"
)

const (
	// userQueueLimit caps the distinct ids and URIs waiting to be published.
	// Beyond it the pending invalidations collapse into one All.
	//
	// 1 件は id (aidx で 16 文字) か URI なので、溢れる直前でも数百 KB に収まる。
	// 溢れるのは Redis に届かない状態が続いたときで、その間に書かれた利用者を
	// 名指しで覚え続けるより、届いたときに全体を捨てさせるほうが安い。
	userQueueLimit = 4096
	// userBatchMax caps the ids and URIs in one message.
	//
	// 1 通を数 KB に抑える。受けた側は 1 通ずつ全部を処理するので、大きすぎる
	// 1 通は受信の drain を長く占有する。
	userBatchMax = 256
	// userQueueDrainTimeout bounds how long Run keeps publishing after its
	// context is done (the emoji queue uses the same budget).
	userQueueDrainTimeout = 2 * time.Second
)

// userQueue collects user cache invalidations and hands them to one sender
// goroutine in batches.
//
// **利用者のキャッシュは書き込みの頻度が高い。** リモートの投稿を受けるたびに
// 投稿者の notesCount を数え直し、フォロー・Update の受信・actor の取り直しでも
// 書く (いずれも CachedUserRepository を通り、1 回で id を 1 つ捨てる)。同期の
// publish は書き込み 1 回ごとに Redis の 1 往復を足し、Redis が詰まると受信の
// ジョブを timeout まで止めるので、ここでは書き込みの経路から切り離す。
//
// **まとめる間隔は置かない。** 送信側は起こされるとすぐ、溜まっている分を
// 1 通にまとめて送る。待ちが無いときは書き込み 1 回 = 1 通で、遅れは Redis の
// 1 往復だけ。書き込みが Redis の往復より速く来る間は、送っている最中に溜まった
// 分が次の 1 通にまとまる (同じ id は 1 つに畳む) ので、通数は 1 往復あたり
// 1 通を超えない。間隔を置いて通数を減らすと、その間は他のプロセスが古い行
// (凍結の反映、パスワードや 2FA の変更を含む) を返し続ける。
type userQueue struct {
	mu sync.Mutex
	// ids / uris は送り待ちを入った順に持つ。idSet / uriSet は重複を畳む。
	ids    []string
	idSet  map[string]struct{}
	uris   []string
	uriSet map[string]struct{}
	all    bool
	limit  int
	batch  int
	// notify は Enqueue が送信側を起こすのに使う (容量 1)。送信側は起きたら
	// 空になるまで送るので、起こし損ねは起きない。
	notify       chan struct{}
	publish      func(repository.UserCacheInvalidation)
	drainTimeout time.Duration
}

func newUserQueue(limit, batch int, publish func(repository.UserCacheInvalidation)) *userQueue {
	return &userQueue{
		idSet:        make(map[string]struct{}),
		uriSet:       make(map[string]struct{}),
		limit:        limit,
		batch:        batch,
		notify:       make(chan struct{}, 1),
		publish:      publish,
		drainTimeout: userQueueDrainTimeout,
	}
}

// Enqueue queues inv without blocking. It never drops an invalidation
// silently: past the limit everything pending collapses into All.
func (q *userQueue) Enqueue(inv repository.UserCacheInvalidation) {
	q.mu.Lock()
	switch {
	case q.all:
		// 送り待ちの All が後から来るものを包含する。
	case inv.All:
		q.collapseLocked()
	default:
		for _, id := range inv.IDs {
			if _, ok := q.idSet[id]; !ok && id != "" {
				q.idSet[id] = struct{}{}
				q.ids = append(q.ids, id)
			}
		}
		for _, uri := range inv.URIs {
			if _, ok := q.uriSet[uri]; !ok && uri != "" {
				q.uriSet[uri] = struct{}{}
				q.uris = append(q.uris, uri)
			}
		}
		if len(q.ids)+len(q.uris) > q.limit {
			q.collapseLocked()
		}
	}
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// collapseLocked replaces everything pending with one All. Caller holds q.mu.
func (q *userQueue) collapseLocked() {
	q.all = true
	q.ids, q.uris = nil, nil
	clear(q.idSet)
	clear(q.uriSet)
}

// take removes and returns the next message to publish, or false when
// nothing is pending.
func (q *userQueue) take() (repository.UserCacheInvalidation, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.all {
		q.all = false
		return repository.UserCacheInvalidation{All: true}, true
	}
	if len(q.ids) == 0 && len(q.uris) == 0 {
		return repository.UserCacheInvalidation{}, false
	}
	var inv repository.UserCacheInvalidation
	n := min(len(q.ids), q.batch)
	if n > 0 {
		inv.IDs = append([]string(nil), q.ids[:n]...)
		for _, id := range inv.IDs {
			delete(q.idSet, id)
		}
		q.ids = q.ids[n:]
	}
	if m := min(len(q.uris), q.batch-n); m > 0 {
		inv.URIs = append([]string(nil), q.uris[:m]...)
		for _, uri := range inv.URIs {
			delete(q.uriSet, uri)
		}
		q.uris = q.uris[m:]
	}
	return inv, true
}

// Run publishes queued invalidations until ctx is done, then keeps publishing
// what is still pending for up to the drain timeout. Invalidations enqueued
// after Run has returned are not published (the other processes fall back to
// the TTL).
func (q *userQueue) Run(ctx context.Context) {
	for {
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
// stops as soon as ctx is done (Run then drains with a deadline); otherwise it
// stops at the deadline.
//
// 通常の送信でも停止を見る。見ないと、停止中も受信ジョブが書き続けている間
// 空にならず、Run が締め切りのある送り切りへ移れない (絵文字の待ち行列と同じ、
// #3383)。
func (q *userQueue) publishPending(ctx context.Context, deadline time.Time) {
	for {
		if deadline.IsZero() {
			if ctx.Err() != nil {
				return
			}
		} else if !time.Now().Before(deadline) {
			return
		}
		inv, ok := q.take()
		if !ok {
			return
		}
		q.publish(inv)
	}
}

// UserLink is the connection of a CachedUserRepository to the other processes.
type UserLink struct {
	queue *userQueue
	stop  func()
}

// ConnectUsers makes c forward its invalidations to the other processes and
// apply theirs (#3664). sender identifies this process. Publishing happens on
// the goroutine that runs Run; until Run is started the invalidations only
// wait in the queue.
//
// **自プロセスのキャッシュは、今までどおり書き込みの直後に捨てる** (待ち行列は
// 他のプロセスへ伝える分だけ)。Redis に届かないときも自プロセスは正しい。
//
// 通知を取りこぼしたプロセス (publish の失敗、購読の再接続の間、停止の 2 秒で
// 送り切れなかった分) では、今までどおり TTL (5 分) が上限になる。受信側の待ち
// 行列が溢れて捨てた分は、全体を捨てて埋める (subscribe)。
func ConnectUsers(c *repository.CachedUserRepository, bus Bus, sender string) *UserLink {
	if c == nil || bus == nil {
		return &UserLink{stop: func() {}}
	}
	q := newUserQueue(userQueueLimit, userBatchMax, func(inv repository.UserCacheInvalidation) {
		publishSync(bus, TopicUser, sender, inv)
	})
	c.SetInvalidationHook(q.Enqueue)
	stop := subscribe(bus, TopicUser, sender, c.ApplyRemoteInvalidation,
		func() { c.ApplyRemoteInvalidation(repository.UserCacheInvalidation{All: true}) })
	return &UserLink{queue: q, stop: stop}
}

// Run publishes the queued invalidations until ctx is done, then drains for up
// to 2 seconds before it returns.
func (l *UserLink) Run(ctx context.Context) {
	if l.queue == nil {
		return
	}
	l.queue.Run(ctx)
}

// Stop stops receiving the other processes' invalidations.
func (l *UserLink) Stop() {
	l.stop()
}
