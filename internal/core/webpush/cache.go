package webpush

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/redis/go-redis/v9"
)

// Cache TTL constants chosen to match the upstream Misskey RedisKVCache
// configuration in PushNotificationService.ts.
const (
	cacheMemoryTTL = 3 * time.Minute
	cacheRedisTTL  = 1 * time.Hour
	cacheKeyPrefix = "userSwSubscriptions:"
)

// SubscriptionCache caches per-user SwSubscription lists with a two-layer
// (in-memory + Redis) strategy. Reads prefer memory, then Redis, finally the
// repository. Writes update both layers. Invalidation removes both layers.
//
// 本家Misskey互換: キー `userSwSubscriptions:{userId}` / memory 3min / Redis 1h。
type SubscriptionCache struct {
	repo  repository.SwSubscriptionRepository
	redis *redis.Client

	mu  sync.Mutex
	mem map[string]memEntry
}

type memEntry struct {
	subs      []*model.SwSubscription
	expiresAt time.Time
}

// NewSubscriptionCache returns a new cache bound to the given repository and
// Redis client. If redis is nil the cache falls back to memory + repository.
func NewSubscriptionCache(repo repository.SwSubscriptionRepository, rdb *redis.Client) *SubscriptionCache {
	return &SubscriptionCache{
		repo:  repo,
		redis: rdb,
		mem:   make(map[string]memEntry),
	}
}

// Get returns the current SwSubscription list for userID, hitting caches first
// and the repository last. The returned slice is safe for callers to iterate
// but must not be mutated — it is shared with the cache.
func (c *SubscriptionCache) Get(ctx context.Context, userID string) ([]*model.SwSubscription, error) {
	now := time.Now()

	c.mu.Lock()
	if e, ok := c.mem[userID]; ok && now.Before(e.expiresAt) {
		c.mu.Unlock()
		return e.subs, nil
	}
	c.mu.Unlock()

	if c.redis != nil {
		if raw, err := c.redis.Get(ctx, cacheKeyPrefix+userID).Bytes(); err == nil {
			var subs []*model.SwSubscription
			if jerr := json.Unmarshal(raw, &subs); jerr == nil {
				c.setMemory(userID, subs, now)
				return subs, nil
			}
		}
	}

	subs, err := c.repo.FindByUserID(userID)
	if err != nil {
		return nil, err
	}
	c.setMemory(userID, subs, now)
	if c.redis != nil {
		if raw, jerr := json.Marshal(subs); jerr == nil {
			_ = c.redis.Set(ctx, cacheKeyPrefix+userID, raw, cacheRedisTTL).Err()
		}
	}
	return subs, nil
}

// Invalidate removes the cached entry for userID in both layers.
// 410 Gone でサブスクリプションを削除した際などに呼ぶ。
func (c *SubscriptionCache) Invalidate(ctx context.Context, userID string) {
	c.mu.Lock()
	delete(c.mem, userID)
	c.mu.Unlock()
	if c.redis != nil {
		_ = c.redis.Del(ctx, cacheKeyPrefix+userID).Err()
	}
}

func (c *SubscriptionCache) setMemory(userID string, subs []*model.SwSubscription, now time.Time) {
	c.mu.Lock()
	c.mem[userID] = memEntry{subs: subs, expiresAt: now.Add(cacheMemoryTTL)}
	c.mu.Unlock()
}
