package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	corecaptcha "github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/core/fedrule"
	corerelay "github.com/elythia-network/elythia/internal/core/relay"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
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
// they have to be dropped: the meta row first, then what is built from it
// (the captcha providers and the reaction buffering switch are read again
// from it, #3777). It is the lower bound of what TestCaches_MetaLine
// inspects.
var metaCachesInOrder = []string{"cachedMeta", "metaResponse", "nodeinfo", "captcha", "reactionBuffering"}

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

// TestCaches_EmojiLine checks on a whole server that the emoji and decoration
// line is listed with the values the server uses, the emoji decorations after
// the emojis they are built from, and that dropping the list makes the three
// read the database again, without telling the other processes (#3727).
//
// 絵文字の通知は世代の worker (emojiInvalidationQueue) が待ち行列から非同期に送るので、
// TestCaches_UserLine と同じく世代を起動しておき、止めてから目印を送る。
func TestCaches_EmojiLine(t *testing.T) {
	h := newMetaRuntimeHarnessWith(t, metaRuntimeOptions{onlyServer: "1", startBackground: true})
	srv := h.srv
	caches := srv.crossProcessCaches

	want := map[string]localCache{"emojis": caches.emojis, "emojiDecorations": caches.emojiDecorations, "avatarDecorations": caches.decorations}
	at := map[string]int{}
	for i, p := range srv.caches.parts {
		if v, ok := want[p.name]; ok {
			at[p.name] = i
			assert.Equal(t, cacheFromDB, p.origin, "cache %q", p.name)
			assert.Same(t, v, p.cache, "cache %q is not the value the server uses", p.name)
		}
	}
	for name := range want {
		require.Contains(t, at, name, "cache %q is not listed", name)
	}
	// 絵文字由来のデコレーションは絵文字のキャッシュの一覧から作り直すので、後に捨てる。
	assert.Greater(t, at["emojiDecorations"], at["emojis"], "emojiDecorations is dropped before the emojis it is built from")

	emojiID := h.idGen.Generate(time.Now())
	decorationID := h.idGen.Generate(time.Now())
	// 名前は ID から作る。(name, host) の一意制約は host が NULL の行どうしを区別しない
	// ので、落ちた回の行が残っていても次の回の前提を崩さない。
	emojiName := "caches_" + emojiID
	t.Cleanup(func() {
		require.NoError(t, h.db.Where("id = ?", emojiID).Delete(&model.Emoji{}).Error)
		require.NoError(t, h.db.Where("id = ?", decorationID).Delete(&model.AvatarDecoration{}).Error)
	})
	key := []model.EmojiKey{{Name: emojiName}}
	hasLocal := func() bool {
		list, err := caches.emojis.ListLocal()
		require.NoError(t, err)
		for _, e := range list {
			if e.ID == emojiID {
				return true
			}
		}
		return false
	}
	byKey := func() int {
		rows, err := caches.emojis.FindManyByKeys(key)
		require.NoError(t, err)
		return len(rows)
	}
	emojiDecoration := func() bool {
		_, _, ok := caches.emojiDecorations.LookupEmojiDecoration(emojiID)
		return ok
	}
	decoration := func() bool {
		_, ok := caches.decorations.LookupURL(decorationID)
		return ok
	}
	require.False(t, hasLocal())
	require.Zero(t, byKey())
	require.False(t, emojiDecoration())
	require.False(t, decoration())

	// 他のプロセスが書いたときと違い、通知は送らない。このプロセスのキャッシュは
	// 古いまま残る。
	now := time.Now()
	require.NoError(t, h.db.Create(&model.Emoji{ID: emojiID, Name: emojiName, OriginalURL: "https://example.test/e.png", PublicURL: "https://example.test/e.png", UpdatedAt: &now}).Error)
	require.NoError(t, h.db.Create(&model.AvatarDecoration{ID: decorationID, URL: "https://example.test/d.png", Name: "caches-emoji-line"}).Error)
	require.False(t, hasLocal(), "前提: 捨てるまでは cache が返る")
	require.Zero(t, byKey(), "前提: 捨てるまでは cache が返る")
	require.False(t, emojiDecoration(), "前提: 捨てるまでは cache が返る")
	require.False(t, decoration(), "前提: 捨てるまでは cache が返る")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	assert.True(t, hasLocal(), "the local emoji list was not dropped")
	assert.Equal(t, 1, byKey(), "the (name, host) emoji entries were not dropped")
	assert.True(t, emojiDecoration(), "the emoji decorations were not dropped")
	assert.True(t, decoration(), "the decoration catalog was not dropped")

	require.NoError(t, srv.StopBackground(context.Background()))
	assert.Empty(t, published("caches-emoji-line"), "dropping the caches told the other processes")
}

