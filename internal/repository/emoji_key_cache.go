package repository

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// emojiKeyCache is the bounded, TTL-limited (name, host) → rows cache behind
// CachedEmojiRepository.FindManyByKeys (#3383).
//
//   - Positive entries live for ttl, negative ones ("no such emoji") for negTTL.
//   - At most capacity entries are kept; the least recently used is evicted.
//   - Concurrent callers missing the same key wait for a single fetch.
//   - A fetch that started before an invalidation does not store its result
//     (generation check), so a write racing a read cannot leave a stale entry.
type emojiKeyCache struct {
	ttl      time.Duration
	negTTL   time.Duration
	capacity int
	now      func() time.Time

	mu sync.Mutex
	// lru の先頭が最近使われたもの。要素の Value は *emojiKeyEntry。
	lru   *list.List
	items map[model.EmojiKey]*list.Element
	// byID は正のエントリに載っている行の id → キー。id でしか対象を受け取らない
	// 更新・削除 (UpdateFields / Delete) から、落とすべきキーを引くのに使う。
	byID map[string]model.EmojiKey
	// inflight は取得中のキー → その取得。同じキーを同時に取りに来た呼び出しは
	// これを待って結果を共有する。
	inflight map[model.EmojiKey]*emojiKeyFlight
	// gen は invalidation のたびに進む。取得の開始時と完了時で違えば、その間に
	// 書き込みがあったので結果を cache に入れない。
	gen uint64
}

// errEmojiFetchAborted is what waiters receive when the fetch they joined
// panicked instead of returning.
var errEmojiFetchAborted = errors.New("emoji key cache: fetch aborted")

type emojiKeyEntry struct {
	key model.EmojiKey
	// rows が空なら「その (name, host) の絵文字は無い」を表す負のエントリ。
	// local は host IS NULL に一意制約が効かず同名の行が複数ありうるので、
	// inner が返したとおり全部持つ。
	rows    []*model.Emoji
	expires time.Time
}

// emojiKeyFlight is one fetch of the keys a caller claimed. rows and err are
// written before done is closed and only read after it.
type emojiKeyFlight struct {
	done chan struct{}
	rows map[model.EmojiKey][]*model.Emoji
	err  error
	// waiters は待ち合わせに入った呼び出しの数 (c.mu の下で数える)。テストが
	// 「全員が待ちに入った」ことを確かめてから取得を終わらせるのに使う。
	waiters int
}

func newEmojiKeyCache(opts EmojiKeyCacheOptions) *emojiKeyCache {
	return &emojiKeyCache{
		ttl:      opts.TTL,
		negTTL:   opts.NegativeTTL,
		capacity: opts.Capacity,
		now:      opts.Now,
		lru:      list.New(),
		items:    map[model.EmojiKey]*list.Element{},
		byID:     map[string]model.EmojiKey{},
		inflight: map[model.EmojiKey]*emojiKeyFlight{},
	}
}

// get returns the rows for keys, fetching the missing ones with fetch.
func (c *emojiKeyCache) get(keys []model.EmojiKey, fetch func([]model.EmojiKey) ([]*model.Emoji, error)) ([]*model.Emoji, error) {
	var out []*model.Emoji
	var owned []model.EmojiKey
	var waits map[model.EmojiKey]*emojiKeyFlight
	var own *emojiKeyFlight

	c.mu.Lock()
	now := c.now()
	gen := c.gen
	seen := make(map[model.EmojiKey]struct{}, len(keys))
	for _, k := range keys {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		// 列に入らない値は一致しえない。cache にも載せない (任意の文字列で
		// 枠を埋められないようにする)。
		if !storable(k.Name) || !storable(k.Host) {
			continue
		}
		if el, ok := c.items[k]; ok {
			ent := el.Value.(*emojiKeyEntry)
			if now.Before(ent.expires) {
				c.lru.MoveToFront(el)
				out = appendEmojiCopies(out, ent.rows)
				continue
			}
			c.removeLocked(el)
		}
		if f, ok := c.inflight[k]; ok {
			if waits == nil {
				waits = map[model.EmojiKey]*emojiKeyFlight{}
			}
			waits[k] = f
			f.waiters++
			continue
		}
		if own == nil {
			own = &emojiKeyFlight{done: make(chan struct{})}
		}
		c.inflight[k] = own
		owned = append(owned, k)
	}
	c.mu.Unlock()

	var firstErr error
	if own != nil {
		// fetch が panic すると done が閉じられず、待っている呼び出しが永久に
		// 止まる。panic はそのまま伝えるが、待ち合わせだけは解く。
		completed := false
		defer func() {
			if !completed {
				c.abandon(owned, own)
			}
		}()
		rows, err := fetch(owned)
		grouped := make(map[model.EmojiKey][]*model.Emoji, len(owned))
		for _, e := range rows {
			if e == nil {
				continue
			}
			k := model.EmojiKeyOf(e)
			grouped[k] = append(grouped[k], e)
		}
		own.rows = grouped
		own.err = err

		c.mu.Lock()
		// 失敗した取得は「無い」と区別できないので載せない。DB の瞬断を負の
		// エントリとして残すと、復旧後も TTL の間解決できなくなる。返った行は
		// 呼び出し元と待っている呼び出しには渡す。
		store := err == nil && gen == c.gen
		now = c.now()
		for _, k := range owned {
			if c.inflight[k] == own {
				delete(c.inflight, k)
			}
			if store {
				c.putLocked(k, grouped[k], now)
			}
		}
		c.mu.Unlock()
		completed = true
		close(own.done)

		for _, k := range owned {
			out = appendEmojiCopies(out, grouped[k])
		}
		if err != nil {
			firstErr = err
		}
	}

	for k, f := range waits {
		<-f.done
		if f.err != nil && firstErr == nil {
			// 他の呼び出しの取得が失敗した。ここで取り直すと、DB が落ちている
			// ときに待っていた全員が続けて叩きにいくので、同じエラーを返す。
			// 失敗しても返った行 (落ちなかった chunk の分) は使う。
			firstErr = f.err
		}
		out = appendEmojiCopies(out, f.rows[k])
	}
	return out, firstErr
}

