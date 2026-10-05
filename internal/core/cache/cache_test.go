package cache

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("failed to setup redis: %v", err)
	}

	code := m.Run()

	testRedis.Teardown(ctx)
	os.Exit(code)
}

func TestNewRedisClients(t *testing.T) {
	cfg := &config.Config{
		Redis:             config.RedisOptions{Host: testRedis.Host(), Port: testRedis.Port()},
		RedisForPubsub:    config.RedisOptions{Host: testRedis.Host(), Port: testRedis.Port()},
		RedisForJobQueue:  config.RedisOptions{Host: testRedis.Host(), Port: testRedis.Port()},
		RedisForTimelines: config.RedisOptions{Host: testRedis.Host(), Port: testRedis.Port()},
		RedisForReactions: config.RedisOptions{Host: testRedis.Host(), Port: testRedis.Port()},
	}

	clients, err := NewRedisClients(cfg)
	require.NoError(t, err)
	assert.NotNil(t, clients.Default)
	assert.NotNil(t, clients.Pubsub)
	assert.NotNil(t, clients.JobQueue)
	assert.NotNil(t, clients.Timelines)
	assert.NotNil(t, clients.Reactions)

	require.NoError(t, clients.Close())
}

func TestNewRedisClients_ConnectionFail(t *testing.T) {
	cfg := &config.Config{
		Redis:             config.RedisOptions{Host: "invalid-host", Port: 1},
		RedisForPubsub:    config.RedisOptions{Host: "invalid-host", Port: 1},
		RedisForJobQueue:  config.RedisOptions{Host: "invalid-host", Port: 1},
		RedisForTimelines: config.RedisOptions{Host: "invalid-host", Port: 1},
		RedisForReactions: config.RedisOptions{Host: "invalid-host", Port: 1},
	}

	_, err := NewRedisClients(cfg)
	assert.Error(t, err)
}

func TestBuildRedisOptions_TCP(t *testing.T) {
	opts := buildRedisOptions(config.RedisOptions{
		Host:     "redis.example.com",
		Port:     6380,
		Pass:     "secret",
		DB:       3,
		Username: "alice",
	})

	assert.Equal(t, "", opts.Network) // デフォルト (tcp) を示す空文字列
	assert.Equal(t, "redis.example.com:6380", opts.Addr)
	assert.Equal(t, "secret", opts.Password)
	assert.Equal(t, 3, opts.DB)
	assert.Equal(t, "alice", opts.Username)
	assert.Equal(t, 0, opts.PoolSize) // 未指定時はgo-redisデフォルト
}

func TestBuildRedisOptions_UnixSocket(t *testing.T) {
	opts := buildRedisOptions(config.RedisOptions{
		Host:     "/var/run/redis/redis.sock",
		Port:     6379, // UDS なので Port は無視されるはず
		Pass:     "secret",
		DB:       1,
		Username: "bob",
	})

	assert.Equal(t, "unix", opts.Network)
	assert.Equal(t, "/var/run/redis/redis.sock", opts.Addr)
	assert.Equal(t, "secret", opts.Password)
	assert.Equal(t, 1, opts.DB)
	assert.Equal(t, "bob", opts.Username)
	assert.Equal(t, 0, opts.PoolSize) // 未指定時はgo-redisデフォルト
}

// `redis.path: /run/...` (ioredis 流) を mk が UNIX domain socket として
// 解釈すること (#519)。同じ config を TS と共有する drop-in 切替で
// 詰まらないようにするための互換 alias。
func TestBuildRedisOptions_PathAliasIsUnixSocket(t *testing.T) {
	opts := buildRedisOptions(config.RedisOptions{
		Path: "/var/run/valkey/valkey.sock",
		// ioredis 互換: host: "127.0.0.1" / port: 6379 が同居していても
		// Path が優先されること (TS の ioredis path 優先と同じ挙動)。
		Host: "127.0.0.1",
		Port: 6379,
		Pass: "secret",
	})
	assert.Equal(t, "unix", opts.Network)
	assert.Equal(t, "/var/run/valkey/valkey.sock", opts.Addr,
		"redis.path must take precedence over redis.host for UDS")
	assert.Equal(t, "secret", opts.Password)
}

func TestBuildRedisOptions_PathEmptyFallsBackToHost(t *testing.T) {
	opts := buildRedisOptions(config.RedisOptions{
		Host: "/var/run/redis/redis.sock", // 旧来 mk の書き方
	})
	assert.Equal(t, "unix", opts.Network)
	assert.Equal(t, "/var/run/redis/redis.sock", opts.Addr)
}

func TestBuildRedisOptions_PoolSize(t *testing.T) {
	poolSize := 50
	opts := buildRedisOptions(config.RedisOptions{
		Host:     "localhost",
		Port:     6379,
		PoolSize: &poolSize,
	})
	assert.Equal(t, 50, opts.PoolSize)
}

func TestBuildRedisOptions_PoolSize_UnixSocket(t *testing.T) {
	poolSize := 100
	opts := buildRedisOptions(config.RedisOptions{
		Host:     "/var/run/redis/redis.sock",
		PoolSize: &poolSize,
	})
	assert.Equal(t, "unix", opts.Network)
	assert.Equal(t, 100, opts.PoolSize)
}

