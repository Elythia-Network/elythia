package repository

import (
	"errors"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// CachedUserRepository wraps a UserRepository with a time-based per-userID
// cache for FindByID + FindProfileByUserID. ShowByID 経路はタイムライン描画
// で大量に呼ばれるが、user / user_profile 行の更新頻度は低いので、5 分 TTL
// + 全 mutation で per-userID invalidate するだけで hit 率がほぼ 100% に
// 張り付く (#300 3-3)。
//
// In-memory wrapper を選択した理由:
//   - role / emoji / meta cache (#300 3-5 / 3-6 / 3-1) と同じ既存パターンに
//     揃えるため。Redis 越しの user cache はインスタンス間共有という別の
//     利点があるが、out-of-band write が無い前提では in-memory で十分。
//   - 全 user 系 mutation が `userRepo` 経由 (admin, federation processor,
//     following service, signin, resetpassword 等) なので wrapper 層で
//     invalidate を保証できる。DB 直接 UPDATE の経路は無い。
//
// 注意: cached value はポインタ参照を直接返す。caller が User / UserProfile
// を mutate すると他の caller の view を破壊するので **read-only として扱う**。
// 既存実装も `FindByID` / `FindProfileByUserID` の戻り値を mutate していない
// (mutation は UpdateUser / UpdateProfile を介する) ので不変条件は保たれる。
type CachedUserRepository struct {
	UserRepository
	ttl time.Duration
	// maxEntries は users / profiles map それぞれの soft cap。size がこれを
	// 超えたタイミングの insert で expired entry を一括削除する。0 なら
	// userCacheMaxEntries が使われる。
	maxEntries int

	mu       sync.RWMutex
	users    map[string]userCacheEntry
	profiles map[string]profileCacheEntry
	// invalidatedAt は「その userID を最後に invalidate した時刻」。
	// FindXxx は DB read を始めた時刻を控えておき、store 時に
	// invalidatedAt がそれ以降なら書き込みを捨てる。これが無いと
	//
	//   R: cache miss → DB SELECT (更新前の行を読む)
	//   W:                          UPDATE → invalidate()
	//   R:                                              store(更新前の行)
	//
	// の順で **更新前の値が TTL 分 (5 分) キャッシュに居座る**。
	// ブラウザを開いていると read は常時走るので実際に踏む (#2257)。
	invalidatedAt map[string]time.Time
	// uriInvalidatedAt は byURI 側の同等物。storeURIMissing は user 不在の
	// negative cache なので userID で守れず、URI 単位で判定する必要がある。
	uriInvalidatedAt map[string]time.Time
	// byURI は AP 由来 URI → user のキャッシュ。inbox worker の hot path
	// (federation.Resolver.ResolveActor) が毎リクエスト FindByURI を叩く
	// ため、これを cache せずに DB に行くと PR #566 で軽量化した HTTP
	// 受信 fast path に対して worker drain がボトルネックになる (#567)。
	byURI map[string]uriCacheEntry
}

type userCacheEntry struct {
	user      *model.User
	expiresAt time.Time
	// missing=true は "DB に存在しない" を表す negative cache。pos は
	// nil pointer と区別したいので別フラグで持つ。
	missing bool
}

type profileCacheEntry struct {
	profile   *model.UserProfile
	expiresAt time.Time
	missing   bool
}

type uriCacheEntry struct {
	user      *model.User
	expiresAt time.Time
	missing   bool
}

// userCacheTTL は CachedUserRepository のデフォルト TTL。短すぎると
// timeline 描画で hit 率が落ち、長すぎると out-of-band の (例: 別の mk
// プロセスが共有 DB に書き込む) write が反映されるまで lag が出る。
// admin / federation processor / signin はすべて当該 wrapper 経由なので、
// 単一プロセス運用では TTL で守る必要は無いが、保守的に 5 分を採用する。
const userCacheTTL = 5 * time.Minute

// userCacheMaxEntries は per-map (users / profiles) のエントリ上限。
// 連合インスタンスでは reachable な remote user 数だけ unique ID lookup が
// 走り得るので、エントリは monotonically に増え続ける。しきい値超過時に
// 期限切れエントリの一括 sweep を走らせて O(1) amortized で抑える。
// しきい値 50000 は per-entry ~256B 想定で ~12MB を上限に取る (Devin
// review #552: unbounded growth 対策)。
const userCacheMaxEntries = 50000

// NewCachedUserRepository wraps inner with the default TTL.
func NewCachedUserRepository(inner UserRepository) *CachedUserRepository {
	return NewCachedUserRepositoryWithTTL(inner, userCacheTTL)
}

// NewCachedUserRepositoryWithTTL is the test-friendly constructor.
func NewCachedUserRepositoryWithTTL(inner UserRepository, ttl time.Duration) *CachedUserRepository {
	return newCachedUserRepository(inner, ttl, userCacheMaxEntries)
}

// newCachedUserRepository is the internal constructor that exposes maxEntries.
// Test-only entry point so the eviction path can be exercised without
// inserting 50k synthetic rows.
func newCachedUserRepository(inner UserRepository, ttl time.Duration, maxEntries int) *CachedUserRepository {
	if maxEntries <= 0 {
		maxEntries = userCacheMaxEntries
	}
	return &CachedUserRepository{
		UserRepository:   inner,
		ttl:              ttl,
		maxEntries:       maxEntries,
		users:            make(map[string]userCacheEntry),
		profiles:         make(map[string]profileCacheEntry),
		byURI:            make(map[string]uriCacheEntry),
		invalidatedAt:    make(map[string]time.Time),
		uriInvalidatedAt: make(map[string]time.Time),
	}
}

// SetMaxEntriesForTest overrides the soft cap. Tests only.
func (c *CachedUserRepository) SetMaxEntriesForTest(n int) {
	c.mu.Lock()
	if n <= 0 {
		c.maxEntries = userCacheMaxEntries
	} else {
		c.maxEntries = n
	}
	c.mu.Unlock()
}

// invalidate drops users[userID], profiles[userID], および byURI 内の同じ
// userID を持つ entry をすべて削除する。Mutation 系メソッドの末尾で呼ぶ。
//
// 当初は byURI を TTL に任せる設計だったが、Devin review #568 BUG-2 の
// 指摘通り FindByURI が cache 経由で stale な User struct (古い
// FollowingCount / IsSuspended 等) を最大 5 分返してしまう問題があり、
// FindByID 経路と一貫性が取れない。byURI map walk は最悪 O(maxEntries)
// だが mutation 経由でのみ発火するので per-request hot path には影響無し。
func (c *CachedUserRepository) invalidate(userID string) {
	if userID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	delete(c.users, userID)
	delete(c.profiles, userID)
	for uri, e := range c.byURI {
		if !e.missing && e.user != nil && e.user.ID == userID {
			delete(c.byURI, uri)
			c.uriInvalidatedAt[uri] = now
		}
	}
	c.markInvalidatedLocked(userID, now)
	c.mu.Unlock()
}

// markInvalidatedLocked records the invalidation instant for userID and
// prunes entries that can no longer affect any in-flight read (older than
// the cache TTL). Caller must hold c.mu.
func (c *CachedUserRepository) markInvalidatedLocked(userID string, now time.Time) {
	if userID != "" {
		c.invalidatedAt[userID] = now
	}
	if len(c.invalidatedAt) > c.maxEntries {
		cutoff := now.Add(-c.ttl)
		for k, t := range c.invalidatedAt {
			if t.Before(cutoff) {
				delete(c.invalidatedAt, k)
			}
		}
	}
	if len(c.uriInvalidatedAt) > c.maxEntries {
		cutoff := now.Add(-c.ttl)
		for k, t := range c.uriInvalidatedAt {
			if t.Before(cutoff) {
				delete(c.uriInvalidatedAt, k)
			}
		}
	}
}

// staleLocked reports whether a read that started at readStart may no longer
// be stored: true when userID was invalidated at or after readStart.
// Caller must hold c.mu.
func (c *CachedUserRepository) staleLocked(userID string, readStart time.Time) bool {
	t, ok := c.invalidatedAt[userID]
	return ok && !t.Before(readStart)
}

// uriStaleLocked is staleLocked for the byURI map. Caller must hold c.mu.
func (c *CachedUserRepository) uriStaleLocked(uri string, readStart time.Time) bool {
	t, ok := c.uriInvalidatedAt[uri]
	return ok && !t.Before(readStart)
}

// Invalidate is the public counterpart of invalidate. Out-of-band code paths
// that bypass this wrapper (例: テスト) can call it to force a refresh.
func (c *CachedUserRepository) Invalidate(userID string) {
	c.invalidate(userID)
}

// FindByID returns the cached user for id, falling through to the inner repo
// on miss / expiry. Errors other than not-found are returned unchanged and
// **never cached** so transient DB errors recover on the next call.
func (c *CachedUserRepository) FindByID(id string) (*model.User, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	if id == "" {
		return c.UserRepository.FindByID(id)
	}
	c.mu.RLock()
	if e, ok := c.users[id]; ok && time.Now().Before(e.expiresAt) {
		c.mu.RUnlock()
		if e.missing {
			return nil, gorm.ErrRecordNotFound
		}
		return e.user, nil
	}
	c.mu.RUnlock()

	readStart := time.Now()
	u, err := c.UserRepository.FindByID(id)
	if err != nil {
		// 「見つからない」は negative-cache する。timeline 描画で
		// 削除済みユーザーへの参照が大量に来る可能性に備える。それ
		// 以外の err は cache せず caller に返して上位で扱わせる。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.storeUserMissing(id, readStart)
			return nil, err
		}
		return nil, err
	}
	c.storeUser(id, u, readStart)
	return u, nil
}

