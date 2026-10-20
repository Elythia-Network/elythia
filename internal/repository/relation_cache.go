package repository

import (
	"fmt"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

// RelationKind names one of the per-user relation lists held by RelationCache.
type RelationKind uint8

const (
	// RelationMuting is keyed by muterId and lists the muteeIds (with expiry).
	RelationMuting RelationKind = iota + 1
	// RelationRenoteMuting is keyed by muterId and lists the renote-muted ids.
	RelationRenoteMuting
	// RelationBlockedBy is keyed by blockeeId and lists the blockerIds
	// (= the users who block the key user).
	RelationBlockedBy
	// RelationFollowing is keyed by followerId and lists the followeeIds.
	RelationFollowing
	// RelationChannelMuting is keyed by userId and lists the muted channel ids
	// (with expiry).
	RelationChannelMuting
	// RelationChannelFollowing is keyed by followerId and lists the followed
	// channel ids.
	RelationChannelFollowing

	relationKindEnd
)

// valid reports whether k is one of the defined kinds.
func (k RelationKind) valid() bool { return k > 0 && k < relationKindEnd }

// RelationInvalidation describes what a write made stale. It is also the wire
// format sent to the other processes.
type RelationInvalidation struct {
	// All drops every entry of every kind.
	All     bool         `json:"all,omitempty"`
	Kind    RelationKind `json:"kind,omitempty"`
	UserIDs []string     `json:"userIds,omitempty"`
}

// defaultRelationCacheTTL is the backstop for an invalidation that never
// arrived (a dropped pub/sub message, a write outside the repositories).
//
// **無効化を省くための値ではない。** 書き込みは repository の中で必ず捨て、
// 他のプロセスへは pub/sub で伝える。それでも Redis の再接続の間に届かなかった
// 通知は戻ってこないので、そのときに古い一覧が残る長さの上限としてだけ置く。
// 本家の CacheService はプロセス内の層を 1 分持ち、他のプロセスの書き込みは
// その 1 分が過ぎるまで見えない。それより悪くならない長さにしてある。
const defaultRelationCacheTTL = time.Minute

// relationCacheMaxEntries caps the number of cached users per kind, and
// relationCacheMaxIDs the number of ids held per kind (a user following
// thousands of accounts weighs that much more).
const (
	relationCacheMaxEntries = 20000
	relationCacheMaxIDs     = 2_000_000
)

// maxForwardedInvalidationIDs caps the ids sent to the other processes in one
// invalidation; a larger one is sent as "drop everything".
const maxForwardedInvalidationIDs = 1000

// maxCachedFollowedChannels mirrors the cap the timeline applied when it paged
// through channel_following itself (5 pages of 100).
const maxCachedFollowedChannels = 500

// RelationCache holds, per user, the relation lists the note-list endpoints
// filter by: who the user mutes / renote-mutes / follows, who blocks the
// user, and which channels the user mutes / follows (#3607).
//
// 本家の CacheService (userMutingsCache / userBlockedCache /
// renoteMutingsCache / userFollowingsCache) と ChannelFollowingService の
// userFollowingChannelsCache に相当する。本家は Redis とプロセス内の 2 層だが、
// ここはプロセス内だけにし、他のプロセスへは無効化を pub/sub で配る。
//
// **無効化は書き込みの関数の側で行う。** muting / renote_muting / blocking /
// following / channel_muting / channel_following の repository は、書き込みに
// 成功するとここを捨てる (NewXxxRepositoryWithRelationCache)。API・連合の受信・
// cron・アカウントの移行のどれから来た書き込みでも、repository を通る限り
// 捨て損ねない。本体のコードで repository を通らずに行が消えるのは、利用者の
// 行の物理削除による ON DELETE CASCADE だけで、HardDeleteUser が全体を捨てる。
// 運用者の手の SQL やプラグインが表へ直接書いた変更は捨てられず、TTL の間
// 反映されない。
//
// **期限付きのミュートは期限の時刻も持ち、読むたびに判定する。** 期限を過ぎた
// 行は cron が消すまで残るが、その前から一覧には出さない (ListMuteeIDs と同じ)。
//
// 読み込みの途中で無効化が来たときは、読んだ結果を保存しない
// (CachedUserRepository の invalidatedAt と同じ考え方、#2257)。
//
// nil の *RelationCache は「キャッシュ無し」として振る舞う (無効化は何もしない)。
type RelationCache struct {
	db         *gorm.DB
	ttl        time.Duration
	maxEntries int
	maxIDs     int
	now        func() time.Time

	mu    sync.Mutex
	kinds [relationKindEnd]relationKindCache
	// allInvalidatedAt は InvalidateAll の最後の時刻。それより前に始まった
	// 読み込みは、どの kind でも保存しない。
	allInvalidatedAt time.Time

	publish func(RelationInvalidation)

	// afterLoad はテスト専用。DB から読み終えて保存する前に呼ばれる。
	afterLoad func()
}

type relationKindCache struct {
	entries       map[string]*relationEntry
	invalidatedAt map[string]time.Time
	ids           int
}

// relationEntry is one cached list. It is never mutated after being stored.
type relationEntry struct {
	ids []string
	// expires は ids と同じ長さで、ゼロ値は無期限。期限の無い kind では nil。
	expires  []time.Time
	loadedAt time.Time
}

// NewRelationCache returns a cache reading from db with the default TTL.
func NewRelationCache(db *gorm.DB) *RelationCache {
	return NewRelationCacheWithTTL(db, defaultRelationCacheTTL)
}

// NewRelationCacheWithTTL returns a cache with a custom TTL (tests).
func NewRelationCacheWithTTL(db *gorm.DB, ttl time.Duration) *RelationCache {
	c := &RelationCache{
		db:         db,
		ttl:        ttl,
		maxEntries: relationCacheMaxEntries,
		maxIDs:     relationCacheMaxIDs,
		now:        time.Now,
	}
	for k := range c.kinds {
		c.kinds[k] = relationKindCache{
			entries:       make(map[string]*relationEntry),
			invalidatedAt: make(map[string]time.Time),
		}
	}
	return c
}

// SetPublisher registers the function that forwards local invalidations to
// the other processes. It is not called for ApplyRemote. nil-safe.
func (c *RelationCache) SetPublisher(fn func(RelationInvalidation)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.publish = fn
	c.mu.Unlock()
}

// MutedUserIDs returns the ids userID mutes with a mute that is active now.
func (c *RelationCache) MutedUserIDs(userID string) ([]string, error) {
	return c.list(RelationMuting, userID)
}

// RenoteMutedUserIDs returns the ids whose renotes userID has muted.
func (c *RelationCache) RenoteMutedUserIDs(userID string) ([]string, error) {
	return c.list(RelationRenoteMuting, userID)
}

// BlockerIDs returns the ids of the users who block userID.
func (c *RelationCache) BlockerIDs(userID string) ([]string, error) {
	return c.list(RelationBlockedBy, userID)
}

// FolloweeIDs returns the ids userID follows.
func (c *RelationCache) FolloweeIDs(userID string) ([]string, error) {
	return c.list(RelationFollowing, userID)
}

// MutedChannelIDs returns the channels userID mutes with a mute that is
// active now.
func (c *RelationCache) MutedChannelIDs(userID string) ([]string, error) {
	return c.list(RelationChannelMuting, userID)
}

// FollowedChannelIDs returns the channels userID follows (at most 500, the
// newest channel ids first, as the timeline read them before).
func (c *RelationCache) FollowedChannelIDs(userID string) ([]string, error) {
	return c.list(RelationChannelFollowing, userID)
}

// list returns a fresh copy of the active ids of one entry. 空なら nil。
func (c *RelationCache) list(kind RelationKind, userID string) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("relation cache: not configured")
	}
	e, err := c.entry(kind, userID)
	if err != nil {
		return nil, err
	}
	if len(e.ids) == 0 {
		return nil, nil
	}
	// 呼び出し側が並べ替えや in-place の絞り込みをしても、キャッシュに響かない
	// よう毎回複製する。
	out := make([]string, 0, len(e.ids))
	if e.expires == nil {
		return append(out, e.ids...), nil
	}
	now := c.now()
	for i, id := range e.ids {
		if exp := e.expires[i]; exp.IsZero() || exp.After(now) {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (c *RelationCache) entry(kind RelationKind, userID string) (*relationEntry, error) {
	if !kind.valid() {
		return nil, fmt.Errorf("relation cache: unknown kind %d", kind)
	}
	if userID == "" || !storable(userID) {
		return &relationEntry{}, nil
	}
	start := c.now()
	c.mu.Lock()
	kc := &c.kinds[kind]
	if e, ok := kc.entries[userID]; ok && start.Sub(e.loadedAt) < c.ttl {
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()

	e, err := c.load(kind, userID, start)
	if err != nil {
		return nil, err
	}
	if c.afterLoad != nil {
		c.afterLoad()
	}
	// 経過時間は読み込みを始めた時刻から数える (保守的な側)。
	e.loadedAt = start

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.staleLocked(kc, userID, start) {
		return e, nil
	}
	// TTL より長くかかった読み込みは保存しない。invalidatedAt は TTL より古い
	// ものから捨てるので、そこまで長い読み込みでは無効化を見落としうる。
	// loadedAt を読み込みの開始にしているので、保存しても次の読み込みでは期限
	// 切れになる (誤りは出ない)。ここで止めるのは、期限切れの項目で上限を
	// 埋めないため。
	if c.now().Sub(start) >= c.ttl {
		return e, nil
	}
	c.storeLocked(kc, userID, e)
	return e, nil
}

// staleLocked reports whether a read started at start may have missed an
// invalidation of userID. Caller holds c.mu.
func (c *RelationCache) staleLocked(kc *relationKindCache, userID string, start time.Time) bool {
	if !c.allInvalidatedAt.IsZero() && !c.allInvalidatedAt.Before(start) {
		return true
	}
	t, ok := kc.invalidatedAt[userID]
	return ok && !t.Before(start)
}

func (c *RelationCache) storeLocked(kc *relationKindCache, userID string, e *relationEntry) {
	if old, ok := kc.entries[userID]; ok {
		kc.ids -= len(old.ids)
		delete(kc.entries, userID)
	}
	if len(kc.entries) >= c.maxEntries || kc.ids+len(e.ids) > c.maxIDs {
		now := c.now()
		for k, old := range kc.entries {
			if now.Sub(old.loadedAt) >= c.ttl {
				kc.ids -= len(old.ids)
				delete(kc.entries, k)
			}
		}
		// 期限切れを落としても溢れるなら、その kind を丸ごと空にする。捨てる
		// 側に倒すぶんには、次の読み込みが DB へ行くだけで誤りは出ない。
		if len(kc.entries) >= c.maxEntries || kc.ids+len(e.ids) > c.maxIDs {
			kc.entries = make(map[string]*relationEntry)
			kc.ids = 0
		}
	}
	if len(e.ids) > c.maxIDs {
		return
	}
	kc.entries[userID] = e
	kc.ids += len(e.ids)
}

// load reads one list from the database, always from the primary.
//
// **レプリカから読まない (dbresolver.Write)。** 無効化は primary でコミットした
// 直後に届くので、その直後の読み直しがまだ複製されていないレプリカへ振られると、
// ミュート・ブロックの前の一覧を TTL の間保存してしまう。以前のように要求ごとに
// 読むなら古いのは複製の遅延の間だけだったが、キャッシュはそれを TTL に引き
// 延ばす。読み直すのは miss と TTL 切れのときだけなので、primary の負荷は小さい。
func (c *RelationCache) load(kind RelationKind, userID string, now time.Time) (*relationEntry, error) {
	switch kind {
	case RelationMuting:
		var rows []struct {
			MuteeID   string     `gorm:"column:muteeId"`
			ExpiresAt *time.Time `gorm:"column:expiresAt"`
		}
		if err := c.db.Clauses(dbresolver.Write).Model(&model.Muting{}).
			Select(`"muteeId"`, `"expiresAt"`).
			Where(`"muterId" = ?`, userID).
			Where(`"expiresAt" IS NULL OR "expiresAt" > ?`, now).
			Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("relation cache: load mutings: %w", err)
		}
		e := &relationEntry{ids: make([]string, 0, len(rows)), expires: make([]time.Time, 0, len(rows))}
		for _, r := range rows {
			e.ids = append(e.ids, r.MuteeID)
			var exp time.Time
			if r.ExpiresAt != nil {
				exp = *r.ExpiresAt
			}
			e.expires = append(e.expires, exp)
		}
		return e, nil
	case RelationChannelMuting:
		var rows []struct {
			ChannelID string     `gorm:"column:channelId"`
			ExpiresAt *time.Time `gorm:"column:expiresAt"`
		}
		if err := c.db.Clauses(dbresolver.Write).Model(&model.ChannelMuting{}).
			Select(`"channelId"`, `"expiresAt"`).
			Where(`"userId" = ?`, userID).
			Where(`"expiresAt" IS NULL OR "expiresAt" > ?`, now).
			Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("relation cache: load channel mutings: %w", err)
		}
		e := &relationEntry{ids: make([]string, 0, len(rows)), expires: make([]time.Time, 0, len(rows))}
		for _, r := range rows {
			e.ids = append(e.ids, r.ChannelID)
			var exp time.Time
			if r.ExpiresAt != nil {
				exp = *r.ExpiresAt
			}
			e.expires = append(e.expires, exp)
		}
		return e, nil
	}

	var (
		ids []string
		q   *gorm.DB
	)
	switch kind {
	case RelationRenoteMuting:
		q = c.db.Clauses(dbresolver.Write).Model(&model.RenoteMuting{}).Where(`"muterId" = ?`, userID).Select(`"muteeId"`)
	case RelationBlockedBy:
		q = c.db.Clauses(dbresolver.Write).Model(&model.Blocking{}).Where(`"blockeeId" = ?`, userID).Select(`"blockerId"`)
	case RelationFollowing:
		q = c.db.Clauses(dbresolver.Write).Model(&model.Following{}).Where(`"followerId" = ?`, userID).Select(`"followeeId"`)
	case RelationChannelFollowing:
		q = c.db.Clauses(dbresolver.Write).Model(&model.ChannelFollowing{}).Where(`"followerId" = ?`, userID).
			Select(`"followeeId"`).Order(`"followeeId" DESC`).Limit(maxCachedFollowedChannels)
	}
	if err := q.Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("relation cache: load kind %d: %w", kind, err)
	}
	return &relationEntry{ids: ids}, nil
}

