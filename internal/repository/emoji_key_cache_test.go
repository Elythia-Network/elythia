package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyedEmojiRepo is a fake inner repository for the FindManyByKeys cache. It
// serves rows from a fixed table and records every key it was asked for.
type keyedEmojiRepo struct {
	countingEmojiRepo

	mu    sync.Mutex
	rows  []*model.Emoji
	asked [][]model.EmojiKey
	err   error
	// entered / release let a test hold a fetch in flight.
	entered chan struct{}
	release chan struct{}
	panicOn bool
}

func (r *keyedEmojiRepo) FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error) {
	r.mu.Lock()
	r.asked = append(r.asked, append([]model.EmojiKey(nil), keys...))
	entered, release, err, panicOn := r.entered, r.release, r.err, r.panicOn
	r.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if panicOn {
		panic("boom")
	}
	if err != nil {
		return nil, err
	}
	want := map[model.EmojiKey]struct{}{}
	for _, k := range keys {
		want[k] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*model.Emoji
	for _, e := range r.rows {
		if _, ok := want[model.EmojiKeyOf(e)]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *keyedEmojiRepo) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
}

func (r *keyedEmojiRepo) lastAsked() []model.EmojiKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.asked) == 0 {
		return nil
	}
	return r.asked[len(r.asked)-1]
}

func (r *keyedEmojiRepo) setRows(rows ...*model.Emoji) {
	r.mu.Lock()
	r.rows = rows
	r.mu.Unlock()
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func remoteEmoji(id, name, host, url string) *model.Emoji {
	h := host
	return &model.Emoji{ID: id, Name: name, Host: &h, OriginalURL: url, PublicURL: url}
}

func rk(name, host string) model.EmojiKey { return model.EmojiKey{Name: name, Host: host} }

func newKeyCache(inner *keyedEmojiRepo, opts repository.EmojiKeyCacheOptions) (*repository.CachedEmojiRepository, *fakeClock) {
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	if opts.Now == nil {
		opts.Now = clk.Now
	}
	return repository.NewCachedEmojiRepositoryWithOptions(inner, time.Hour, opts), clk
}

func urlsOf(rows []*model.Emoji) []string {
	out := make([]string, 0, len(rows))
	for _, e := range rows {
		out = append(out, e.Name+"@"+*e.Host+"="+e.PublicURL)
	}
	sort.Strings(out)
	return out
}

func TestEmojiKeyCache_HitDoesNotFetchAgain(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{
		remoteEmoji("e1", "blob", "a.example", "https://a.example/blob.png"),
		remoteEmoji("e2", "cat", "b.example", "https://b.example/cat.png"),
	}}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})

	keys := []model.EmojiKey{rk("blob", "a.example"), rk("cat", "b.example")}
	first, err := c.FindManyByKeys(keys)
	require.NoError(t, err)
	second, err := c.FindManyByKeys(keys)
	require.NoError(t, err)

	assert.Equal(t, 1, inner.calls(), "the second resolution must be served from the cache")
	assert.Equal(t, urlsOf(first), urlsOf(second))
	assert.Equal(t, []string{"blob@a.example=https://a.example/blob.png", "cat@b.example=https://b.example/cat.png"}, urlsOf(second))
	// 複数 host を 1 回で引く。
	assert.ElementsMatch(t, keys, inner.asked[0])
}

func TestEmojiKeyCache_FetchesOnlyMisses(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{
		remoteEmoji("e1", "blob", "a.example", "u1"),
		remoteEmoji("e2", "cat", "a.example", "u2"),
	}}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})

	_, err := c.FindManyByKeys([]model.EmojiKey{rk("blob", "a.example")})
	require.NoError(t, err)
	got, err := c.FindManyByKeys([]model.EmojiKey{rk("blob", "a.example"), rk("cat", "a.example"), rk("cat", "a.example")})
	require.NoError(t, err)

	require.Equal(t, 2, inner.calls())
	assert.Equal(t, []model.EmojiKey{rk("cat", "a.example")}, inner.lastAsked(),
		"only the uncached key is fetched, once even if repeated")
	assert.Len(t, got, 2)
}

