package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// recordingCache records the order in which DropLocal is called.
type recordingCache struct {
	name    string
	dropped *[]string
}

func (c *recordingCache) DropLocal() { *c.dropped = append(*c.dropped, c.name) }

// funcCache is a cache whose value is a func, a kind that can be a typed nil
// without being a pointer.
type funcCache func()

func (f funcCache) DropLocal() { f() }

// sliceCache is a cache of a type that is not comparable.
type sliceCache []string

func (sliceCache) DropLocal() {}

// boxCache is comparable as a type, but not as a value when held holds a
// value that is not comparable.
type boxCache struct{ held any }

func (boxCache) DropLocal() {}

// TestCacheList_RejectsInvalidRegistrations checks that each invalid
// registration is recorded as an error and keeps the cache out of the list.
func TestCacheList_RejectsInvalidRegistrations(t *testing.T) {
	var dropped []string
	valid := &recordingCache{name: "valid", dropped: &dropped}
	tests := []struct {
		name   string
		add    func(l *cacheList)
		errMsg string
	}{
		{"empty name", func(l *cacheList) { l.add("", cacheFromDB, &recordingCache{dropped: &dropped}) }, "has no name"},
		{"unknown origin", func(l *cacheList) { l.add("x", cacheOrigin("disk"), &recordingCache{dropped: &dropped}) }, "unknown origin"},
		{"nil interface", func(l *cacheList) { l.add("x", cacheFromDB, nil) }, "is nil"},
		{"typed nil", func(l *cacheList) { l.add("x", cacheFromDB, (*recordingCache)(nil)) }, "is nil"},
		{"typed nil func", func(l *cacheList) { l.add("x", cacheFromDB, funcCache(nil)) }, "is nil"},
		{"duplicate name", func(l *cacheList) { l.add("valid", cacheFromDB, &recordingCache{dropped: &dropped}) }, "added twice"},
		{"same value twice", func(l *cacheList) { l.add("other", cacheFromDB, valid) }, "already added"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l cacheList
			l.add("valid", cacheFromDB, valid)
			require.NoError(t, l.err)
			tt.add(&l)
			require.ErrorIs(t, l.err, errCachePart)
			assert.Contains(t, l.err.Error(), tt.errMsg)
			assert.Equal(t, []string{"valid"}, l.names(), "an invalid registration must not be listed")
		})
	}
}

// TestCacheList_AcceptsNonComparableValues checks that a cache of a type that
// is not comparable is added without a panic: the check for the same value
// added twice cannot compare it, so it is skipped for such a value.
func TestCacheList_AcceptsNonComparableValues(t *testing.T) {
	var l cacheList
	require.NotPanics(t, func() {
		l.add("a", cacheFromDB, sliceCache{"x"})
		l.add("b", cacheFromDB, sliceCache{"x"})
		l.add("c", cacheFromDB, boxCache{held: []string{"x"}})
		l.add("d", cacheFromDB, boxCache{held: []string{"x"}})
	})
	require.NoError(t, l.err)
	assert.Equal(t, []string{"a", "b", "c", "d"}, l.names())
}

// TestCacheList_DropAllInOrder checks that dropAll drops every cache once, in
// the order they were added, and that every origin is accepted.
func TestCacheList_DropAllInOrder(t *testing.T) {
	var dropped []string
	var l cacheList
	for _, c := range []struct {
		name   string
		origin cacheOrigin
	}{{"a", cacheFromDB}, {"b", cacheFromConfig}, {"c", cacheFromExternal}, {"d", cacheFromFile}} {
		l.add(c.name, c.origin, &recordingCache{name: c.name, dropped: &dropped})
	}
	require.NoError(t, l.err)
	l.dropAll()
	assert.Equal(t, []string{"a", "b", "c", "d"}, dropped)
}

