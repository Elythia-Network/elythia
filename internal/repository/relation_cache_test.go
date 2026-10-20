package repository

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relFixture seeds users (and channels) for one relation cache test and
// removes them afterwards. ON DELETE CASCADE takes the relation rows along.
type relFixture struct {
	t      *testing.T
	prefix string
}

func newRelFixture(t *testing.T, prefix string) *relFixture {
	t.Helper()
	f := &relFixture{t: t, prefix: prefix}
	cleanup := func() {
		testDB.Exec(`DELETE FROM "channel" WHERE id LIKE ?`, prefix+"%")
		testDB.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)
	return f
}

func (f *relFixture) user(name string) string {
	f.t.Helper()
	id := f.prefix + name
	insertTestUser(f.t, id, id)
	return id
}

func (f *relFixture) channel(name, ownerID string) string {
	f.t.Helper()
	id := f.prefix + name
	require.NoError(f.t, testDB.Exec(`INSERT INTO "channel" (id, name, "userId") VALUES (?, ?, ?)`, id, id, ownerID).Error)
	return id
}

func (f *relFixture) id(name string) string { return f.prefix + name }

// mustInsert writes a row directly, bypassing the repositories (and therefore
// the cache invalidation). Tests use it to tell a cached list from a reload.
func mustInsert(t *testing.T, row any) {
	t.Helper()
	require.NoError(t, testDB.Create(row).Error)
}

func TestRelationCache_ServesFromCacheUntilInvalidated(t *testing.T) {
	f := newRelFixture(t, "rcA_")
	viewer, x, y := f.user("v"), f.user("x"), f.user("y")
	rc := NewRelationCache(testDB)

	mustInsert(t, &model.Muting{ID: f.id("m1"), MuterID: viewer, MuteeID: x})
	ids, err := rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, ids)

	// repository を通らない書き込みはキャッシュに見えない (= キャッシュから返している)。
	mustInsert(t, &model.Muting{ID: f.id("m2"), MuterID: viewer, MuteeID: y})
	ids, err = rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, ids)

	rc.Invalidate(RelationMuting, viewer)
	ids, err = rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{x, y}, ids)

	// 呼び出し側が書き換えても、キャッシュに響かない。
	ids[0] = "tampered"
	again, err := rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{x, y}, again)
}

