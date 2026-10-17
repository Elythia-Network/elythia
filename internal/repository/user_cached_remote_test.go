package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// remoteInvUserRepo is a map-backed inner repository. during, when set, runs
// once inside the next read, i.e. while the wrapper's DB read is in flight.
type remoteInvUserRepo struct {
	UserRepository
	users    map[string]*model.User
	profiles map[string]*model.UserProfile
	reads    int
	during   func()
}

func newRemoteInvUserRepo() *remoteInvUserRepo {
	return &remoteInvUserRepo{users: map[string]*model.User{}, profiles: map[string]*model.UserProfile{}}
}

func (r *remoteInvUserRepo) inRead() {
	r.reads++
	if r.during != nil {
		d := r.during
		r.during = nil
		d()
	}
}

func (r *remoteInvUserRepo) FindByID(id string) (*model.User, error) {
	u, ok := r.users[id]
	r.inRead()
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return u, nil
}

func (r *remoteInvUserRepo) FindByURI(uri string) (*model.User, error) {
	var found *model.User
	for _, u := range r.users {
		if u.URI != nil && *u.URI == uri {
			found = u
		}
	}
	r.inRead()
	if found == nil {
		return nil, gorm.ErrRecordNotFound
	}
	return found, nil
}

func (r *remoteInvUserRepo) FindProfileByUserID(id string) (*model.UserProfile, error) {
	p, ok := r.profiles[id]
	r.inRead()
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return p, nil
}

func (r *remoteInvUserRepo) Create(u *model.User) error {
	r.users[u.ID] = u
	return nil
}

func (r *remoteInvUserRepo) UpdateUser(string, map[string]any) error { return nil }

func (r *remoteInvUserRepo) IncrementNotesCount(string, int) error { return nil }

// set replaces the stored row, as another process's write would.
func (r *remoteInvUserRepo) set(u *model.User) { r.users[u.ID] = u }

func strPtr(s string) *string { return &s }

// TestCachedUserRepository_InvalidationHook pins that writes drop this
// process's entries before handing the invalidation to the hook (#3664), and
// that Create names the URI too (another process may hold "no such user").
func TestCachedUserRepository_InvalidationHook(t *testing.T) {
	inner := newRemoteInvUserRepo()
	c := NewCachedUserRepositoryWithTTL(inner, time.Hour)
	var got []UserCacheInvalidation
	c.SetInvalidationHook(func(inv UserCacheInvalidation) {
		c.mu.RLock()
		_, cached := c.users["u1"]
		c.mu.RUnlock()
		assert.False(t, cached, "the local entry is dropped before the hook runs")
		got = append(got, inv)
	})

	require.NoError(t, c.Create(&model.User{ID: "u1", URI: strPtr("https://r.example/u1")}))
	_, err := c.FindByID("u1")
	require.NoError(t, err)
	require.NoError(t, c.IncrementNotesCount("u1", 1))
	_, err = c.FindByID("u1")
	require.NoError(t, err)
	c.Invalidate("u1")

	assert.Equal(t, []UserCacheInvalidation{
		{IDs: []string{"u1"}},
		{URIs: []string{"https://r.example/u1"}},
		{IDs: []string{"u1"}},
		{IDs: []string{"u1"}},
	}, got)
}