// Invalidate drops the kind's entries for userIDs in this process and tells
// the other processes to do the same. nil-safe.
func (c *RelationCache) Invalidate(kind RelationKind, userIDs ...string) {
	if c == nil || !kind.valid() {
		return
	}
	ids := dedupeNonEmpty(userIDs)
	if len(ids) == 0 {
		return
	}
	inv := RelationInvalidation{Kind: kind, UserIDs: ids}
	c.apply(inv)
	// 所有者が多い無効化 (フォロワーの多いアカウントの DeleteAllByUser) は、
	// 1 通に数万の ID が載る。他のプロセスへは全体を捨てる通知に畳む。自分の
	// プロセスは上で精密に捨て済み。
	if len(ids) > maxForwardedInvalidationIDs {
		inv = RelationInvalidation{All: true}
	}
	c.forward(inv)
}

// InvalidateAll drops every entry in this process and in the other processes.
// nil-safe.
func (c *RelationCache) InvalidateAll() {
	if c == nil {
		return
	}
	inv := RelationInvalidation{All: true}
	c.apply(inv)
	c.forward(inv)
}

// DropLocal drops every entry in this process without forwarding the
// invalidation, so no other process is told (#3723). It is what the cache
// list of internal/server calls (docs/architecture.md 4.4). nil-safe.
//
// InvalidateAll は他のプロセスにも通知するので、一覧からは呼ばない。
func (c *RelationCache) DropLocal() {
	if c == nil {
		return
	}
	c.apply(RelationInvalidation{All: true})
}

