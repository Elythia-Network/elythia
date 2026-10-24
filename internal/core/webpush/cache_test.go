package webpush_test

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("webpush test: redis setup failed: %v", err)
	}
	testRedis = tr

	code := m.Run()

	testRedis.Teardown(ctx)
	os.Exit(code)
}

// fakeSwRepo records FindByUserID calls and returns canned results.
type fakeSwRepo struct {
	data  map[string][]*model.SwSubscription
	calls int
	err   error
	// onFind runs inside FindByUserID, after the call is counted and the
	// result is read, so changing data in it does not change what this call
	// returns.
	onFind func()
}

func (f *fakeSwRepo) FindByUserAndEndpoint(_, _ string) (*model.SwSubscription, error) {
	return nil, errors.New("not used")
}
func (f *fakeSwRepo) FindByUserEndpointAuthKey(_, _, _, _ string) (*model.SwSubscription, error) {
	return nil, errors.New("not used")
}
func (f *fakeSwRepo) FindByUserID(userID string) ([]*model.SwSubscription, error) {
	f.calls++
	res := f.data[userID]
	if f.onFind != nil {
		f.onFind()
	}
	if f.err != nil {
		return nil, f.err
	}
	return res, nil
}
func (f *fakeSwRepo) Create(_ *model.SwSubscription) error { return nil }
func (f *fakeSwRepo) Update(_ *model.SwSubscription) error { return nil }
func (f *fakeSwRepo) FindByEndpointAuthKey(_ *string, _, _, _ string) ([]*model.SwSubscription, error) {
	return nil, errors.New("not used")
}
func (f *fakeSwRepo) DeleteByIDs(_ []string) error                     { return nil }
func (f *fakeSwRepo) DeleteByUserAndEndpoint(_ string, _ string) error { return nil }

func TestCache_GetFetchesFromRepo(t *testing.T) {
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{
		"u1": {{ID: "s1", UserID: "u1", Endpoint: "https://push.example/1"}},
	}}
	cache := webpush.NewSubscriptionCache(repo, nil)
	got, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, 1, repo.calls)
}

func TestCache_GetHitsMemoryCache(t *testing.T) {
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{
		"u1": {{ID: "s1"}},
	}}
	cache := webpush.NewSubscriptionCache(repo, nil)

	_, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	_, err = cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, 1, repo.calls, "memory cache should be reused")
}

func TestCache_InvalidateForcesRefetch(t *testing.T) {
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{
		"u1": {{ID: "s1"}},
	}}
	cache := webpush.NewSubscriptionCache(repo, nil)

	_, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	cache.Invalidate(context.Background(), "u1")
	_, err = cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, 2, repo.calls)
}

func TestCache_GetRepoError(t *testing.T) {
	repo := &fakeSwRepo{err: errors.New("boom")}
	cache := webpush.NewSubscriptionCache(repo, nil)
	_, err := cache.Get(context.Background(), "u1")
	assert.Error(t, err)
}

func TestCache_EmptyUser(t *testing.T) {
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{}}
	cache := webpush.NewSubscriptionCache(repo, nil)
	got, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCache_WithRedis_RepoMissThenHit(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{
		"u1": {{ID: "s1", UserID: "u1", Endpoint: "https://push.example/1"}},
	}}
	cache := webpush.NewSubscriptionCache(repo, testRedis.Client)

	// 1st call: miss -> hits repo, populates memory + redis
	_, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, 1, repo.calls)

	// 2nd call: memory hit, repo not called
	_, err = cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, 1, repo.calls)

	// Invalidate memory only by constructing a fresh cache over the same Redis
	// to force the Redis path.
	cache2 := webpush.NewSubscriptionCache(repo, testRedis.Client)
	_, err = cache2.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, 1, repo.calls, "redis cache should be reused")
}

func TestCache_WithRedis_Invalidate(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{
		"u1": {{ID: "s1"}},
	}}
	cache := webpush.NewSubscriptionCache(repo, testRedis.Client)

	_, _ = cache.Get(context.Background(), "u1")
	cache.Invalidate(context.Background(), "u1")
	// Redis key is deleted
	n, err := testRedis.Client.Exists(context.Background(), "userSwSubscriptions:u1").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