func TestEmojiKeyCache_ReturnsCopies(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "u1")}}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})

	got, err := c.FindManyByKeys([]model.EmojiKey{rk("blob", "a.example")})
	require.NoError(t, err)
	got[0].PublicURL = "mutated"
	again, err := c.FindManyByKeys([]model.EmojiKey{rk("blob", "a.example")})
	require.NoError(t, err)
	assert.Equal(t, "u1", again[0].PublicURL, "a caller mutating its result must not corrupt the cache")
}

func TestEmojiKeyCache_PositiveTTLExpiry(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "u1")}}
	c, clk := newKeyCache(inner, repository.EmojiKeyCacheOptions{TTL: 10 * time.Minute, NegativeTTL: time.Minute})
	keys := []model.EmojiKey{rk("blob", "a.example")}

	_, _ = c.FindManyByKeys(keys)
	clk.Advance(9 * time.Minute)
	_, _ = c.FindManyByKeys(keys)
	assert.Equal(t, 1, inner.calls(), "still fresh before the TTL (and past the negative TTL)")

	clk.Advance(2 * time.Minute)
	_, _ = c.FindManyByKeys(keys)
	assert.Equal(t, 2, inner.calls(), "refetched after the TTL")
}

func TestEmojiKeyCache_NegativeEntryUsesShortTTL(t *testing.T) {
	inner := &keyedEmojiRepo{}
	c, clk := newKeyCache(inner, repository.EmojiKeyCacheOptions{TTL: 10 * time.Minute, NegativeTTL: time.Minute})
	keys := []model.EmojiKey{rk("missing", "a.example")}

	got, err := c.FindManyByKeys(keys)
	require.NoError(t, err)
	assert.Empty(t, got)
	_, _ = c.FindManyByKeys(keys)
	assert.Equal(t, 1, inner.calls(), "a miss is cached")

	// 後から行ができても (経路外の書き込み)、負の TTL が切れれば拾う。
	inner.setRows(remoteEmoji("e1", "missing", "a.example", "u1"))
	clk.Advance(61 * time.Second)
	got, err = c.FindManyByKeys(keys)
	require.NoError(t, err)
	assert.Equal(t, 2, inner.calls())
	assert.Len(t, got, 1)
}

func TestEmojiKeyCache_EvictsLeastRecentlyUsed(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{
		remoteEmoji("e1", "a", "h.example", "ua"),
		remoteEmoji("e2", "b", "h.example", "ub"),
		remoteEmoji("e3", "c", "h.example", "uc"),
	}}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{Capacity: 2})

	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("a", "h.example")})
	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("b", "h.example")})
	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("a", "h.example")}) // a を最近使ったものにする
	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("c", "h.example")}) // b が追い出される
	require.Equal(t, 3, inner.calls())

	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("a", "h.example")})
	assert.Equal(t, 3, inner.calls(), "recently used entry survives eviction")
	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("b", "h.example")})
	assert.Equal(t, 4, inner.calls(), "least recently used entry was evicted")
	assert.Equal(t, []model.EmojiKey{rk("b", "h.example")}, inner.lastAsked())
}

func TestEmojiKeyCache_CreateDropsNegativeEntry(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{remoteEmoji("e0", "other", "a.example", "u0")}}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	keys := []model.EmojiKey{rk("new", "a.example"), rk("other", "a.example")}

	_, _ = c.FindManyByKeys(keys)
	created := remoteEmoji("e1", "new", "a.example", "u1")
	inner.setRows(inner.rows[0], created)
	require.NoError(t, c.Create(created))

	got, err := c.FindManyByKeys(keys)
	require.NoError(t, err)
	assert.Equal(t, 2, inner.calls())
	assert.Equal(t, []model.EmojiKey{rk("new", "a.example")}, inner.lastAsked(),
		"Create drops only the created key")
	assert.Len(t, got, 2)
}

