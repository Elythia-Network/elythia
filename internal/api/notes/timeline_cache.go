package notes

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// ----------------------------------------------------------------------------
// timeline JSON cache (flag-gated via enableTimelineCache / MK_ENABLETIMELINECACHE).
//
// timeline read hot path の driver 層最適化 (Joins / raw scan / pgx native) は
// 全滅した。本 cache は driver ではなく上位で、first-page (cursor 無し) の最終
// JSON レスポンスを per-viewer + per-param で短 TTL (default 3s、config 可変)
// キャッシュし、hit 時に DB hydration + entity packing + JSON encode を丸ごと
// スキップする。
//
// 設計上の割り切り:
//   - first-page のみ (cursor 付き無限スクロールは hit 率低く対象外、メモリ有界化)。
//   - per-viewer key (packMany は myReaction 等 viewer 固有 field を詰めるため)。
//   - TTL 3s の staleness は許容する。frontend は新着を WebSocket stream で前置き
//     するので REST の数秒遅延はほぼマスクされる。
// ----------------------------------------------------------------------------

type timelineCacheEntry struct {
	body      []byte
	expiresAt time.Time
}

// TimelineJSONCache is a bounded in-memory TTL cache of marshaled timeline
// responses. 単一 process 内のみ (multi-process では process ごとに別 cache だが
// 応答の正しさは保たれる)。
type TimelineJSONCache struct {
	mu         sync.Mutex
	m          map[string]timelineCacheEntry
	ttl        time.Duration
	maxEntries int
	// gen は DropLocal で進める世代の番号 (#3736)。応答はロックの外で組み立てる
	// ので、捨てる前に組み立て始めた応答を、捨てた後に積まないために使う。
	gen uint64
}

func newTimelineJSONCache(ttl time.Duration) *TimelineJSONCache {
	return &TimelineJSONCache{
		m:          make(map[string]timelineCacheEntry),
		ttl:        ttl,
		maxEntries: 50000,
	}
}

// get returns the cached body if present and unexpired. 期限切れ entry は lazy に
// 削除する。
func (c *TimelineJSONCache) get(key string, now time.Time) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if now.After(e.expiresAt) {
		delete(c.m, key)
		return nil, false
	}
	return e.body, true
}

// generation returns the current drop generation. 組み立てを始める前に読み、
// set に渡す。
func (c *TimelineJSONCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// DropLocal drops every cached response held by this process (#3736). Other
// processes are not told.
func (c *TimelineJSONCache) DropLocal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	clear(c.m)
}

// set stores body under key with the configured TTL, unless the cache was
// dropped after gen was read. 上限到達時はまず期限切れを
// 掃き、それでも上限なら今回は cache しない (= bounded、メモリ無制限増加を防ぐ)。
func (c *TimelineJSONCache) set(gen uint64, key string, body []byte, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	if len(c.m) >= c.maxEntries {
		for k, e := range c.m {
			if now.After(e.expiresAt) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.maxEntries {
			return
		}
	}
	c.m[key] = timelineCacheEntry{body: body, expiresAt: now.Add(c.ttl)}
}

// timelineCacheKey builds a cache key from everything that affects the output
// for a first-page request. cursor (sinceId/untilId) は first-page では空なので
// 含めない。出力に影響する param を取り落とすと別 viewer/別条件の応答を誤配する
// ため、TimelineRequest の出力影響 field を全て織り込む。
func timelineCacheKey(kind, viewerID string, req TimelineRequest) string {
	return strings.Join([]string{
		kind,
		viewerID,
		strconv.Itoa(int(*req.Limit)),
		boolKey(req.WithFiles),
		ptrBoolKey(req.WithRenotes),
		ptrBoolKey(req.WithReplies),
		ptrBoolKey(req.IncludeMyRenotes),
		ptrBoolKey(req.IncludeRenotedMyNotes),
		ptrBoolKey(req.IncludeLocalRenotes),
		boolKey(req.AllowPartial),
	}, "|")
}

func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ptrBoolKey distinguishes nil / true / false (nil は upstream default 解釈で
// 出力が変わるため別 key にする)。
func ptrBoolKey(b *bool) string {
	if b == nil {
		return "n"
	}
	if *b {
		return "1"
	}
	return "0"
}
