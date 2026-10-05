package mkqdriver

import (
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/config"
)

// BuildRedisOptions converts mk-go's config.RedisOptions into the
// redis.UniversalOptions value mkq.Config consumes. UNIX socket paths
// (any host string starting with "/") are passed through unchanged —
// go-redis auto-detects the network type from the leading slash.
//
// PoolSize is propagated when the operator set redisForJobQueue.poolSize
// in YAML; nil keeps go-redis's default (10*GOMAXPROCS) which mkq
// references when warning about under-sized pools (pool >= concurrency
// + 8 is recommended for the BZPopMin-based dispatch path).
func BuildRedisOptions(opts config.RedisOptions) redis.UniversalOptions {
	addr := opts.Host
	if !config.IsUnixSocketPath(opts.Host) {
		addr = fmt.Sprintf("%s:%d", opts.Host, opts.Port)
	}
	uo := redis.UniversalOptions{
		Addrs:    []string{addr},
		Password: opts.Pass,
		Username: opts.Username,
		DB:       opts.DB,
	}
	if opts.PoolSize != nil && *opts.PoolSize > 0 {
		uo.PoolSize = *opts.PoolSize
	}
	return uo
}
