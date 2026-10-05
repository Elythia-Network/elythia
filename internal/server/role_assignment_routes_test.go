package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var serverIntegrationDB *gorm.DB

func TestMain(m *testing.M) {
	if db, err := testutil.OpenTestDB(); err == nil {
		testutil.ApplyMigrations(db)
		serverIntegrationDB = db
	}
	os.Exit(m.Run())
}

func TestSetupRoutes_RoleAssignmentShowAuthorization(t *testing.T) {
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
	// **`New` はプロセス共有の状態を張り替える。** 戻さないと後続のテストが
	// 署名付きプロキシ URL を受け取って落ちる (#2795)。
	restoreProcessGlobals(t)
	srv, err := New(cfg, db, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	post := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{}`)))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		if token != "" {
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	rootID, rootToken := "route-root", "rootroute0000001"
	nonModeratorToken := "userroute0000001"
	cleanup := func() {
		require.NoError(t, db.Where("id IN ?", []string{"route-read-account", "route-read-admin", "route-no-scope"}).Delete(&model.AccessToken{}).Error)
		require.NoError(t, db.Where(`"usernameLower" IN ?`, []string{"route_root", "route_user"}).Delete(&model.User{}).Error)
	}
	cleanup()
	t.Cleanup(cleanup)
	userRepo := repository.NewUserRepository(db)
	require.NoError(t, userRepo.Create(&model.User{ID: rootID, Username: "route_root", UsernameLower: "route_root", Token: &rootToken, IsRoot: true}))
	require.NoError(t, userRepo.Create(&model.User{ID: "route-user", Username: "route_user", UsernameLower: "route_user", Token: &nonModeratorToken}))

	createAccessToken := func(id, token string, permissions ...string) {
		require.NoError(t, repository.NewAccessTokenRepository(db).Create(&model.AccessToken{
			ID: id, Token: token, Hash: "hash-" + id, UserID: rootID, Permission: model.StringArray(permissions),
		}))
	}
	createAccessToken("route-read-account", "route-read-account-token", "read:account")
	createAccessToken("route-read-admin", "route-read-admin-token", "read:admin:roles")
	createAccessToken("route-no-scope", "route-no-scope-token")

	require.Equal(t, http.StatusUnauthorized, post("/api/roles/assignment-show", "").Code)
	require.Equal(t, http.StatusBadRequest, post("/api/roles/assignment-show", "route-read-account-token").Code)
	require.Equal(t, http.StatusForbidden, post("/api/roles/assignment-show", "route-read-admin-token").Code)
	require.Equal(t, http.StatusForbidden, post("/api/roles/assignment-show", "route-no-scope-token").Code)
	require.Equal(t, http.StatusForbidden, post("/api/admin/roles/assignment-show", nonModeratorToken).Code)
	require.Equal(t, http.StatusBadRequest, post("/api/admin/roles/assignment-show", "route-read-admin-token").Code)
	require.Equal(t, http.StatusForbidden, post("/api/admin/roles/assignment-show", "route-read-account-token").Code)
	require.Equal(t, http.StatusForbidden, post("/api/admin/roles/assignment-show", "route-no-scope-token").Code)
}