// FindProfileByUserID returns the cached profile for userID with the same
// negative-cache semantics as FindByID.
func (c *CachedUserRepository) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	if !storable(userID) {
		return nil, ErrNotFound
	}
	if userID == "" {
		return c.UserRepository.FindProfileByUserID(userID)
	}
	c.mu.RLock()
	if e, ok := c.profiles[userID]; ok && time.Now().Before(e.expiresAt) {
		c.mu.RUnlock()
		if e.missing {
			return nil, gorm.ErrRecordNotFound
		}
		return e.profile, nil
	}
	c.mu.RUnlock()

	readStart := time.Now()
	p, err := c.UserRepository.FindProfileByUserID(userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.storeProfileMissing(userID, readStart)
			return nil, err
		}
		return nil, err
	}
	c.storeProfile(userID, p, readStart)
	return p, nil
}

// storeUser caches u under id unless the entry was invalidated while the
// DB read (started at readStart) was in flight. See invalidatedAt.
func (c *CachedUserRepository) storeUser(id string, u *model.User, readStart time.Time) {
	c.mu.Lock()
	if !c.staleLocked(id, readStart) {
		c.evictExpiredUsersLocked()
		c.users[id] = userCacheEntry{user: u, expiresAt: time.Now().Add(c.ttl)}
	}
	c.mu.Unlock()
}