func TestRelationCache_LoadsEachKind(t *testing.T) {
	f := newRelFixture(t, "rcK_")
	viewer, a, b, c := f.user("v"), f.user("a"), f.user("b"), f.user("c")
	ch1, ch2 := f.channel("ch1", a), f.channel("ch2", a)
	past := time.Now().Add(-time.Hour)

	mustInsert(t, &model.Muting{ID: f.id("m1"), MuterID: viewer, MuteeID: a})
	mustInsert(t, &model.Muting{ID: f.id("m2"), MuterID: viewer, MuteeID: b, ExpiresAt: &past})
	mustInsert(t, &model.RenoteMuting{ID: f.id("r1"), MuterID: viewer, MuteeID: b})
	mustInsert(t, &model.Blocking{ID: f.id("b1"), BlockerID: c, BlockeeID: viewer})
	mustInsert(t, &model.Blocking{ID: f.id("b2"), BlockerID: viewer, BlockeeID: a})
	mustInsert(t, &model.Following{ID: f.id("f1"), FollowerID: viewer, FolloweeID: a})
	mustInsert(t, &model.Following{ID: f.id("f2"), FollowerID: b, FolloweeID: viewer})
	mustInsert(t, &model.ChannelMuting{ID: f.id("cm1"), UserID: viewer, ChannelID: ch1})
	mustInsert(t, &model.ChannelMuting{ID: f.id("cm2"), UserID: viewer, ChannelID: ch2, ExpiresAt: &past})
	mustInsert(t, &model.ChannelFollowing{ID: f.id("cf1"), FollowerID: viewer, FolloweeID: ch1})
	mustInsert(t, &model.ChannelFollowing{ID: f.id("cf2"), FollowerID: viewer, FolloweeID: ch2})

	rc := NewRelationCache(testDB)
	cases := []struct {
		name string
		get  func(string) ([]string, error)
		want []string
	}{
		{"muting excludes expired", rc.MutedUserIDs, []string{a}},
		{"renote muting", rc.RenoteMutedUserIDs, []string{b}},
		{"blocked by (not blocking)", rc.BlockerIDs, []string{c}},
		{"followees (not followers)", rc.FolloweeIDs, []string{a}},
		{"channel muting excludes expired", rc.MutedChannelIDs, []string{ch1}},
		{"followed channels newest first", rc.FollowedChannelIDs, []string{ch2, ch1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.get(viewer)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	ids, err := rc.MutedUserIDs("")
	require.NoError(t, err)
	assert.Nil(t, ids)
	ids, err = rc.MutedUserIDs("nul\x00id")
	require.NoError(t, err)
	assert.Nil(t, ids)
	ids, err = rc.RenoteMutedUserIDs(a)
	require.NoError(t, err)
	assert.Nil(t, ids, "an empty list is nil")
}

// TestRelationCache_RepositoryWritesInvalidate pins that every write method of
// the relation repositories drops the cached list, so the next read sees the
// change (#3607). Each case primes the cache, writes through a repository
// built with the cache, and reads again.
func TestRelationCache_RepositoryWritesInvalidate(t *testing.T) {
	type env struct {
		f                 *relFixture
		rc                *RelationCache
		viewer, x, y, ch1 string
		muting            MutingRepository
		renote            RenoteMutingRepository
		blocking          BlockingRepository
		following         FollowingRepository
		chMuting          ChannelMutingRepository
		chFollowing       ChannelFollowingRepository
		users             UserRepository
		clock             *time.Time
	}
	cases := []struct {
		name  string
		read  func(e *env) ([]string, error)
		setup func(e *env)
		write func(e *env) error
		want  func(e *env) []string
	}{
		{
			name: "muting Create",
			read: func(e *env) ([]string, error) { return e.rc.MutedUserIDs(e.viewer) },
			write: func(e *env) error {
				return e.muting.Create(&model.Muting{ID: e.f.id("m1"), MuterID: e.viewer, MuteeID: e.x})
			},
			want: func(e *env) []string { return []string{e.x} },
		},
		{
			name:  "muting Delete",
			setup: func(e *env) { mustInsert(t, &model.Muting{ID: e.f.id("m1"), MuterID: e.viewer, MuteeID: e.x}) },
			read:  func(e *env) ([]string, error) { return e.rc.MutedUserIDs(e.viewer) },
			write: func(e *env) error {
				return e.muting.Delete(&model.Muting{ID: e.f.id("m1"), MuterID: e.viewer, MuteeID: e.x})
			},
			want: func(e *env) []string { return nil },
		},
		{
			name:  "muting Delete by id only",
			setup: func(e *env) { mustInsert(t, &model.Muting{ID: e.f.id("m1"), MuterID: e.viewer, MuteeID: e.x}) },
			read:  func(e *env) ([]string, error) { return e.rc.MutedUserIDs(e.viewer) },
			write: func(e *env) error { return e.muting.Delete(&model.Muting{ID: e.f.id("m1")}) },
			want:  func(e *env) []string { return nil },
		},
		{
			// 期限の判定は読むときに行うが、ここでは時計を止めて「キャッシュ上は
			// まだ有効」にし、DeleteExpired の無効化だけを見る。
			name: "muting DeleteExpired",
			setup: func(e *env) {
				exp := e.clock.Add(time.Hour)
				mustInsert(t, &model.Muting{ID: e.f.id("m1"), MuterID: e.viewer, MuteeID: e.x, ExpiresAt: &exp})
			},
			read: func(e *env) ([]string, error) { return e.rc.MutedUserIDs(e.viewer) },
			write: func(e *env) error {
				_, err := e.muting.DeleteExpired(e.clock.Add(2 * time.Hour))
				return err
			},
			want: func(e *env) []string { return nil },
		},
		{
			name: "renote muting Create",
			read: func(e *env) ([]string, error) { return e.rc.RenoteMutedUserIDs(e.viewer) },
			write: func(e *env) error {
				return e.renote.Create(&model.RenoteMuting{ID: e.f.id("r1"), MuterID: e.viewer, MuteeID: e.x})
			},
			want: func(e *env) []string { return []string{e.x} },
		},
		{
			name:  "renote muting Delete by id only",
			setup: func(e *env) { mustInsert(t, &model.RenoteMuting{ID: e.f.id("r1"), MuterID: e.viewer, MuteeID: e.x}) },
			read:  func(e *env) ([]string, error) { return e.rc.RenoteMutedUserIDs(e.viewer) },
			write: func(e *env) error { return e.renote.Delete(&model.RenoteMuting{ID: e.f.id("r1")}) },
			want:  func(e *env) []string { return nil },
		},
		{
			// キャッシュは blockee ごとなので、blocker が書いても blockee の一覧が変わる。
			name: "blocking Create (blocked viewer's list)",
			read: func(e *env) ([]string, error) { return e.rc.BlockerIDs(e.viewer) },
			write: func(e *env) error {
				return e.blocking.Create(&model.Blocking{ID: e.f.id("b1"), BlockerID: e.x, BlockeeID: e.viewer})
			},
			want: func(e *env) []string { return []string{e.x} },
		},
		{
			name:  "blocking Delete by id only",
			setup: func(e *env) { mustInsert(t, &model.Blocking{ID: e.f.id("b1"), BlockerID: e.x, BlockeeID: e.viewer}) },
			read:  func(e *env) ([]string, error) { return e.rc.BlockerIDs(e.viewer) },
			write: func(e *env) error { return e.blocking.Delete(&model.Blocking{ID: e.f.id("b1")}) },
			want:  func(e *env) []string { return nil },
		},
		{
			name: "following Create",
			read: func(e *env) ([]string, error) { return e.rc.FolloweeIDs(e.viewer) },
			write: func(e *env) error {
				return e.following.Create(&model.Following{ID: e.f.id("f1"), FollowerID: e.viewer, FolloweeID: e.x})
			},
			want: func(e *env) []string { return []string{e.x} },
		},
		{
			name:  "following Delete by id only",
			setup: func(e *env) { mustInsert(t, &model.Following{ID: e.f.id("f1"), FollowerID: e.viewer, FolloweeID: e.x}) },
			read:  func(e *env) ([]string, error) { return e.rc.FolloweeIDs(e.viewer) },
			write: func(e *env) error { return e.following.Delete(&model.Following{ID: e.f.id("f1")}) },
			want:  func(e *env) []string { return nil },
		},
		{
			// 消えるのは followee 側の利用者。その人をフォローしていた閲覧者の一覧が変わる。
			name: "following DeleteAllByUser (as followee)",
			setup: func(e *env) {
				mustInsert(t, &model.Following{ID: e.f.id("f1"), FollowerID: e.viewer, FolloweeID: e.x})
				mustInsert(t, &model.Following{ID: e.f.id("f2"), FollowerID: e.viewer, FolloweeID: e.y})
			},
			read: func(e *env) ([]string, error) { return e.rc.FolloweeIDs(e.viewer) },
			write: func(e *env) error {
				_, err := e.following.DeleteAllByUser(e.x)
				return err
			},
			want: func(e *env) []string { return []string{e.y} },
		},
		{
			name:  "following DeleteAllByUser (as follower)",
			setup: func(e *env) { mustInsert(t, &model.Following{ID: e.f.id("f1"), FollowerID: e.viewer, FolloweeID: e.x}) },
			read:  func(e *env) ([]string, error) { return e.rc.FolloweeIDs(e.viewer) },
			write: func(e *env) error {
				_, err := e.following.DeleteAllByUser(e.viewer)
				return err
			},
			want: func(e *env) []string { return nil },
		},
		{
			// 集合は変わらない更新なので、repository を通らずに足した行が見えるかで
			// 捨てたことを確かめる。
			name:  "following UpdateRelation",
			setup: func(e *env) { mustInsert(t, &model.Following{ID: e.f.id("f1"), FollowerID: e.viewer, FolloweeID: e.x}) },
			read:  func(e *env) ([]string, error) { return e.rc.FolloweeIDs(e.viewer) },
			write: func(e *env) error {
				mustInsert(t, &model.Following{ID: e.f.id("f2"), FollowerID: e.viewer, FolloweeID: e.y})
				return e.following.UpdateRelation(e.viewer, e.x, map[string]any{"withReplies": true})
			},
			want: func(e *env) []string { return []string{e.x, e.y} },
		},
		{
			name:  "following UpdateAllByFollower",
			setup: func(e *env) { mustInsert(t, &model.Following{ID: e.f.id("f1"), FollowerID: e.viewer, FolloweeID: e.x}) },
			read:  func(e *env) ([]string, error) { return e.rc.FolloweeIDs(e.viewer) },
			write: func(e *env) error {
				mustInsert(t, &model.Following{ID: e.f.id("f2"), FollowerID: e.viewer, FolloweeID: e.y})
				return e.following.UpdateAllByFollower(e.viewer, map[string]any{"withReplies": true})
			},
			want: func(e *env) []string { return []string{e.x, e.y} },
		},
		{
			name: "channel muting Create",
			read: func(e *env) ([]string, error) { return e.rc.MutedChannelIDs(e.viewer) },
			write: func(e *env) error {
				return e.chMuting.Create(&model.ChannelMuting{ID: e.f.id("cm1"), UserID: e.viewer, ChannelID: e.ch1})
			},
			want: func(e *env) []string { return []string{e.ch1} },
		},
		{
			name: "channel muting Delete",
			setup: func(e *env) {
				mustInsert(t, &model.ChannelMuting{ID: e.f.id("cm1"), UserID: e.viewer, ChannelID: e.ch1})
			},
			read:  func(e *env) ([]string, error) { return e.rc.MutedChannelIDs(e.viewer) },
			write: func(e *env) error { return e.chMuting.Delete(e.viewer, e.ch1) },
			want:  func(e *env) []string { return nil },
		},
		{
			name: "channel muting DeleteExpired",
			setup: func(e *env) {
				exp := e.clock.Add(time.Hour)
				mustInsert(t, &model.ChannelMuting{ID: e.f.id("cm1"), UserID: e.viewer, ChannelID: e.ch1, ExpiresAt: &exp})
			},
			read: func(e *env) ([]string, error) { return e.rc.MutedChannelIDs(e.viewer) },
			write: func(e *env) error {
				_, err := e.chMuting.DeleteExpired(e.clock.Add(2 * time.Hour))
				return err
			},
			want: func(e *env) []string { return nil },
		},
		{
			name: "channel following Create",
			read: func(e *env) ([]string, error) { return e.rc.FollowedChannelIDs(e.viewer) },
			write: func(e *env) error {
				return e.chFollowing.Create(&model.ChannelFollowing{ID: e.f.id("cf1"), FollowerID: e.viewer, FolloweeID: e.ch1})
			},
			want: func(e *env) []string { return []string{e.ch1} },
		},
		{
			name: "channel following Delete by id only",
			setup: func(e *env) {
				mustInsert(t, &model.ChannelFollowing{ID: e.f.id("cf1"), FollowerID: e.viewer, FolloweeID: e.ch1})
			},
			read:  func(e *env) ([]string, error) { return e.rc.FollowedChannelIDs(e.viewer) },
			write: func(e *env) error { return e.chFollowing.Delete(&model.ChannelFollowing{ID: e.f.id("cf1")}) },
			want:  func(e *env) []string { return nil },
		},
		{
			// 物理削除は CASCADE で関係の行を消すので、全体を捨てる。消えた利用者と
			// 関係の無い一覧も捨てたことを、直接足した行が見えるかで確かめる。
			name:  "user HardDeleteUser drops everything",
			setup: func(e *env) { mustInsert(t, &model.Muting{ID: e.f.id("m1"), MuterID: e.viewer, MuteeID: e.x}) },
			read:  func(e *env) ([]string, error) { return e.rc.MutedUserIDs(e.viewer) },
			write: func(e *env) error {
				mustInsert(t, &model.Muting{ID: e.f.id("m2"), MuterID: e.viewer, MuteeID: e.y})
				gone := e.f.user("gone")
				return e.users.HardDeleteUser(gone)
			},
			want: func(e *env) []string { return []string{e.x, e.y} },
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, fmt.Sprintf("rcW%02d_", i))
			clock := time.Now()
			e := &env{f: f, viewer: f.user("v"), x: f.user("x"), y: f.user("y"), clock: &clock}
			e.ch1 = f.channel("ch1", e.x)
			e.rc = NewRelationCacheWithTTL(testDB, time.Hour)
			e.rc.now = func() time.Time { return clock }
			e.muting = NewMutingRepositoryWithRelationCache(testDB, e.rc)
			e.renote = NewRenoteMutingRepositoryWithRelationCache(testDB, e.rc)
			e.blocking = NewBlockingRepositoryWithRelationCache(testDB, e.rc)
			e.following = NewFollowingRepositoryWithRelationCache(testDB, e.rc)
			e.chMuting = NewChannelMutingRepositoryWithRelationCache(testDB, e.rc)
			e.chFollowing = NewChannelFollowingRepositoryWithRelationCache(testDB, e.rc)
			e.users = NewUserRepositoryWithRelationCache(testDB, e.rc)
			if tc.setup != nil {
				tc.setup(e)
			}
			_, err := tc.read(e) // prime
			require.NoError(t, err)
			// 読み込みと書き込みが同じ時刻に見えると、保守的な側に倒れて保存
			// されないことがある。時計を進めて「読み込みは書き込みより前」にする。
			clock = clock.Add(time.Millisecond)
			require.NoError(t, tc.write(e))
			got, err := tc.read(e)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.want(e), got)
		})
	}
}

