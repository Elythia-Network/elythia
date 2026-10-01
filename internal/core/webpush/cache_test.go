package webpush_test

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"

	"github.com/shiroha-a/mk/internal/core/webpush"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
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
}

func (f *fakeSwRepo) FindByUserAndEndpoint(_, _ string) (*model.SwSubscription, error) {
	return nil, errors.New("not used")
}
func (f *fakeSwRepo) FindByUserEndpointAuthKey(_, _, _, _ string) (*model.SwSubscription, error) {
	return nil, errors.New("not used")
}
func (f *fakeSwRepo) FindByUserID(userID string) ([]*model.SwSubscription, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.data[userID], nil
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