func TestEmojiKeyCache_UpdateAndDeleteDropByID(t *testing.T) {
	cases := []struct {
		name  string
		write func(c *repository.CachedEmojiRepository) error
	}{
		{"UpdateFields", func(c *repository.CachedEmojiRepository) error {
			return c.UpdateFields("e1", map[string]any{"publicUrl": "u1b"})
		}},
		{"UpdateFieldsMany", func(c *repository.CachedEmojiRepository) error {
			return c.UpdateFieldsMany([]string{"e1"}, map[string]any{"publicUrl": "u1b"})
		}},
		{"Delete", func(c *repository.CachedEmojiRepository) error { return c.Delete("e1") }},
		{"DeleteMany", func(c *repository.CachedEmojiRepository) error { return c.DeleteMany([]string{"e1"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &keyedEmojiRepo{rows: []*model.Emoji{
				remoteEmoji("e1", "blob", "a.example", "u1"),
				remoteEmoji("e2", "cat", "a.example", "u2"),
			}}
			c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
			keys := []model.EmojiKey{rk("blob", "a.example"), rk("cat", "a.example")}
			_, _ = c.FindManyByKeys(keys)

			require.NoError(t, tc.write(c))
			_, _ = c.FindManyByKeys(keys)
			require.Equal(t, 2, inner.calls())
			assert.Equal(t, []model.EmojiKey{rk("blob", "a.example")}, inner.lastAsked(),
				"only the entry holding the written row is dropped")
		})
	}
}

func TestEmojiKeyCache_RenameDropsEverything(t *testing.T) {
	for _, field := range []string{"name", "host"} {
		t.Run(field, func(t *testing.T) {
			inner := &keyedEmojiRepo{rows: []*model.Emoji{remoteEmoji("e2", "cat", "a.example", "u2")}}
			c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
			keys := []model.EmojiKey{rk("renamed", "a.example"), rk("cat", "a.example")}
			_, _ = c.FindManyByKeys(keys) // renamed は負のエントリになる

			// e9 (cache に無い行) を renamed へ改名する。
			require.NoError(t, c.UpdateFields("e9", map[string]any{field: "renamed"}))
			_, _ = c.FindManyByKeys(keys)
			require.Equal(t, 2, inner.calls())
			assert.ElementsMatch(t, keys, inner.lastAsked(), "a rename drops the whole cache, negatives included")
		})
	}
}

func TestEmojiKeyCache_FailedWriteStillInvalidates(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "u1")}}
	inner.deleteErr = errors.New("connection reset after commit")
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	var hooked []repository.EmojiCacheInvalidation
	c.SetInvalidationHook(func(ev repository.EmojiCacheInvalidation) { hooked = append(hooked, ev) })
	keys := []model.EmojiKey{rk("blob", "a.example")}

	_, _ = c.FindManyByKeys(keys)
	require.Error(t, c.Delete("e1"))
	_, _ = c.FindManyByKeys(keys)
	assert.Equal(t, 2, inner.calls(), "a write that may have committed must still drop the entry")
	assert.Equal(t, []repository.EmojiCacheInvalidation{{IDs: []string{"e1"}}}, hooked)
}

func TestEmojiKeyCache_HookReceivesEachWrite(t *testing.T) {
	inner := &keyedEmojiRepo{}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	var hooked []repository.EmojiCacheInvalidation
	c.SetInvalidationHook(func(ev repository.EmojiCacheInvalidation) { hooked = append(hooked, ev) })

	require.NoError(t, c.Create(remoteEmoji("e1", "blob", "a.example", "u1")))
	require.NoError(t, c.Create(&model.Emoji{ID: "e2", Name: "local"}))
	require.NoError(t, c.UpdateFields("e1", map[string]any{"publicUrl": "x"}))
	require.NoError(t, c.UpdateFieldsMany([]string{"e1", "e2"}, map[string]any{"name": "y"}))
	require.NoError(t, c.Delete("e1"))
	require.NoError(t, c.DeleteMany([]string{"e1", "e2"}))

	assert.Equal(t, []repository.EmojiCacheInvalidation{
		{Keys: []model.EmojiKey{rk("blob", "a.example")}},
		{Keys: []model.EmojiKey{rk("local", "")}},
		{IDs: []string{"e1"}},
		{All: true},
		{IDs: []string{"e1"}},
		{IDs: []string{"e1", "e2"}},
	}, hooked)
}