func TestRelationCache_ExpiryIsCheckedOnRead(t *testing.T) {
	f := newRelFixture(t, "rcE_")
	viewer, x, y := f.user("v"), f.user("x"), f.user("y")
	ch := f.channel("ch", x)
	clock := time.Now()
	rc := NewRelationCacheWithTTL(testDB, time.Hour)
	rc.now = func() time.Time { return clock }

	exp := clock.Add(time.Minute)
	mustInsert(t, &model.Muting{ID: f.id("m1"), MuterID: viewer, MuteeID: x, ExpiresAt: &exp})
	mustInsert(t, &model.Muting{ID: f.id("m2"), MuterID: viewer, MuteeID: y})
	mustInsert(t, &model.ChannelMuting{ID: f.id("cm1"), UserID: viewer, ChannelID: ch, ExpiresAt: &exp})

	ids, err := rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{x, y}, ids)
	chs, err := rc.MutedChannelIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{ch}, chs)

	// TTL の内側でも、期限を過ぎたミュートはその時点から一覧に出ない。
	clock = clock.Add(2 * time.Minute)
	ids, err = rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{y}, ids)
	chs, err = rc.MutedChannelIDs(viewer)
	require.NoError(t, err)
	assert.Nil(t, chs)
}

func TestRelationCache_TTLReloads(t *testing.T) {
	f := newRelFixture(t, "rcT_")
	viewer, x := f.user("v"), f.user("x")
	clock := time.Now()
	rc := NewRelationCacheWithTTL(testDB, time.Minute)
	rc.now = func() time.Time { return clock }

	ids, err := rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.Nil(t, ids)
	mustInsert(t, &model.Following{ID: f.id("f1"), FollowerID: viewer, FolloweeID: x})

	clock = clock.Add(59 * time.Second)
	ids, err = rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.Nil(t, ids, "still cached inside the TTL")

	clock = clock.Add(2 * time.Second)
	ids, err = rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, ids, "reloaded after the TTL")
}