func TestCache_WithRedis_CorruptValueFallsThroughToRepo(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{
		"u1": {{ID: "s1"}},
	}}
	// 直接Redisに壊れたJSONを書き込む
	err := testRedis.Client.Set(context.Background(), "userSwSubscriptions:u1", "not-json", 0).Err()
	require.NoError(t, err)

	cache := webpush.NewSubscriptionCache(repo, testRedis.Client)
	got, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, 1, repo.calls)
}

// DropLocal drops the memory layer only. Redis の層はプロセスの間で共有するので
// 残り、捨てた後の Get は Redis から読む。
func TestCache_DropLocal_DropsMemoryOnly(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{
		"u1": {{ID: "s1"}},
		"u2": {{ID: "s2"}},
	}}
	cache := webpush.NewSubscriptionCache(repo, testRedis.Client)
	for _, u := range []string{"u1", "u2"} {
		_, err := cache.Get(context.Background(), u)
		require.NoError(t, err)
	}
	require.NoError(t, testRedis.Client.Del(context.Background(), "userSwSubscriptions:u2").Err())

	cache.DropLocal()
	_, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, 2, repo.calls, "u1 is read from Redis, which DropLocal leaves alone")
	_, err = cache.Get(context.Background(), "u2")
	require.NoError(t, err)
	assert.Equal(t, 3, repo.calls, "u2 is gone from memory, so it is read from the DB")
}

// 読み込みの途中で捨てたときは、捨てる前に読んだ行をメモリにも Redis にも積まない。
func TestCache_DropLocal_DuringReadIsNotStored(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "s1"}}}}
	cache := webpush.NewSubscriptionCache(repo, testRedis.Client)
	repo.onFind = func() {
		repo.onFind = nil
		cache.DropLocal()
	}
	got, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Len(t, got, 1, "the caller still gets what it read")

	n, err := testRedis.Client.Exists(context.Background(), "userSwSubscriptions:u1").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "the rows read before the drop are not written to Redis")
	_, err = cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, 2, repo.calls, "the rows read before the drop are not kept in memory")
}

// onCmdHook runs before (or after) the first Redis command with the given
// name. テストから、Get が Redis を読む前後・書く直前に Invalidate を割り込ませる。
type onCmdHook struct {
	name  string
	after bool
	fn    func()
	fired atomic.Bool
}

func (h *onCmdHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *onCmdHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		hit := cmd.Name() == h.name && h.fired.CompareAndSwap(false, true)
		if hit && !h.after {
			h.fn()
		}
		err := next(ctx, cmd)
		if hit && h.after {
			h.fn()
		}
		return err
	}
}
func (h *onCmdHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// hookedRedis returns a client to the test Redis that runs hook.
func hookedRedis(t *testing.T, hook redis.Hook) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: testRedis.Addr})
	t.Cleanup(func() { _ = c.Close() })
	c.AddHook(hook)
	return c
}

// An Invalidate that lands while Get reads the DB keeps what Get read out of
// both layers (#3804). 登録の直前に読み始めた配送が、新しい購読を含まない一覧を
// 積み直さないこと。
func TestCache_InvalidateDuringDBReadIsNotStored(t *testing.T) {
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "old"}}}}
	cache := webpush.NewSubscriptionCache(repo, testRedis.Client)
	repo.onFind = func() {
		repo.onFind = nil
		// DB を読んだ後に購読が増え、登録の API が Invalidate を呼ぶ形。
		repo.data["u1"] = []*model.SwSubscription{{ID: "old"}, {ID: "new"}}
		cache.Invalidate(ctx, "u1")
	}
	got, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, got, 1, "前提: 割り込まれた Get は無効化の前の一覧を返す")

	n, err := testRedis.Client.Exists(ctx, "userSwSubscriptions:u1").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "the list read before the invalidation is not written to Redis")

	got, err = cache.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, got, 2, "the list read before the invalidation is kept in memory")
	assert.Equal(t, 2, repo.calls)
}

