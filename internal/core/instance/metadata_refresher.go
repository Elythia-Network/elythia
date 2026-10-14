package instance

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/repository"
)

// metadataStaleAfter is how old infoUpdatedAt must be before an inbound or
// outbound request triggers a refetch. upstream FetchInstanceMetadataService の
// `1000 * 60 * 60 * 24` と同じ (#3564)。
const metadataStaleAfter = 24 * time.Hour

// metadataRefreshMaxInflight caps the number of concurrent background refetches
// per process.
const metadataRefreshMaxInflight = 4

// metadataRefreshRetryAfter is how long a host is left alone after a check that
// could not decide (lock held elsewhere, Redis or DB unavailable).
const metadataRefreshRetryAfter = time.Minute

// metadataRefreshMaxTracked bounds the per-host bookkeeping map.
const metadataRefreshMaxTracked = 10000

// metadataRefreshWarnEvery limits how often a failure is logged process-wide.
const metadataRefreshWarnEvery = time.Minute

// metadataRefreshCloseWait caps how long Close waits for in-flight refetches.
const metadataRefreshCloseWait = 500 * time.Millisecond

// RefreshLock serialises metadata refetches of one host across processes.
type RefreshLock interface {
	// TryAcquire takes the lock without waiting. ok is false when another
	// holder has it. release must be called once when ok is true.
	TryAcquire(ctx context.Context, host string) (release func(), ok bool, err error)
}

// MetadataRefresher refetches a remote instance's nodeinfo in the background
// when an inbound or outbound request finds it older than 24 hours (#3564).
//
// 本家は受信と配送のたびに `fetchInstanceMetadata` を fire-and-forget で呼び、
// Redis のロックと infoUpdatedAt の 24 時間で間引く。Elythia は登録時と毎日の
// 定期の取り直し (全体で 1 日 100 件) しか持たず、配信停止 / 受信の拒否の判定が
// 古い softwareVersion のまま何十日も続いていた。
type MetadataRefresher struct {
	repo    repository.InstanceRepository
	fetcher MetadataFetcher
	lock    RefreshLock
	clock   func() time.Time

	mu sync.Mutex
	// nextCheck は host -> この時刻までは DB も見ない。受信の最頻経路で毎回
	// SELECT しないための process 内の間引き。
	nextCheck map[string]time.Time
	inflight  map[string]struct{}
	closed    bool
	wg        sync.WaitGroup
	// checkOrder は nextCheck を期限の順に並べた heap。満杯のときに追い出す
	// 項目を選ぶ。
	checkOrder checkHeap
	// lastWarn / suppressedWarns は Redis や DB の障害のログをプロセス全体で
	// 間引く。障害中は、ホストごとに毎分出てしまうため。
	lastWarn        time.Time
	suppressedWarns int
	warnf           func(msg string, args ...any)
}

// NewMetadataRefresher constructs a MetadataRefresher. lock may be nil, in which
// case only the in-process dedup applies.
func NewMetadataRefresher(repo repository.InstanceRepository, fetcher MetadataFetcher, lock RefreshLock) *MetadataRefresher {
	return &MetadataRefresher{
		repo:      repo,
		fetcher:   fetcher,
		lock:      lock,
		clock:     time.Now,
		nextCheck: make(map[string]time.Time),
		inflight:  make(map[string]struct{}),
		warnf:     slog.Warn,
	}
}

// SetClock overrides the time source. Intended for tests.
func (r *MetadataRefresher) SetClock(now func() time.Time) {
	if now != nil {
		r.mu.Lock()
		r.clock = now
		r.mu.Unlock()
	}
}

func (r *MetadataRefresher) now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clock()
}

// MaybeRefresh schedules a background refetch of host's metadata if it is older
// than 24 hours. It never blocks on I/O and is safe for concurrent use.
//
// 呼び出し側でドメインブロックを見てから呼ぶ (本家もブロックした相手には
// 受信・配送のどちらでも取りに行かない)。
func (r *MetadataRefresher) MaybeRefresh(host string) {
	if r == nil || host == "" || r.repo == nil || r.fetcher == nil {
		return
	}
	r.mu.Lock()
	now := r.clock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if next, ok := r.nextCheck[host]; ok && now.Before(next) {
		r.mu.Unlock()
		return
	}
	if _, ok := r.inflight[host]; ok {
		r.mu.Unlock()
		return
	}
	// 上限に達したら今回は見送る。間引きの印を付けないので、次の受信か
	// 配送でもう一度試す。
	if len(r.inflight) >= metadataRefreshMaxInflight {
		r.mu.Unlock()
		return
	}
	r.inflight[host] = struct{}{}
	r.wg.Add(1)
	r.mu.Unlock()

	// **リクエストやジョブの context に縛らない。** 本家の fire-and-forget と
	// 同じく、受信の処理が終わった後も取り直しは続ける。終了時は Close が待つ。
	go r.run(host)
}