// TestNewServer_InvalidCacheFailsTheConstruction checks that an invalid
// registration in the cache list fails newServer (constructionError), and that
// the rollback releases the list.
func TestNewServer_InvalidCacheFailsTheConstruction(t *testing.T) {
	db, clients, _, cfg := constructionFixture(t)
	restoreProcessGlobals(t)
	var dropped []string
	var built *Server
	_, err := newServer(cfg, db, clients, nil, noopStorage, func(s *Server) {
		built = s
		s.addCache("cachedMeta", cacheFromDB, &recordingCache{name: "cachedMeta", dropped: &dropped})
	})
	require.ErrorIs(t, err, errCachePart)
	assert.Contains(t, err.Error(), `"cachedMeta" is added twice`)
	// 失敗した世代の後始末は、一覧の参照も手放す (releaseParts)。
	assert.Empty(t, built.caches.parts, "the rollback kept the caches of the failed generation")
}

// metaCachesInOrder are the caches of the meta line (#3721), in the order
// they have to be dropped: the meta row first, then what is built from it.
// It is the lower bound of what TestCaches_MetaLine inspects.
var metaCachesInOrder = []string{"cachedMeta", "metaResponse", "nodeinfo"}

// TestCaches_MetaLine checks on a whole server that the meta line is listed in
// drop order, and that dropping the list makes /api/meta and /nodeinfo read
// the meta row again, without telling the other processes.
//
// 一覧の値が配線で実際に使われている値と同じであることは、応答が変わることで
// 確かめる (別の値を登録していれば、捨てても応答は古いまま)。
func TestCaches_MetaLine(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	srv := h.srv

	var listed []string
	for _, p := range srv.caches.parts {
		for _, name := range metaCachesInOrder {
			if p.name == name {
				listed = append(listed, p.name)
				assert.Equal(t, cacheFromDB, p.origin, "cache %q", p.name)
			}
		}
	}
	require.Equal(t, metaCachesInOrder, listed, "the meta line is not listed in drop order")

	metaName := func() string {
		rec := h.post("/api/meta", "", map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Name string `json:"name"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.Name
	}
	require.Equal(t, "before", metaName())
	require.Equal(t, "before", h.nodeinfoName())

	// 他のプロセスが書いたときと違い、通知は送らない。このプロセスのキャッシュは
	// 古いまま残る。
	require.NoError(t, h.db.Model(&model.Meta{}).Where("1 = 1").Update("name", "dropped").Error)
	require.Equal(t, "before", metaName(), "前提: 捨てるまでは cache が返る")
	require.Equal(t, "before", h.nodeinfoName(), "前提: 捨てるまでは cache が返る")

	// 捨てた後に届いた internal:metaUpdated を記録する。自分の publish も届くので、
	// 捨てたことで通知していれば、目印より前に記録される。
	var mu sync.Mutex
	var received []string
	seen := make(chan struct{})
	var once sync.Once
	const marker = `"caches-meta-line"`
	hook := func(payload []byte) {
		mu.Lock()
		received = append(received, string(payload))
		mu.Unlock()
		if string(payload) == marker {
			once.Do(func() { close(seen) })
		}
	}
	srv.metaUpdatedHandled.Store(&hook)
	t.Cleanup(func() { srv.metaUpdatedHandled.Store(nil) })

	srv.caches.dropAll()
	// 目印の通知を受けると購読がキャッシュを捨てるので、読み直したことは目印より
	// 前に確かめる。
	assert.Equal(t, "dropped", metaName(), "/api/meta was not read again")
	assert.Equal(t, "dropped", h.nodeinfoName(), "/nodeinfo was not read again")

	require.NoError(t, h.rdb.Publish(context.Background(), "internal:metaUpdated", marker).Err())
	select {
	case <-seen:
	case <-time.After(10 * time.Second):
		t.Fatal("this process did not handle internal:metaUpdated")
	}
	mu.Lock()
	assert.Equal(t, []string{marker}, received, "dropping the caches told the other processes")
	mu.Unlock()
}

// userLineCaches are the caches of the user and relation line (#3723). They
// are built in newServer and do not depend on each other, so their order is
// not checked.
var userLineCaches = []string{"userCache", "relationCache", "authTokens"}

// TestCaches_UserLine checks on a whole server that the user and relation
// line is listed with the values the server uses, and that dropping the list
// makes the user cache, the relation cache and the auth token cache read the
// database again, without telling the other processes.
//
// 利用者と関係のキャッシュは Server のフィールド (配線に渡しているもの) から読み、
// token のキャッシュは API を通して確かめる。
//
// 利用者のキャッシュの通知は、世代の worker (userCacheLink) が待ち行列から非同期に
// 送る。worker を起動しておき、捨てた後に世代を止めてから (止めるときに待ち行列を
// 送り切る) 目印を送るので、捨てたことで積まれた通知は目印より前に届く。
func TestCaches_UserLine(t *testing.T) {
	h := newMetaRuntimeHarnessWith(t, metaRuntimeOptions{onlyServer: "1", startBackground: true})
	srv := h.srv

	want := map[string]localCache{"userCache": srv.userCache, "relationCache": srv.relationCache, "authTokens": srv.auth}
	seen := map[string]bool{}
	for _, p := range srv.caches.parts {
		if v, ok := want[p.name]; ok {
			seen[p.name] = true
			assert.Equal(t, cacheFromDB, p.origin, "cache %q", p.name)
			assert.Same(t, v, p.cache, "cache %q is not the value the server uses", p.name)
		}
	}
	for _, name := range userLineCaches {
		assert.True(t, seen[name], "cache %q is not listed", name)
	}

	const token = "metartuser000023"
	const userID = "metart-user-23"
	h.createUser(userID, "metart_user23", token, false)
	muteID := h.idGen.Generate(time.Now())
	t.Cleanup(func() { h.db.Where("id = ?", muteID).Delete(&model.Muting{}) })

	user, err := srv.userCache.FindByID(userID)
	require.NoError(t, err)
	require.Nil(t, user.Name)
	muted, err := srv.relationCache.MutedUserIDs(userID)
	require.NoError(t, err)
	require.Empty(t, muted)
	require.Equal(t, http.StatusOK, h.post("/api/i", token, map[string]any{}).Code)

	// 他のプロセスが書いたときと違い、通知は送らない。このプロセスのキャッシュは
	// 古いまま残る。
	require.NoError(t, h.db.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]any{"name": "dropped", "isSuspended": true}).Error)
	require.NoError(t, h.db.Create(&model.Muting{ID: muteID, MuterID: userID, MuteeID: h.rootID}).Error)
	user, err = srv.userCache.FindByID(userID)
	require.NoError(t, err)
	require.Nil(t, user.Name, "前提: 捨てるまでは cache が返る")
	muted, err = srv.relationCache.MutedUserIDs(userID)
	require.NoError(t, err)
	require.Empty(t, muted, "前提: 捨てるまでは cache が返る")
	require.Equal(t, http.StatusOK, h.post("/api/i", token, map[string]any{}).Code, "前提: 捨てるまでは cache が返る")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	user, err = srv.userCache.FindByID(userID)
	require.NoError(t, err)
	if assert.NotNil(t, user.Name, "the user cache was not dropped") {
		assert.Equal(t, "dropped", *user.Name)
	}
	muted, err = srv.relationCache.MutedUserIDs(userID)
	require.NoError(t, err)
	assert.Equal(t, []string{h.rootID}, muted, "the relation cache was not dropped")
	assert.Equal(t, http.StatusForbidden, h.post("/api/i", token, map[string]any{}).Code, "the auth token cache was not dropped")

	require.NoError(t, srv.StopBackground(context.Background()))
	assert.Empty(t, published("caches-user-line"), "dropping the caches told the other processes")
}

// watchInternal subscribes to every internal: channel and returns a function
// that publishes a marker and returns the channels published on before it, in
// order. 目印より前に何か届けば、それは購読を始めてから目印までの間に他の
// プロセスへ通知したもの。
//
// 非同期に送る通知 (利用者のキャッシュの待ち行列など) は、目印を送る前に送り
// 切らせておくこと。
func watchInternal(t *testing.T, h *metaRuntimeHarness) func(marker string) []string {
	t.Helper()
	sub := h.rdb.PSubscribe(context.Background(), "internal:*")
	t.Cleanup(func() { _ = sub.Close() })
	_, err := sub.Receive(context.Background())
	require.NoError(t, err)
	return func(marker string) []string {
		t.Helper()
		require.NoError(t, h.rdb.Publish(context.Background(), "internal:"+marker, "x").Err())
		ch := sub.Channel()
		var got []string
		timeout := time.After(10 * time.Second)
		for {
			select {
			case msg := <-ch:
				if msg.Channel == "internal:"+marker {
					return got
				}
				got = append(got, msg.Channel)
			case <-timeout:
				t.Fatalf("the marker did not arrive; got %v", got)
			}
		}
	}
}

// TestCaches_RoleLine checks on a whole server that the role line is listed
// with the role service the server uses, and that dropping the list makes the
// role checks read the database again, without telling the other processes
// (#3725).
//
// ロールのキャッシュは Server のフィールド (配線に渡しているもの) から読むのに
// 加えて、管理 API の認可 (RequireAdmin) を通して確かめる。
func TestCaches_RoleLine(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	srv := h.srv

	// 条件付きロールは利用者のキャッシュから評価するので、roles は userCache の後に捨てる。
	at := map[string]int{}
	for i, p := range srv.caches.parts {
		at[p.name] = i
		if p.name == "roles" {
			assert.Equal(t, cacheFromDB, p.origin)
			assert.Same(t, srv.roleService, p.cache, "cache %q is not the role service the server uses", p.name)
		}
	}
	rolesAt, ok := at["roles"]
	require.True(t, ok, `cache "roles" is not listed`)
	userAt, ok := at["userCache"]
	require.True(t, ok, `cache "userCache" is not listed`)
	assert.Greater(t, rolesAt, userAt, "roles is dropped before the user cache it is evaluated from")

	const token = "metartuser000025"
	const userID = "metart-user-25"
	h.createUser(userID, "metart_user25", token, false)
	roleID := h.idGen.Generate(time.Now())
	assignID := h.idGen.Generate(time.Now())
	// 管理者のロールが残ると、後のテストに管理者が混ざる。
	t.Cleanup(func() {
		require.NoError(t, h.db.Where("id = ?", assignID).Delete(&model.RoleAssignment{}).Error)
		require.NoError(t, h.db.Where("id = ?", roleID).Delete(&model.Role{}).Error)
	})

	require.False(t, srv.roleService.IsAdministrator(userID))
	require.NotEqual(t, http.StatusOK, h.post("/api/admin/meta", token, map[string]any{}).Code)

	// 他のプロセスが書いたときと違い、通知は送らない。このプロセスのキャッシュは
	// 古いまま残る。
	now := time.Now()
	require.NoError(t, h.db.Create(&model.Role{ID: roleID, Name: "caches-role-line", UpdatedAt: now, LastUsedAt: now, IsAdministrator: true}).Error)
	require.NoError(t, h.db.Create(&model.RoleAssignment{ID: assignID, UserID: userID, RoleID: roleID}).Error)
	require.False(t, srv.roleService.IsAdministrator(userID), "前提: 捨てるまでは cache が返る")
	require.NotEqual(t, http.StatusOK, h.post("/api/admin/meta", token, map[string]any{}).Code, "前提: 捨てるまでは cache が返る")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	assert.True(t, srv.roleService.IsAdministrator(userID), "the role cache was not dropped")
	rec := h.post("/api/admin/meta", token, map[string]any{})
	assert.Equal(t, http.StatusOK, rec.Code, "the admin API still sees the old roles: %s", rec.Body.String())
	assert.Empty(t, published("caches-role-line"), "dropping the caches told the other processes")
}