// putLocked stores rows for k (an empty rows means negative). c.mu must be held.
func (c *emojiKeyCache) putLocked(k model.EmojiKey, rows []*model.Emoji, now time.Time) {
	if el, ok := c.items[k]; ok {
		c.removeLocked(el)
	}
	ttl := c.ttl
	if len(rows) == 0 {
		ttl = c.negTTL
	}
	ent := &emojiKeyEntry{key: k, rows: rows, expires: now.Add(ttl)}
	c.items[k] = c.lru.PushFront(ent)
	for _, e := range rows {
		c.byID[e.ID] = k
	}
	for c.lru.Len() > c.capacity {
		c.removeLocked(c.lru.Back())
	}
}

// removeLocked drops one entry and its id index. c.mu must be held.
func (c *emojiKeyCache) removeLocked(el *list.Element) {
	ent := el.Value.(*emojiKeyEntry)
	c.lru.Remove(el)
	delete(c.items, ent.key)
	for _, e := range ent.rows {
		if c.byID[e.ID] == ent.key {
			delete(c.byID, e.ID)
		}
	}
}

// invalidate drops the entries named by ev and makes in-flight fetches
// discard their results.
func (c *emojiKeyCache) invalidate(ev EmojiCacheInvalidation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 世代は cache 全体で 1 つなので、無関係なキーの取得中の結果も捨てる。
	// 書き込みと重なった取得が次の呼び出しで引き直されるだけなので、キーごとに
	// 世代を持つ複雑さより単純さを取る。
	c.gen++
	// 取得中のものは結果を cache に入れなくなる (gen が変わる) が、そのまま
	// 待ち合わせに使わせると、書き込みより後に来た呼び出しが書き込み前の値を
	// 受け取る。新しく来た呼び出しは取り直させる。
	clear(c.inflight)
	if ev.All {
		c.lru.Init()
		clear(c.items)
		clear(c.byID)
		return
	}
	for _, k := range ev.Keys {
		if el, ok := c.items[k]; ok {
			c.removeLocked(el)
		}
	}
	for _, id := range ev.IDs {
		k, ok := c.byID[id]
		if !ok {
			// 載っていない行は、どのエントリの内容にも入っていない。負の
			// エントリは id を持たないが、id で指せる行は既に存在するので
			// 「無い」が古くなることはない (作成は Create の Keys で落とす)。
			continue
		}
		if el, ok := c.items[k]; ok {
			c.removeLocked(el)
		}
	}
}

// abandon releases the keys of a fetch that did not complete (the fetch
// panicked), so the callers waiting on it do not block forever.
func (c *emojiKeyCache) abandon(owned []model.EmojiKey, f *emojiKeyFlight) {
	f.rows = nil
	f.err = errEmojiFetchAborted
	c.mu.Lock()
	for _, k := range owned {
		if c.inflight[k] == f {
			delete(c.inflight, k)
		}
	}
	c.mu.Unlock()
	close(f.done)
}

func appendEmojiCopies(out []*model.Emoji, rows []*model.Emoji) []*model.Emoji {
	for _, e := range rows {
		cp := *e
		out = append(out, &cp)
	}
	return out
}
