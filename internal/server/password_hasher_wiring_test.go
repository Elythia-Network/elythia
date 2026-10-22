package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// newServerWithConfig builds a full server through New on the integration
// database, after mutate has adjusted the config, and returns it.
//
// 設定から部品へ渡す値(#3780)が、組み立てで実際に渡っていることを、外から
// リクエストを送って見るためのもの。
func newServerWithConfig(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	// ドライブのファイルは作業ディレクトリ相対の./drive-filesに置かれるので、
	// リポジトリを汚さないよう一時ディレクトリへ移る。
	t.Chdir(t.TempDir())

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
		MediaProxySecret: []byte("test-secret"),
		Redis:            redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
	}
	mutate(cfg)
	restoreProcessGlobals(t)
	srv, err := New(cfg, serverIntegrationDB, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

// postJSON sends a JSON POST to srv as the holder of token (none when empty).
func postJSON(t *testing.T, srv *Server, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// **設定のbcryptCostが、パスワードをハッシュする5つの部品すべてへ渡ること(#3780)。**
//
// 以前はpassword.SetCostでプロセスの変数を書き換えていたので、配線の漏れは
// 起こりえなかった。部品ごとにsetterで渡す形にしたので、1つ渡し忘れると
// その経路だけ既定のcostでハッシュを作る。既定(10)と違うcostを設定し、5つの
// 経路それぞれで保存されたハッシュのcostを見る。
func TestNewServer_WiresTheConfiguredBcryptCost(t *testing.T) {
	const configured = bcrypt.MinCost + 1
	srv := newServerWithConfig(t, func(cfg *config.Config) { cfg.BcryptCost = configured })
	db := serverIntegrationDB
	cleanupPluginAccountUsers(t, db, "ph_root", "ph_target", "ph_weak", "ph_created")
	createLocalUser(t, db, "ph-root", "ph_root", "phroottokenxx780", "root-pass", true)
	createLocalUser(t, db, "ph-target", "ph_target", "phtargettoken780", "target-pass", false)
	createLocalUser(t, db, "ph-weak", "ph_weak", "phweaktokenxx780", "weak-pass", false)

	storedCost := func(t *testing.T, userID string) int {
		t.Helper()
		var p model.UserProfile
		require.NoError(t, db.First(&p, `"userId" = ?`, userID).Error)
		require.NotNil(t, p.Password)
		c, err := bcrypt.Cost([]byte(*p.Password))
		require.NoError(t, err)
		return c
	}

	t.Run("signin rehashes to the configured cost", func(t *testing.T) {
		weak, err := bcrypt.GenerateFromPassword([]byte("weak-pass"), bcrypt.MinCost)
		require.NoError(t, err)
		require.NoError(t, db.Model(&model.UserProfile{}).Where(`"userId" = ?`, "ph-weak").
			Update("password", string(weak)).Error)

		rec := postJSON(t, srv, "/api/signin-flow", "", map[string]any{"username": "ph_weak", "password": "weak-pass"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, configured, storedCost(t, "ph-weak"))
	})

	t.Run("admin/accounts/create (signup service)", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/admin/accounts/create", "phroottokenxx780",
			map[string]any{"username": "ph_created", "password": "created-pass"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var created model.User
		require.NoError(t, db.First(&created, `"usernameLower" = ? AND "host" IS NULL`, "ph_created").Error)
		assert.Equal(t, configured, storedCost(t, created.ID))
	})

	t.Run("admin/reset-password", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/admin/reset-password", "phroottokenxx780", map[string]any{"userId": "ph-target"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, configured, storedCost(t, "ph-target"))

		var body struct {
			Password string `json:"password"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

		t.Run("i/change-password", func(t *testing.T) {
			rec := postJSON(t, srv, "/api/i/change-password", "phtargettoken780",
				map[string]any{"currentPassword": body.Password, "newPassword": "changed-pass"})
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.Equal(t, configured, storedCost(t, "ph-target"))
		})
	})

	t.Run("reset-password", func(t *testing.T) {
		require.NoError(t, db.Model(&model.UserProfile{}).Where(`"userId" = ?`, "ph-weak").
			Update("password", mustBcrypt(t, "weak-pass", bcrypt.MinCost)).Error)
		const tok = "resettoken-ph780"
		// 再設定の要求はIDの時刻で期限を見るので、今の時刻で作る。
		idGen, err := id.NewGenerator("aidx")
		require.NoError(t, err)
		require.NoError(t, db.Exec(`INSERT INTO "password_reset_request" ("id", "token", "userId") VALUES (?, ?, ?)`,
			idGen.Generate(time.Now()), tok, "ph-weak").Error)
		rec := postJSON(t, srv, "/api/reset-password", "", map[string]any{"token": tok, "password": "reset-pass"})
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, configured, storedCost(t, "ph-weak"))
	})
}

// mustBcrypt returns the bcrypt hash of plain at cost.
func mustBcrypt(t *testing.T, plain string, cost int) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	require.NoError(t, err)
	return string(h)
}