// ApplyRemote applies an invalidation received from another process. It does
// not forward it again. nil-safe.
func (c *RelationCache) ApplyRemote(inv RelationInvalidation) {
	if c == nil {
		return
	}
	// 未知の kind が届いたら (新しい版のプロセスが混ざっている)、全体を捨てる
	// 側に倒す。捨て損ねるより安全。
	if !inv.All && !inv.Kind.valid() {
		inv = RelationInvalidation{All: true}
	}
	c.apply(inv)
}

func (c *RelationCache) forward(inv RelationInvalidation) {
	c.mu.Lock()
	fn := c.publish
	c.mu.Unlock()
	if fn != nil {
		fn(inv)
	}
}

func (c *RelationCache) apply(inv RelationInvalidation) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if inv.All {
		c.allInvalidatedAt = now
		for k := range c.kinds {
			kc := &c.kinds[k]
			kc.entries = make(map[string]*relationEntry)
			kc.ids = 0
		}
		return
	}
	kc := &c.kinds[inv.Kind]
	for _, id := range inv.UserIDs {
		if old, ok := kc.entries[id]; ok {
			kc.ids -= len(old.ids)
			delete(kc.entries, id)
		}
		kc.invalidatedAt[id] = now
	}
	if len(kc.invalidatedAt) > c.maxEntries {
		cutoff := now.Add(-c.ttl)
		for id, t := range kc.invalidatedAt {
			if t.Before(cutoff) {
				delete(kc.invalidatedAt, id)
			}
		}
	}
}

func dedupeNonEmpty(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// deleteByIDReturning deletes the row of value's table whose id is id and
// returns the given column of the deleted row (empty when nothing matched).
//
// 呼び出し側が ID だけを詰めた値で Delete を呼んでも、キャッシュを捨てる相手を
// 取り違えないよう、消した行そのものから所有者を読む。id が空のときは以前と
// 同じく gorm の Delete に任せる (WHERE の無い削除として拒否される)。
func deleteByIDReturning(db *gorm.DB, value any, id, column string) ([]string, error) {
	if id == "" {
		return nil, db.Delete(value).Error
	}
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(value); err != nil {
		return nil, err
	}
	var owners []string
	err := db.Raw(`DELETE FROM "`+stmt.Schema.Table+`" WHERE "id" = ? RETURNING "`+column+`"`, id).
		Scan(&owners).Error
	if err != nil {
		return nil, err
	}
	return owners, nil
}