// TestCaches_FederationLine checks on a whole server that the instance
// decisions, the relay list and the federation rules are listed with the
// values the server uses, and that dropping the list makes them read the
// database again, without telling the other processes (#3729).
func TestCaches_FederationLine(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	srv := h.srv
	caches := srv.crossProcessCaches
	// 停止の判定は、連合のモードが「すべて」でないと停止を見る前に配送を止める。
	var federation string
	require.NoError(t, h.db.Model(&model.Meta{}).Select("federation").Take(&federation).Error)
	t.Cleanup(func() {
		require.NoError(t, h.db.Model(&model.Meta{}).Where("1 = 1").Update("federation", federation).Error)
	})
	h.updateMetaElsewhere(map[string]any{"federation": "all"})

	want := map[string]localCache{"instances": caches.instances, "relays": caches.relays, "fedRules": caches.fedRules}
	seen := map[string]bool{}
	for _, p := range srv.caches.parts {
		if v, ok := want[p.name]; ok {
			seen[p.name] = true
			assert.Equal(t, cacheFromDB, p.origin, "cache %q", p.name)
			assert.Same(t, v, p.cache, "cache %q is not the value the server uses", p.name)
		}
	}
	for name := range want {
		require.True(t, seen[name], "cache %q is not listed", name)
	}

	// 名前は ID から作る。落ちた回の行が残っていても、次の回の前提を崩さない。
	suffix := h.idGen.Generate(time.Now())
	host := "fed-" + suffix + ".example"
	instanceID := h.idGen.Generate(time.Now())
	relayA, relayB := h.idGen.Generate(time.Now()), h.idGen.Generate(time.Now())
	inboxA, inboxB := "https://relay-a-"+suffix+".example/inbox", "https://relay-b-"+suffix+".example/inbox"
	ruleID := h.idGen.Generate(time.Now())
	t.Cleanup(func() {
		require.NoError(t, h.db.Where("id = ?", instanceID).Delete(&model.Instance{}).Error)
		require.NoError(t, h.db.Where("id IN ?", []string{relayA, relayB}).Delete(&model.Relay{}).Error)
		require.NoError(t, h.db.Where("id = ?", ruleID).Delete(&model.FederationRule{}).Error)
	})
	now := time.Now()
	require.NoError(t, h.db.Create(&model.Instance{ID: instanceID, Host: host, FirstRetrievedAt: now, SuspensionState: model.SuspensionStateNone}).Error)
	// 承認済みの relay を 1 つ置いてから一覧を読ませる (空の一覧は積まれないことがあり、
	// 捨てなくても読み直して検査が空振りする)。
	require.NoError(t, h.db.Create(&model.Relay{ID: relayA, Inbox: inboxA, Status: corerelay.StatusAccepted}).Error)

	relayActor := func(inbox string) bool {
		return caches.relays.IsRelayActor(&model.User{ID: "relay-actor", Inbox: &inbox})
	}
	rejects := func() bool {
		return caches.fedRules.EvaluateNote(fedrule.NoteInput{Host: host}).Reject
	}
	require.False(t, caches.instances.ShouldSkipDelivery(host))
	require.True(t, relayActor(inboxA))
	require.False(t, relayActor(inboxB))
	require.False(t, rejects())

	// 他のプロセスが書いたときと違い、通知は送らない。このプロセスのキャッシュは
	// 古いまま残る。
	require.NoError(t, h.db.Model(&model.Instance{}).Where("id = ?", instanceID).Update("suspensionState", model.SuspensionStateManuallySuspended).Error)
	require.NoError(t, h.db.Create(&model.Relay{ID: relayB, Inbox: inboxB, Status: corerelay.StatusAccepted}).Error)
	require.NoError(t, h.db.Create(&model.FederationRule{ID: ruleID, CreatedAt: now, UpdatedAt: now, Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetNote, Hosts: model.StringArray{host}, Reject: true}).Error)
	require.False(t, caches.instances.ShouldSkipDelivery(host), "前提: 捨てるまでは cache が返る")
	require.False(t, relayActor(inboxB), "前提: 捨てるまでは cache が返る")
	require.False(t, rejects(), "前提: 捨てるまでは cache が返る")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	assert.True(t, caches.instances.ShouldSkipDelivery(host), "the suspend decisions were not dropped")
	assert.True(t, relayActor(inboxB), "the relay list was not dropped")
	assert.True(t, rejects(), "the federation rules were not reloaded")
	assert.Empty(t, published("caches-federation-line"), "dropping the caches told the other processes")
}

