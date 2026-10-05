package cache

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/redis/go-redis/v9"
)

// RedisClients holds multiple Redis connections for different purposes.
type RedisClients struct {
	Default   *redis.Client
	Pubsub    *redis.Client
	JobQueue  *redis.Client
	Timelines *redis.Client
	Reactions *redis.Client
}

// NewRedisClients creates Redis clients from configuration.
func NewRedisClients(cfg *config.Config) (*RedisClients, error) {
	clients := &RedisClients{
		Default:   newClient(cfg.Redis),
		Pubsub:    newClient(cfg.RedisForPubsub),
		JobQueue:  newClient(cfg.RedisForJobQueue),
		Timelines: newClient(cfg.RedisForTimelines),
		Reactions: newClient(cfg.RedisForReactions),
	}

	ctx := context.Background()
	if err := clients.Default.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	if sock := unixSocketPath(cfg.Redis); sock != "" {
		slog.Info("connected to Redis", "socket", sock)
	} else {
		slog.Info("connected to Redis",
			"host", cfg.Redis.Host,
			"port", cfg.Redis.Port,
		)
	}

	return clients, nil
}

func newClient(opts config.RedisOptions) *redis.Client {
	return redis.NewClient(buildRedisOptions(opts))
}

// unixSocketPath は config から UNIX domain socket パスを取り出す。
// `redis.path` (ioredis 流) を最優先し、未指定時は `redis.host` が socket
// パス ("/" 始まり) のときに限ってそれを採用する。それ以外は "" を返す
// (= TCP 接続にフォールバック)。
func unixSocketPath(opts config.RedisOptions) string {
	if opts.Path != "" {
		return opts.Path
	}
	if config.IsUnixSocketPath(opts.Host) {
		return opts.Host
	}
	return ""
}

// buildRedisOptions maps mk-go の config.RedisOptions を go-redis の
// redis.Options に変換する。`redis.path` (ioredis 互換 alias) もしくは
// Host が UNIX domain socket パス ("/" 始まり) のときは Network を "unix"
// に切り替え、Addr にはパスをそのまま入れる。そうでなければ従来どおり
// host:port 形式の TCP 接続にする (#519)。
func buildRedisOptions(opts config.RedisOptions) *redis.Options {
	// go-redisのデフォルトPoolSizeはruntime.GOMAXPROCS * 10。
	// 明示的に指定された場合のみ上書きする（0はgo-redisデフォルトを意味する）。
	poolSize := 0
	if opts.PoolSize != nil {
		poolSize = *opts.PoolSize
	}

	if sock := unixSocketPath(opts); sock != "" {
		return &redis.Options{
			Network:  "unix",
			Addr:     sock,
			Password: opts.Pass,
			DB:       opts.DB,
			Username: opts.Username,
			PoolSize: poolSize,
		}
	}
	return &redis.Options{
		Addr:     fmt.Sprintf("%s:%d", opts.Host, opts.Port),
		Password: opts.Pass,
		DB:       opts.DB,
		Username: opts.Username,
		PoolSize: poolSize,
	}
}

// Close closes all Redis connections.
func (r *RedisClients) Close() error {
	for _, c := range []*redis.Client{
		r.Default, r.Pubsub, r.JobQueue, r.Timelines, r.Reactions,
	} {
		if err := c.Close(); err != nil {
			return err
		}
	}
	return nil
}

// KeyPrefix returns the key prefix for Redis keys.
func KeyPrefix(cfg *config.Config) string {
	return cfg.Redis.Prefix + ":"
}
