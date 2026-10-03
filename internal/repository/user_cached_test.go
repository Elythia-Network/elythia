package repository_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// countingUserRepo wraps a stub UserRepository and exposes call counts on
// the methods we cache or invalidate around. それ以外は zero-value pass-
// through で十分 (caching とは無関係なので)。
type countingUserRepo struct {
	repository.UserRepository

	users    map[string]*model.User
	profiles map[string]*model.UserProfile

	findByIDCalls            atomic.Int64
	findProfileByUserIDCalls atomic.Int64
	findByURICalls           atomic.Int64

	updateUserErr        error
	updateProfileErr     error
	createProfileErr     error
	incFollowingErr      error
	incFollowersErr      error
	updateUserCalls      atomic.Int64
	updateProfileCalls   atomic.Int64
	updatePasswordCalls  atomic.Int64
	updatePasswordResult bool
	updatePasswordErr    error
	removeBackupCodeErr  error
	hardDeleteErr        error
	// onBulkRead runs after the "DB read" has produced its rows but before the
	// wrapper stores them. in-flight read 中に確定した更新を再現するのに使う。
	onBulkRead func()
}

func (c *countingUserRepo) RemoveBackupCode(userID, code string) error {
	if c.removeBackupCodeErr != nil {
		return c.removeBackupCodeErr
	}
	p, ok := c.profiles[userID]
	if !ok {
		return nil
	}
	out := make(model.StringArray, 0, len(p.TwoFactorBackupSecret))
	for _, v := range p.TwoFactorBackupSecret {
		if v != code {
			out = append(out, v)
		}
	}
	next := *p
	next.TwoFactorBackupSecret = out
	c.profiles[userID] = &next
	return nil
}

func (c *countingUserRepo) HardDeleteUser(userID string) error {
	if c.hardDeleteErr != nil {
		return c.hardDeleteErr
	}
	delete(c.users, userID)
	delete(c.profiles, userID)
	return nil
}

func newCountingUserRepo() *countingUserRepo {
	return &countingUserRepo{
		users:    make(map[string]*model.User),
		profiles: make(map[string]*model.UserProfile),
	}
}

func (c *countingUserRepo) FindByID(id string) (*model.User, error) {
	c.findByIDCalls.Add(1)
	if u, ok := c.users[id]; ok {
		return u, nil
	}
	return nil, gorm.ErrRecordNotFound
}

func (c *countingUserRepo) Create(u *model.User) error {
	if u == nil {
		return errors.New("nil user")
	}
	c.users[u.ID] = u
	return nil
}

