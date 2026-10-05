package repository

import (
	"sync"
	"time"

	"github.com/shiroha-a/mk/internal/model"
)

// CachedEmojiRepository wraps an EmojiRepository with two process-local
// caches:
//
//   - ListLocal: the whole local emoji list, with a short TTL. The /api/emojis
//     endpoint is hit by every timeline render (frontend caches client-side,
//     but cold loads still flood it), and ListLocal performs a full table scan
//     with ORDER BY each time. Local emoji set rarely changes (admin-only
//     writes), so a short TTL plus invalidation on mutation gives near-perfect
//     cache hit rate while keeping staleness bounded (#300 3-6).
//   - FindManyByKeys: a bounded (name, host) → rows cache used to resolve emoji
//     URLs while packing notes / users, so the next timeline page does not
//     fetch the same emojis again (#3383). See emojiKeyCache.
//
// Mutations through this wrapper invalidate both caches. Writes made by other
// processes reach this one only through ApplyRemoteInvalidation (wired to a
// Redis pub/sub channel by the server); the TTLs bound the staleness of writes
// that bypass both (e.g. the one-shot backfill command writing SQL directly, or
// a pub/sub message lost while Redis was reconnecting).
type CachedEmojiRepository struct {
	inner EmojiRepository
	ttl   time.Duration

	mu    sync.RWMutex
	local []*model.Emoji
	at    time.Time

	keys *emojiKeyCache

	// onInvalidate は書き込みで自プロセスの cache を落とした後に呼ぶ hook。
	// 他プロセスへ同じ invalidation を publish するのに使う。配線時に 1 度だけ
	// 設定し、以後は書き換えない (nil 安全)。
	onInvalidate func(EmojiCacheInvalidation)
}

// **interface 充足をこのファイルで固定する。** 新しい書き込みメソッドを
// interface に足したとき、ここで wrapper の実装漏れ (= invalidate 漏れ) に
// 気付けるようにする。
var _ EmojiRepository = (*CachedEmojiRepository)(nil)

const (
	// defaultEmojiKeyCacheTTL bounds how long a resolved emoji stays cached
	// without any invalidation reaching this process.
	//
	// 本家はリモート絵文字をプロセス内に 12 時間持つが、mk-go は書き込みと
	// pub/sub で落とすので TTL が効くのは「経路に乗らなかった書き込み」だけ。
	// その場合に古い URL (消えたファイルを指しうる) を出し続ける時間を短く
	// 抑えるため 10 分にする。次ページで同じ絵文字を引き直さないという目的には
	// 10 分で十分足りる。
	defaultEmojiKeyCacheTTL = 10 * time.Minute
	// defaultEmojiKeyNegativeTTL bounds how long "no such emoji" is cached.
	//
	// リモート絵文字は note の受信時に後から作られる。作成は invalidation で
	// 落ちるが、pub/sub を取りこぼすと「無い」が残り続けるので、正の値より
	// 短くする。0 にしないのは、解決できない名前 (reaction に残った削除済みの
	// 絵文字など) がページごとに DB へ届くのを防ぐため。
	defaultEmojiKeyNegativeTTL = time.Minute
	// defaultEmojiKeyCacheCap caps the number of cached (name, host) entries,
	// positive and negative together. Least recently used entries are evicted.
	//
	// 1 件は model.Emoji 1 行とキー程度 (数百バイト) なので、1 万件で数 MB に
	// 収まる。タイムラインを遡って現れる絵文字の作業集合はこれより十分小さい。
	defaultEmojiKeyCacheCap = 10000
)

// EmojiCacheInvalidation describes the emoji cache entries a write may have
// made stale. It is what CachedEmojiRepository hands to the invalidation hook
// and what ApplyRemoteInvalidation receives from other processes, so it is
// JSON-encoded on the wire.
type EmojiCacheInvalidation struct {
	// Keys are (name, host) pairs whose cached entry (positive or negative)
	// must be dropped. Set by Create.
	Keys []model.EmojiKey `json:"keys,omitempty"`
	// IDs are emoji row ids whose cached entries must be dropped. Set by the
	// id-based updates / deletes.
	IDs []string `json:"ids,omitempty"`
	// All drops every cached entry. Set when a row may have moved to another
	// (name, host), which can make an unrelated negative entry stale.
	All bool `json:"all,omitempty"`
}

// mayAffectLocal reports whether ev can concern a local emoji, i.e. whether
// the ListLocal cache must be dropped too.
func (ev EmojiCacheInvalidation) mayAffectLocal() bool {
	if ev.All || len(ev.IDs) > 0 {
		// id だけでは host が分からないので、local の可能性を残す側に倒す。
		return true
	}
	for _, k := range ev.Keys {
		if k.Host == "" {
			return true
		}
	}
	return false
}