func TestEmojiKeyCache_ApplyRemoteInvalidation(t *testing.T) {
	inner := &keyedEmojiRepo{
		countingEmojiRepo: countingEmojiRepo{emojis: []*model.Emoji{{ID: "l1", Name: "local"}}},
		rows: []*model.Emoji{
			remoteEmoji("e1", "blob", "a.example", "u1"),
			remoteEmoji("e2", "cat", "a.example", "u2"),
		},
	}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	hooked := 0
	c.SetInvalidationHook(func(repository.EmojiCacheInvalidation) { hooked++ })
	keys := []model.EmojiKey{rk("blob", "a.example"), rk("cat", "a.example"), rk("gone", "a.example")}
	_, _ = c.FindManyByKeys(keys)
	_, _ = c.ListLocal()

	// 他プロセスのリモート絵文字の作成: そのキーだけ落ち、ListLocal は残る。
	c.ApplyRemoteInvalidation(repository.EmojiCacheInvalidation{Keys: []model.EmojiKey{rk("gone", "a.example")}})
	_, _ = c.FindManyByKeys(keys)
	_, _ = c.ListLocal()
	assert.Equal(t, []model.EmojiKey{rk("gone", "a.example")}, inner.lastAsked())
	assert.Equal(t, int64(1), inner.listLocalCalls.Load(), "a remote-only change keeps ListLocal")

	// id 指定: その行のエントリが落ち、host が分からないので ListLocal も落ちる。
	c.ApplyRemoteInvalidation(repository.EmojiCacheInvalidation{IDs: []string{"e2"}})
	_, _ = c.FindManyByKeys(keys)
	_, _ = c.ListLocal()
	assert.Equal(t, []model.EmojiKey{rk("cat", "a.example")}, inner.lastAsked())
	assert.Equal(t, int64(2), inner.listLocalCalls.Load())

	// local のキー: ListLocal も落ちる。
	c.ApplyRemoteInvalidation(repository.EmojiCacheInvalidation{Keys: []model.EmojiKey{rk("local", "")}})
	_, _ = c.ListLocal()
	assert.Equal(t, int64(3), inner.listLocalCalls.Load())

	// All: 全部落ちる。
	before := inner.calls()
	c.ApplyRemoteInvalidation(repository.EmojiCacheInvalidation{All: true})
	_, _ = c.FindManyByKeys(keys)
	assert.Equal(t, before+1, inner.calls())
	assert.ElementsMatch(t, keys, inner.lastAsked())

	assert.Zero(t, hooked, "applying a remote invalidation must not publish again")
}

func TestEmojiKeyCache_ErrorIsNotCachedAndHitsSurvive(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "u1")}}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("blob", "a.example")})

	inner.mu.Lock()
	inner.err = errors.New("db down")
	inner.mu.Unlock()
	got, err := c.FindManyByKeys([]model.EmojiKey{rk("blob", "a.example"), rk("cat", "a.example")})
	require.Error(t, err)
	assert.Equal(t, []string{"blob@a.example=u1"}, urlsOf(got), "cache hits are still returned on error")

	inner.mu.Lock()
	inner.err = nil
	inner.mu.Unlock()
	_, err = c.FindManyByKeys([]model.EmojiKey{rk("cat", "a.example")})
	require.NoError(t, err)
	assert.Equal(t, 3, inner.calls(), "a failed fetch must not be cached as a miss")
}

func TestEmojiKeyCache_SkipsUnstorableKeys(t *testing.T) {
	inner := &keyedEmojiRepo{}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})

	got, err := c.FindManyByKeys([]model.EmojiKey{rk("bad\x00", "a.example"), rk("ok", "b\x00ad")})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Zero(t, inner.calls(), "keys that cannot match a row are never fetched")
	_, err = c.FindManyByKeys(nil)
	require.NoError(t, err)
	assert.Zero(t, inner.calls())
}