// TestCaches_FederationKeysLine checks on a whole server that the rest of the
// federation line (#3732) is listed with the values the server uses, that the
// instance actor key is one value shared by the AP fetcher and the plugin
// peers, and that dropping the list makes the public key of a remote actor and
// the instance actor key read the database again, without telling the other
// processes.
//
// 配送の Ed25519 の対応と署名鍵は、配送の job を流さないと観測できないので、
// 各パッケージのテスト (TestDeliverService_DropLocal・TestDeliverProcessor_DropLocal)
// に任せ、ここでは一覧にあることと値が同じことだけを見る。
func TestCaches_FederationKeysLine(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	srv := h.srv
	caches := srv.crossProcessCaches

	want := map[string]localCache{
		"deliverEd25519": caches.deliver, "federationPublicKeys": caches.resolver,
		"deliverSigningKeys": caches.deliverProcessor, "instanceActorSigner": caches.instanceSigner,
	}
	at := map[string]int{}
	for i, p := range srv.caches.parts {
		at[p.name] = i
		if v, ok := want[p.name]; ok {
			assert.Equal(t, cacheFromDB, p.origin, "cache %q", p.name)
			assert.Same(t, v, p.cache, "cache %q is not the value the server uses", p.name)
		}
	}
	for name := range want {
		require.Contains(t, at, name, "cache %q is not listed", name)
	}
	// instance actor の利用者は CachedUserRepository から引くので、userCache の後に捨てる。
	assert.Greater(t, at["instanceActorSigner"], at["userCache"], "instanceActorSigner is dropped before the user cache it reads from")
	// 秘密鍵のキャッシュは 1 つで、AP の取得とプラグインの peer が同じ値を使う。
	assert.Same(t, caches.instanceSigner, caches.fetcherSigner, "the AP fetcher does not use the listed instance actor signer")
	assert.Same(t, caches.instanceSigner, srv.peerDeps.signer, "the plugin peers do not use the listed instance actor signer")

	// 相手の公開鍵: 名前は ID から作る (落ちた回の行が残っていても前提を崩さない)。
	remoteID := h.idGen.Generate(time.Now())
	host := "keys-" + remoteID + ".example"
	t.Cleanup(func() {
		require.NoError(t, h.db.Where(`"userId" = ?`, remoteID).Delete(&model.UserPublickey{}).Error)
		require.NoError(t, h.db.Where("id = ?", remoteID).Delete(&model.User{}).Error)
	})
	require.NoError(t, h.db.Create(&model.User{ID: remoteID, Username: "keys", UsernameLower: "keys", Host: &host}).Error)
	require.NoError(t, h.db.Create(&model.UserPublickey{UserID: remoteID, KeyID: "https://" + host + "/users/keys#main-key", KeyPEM: "OLD"}).Error)
	publicKey := func() string {
		pem, err := caches.resolver.PublicKeyForActor(remoteID)
		require.NoError(t, err)
		return pem
	}

	// instance actor の秘密鍵: 読み込みで system account と鍵が作られる。
	signer := func() string {
		key, err := caches.instanceSigner.Signer()
		require.NoError(t, err)
		return key.PrivatePEM
	}
	before := signer()
	var actor model.UserKeypair
	require.NoError(t, h.db.Where(`"privateKey" = ?`, before).Take(&actor).Error)
	newPEM := testRSAPrivateKeyPEM(t)
	t.Cleanup(func() {
		require.NoError(t, h.db.Model(&model.UserKeypair{}).Where(`"userId" = ?`, actor.UserID).Update("privateKey", before).Error)
	})

	require.Equal(t, "OLD", publicKey())
	// 他のプロセスが書いたときと違い、通知は送らない。このプロセスのキャッシュは
	// 古いまま残る。
	require.NoError(t, h.db.Model(&model.UserPublickey{}).Where(`"userId" = ?`, remoteID).Update("keyPem", "NEW").Error)
	require.NoError(t, h.db.Model(&model.UserKeypair{}).Where(`"userId" = ?`, actor.UserID).Update("privateKey", newPEM).Error)
	require.Equal(t, "OLD", publicKey(), "前提: 捨てるまでは cache が返る")
	require.Equal(t, before, signer(), "前提: 捨てるまでは cache が返る")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	assert.Equal(t, "NEW", publicKey(), "the public keys were not dropped")
	assert.Equal(t, newPEM, signer(), "the instance actor key was not dropped")
	assert.Empty(t, published("caches-federation-keys-line"), "dropping the caches told the other processes")
}