// TestCachedUserRepository_ApplyRemoteInvalidation pins what a received
// invalidation drops, and that it never calls the hook.
func TestCachedUserRepository_ApplyRemoteInvalidation(t *testing.T) {
	inner := newRemoteInvUserRepo()
	c := NewCachedUserRepositoryWithTTL(inner, time.Hour)
	c.SetInvalidationHook(func(UserCacheInvalidation) { t.Fatal("hook must not be called") })
	const uri = "https://r.example/u1"
	inner.set(&model.User{ID: "u1", URI: strPtr(uri), NotesCount: 1})
	inner.set(&model.User{ID: "u2", NotesCount: 1})
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Description: strPtr("old")}

	warm := func() {
		_, _ = c.FindByID("u1")
		_, _ = c.FindByID("u2")
		_, _ = c.FindByURI(uri)
		_, _ = c.FindProfileByUserID("u1")
	}
	warm()
	// 他のプロセスが書いた。
	inner.set(&model.User{ID: "u1", URI: strPtr(uri), NotesCount: 2})
	inner.set(&model.User{ID: "u2", NotesCount: 2})
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Description: strPtr("new")}

	c.ApplyRemoteInvalidation(UserCacheInvalidation{IDs: []string{"u1"}})
	u, err := c.FindByID("u1")
	require.NoError(t, err)
	assert.Equal(t, 2, u.NotesCount)
	u, err = c.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, 2, u.NotesCount, "the byURI entry of that user goes too")
	p, err := c.FindProfileByUserID("u1")
	require.NoError(t, err)
	assert.Equal(t, "new", *p.Description)
	u, err = c.FindByID("u2")
	require.NoError(t, err)
	assert.Equal(t, 1, u.NotesCount, "others stay cached")

	// 「その URI の利用者は居ない」を、作られたことの通知で捨てる。
	const newURI = "https://r.example/u3"
	_, err = c.FindByURI(newURI)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	inner.set(&model.User{ID: "u3", URI: strPtr(newURI)})
	c.ApplyRemoteInvalidation(UserCacheInvalidation{URIs: []string{newURI}})
	u, err = c.FindByURI(newURI)
	require.NoError(t, err)
	assert.Equal(t, "u3", u.ID)

	// 全体を捨てる通知は、id・URI・profile のすべてを読み直させる。送信側の
	// 溢れと読めない通知の唯一の受け皿で、profile はパスワードと 2FA を持つ。
	warm()
	inner.set(&model.User{ID: "u1", URI: strPtr(uri), NotesCount: 3})
	inner.set(&model.User{ID: "u2", NotesCount: 3})
	inner.profiles["u1"] = &model.UserProfile{UserID: "u1", Description: strPtr("newer")}
	u, err = c.FindByURI(uri)
	require.NoError(t, err)
	require.Equal(t, 2, u.NotesCount, "cached before All")
	p, err = c.FindProfileByUserID("u1")
	require.NoError(t, err)
	require.Equal(t, "new", *p.Description, "cached before All")

	c.ApplyRemoteInvalidation(UserCacheInvalidation{All: true})
	u, err = c.FindByID("u2")
	require.NoError(t, err)
	assert.Equal(t, 3, u.NotesCount, "All drops the users")
	// FindByID を先に読むと byURI の側は storeUser で埋まらないので、URI は
	// byURI の entry が消えたかどうかだけで決まる。
	u, err = c.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, 3, u.NotesCount, "All drops the byURI entries")
	assert.Equal(t, []string{uri}, c.urisByID["u1"], "the index is rebuilt, not left with stale keys")
	p, err = c.FindProfileByUserID("u1")
	require.NoError(t, err)
	assert.Equal(t, "newer", *p.Description, "All drops the profiles")
}

// TestCachedUserRepository_RemoteInvalidationDuringRead pins that a read in
// flight when another process's invalidation arrives does not store what it
// read: the row it read may predate the write.
func TestCachedUserRepository_RemoteInvalidationDuringRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		inv  UserCacheInvalidation
	}{
		{"by id", UserCacheInvalidation{IDs: []string{"u1"}}},
		{"all", UserCacheInvalidation{All: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := newRemoteInvUserRepo()
			c := NewCachedUserRepositoryWithTTL(inner, time.Hour)
			inner.set(&model.User{ID: "u1", URI: strPtr("https://r.example/u1"), IsSuspended: false})
			inner.during = func() {
				inner.set(&model.User{ID: "u1", URI: strPtr("https://r.example/u1"), IsSuspended: true})
				c.ApplyRemoteInvalidation(tc.inv)
			}
			u, err := c.FindByID("u1")
			require.NoError(t, err)
			assert.False(t, u.IsSuspended, "the in-flight read returns what it read")
			u, err = c.FindByID("u1")
			require.NoError(t, err)
			assert.True(t, u.IsSuspended, "the stale row was not stored")

			// byURI の側も同じ。
			inner.during = func() {
				inner.set(&model.User{ID: "u1", URI: strPtr("https://r.example/u1"), IsSuspended: false})
				c.ApplyRemoteInvalidation(tc.inv)
			}
			c.ApplyRemoteInvalidation(UserCacheInvalidation{All: true})
			u, err = c.FindByURI("https://r.example/u1")
			require.NoError(t, err)
			assert.True(t, u.IsSuspended)
			u, err = c.FindByURI("https://r.example/u1")
			require.NoError(t, err)
			assert.False(t, u.IsSuspended, "the stale byURI row was not stored")

			// 「居ない」を読んでいる間に、他のプロセスで作られた。
			const newURI = "https://r.example/new"
			inner.during = func() {
				inner.set(&model.User{ID: "u9", URI: strPtr(newURI)})
				inv := tc.inv
				if !inv.All {
					inv = UserCacheInvalidation{URIs: []string{newURI}}
				}
				c.ApplyRemoteInvalidation(inv)
			}
			_, err = c.FindByURI(newURI)
			require.ErrorIs(t, err, gorm.ErrRecordNotFound, "the in-flight read returns what it read")
			u, err = c.FindByURI(newURI)
			require.NoError(t, err, "the stale negative entry was not stored")
			assert.Equal(t, "u9", u.ID)
		})
	}
}