func (c *countingUserRepo) FindByURI(uri string) (*model.User, error) {
	c.findByURICalls.Add(1)
	for _, u := range c.users {
		if u.URI != nil && *u.URI == uri {
			return u, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (c *countingUserRepo) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	c.findProfileByUserIDCalls.Add(1)
	if p, ok := c.profiles[userID]; ok {
		return p, nil
	}
	return nil, gorm.ErrRecordNotFound
}

func (c *countingUserRepo) UpdateUser(userID string, _ map[string]any) error {
	c.updateUserCalls.Add(1)
	return c.updateUserErr
}

func (c *countingUserRepo) UpdateProfile(userID string, _ map[string]any) error {
	c.updateProfileCalls.Add(1)
	return c.updateProfileErr
}

func (c *countingUserRepo) UpdatePasswordIfCurrent(userID, currentHash, newHash string) (bool, error) {
	c.updatePasswordCalls.Add(1)
	if c.updatePasswordErr != nil {
		return false, c.updatePasswordErr
	}
	if !c.updatePasswordResult {
		return false, nil
	}
	p := c.profiles[userID]
	if p == nil || p.Password == nil || *p.Password != currentHash {
		return false, nil
	}
	p.Password = &newHash
	return true, nil
}

func (c *countingUserRepo) CreateProfile(p *model.UserProfile) error {
	if c.createProfileErr != nil {
		return c.createProfileErr
	}
	if p != nil {
		c.profiles[p.UserID] = p
	}
	return nil
}

func (c *countingUserRepo) IncrementFollowingCount(_ string, _ int) error {
	return c.incFollowingErr
}

func (c *countingUserRepo) IncrementFollowersCount(_ string, _ int) error {
	return c.incFollowersErr
}

func (c *countingUserRepo) IncrementNotesCount(_ string, _ int) error {
	return nil
}

// FindManyByIDs returns **copies**, like a real DB read does (#2862)。
// ポインタをそのまま返すと、read 中に元の行が書き換わったとき既に取り出した
// 行まで一緒に変わってしまい、in-flight read の再現にならない。
func (c *countingUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	out := make([]*model.User, 0, len(ids))
	for _, id := range ids {
		if u, ok := c.users[id]; ok {
			cp := *u
			out = append(out, &cp)
		}
	}
	if c.onBulkRead != nil {
		c.onBulkRead()
	}
	return out, nil
}

func (c *countingUserRepo) FindProfilesByUserIDs(ids []string) ([]*model.UserProfile, error) {
	out := make([]*model.UserProfile, 0, len(ids))
	for _, id := range ids {
		if p, ok := c.profiles[id]; ok {
			cp := *p
			out = append(out, &cp)
		}
	}
	if c.onBulkRead != nil {
		c.onBulkRead()
	}
	return out, nil
}

func TestCachedUserRepository_FindByIDHitsInnerOnce(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1", Username: "alice"}
	cached := repository.NewCachedUserRepository(inner)

	for i := 0; i < 10; i++ {
		got, err := cached.FindByID("u1")
		require.NoError(t, err)
		assert.Equal(t, "alice", got.Username)
	}
	assert.Equal(t, int64(1), inner.findByIDCalls.Load(),
		"10 cached lookups must hit inner exactly once")
}

func TestCachedUserRepository_FindProfileByUserIDHitsInnerOnce(t *testing.T) {
	inner := newCountingUserRepo()
	desc := "hi"
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &desc}
	cached := repository.NewCachedUserRepository(inner)

	for i := 0; i < 5; i++ {
		got, err := cached.FindProfileByUserID("u1")
		require.NoError(t, err)
		assert.Equal(t, "hi", *got.Description)
	}
	assert.Equal(t, int64(1), inner.findProfileByUserIDCalls.Load())
}

func TestCachedUserRepository_UpdatePasswordIfCurrentInvalidatesOnlyOnChange(t *testing.T) {
	inner := newCountingUserRepo()
	oldHash := "old"
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &oldHash}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)
	_, err := cached.FindProfileByUserID("u1")
	require.NoError(t, err)
	require.Equal(t, int64(1), inner.findProfileByUserIDCalls.Load())

	inner.updatePasswordResult = false
	updated, err := cached.UpdatePasswordIfCurrent("u1", "old", "new")
	require.NoError(t, err)
	assert.False(t, updated)
	_, _ = cached.FindProfileByUserID("u1")
	assert.Equal(t, int64(1), inner.findProfileByUserIDCalls.Load())

	inner.updatePasswordErr = assert.AnError
	updated, err = cached.UpdatePasswordIfCurrent("u1", "old", "new")
	assert.ErrorIs(t, err, assert.AnError)
	assert.False(t, updated)
	_, _ = cached.FindProfileByUserID("u1")
	assert.Equal(t, int64(1), inner.findProfileByUserIDCalls.Load())
	inner.updatePasswordErr = nil

	inner.updatePasswordResult = true
	updated, err = cached.UpdatePasswordIfCurrent("u1", "old", "new")
	require.NoError(t, err)
	assert.True(t, updated)
	_, _ = cached.FindProfileByUserID("u1")
	assert.Equal(t, int64(2), inner.findProfileByUserIDCalls.Load())
}

func TestCachedUserRepository_FindByURIHitsInnerOnce(t *testing.T) {
	inner := newCountingUserRepo()
	uri := "https://remote.example/users/alice"
	inner.users["u1"] = &model.User{ID: "u1", Username: "alice", URI: &uri}
	cached := repository.NewCachedUserRepository(inner)

	for range 10 {
		got, err := cached.FindByURI(uri)
		require.NoError(t, err)
		assert.Equal(t, "u1", got.ID)
	}
	assert.Equal(t, int64(1), inner.findByURICalls.Load(),
		"10 cached URI lookups must hit inner exactly once")

	// FindByURI 経由で FindByID 側にも cache が乗ること (worker hot path)。
	_, err := cached.FindByID("u1")
	require.NoError(t, err)
	assert.Equal(t, int64(0), inner.findByIDCalls.Load(),
		"FindByID after FindByURI hit must not call inner")
}

func TestCachedUserRepository_FindByURINegativeCache(t *testing.T) {
	inner := newCountingUserRepo()
	cached := repository.NewCachedUserRepository(inner)

	for range 5 {
		_, err := cached.FindByURI("https://ghost/users/x")
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	}
	assert.Equal(t, int64(1), inner.findByURICalls.Load(),
		"missing URI must be negative-cached")
}

func TestCachedUserRepository_FindByURITransientErrorNotCached(t *testing.T) {
	inner := &erroringUserRepo{}
	cached := repository.NewCachedUserRepository(inner)
	_, err := cached.FindByURI("https://x/users/a")
	require.Error(t, err)
	_, err = cached.FindByURI("https://x/users/a")
	require.Error(t, err)
	assert.Equal(t, int64(2), inner.calls.Load())
}

// 同一 URI で先に NotFound 負 cache が乗ったあと、Create で同 URI の user
// を作っても次回 FindByURI で 404 を返さないこと (federation.Resolver の
// resolve → fetch → Create flow がここで詰まると pre-#568 の e2e
// regression と同じ「直前に作った remote user が 404」になる)。
func TestCachedUserRepository_CreateInvalidatesNegativeURICache(t *testing.T) {
	inner := newCountingUserRepo()
	uri := "https://r/u/late"
	cached := repository.NewCachedUserRepository(inner)

	// 1. 先に存在しない URI を引いて負 cache を作る
	_, err := cached.FindByURI(uri)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	// 2. user を Create
	require.NoError(t, cached.Create(&model.User{ID: "u1", URI: &uri}))

	// 3. もう一度 FindByURI して、404 でなく作成した user が返ること
	got, err := cached.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, "u1", got.ID)
}

