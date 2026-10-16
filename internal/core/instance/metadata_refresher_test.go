package instance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// refreshFetcher records Fetch calls and advances infoUpdatedAt like the real
// FetchMetadataService.
type refreshFetcher struct {
	mu    sync.Mutex
	repo  *testutil.MockInstanceRepository
	now   func() time.Time
	calls []string
	gate  chan struct{}
	err   error
	panic bool
}

func (f *refreshFetcher) Fetch(host string) error {
	if f.gate != nil {
		<-f.gate
	}
	if f.panic {
		panic("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, host)
	if inst, ok := f.repo.Instances[host]; ok && f.now != nil {
		t := f.now()
		inst.InfoUpdatedAt = &t
	}
	return f.err
}

func (f *refreshFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// syncInstanceRepo serialises FindByHost, because the refresher calls it from
// several goroutines and the mock is not safe for concurrent use.
type syncInstanceRepo struct {
	*testutil.MockInstanceRepository
	mu sync.Mutex
}

func (r *syncInstanceRepo) FindByHost(host string) (*model.Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.MockInstanceRepository.FindByHost(host)
}

// fakeClock is a mutable clock safe for concurrent reads.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeRefreshLock struct {
	held     bool
	err      error
	acquired int
	released int
}

func (l *fakeRefreshLock) TryAcquire(context.Context, string) (func(), bool, error) {
	if l.err != nil {
		return nil, false, l.err
	}
	if l.held {
		return nil, false, nil
	}
	l.acquired++
	return func() { l.released++ }, true, nil
}

func newRefresherFixture(t *testing.T, infoAge *time.Duration) (*MetadataRefresher, *testutil.MockInstanceRepository, *refreshFetcher, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	repo := testutil.NewMockInstanceRepository()
	inst := &model.Instance{ID: "i1", Host: "remote.example"}
	if infoAge != nil {
		at := clock.Now().Add(-*infoAge)
		inst.InfoUpdatedAt = &at
	}
	repo.Instances[inst.Host] = inst
	fetcher := &refreshFetcher{repo: repo, now: clock.Now}
	r := NewMetadataRefresher(&syncInstanceRepo{MockInstanceRepository: repo}, fetcher, nil)
	r.SetClock(clock.Now)
	return r, repo, fetcher, clock
}

func durPtr(d time.Duration) *time.Duration { return &d }

func TestMetadataRefresher_RefetchesStaleHost(t *testing.T) {
	cases := []struct {
		name    string
		age     *time.Duration
		fetches int
	}{
		{name: "older than 24h is refetched", age: durPtr(25 * time.Hour), fetches: 1},
		{name: "exactly 24h is refetched", age: durPtr(24 * time.Hour), fetches: 1},
		{name: "never fetched is refetched", age: nil, fetches: 1},
		{name: "fresh info is left alone", age: durPtr(23 * time.Hour), fetches: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, fetcher, _ := newRefresherFixture(t, tc.age)
			r.MaybeRefresh("remote.example")
			r.wg.Wait()
			if got := fetcher.count(); got != tc.fetches {
				t.Fatalf("fetches = %d, want %d", got, tc.fetches)
			}
		})
	}
}

func TestMetadataRefresher_OncePerDayPerHost(t *testing.T) {
	r, repo, fetcher, clock := newRefresherFixture(t, durPtr(48*time.Hour))

	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if fetcher.count() != 1 {
		t.Fatalf("first refresh: fetches = %d, want 1", fetcher.count())
	}
	finds := repo.FindCalls

	// 同じ日のうちは DB も見ない。
	clock.Advance(23 * time.Hour)
	for range 5 {
		r.MaybeRefresh("remote.example")
	}
	r.wg.Wait()
	if fetcher.count() != 1 {
		t.Fatalf("within a day: fetches = %d, want 1", fetcher.count())
	}
	if repo.FindCalls != finds {
		t.Fatalf("within a day: FindByHost called %d more times, want 0", repo.FindCalls-finds)
	}

	clock.Advance(time.Hour)
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if fetcher.count() != 2 {
		t.Fatalf("after a day: fetches = %d, want 2", fetcher.count())
	}
}

func TestMetadataRefresher_FreshHostIsRecheckedWhenItTurnsStale(t *testing.T) {
	// 20 時間前に取った相手は、4 時間後に古くなる。それまでは DB を見ない。
	r, repo, fetcher, clock := newRefresherFixture(t, durPtr(20*time.Hour))
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	finds := repo.FindCalls

	clock.Advance(3 * time.Hour)
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if repo.FindCalls != finds || fetcher.count() != 0 {
		t.Fatalf("before stale: finds +%d, fetches %d; want 0, 0", repo.FindCalls-finds, fetcher.count())
	}

	clock.Advance(time.Hour)
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if fetcher.count() != 1 {
		t.Fatalf("once stale: fetches = %d, want 1", fetcher.count())
	}
}

func TestMetadataRefresher_DedupsConcurrentCalls(t *testing.T) {
	r, _, fetcher, _ := newRefresherFixture(t, nil)
	fetcher.gate = make(chan struct{})
	// infoUpdatedAt を進めない。進めると、後から走った goroutine が DB で新しいと
	// 見て取りに行かず、process 内の重複排除が外れても数が 1 のままになりうる。
	fetcher.now = nil
	for range 10 {
		r.MaybeRefresh("remote.example")
	}
	close(fetcher.gate)
	r.wg.Wait()
	if fetcher.count() != 1 {
		t.Fatalf("fetches = %d, want 1", fetcher.count())
	}
}

func TestMetadataRefresher_CapsInflight(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	repo := testutil.NewMockInstanceRepository()
	hosts := []string{"a.example", "b.example", "c.example", "d.example", "e.example"}
	for _, h := range hosts {
		repo.Instances[h] = &model.Instance{ID: h, Host: h}
	}
	fetcher := &refreshFetcher{repo: repo, now: clock.Now, gate: make(chan struct{})}
	r := NewMetadataRefresher(&syncInstanceRepo{MockInstanceRepository: repo}, fetcher, nil)
	r.SetClock(clock.Now)

	for _, h := range hosts {
		r.MaybeRefresh(h)
	}
	r.mu.Lock()
	inflight := len(r.inflight)
	_, rememberedE := r.nextCheck["e.example"]
	r.mu.Unlock()
	if inflight != metadataRefreshMaxInflight {
		t.Fatalf("inflight = %d, want %d", inflight, metadataRefreshMaxInflight)
	}
	close(fetcher.gate)
	r.wg.Wait()
	if fetcher.count() != metadataRefreshMaxInflight {
		t.Fatalf("fetches = %d, want %d", fetcher.count(), metadataRefreshMaxInflight)
	}
	if rememberedE {
		t.Fatal("dropped host must not be remembered")
	}
	// 見送った相手は次の呼び出しで取りに行く。
	r.MaybeRefresh("e.example")
	r.wg.Wait()
	if fetcher.count() != metadataRefreshMaxInflight+1 {
		t.Fatalf("retry of dropped host: fetches = %d, want %d", fetcher.count(), metadataRefreshMaxInflight+1)
	}
}

func TestMetadataRefresher_SkipsUnknownAndEmptyHosts(t *testing.T) {
	r, _, fetcher, _ := newRefresherFixture(t, nil)
	r.MaybeRefresh("")
	r.MaybeRefresh("unknown.example")
	r.wg.Wait()
	if fetcher.count() != 0 {
		t.Fatalf("fetches = %d, want 0", fetcher.count())
	}
	r.mu.Lock()
	next := r.nextCheck["unknown.example"]
	r.mu.Unlock()
	if want := r.clock().Add(metadataStaleAfter); !next.Equal(want) {
		t.Fatalf("unknown host next check = %v, want %v", next, want)
	}
}

func TestMetadataRefresher_LookupErrorRetriesSoon(t *testing.T) {
	r, repo, fetcher, clock := newRefresherFixture(t, nil)
	repo.FindByHostErr = errors.New("db down")
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if fetcher.count() != 0 {
		t.Fatalf("fetches = %d, want 0", fetcher.count())
	}
	repo.FindByHostErr = nil
	clock.Advance(metadataRefreshRetryAfter)
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if fetcher.count() != 1 {
		t.Fatalf("after retry window: fetches = %d, want 1", fetcher.count())
	}
}

func TestMetadataRefresher_FetchFailureStillWaitsADay(t *testing.T) {
	r, _, fetcher, clock := newRefresherFixture(t, nil)
	fetcher.err = errors.New("nodeinfo unreachable")
	// infoUpdatedAt を進めない失敗 (DB への書き込みまで届かなかった場合)。DB の
	// 鮮度ではなく、process 内の間引きで 1 日待つこと。
	fetcher.now = nil
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	clock.Advance(time.Hour)
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if fetcher.count() != 1 {
		t.Fatalf("fetches = %d, want 1", fetcher.count())
	}
}

func TestMetadataRefresher_FetchNotFoundWaitsADay(t *testing.T) {
	r, _, fetcher, clock := newRefresherFixture(t, nil)
	fetcher.err = ErrInstanceNotFound
	fetcher.now = nil
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	clock.Advance(time.Hour)
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if fetcher.count() != 1 {
		t.Fatalf("fetches = %d, want 1", fetcher.count())
	}
}

func TestMetadataRefresher_RespectsLock(t *testing.T) {
	t.Run("held elsewhere", func(t *testing.T) {
		r, _, fetcher, clock := newRefresherFixture(t, nil)
		lock := &fakeRefreshLock{held: true}
		r.lock = lock
		r.MaybeRefresh("remote.example")
		r.wg.Wait()
		if fetcher.count() != 0 {
			t.Fatalf("fetches = %d, want 0", fetcher.count())
		}
		lock.held = false
		clock.Advance(metadataRefreshRetryAfter)
		r.MaybeRefresh("remote.example")
		r.wg.Wait()
		if fetcher.count() != 1 || lock.released != 1 {
			t.Fatalf("after release: fetches = %d, released = %d; want 1, 1", fetcher.count(), lock.released)
		}
	})
	t.Run("lock error", func(t *testing.T) {
		r, _, fetcher, _ := newRefresherFixture(t, nil)
		r.lock = &fakeRefreshLock{err: errors.New("redis down")}
		r.MaybeRefresh("remote.example")
		r.wg.Wait()
		if fetcher.count() != 0 {
			t.Fatalf("fetches = %d, want 0", fetcher.count())
		}
	})
	t.Run("fresh after lock is released", func(t *testing.T) {
		r, _, fetcher, _ := newRefresherFixture(t, durPtr(time.Hour))
		lock := &fakeRefreshLock{}
		r.lock = lock
		r.MaybeRefresh("remote.example")
		r.wg.Wait()
		if fetcher.count() != 0 || lock.acquired != 1 || lock.released != 1 {
			t.Fatalf("fetches=%d acquired=%d released=%d; want 0,1,1", fetcher.count(), lock.acquired, lock.released)
		}
	})
}

func TestMetadataRefresher_RecoversPanic(t *testing.T) {
	r, _, fetcher, clock := newRefresherFixture(t, nil)
	lock := &fakeRefreshLock{}
	r.lock = lock
	fetcher.panic = true
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	r.mu.Lock()
	_, inflight := r.inflight["remote.example"]
	next := r.nextCheck["remote.example"]
	r.mu.Unlock()
	if inflight {
		t.Fatal("panicked refresh must leave the inflight set")
	}
	// panic でもロックを外す。外さないと、TTL が切れるまで他のプロセスも
	// 取りに行けない。
	if lock.acquired != 1 || lock.released != 1 {
		t.Fatalf("lock acquired=%d released=%d; want 1, 1", lock.acquired, lock.released)
	}
	if want := clock.Now().Add(metadataStaleAfter); !next.Equal(want) {
		t.Fatalf("next check = %v, want %v", next, want)
	}
}

func TestMetadataRefresher_CloseWaitsAndStopsNewWork(t *testing.T) {
	r, _, fetcher, _ := newRefresherFixture(t, nil)
	fetcher.gate = make(chan struct{})
	r.MaybeRefresh("remote.example")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r.Close(ctx) // in-flight の取得が終わらないので ctx で戻る
	if fetcher.count() != 0 {
		t.Fatalf("fetch finished before gate opened")
	}
	close(fetcher.gate)
	// 取得が終われば ctx の期限を待たずに戻ること。
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	start := time.Now()
	r.Close(waitCtx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close waited %v; it must return once in-flight work is done", elapsed)
	}
	if fetcher.count() != 1 {
		t.Fatalf("Close must wait for the in-flight fetch; fetches = %d", fetcher.count())
	}

	r.MaybeRefresh("other.example")
	r.mu.Lock()
	n := len(r.inflight)
	r.mu.Unlock()
	if n != 0 {
		t.Fatal("MaybeRefresh after Close must not start work")
	}
	var nilRefresher *MetadataRefresher
	nilRefresher.MaybeRefresh("remote.example")
	nilRefresher.Close(context.Background())
}

func TestMetadataRefresher_BoundsTrackedHosts(t *testing.T) {
	r, _, _, clock := newRefresherFixture(t, nil)
	base := clock.Now()
	host := func(i int) string { return fmt.Sprintf("h%d.example", i) }

	r.mu.Lock()
	defer r.mu.Unlock()
	// 期限が i 秒後の項目で満杯にする。h0 が最も近い。
	for i := range metadataRefreshMaxTracked {
		r.rememberLocked(host(i), base.Add(time.Duration(i+1)*time.Second))
	}
	// h0 を覚え直して遠くへ送る。heap には古い h0 が残るが、追い出しの対象は
	// 今の期限で選ぶこと。
	r.rememberLocked(host(0), base.Add(time.Hour))

	r.rememberLocked("new.example", base.Add(time.Hour))
	if n := len(r.nextCheck); n != metadataRefreshMaxTracked {
		t.Fatalf("len = %d, want %d", n, metadataRefreshMaxTracked)
	}
	if _, ok := r.nextCheck["new.example"]; !ok {
		t.Fatal("a full map must still remember the new host")
	}
	if _, ok := r.nextCheck[host(1)]; ok {
		t.Fatal("the entry with the earliest next check (h1) must be evicted")
	}
	if _, ok := r.nextCheck[host(0)]; !ok {
		t.Fatal("a host whose stale heap entry is earliest must not be evicted")
	}

	// 覚えている相手を覚え直すときは追い出さない。
	r.rememberLocked(host(7), base.Add(2*time.Hour))
	if _, ok := r.nextCheck[host(2)]; !ok || len(r.nextCheck) != metadataRefreshMaxTracked {
		t.Fatal("re-remembering a tracked host must not evict another")
	}

	// 期限切れの項目は先に追い出される。
	r.nextCheck[host(9)] = base.Add(-time.Hour)
	heapPush := checkEntry{host: host(9), until: base.Add(-time.Hour)}
	r.checkOrder = append(r.checkOrder, heapPush)
	r.rebuildCheckOrderLocked()
	r.rememberLocked("newer.example", base.Add(time.Hour))
	if _, ok := r.nextCheck[host(9)]; ok {
		t.Fatal("an expired entry must be evicted first")
	}

	// heap は覚え直しで膨らみ続けない。
	for range 5 * metadataRefreshMaxTracked {
		r.rememberLocked(host(5), base.Add(3*time.Hour))
	}
	if n := len(r.checkOrder); n > 2*metadataRefreshMaxTracked {
		t.Fatalf("heap grew to %d entries", n)
	}
}

func TestMetadataRefresher_CloseDoesNotWaitForSlowFetch(t *testing.T) {
	r, _, fetcher, _ := newRefresherFixture(t, nil)
	fetcher.gate = make(chan struct{})
	t.Cleanup(func() {
		close(fetcher.gate)
		r.wg.Wait()
	})
	r.MaybeRefresh("remote.example")

	// shutdown の hook は ctx を共有する。応答しない相手の取り直しで後続の
	// hook の時間を使い切らないこと。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	r.Close(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close waited %v for a fetch that never ends; want about %v", elapsed, metadataRefreshCloseWait)
	}
}