// TestRelationCache_InvalidationDuringLoadIsNotStored pins the read/write race:
// a write that commits and invalidates while a read is between its SELECT and
// its store must not leave the pre-write list cached.
func TestRelationCache_InvalidationDuringLoadIsNotStored(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%v", all), func(t *testing.T) {
			f := newRelFixture(t, fmt.Sprintf("rcR%v_", all))
			viewer, x := f.user("v"), f.user("x")
			rc := NewRelationCacheWithTTL(testDB, time.Hour)

			var fired atomic.Bool
			rc.afterLoad = func() {
				if fired.Swap(true) {
					return
				}
				mustInsert(t, &model.Blocking{ID: f.id("b1"), BlockerID: x, BlockeeID: viewer})
				if all {
					rc.InvalidateAll()
				} else {
					rc.Invalidate(RelationBlockedBy, viewer)
				}
			}
			ids, err := rc.BlockerIDs(viewer)
			require.NoError(t, err)
			assert.Nil(t, ids, "the in-flight read returns what it read")

			ids, err = rc.BlockerIDs(viewer)
			require.NoError(t, err)
			assert.Equal(t, []string{x}, ids, "the stale read was not stored")
		})
	}
}

func TestRelationCache_PublishAndApplyRemote(t *testing.T) {
	f := newRelFixture(t, "rcP_")
	viewer, x, y := f.user("v"), f.user("x"), f.user("y")
	rc := NewRelationCache(testDB)
	var sent []RelationInvalidation
	rc.SetPublisher(func(inv RelationInvalidation) { sent = append(sent, inv) })

	repo := NewMutingRepositoryWithRelationCache(testDB, rc)
	require.NoError(t, repo.Create(&model.Muting{ID: f.id("m1"), MuterID: viewer, MuteeID: x}))
	require.Len(t, sent, 1)
	assert.Equal(t, RelationInvalidation{Kind: RelationMuting, UserIDs: []string{viewer}}, sent[0])

	ids, err := rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, ids)

	// 他のプロセスからの通知は、受けたプロセスで捨てるだけで送り返さない。
	mustInsert(t, &model.Muting{ID: f.id("m2"), MuterID: viewer, MuteeID: y})
	rc.ApplyRemote(RelationInvalidation{Kind: RelationMuting, UserIDs: []string{viewer}})
	assert.Len(t, sent, 1)
	ids, err = rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{x, y}, ids)

	// 知らない kind は全体を捨てる側に倒す。
	testDB.Exec(`DELETE FROM "muting" WHERE id = ?`, f.id("m2"))
	rc.ApplyRemote(RelationInvalidation{Kind: RelationKind(200), UserIDs: []string{"someone"}})
	ids, err = rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, ids)

	rc.InvalidateAll()
	require.Len(t, sent, 2)
	assert.Equal(t, RelationInvalidation{All: true}, sent[1])

	// 書き込みに失敗したら捨てない (送らない)。
	err = repo.Create(&model.Muting{ID: f.id("m1"), MuterID: viewer, MuteeID: x})
	require.Error(t, err)
	assert.Len(t, sent, 2)

	// 空の ID だけなら何も送らない。
	rc.Invalidate(RelationMuting, "", "")
	rc.Invalidate(RelationKind(0), viewer)
	assert.Len(t, sent, 2)
}