// An Invalidate that lands after Get read Redis keeps the list out of memory
// (#3804).
func TestCache_InvalidateDuringRedisReadIsNotStored(t *testing.T) {
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "old"}}}}
	// 別のプロセスが積んだ古い一覧が Redis にある。
	raw, err := json.Marshal(repo.data["u1"])
	require.NoError(t, err)
	require.NoError(t, testRedis.Client.Set(ctx, "userSwSubscriptions:u1", raw, time.Hour).Err())

	var cache *webpush.SubscriptionCache
	hook := &onCmdHook{name: "get", after: true, fn: func() {
		repo.data["u1"] = []*model.SwSubscription{{ID: "old"}, {ID: "new"}}
		cache.Invalidate(ctx, "u1")
	}}
	cache = webpush.NewSubscriptionCache(repo, hookedRedis(t, hook))

	got, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	require.True(t, hook.fired.Load(), "前提: Redis を読んだ直後に無効化が入る")
	require.Len(t, got, 1, "前提: 割り込まれた Get は Redis から読んだ一覧を返す")
	require.Equal(t, 0, repo.calls)

	got, err = cache.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, got, 2, "the list read from Redis before the invalidation is kept in memory")
	assert.Equal(t, 1, repo.calls)
}

// An Invalidate that lands after Get stored the list in memory but before it
// wrote Redis does not leave the list in Redis (#3804). Invalidate の Del が
// Get の Set より先に届く形。
func TestCache_InvalidateBeforeRedisWriteIsNotLeftInRedis(t *testing.T) {
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "old"}}}}
	var cache *webpush.SubscriptionCache
	hook := &onCmdHook{name: "set", fn: func() {
		repo.data["u1"] = []*model.SwSubscription{{ID: "old"}, {ID: "new"}}
		cache.Invalidate(ctx, "u1")
	}}
	cache = webpush.NewSubscriptionCache(repo, hookedRedis(t, hook))

	got, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	require.True(t, hook.fired.Load(), "前提: Redis へ書く直前に無効化が入る")
	require.Len(t, got, 1)

	n, err := testRedis.Client.Exists(ctx, "userSwSubscriptions:u1").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "the list read before the invalidation is left in Redis")

	// 別のプロセス (メモリを持たない) から見ても新しい一覧が読める。
	other := webpush.NewSubscriptionCache(repo, testRedis.Client)
	got, err = other.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, got, 2)
	got, err = cache.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// A Get that read the list Redis held between another Get's Set and that
// Get's own Del (it found its Set overtaken by an invalidation) does not keep
// the list in memory (#3804).
func TestCache_ReadOfOvertakenSetIsNotKept(t *testing.T) {
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "old"}}}}
	var cache *webpush.SubscriptionCache
	invalidate := &onCmdHook{name: "set", fn: func() {
		repo.data["u1"] = []*model.SwSubscription{{ID: "old"}, {ID: "new"}}
		cache.Invalidate(ctx, "u1")
	}}
	var between []*model.SwSubscription
	readBetween := &onCmdHook{name: "set", after: true, fn: func() {
		// Set の後、Del の前に、別の読み込みが Redis の古い行を読む。
		var err error
		between, err = cache.Get(ctx, "u1")
		require.NoError(t, err)
	}}
	rdb := redis.NewClient(&redis.Options{Addr: testRedis.Addr})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(invalidate)
	rdb.AddHook(readBetween)
	cache = webpush.NewSubscriptionCache(repo, rdb)

	_, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	require.True(t, readBetween.fired.Load())
	require.Len(t, between, 1, "前提: 間に入った読み込みは Redis の古い行を読む")
	require.Equal(t, 1, repo.calls)

	got, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, got, 2, "the list read from the overtaken Set is kept in memory")
	assert.Equal(t, 2, repo.calls)
}

// A Get that reads Redis after Invalidate advanced the generation but before
// its Del arrived does not keep the old list in memory (#3804).
func TestCache_ReadBeforeInvalidateDelIsNotKept(t *testing.T) {
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "old"}, {ID: "new"}}}}
	// 別のプロセスが積んだ古い一覧が Redis にある。
	raw, err := json.Marshal([]*model.SwSubscription{{ID: "old"}})
	require.NoError(t, err)
	require.NoError(t, testRedis.Client.Set(ctx, "userSwSubscriptions:u1", raw, time.Hour).Err())

	var cache *webpush.SubscriptionCache
	var between []*model.SwSubscription
	hook := &onCmdHook{name: "del", fn: func() {
		var err error
		between, err = cache.Get(ctx, "u1")
		require.NoError(t, err)
	}}
	cache = webpush.NewSubscriptionCache(repo, hookedRedis(t, hook))

	cache.Invalidate(ctx, "u1")
	require.True(t, hook.fired.Load())
	require.Len(t, between, 1, "前提: Del の前の読み込みは Redis の古い行を読む")
	require.Equal(t, 0, repo.calls)

	got, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, got, 2, "the list read before the Del of Invalidate is kept in memory")
	assert.Equal(t, 1, repo.calls)
}

