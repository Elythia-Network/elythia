package middleware

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/elythia-network/elythia/internal/model"
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

	// invalidatedMu は invalidatedAt / allInvalidatedAt を守る。
	invalidatedMu sync.Mutex
	// invalidatedAt は「その利用者の entry を最後に捨てた時刻」(#3664)。
	// resolveUser は DB を引き始めた時刻を控え、その後に捨てられた利用者の
	// 結果は積まない。これが無いと、捨てる直前に DB を引いたリクエストが捨てた後に
	// 古い利用者を積み、TTL の間残る (他のプロセスからの通知は書き込みの直後に
	// 届くので、読み込みと重なりうる)。TTL より古い印は誰の読み込みにも効かない
	// ので、溜まったら掃除する。
	invalidatedAt map[string]time.Time
	// allInvalidatedAt は最後に全体を捨てた時刻 (読めない通知・受け側の溢れ)。
	allInvalidatedAt time.Time
	// afterDrop は entry を消し終えた直後に呼ぶ (テスト用)。消した後・印の前に
	// 積まれる形を再現して、「印を先に付ける」順を検査する。
	afterDrop func()
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

// putIfFresh stores the entry like put unless the user (or the whole cache)
// was invalidated at or after readStart, i.e. while the DB read that produced
// it was in flight.
//
// 判定と格納を同じロックの中で行う。捨てる側は印を付けてから entry を消すので、
// 格納が印より先なら後の消去で消え、後なら判定で積まない (間に挟まる隙が無い)。
func (c *tokenCache) putIfFresh(readStart time.Time, token string, user *model.User, scopes []string, tokenID string, isApp bool) {
	c.invalidatedMu.Lock()
	defer c.invalidatedMu.Unlock()
	if c.invalidatedSinceLocked(user, readStart) {
		return
	}
	c.put(token, user, scopes, tokenID, isApp)
}

// invalidatedSinceLocked reports whether user's entries (or every entry) were
// dropped at or after readStart. Caller holds invalidatedMu.
func (c *tokenCache) invalidatedSinceLocked(user *model.User, readStart time.Time) bool {
	if !c.allInvalidatedAt.IsZero() && !c.allInvalidatedAt.Before(readStart) {
		return true
	}
	if user == nil {
		return false
	}
	t, ok := c.invalidatedAt[user.ID]
	return ok && !t.Before(readStart)
}

// markInvalidated records that userID's entries were dropped now.
func (c *tokenCache) markInvalidated(userID string) {
	now := c.now()
	c.invalidatedMu.Lock()
	defer c.invalidatedMu.Unlock()
	if c.invalidatedAt == nil {
		c.invalidatedAt = make(map[string]time.Time)
	}
	c.invalidatedAt[userID] = now
	if len(c.invalidatedAt) > invalidatedAtSweepAbove {
		cutoff := now.Add(-c.ttl)
		for k, t := range c.invalidatedAt {
			if t.Before(cutoff) {
				delete(c.invalidatedAt, k)
			}
		}
	}
}

// invalidatedAtSweepAbove is the size of invalidatedAt above which marking
// also removes the marks older than the TTL.
const invalidatedAtSweepAbove = 1024

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

// invalidateUserToken removes the entry of token and marks userID as
// invalidated, so a lookup of that user in flight is not stored afterwards.
func (c *tokenCache) invalidateUserToken(userID, token string) {
	if userID != "" {
		c.markInvalidated(userID)
	}
	c.m.Delete(token)
}

// invalidateAll removes every entry and marks the whole cache invalidated.
func (c *tokenCache) invalidateAll() {
	now := c.now()
	c.invalidatedMu.Lock()
	c.allInvalidatedAt = now
	// 個々の印は全体の印が包含する。
	c.invalidatedAt = nil
	c.invalidatedMu.Unlock()
	// 印の後に消す (invalidateByUserID と同じ理由)。
	c.dropEntries(func(*cachedAuthEntry) bool { return true })
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
	// 印を先に付ける。走査の後だと、走査と印の間に積まれた古い entry が残る。
	c.markInvalidated(userID)
	c.dropEntries(func(entry *cachedAuthEntry) bool {
		return entry.user != nil && entry.user.ID == userID
	})
}

// dropEntries deletes every entry for which match returns true (and any
// value that is not an entry), then calls afterDrop when it is set (tests).
func (c *tokenCache) dropEntries(match func(*cachedAuthEntry) bool) {
	c.m.Range(func(k, v any) bool {
		if entry, ok := v.(*cachedAuthEntry); !ok || match(entry) {
			c.m.Delete(k)
		}
		return true
	})
	if c.afterDrop != nil {
		c.afterDrop()
	}
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