func TestMetadataRefresher_ThrottlesWarnings(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	repo := testutil.NewMockInstanceRepository()
	repo.FindByHostErr = errors.New("db down")
	r := NewMetadataRefresher(&syncInstanceRepo{MockInstanceRepository: repo}, &refreshFetcher{repo: repo}, nil)
	r.SetClock(clock.Now)
	var logged [][]any
	r.warnf = func(_ string, args ...any) { logged = append(logged, args) }

	for _, h := range []string{"a.example", "b.example", "c.example"} {
		r.MaybeRefresh(h)
		r.wg.Wait()
	}
	if len(logged) != 1 {
		t.Fatalf("warnings within a minute = %d, want 1", len(logged))
	}
	clock.Advance(metadataRefreshWarnEvery)
	r.MaybeRefresh("d.example")
	r.wg.Wait()
	if len(logged) != 2 {
		t.Fatalf("warnings after a minute = %d, want 2", len(logged))
	}
	last := logged[1]
	if got := last[len(last)-1]; got != 2 {
		t.Fatalf("suppressed = %v, want 2", got)
	}
	// 数は出したら 0 に戻す。
	clock.Advance(metadataRefreshWarnEvery)
	r.MaybeRefresh("e.example")
	r.wg.Wait()
	if len(logged) != 3 {
		t.Fatalf("warnings after another minute = %d, want 3", len(logged))
	}
	last = logged[2]
	if got := last[len(last)-1]; got != 0 {
		t.Fatalf("suppressed after reset = %v, want 0", got)
	}
}