func (c *CachedUserRepository) storeUserMissing(id string, readStart time.Time) {
	c.mu.Lock()
	if !c.staleLocked(id, readStart) {
		c.evictExpiredUsersLocked()
		c.users[id] = userCacheEntry{missing: true, expiresAt: time.Now().Add(c.ttl)}
	}
	c.mu.Unlock()
}

func (c *CachedUserRepository) storeProfile(userID string, p *model.UserProfile, readStart time.Time) {
	c.mu.Lock()
	if !c.staleLocked(userID, readStart) {
		c.evictExpiredProfilesLocked()
		c.profiles[userID] = profileCacheEntry{profile: p, expiresAt: time.Now().Add(c.ttl)}
	}
	c.mu.Unlock()
}

func (c *CachedUserRepository) storeProfileMissing(userID string, readStart time.Time) {
	c.mu.Lock()
	if !c.staleLocked(userID, readStart) {
		c.evictExpiredProfilesLocked()
		c.profiles[userID] = profileCacheEntry{missing: true, expiresAt: time.Now().Add(c.ttl)}
	}
	c.mu.Unlock()
}

// FindByURI returns the cached user for a remote AP URI, falling through
// to the inner repo on miss / expiry. AP inbox worker の hot path から
// 毎 request で呼ばれるため cache が effective: 同じ remote actor 由来の
// activity が連続して届く federation 受信は典型的に高 hit-ratio。
//
// negative cache 含む semantic は FindByID と同じ。URI は actor の
// canonical id (lifetime-stable) なので invalidate は通常 TTL 任せだが、
// 別経路で user row が更新された場合に refresh されるよう同期 store する
// FindByID 経路と TTL を揃える。
func (c *CachedUserRepository) FindByURI(uri string) (*model.User, error) {
	if !storable(uri) {
		return nil, ErrNotFound
	}
	if uri == "" {
		return c.UserRepository.FindByURI(uri)
	}
	c.mu.RLock()
	if e, ok := c.byURI[uri]; ok && time.Now().Before(e.expiresAt) {
		c.mu.RUnlock()
		if e.missing {
			return nil, gorm.ErrRecordNotFound
		}
		return e.user, nil
	}
	c.mu.RUnlock()

	readStart := time.Now()
	u, err := c.UserRepository.FindByURI(uri)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.storeURIMissing(uri, readStart)
			return nil, err
		}
		return nil, err
	}
	c.storeURI(uri, u, readStart)
	// FindByID 経路の cache にも入れておく。worker は両方の lookup を
	// する経路があり、片方 miss → DB → 反対側だけ updated は無駄なので。
	c.storeUser(u.ID, u, readStart)
	return u, nil
}