func (r *MetadataRefresher) run(host string) {
	next := metadataRefreshRetryAfter
	defer func() {
		// fetch はリモートが決める HTML や JSON を読むので、別 goroutine の
		// panic でプロセスごと落ちないよう拾う (fetch_metadata.go の icon と同じ)。
		if rec := recover(); rec != nil {
			slog.Error("instance metadata refresh: panic",
				"host", host, "panic", rec, "stack", string(debug.Stack()))
			// 同じ相手の文書でまた panic するだけなので、取得の失敗と同じく
			// 1 日は取りに行かない。
			next = metadataStaleAfter
		}
		r.mu.Lock()
		delete(r.inflight, host)
		r.rememberLocked(host, r.clock().Add(next))
		r.mu.Unlock()
		r.wg.Done()
	}()
	next = r.refresh(host)
}

// refresh performs one check and returns how long to leave host alone.
func (r *MetadataRefresher) refresh(host string) time.Duration {
	if r.lock != nil {
		release, ok, err := r.lock.TryAcquire(context.Background(), host)
		if err != nil {
			r.warn("instance metadata refresh: lock unavailable", "host", host, "err", err)
			return metadataRefreshRetryAfter
		}
		if !ok {
			// 他のプロセスが取り直している。終われば infoUpdatedAt が進むので、
			// 次に見たときに新しいと分かる。
			return metadataRefreshRetryAfter
		}
		defer release()
	}

	// ロックを取ってから鮮度を見る (本家と同じ順。先に見ると、2 つのプロセスが
	// 同時に古いと判断して両方取りに行く)。
	inst, err := r.repo.FindByHost(host)
	if err != nil {
		if repository.IsNotFound(err) {
			// 未登録の相手は取り直さない。登録は resolver の RegisterFromHost が
			// 行い、そこで nodeinfo も取る。
			return metadataStaleAfter
		}
		r.warn("instance metadata refresh: lookup failed", "host", host, "err", err)
		return metadataRefreshRetryAfter
	}
	now := r.now()
	if inst.InfoUpdatedAt != nil {
		if age := now.Sub(*inst.InfoUpdatedAt); age < metadataStaleAfter {
			// infoUpdatedAt が未来 (時計のずれなど) だと age が負になり、24 時間を
			// 超えて見なくなるので頭打ちにする。
			return min(metadataStaleAfter-age, metadataStaleAfter)
		}
	}
	// **取得に失敗しても 1 日は取りに行かない。** 失敗のたびに次の受信で取りに
	// 行くと、応答しない相手へ受信のたびに通信が増えるため。本家も失敗を握って
	// infoUpdatedAt を書く。nodeinfo が取れないだけなら Fetch も infoUpdatedAt を
	// 進めるが、DB への書き込みが失敗したときは進まないので、ここで返す間隔
	// (process 内の間引き) で同じ 1 日を待つ。
	if err := r.fetcher.Fetch(inst.Host); err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return metadataStaleAfter
		}
		// 相手の問題 (応答しない、壊れた文書) で出るので Warn にしない。間引く
		// Warn は、こちらの障害 (Redis / DB) だけにする。
		slog.Debug("instance metadata refresh: fetch failed", "host", host, "err", err)
	}
	return metadataStaleAfter
}

// rememberLocked records when host may be checked again. Must hold r.mu.
//
// **満杯なら、期限が最も近い項目を追い出して覚える。** 期限の順の heap を
// 併せて持ち、O(log n) で選ぶ (ロックの中で全件を走査すると、受信の最頻経路が
// 待たされる)。期限切れの項目は期限が最も近いので、先に追い出される。
func (r *MetadataRefresher) rememberLocked(host string, until time.Time) {
	if _, ok := r.nextCheck[host]; !ok && len(r.nextCheck) >= metadataRefreshMaxTracked {
		r.evictEarliestLocked()
	}
	r.nextCheck[host] = until
	heap.Push(&r.checkOrder, checkEntry{host: host, until: until})
	// 同じホストを覚え直すたびに古い項目が heap に残るので、map の 2 倍を
	// 超えたら作り直す (償却で O(1))。
	if len(r.checkOrder) > 2*max(len(r.nextCheck), 64) {
		r.rebuildCheckOrderLocked()
	}
}