// waitForWaiters blocks until n callers wait on the in-flight fetch of k.
func waitForWaiters(t *testing.T, c *repository.CachedEmojiRepository, k model.EmojiKey, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for repository.EmojiKeyWaiters(c, k) < n {
		if time.Now().After(deadline) {
			t.Errorf("only %d of %d callers joined the in-flight fetch", repository.EmojiKeyWaiters(c, k), n)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEmojiKeyCache_ConcurrentMissesShareOneFetch(t *testing.T) {
	inner := &keyedEmojiRepo{
		rows:    []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "u1")},
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	keys := []model.EmojiKey{rk("blob", "a.example")}

	const n = 8
	var wg sync.WaitGroup
	var resolved atomic.Int64
	run := func() {
		defer wg.Done()
		got, err := c.FindManyByKeys(keys)
		if err == nil && len(got) == 1 && got[0].PublicURL == "u1" {
			resolved.Add(1)
		}
	}
	wg.Add(1)
	go run()
	<-inner.entered // 1 本目が取得中になった
	for i := 1; i < n; i++ {
		wg.Add(1)
		go run()
	}
	// 残りが全員待ち合わせに入るまで待つ。重複を抑えない実装では待ち合わせに
	// 入らず inner へ行くので、ここで時間切れになる。
	waitForWaiters(t, c, keys[0], n-1)
	close(inner.release)
	wg.Wait()

	assert.Equal(t, 1, inner.calls(), "concurrent misses for the same key must share one fetch")
	assert.Equal(t, int64(n), resolved.Load(), "every caller receives the shared result")
}

func TestEmojiKeyCache_WaitersShareFetchError(t *testing.T) {
	inner := &keyedEmojiRepo{
		err:     errors.New("db down"),
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	keys := []model.EmojiKey{rk("blob", "a.example")}

	errs := make(chan error, 2)
	go func() { _, err := c.FindManyByKeys(keys); errs <- err }()
	<-inner.entered
	go func() { _, err := c.FindManyByKeys(keys); errs <- err }()
	waitForWaiters(t, c, keys[0], 1)
	close(inner.release)
	assert.Error(t, <-errs)
	assert.Error(t, <-errs)
	assert.Equal(t, 1, inner.calls(), "a waiter does not refetch after the shared fetch failed")
}

func TestEmojiKeyCache_WriteDuringFetchDiscardsResult(t *testing.T) {
	inner := &keyedEmojiRepo{
		rows:    []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "old")},
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	keys := []model.EmojiKey{rk("blob", "a.example")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.FindManyByKeys(keys)
	}()
	<-inner.entered
	// 取得中に URL が書き換わる。取得は書き込み前の値 (old) を返す。
	require.NoError(t, c.UpdateFields("e1", map[string]any{"publicUrl": "new"}))
	rel := inner.release
	inner.mu.Lock()
	inner.entered, inner.release = nil, nil
	inner.mu.Unlock()
	close(rel)
	<-done
	inner.setRows(remoteEmoji("e1", "blob", "a.example", "new"))

	got, err := c.FindManyByKeys(keys)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "new", got[0].PublicURL, "a fetch overtaken by a write must not be cached")
}

func TestEmojiKeyCache_PanickingFetchReleasesKeys(t *testing.T) {
	inner := &keyedEmojiRepo{panicOn: true}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	keys := []model.EmojiKey{rk("blob", "a.example")}

	require.Panics(t, func() { _, _ = c.FindManyByKeys(keys) })

	inner.mu.Lock()
	inner.panicOn = false
	inner.rows = []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "u1")}
	inner.mu.Unlock()
	done := make(chan []*model.Emoji, 1)
	go func() {
		got, _ := c.FindManyByKeys(keys)
		done <- got
	}()
	select {
	case got := <-done:
		assert.Len(t, got, 1)
	case <-time.After(5 * time.Second):
		t.Fatal("a later caller is stuck waiting on the fetch that panicked")
	}
}

func TestEmojiKeyCache_CallerAfterWriteDoesNotJoinOlderFetch(t *testing.T) {
	inner := &keyedEmojiRepo{
		rows:    []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "u1")},
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{})
	keys := []model.EmojiKey{rk("blob", "a.example")}

	rel := inner.release
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.FindManyByKeys(keys)
	}()
	<-inner.entered
	require.NoError(t, c.Create(remoteEmoji("e1", "blob", "a.example", "u1")))

	// 書き込みより後に来た呼び出しは、書き込み前に始まった取得を待たずに
	// 自分で取り直す。
	inner.mu.Lock()
	inner.entered, inner.release = nil, nil
	inner.mu.Unlock()
	second := make(chan []*model.Emoji, 1)
	go func() {
		got, _ := c.FindManyByKeys(keys)
		second <- got
	}()
	select {
	case got := <-second:
		assert.Len(t, got, 1)
		assert.Equal(t, 2, inner.calls())
	case <-time.After(2 * time.Second):
		t.Error("a caller arriving after the write joined the fetch that started before it")
	}
	close(rel)
	<-done
}