func TestMetadataRefresher_FutureInfoUpdatedAtIsCapped(t *testing.T) {
	r, _, fetcher, clock := newRefresherFixture(t, durPtr(-48*time.Hour))
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	r.mu.Lock()
	next := r.nextCheck["remote.example"]
	r.mu.Unlock()
	if want := clock.Now().Add(metadataStaleAfter); !next.Equal(want) {
		t.Fatalf("next check = %v, want %v (capped at 24h)", next, want)
	}
	if fetcher.count() != 0 {
		t.Fatalf("fetches = %d, want 0", fetcher.count())
	}
}

func TestMetadataRefresher_HeapIsNotRebuiltOnEveryRemember(t *testing.T) {
	r, _, _, clock := newRefresherFixture(t, nil)
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range 10 {
		r.rememberLocked("remote.example", clock.Now().Add(time.Duration(i)*time.Second))
	}
	if len(r.nextCheck) != 1 {
		t.Fatalf("map len = %d, want 1", len(r.nextCheck))
	}
	if n := len(r.checkOrder); n <= 1 {
		t.Fatalf("heap len = %d; it must not be rebuilt on every remember", n)
	}
}

// 取得の失敗は相手の問題なので Warn にせず、障害の Warn の間引きも消費しない。
func TestMetadataRefresher_FetchFailureIsNotWarned(t *testing.T) {
	r, _, fetcher, _ := newRefresherFixture(t, nil)
	fetcher.err = errors.New("nodeinfo unreachable")
	var warned int
	r.warnf = func(string, ...any) { warned++ }
	r.MaybeRefresh("remote.example")
	r.wg.Wait()
	if warned != 0 {
		t.Fatalf("fetch failure warned %d times, want 0", warned)
	}
}

