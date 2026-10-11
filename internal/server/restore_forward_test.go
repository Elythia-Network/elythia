package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// fakeBackupService records what reaches the control API.
type fakeBackupService struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
	ips    []string
}

func (f *fakeBackupService) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.bodies = append(f.bodies, m)
		f.ips = append(f.ips, r.Header.Get("X-Elythia-Client-IP"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"record":{"step":"ready"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSetupRoutes_RestoreIsForwardedWithoutTheGuard checks that the restore
// endpoints reach the backup service without the main server checking the
// re-authentication (#3463 (6c)): a guard here would consume the TOTP code
// before the service checks it.
func TestSetupRoutes_RestoreIsForwardedWithoutTheGuard(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	svc := &fakeBackupService{}
	daemon := svc.server(t)
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	redisClients := &cache.RedisClients{Default: redisClient, Pubsub: redisClient, JobQueue: redisClient, Timelines: redisClient, Reactions: redisClient}
	redisPort, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	ro := config.RedisOptions{Host: mr.Host(), Port: redisPort}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		Redis: ro, RedisForPubsub: ro, RedisForJobQueue: ro, RedisForTimelines: ro, RedisForReactions: ro,
		Backup: &config.BackupOptions{Server: config.BackupServerOptions{ServiceURL: daemon.URL, ServiceToken: "svc"}},
	}
	restoreProcessGlobals(t)
	srv, err := New(cfg, serverIntegrationDB, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	// 管理者 (root) の native token。TOTP は登録していないので、本体が再認証をすれば
	// 必ず断る (= 転送されない)。
	rootID, rootToken := "restore-fwd-root", "restorefwd000001"
	cleanup := func() {
		require.NoError(t, serverIntegrationDB.Where("id = ?", rootID).Delete(&model.User{}).Error)
	}
	cleanup()
	t.Cleanup(cleanup)
	require.NoError(t, repository.NewUserRepository(serverIntegrationDB).Create(&model.User{
		ID: rootID, Username: "restore_fwd_root", UsernameLower: "restore_fwd_root", Token: &rootToken, IsRoot: true}))
	post := func(op, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/backup/restore/"+op, bytes.NewReader([]byte(`{"opId":"x","ticket":"t","password":"pw","token":"123456"}`)))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		if token != "" {
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	for _, op := range []string{"prepare", "confirm", "abort", "status"} {
		rec := post(op, rootToken)
		assert.Equal(t, http.StatusOK, rec.Code, op+": "+rec.Body.String())
		assert.JSONEq(t, `{"record":{"step":"ready"}}`, rec.Body.String())
	}
	// 本体が動いている間は、管理者でなければ転送しない。
	assert.Equal(t, http.StatusUnauthorized, post("status", "").Code)
	svc.mu.Lock()
	defer svc.mu.Unlock()
	assert.Equal(t, []string{"/restore/prepare", "/restore/confirm", "/restore/abort", "/restore/status"}, svc.paths)
	assert.Equal(t, "123456", svc.bodies[0]["token"], "the second factor reaches the service unconsumed")
	assert.Equal(t, rootToken, svc.bodies[0]["i"], "the session token moves into the body")
}

// TestMaintenanceRestoreForwardResolvesTheClientLikeEcho checks that the
// forward served during a maintenance passes the address echo would have
// resolved (trustProxy), which the failure budget of the re-authentication
// is keyed on.
func TestMaintenanceRestoreForwardResolvesTheClientLikeEcho(t *testing.T) {
	svc := &fakeBackupService{}
	daemon := svc.server(t)
	cfg := &config.Config{
		TrustProxy: []string{"10.0.0.0/8"},
		Backup:     &config.BackupOptions{Server: config.BackupServerOptions{ServiceURL: daemon.URL, ServiceToken: "svc"}},
	}
	fwd, prefix := maintenanceRestoreForward(cfg)
	require.NotNil(t, fwd)
	assert.Equal(t, "/api/admin/backup/restore/", prefix)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/backup/restore/status", bytes.NewReader([]byte(`{"opId":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.RemoteAddr = "10.1.2.3:4567"
	rec := httptest.NewRecorder()
	fwd(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// 信頼しない接続元からの XFF は使わない。
	req = httptest.NewRequest(http.MethodPost, "/api/admin/backup/restore/status", bytes.NewReader([]byte(`{"opId":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.RemoteAddr = "198.51.100.4:4567"
	fwd(httptest.NewRecorder(), req)
	svc.mu.Lock()
	defer svc.mu.Unlock()
	assert.Equal(t, []string{"203.0.113.9", "198.51.100.4"}, svc.ips)

	_, prefix = maintenanceRestoreForward(&config.Config{TrustProxy: []string{"not a cidr"}})
	assert.Empty(t, prefix)
}

// TestMaintenanceHandlerForwardsTheRestore checks that the handler a
// process serves during a maintenance is wired to the forward, and that a
// queue-only node is not.
func TestMaintenanceHandlerForwardsTheRestore(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	svc := &fakeBackupService{}
	daemon := svc.server(t)
	cfg := &config.Config{Backup: &config.BackupOptions{Server: config.BackupServerOptions{ServiceURL: daemon.URL, ServiceToken: "svc"}}}
	var m maintTracker
	m.init(&Supervisor{cfg: cfg})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/backup/restore/status", bytes.NewReader([]byte(`{"opId":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	m.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	t.Setenv(config.EnvOnlyQueue, "1")
	var q maintTracker
	q.init(&Supervisor{cfg: cfg})
	rec = httptest.NewRecorder()
	q.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/backup/restore/status", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "a queue-only node serves no API")
	svc.mu.Lock()
	defer svc.mu.Unlock()
	assert.Len(t, svc.paths, 1)
}