// UpdateUser で URI が変わるケース (rename / move): byURI cache を userID
// で逆引き invalidate する。
func TestCachedUserRepository_UpdateUserInvalidatesURICache(t *testing.T) {
	inner := newCountingUserRepo()
	uri := "https://r/u/x"
	inner.users["u1"] = &model.User{ID: "u1", URI: &uri}
	cached := repository.NewCachedUserRepository(inner)

	// warm cache via FindByURI
	_, err := cached.FindByURI(uri)
	require.NoError(t, err)

	// update via cached repo (e.g. fields map で uri 変更想定)
	require.NoError(t, cached.UpdateUser("u1", map[string]any{"uri": "https://r/u/y"}))

	// FindByURI(old_uri) は inner に再度行くこと (cache hit せず)
	prev := inner.findByURICalls.Load()
	_, _ = cached.FindByURI(uri)
	assert.Greater(t, inner.findByURICalls.Load(), prev,
		"UpdateUser must invalidate URI cache for that user")
}

func TestCachedUserRepository_FindByURIRefreshesAfterTTL(t *testing.T) {
	inner := newCountingUserRepo()
	uri := "https://r/u/a"
	inner.users["u1"] = &model.User{ID: "u1", URI: &uri}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, 1*time.Millisecond)

	_, err := cached.FindByURI(uri)
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	_, err = cached.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, int64(2), inner.findByURICalls.Load())
}

func TestCachedUserRepository_NotFoundIsNegativeCached(t *testing.T) {
	inner := newCountingUserRepo()
	cached := repository.NewCachedUserRepository(inner)

	for i := 0; i < 5; i++ {
		_, err := cached.FindByID("ghost")
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	}
	assert.Equal(t, int64(1), inner.findByIDCalls.Load(),
		"missing rows must be negative-cached so timeline ghost lookups don't reflood inner")
}

// ErrNotFound 以外の error は cache されない。transient な DB 障害が次回
// 呼び出しで自動回復することを担保する。
type erroringUserRepo struct {
	repository.UserRepository
	calls atomic.Int64
}

func (e *erroringUserRepo) FindByID(_ string) (*model.User, error) {
	e.calls.Add(1)
	return nil, errors.New("db down")
}

func (e *erroringUserRepo) FindProfileByUserID(_ string) (*model.UserProfile, error) {
	e.calls.Add(1)
	return nil, errors.New("db down")
}

func (e *erroringUserRepo) FindByURI(_ string) (*model.User, error) {
	e.calls.Add(1)
	return nil, errors.New("db down")
}

func TestCachedUserRepository_TransientErrorIsNotCached(t *testing.T) {
	inner := &erroringUserRepo{}
	cached := repository.NewCachedUserRepository(inner)

	_, err := cached.FindByID("u1")
	require.Error(t, err)
	_, err = cached.FindByID("u1")
	require.Error(t, err)
	assert.Equal(t, int64(2), inner.calls.Load(),
		"non-NotFound errors must not be cached")

	_, err = cached.FindProfileByUserID("u1")
	require.Error(t, err)
	_, err = cached.FindProfileByUserID("u1")
	require.Error(t, err)
	assert.Equal(t, int64(4), inner.calls.Load())
}

func TestCachedUserRepository_RefreshesAfterTTL(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1"}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, 1*time.Millisecond)

	_, err := cached.FindByID("u1")
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	_, err = cached.FindByID("u1")
	require.NoError(t, err)
	assert.Equal(t, int64(2), inner.findByIDCalls.Load())
}

func TestCachedUserRepository_UpdateUserInvalidates(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1", Username: "alice"}
	cached := repository.NewCachedUserRepository(inner)

	_, _ = cached.FindByID("u1") // warm
	require.NoError(t, cached.UpdateUser("u1", map[string]any{"name": "x"}))
	_, _ = cached.FindByID("u1")
	assert.Equal(t, int64(2), inner.findByIDCalls.Load(),
		"UpdateUser must invalidate so the next FindByID re-reads")
}

func TestCachedUserRepository_UpdateUserErrDoesNotInvalidate(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1"}
	inner.updateUserErr = errors.New("db down")
	cached := repository.NewCachedUserRepository(inner)

	_, _ = cached.FindByID("u1") // warm
	err := cached.UpdateUser("u1", map[string]any{"name": "x"})
	require.Error(t, err)
	_, _ = cached.FindByID("u1")
	assert.Equal(t, int64(1), inner.findByIDCalls.Load(),
		"failed UpdateUser must not invalidate (DB state unchanged)")
}

func TestCachedUserRepository_UpdateProfileInvalidatesProfile(t *testing.T) {
	inner := newCountingUserRepo()
	desc := "hi"
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &desc}
	cached := repository.NewCachedUserRepository(inner)

	_, _ = cached.FindProfileByUserID("u1")
	require.NoError(t, cached.UpdateProfile("u1", map[string]any{"desc": "y"}))
	_, _ = cached.FindProfileByUserID("u1")
	assert.Equal(t, int64(2), inner.findProfileByUserIDCalls.Load())
}