// TestCachedUserRepository_URIIndex pins the userID -> byURI keys index that
// replaced the full byURI scan of invalidate: it follows replacement,
// negative entries, URI invalidation and eviction, and a stale index key never
// drops another user's entry.
func TestCachedUserRepository_URIIndex(t *testing.T) {
	inner := newRemoteInvUserRepo()
	c := newCachedUserRepository(inner, time.Hour, 3)
	const uri = "https://r.example/shared"
	inner.set(&model.User{ID: "u1", URI: strPtr(uri)})
	_, err := c.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, []string{uri}, c.urisByID["u1"])

	// 同じ URI が別の利用者を指すようになった (u1 の行が消え、u2 が作られた)。
	delete(inner.users, "u1")
	inner.set(&model.User{ID: "u2", URI: strPtr(uri)})
	c.ApplyRemoteInvalidation(UserCacheInvalidation{URIs: []string{uri}})
	assert.NotContains(t, c.urisByID, "u1")
	_, err = c.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, []string{uri}, c.urisByID["u2"])

	// u1 を捨てても、u2 の entry は残る。
	c.ApplyRemoteInvalidation(UserCacheInvalidation{IDs: []string{"u1"}})
	reads := inner.reads
	_, err = c.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, reads, inner.reads, "u2's entry is still cached")

	// 正の entry を負の entry で置き換えると、索引から外れる。
	delete(inner.users, "u2")
	c.ApplyRemoteInvalidation(UserCacheInvalidation{URIs: []string{uri}})
	_, err = c.FindByURI(uri)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.Empty(t, c.urisByID)

	// 期限切れの entry を掃除すると、索引からも外れる。
	short := newCachedUserRepository(inner, time.Millisecond, 1)
	inner.set(&model.User{ID: "a", URI: strPtr("https://r.example/a")})
	inner.set(&model.User{ID: "b", URI: strPtr("https://r.example/b")})
	_, err = short.FindByURI("https://r.example/a")
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	_, err = short.FindByURI("https://r.example/b")
	require.NoError(t, err)
	assert.NotContains(t, short.urisByID, "a")
	assert.Equal(t, []string{"https://r.example/b"}, short.urisByID["b"])

	// 同じ利用者の複数の URI は、片方だけ外れる。
	multi := newCachedUserRepository(inner, time.Hour, 100)
	inner.users = map[string]*model.User{"m": {ID: "m", URI: strPtr("https://r.example/m1")}}
	_, err = multi.FindByURI("https://r.example/m1")
	require.NoError(t, err)
	inner.users["m"] = &model.User{ID: "m", URI: strPtr("https://r.example/m2")}
	_, err = multi.FindByURI("https://r.example/m2")
	require.NoError(t, err)
	multi.ApplyRemoteInvalidation(UserCacheInvalidation{URIs: []string{"https://r.example/m1"}})
	assert.Equal(t, []string{"https://r.example/m2"}, multi.urisByID["m"])
}