// testRSAPrivateKeyPEM returns a fresh RSA private key in PKCS#8 PEM.
func testRSAPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// TestInstanceActorSigner_DropLocal pins that DropLocal drops both the cached
// key and the backoff after a failed load (#3732): a failure just before a
// restore would otherwise keep the fetches unsigned for the backoff.
func TestInstanceActorSigner_DropLocal(t *testing.T) {
	s := &instanceActorSigner{cachedKey: &activitypub.PrivateKey{KeyID: "k"}, lastFailure: time.Now()}
	s.DropLocal()
	assert.Nil(t, s.cachedKey, "DropLocal did not drop the key")
	assert.True(t, s.lastFailure.IsZero(), "DropLocal did not drop the load backoff")
}

// notesLineCaches are the caches of the notes, timelines and notifications
// line (#3736). notePublisher と timelineJSON は pack した利用者・ロール・
// 絵文字・デコレーション・インスタンスを含むので、それらの後に並ぶ。
var notesLineCaches = []string{"antennas", "pushSubscriptions", "featuredNotes", "notePublisher", "timelineJSON"}

// notesLinePackedFrom are the caches that notePublisher and timelineJSON pack from.
var notesLinePackedFrom = []string{"userCache", "relationCache", "roles", "emojis", "emojiDecorations", "avatarDecorations", "instances"}