// UpdateProfile (例: lastActiveDate / password) が user 行も同 ID で
// 共有されているケースに備え、profile 更新時は user 側 cache も飛ばす。
// (現状は同 userID で削除しているので user 側エントリも消える挙動。)
func TestCachedUserRepository_UpdateProfileInvalidatesUserAlso(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1"}
	desc := "hi"
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &desc}
	cached := repository.NewCachedUserRepository(inner)

	_, _ = cached.FindByID("u1")
	_, _ = cached.FindProfileByUserID("u1")
	require.NoError(t, cached.UpdateProfile("u1", map[string]any{"desc": "y"}))
	_, _ = cached.FindByID("u1")
	assert.Equal(t, int64(2), inner.findByIDCalls.Load(),
		"UpdateProfile invalidates both user and profile entries for the same ID")
}

func TestCachedUserRepository_CreateProfileInvalidates(t *testing.T) {
	inner := newCountingUserRepo()
	cached := repository.NewCachedUserRepository(inner)

	// 最初は profile なし → negative cache に乗る
	_, err := cached.FindProfileByUserID("u1")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.Equal(t, int64(1), inner.findProfileByUserIDCalls.Load())

	// CreateProfile 後は negative cache を飛ばして再取得する
	require.NoError(t, cached.CreateProfile(&model.UserProfile{UserID: "u1"}))
	got, err := cached.FindProfileByUserID("u1")
	require.NoError(t, err)
	assert.Equal(t, "u1", got.UserID)
	assert.Equal(t, int64(2), inner.findProfileByUserIDCalls.Load())
}

func TestCachedUserRepository_IncrementFollowingCountInvalidates(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1", FollowingCount: 0}
	cached := repository.NewCachedUserRepository(inner)

	_, _ = cached.FindByID("u1")
	require.NoError(t, cached.IncrementFollowingCount("u1", 1))
	_, _ = cached.FindByID("u1")
	assert.Equal(t, int64(2), inner.findByIDCalls.Load())
}

// notesCount は note 作成のたびに userRepo.IncrementNotesCount で増減
// される (旧経路の noteRepo.IncrementUserNotesCount は cache を bypass
// していた)。CachedUserRepository が invalidate を取りこぼすと profile
// 表示で stale notesCount が出続けるので必ず飛ぶこと (Devin #552 BUG-2)。
func TestCachedUserRepository_IncrementNotesCountInvalidates(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1"}
	cached := repository.NewCachedUserRepository(inner)

	_, _ = cached.FindByID("u1")
	require.NoError(t, cached.IncrementNotesCount("u1", 1))
	_, _ = cached.FindByID("u1")
	assert.Equal(t, int64(2), inner.findByIDCalls.Load())
}

func TestCachedUserRepository_IncrementFollowersCountInvalidates(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1"}
	cached := repository.NewCachedUserRepository(inner)

	_, _ = cached.FindByID("u1")
	require.NoError(t, cached.IncrementFollowersCount("u1", 1))
	_, _ = cached.FindByID("u1")
	assert.Equal(t, int64(2), inner.findByIDCalls.Load())
}

func TestCachedUserRepository_PublicInvalidate(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1"}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Hour)

	_, _ = cached.FindByID("u1")
	cached.Invalidate("u1")
	_, _ = cached.FindByID("u1")
	assert.Equal(t, int64(2), inner.findByIDCalls.Load())
}

// FindManyByIDs / FindProfilesByUserIDs はバルク結果で per-userID cache を
// warm する。直後の FindByID / FindProfileByUserID は inner を再度叩かない
// (Devin review #552 FLAG-1)。
func TestCachedUserRepository_FindManyByIDsWarmsPerKeyCache(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1"}
	inner.users["u2"] = &model.User{ID: "u2"}
	cached := repository.NewCachedUserRepository(inner)

	out, err := cached.FindManyByIDs([]string{"u1", "u2"})
	require.NoError(t, err)
	require.Len(t, out, 2)

	// per-key cache が warm されているので FindByID は inner を叩かない。
	_, err = cached.FindByID("u1")
	require.NoError(t, err)
	_, err = cached.FindByID("u2")
	require.NoError(t, err)
	assert.Equal(t, int64(0), inner.findByIDCalls.Load(),
		"bulk fetch must populate per-key cache")
}

func TestCachedUserRepository_FindProfilesByUserIDsWarmsPerKeyCache(t *testing.T) {
	inner := newCountingUserRepo()
	desc := "p"
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &desc}
	inner.profiles["u2"] = &model.UserProfile{UserID: "u2", Description: &desc}
	cached := repository.NewCachedUserRepository(inner)

	out, err := cached.FindProfilesByUserIDs([]string{"u1", "u2"})
	require.NoError(t, err)
	require.Len(t, out, 2)

	_, err = cached.FindProfileByUserID("u1")
	require.NoError(t, err)
	_, err = cached.FindProfileByUserID("u2")
	require.NoError(t, err)
	assert.Equal(t, int64(0), inner.findProfileByUserIDCalls.Load(),
		"bulk profile fetch must populate per-key cache")
}