// The Del of a Get whose Set was overtaken still runs when the caller's
// context ends right after the Set (#3804). 配送の job の ctx が切れても、古い
// 一覧を Redis に 1 時間残さない。
func TestCache_OvertakenSetIsDeletedAfterCancel(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "old"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cache *webpush.SubscriptionCache
	invalidate := &onCmdHook{name: "set", fn: func() {
		repo.data["u1"] = []*model.SwSubscription{{ID: "old"}, {ID: "new"}}
		cache.Invalidate(context.Background(), "u1")
	}}
	cancelAfterSet := &onCmdHook{name: "set", after: true, fn: cancel}
	rdb := redis.NewClient(&redis.Options{Addr: testRedis.Addr})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(invalidate)
	rdb.AddHook(cancelAfterSet)
	cache = webpush.NewSubscriptionCache(repo, rdb)

	_, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	require.True(t, cancelAfterSet.fired.Load())

	n, err := testRedis.Client.Exists(context.Background(), "userSwSubscriptions:u1").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "the overtaken Set is left in Redis when the caller's context ended")
}

// Invalidate deletes the Redis entry even when the caller's context has
// already ended (#3804). 登録の直後にクライアントが切断しても、古い一覧を残さない。
func TestCache_InvalidateDeletesAfterCancel(t *testing.T) {
	testRedis.FlushAll(context.Background())
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "old"}}}}
	cache := webpush.NewSubscriptionCache(repo, testRedis.Client)
	_, err := cache.Get(context.Background(), "u1")
	require.NoError(t, err)
	n, err := testRedis.Client.Exists(context.Background(), "userSwSubscriptions:u1").Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "前提: Redis に積まれている")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache.Invalidate(ctx, "u1")
	n, err = testRedis.Client.Exists(context.Background(), "userSwSubscriptions:u1").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "the entry is left in Redis when the caller's context ended")
}

// The first half of Invalidate advances the generation, so a Get that read
// the DB before the invalidation and stores after its Del does not leave the
// old list in Redis (#3804). Invalidate の Del が Get の setMemory より前に
// 届き、後半の dropEntry は Get の Set より後に動く順。
func TestCache_InvalidateAdvancesGenerationBeforeDel(t *testing.T) {
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	repo := &fakeSwRepo{data: map[string][]*model.SwSubscription{"u1": {{ID: "old"}}}}
	delDone := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	// 途中で落ちても、Invalidate の goroutine を止めたまま残さない。
	t.Cleanup(releaseAll)
	hook := &onCmdHook{name: "del", after: true, fn: func() {
		close(delDone)
		<-release
	}}
	cache := webpush.NewSubscriptionCache(repo, hookedRedis(t, hook))
	invalidated := make(chan struct{})
	repo.onFind = func() {
		repo.onFind = nil
		repo.data["u1"] = []*model.SwSubscription{{ID: "old"}, {ID: "new"}}
		go func() {
			cache.Invalidate(ctx, "u1")
			close(invalidated)
		}()
		// Invalidate の Del が終わってから、Get を続ける。
		select {
		case <-delDone:
		case <-time.After(10 * time.Second):
			t.Error("Invalidate did not send its Del")
		}
	}
	got, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, got, 1, "前提: 割り込まれた Get は無効化の前の一覧を返す")
	releaseAll()
	select {
	case <-invalidated:
	case <-time.After(10 * time.Second):
		t.Fatal("Invalidate did not return")
	}

	n, err := testRedis.Client.Exists(ctx, "userSwSubscriptions:u1").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "the list read before the invalidation is left in Redis")
}