func (c *CachedUserRepository) storeURI(uri string, u *model.User, readStart time.Time) {
	c.mu.Lock()
	// URI 単位と userID 単位の両方を見る。URI が未 cache のまま user 側だけ
	// invalidate された場合は uriInvalidatedAt に印が付かないため。
	if !c.uriStaleLocked(uri, readStart) && !(u != nil && c.staleLocked(u.ID, readStart)) {
		c.evictExpiredURIsLocked()
		c.byURI[uri] = uriCacheEntry{user: u, expiresAt: time.Now().Add(c.ttl)}
	}
	c.mu.Unlock()
}

func (c *CachedUserRepository) storeURIMissing(uri string, readStart time.Time) {
	c.mu.Lock()
	if !c.uriStaleLocked(uri, readStart) {
		c.evictExpiredURIsLocked()
		c.byURI[uri] = uriCacheEntry{missing: true, expiresAt: time.Now().Add(c.ttl)}
	}
	c.mu.Unlock()
}

// evictExpiredUsersLocked は users map のサイズが userCacheMaxEntries を
// 超えていたら、期限切れエントリを一括削除して memory growth を抑える。
// 呼び出し側で c.mu.Lock 済みである必要がある。Devin #552: unbounded
// growth 対策。
func (c *CachedUserRepository) evictExpiredUsersLocked() {
	if len(c.users) < c.maxEntries {
		return
	}
	now := time.Now()
	for k, e := range c.users {
		if now.After(e.expiresAt) {
			delete(c.users, k)
		}
	}
}

func (c *CachedUserRepository) evictExpiredProfilesLocked() {
	if len(c.profiles) < c.maxEntries {
		return
	}
	now := time.Now()
	for k, e := range c.profiles {
		if now.After(e.expiresAt) {
			delete(c.profiles, k)
		}
	}
}

func (c *CachedUserRepository) evictExpiredURIsLocked() {
	if len(c.byURI) < c.maxEntries {
		return
	}
	now := time.Now()
	for k, e := range c.byURI {
		if now.After(e.expiresAt) {
			delete(c.byURI, k)
		}
	}
}

// --- mutating methods: delegate then invalidate -----------------------------