func TestKeyPrefix(t *testing.T) {
	cfg := &config.Config{
		Redis: config.RedisOptions{Prefix: "myhost"},
	}
	assert.Equal(t, "myhost:", KeyPrefix(cfg))
}

func newTestCacheService() *CacheService {
	clients := &RedisClients{Default: testRedis.Client}
	return NewCacheService(clients, "test:")
}

func TestCacheService_SetAndGet(t *testing.T) {
	svc := newTestCacheService()
	ctx := context.Background()
	testRedis.FlushAll(ctx)

	type payload struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	err := svc.Set(ctx, "key1", payload{Name: "test", Value: 42}, time.Minute)
	require.NoError(t, err)

	var got payload
	found, err := svc.Get(ctx, "key1", &got)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "test", got.Name)
	assert.Equal(t, 42, got.Value)
}

func TestCacheService_Get_NotFound(t *testing.T) {
	svc := newTestCacheService()
	ctx := context.Background()
	testRedis.FlushAll(ctx)

	var got string
	found, err := svc.Get(ctx, "nonexistent", &got)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCacheService_Delete(t *testing.T) {
	svc := newTestCacheService()
	ctx := context.Background()
	testRedis.FlushAll(ctx)

	require.NoError(t, svc.Set(ctx, "delme", "value", time.Minute))

	require.NoError(t, svc.Delete(ctx, "delme"))

	var got string
	found, _ := svc.Get(ctx, "delme", &got)
	assert.False(t, found)
}

func TestCacheService_Exists(t *testing.T) {
	svc := newTestCacheService()
	ctx := context.Background()
	testRedis.FlushAll(ctx)

	exists, err := svc.Exists(ctx, "nope")
	require.NoError(t, err)
	assert.False(t, exists)

	require.NoError(t, svc.Set(ctx, "yep", "val", time.Minute))

	exists, err = svc.Exists(ctx, "yep")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestCacheService_Get_UnmarshalError(t *testing.T) {
	svc := newTestCacheService()
	ctx := context.Background()
	testRedis.FlushAll(ctx)

	// 生のバイト列を直接Redisにセットして、JSONとしてパースできないデータを作る
	testRedis.Client.Set(ctx, "test:badkey", "not-valid-json{{{", time.Minute)

	var got map[string]string
	found, err := svc.Get(ctx, "badkey", &got)
	assert.False(t, found)
	assert.Error(t, err)
}

func TestCacheService_Set_MarshalError(t *testing.T) {
	svc := newTestCacheService()
	ctx := context.Background()

	// chanはJSONにmarshalできない
	err := svc.Set(ctx, "badval", make(chan int), time.Minute)
	assert.Error(t, err)
}

func TestCacheService_Get_RedisError(t *testing.T) {
	// 閉じたクライアントでエラーを発生させる
	closedClient := redis.NewClient(&redis.Options{Addr: "localhost:1"})
	closedClient.Close()
	svc := &CacheService{client: closedClient, prefix: "test:"}
	ctx := context.Background()

	var got string
	found, err := svc.Get(ctx, "key", &got)
	assert.False(t, found)
	assert.Error(t, err)
}

func TestCacheService_Exists_RedisError(t *testing.T) {
	closedClient := redis.NewClient(&redis.Options{Addr: "localhost:1"})
	closedClient.Close()
	svc := &CacheService{client: closedClient, prefix: "test:"}
	ctx := context.Background()

	exists, err := svc.Exists(ctx, "key")
	assert.False(t, exists)
	assert.Error(t, err)
}

func TestRedisClients_Close_AllFresh(t *testing.T) {
	addr := testRedis.Client.Options().Addr
	clients := &RedisClients{
		Default:   redis.NewClient(&redis.Options{Addr: addr}),
		Pubsub:    redis.NewClient(&redis.Options{Addr: addr}),
		JobQueue:  redis.NewClient(&redis.Options{Addr: addr}),
		Timelines: redis.NewClient(&redis.Options{Addr: addr}),
		Reactions: redis.NewClient(&redis.Options{Addr: addr}),
	}

	err := clients.Close()
	assert.NoError(t, err)
}

func TestRedisClients_Close_AlreadyClosed(t *testing.T) {
	addr := testRedis.Client.Options().Addr
	alreadyClosed := redis.NewClient(&redis.Options{Addr: addr})
	alreadyClosed.Close()

	clients := &RedisClients{
		Default:   alreadyClosed,
		Pubsub:    redis.NewClient(&redis.Options{Addr: addr}),
		JobQueue:  redis.NewClient(&redis.Options{Addr: addr}),
		Timelines: redis.NewClient(&redis.Options{Addr: addr}),
		Reactions: redis.NewClient(&redis.Options{Addr: addr}),
	}

	err := clients.Close()
	assert.Error(t, err)
}

func TestCacheService_TTLExpiry(t *testing.T) {
	svc := newTestCacheService()
	ctx := context.Background()
	testRedis.FlushAll(ctx)

	require.NoError(t, svc.Set(ctx, "expiring", "val", 100*time.Millisecond))

	var got string
	found, _ := svc.Get(ctx, "expiring", &got)
	assert.True(t, found)

	time.Sleep(150 * time.Millisecond)

	found, _ = svc.Get(ctx, "expiring", &got)
	assert.False(t, found)
}
