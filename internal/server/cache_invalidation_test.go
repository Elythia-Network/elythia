package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/core/cachebus"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestSetupRoutes_CacheInvalidationReachesOtherProcess builds two servers on
// one Redis and one DB, as two processes of a split deployment, and pins the
// wiring of #3664: a change made in process A (an admin operation, an
// accepted relay, a user write) is published by A's hooks, received by B's
// subscriptions, and makes B read the cache it had already filled again, for
// each of the four caches. The caches' own handling is tested in
// core/cachebus.
func TestSetupRoutes_CacheInvalidationReachesOtherProcess(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB

	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	redisClients := &cache.RedisClients{
		Default: redisClient, Pubsub: redisClient, JobQueue: redisClient,
		Timelines: redisClient, Reactions: redisClient,
	}
	redisPort, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	redisOptions := config.RedisOptions{Host: mr.Host(), Port: redisPort}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
	}
	// New はプロセス共有の状態を張り替える (#2795)。2 つ作っても戻すのは 1 回でよい。
	restoreProcessGlobals(t)
	// 停止判定は meta の federation が all のときだけ instance 行で決まる。B の meta は
	// キャッシュされるので、サーバーを作る前に揃え、終わったら戻す。
	_, err = repository.NewMetaRepository(db).Fetch()
	require.NoError(t, err)
	var federation string
	require.NoError(t, db.Raw(`SELECT federation FROM meta LIMIT 1`).Scan(&federation).Error)
	require.NoError(t, db.Exec(`UPDATE meta SET federation = 'all'`).Error)
	t.Cleanup(func() { db.Exec(`UPDATE meta SET federation = ?`, federation) })
	srvA, err := New(cfg, db, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srvA.Shutdown(context.Background())) })
	srvB, err := New(cfg, db, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srvB.Shutdown(context.Background())) })

	const prefix = "cinv3664_"
	const host = "cinv3664.example"
	const acceptedRelayInbox = "https://cinv3664-relay0.example/inbox"
	cleanup := func() {
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM instance WHERE host = ?`, host)
		db.Exec(`DELETE FROM relay WHERE inbox = ?`, acceptedRelayInbox)
		db.Exec(`DELETE FROM avatar_decoration WHERE name = ?`, prefix+"deco")
	}
	cleanup()
	t.Cleanup(cleanup)
	rootID, rootToken := prefix+"root", "cinv3664root0001"
	require.NoError(t, repository.NewUserRepository(db).Create(&model.User{ID: rootID, Username: prefix + "root", UsernameLower: prefix + "root", Token: &rootToken, IsRoot: true, AvatarDecorations: datatypes.JSON([]byte("[]"))}))
	require.NoError(t, db.Create(&model.Instance{ID: prefix + "inst", Host: host, SuspensionState: model.SuspensionStateNone}).Error)

	// B の購読がすべての topic にあること (配線の抜けを見る)。
	topics := []string{cachebus.TopicInstanceSuspension, cachebus.TopicRelay, cachebus.TopicAvatarDecoration, cachebus.TopicUser}
	for _, topic := range topics {
		require.Positive(t, srvB.internalPubSub.SubscriberCount(topic), "process B does not subscribe to %s", topic)
	}
	// A が送ったものを B の側で受けて記録する。
	var mu sync.Mutex
	received := map[string][]json.RawMessage{}
	for _, topic := range topics {
		stop := srvB.internalPubSub.Subscribe(context.Background(), topic, func(raw []byte) {
			mu.Lock()
			received[topic] = append(received[topic], append(json.RawMessage(nil), raw...))
			mu.Unlock()
		})
		t.Cleanup(stop)
	}
	var probed atomic.Bool
	t.Cleanup(srvB.internalPubSub.Subscribe(context.Background(), "probe3664", func([]byte) { probed.Store(true) }))
	require.Eventually(t, func() bool {
		_ = srvA.internalPubSub.Publish(context.Background(), "probe3664", struct{}{})
		return probed.Load()
	}, 5*time.Second, 20*time.Millisecond)

	post := func(path, body string) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+rootToken)
		rec := httptest.NewRecorder()
		srvA.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	// waitFor waits until B received a message on topic whose inv contains want.
	waitFor := func(topic, want string) {
		t.Helper()
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, raw := range received[topic] {
				if bytes.Contains(raw, []byte(want)) {
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "process B did not receive %s containing %s", topic, want)
	}

	// 各キャッシュについて、B に先に読ませて (キャッシュさせて) から A で変え、
	// B が通知を受けて実際に捨てた (読み直した) ことまで見る。送信元の取り違え
	// (両方が同じ sender で互いの通知を受け流す) や配線先の取り違えも、ここで落ちる。
	parts := srvB.crossProcessCaches
	require.False(t, parts.instances.ShouldSkipDelivery(host), "B caches \"deliver\"")
	require.Equal(t, http.StatusNoContent, post("/api/admin/federation/update-instance", `{"host":"`+host+`","isSuspended":true}`))
	waitFor(cachebus.TopicInstanceSuspension, `"host":"`+host+`"`)
	require.Eventually(t, func() bool { return parts.instances.ShouldSkipDelivery(host) }, 5*time.Second, 10*time.Millisecond,
		"process B must stop delivering to the host suspended through process A")

	// デコレーション: B が一覧をキャッシュした後に行を足し、A で別のデコレーションを作る。
	const decoID = prefix + "deco0"
	_, ok := parts.decorations.LookupURL(decoID)
	require.False(t, ok)
	require.NoError(t, db.Create(&model.AvatarDecoration{ID: decoID, URL: "https://example.test/deco0.png", Name: prefix + "deco"}).Error)
	_, ok = parts.decorations.LookupURL(decoID)
	require.False(t, ok, "B answers from its cached catalog")
	require.Equal(t, http.StatusOK, post("/api/admin/avatar-decorations/create", `{"name":"`+prefix+`deco","url":"https://example.test/deco.png"}`))
	waitFor(cachebus.TopicAvatarDecoration, `"catalog":true`)
	require.Eventually(t, func() bool {
		_, ok := parts.decorations.LookupURL(decoID)
		return ok
	}, 5*time.Second, 10*time.Millisecond, "process B must drop its catalog")

	// 絵文字由来のデコレーション: B の map と絵文字の一覧をキャッシュさせた後に
	// ローカルの絵文字を DB へ足し、B の絵文字の一覧だけを捨てる (map は残る)。
	// A の管理画面の操作 (デコレーションの map を捨てる) が届けば、B も読み直す。
	const emojiID = prefix + "emoji0"
	db.Exec(`DELETE FROM emoji WHERE id LIKE ?`, prefix+"emoji%")
	t.Cleanup(func() { db.Exec(`DELETE FROM emoji WHERE id LIKE ?`, prefix+"emoji%") })
	emojiWorn := func(id string) bool {
		_, _, ok := parts.emojiDecorations.LookupEmojiDecoration(id)
		return ok
	}
	require.False(t, emojiWorn(emojiID))
	require.NoError(t, db.Create(&model.Emoji{ID: emojiID, Name: prefix + "emoji0", OriginalURL: "https://example.test/e0.png"}).Error)
	parts.emojis.ApplyRemoteInvalidation(repository.EmojiCacheInvalidation{IDs: []string{emojiID}})
	require.False(t, emojiWorn(emojiID), "B answers from its cached map")
	srvA.crossProcessCaches.emojiDecorations.Invalidate()
	waitFor(cachebus.TopicAvatarDecoration, `"emoji":true`)
	require.Eventually(t, func() bool { return emojiWorn(emojiID) }, 5*time.Second, 10*time.Millisecond,
		"process B must drop its emoji decoration map")
	// A の絵文字のキャッシュへの書き込み (emojiCacheInvalidated) だけでも、B は絵文字の
	// 一覧と map の両方を捨てて読み直す。
	const emojiID1 = prefix + "emoji1"
	require.NoError(t, srvA.crossProcessCaches.emojis.Create(&model.Emoji{ID: emojiID1, Name: prefix + "emoji1", OriginalURL: "https://example.test/e1.png"}))
	require.Eventually(t, func() bool { return emojiWorn(emojiID1) }, 5*time.Second, 10*time.Millisecond,
		"process B must see the local emoji created through process A")

	// relay: Accept の受信 (A の連合の受信) で承認された relay を、一覧をキャッシュ
	// していた B が使うようになる。
	accepted := acceptedRelayInbox
	actor := &model.User{ID: prefix + "relayactor", SharedInbox: &accepted}
	require.NoError(t, db.Create(&model.Relay{ID: prefix + "relay0", Inbox: acceptedRelayInbox, Status: "requesting"}).Error)
	require.False(t, parts.relays.IsRelayActor(actor), "B caches the list without the relay")
	require.NoError(t, srvA.crossProcessCaches.relays.MarkAccepted(context.Background(), prefix+"relay0"))
	waitFor(cachebus.TopicRelay, `"inv":{}`)
	require.Eventually(t, func() bool { return parts.relays.IsRelayActor(actor) }, 5*time.Second, 10*time.Millisecond,
		"process B must use the relay accepted in process A")

	// 利用者は、A の repository の書き込みが B の読み出しに届くところまで見る。
	id := prefix + "u1"
	require.NoError(t, srvA.userCache.Create(&model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}))
	u, err := srvB.userCache.FindByID(id)
	require.NoError(t, err)
	require.False(t, u.IsSuspended)
	require.NoError(t, srvA.userCache.UpdateUser(id, map[string]any{"isSuspended": true}))
	waitFor(cachebus.TopicUser, id)
	require.Eventually(t, func() bool {
		u, err := srvB.userCache.FindByID(id)
		return err == nil && u.IsSuspended
	}, 5*time.Second, 10*time.Millisecond)
	assert.Same(t, srvA.userRepo, repository.UserRepository(srvA.userCache), "userCache must be the repository the routes use")
}