// evictEarliestLocked removes the host whose next check comes first. Must hold
// r.mu.
func (r *MetadataRefresher) evictEarliestLocked() {
	for len(r.checkOrder) > 0 {
		e := heap.Pop(&r.checkOrder).(checkEntry)
		// heap には覚え直す前の古い項目も残る。map の値と一致するものだけが
		// 今の項目。
		if cur, ok := r.nextCheck[e.host]; ok && cur.Equal(e.until) {
			delete(r.nextCheck, e.host)
			return
		}
	}
}

func (r *MetadataRefresher) rebuildCheckOrderLocked() {
	r.checkOrder = r.checkOrder[:0]
	for h, t := range r.nextCheck {
		r.checkOrder = append(r.checkOrder, checkEntry{host: h, until: t})
	}
	heap.Init(&r.checkOrder)
}

// checkEntry is one (host, next check) pair in checkHeap.
type checkEntry struct {
	host  string
	until time.Time
}

// checkHeap orders entries by the earliest next check.
type checkHeap []checkEntry

func (h checkHeap) Len() int           { return len(h) }
func (h checkHeap) Less(i, j int) bool { return h[i].until.Before(h[j].until) }
func (h checkHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *checkHeap) Push(x any)        { *h = append(*h, x.(checkEntry)) }
func (h *checkHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

// warn logs a refresh failure at most once per metadataRefreshWarnEvery across
// all hosts, reporting how many were suppressed in between.
func (r *MetadataRefresher) warn(msg string, args ...any) {
	r.mu.Lock()
	now := r.clock()
	if !r.lastWarn.IsZero() && now.Sub(r.lastWarn) < metadataRefreshWarnEvery {
		r.suppressedWarns++
		r.mu.Unlock()
		return
	}
	suppressed := r.suppressedWarns
	r.suppressedWarns = 0
	r.lastWarn = now
	warnf := r.warnf
	r.mu.Unlock()
	warnf(msg, append(args, "suppressed", suppressed)...)
}

// Close stops accepting new refetches and waits briefly for in-flight ones.
//
// **長くは待たない。** shutdown の hook は 1 つの ctx を共有するので、応答しない
// 相手の取り直し (Fetch は ctx を取らず止められない) を待つと、後に並ぶ hook
// (集計の保存など) が期限切れの ctx で走る。本家も fire-and-forget で待たない。
// 待つのは ctx の期限と metadataRefreshCloseWait の短い方まで。
func (r *MetadataRefresher) Close(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(metadataRefreshCloseWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// metadataRefreshLockKeyPrefix namespaces the refresh lock keys.
const metadataRefreshLockKeyPrefix = "mk:instance:metadataRefresh:lock:"

// metadataRefreshLockTTL bounds how long a crashed holder keeps the lock. 本家は
// 30 秒だが、Elythia の Fetch は discovery と文書の 2 回の取得 (各 30 秒の
// timeout) を直列に行うので、その分だけ長くする。
const metadataRefreshLockTTL = 2 * time.Minute

// RedisRefreshLock implements RefreshLock with SET NX + a token-checked release.
type RedisRefreshLock struct {
	client redis.UniversalClient
}

// NewRedisRefreshLock constructs a RedisRefreshLock.
func NewRedisRefreshLock(client redis.UniversalClient) *RedisRefreshLock {
	return &RedisRefreshLock{client: client}
}

// refreshLockRelease deletes the key only while it still holds our token, so a
// holder whose TTL expired does not release the next holder's lock.
var refreshLockRelease = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
else
    return 0
end
`)

// TryAcquire implements RefreshLock.
func (l *RedisRefreshLock) TryAcquire(ctx context.Context, host string) (func(), bool, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, false, err
	}
	token := hex.EncodeToString(b[:])
	key := metadataRefreshLockKeyPrefix + host
	_, err := l.client.SetArgs(ctx, key, token, redis.SetArgs{Mode: "NX", TTL: metadataRefreshLockTTL}).Result()
	switch {
	case err == nil:
		return func() {
			refreshLockRelease.Run(context.Background(), l.client, []string{key}, token)
		}, true, nil
	case errors.Is(err, redis.Nil):
		return nil, false, nil
	default:
		return nil, false, err
	}
}