// users map のサイズが maxEntries を超えたら、insert 時に期限切れエントリ
// を一括削除する (Devin review #552: unbounded growth 対策)。
//
// 検証戦略: cap=3 / TTL=10ms で 3 件 warm → 期限切れ待機 → 4 件目を insert
// → 期限切れ 3 件は map から消えるので、それらを再取得すると inner を
// 叩く回数が増える (cache miss に戻る)。新しい entry は live なので
// cache hit のまま。
func TestCachedUserRepository_EvictsExpiredEntriesOnInsertOverCap(t *testing.T) {
	inner := newCountingUserRepo()
	for _, id := range []string{"u1", "u2", "u3", "u4"} {
		inner.users[id] = &model.User{ID: id}
	}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, 10*time.Millisecond)
	cached.SetMaxEntriesForTest(3)

	// 3 件 warm。inner は 3 回叩かれる。
	for _, id := range []string{"u1", "u2", "u3"} {
		_, err := cached.FindByID(id)
		require.NoError(t, err)
	}
	require.Equal(t, int64(3), inner.findByIDCalls.Load())

	// TTL 失効を待つ。
	time.Sleep(15 * time.Millisecond)

	// 4 件目を insert: len == cap(3) で eviction 発火 → expired 3 件削除。
	_, err := cached.FindByID("u4")
	require.NoError(t, err)
	// u4 lookup で +1
	require.Equal(t, int64(4), inner.findByIDCalls.Load())

	// 期限切れだった u1/u2/u3 は map から消えているので、再 lookup は
	// inner を叩く (cache miss)。
	for _, id := range []string{"u1", "u2", "u3"} {
		_, _ = cached.FindByID(id)
	}
	assert.Equal(t, int64(7), inner.findByIDCalls.Load(),
		"expired entries must be evicted on insert when over cap, forcing inner re-fetch")
}

// profile 側の同 eviction 挙動。
func TestCachedUserRepository_EvictsExpiredProfilesOnInsertOverCap(t *testing.T) {
	inner := newCountingUserRepo()
	desc := "p"
	for _, id := range []string{"u1", "u2", "u3", "u4"} {
		inner.profiles[id] = &model.UserProfile{UserID: id, Description: &desc}
	}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, 10*time.Millisecond)
	cached.SetMaxEntriesForTest(3)

	for _, id := range []string{"u1", "u2", "u3"} {
		_, err := cached.FindProfileByUserID(id)
		require.NoError(t, err)
	}
	require.Equal(t, int64(3), inner.findProfileByUserIDCalls.Load())

	time.Sleep(15 * time.Millisecond)

	_, err := cached.FindProfileByUserID("u4")
	require.NoError(t, err)
	require.Equal(t, int64(4), inner.findProfileByUserIDCalls.Load())

	for _, id := range []string{"u1", "u2", "u3"} {
		_, _ = cached.FindProfileByUserID(id)
	}
	assert.Equal(t, int64(7), inner.findProfileByUserIDCalls.Load(),
		"expired profile entries must be evicted on insert when over cap")
}

// 空 ID は no-op で inner にだけ任せる (lookup error がそのまま返る)。
// 空 ID を cache key にすると意図しない衝突を起こすので意図的に bypass する。
func TestCachedUserRepository_EmptyIDIsBypassed(t *testing.T) {
	inner := newCountingUserRepo()
	cached := repository.NewCachedUserRepository(inner)

	for i := 0; i < 3; i++ {
		_, _ = cached.FindByID("")
		_, _ = cached.FindProfileByUserID("")
	}
	assert.Equal(t, int64(3), inner.findByIDCalls.Load())
	assert.Equal(t, int64(3), inner.findProfileByUserIDCalls.Load())
}

// slowUserRepo は「DB read が始まってから完了するまでの間に UPDATE +
// invalidate が割り込む」レースを決定的に再現するための stub。
// FindByID / FindProfileByUserID / FindByURI は読み取りを開始した時点の
// 値を返し、途中で hook を呼んで caller に割り込みの機会を与える。
type slowUserRepo struct {
	repository.UserRepository

	user    *model.User
	profile *model.UserProfile
	// onRead は DB からの読み取り「後」、cache へ store される「前」に
	// 呼ばれる。ここで UpdateUser を叩けば実運用のレース窓と同じ順序になる。
	onRead func()
}

func (s *slowUserRepo) FindByID(string) (*model.User, error) {
	snapshot := s.user
	if s.onRead != nil {
		s.onRead()
	}
	return snapshot, nil
}

func (s *slowUserRepo) FindProfileByUserID(string) (*model.UserProfile, error) {
	snapshot := s.profile
	if s.onRead != nil {
		s.onRead()
	}
	return snapshot, nil
}