// NewCachedEmojiRepository wraps inner with a 5-minute ListLocal TTL and the
// default FindManyByKeys cache (10-minute TTL, 1-minute negative TTL, 10000
// entries). Mutations through this wrapper invalidate the caches immediately,
// so the TTLs only matter for out-of-band writes.
func NewCachedEmojiRepository(inner EmojiRepository) *CachedEmojiRepository {
	return NewCachedEmojiRepositoryWithTTL(inner, 5*time.Minute)
}

// NewCachedEmojiRepositoryWithTTL is the test-friendly constructor with an
// explicit ListLocal TTL. The FindManyByKeys cache uses the defaults.
func NewCachedEmojiRepositoryWithTTL(inner EmojiRepository, ttl time.Duration) *CachedEmojiRepository {
	return NewCachedEmojiRepositoryWithOptions(inner, ttl, EmojiKeyCacheOptions{})
}

// EmojiKeyCacheOptions configures the FindManyByKeys cache. Zero fields take
// the defaults.
type EmojiKeyCacheOptions struct {
	TTL         time.Duration
	NegativeTTL time.Duration
	Capacity    int
	// Now overrides the clock (tests).
	Now func() time.Time
}

// NewCachedEmojiRepositoryWithOptions is the fully configurable constructor.
func NewCachedEmojiRepositoryWithOptions(inner EmojiRepository, ttl time.Duration, opts EmojiKeyCacheOptions) *CachedEmojiRepository {
	if opts.TTL <= 0 {
		opts.TTL = defaultEmojiKeyCacheTTL
	}
	if opts.NegativeTTL <= 0 {
		opts.NegativeTTL = defaultEmojiKeyNegativeTTL
	}
	if opts.Capacity <= 0 {
		opts.Capacity = defaultEmojiKeyCacheCap
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &CachedEmojiRepository{
		inner: inner,
		ttl:   ttl,
		keys:  newEmojiKeyCache(opts),
	}
}

// SetInvalidationHook registers a callback fired after every write through
// this wrapper, once this process's caches have been dropped. The server uses
// it to publish the invalidation to the other processes. Call it during wiring
// only, before the repository is used concurrently. nil-safe.
func (c *CachedEmojiRepository) SetInvalidationHook(fn func(EmojiCacheInvalidation)) {
	c.onInvalidate = fn
}

// ApplyRemoteInvalidation drops this process's cache entries named by ev,
// which another process published after writing. It never fires the
// invalidation hook, so processes do not echo each other's messages.
func (c *CachedEmojiRepository) ApplyRemoteInvalidation(ev EmojiCacheInvalidation) {
	c.keys.invalidate(ev)
	// 他プロセスのリモート絵文字の作成 (連合の受信で頻繁に起きる) で ListLocal
	// まで落とすと、/api/emojis のフルスキャンが受信のたびに走る。local に
	// 関わりうるものだけ落とす。
	if ev.mayAffectLocal() {
		c.Invalidate()
	}
}

// ListLocal returns the cached local emoji slice if still valid, otherwise
// fetches from the inner repo. The returned slice is the cache-internal
// pointer; callers must treat it as read-only (never mutate elements or
// the slice itself).
func (c *CachedEmojiRepository) ListLocal() ([]*model.Emoji, error) {
	c.mu.RLock()
	// "キャッシュ済みかどうか" は c.at が non-zero かどうかで判定する。
	// inner.ListLocal() は emoji table が空のとき GORM 由来で (nil, nil)
	// を返す可能性があり、`c.local != nil` で判定すると空テーブルの
	// instance で cache が永久に効かなくなる (Devin #541 BUG-1)。
	if !c.at.IsZero() && time.Since(c.at) < c.ttl {
		v := c.local
		c.mu.RUnlock()
		return v, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// double-check: RUnlock〜Lock 間に別 goroutine がフェッチ済みの場合
	if !c.at.IsZero() && time.Since(c.at) < c.ttl {
		return c.local, nil
	}
	list, err := c.inner.ListLocal()
	if err != nil {
		return nil, err
	}
	c.local = list
	c.at = time.Now()
	return list, nil
}

// Invalidate drops the cached ListLocal result. Public so out-of-band paths
// (e.g. emoji import) can force a refresh without going through the wrapper.
func (c *CachedEmojiRepository) Invalidate() {
	c.mu.Lock()
	c.local = nil
	c.at = time.Time{}
	c.mu.Unlock()
}

// afterWrite drops the caches a write may have made stale and tells the
// other processes. ok reports whether the inner write returned success.
//
// **(name, host) の cache は失敗しても落とす。** COMMIT の後・応答の前に
// 接続が切れると「書けたのにエラー」になる (emojiimport が実際に扱っている
// ケース)。そこで残すと、消えた絵文字や古い URL を TTL の間出し続ける。
// 落としすぎても次の読み込みで引き直すだけなので、こちらに倒す。
// ListLocal は従来どおり成功時だけ落とす (TTL が 5 分と短い)。
func (c *CachedEmojiRepository) afterWrite(ok bool, ev EmojiCacheInvalidation) {
	c.keys.invalidate(ev)
	if ok {
		c.Invalidate()
	}
	if c.onInvalidate != nil {
		c.onInvalidate(ev)
	}
}

// fieldsInvalidation builds the invalidation for an id-based update.
func fieldsInvalidation(ids []string, fields map[string]any) EmojiCacheInvalidation {
	_, renamed := fields["name"]
	_, moved := fields["host"]
	if renamed || moved {
		// 行が別の (name, host) へ移ると、移り先に置かれた「無い」の
		// 負キャッシュも古くなる。移り先は fields だけでは決まらない (host は
		// 行の値) ので全体を落とす。改名は管理画面からしか起きず頻度は低い。
		return EmojiCacheInvalidation{All: true}
	}
	return EmojiCacheInvalidation{IDs: ids}
}

// --- mutating methods: delegate then invalidate ----------------------------

func (c *CachedEmojiRepository) Create(e *model.Emoji) error {
	err := c.inner.Create(e)
	c.afterWrite(err == nil, EmojiCacheInvalidation{Keys: []model.EmojiKey{model.EmojiKeyOf(e)}})
	return err
}

func (c *CachedEmojiRepository) UpdateFields(id string, fields map[string]any) error {
	err := c.inner.UpdateFields(id, fields)
	c.afterWrite(err == nil, fieldsInvalidation([]string{id}, fields))
	return err
}

func (c *CachedEmojiRepository) UpdateFieldsMany(ids []string, fields map[string]any) error {
	err := c.inner.UpdateFieldsMany(ids, fields)
	c.afterWrite(err == nil, fieldsInvalidation(ids, fields))
	return err
}

func (c *CachedEmojiRepository) Delete(id string) error {
	err := c.inner.Delete(id)
	c.afterWrite(err == nil, EmojiCacheInvalidation{IDs: []string{id}})
	return err
}

func (c *CachedEmojiRepository) DeleteMany(ids []string) error {
	err := c.inner.DeleteMany(ids)
	c.afterWrite(err == nil, EmojiCacheInvalidation{IDs: ids})
	return err
}

// FindManyByKeys resolves (name, host) pairs through the bounded TTL cache.
// Only the keys missing from the cache are fetched, in one batched query to
// the inner repository; concurrent callers missing the same key share one
// fetch. Rows are returned as shallow copies, so callers may not mutate the
// slices they contain (Aliases etc.) but may mutate the structs.
func (c *CachedEmojiRepository) FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error) {
	return c.keys.get(keys, c.inner.FindManyByKeys)
}

// --- read-only methods: direct delegate ------------------------------------

func (c *CachedEmojiRepository) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	if !storable(name) || (host != nil && !storable(*host)) {
		return nil, ErrNotFound
	}
	return c.inner.FindByNameAndHost(name, host)
}

func (c *CachedEmojiRepository) FindByID(id string) (*model.Emoji, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	return c.inner.FindByID(id)
}

func (c *CachedEmojiRepository) FindManyByIDs(ids []string) ([]*model.Emoji, error) {
	ids = storableIDs(ids)
	return c.inner.FindManyByIDs(ids)
}

func (c *CachedEmojiRepository) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	return c.inner.FindManyByNamesAndHost(names, host)
}

func (c *CachedEmojiRepository) ListWithFilter(query, category string, local bool, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error) {
	return c.inner.ListWithFilter(query, category, local, sinceID, untilID, limit, offset)
}

func (c *CachedEmojiRepository) ListRemoteWithFilter(query, host, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error) {
	return c.inner.ListRemoteWithFilter(query, host, sinceID, untilID, limit, offset)
}

func (c *CachedEmojiRepository) ListV2(filter model.EmojiV2Filter) ([]*model.Emoji, error) {
	return c.inner.ListV2(filter)
}

func (c *CachedEmojiRepository) CountV2(filter model.EmojiV2Filter) (int64, error) {
	return c.inner.CountV2(filter)
}