// Create persists a new user row. URI が指定されている場合は byURI cache の
// 該当 entry を必ず削除する (#567 / #568 fix): federation.Resolver は
// FindByURI miss → DB miss → 負 cache → 同 URI で remote fetch + Create
// → 次の FindByURI で stale 負 cache hit という flow を踏むため、Create
// 時の URI cache invalidation が無いと作ったばかりの user を 404 として
// 返してしまう。
func (c *CachedUserRepository) Create(u *model.User) error {
	if err := c.UserRepository.Create(u); err != nil {
		return err
	}
	if u != nil {
		c.invalidate(u.ID)
		if u.URI != nil && *u.URI != "" {
			c.invalidateURI(*u.URI)
		}
	}
	return nil
}

// invalidateURI drops the byURI cache entry for the given uri. URI を
// 知っている呼び出し側専用 (例: Create / Update でリモート user を作る /
// 更新するとき)。byURI map の負 cache が CREATE と競合して 404 を返す
// 問題への対処 (#568)。
func (c *CachedUserRepository) invalidateURI(uri string) {
	if uri == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	delete(c.byURI, uri)
	c.uriInvalidatedAt[uri] = now
	c.markInvalidatedLocked("", now)
	c.mu.Unlock()
}

// InvalidateURI is the public counterpart of invalidateURI for callers that
// know they have just persisted a user with a given URI.
func (c *CachedUserRepository) InvalidateURI(uri string) {
	c.invalidateURI(uri)
}

func (c *CachedUserRepository) UpdateUser(userID string, fields map[string]any) error {
	if err := c.UserRepository.UpdateUser(userID, fields); err != nil {
		return err
	}
	c.invalidate(userID)
	return nil
}

func (c *CachedUserRepository) UpdateProfile(userID string, fields map[string]any) error {
	if err := c.UserRepository.UpdateProfile(userID, fields); err != nil {
		return err
	}
	c.invalidate(userID)
	return nil
}

// RemoveBackupCode invalidates the cache after consuming a 2FA backup code.
//
// **override しないと消したコードがまた通る** (#2862)。profile はここで 5 分
// キャッシュされるので、embed のまま素通しすると `array_remove` で DB から
// 消えたコードがキャッシュ上には残る。予約 (SETNX、TTL 120s) が切れたあと
// キャッシュが切れるまでの窓では、`ConsumeBackupCode` が一致し、予約も通り、
// `array_remove` は no-op で成功するので**ログインが成立する**。
// signin が `UpdateProfile` から乗り換えた結果、そこにあった invalidate が
// 落ちて生まれた穴だった。
func (c *CachedUserRepository) RemoveBackupCode(userID, code string) error {
	if err := c.UserRepository.RemoveBackupCode(userID, code); err != nil {
		return err
	}
	c.invalidate(userID)
	return nil
}

// HardDeleteUser invalidates the cache after removing the row.
//
// 実際には削除の前に `isDeleted` を立てる経路 (UpdateUser 経由で invalidate
// 済み) を通るので現状は素通しでも表に出ないが、**wrapper の不変条件を
// 破ったまま残すと次に前提が変わったときに気付けない** (#2862)。
func (c *CachedUserRepository) HardDeleteUser(userID string) error {
	if err := c.UserRepository.HardDeleteUser(userID); err != nil {
		return err
	}
	c.invalidate(userID)
	return nil
}

// RevokeDeletedLocalCredentials invalidates cached user and profile secrets.
func (c *CachedUserRepository) RevokeDeletedLocalCredentials(userID string) error {
	if err := c.UserRepository.RevokeDeletedLocalCredentials(userID); err != nil {
		return err
	}
	c.invalidate(userID)
	return nil
}

// **DeleteOrphanRemoteUsers は意図的に override しない** (#2862)。
//
// 消えた userID を返さないので個別に invalidate できず、`invalidate("")` は
// no-op (`userID == ""` の early return)。`invalidatedAt` は per-userID / per-URI なので、
// 「in-flight read を壊さない全消し」を表す語彙がそもそも無い。
//
// 残る影響は「削除済みの remote user が最大 TTL 分キャッシュに残る」だけで、
// 対象は grace 期間を過ぎた非活動ユーザー。**この 1 つだけが override されて
// いないのは漏れではない**ので、消して回らないこと。