func (s *slowUserRepo) FindByURI(string) (*model.User, error) {
	snapshot := s.user
	if s.onRead != nil {
		s.onRead()
	}
	return snapshot, nil
}

func (s *slowUserRepo) UpdateUser(string, map[string]any) error    { return nil }
func (s *slowUserRepo) UpdateProfile(string, map[string]any) error { return nil }

// 更新と重なった読み取りが「更新前の値」を cache に焼き付けないこと (#2257)。
//
// 旧実装は store 時に無条件で map へ書いていたため、
//
//	R: cache miss → DB SELECT (更新前)
//	W:                         UPDATE → invalidate()
//	R:                                             store(更新前)
//
// の順で更新前の値が TTL (5 分) 居座り、i/update 直後の i / users/show が
// 古い値を返し続けた。
func TestCachedUserRepository_StaleReadNotCachedAcrossInvalidate(t *testing.T) {
	t.Run("FindByID", func(t *testing.T) {
		oldUser := &model.User{ID: "u1", Name: strPtrUserCached("old")}
		newUser := &model.User{ID: "u1", Name: strPtrUserCached("new")}
		inner := &slowUserRepo{user: oldUser}
		cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)

		inner.onRead = func() {
			// 読み取り中に更新が確定したことにする
			inner.user = newUser
			require.NoError(t, cached.UpdateUser("u1", map[string]any{"name": "new"}))
		}

		got, err := cached.FindByID("u1")
		require.NoError(t, err)
		assert.Equal(t, "old", *got.Name, "in-flight read still returns its own snapshot")

		// 次の読み取りは cache ではなく DB を見て新しい値を返すこと
		inner.onRead = nil
		got2, err := cached.FindByID("u1")
		require.NoError(t, err)
		assert.Equal(t, "new", *got2.Name, "stale snapshot must not be cached")
	})

	t.Run("FindProfileByUserID", func(t *testing.T) {
		oldProfile := &model.UserProfile{UserID: "u1", Description: strPtrUserCached("old")}
		newProfile := &model.UserProfile{UserID: "u1", Description: strPtrUserCached("new")}
		inner := &slowUserRepo{profile: oldProfile}
		cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)

		inner.onRead = func() {
			inner.profile = newProfile
			require.NoError(t, cached.UpdateProfile("u1", map[string]any{"description": "new"}))
		}

		_, err := cached.FindProfileByUserID("u1")
		require.NoError(t, err)

		inner.onRead = nil
		got2, err := cached.FindProfileByUserID("u1")
		require.NoError(t, err)
		assert.Equal(t, "new", *got2.Description, "stale profile must not be cached")
	})

	t.Run("FindByURI", func(t *testing.T) {
		uri := "https://remote.example/users/1"
		oldUser := &model.User{ID: "u1", Name: strPtrUserCached("old"), URI: &uri}
		newUser := &model.User{ID: "u1", Name: strPtrUserCached("new"), URI: &uri}
		inner := &slowUserRepo{user: oldUser}
		cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)

		inner.onRead = func() {
			inner.user = newUser
			require.NoError(t, cached.UpdateUser("u1", map[string]any{"name": "new"}))
		}

		_, err := cached.FindByURI(uri)
		require.NoError(t, err)

		inner.onRead = nil
		got2, err := cached.FindByURI(uri)
		require.NoError(t, err)
		assert.Equal(t, "new", *got2.Name, "stale byURI entry must not be cached")
		// FindByURI は byID 側にも書くので、そちらも stale であってはならない
		got3, err := cached.FindByID("u1")
		require.NoError(t, err)
		assert.Equal(t, "new", *got3.Name, "stale byID entry must not be cached from FindByURI")
	})
}

func strPtrUserCached(s string) *string { return &s }

// markInvalidatedLocked の prune 分岐: invalidatedAt / uriInvalidatedAt が
// maxEntries を超えたら TTL より古い印を落として unbounded growth を防ぐこと。
func TestCachedUserRepository_InvalidationMarkerPruning(t *testing.T) {
	inner := newCountingUserRepo()
	cached := repository.NewCachedUserRepositoryWithTTL(inner, 10*time.Millisecond)
	cached.SetMaxEntriesForTest(2)

	// TTL より古くなる印を 3 件作る
	for _, id := range []string{"old1", "old2", "old3"} {
		require.NoError(t, cached.UpdateUser(id, map[string]any{"name": "x"}))
	}
	time.Sleep(20 * time.Millisecond)

	// maxEntries 超過状態で更に invalidate すると prune が走る。
	// 観測点: prune 後も「今 invalidate した id」は stale 判定が効くこと。
	require.NoError(t, cached.UpdateUser("fresh", map[string]any{"name": "x"}))

	inner.users["old1"] = &model.User{ID: "old1", Name: strPtrUserCached("v1")}
	got, err := cached.FindByID("old1")
	require.NoError(t, err)
	assert.Equal(t, "v1", *got.Name, "prune 済みの古い印は以後の store を妨げない")

	// URI 側の印も同様に prune 対象。
	cached.InvalidateURI("https://remote.example/users/pruned")
	uri := "https://remote.example/users/live"
	inner.users["u9"] = &model.User{ID: "u9", Name: strPtrUserCached("v9"), URI: &uri}
	byURI, err := cached.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, "v9", *byURI.Name)
}