func TestRelationCache_Bounds(t *testing.T) {
	f := newRelFixture(t, "rcB_")
	a, b, c, x := f.user("a"), f.user("b"), f.user("c"), f.user("x")
	for _, u := range []string{a, b, c} {
		mustInsert(t, &model.Following{ID: f.id("f" + u[len(u)-1:]), FollowerID: u, FolloweeID: x})
	}
	rc := NewRelationCache(testDB)
	rc.maxEntries = 2
	for _, u := range []string{a, b, c, a} {
		ids, err := rc.FolloweeIDs(u)
		require.NoError(t, err)
		assert.Equal(t, []string{x}, ids)
	}
	kc := &rc.kinds[RelationFollowing]
	assert.LessOrEqual(t, len(kc.entries), 2)

	rc.maxEntries = 100
	rc.maxIDs = 0
	ids, err := rc.FolloweeIDs(b)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, ids)
	assert.Empty(t, kc.entries, "a list over the id budget is not stored")
	assert.Equal(t, 0, kc.ids)
}

func TestRelationCache_NilAndInvalidKind(t *testing.T) {
	var nilCache *RelationCache
	nilCache.Invalidate(RelationMuting, "x")
	nilCache.InvalidateAll()
	nilCache.ApplyRemote(RelationInvalidation{All: true})
	nilCache.SetPublisher(func(RelationInvalidation) {})
	_, err := nilCache.MutedUserIDs("x")
	assert.Error(t, err)

	rc := NewRelationCache(testDB)
	_, err = rc.list(RelationKind(0), "x")
	assert.Error(t, err)
	_, err = rc.list(relationKindEnd, "x")
	assert.Error(t, err)
}