func TestEmojiKeyCache_EvictionDropsIDIndex(t *testing.T) {
	inner := &keyedEmojiRepo{rows: []*model.Emoji{
		remoteEmoji("e1", "a", "h.example", "ua"),
		remoteEmoji("e2", "b", "h.example", "ub"),
	}}
	c, _ := newKeyCache(inner, repository.EmojiKeyCacheOptions{Capacity: 1})

	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("a", "h.example")})
	_, _ = c.FindManyByKeys([]model.EmojiKey{rk("b", "h.example")}) // a が追い出される
	assert.Equal(t, 1, repository.EmojiKeyByIDCount(c), "an evicted entry must leave the id index")
}

// partialEmojiRepo returns some rows together with an error, like a
// multi-chunk fetch whose later chunk failed.
type partialEmojiRepo struct {
	keyedEmojiRepo
}

func (r *partialEmojiRepo) FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error) {
	rows, _ := r.keyedEmojiRepo.FindManyByKeys(keys)
	return rows, errors.New("second chunk failed")
}

func TestEmojiKeyCache_PartialRowsOnErrorReachOwnerAndWaiters(t *testing.T) {
	inner := &partialEmojiRepo{keyedEmojiRepo{
		rows:    []*model.Emoji{remoteEmoji("e1", "blob", "a.example", "u1")},
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}}
	c := repository.NewCachedEmojiRepositoryWithOptions(inner, time.Hour, repository.EmojiKeyCacheOptions{})
	keys := []model.EmojiKey{rk("blob", "a.example"), rk("cat", "a.example")}

	type result struct {
		rows []*model.Emoji
		err  error
	}
	owner := make(chan result, 1)
	go func() { r, err := c.FindManyByKeys(keys); owner <- result{r, err} }()
	<-inner.entered
	waiter := make(chan result, 1)
	go func() { r, err := c.FindManyByKeys(keys[:1]); waiter <- result{r, err} }()
	waitForWaiters(t, c, keys[0], 1)
	close(inner.release)

	for name, ch := range map[string]chan result{"owner": owner, "waiter": waiter} {
		got := <-ch
		assert.Error(t, got.err, name)
		assert.Equal(t, []string{"blob@a.example=u1"}, urlsOf(got.rows), "%s keeps the rows that were returned", name)
	}
	// 失敗した取得は載せない。
	inner.mu.Lock()
	inner.entered, inner.release = nil, nil
	inner.mu.Unlock()
	_, _ = c.FindManyByKeys(keys[:1])
	assert.Equal(t, 2, inner.calls())
}

func TestEmojiCacheInvalidation_JSONRoundTrip(t *testing.T) {
	for _, ev := range []repository.EmojiCacheInvalidation{
		{Keys: []model.EmojiKey{rk("blob", "a.example"), rk("local", "")}},
		{IDs: []string{"e1", "e2"}},
		{All: true},
		{Keys: []model.EmojiKey{rk("x", "b.example")}, IDs: []string{"e3"}, All: true},
	} {
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		var got repository.EmojiCacheInvalidation
		require.NoError(t, json.Unmarshal(b, &got))
		assert.Equal(t, ev, got, string(b))
	}
}

func TestEmojiInvalidationQueue_PublishesInOrder(t *testing.T) {
	published := make(chan repository.EmojiCacheInvalidation, 8)
	q := repository.NewEmojiInvalidationQueue(4, func(ev repository.EmojiCacheInvalidation) { published <- ev })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{"e1"}})
	q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{"e2"}})
	for _, want := range []string{"e1", "e2"} {
		select {
		case ev := <-published:
			assert.Equal(t, []string{want}, ev.IDs)
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was never published", want)
		}
	}
}

func TestEmojiInvalidationQueue_OverflowCollapsesToAll(t *testing.T) {
	entered := make(chan struct{}, 8)
	gate := make(chan struct{})
	var mu sync.Mutex
	var published []repository.EmojiCacheInvalidation
	q := repository.NewEmojiInvalidationQueue(2, func(ev repository.EmojiCacheInvalidation) {
		entered <- struct{}{}
		<-gate
		mu.Lock()
		published = append(published, ev)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{"a"}})
	select {
	case <-entered: // 送信側が a の publish で止まっている
	case <-time.After(2 * time.Second):
		t.Fatal("the sender never started publishing")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// buffer は 2 なので b, c で埋まり、d は溢れる。Enqueue は止まらない。
		for _, id := range []string{"b", "c", "d"} {
			q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{id}})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked on a full queue")
	}
	close(gate)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(published)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// 溢れた後に溜まっていたものが出切るのを少し待ち、余計な publish が無いことを見る。
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []repository.EmojiCacheInvalidation{
		{IDs: []string{"a"}},
		{All: true},
	}, published, "an overflow collapses everything pending into one All")
}