// negative cache (missing) 側も invalidate と競合したら焼き付けないこと。
// 「作成中の user を並行 read が 404 と cache する」と Create 直後の
// lookup が TTL の間ずっと 404 になる。
func TestCachedUserRepository_StaleMissingNotCachedAcrossInvalidate(t *testing.T) {
	t.Run("FindByID", func(t *testing.T) {
		inner := &missingThenFoundRepo{user: &model.User{ID: "u1", Name: strPtrUserCached("created")}}
		cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)
		inner.onRead = func() {
			inner.found = true
			require.NoError(t, cached.UpdateUser("u1", map[string]any{"name": "created"}))
		}

		_, err := cached.FindByID("u1")
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)

		inner.onRead = nil
		got, err := cached.FindByID("u1")
		require.NoError(t, err, "stale negative cache must not stick")
		assert.Equal(t, "created", *got.Name)
	})

	t.Run("FindByURI", func(t *testing.T) {
		uri := "https://remote.example/users/new"
		u := &model.User{ID: "u1", Name: strPtrUserCached("created"), URI: &uri}
		inner := &missingThenFoundRepo{user: u}
		cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)
		inner.onRead = func() {
			inner.found = true
			cached.InvalidateURI(uri)
		}

		_, err := cached.FindByURI(uri)
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)

		inner.onRead = nil
		got, err := cached.FindByURI(uri)
		require.NoError(t, err, "stale negative byURI cache must not stick")
		assert.Equal(t, "created", *got.Name)
	})
}

// missingThenFoundRepo は「read 開始時点では未作成、read 中に作成される」
// 状況を再現する stub。
type missingThenFoundRepo struct {
	repository.UserRepository

	user   *model.User
	found  bool
	onRead func()
}

func (m *missingThenFoundRepo) find() (*model.User, error) {
	wasFound := m.found
	if m.onRead != nil {
		m.onRead()
	}
	if !wasFound {
		return nil, gorm.ErrRecordNotFound
	}
	return m.user, nil
}

func (m *missingThenFoundRepo) FindByID(string) (*model.User, error)  { return m.find() }
func (m *missingThenFoundRepo) FindByURI(string) (*model.User, error) { return m.find() }
func (m *missingThenFoundRepo) UpdateUser(string, map[string]any) error {
	return nil
}

// **消費したバックアップコードがキャッシュに残らないこと** (#2862)。
//
// profile は 5 分キャッシュされる。`RemoveBackupCode` が invalidate しないと、
// `array_remove` で DB から消えたコードがキャッシュ上には残り続ける。予約
// (SETNX、TTL 120s) が切れたあとキャッシュが切れるまでの窓では
// `ConsumeBackupCode` が一致し、予約も通り、`array_remove` は no-op で成功する
// ので**ログインが成立する**。signin が UpdateProfile から乗り換えたときに、
// そこにあった invalidate が落ちて生まれた穴だった。
func TestCachedUserRepository_RemoveBackupCodeInvalidates(t *testing.T) {
	inner := newCountingUserRepo()
	inner.profiles["u1"] = &model.UserProfile{
		UserID:                "u1",
		TwoFactorBackupSecret: model.StringArray{"c1", "c2"},
	}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)

	before, err := cached.FindProfileByUserID("u1")
	require.NoError(t, err)
	require.Equal(t, []string{"c1", "c2"}, []string(before.TwoFactorBackupSecret))
	require.Equal(t, int64(1), inner.findProfileByUserIDCalls.Load())

	require.NoError(t, cached.RemoveBackupCode("u1", "c1"))

	after, err := cached.FindProfileByUserID("u1")
	require.NoError(t, err)
	assert.Equal(t, []string{"c2"}, []string(after.TwoFactorBackupSecret),
		"消したコードがキャッシュから返っている")
	assert.Equal(t, int64(2), inner.findProfileByUserIDCalls.Load(), "inner を引き直していない")
}

// 消費に失敗したらキャッシュを落とさない (落とす必要が無く、無駄に温度を下げる)。
func TestCachedUserRepository_RemoveBackupCodeKeepsCacheOnError(t *testing.T) {
	inner := newCountingUserRepo()
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", TwoFactorBackupSecret: model.StringArray{"c1"}}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)
	_, err := cached.FindProfileByUserID("u1")
	require.NoError(t, err)

	inner.removeBackupCodeErr = assert.AnError
	assert.ErrorIs(t, cached.RemoveBackupCode("u1", "c1"), assert.AnError)

	_, _ = cached.FindProfileByUserID("u1")
	assert.Equal(t, int64(1), inner.findProfileByUserIDCalls.Load())
}

// 削除に失敗したらキャッシュを落とさない (RemoveBackupCode と同じ判断)。
func (c *countingUserRepo) RevokeDeletedLocalCredentials(userID string) error {
	if c.hardDeleteErr != nil {
		return c.hardDeleteErr
	}
	if u := c.users[userID]; u != nil {
		u.Token = nil
	}
	if p := c.profiles[userID]; p != nil {
		p.Password = nil
	}
	return nil
}