func TestCaches_NotesLine(t *testing.T) {
	h := newMetaRuntimeHarness(t, func(s *Server) {
		s.config.EnableTimelineCache = true
		s.config.TimelineCacheTTLSeconds = 3600
	})
	srv := h.srv
	caches := srv.crossProcessCaches
	require.NotNil(t, caches.notes.TimelineJSONCache())

	want := map[string]localCache{
		"antennas": caches.antennas, "pushSubscriptions": caches.pushSubscriptions,
		"featuredNotes": caches.notes.FeaturedRankingCache(), "notePublisher": caches.notePublisher,
		"timelineJSON": caches.notes.TimelineJSONCache(),
	}
	at := map[string]int{}
	for i, p := range srv.caches.parts {
		at[p.name] = i
		if v, ok := want[p.name]; ok {
			assert.Equal(t, cacheFromDB, p.origin, "cache %q", p.name)
			assert.Same(t, v, p.cache, "cache %q is not the value the server uses", p.name)
		}
	}
	for _, name := range notesLineCaches {
		require.Contains(t, at, name, "cache %q is not listed", name)
	}
	for _, packed := range []string{"notePublisher", "timelineJSON"} {
		for _, from := range notesLinePackedFrom {
			require.Contains(t, at, from)
			assert.Greater(t, at[packed], at[from], "%q must be dropped after %q", packed, from)
		}
	}

	const token = "metartuser000036"
	const userID = "metart-user-36"
	h.createUser(userID, "metart_user36", token, false)
	antennaID, subID := h.idGen.Generate(time.Now()), h.idGen.Generate(time.Now())
	t.Cleanup(func() {
		require.NoError(t, h.db.Where("id = ?", antennaID).Delete(&model.Antenna{}).Error)
		require.NoError(t, h.db.Where("id = ?", subID).Delete(&model.SwSubscription{}).Error)
	})

	hasAntenna := func() bool {
		rows, err := caches.antennas.ListAllActive()
		require.NoError(t, err)
		for _, a := range rows {
			if a.ID == antennaID {
				return true
			}
		}
		return false
	}
	subscriptions := func() int {
		subs, err := caches.pushSubscriptions.Get(context.Background(), userID)
		require.NoError(t, err)
		return len(subs)
	}
	timeline := func() []string {
		rec := h.post("/api/notes/local-timeline", token, map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var notes []struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &notes))
		ids := make([]string, len(notes))
		for i, n := range notes {
			ids[i] = n.ID
		}
		return ids
	}
	require.False(t, hasAntenna())
	require.Zero(t, subscriptions())
	before := timeline()

	// 他のプロセスが書いたときと違い、通知は送らない。このプロセスのキャッシュは
	// 古いまま残る。
	require.NoError(t, h.db.Create(&model.Antenna{ID: antennaID, LastUsedAt: time.Now(), UserID: userID, Name: "dropped", Src: model.AntennaSourceAll, IsActive: true}).Error)
	require.NoError(t, h.db.Create(&model.SwSubscription{ID: subID, UserID: userID, Endpoint: "https://push.example/" + subID, Auth: "auth", PublicKey: "key"}).Error)
	noteID := h.createNote(userID, "dropped")
	require.False(t, hasAntenna(), "前提: 捨てるまでは cache が返る")
	require.Zero(t, subscriptions(), "前提: 捨てるまでは cache が返る")
	require.Equal(t, before, timeline(), "前提: 捨てるまでは cache が返る")
	// push 購読の Redis の層はプロセスの間で共有していて、DropLocal は触らない。
	// DB を戻したときと同じく、Redis の層は先に消しておく (CleanRedisAfterRestore)。
	require.NoError(t, h.rdb.Del(context.Background(), "userSwSubscriptions:"+userID).Err())
	require.Zero(t, subscriptions(), "前提: 捨てるまではメモリの層が返る")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	assert.True(t, hasAntenna(), "the antennas were not dropped")
	assert.Equal(t, 1, subscriptions(), "the push subscriptions were not dropped")
	assert.Contains(t, timeline(), noteID, "the timeline responses were not dropped")
	assert.Empty(t, published("caches-notes-line"), "dropping the caches told the other processes")
}

// The timeline response cache is opt-in, and is listed only when it is on.
func TestCaches_TimelineJSONOnlyWhenEnabled(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	require.Nil(t, h.srv.crossProcessCaches.notes.TimelineJSONCache())
	for _, p := range h.srv.caches.parts {
		assert.NotEqual(t, "timelineJSON", p.name)
	}
}

// adminStatsLineCaches are the caches of the admin and statistics line (#3738).
var adminStatsLineCaches = []string{"driveUsage", "dbHealth", "statsCounts", "ipLogWindow"}

