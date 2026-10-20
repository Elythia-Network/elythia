package meta

import (
	"sync"
	"time"
)

// metaResponseCacheTTL bounds how long a serialized /api/meta body is reused.
//
// **短さ自体が安全側の根拠になっている。** 長い TTL + 完璧な invalidation では
// なく数秒にすることで、invalidation の取りこぼしがあっても被害がその秒数で
// 収まる。高トラフィックでは数秒でもほぼ全てのリクエストが cache hit になるので、
// 取り分はほとんど落ちない。伸ばすなら invalidation の網羅性を先に上げること
// (TestMetaResponseCacheTTL_StaysShort がこの前提を固定している)。
//
// **invalidate がその worker に届く限り、meta 更新がこの cache に足す stale は
// 無い。** putIfCurrent と invalidate は同じ mutex で直列化されるので、put が
// 先なら invalidate が消し、put が後なら世代 (gen) が合わずに捨てられる。
// 第三の順序が無い。
//
// TTL 分だけ残るのは次の 4 つ:
//
//   - 他 worker が処理した ads の変更 (meta と違って cross-worker 経路が無い)
//   - ads の startsAt / expiresAt の境界またぎ
//   - proxyAccountName の変更 (system_account の insert は meta 更新を伴わない)
//   - invalidate がこの worker に届かなかった meta 更新。CachedMetaRepository を
//     経由しない書き込みのほか、metaUpdated の publish 失敗 (router は warn を
//     出すだけ) や subscriber の切断中に流れた分も含む。Redis pub/sub は再送
//     しないので、その worker では meta 行 cache の 5 分 TTL に加えてこの
//     cache の TTL 分だけ古くなる
const metaResponseCacheTTL = 5 * time.Second

// metaPrettyIndent mirrors echo's unexported `defaultIndent` (context.go), the
// indent `c.JSON` uses for `?pretty` and Debug.
const metaPrettyIndent = "  "

// metaResponseCache holds the serialized /api/meta body for each `detail`
// variant. The response depends only on the (already cached) meta row, static
// config, the active ads, the proxy account and the chunked-upload capability,
// so a single process-wide entry per variant is correct — there is no per-user
// or per-request variation.
type metaResponseCache struct {
	mu sync.RWMutex
	// entries[0] は detail=false (MetaLite)、entries[1] は detail=true。
	entries [2]metaResponseEntry
	// gen は invalidate のたびに進む世代番号。ハンドラは組み立てを始める前の
	// 世代を控え、書き込むときに一致していなければ捨てる。これが無いと
	//
	//   R: cache miss → buildMeta (更新前の meta を読む)
	//   W:                        admin が meta 更新 → invalidate
	//   R:                                                put(更新前の body)
	//
	// の順で **更新前のレスポンスが TTL 分キャッシュに居座る**。
	// CachedUserRepository の invalidatedAt (#2257) と同じ形の対策。
	gen uint64
	// now / ttl はテスト用の seam。**単一 goroutine から、ハンドラを呼ぶ前に
	// だけ設定すること** (lock を取らずに書く)。0 / nil なら本番の値を使う。
	now func() time.Time
	ttl time.Duration
}

// metaResponseEntry is one cached variant.
type metaResponseEntry struct {
	body []byte
	at   time.Time
}

func metaVariantIndex(detail bool) int {
	if detail {
		return 1
	}
	return 0
}

func (c *metaResponseCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *metaResponseCache) lifetime() time.Duration {
	if c.ttl > 0 {
		return c.ttl
	}
	return metaResponseCacheTTL
}

// get returns the cached body for the variant along with the generation the
// caller must pass back to putIfCurrent. body is nil when absent or expired.
// The returned slice is the cache-internal one; callers must treat it as
// read-only (it is only ever handed to the response writer, which does not
// modify its input).
func (c *metaResponseCache) get(detail bool) (body []byte, gen uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e := c.entries[metaVariantIndex(detail)]
	if e.body == nil || c.clock().Sub(e.at) >= c.lifetime() {
		return nil, c.gen
	}
	return e.body, c.gen
}

// putIfCurrent stores the serialized body unless the cache was invalidated
// since gen was read, in which case the body was built from state that is
// already known to be stale and is dropped.
func (c *metaResponseCache) putIfCurrent(detail bool, body []byte, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	c.entries[metaVariantIndex(detail)] = metaResponseEntry{body: body, at: c.clock()}
}

// invalidate drops both variants and advances the generation so an in-flight
// build cannot refill the cache with pre-update state.
func (c *metaResponseCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = [2]metaResponseEntry{}
	c.gen++
}

// InvalidateResponseCache drops the cached /api/meta bodies. Wired by the
// router to the same signals that invalidate the meta row cache (the local
// update hook and the cross-worker `metaUpdated` event, #1740) and to ad
// mutations, so an admin change shows up immediately instead of after the TTL.
func (h *Handler) InvalidateResponseCache() {
	h.respCache.invalidate()
}

// DropLocal drops the cached /api/meta bodies of this process (#3721). It is
// what the cache list of internal/server calls (docs/architecture.md 4.4).
// No other process is told.
func (h *Handler) DropLocal() {
	h.respCache.invalidate()
}
