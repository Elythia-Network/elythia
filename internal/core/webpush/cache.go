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
	// cacheDelTimeout bounds the Del of Invalidate and the Del that Get sends
	// after it finds its Set overtaken by an invalidation.
	cacheDelTimeout = 5 * time.Second
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
	// gen は DropLocal (#3736) と Invalidate (#3804) で進める世代の番号。読み込みは
	// mu の外で Redis と DB を読むので、捨てる前に読み始めた行を、捨てた後に積まない
	// ために使う。Invalidate も進めるので、無効化と重なった他の利用者の読み込みも
	// 積まなくなるが、次の読み込みで取り直すだけで済む。
	gen uint64
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
	gen := c.gen
	c.mu.Unlock()

	if c.redis != nil {
		if raw, err := c.redis.Get(ctx, cacheKeyPrefix+userID).Bytes(); err == nil {
			var subs []*model.SwSubscription
			if jerr := json.Unmarshal(raw, &subs); jerr == nil {
				c.setMemory(gen, userID, subs, now)
				return subs, nil
			}
		}
	}

	subs, err := c.repo.FindByUserID(userID)
	if err != nil {
		return nil, err
	}
	// 捨てた後は、捨てる前に読んだ行を Redis にも書かない。Redis の層は
	// プロセスの間で共有していて、DB を戻したときは CleanRedisAfterRestore が消す。
	// 消した後に古い行を書き戻すと、他のプロセスも 1 時間それを読む。
	if !c.setMemory(gen, userID, subs, now) {
		return subs, nil
	}
	if c.redis != nil {
		if raw, jerr := json.Marshal(subs); jerr == nil {
			_ = c.redis.Set(ctx, cacheKeyPrefix+userID, raw, cacheRedisTTL).Err()
			// Redis への書き込みは mu の外なので、メモリに積んだ後、書く前に
			// Invalidate が入ると、その Del の後に古い行を書いてしまう (#3804)。
			// 書いた後で世代を見直し、進んでいたら自分で消す。Invalidate は世代を
			// 進めてから Del するので、ここで世代が変わっていなければ、その Del は
			// この Set より後に届き、どちらの順でも古い行は残らない。消すのが新しい
			// 読み込みの書いた行でも、次の読み込みが取り直すだけで済む。
			// 他のプロセスの読み込みとの競合は、この段では扱わない。
			if !c.sameGeneration(gen) {
				// 呼び出し元の ctx が切れていても消す。Set は済んでいるので、
				// 消せないと古い行が Redis に 1 時間残る。
				delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheDelTimeout)
				_ = c.redis.Del(delCtx, cacheKeyPrefix+userID).Err()
				cancel()
				// Set から Del までの間に、別の読み込みがこの古い行を Redis から
				// 読んでメモリに積んでいることがある。Del の後でもう一度捨てる。
				c.dropEntry(userID)
			}
		}
	}
	return subs, nil
}

// Invalidate removes the cached entry for userID in both layers.
// 410 Gone でサブスクリプションを削除した際などに呼ぶ。
//
// It also advances the generation, so a Get that started reading before the
// invalidation does not store what it read (#3804).
func (c *SubscriptionCache) Invalidate(ctx context.Context, userID string) {
	c.dropEntry(userID)
	if c.redis != nil {
		// 呼び出し元の ctx が切れていても消す。購読の登録の直後にクライアントが
		// 切断すると、新しい購読を含まない一覧が Redis に 1 時間残るため。
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheDelTimeout)
		_ = c.redis.Del(delCtx, cacheKeyPrefix+userID).Err()
		cancel()
		// 世代を進めてから Del が届くまでの間に、別の読み込みが Redis の古い行を
		// 読んで新しい世代で積んでいることがある。Del の後でもう一度捨てる。
		// 前半の dropEntry も要る。Get は Set の後で世代を見直すので、世代を進める
		// のが Del の後だけだと、見直しが Del より前に通り、古い行が Redis に残る。
		c.dropEntry(userID)
	}
}

// dropEntry drops userID from memory and advances the generation.
func (c *SubscriptionCache) dropEntry(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	delete(c.mem, userID)
}

// DropLocal drops the in-memory layer held by this process (#3736). Redis の
// 層はプロセスの間で共有しているので触らない。他のプロセスへも伝えない。
func (c *SubscriptionCache) DropLocal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	clear(c.mem)
}

// sameGeneration reports whether the cache has not been dropped or
// invalidated since gen was read.
func (c *SubscriptionCache) sameGeneration(gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen == gen
}

// setMemory stores subs unless the cache was dropped or invalidated after gen
// was read, and reports whether it stored them.
func (c *SubscriptionCache) setMemory(gen uint64, userID string, subs []*model.SwSubscription, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return false
	}
	c.mem[userID] = memEntry{subs: subs, expiresAt: now.Add(cacheMemoryTTL)}
	return true
}
