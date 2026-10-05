package repository

import (
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// CachedMetaRepository wraps a MetaRepository with a time-based in-memory
// cache. The meta table is a singleton row that rarely changes (admin-only
// updates), so a short TTL eliminates repeated DB round-trips on hot paths
// such as federation host blocking checks.
type CachedMetaRepository struct {
	inner MetaRepository
	ttl   time.Duration

	mu  sync.RWMutex
	val *model.Meta
	at  time.Time

	// onUpdate は Update / EnsureInitial 成功時に呼ばれる post-update hook。
	// cross-worker cache invalidation のため、自プロセスのローカル invalidate に
	// 加えて他 worker へ metaUpdated を publish するのに使う (#1740)。nil 安全。
	// repository 層が event/redis に直接依存しないよう func で受け取る。
	onUpdate func()
}

// NewCachedMetaRepository creates a cached wrapper with a 5-minute TTL.
func NewCachedMetaRepository(inner MetaRepository) MetaRepository {
	return &CachedMetaRepository{inner: inner, ttl: 5 * time.Minute}
}

// NewCachedMetaRepositoryWithTTL creates a cached wrapper with a custom TTL.
func NewCachedMetaRepositoryWithTTL(inner MetaRepository, ttl time.Duration) *CachedMetaRepository {
	return &CachedMetaRepository{inner: inner, ttl: ttl}
}

// SetInvalidationHook registers a callback fired after a successful Update /
// EnsureInitial (in addition to the local cache invalidation). Used to publish
// a cross-worker metaUpdated event so other processes invalidate their caches
// (#1740). nil-safe.
func (c *CachedMetaRepository) SetInvalidationHook(fn func()) {
	c.onUpdate = fn
}

// Invalidate clears the cached meta. Exported so a cross-worker subscriber can
// drop this process's cache on a remote metaUpdated event (#1740).
func (c *CachedMetaRepository) Invalidate() {
	c.invalidate()
}

// Fetch returns the cached meta if still valid, otherwise fetches from DB.
// 返却される *model.Meta はキャッシュ内部と同一ポインタのため、呼び出し側で
// フィールドを変更してはならない（read-only）。
func (c *CachedMetaRepository) Fetch() (*model.Meta, error) {
	c.mu.RLock()
	if c.val != nil && time.Since(c.at) < c.ttl {
		v := c.val
		c.mu.RUnlock()
		return v, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// ダブルチェック: RUnlock〜Lock間に別goroutineがフェッチ済みの場合
	if c.val != nil && time.Since(c.at) < c.ttl {
		return c.val, nil
	}
	m, err := c.inner.Fetch()
	if err != nil {
		return nil, err
	}
	c.val = m
	c.at = time.Now()
	return m, nil
}

// Update delegates to the inner repository and invalidates the cache.
func (c *CachedMetaRepository) Update(fields map[string]any) error {
	err := c.inner.Update(fields)
	if err == nil {
		c.invalidate()
		c.notifyUpdate()
	}
	return err
}

// EnsureInitial delegates to the inner repository and invalidates the cache.
func (c *CachedMetaRepository) EnsureInitial(id string) error {
	err := c.inner.EnsureInitial(id)
	if err == nil {
		c.invalidate()
		c.notifyUpdate()
	}
	return err
}

func (c *CachedMetaRepository) notifyUpdate() {
	if c.onUpdate != nil {
		c.onUpdate()
	}
}

func (c *CachedMetaRepository) invalidate() {
	c.mu.Lock()
	c.val = nil
	c.mu.Unlock()
}
