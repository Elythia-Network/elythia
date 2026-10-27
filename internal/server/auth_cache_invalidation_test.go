package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/core/cachebus"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestSetupRoutes_AuthCacheInvalidationReachesOtherProcess builds two servers
// on one Redis and one DB and pins that dropping a user's cached
// authentications in process A (through admin/unsuspend-user and i/update)
// makes process B, which had cached them, read the user again (#3664).
func TestSetupRoutes_AuthCacheInvalidationReachesOtherProcess(t *testing.T) {
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
	srvA, err := New(cfg, db, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srvA.Shutdown(context.Background())) })
	srvB, err := New(cfg, db, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srvB.Shutdown(context.Background())) })

	const prefix = "ainv3664"
	cleanup := func() { db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%") }
	cleanup()
	t.Cleanup(cleanup)
	users := repository.NewUserRepository(db)
	rootToken, userToken := "ainv3664root0001", "ainv3664user0001"
	require.NoError(t, users.Create(&model.User{ID: prefix + "root", Username: prefix + "root", UsernameLower: prefix + "root", Token: &rootToken, IsRoot: true, AvatarDecorations: datatypes.JSON([]byte("[]"))}))
	userID := prefix + "u1"
	require.NoError(t, users.Create(&model.User{ID: userID, Username: prefix + "u1", UsernameLower: prefix + "u1", Token: &userToken, AvatarDecorations: datatypes.JSON([]byte("[]"))}))
	require.NoError(t, users.CreateProfile(&model.UserProfile{UserID: userID}))

	require.Positive(t, srvB.internalPubSub.SubscriberCount(cachebus.TopicAuth), "process B does not subscribe to %s", cachebus.TopicAuth)
	var authMessages atomic.Int32
	t.Cleanup(srvB.internalPubSub.Subscribe(context.Background(), cachebus.TopicAuth, func([]byte) { authMessages.Add(1) }))
	var revokeMessages atomic.Int32
	t.Cleanup(srvB.streamPubSub.Subscribe(context.Background(), stream.StreamRevokeTopic, func([]byte) { revokeMessages.Add(1) }))
	var probed atomic.Bool
	t.Cleanup(srvB.internalPubSub.Subscribe(context.Background(), "probeauth3664", func([]byte) { probed.Store(true) }))
	require.Eventually(t, func() bool {
		_ = srvA.internalPubSub.Publish(context.Background(), "probeauth3664", struct{}{})
		return probed.Load()
	}, 5*time.Second, 20*time.Millisecond)

	postA := func(path, token, body string) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		rec := httptest.NewRecorder()
		srvA.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	// seenByB は、B の認証の middleware が userToken をどの利用者に解決するか
	// (B のキャッシュ) を返す。拒んだときは nil。
	seenByB := func() *model.User {
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+userToken)
		var seen *model.User
		_ = srvB.auth.Authenticate()(func(c echo.Context) error {
			seen = middleware.GetUser(c)
			return nil
		})(e.NewContext(req, httptest.NewRecorder()))
		return seen
	}

	// 解除: B が凍結中の利用者を積んでいる状態で、A で解除する。
	require.Equal(t, http.StatusNoContent, postA("/api/admin/suspend-user", rootToken, `{"userId":"`+userID+`"}`))
	// 凍結は authCacheInvalidated を 1 通だけ出す。WebSocket の失効の通知に相乗りして
	// 捨てる側 (全プロセス) は自プロセスだけで捨て、通知を増やさない。
	// 凍結の通知 (auth と WebSocket の失効の 2 つ) が B に届き、適用されるのを待ってから
	// B に積ませる。先に積ませると、後から届いた通知で捨てられ、解除を伝えなくても
	// B が DB を読み直して通ってしまう (偶然に頼らないため)。同じ通知の他の
	// ハンドラは同じ goroutine で続けて呼ばれるので、少し待てば済んでいる。
	require.Eventually(t, func() bool { return authMessages.Load() >= 1 && revokeMessages.Load() >= 1 },
		5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, int32(1), authMessages.Load(), "one notification per invalidation")
	require.Nil(t, seenByB(), "B rejects the suspended user (and caches it)")
	// B が積んだことを確かめる: DB を直接戻しても (通知は出ない)、B は積んだ凍結中の
	// 利用者を拒み続ける。
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", userID).Update("isSuspended", false).Error)
	require.Nil(t, seenByB(), "B answers from its cache")
	require.Equal(t, http.StatusNoContent, postA("/api/admin/unsuspend-user", rootToken, `{"userId":"`+userID+`"}`))
	require.Eventually(t, func() bool {
		u := seenByB()
		return u != nil && !u.IsSuspended
	}, 5*time.Second, 10*time.Millisecond, "process B must accept the user unsuspended in process A")

	// i/update: B が利用者を積んでいる状態で、A で名前を変える。
	u := seenByB()
	require.NotNil(t, u)
	require.Nil(t, u.Name)
	require.Equal(t, http.StatusOK, postA("/api/i/update", userToken, `{"name":"renamed3664"}`))
	require.Eventually(t, func() bool {
		u := seenByB()
		return u != nil && u.Name != nil && *u.Name == "renamed3664"
	}, 5*time.Second, 10*time.Millisecond, "process B must read the user updated in process A")
}