func TestCaches_AdminStatsLine(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	srv := h.srv
	caches := srv.crossProcessCaches

	want := map[string]localCache{
		"driveUsage": caches.driveUsage, "dbHealth": caches.dbHealth,
		"statsCounts": caches.stats, "ipLogWindow": caches.ipLog,
	}
	at := map[string]int{}
	for i, p := range srv.caches.parts {
		at[p.name] = i
		if v, ok := want[p.name]; ok {
			assert.Equal(t, cacheFromDB, p.origin, "cache %q", p.name)
			assert.Same(t, v, p.cache, "cache %q is not the value the server uses", p.name)
		}
	}
	// 4 つの間に順の制約は無い。投稿・タイムライン・通知の系統より後に、それらの
	// 元になるキャッシュを置かないため、その前に並ぶ。
	for _, name := range adminStatsLineCaches {
		require.Contains(t, at, name, "cache %q is not listed", name)
		require.Contains(t, at, "antennas")
		assert.Less(t, at[name], at["antennas"], "%q must be listed before the notes line", name)
	}

	const userID = "metart-user-38"
	const token = "metartuser000038"
	// httptest.NewRequest が付ける接続元の IP。
	const ip = "192.0.2.1"
	h.createUser(userID, "metart_user38", token, false)
	instanceID := h.idGen.Generate(time.Now())
	t.Cleanup(func() {
		require.NoError(t, h.db.Where("id = ?", instanceID).Delete(&model.Instance{}).Error)
		// 管理画面の API を呼ぶので、root の行も書かれる。user_ip は利用者を消しても
		// 残る (FK が無い) ので消す。書き込みは goroutine で走るので、捨てる前と後の
		// 2 回分が届いてから消す。途中で失敗したときは待たずに消す (次に流したときは
		// 始めに消す)。
		if !t.Failed() {
			assert.Eventually(t, func() bool {
				var n int
				require.NoError(t, h.db.Model(&model.UserIP{}).Where(`"userId" = ? AND ip = ?`, h.rootID, ip).
					Select(`COALESCE(MAX("observationCount"), 0)`).Scan(&n).Error)
				return n == 2
			}, 5*time.Second, 10*time.Millisecond)
		}
		require.NoError(t, h.db.Where(`"userId" IN ?`, []string{userID, h.rootID}).Delete(&model.UserIP{}).Error)
	})
	require.NoError(t, h.db.Where(`"userId" IN ?`, []string{userID, h.rootID}).Delete(&model.UserIP{}).Error)
	h.updateMetaHere(map[string]any{"enableIpLogging": true})

	instances := func() float64 {
		rec := h.post("/api/stats", "", map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got["instances"].(float64)
	}
	// 管理画面の 2 つは API を通して読む。一覧に登録した値と、管理画面が使う値が
	// 同じであることも確かめるため。
	driveUsageCached := func() bool {
		rec := h.post("/api/admin/drive/usage", h.rootToken, map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got struct {
			Cached bool `json:"cached"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got.Cached
	}
	dbHealthAt := func() string {
		rec := h.post("/api/admin/database-health", h.rootToken, map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got struct {
			GeneratedAt string `json:"generatedAt"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.NotEmpty(t, got.GeneratedAt)
		return got.GeneratedAt
	}
	// IP の記録は API を通して起こす。一覧に登録した値と、認証の middleware が
	// 使う値が同じであることも確かめるため。
	request := func() {
		rec := h.post("/api/i", token, map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	ipRows := func() int64 {
		var n int64
		require.NoError(t, h.db.Model(&model.UserIP{}).Where(`"userId" = ? AND ip = ?`, userID, ip).Count(&n).Error)
		return n
	}

	before := instances()
	driveUsageCached()
	require.True(t, driveUsageCached(), "前提: 2 回目はキャッシュから返る")
	reportAt := dbHealthAt()
	require.Equal(t, reportAt, dbHealthAt(), "前提: 2 回目はキャッシュから返る")
	request()
	require.Eventually(t, func() bool { return ipRows() == 1 }, 5*time.Second, 10*time.Millisecond)

	// 他のプロセスが書いたときやDBを戻したときと同じく、DBだけを変える。
	require.NoError(t, h.db.Create(&model.Instance{ID: instanceID, Host: instanceID + ".example", FirstRetrievedAt: time.Now()}).Error)
	require.NoError(t, h.db.Where(`"userId" = ?`, userID).Delete(&model.UserIP{}).Error)
	require.Equal(t, before, instances(), "前提: 捨てるまではキャッシュが返る")
	request()
	// 前提の確認なので、書き込みの goroutine を待つのは短い時間だけにする。
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, ipRows(), "前提: 捨てるまでは記録済みとして書かない")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	assert.Equal(t, before+1, instances(), "the stats counts were not dropped")
	assert.False(t, driveUsageCached(), "the drive usage was not dropped")
	assert.NotEqual(t, reportAt, dbHealthAt(), "the database health was not dropped")
	request()
	assert.Eventually(t, func() bool { return ipRows() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the IP log window was not dropped")
	assert.Empty(t, published("caches-admin-stats-line"), "dropping the caches told the other processes")
}

// externalLineCaches are the caches of the external and file line (#3776) and
// where their values come from.
var externalLineCaches = map[string]cacheOrigin{
	"urlPreview": cacheFromExternal, "remoteStats": cacheFromExternal, "fetchRSS": cacheFromExternal,
	"pluginPeers": cacheFromExternal, "twemojiBadge": cacheFromFile,
}

// TestCaches_ExternalLine checks on a whole server that the external and file
// line is listed with the values the server uses and the origins decided for
// them, and that dropping the list makes the URL preview, /api/fetch-rss and
// the twemoji badge read their source again, without telling the other
// processes (#3776).
//
// URL プレビューと RSS は、127.0.0.1 の httptest のサーバーを allowedPrivateNetworks
// で許して、API を通して読む。リモート利用者の統計と peer のプラグイン一覧は相手へ
// https で取りに行くので、ここでは一覧の値がサーバーの使う値と同じことだけを見て、
// 捨てることと捨てる前の取得を積まないことは各パッケージのテスト
// (TestRemoteStatsFetcher_DropLocal、TestNodeInfoPeerLister_DropLocal など) に任せる。
func TestCaches_ExternalLine(t *testing.T) {
	twemojiDir := t.TempDir()
	svgPath := filepath.Join(twemojiDir, "1f004.svg")
	require.NoError(t, os.WriteFile(svgPath, []byte(testSVG), 0o600))
	t.Setenv("MISSKEY_TWEMOJI_DIR", twemojiDir)

	var pageHits, feedHits atomic.Int64
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<html><head><title>Page %d</title></head></html>`, pageHits.Add(1))
	}))
	t.Cleanup(page.Close)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Feed %d</title></channel></rss>`, feedHits.Add(1))
	}))
	t.Cleanup(feed.Close)

	h := newMetaRuntimeHarness(t, func(s *Server) {
		s.config.AllowedPrivateNetworks = []string{"127.0.0.1/32"}
	})
	srv := h.srv
	caches := srv.crossProcessCaches

	want := map[string]localCache{
		"urlPreview": caches.urlPreview, "remoteStats": caches.remoteStats, "fetchRSS": caches.fetchRSS,
		"pluginPeers": caches.pluginPeers, "twemojiBadge": caches.twemojiBadge,
	}
	seen := map[string]bool{}
	for _, p := range srv.caches.parts {
		if v, ok := want[p.name]; ok {
			seen[p.name] = true
			assert.Equal(t, externalLineCaches[p.name], p.origin, "cache %q", p.name)
			assert.Same(t, v, p.cache, "cache %q is not the value the server uses", p.name)
		}
	}
	for name := range externalLineCaches {
		require.True(t, seen[name], "cache %q is not listed", name)
	}
	// peer のプラグイン一覧は、プラグインの通信が使う値と同じであること。
	assert.Equal(t, peerPluginLister(caches.pluginPeers), srv.peerDeps.remote,
		"the plugin peers are not the lister the plugin peer protocol uses")

	var saved struct{ URLPreviewEnabled bool }
	require.NoError(t, h.db.Model(&model.Meta{}).Select(`"urlPreviewEnabled"`).Take(&saved).Error)
	t.Cleanup(func() {
		require.NoError(t, h.db.Model(&model.Meta{}).Where("1 = 1").Update("urlPreviewEnabled", saved.URLPreviewEnabled).Error)
	})
	h.updateMetaHere(map[string]any{"urlPreviewEnabled": true})

	preview := func() string {
		rec := h.get("/url?url=" + url.QueryEscape(page.URL))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got struct {
			Title string `json:"title"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got.Title
	}
	rss := func() string {
		rec := h.get("/api/fetch-rss?url=" + url.QueryEscape(feed.URL))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got struct {
			Title string `json:"title"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got.Title
	}
	badge := func() []byte {
		rec := h.get("/twemoji-badge/1f004.png")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.Bytes()
	}

	require.Equal(t, "Page 1", preview())
	require.Equal(t, "Feed 1", rss())
	before := badge()

	// 相手の内容とファイルだけを変える (相手のサーバーは呼ばれるたびに番号を進める)。
	require.NoError(t, os.WriteFile(svgPath, []byte(testSVGSmall), 0o600))
	require.Equal(t, "Page 1", preview(), "前提: 捨てるまではキャッシュが返る")
	require.Equal(t, "Feed 1", rss(), "前提: 捨てるまではキャッシュが返る")
	require.Equal(t, before, badge(), "前提: 捨てるまではキャッシュが返る")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	assert.Equal(t, "Page 2", preview(), "the URL previews were not dropped")
	assert.Equal(t, "Feed 2", rss(), "the feeds were not dropped")
	assert.NotEqual(t, before, badge(), "the twemoji badges were not dropped")
	assert.Empty(t, published("caches-external-line"), "dropping the caches told the other processes")
}

// reloadedOnDrop are the items of the cache list that read their values again
// instead of dropping them (#3777).
var reloadedOnDrop = []string{"captcha", "reactionBuffering"}

// TestCaches_ReloadedFromMeta checks on a whole server that the captcha
// providers and the reaction buffering switch are listed as items that read
// meta again, and that after the meta row changes in the database, dropping
// the list makes captcha verify with the new providers and reactions follow
// the new switch, without telling the other processes (#3777).
//
// 読み直しの項目はfuncの型なので、サーバーの使う値と同じかは比べられない。
// 捨てた後に、サーバーの使う値(captchaのserviceとリアクションのwriter)が
// 新しい設定で動くことで確かめる。
func TestCaches_ReloadedFromMeta(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	// testcaptchaは無効で起動させる(起動時の写しが前提の状態になるように)。
	// metaの行が無いschema(このテストだけを流したとき)でも読めるよう、ハーネスと
	// 同じく先に行を作る。
	db := serverIntegrationDB
	require.NoError(t, repository.NewMetaRepository(db).EnsureInitial("meta-runtime-meta"))
	var testcaptcha bool
	require.NoError(t, db.Model(&model.Meta{}).Select(`"enableTestcaptcha"`).Take(&testcaptcha).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Model(&model.Meta{}).Where("1 = 1").Update("enableTestcaptcha", testcaptcha).Error)
	})
	require.NoError(t, db.Model(&model.Meta{}).Where("1 = 1").Update("enableTestcaptcha", false).Error)

	h := newMetaRuntimeHarness(t)
	srv := h.srv
	svc := srv.crossProcessCaches.captcha
	require.NotNil(t, svc)
	writer := srv.reactionCountWriter
	require.NotNil(t, writer)

	var listed []string
	for _, p := range srv.caches.parts {
		if _, ok := p.cache.(reloadOnDrop); ok {
			listed = append(listed, p.name)
			assert.Equal(t, cacheFromDB, p.origin, "cache %q", p.name)
		}
	}
	require.Equal(t, reloadedOnDrop, listed, "the items read again from meta are not listed")
	require.False(t, svc.IsEnabled(), "前提: captchaは無効")
	require.False(t, writer.Buffering(), "前提: バッファは無効")

	// 他のプロセスが書いたときと違い、通知は送らない。このプロセスの写しは古いまま
	// 残る。
	require.NoError(t, h.db.Model(&model.Meta{}).Where("1 = 1").Updates(map[string]any{
		"enableTestcaptcha": true, "enableReactionsBuffering": true,
	}).Error)
	require.NoError(t, svc.Verify(context.Background(), corecaptcha.CaptchaTokens{}), "前提: 捨てるまでは前のproviderの組で検証する")
	require.False(t, writer.Buffering(), "前提: 捨てるまでは前の切り替えのまま")

	published := watchInternal(t, h)

	srv.caches.dropAll()

	assert.True(t, svc.IsEnabled(), "the captcha providers were not read again")
	assert.Error(t, svc.Verify(context.Background(), corecaptcha.CaptchaTokens{}), "captcha does not verify with the new providers")
	assert.NoError(t, svc.Verify(context.Background(), corecaptcha.CaptchaTokens{Testcaptcha: "testcaptcha-passed"}))
	assert.True(t, writer.Buffering(), "the reaction buffering switch was not read again")
	assert.Empty(t, published("caches-reloaded-from-meta"), "dropping the caches told the other processes")
}

// TestReloadOnDrop_DropLocalReads checks that DropLocal of a reload item calls
// the reload function, every time.
func TestReloadOnDrop_DropLocalReads(t *testing.T) {
	calls := 0
	r := reloadOnDrop(func() { calls++ })
	r.DropLocal()
	r.DropLocal()
	assert.Equal(t, 2, calls)
}