func TestEmojiInvalidationQueue_OverflowWhileIdlePublishesAll(t *testing.T) {
	published := make(chan repository.EmojiCacheInvalidation, 8)
	q := repository.NewEmojiInvalidationQueue(1, func(ev repository.EmojiCacheInvalidation) { published <- ev })

	// 送信側を動かす前に溢れさせ、その後に起動する。
	q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{"a"}})
	q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{"b"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	select {
	case ev := <-published:
		assert.Equal(t, repository.EmojiCacheInvalidation{All: true}, ev)
	case <-time.After(2 * time.Second):
		t.Fatal("overflowed invalidation was never published")
	}
}

func TestEmojiInvalidationQueue_StopsOnContextDone(t *testing.T) {
	q := repository.NewEmojiInvalidationQueue(0, func(repository.EmojiCacheInvalidation) {})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}

func TestEmojiInvalidationQueue_DrainsPendingOnStop(t *testing.T) {
	var published []repository.EmojiCacheInvalidation
	q := repository.NewEmojiInvalidationQueue(8, func(ev repository.EmojiCacheInvalidation) {
		published = append(published, ev)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 停止の後に積まれた分 (停止中の受信ジョブ) も、Run が返る前なら送る。
	q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{"a"}})
	q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{"b"}})
	q.Run(ctx)
	assert.Equal(t, []repository.EmojiCacheInvalidation{{IDs: []string{"a"}}, {IDs: []string{"b"}}}, published)
}

func TestEmojiInvalidationQueue_DrainStopsAtDeadline(t *testing.T) {
	var n atomic.Int64
	q := repository.NewEmojiInvalidationQueue(8, func(repository.EmojiCacheInvalidation) {
		n.Add(1)
		time.Sleep(40 * time.Millisecond)
	})
	repository.SetEmojiInvalidationDrainTimeout(q, 60*time.Millisecond)
	for i := 0; i < 6; i++ {
		q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{fmt.Sprint(i)}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q.Run(ctx)
	assert.Less(t, n.Load(), int64(6), "draining on stop is bounded by the timeout")
	assert.Positive(t, n.Load())
}

func TestEmojiInvalidationQueue_NothingQueuedBehindAll(t *testing.T) {
	var published []repository.EmojiCacheInvalidation
	q := repository.NewEmojiInvalidationQueue(3, func(ev repository.EmojiCacheInvalidation) {
		published = append(published, ev)
	})
	for _, id := range []string{"a", "b", "c", "d"} { // d で溢れて All に畳まれる
		q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{id}})
	}
	q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{"e"}})
	q.Enqueue(repository.EmojiCacheInvalidation{Keys: []model.EmojiKey{rk("f", "x.example")}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q.Run(ctx)
	assert.Equal(t, []repository.EmojiCacheInvalidation{{All: true}}, published,
		"invalidations behind a pending All are covered by it")
}

// 書き込みが続いている間に止めても、Run は締め切りまでに返る。通常の送信が停止を
// 見ないと、pending が空にならない限り先頭の停止の確認へ戻れず、停止処理の予算を
// 使い切る (#3383 のレビューで実測: 3 秒たっても返らなかった)。
func TestEmojiInvalidationQueue_StopsWhileWritesContinue(t *testing.T) {
	q := repository.NewEmojiInvalidationQueue(1024, func(repository.EmojiCacheInvalidation) {
		time.Sleep(20 * time.Millisecond)
	})
	repository.SetEmojiInvalidationDrainTimeout(q, 100*time.Millisecond)

	stopWriting := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-stopWriting:
				return
			case <-time.After(5 * time.Millisecond):
				q.Enqueue(repository.EmojiCacheInvalidation{IDs: []string{fmt.Sprint(i)}})
			}
		}
	}()
	t.Cleanup(func() { close(stopWriting); <-writerDone })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return while writes kept coming")
	}
}