func TestRedisRefreshLock(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	lock := NewRedisRefreshLock(client)
	ctx := context.Background()

	release, ok, err := lock.TryAcquire(ctx, "remote.example")
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	if ttl := mr.TTL(metadataRefreshLockKeyPrefix + "remote.example"); ttl != metadataRefreshLockTTL {
		t.Fatalf("ttl = %v, want %v", ttl, metadataRefreshLockTTL)
	}
	if _, ok, err := lock.TryAcquire(ctx, "remote.example"); err != nil || ok {
		t.Fatalf("second acquire must fail: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := lock.TryAcquire(ctx, "other.example"); !ok {
		t.Fatal("different host must not share the lock")
	}
	release()
	if _, ok, _ := lock.TryAcquire(ctx, "remote.example"); !ok {
		t.Fatal("acquire after release must succeed")
	}

	// TTL が切れて他の holder が取った後の release は、その holder の鍵を消さない。
	release2, _, _ := lock.TryAcquire(ctx, "late.example")
	mr.FastForward(metadataRefreshLockTTL + time.Second)
	if _, ok, _ := lock.TryAcquire(ctx, "late.example"); !ok {
		t.Fatal("acquire after TTL must succeed")
	}
	release2()
	if !mr.Exists(metadataRefreshLockKeyPrefix + "late.example") {
		t.Fatal("stale release must not delete the new holder's lock")
	}

	testutil.MiniredisDown(mr)
	if _, _, err := lock.TryAcquire(ctx, "remote.example"); err == nil {
		t.Fatal("redis down must return an error")
	}
}