func TestCachedUserRepository_RevokeCredentialsInvalidates(t *testing.T) {
	for _, fail := range []bool{false, true} {
		inner := newCountingUserRepo()
		secret := "secret"
		inner.users["u1"] = &model.User{ID: "u1", Token: &secret}
		inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &secret}
		cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)
		_, err := cached.FindByID("u1")
		require.NoError(t, err)
		_, err = cached.FindProfileByUserID("u1")
		require.NoError(t, err)
		if fail {
			inner.hardDeleteErr = assert.AnError
			require.ErrorIs(t, cached.RevokeDeletedLocalCredentials("u1"), assert.AnError)
		} else {
			require.NoError(t, cached.RevokeDeletedLocalCredentials("u1"))
		}
		u, err := cached.FindByID("u1")
		require.NoError(t, err)
		p, err := cached.FindProfileByUserID("u1")
		require.NoError(t, err)
		if fail {
			assert.NotNil(t, u.Token)
			assert.NotNil(t, p.Password)
			assert.EqualValues(t, 1, inner.findByIDCalls.Load())
			assert.EqualValues(t, 1, inner.findProfileByUserIDCalls.Load())
		} else {
			assert.Nil(t, u.Token)
			assert.Nil(t, p.Password)
			assert.EqualValues(t, 2, inner.findByIDCalls.Load())
			assert.EqualValues(t, 2, inner.findProfileByUserIDCalls.Load())
		}
	}
}

func TestCachedUserRepository_HardDeleteUserKeepsCacheOnError(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1", Username: "alice"}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)
	_, err := cached.FindByID("u1")
	require.NoError(t, err)

	inner.hardDeleteErr = assert.AnError
	assert.ErrorIs(t, cached.HardDeleteUser("u1"), assert.AnError)

	_, _ = cached.FindByID("u1")
	assert.Equal(t, int64(1), inner.findByIDCalls.Load(), "失敗したのにキャッシュを落としている")
}

// HardDeleteUser も同型 (#2862)。今は削除前に isDeleted を立てる経路を通るので
// 表には出ないが、wrapper の不変条件を破ったまま残さない。
func TestCachedUserRepository_HardDeleteUserInvalidates(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1", Username: "alice"}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)
	_, err := cached.FindByID("u1")
	require.NoError(t, err)
	require.Equal(t, int64(1), inner.findByIDCalls.Load())

	require.NoError(t, cached.HardDeleteUser("u1"))

	_, err = cached.FindByID("u1")
	assert.Error(t, err, "削除済みユーザーがキャッシュから返っている")
	assert.Equal(t, int64(2), inner.findByIDCalls.Load())
}

// **bulk read が in-flight のあいだに確定した更新を、古い行で塗り直さないこと**
// (#2862)。単発の経路は #2257 で塞いだが bulk が残っていた。
//
// profile は `twoFactorBackupSecret` と `password` を持つので、ここが破れると
// **消費済みのバックアップコードや旧パスワードのハッシュが TTL のあいだ復活する**。
// 大きな bulk read を投げれば窓は好きなだけ広げられる。
func TestCachedUserRepository_BulkProfileReadDoesNotResurrectStaleRow(t *testing.T) {
	inner := newCountingUserRepo()
	inner.profiles["u1"] = &model.UserProfile{
		UserID:                "u1",
		TwoFactorBackupSecret: model.StringArray{"c1", "c2"},
	}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)

	// bulk read が行を取り出したあと、cache へ入れる前に消費を確定させる。
	inner.onBulkRead = func() {
		require.NoError(t, cached.RemoveBackupCode("u1", "c1"))
	}
	_, err := cached.FindProfilesByUserIDs([]string{"u1"})
	require.NoError(t, err)

	got, err := cached.FindProfileByUserID("u1")
	require.NoError(t, err)
	assert.NotContains(t, []string(got.TwoFactorBackupSecret), "c1",
		"消したコードが bulk read の結果で焼き直されている")
}

// user 側も同型。凍結を確定させた直後に、凍結前の行が焼き直されないこと。
func TestCachedUserRepository_BulkUserReadDoesNotResurrectStaleRow(t *testing.T) {
	inner := newCountingUserRepo()
	inner.users["u1"] = &model.User{ID: "u1", Username: "alice"}
	cached := repository.NewCachedUserRepositoryWithTTL(inner, time.Minute)

	inner.onBulkRead = func() {
		inner.users["u1"] = &model.User{ID: "u1", Username: "alice", IsSuspended: true}
		require.NoError(t, cached.UpdateUser("u1", map[string]any{"isSuspended": true}))
	}
	_, err := cached.FindManyByIDs([]string{"u1"})
	require.NoError(t, err)

	got, err := cached.FindByID("u1")
	require.NoError(t, err)
	assert.True(t, got.IsSuspended, "凍結前の行が bulk read の結果で焼き直されている")
}
