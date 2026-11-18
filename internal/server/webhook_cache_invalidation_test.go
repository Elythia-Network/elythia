package server

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// builtWebhookCache returns the cache of the active webhooks the server wired
// into the webhook service.
func builtWebhookCache(t *testing.T, srv *Server) *repository.CachedWebhookRepository {
	t.Helper()
	repo := reflect.ValueOf(srv.builtCore.webhookReactionHook).Elem().FieldByName("svc").Elem().FieldByName("userRepo")
	require.True(t, repo.IsValid())
	cached, ok := reflect.NewAt(repo.Type(), repo.Addr().UnsafePointer()).Elem().Interface().(*repository.CachedWebhookRepository)
	require.True(t, ok, "the webhook service does not use the cached repository")
	return cached
}

// 利用者の webhook のキャッシュの無効化が、他のプロセスへ届くこと (#4218)。同じ
// Redis と DB の上に 2 つのサーバーを作り、B に一覧を読ませてから、A で webhook を
// 作る (wrapper を通す書き込み) と、退会のジョブと同じく wrapper を通さずに行を消して
// NotifyChanged を呼ぶ。どちらも B が TTL (1 分) を待たずに読み直すことを見る。
// publish (SetInvalidationHook) と購読 (Subscribe) のどちらが抜けても落ちる。
func TestSetupRoutes_WebhookCacheInvalidationReachesOtherProcess(t *testing.T) {
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

	const userID = "whinv4218user"
	cleanup := func() {
		db.Exec(`DELETE FROM "webhook" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "user" WHERE id = ?`, userID)
	}
	cleanup()
	t.Cleanup(cleanup)
	require.NoError(t, repository.NewUserRepository(db).Create(&model.User{ID: userID, Username: userID, UsernameLower: userID, AvatarDecorations: datatypes.JSON([]byte("[]"))}))

	cacheA, cacheB := builtWebhookCache(t, srvA), builtWebhookCache(t, srvB)
	got, err := cacheB.ListActiveByUserID(userID)
	require.NoError(t, err)
	require.Empty(t, got)

	require.NoError(t, cacheA.Create(&model.Webhook{ID: "whinv4218a", UserID: userID, Name: "h", On: model.StringArray{"note"}, URL: "https://example.com/hook", Secret: "s", Active: true}))
	require.Eventually(t, func() bool {
		rows, err := cacheB.ListActiveByUserID(userID)
		return err == nil && len(rows) == 1
	}, 5*time.Second, 20*time.Millisecond, "process B kept the cached list after A created a webhook")

	require.NoError(t, db.Exec(`DELETE FROM "webhook" WHERE "userId" = ?`, userID).Error)
	cacheA.NotifyChanged()
	require.Eventually(t, func() bool {
		rows, err := cacheB.ListActiveByUserID(userID)
		return err == nil && len(rows) == 0
	}, 5*time.Second, 20*time.Millisecond, "process B kept the cached list after NotifyChanged in A")
}