func TestRelationCache_LoadErrorIsReturned(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	rc := NewRelationCache(db)
	for _, get := range []func(string) ([]string, error){
		rc.MutedUserIDs, rc.RenoteMutedUserIDs, rc.BlockerIDs, rc.FolloweeIDs, rc.MutedChannelIDs, rc.FollowedChannelIDs,
	} {
		_, err := get("someone")
		assert.Error(t, err)
	}
}

// TestRelationCache_ReadsThePrimary pins that the lists are reloaded from the
// primary even when dbresolver routes SELECTs to a replica. An invalidation
// arrives right after the primary commits; a reload from a lagging replica
// would cache the pre-mute / pre-block list for the whole TTL (#3607).
//
// レプリカの遅延は、同じ DB の中の空の schema で模す (note_primary_test.go と同じ形)。
func TestRelationCache_ReadsThePrimary(t *testing.T) {
	replicaSchema := "repo_relcache_probe_replica"
	require.NoError(t, testDB.Exec("CREATE SCHEMA IF NOT EXISTS "+replicaSchema).Error)
	t.Cleanup(func() { testDB.Exec("DROP SCHEMA IF EXISTS " + replicaSchema + " CASCADE") })
	var primarySchema string
	require.NoError(t, testDB.Raw("SELECT current_schema()").Scan(&primarySchema).Error)
	for _, table := range []string{"muting", "renote_muting", "blocking", "following", "channel_muting", "channel_following"} {
		require.NoError(t, testDB.Exec(fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s."%s" (LIKE "%s"."%s" INCLUDING ALL)`,
			replicaSchema, table, primarySchema, table)).Error)
	}
	gdb := openWithReplicaSchema(t, primarySchema, replicaSchema)

	f := newRelFixture(t, "rcPr_")
	viewer, x := f.user("v"), f.user("x")
	ch := f.channel("ch", x)
	mustInsert(t, &model.Muting{ID: f.id("m"), MuterID: viewer, MuteeID: x})
	mustInsert(t, &model.RenoteMuting{ID: f.id("r"), MuterID: viewer, MuteeID: x})
	mustInsert(t, &model.Blocking{ID: f.id("b"), BlockerID: x, BlockeeID: viewer})
	mustInsert(t, &model.Following{ID: f.id("f"), FollowerID: viewer, FolloweeID: x})
	mustInsert(t, &model.ChannelMuting{ID: f.id("cm"), UserID: viewer, ChannelID: ch})
	mustInsert(t, &model.ChannelFollowing{ID: f.id("cf"), FollowerID: viewer, FolloweeID: ch})

	// テストの前提: 素の SELECT はレプリカへ振られて空振りする。
	var onReplica []string
	require.NoError(t, gdb.Model(&model.Muting{}).Where(`"muterId" = ?`, viewer).Pluck(`"muteeId"`, &onReplica).Error)
	require.Empty(t, onReplica)

	rc := NewRelationCache(gdb)
	cases := []struct {
		name string
		get  func(string) ([]string, error)
		want string
	}{
		{"muting", rc.MutedUserIDs, x},
		{"renote muting", rc.RenoteMutedUserIDs, x},
		{"blocked by", rc.BlockerIDs, x},
		{"following", rc.FolloweeIDs, x},
		{"channel muting", rc.MutedChannelIDs, ch},
		{"channel following", rc.FollowedChannelIDs, ch},
	}
	for _, tc := range cases {
		got, err := tc.get(viewer)
		require.NoError(t, err, tc.name)
		assert.Equal(t, []string{tc.want}, got, tc.name)
	}

	// 書き込み (RETURNING 付きの DELETE) も primary へ行き、所有者を読める。
	repo := NewFollowingRepositoryWithRelationCache(gdb, rc)
	require.NoError(t, repo.Delete(&model.Following{ID: f.id("f")}))
	got, err := rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestRelationCache_ReturnedSliceIsACopy pins that a caller sorting or
// filtering the returned slice in place cannot corrupt the list shared with
// other viewers' requests. Kinds without expiry return the cached ids
// directly unless copied.
func TestRelationCache_ReturnedSliceIsACopy(t *testing.T) {
	f := newRelFixture(t, "rcCp_")
	viewer, x, y := f.user("v"), f.user("x"), f.user("y")
	mustInsert(t, &model.Following{ID: f.id("f1"), FollowerID: viewer, FolloweeID: x})
	mustInsert(t, &model.Blocking{ID: f.id("b1"), BlockerID: y, BlockeeID: viewer})
	rc := NewRelationCache(testDB)

	for _, get := range []func(string) ([]string, error){rc.FolloweeIDs, rc.BlockerIDs} {
		first, err := get(viewer)
		require.NoError(t, err)
		require.Len(t, first, 1)
		want := first[0]
		first[0] = "tampered"
		again, err := get(viewer)
		require.NoError(t, err)
		assert.Equal(t, []string{want}, again)
	}
}

// TestRelationCache_LongReadIsNotKeptAfterItsInvalidationWasPruned covers a
// read that outlives the TTL while its key's invalidation record is pruned:
// the next read must go to the database again, and the stale result must not
// occupy the cache (#3607).
func TestRelationCache_LongReadIsNotKeptAfterItsInvalidationWasPruned(t *testing.T) {
	f := newRelFixture(t, "rcLg_")
	viewer, x := f.user("v"), f.user("x")
	clock := time.Now()
	const ttl = time.Minute
	rc := NewRelationCacheWithTTL(testDB, ttl)
	rc.now = func() time.Time { return clock }
	rc.maxEntries = 1

	var fired atomic.Bool
	rc.afterLoad = func() {
		if fired.Swap(true) {
			return
		}
		// 読み込みの途中で書き込みが入り、その無効化の記録が、TTL を過ぎてから
		// 別の無効化の刈り込みで消える。
		mustInsert(t, &model.Following{ID: f.id("f1"), FollowerID: viewer, FolloweeID: x})
		clock = clock.Add(time.Millisecond)
		rc.Invalidate(RelationFollowing, viewer)
		clock = clock.Add(ttl + time.Millisecond)
		rc.Invalidate(RelationFollowing, f.id("other1"), f.id("other2"))
	}
	got, err := rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.Nil(t, got, "the in-flight read returns what it read")
	_, recorded := rc.kinds[RelationFollowing].invalidatedAt[viewer]
	require.False(t, recorded, "test premise: the invalidation record was pruned")
	_, kept := rc.kinds[RelationFollowing].entries[viewer]
	assert.False(t, kept, "a read longer than the TTL is not stored")

	got, err = rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, got)
}

func TestRelationCache_LargeInvalidationIsForwardedAsAll(t *testing.T) {
	f := newRelFixture(t, "rcLa_")
	viewer, x := f.user("v"), f.user("x")
	rc := NewRelationCache(testDB)
	var sent []RelationInvalidation
	rc.SetPublisher(func(inv RelationInvalidation) { sent = append(sent, inv) })

	ids := make([]string, 0, maxForwardedInvalidationIDs+1)
	for i := 0; i <= maxForwardedInvalidationIDs; i++ {
		ids = append(ids, fmt.Sprintf("u%d", i))
	}
	rc.Invalidate(RelationFollowing, ids[:maxForwardedInvalidationIDs]...)
	require.Len(t, sent, 1)
	assert.Len(t, sent[0].UserIDs, maxForwardedInvalidationIDs, "up to the cap the ids are sent")

	// 自分のプロセスは精密に捨てる (関係の無い項目は残る)。
	mustInsert(t, &model.Muting{ID: f.id("m0"), MuterID: viewer, MuteeID: x})
	got, err := rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	require.Equal(t, []string{x}, got)
	rc.Invalidate(RelationFollowing, ids...)
	require.Len(t, sent, 2)
	assert.Equal(t, RelationInvalidation{All: true}, sent[1])
	_, kept := rc.kinds[RelationMuting].entries[viewer]
	assert.True(t, kept, "locally only the listed keys are dropped")
}

// TestRelationCache_AgeCountsFromReadStart pins that an entry's age counts
// from the start of the read, so a missed invalidation is bounded by the TTL
// from when the data was read, not from when a slow read finished.
func TestRelationCache_AgeCountsFromReadStart(t *testing.T) {
	f := newRelFixture(t, "rcAg_")
	viewer, x := f.user("v"), f.user("x")
	clock := time.Now()
	start := clock
	const ttl = time.Minute
	rc := NewRelationCacheWithTTL(testDB, ttl)
	rc.now = func() time.Time { return clock }
	var fired atomic.Bool
	rc.afterLoad = func() {
		if !fired.Swap(true) {
			clock = clock.Add(30 * time.Second) // 遅い読み込み
		}
	}
	got, err := rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.Nil(t, got)
	mustInsert(t, &model.Following{ID: f.id("f1"), FollowerID: viewer, FolloweeID: x})

	clock = start.Add(ttl + time.Millisecond)
	got, err = rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, got, "expired one TTL after the read started")
}

// TestRelationCache_DropLocalDoesNotForward pins that DropLocal drops every
// entry of this process without handing an invalidation to the publisher
// (#3723), unlike InvalidateAll.
func TestRelationCache_DropLocalDoesNotForward(t *testing.T) {
	f := newRelFixture(t, "rcD_")
	viewer, x, y := f.user("v"), f.user("x"), f.user("y")
	rc := NewRelationCache(testDB)
	var sent []RelationInvalidation
	rc.SetPublisher(func(inv RelationInvalidation) { sent = append(sent, inv) })

	mustInsert(t, &model.Muting{ID: f.id("m1"), MuterID: viewer, MuteeID: x})
	mustInsert(t, &model.Following{ID: f.id("f1"), FollowerID: viewer, FolloweeID: x})
	ids, err := rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	require.Equal(t, []string{x}, ids)
	ids, err = rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	require.Equal(t, []string{x}, ids)

	// DB を直接書くので、キャッシュは古いまま。
	mustInsert(t, &model.Muting{ID: f.id("m2"), MuterID: viewer, MuteeID: y})
	mustInsert(t, &model.Following{ID: f.id("f2"), FollowerID: viewer, FolloweeID: y})
	ids, err = rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	require.Equal(t, []string{x}, ids, "前提: 捨てるまでは cache が返る")

	rc.DropLocal()
	ids, err = rc.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{x, y}, ids, "DropLocal did not drop the mutings")
	ids, err = rc.FolloweeIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{x, y}, ids, "DropLocal did not drop the followings")
	assert.Empty(t, sent, "DropLocal must not forward the invalidation")

	var nilCache *RelationCache
	assert.NotPanics(t, nilCache.DropLocal)
}
