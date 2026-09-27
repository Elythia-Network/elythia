package middleware

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/shiroha-a/mk/internal/model"
)

// AuthCacheTTL is how long a resolved token stays in the per-process auth
// cache. stream.Manager の 2 回目の閉じ処理 (SetRevokeRecheckDelay) はこれより
// 長くする必要があるので公開している。
//
// token → User を hot path でキャッシュする寿命。
// 短い TTL は logout / token revoke の反映遅延を 30 秒以内に抑えつつ、
// 高頻度の認証リクエストで DB の `FindByToken` 呼び出しをほぼ消す (#512 / #413 #3)。
const AuthCacheTTL = 30 * time.Second

// authCacheSweepEvery はキャッシュ growth が unbounded にならないように
// `put` 1 回ごとに sweep を掛ける確率の分母 (1/N の確率で full sweep)。
// hot path での余分な O(N) 走査を抑える妥協値。
const authCacheSweepEvery uint64 = 64

// cachedAuthEntry holds a resolved user, its OAuth scope view and cache
// expiry time. scopes/isApp carry the access-token permission needed by
// RequireScope (#1552): isApp is true when the token was resolved from an
// app access_token (scoped), false for a native login token (full access);
// scopes is the access_token.permission array (nil for native).
type cachedAuthEntry struct {
	user      *model.User
	scopes    []string
	isApp     bool
	tokenID   string // app access_token の id (registry domain 分離用、#1717)。native は ""。
	expiresAt time.Time
}

// tokenCache is a sync.Map-backed cache for token → resolved user, with
// lazy TTL eviction on read and probabilistic full sweep on write. Negative
// results (token not found) are *not* cached: not-found queries should fail
// fast through DB so token revoke takes effect immediately (#512).
type tokenCache struct {
	m          sync.Map // string → *cachedAuthEntry
	storeCount atomic.Uint64
	ttl        time.Duration
	sweepEvery uint64
	now        func() time.Time // injected clock for test
}

// newTokenCache returns a tokenCache with default TTL / sweep cadence and
// the wall clock as the now function.
func newTokenCache() *tokenCache {
	return &tokenCache{
		ttl:        AuthCacheTTL,
		sweepEvery: authCacheSweepEvery,
		now:        time.Now,
	}
}

// get returns the cached user and its scope view if present and not expired.
// Expired entries are deleted lazily on read.
func (c *tokenCache) get(token string) (user *model.User, scopes []string, tokenID string, isApp, ok bool) {
	v, loaded := c.m.Load(token)
	if !loaded {
		return nil, nil, "", false, false
	}
	entry, isEntry := v.(*cachedAuthEntry)
	if !isEntry || c.now().After(entry.expiresAt) {
		c.m.Delete(token)
		return nil, nil, "", false, false
	}
	return entry.user, entry.scopes, entry.tokenID, entry.isApp, true
}

// put stores token → (user, scope view) with TTL. Triggers a full sweep every
// sweepEvery stores so the cache size stays bounded by "active tokens within
// the TTL window" rather than "every token ever seen".
func (c *tokenCache) put(token string, user *model.User, scopes []string, tokenID string, isApp bool) {
	c.m.Store(token, &cachedAuthEntry{
		user:      user,
		scopes:    scopes,
		isApp:     isApp,
		tokenID:   tokenID,
		expiresAt: c.now().Add(c.ttl),
	})
	if c.sweepEvery > 0 && c.storeCount.Add(1)%c.sweepEvery == 0 {
		c.sweep()
	}
}

// invalidate removes a single entry. Useful when an explicit logout /
// token revoke needs to drop a cached user immediately.
func (c *tokenCache) invalidate(token string) {
	c.m.Delete(token)
}

// invalidateByUserID removes every cached entry whose resolved user has
// the given userID, regardless of which token is keying the entry. Used
// when admin actions (suspend / unsuspend / delete-account) change a
// user's middleware-relevant fields and need to drop the stale cached
// view across all of that user's sessions/tokens at once (#965).
//
// The implementation is a linear sync.Map.Range. Steady-state cache size
// is bounded by active sessions within the TTL window (typically O(1k)),
// so per-call cost is sub-millisecond. Admin invalidation events are
// rare, so this is acceptable without maintaining a reverse userID→tokens
// index.
func (c *tokenCache) invalidateByUserID(userID string) {
	if userID == "" {
		return
	}
	c.m.Range(func(k, v any) bool {
		entry, ok := v.(*cachedAuthEntry)
		if ok && entry.user != nil && entry.user.ID == userID {
			c.m.Delete(k)
		}
		return true
	})
}

// sweep walks the entire cache and removes expired entries. Cheap when the
// cache is small (which is the steady state thanks to the periodic sweep
// cadence on `put`).
func (c *tokenCache) sweep() {
	now := c.now()
	c.m.Range(func(k, v any) bool {
		if entry, ok := v.(*cachedAuthEntry); ok && now.After(entry.expiresAt) {
			c.m.Delete(k)
		}
		return true
	})
}

// len returns the current number of entries (test-only). The returned
// value is a snapshot and may be stale immediately under concurrent access.
func (c *tokenCache) len() int {
	n := 0
	c.m.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