func (c *CachedUserRepository) UpdatePasswordIfCurrent(userID, currentHash, newHash string) (bool, error) {
	updated, err := c.UserRepository.UpdatePasswordIfCurrent(userID, currentHash, newHash)
	if err != nil || !updated {
		return updated, err
	}
	c.invalidate(userID)
	return true, nil
}

func (c *CachedUserRepository) CreateProfile(profile *model.UserProfile) error {
	if err := c.UserRepository.CreateProfile(profile); err != nil {
		return err
	}
	if profile != nil {
		c.invalidate(profile.UserID)
	}
	return nil
}

func (c *CachedUserRepository) IncrementFollowingCount(userID string, delta int) error {
	if err := c.UserRepository.IncrementFollowingCount(userID, delta); err != nil {
		return err
	}
	c.invalidate(userID)
	return nil
}

func (c *CachedUserRepository) IncrementFollowersCount(userID string, delta int) error {
	if err := c.UserRepository.IncrementFollowersCount(userID, delta); err != nil {
		return err
	}
	c.invalidate(userID)
	return nil
}

func (c *CachedUserRepository) IncrementNotesCount(userID string, delta int) error {
	if err := c.UserRepository.IncrementNotesCount(userID, delta); err != nil {
		return err
	}
	c.invalidate(userID)
	return nil
}

// FindManyByIDs delegates to the inner repo and warms the per-userID cache
// with the returned rows. これにより users/show バルク経路 (#503) の結果が
// 直後の ShowByID / FindByID で hit するようになる (Devin review #552 FLAG-1)。
// missing rows は inner が silently skip するため negative-cache は付けない
// (どの ID が抜けたかは戻り値からは判別できないため、別 round-trip での
// FindByID が走った時に negative-cache する経路に任せる)。
func (c *CachedUserRepository) FindManyByIDs(ids []string) ([]*model.User, error) {
	ids = storableIDs(ids)
	// **単発の経路と同じく readStart を取る** (#2862)。取らずに warm すると、
	// この read が飛んでいる最中に確定した更新を**古い行で塗り直す**。
	// 単発側は #2257 で塞いだが bulk が残っていた。攻撃者が大きな bulk read を
	// 投げれば窓は好きなだけ広げられる。
	readStart := time.Now()
	users, err := c.UserRepository.FindManyByIDs(ids)
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		c.mu.Lock()
		c.evictExpiredUsersLocked()
		expiry := time.Now().Add(c.ttl)
		for _, u := range users {
			if u != nil && u.ID != "" && !c.staleLocked(u.ID, readStart) {
				c.users[u.ID] = userCacheEntry{user: u, expiresAt: expiry}
			}
		}
		c.mu.Unlock()
	}
	return users, nil
}

// FindProfilesByUserIDs is the profile counterpart of FindManyByIDs:
// バルクで返ってきた行を per-userID cache に warm する。
func (c *CachedUserRepository) FindProfilesByUserIDs(userIDs []string) ([]*model.UserProfile, error) {
	// FindManyByIDs と同じ理由で readStart を取る (#2862)。**こちらは影響が重い** —
	// profile は `twoFactorBackupSecret` と `password` を持つので、古い行を
	// 焼き直すと消費済みのバックアップコードや旧パスワードのハッシュが復活する。
	readStart := time.Now()
	profiles, err := c.UserRepository.FindProfilesByUserIDs(userIDs)
	if err != nil {
		return nil, err
	}
	if len(profiles) > 0 {
		c.mu.Lock()
		c.evictExpiredProfilesLocked()
		expiry := time.Now().Add(c.ttl)
		for _, p := range profiles {
			if p != nil && p.UserID != "" && !c.staleLocked(p.UserID, readStart) {
				c.profiles[p.UserID] = profileCacheEntry{profile: p, expiresAt: expiry}
			}
		}
		c.mu.Unlock()
	}
	return profiles, nil
}
