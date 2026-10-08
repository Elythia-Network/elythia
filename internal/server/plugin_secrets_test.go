package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/core/pluginsecret/pgrepo"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/plugin"
)

// 未配線でも Secrets() は nil を返さず、呼ぶと「使えない」になる。
func TestPluginContext_SecretsIsNeverNil(t *testing.T) {
	c := &pluginContext{name: "p"}
	require.NotNil(t, c.Secrets())
	_, err := c.Secrets().Get(context.Background(), "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretsUnavailable)

	s, api := newPluginTestServer(config.RoleServer)
	var got plugin.Secrets
	def := pluginDef("p", func(ctx plugin.Context, _ plugin.Router) error { got = ctx.Secrets(); return nil }, nil)
	require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, noopStorage))
	require.NotNil(t, got)
	assert.ErrorIs(t, got.Set(context.Background(), "apiKey", "v"), plugin.ErrSecretsUnavailable)
}

// 入力口は Secrets を宣言したプラグインにだけ張り、Routes を持たない
// プラグインでも張る (ジョブだけで動く bot が鍵を受け取れるように)。
func TestSetupPlugins_SecretRoutesOnlyForDeclaringPlugins(t *testing.T) {
	s, api := newPluginTestServer(config.RoleServer)
	svc, err := pluginsecret.New(nil, bytes.Repeat([]byte{1}, pluginsecret.KeySize))
	require.NoError(t, err)
	s.pluginSecrets = svc

	declaring := plugin.Definition{
		Name: "declaring", APIVersion: plugin.APIVersion,
		Secrets: []plugin.SecretSpec{{Name: "apiKey"}},
	}
	plain := pluginDef("plain", func(plugin.Context, plugin.Router) error { return nil }, nil)
	require.NoError(t, s.setupPlugins(api, []plugin.Definition{declaring, plain}, noopStorage))

	post := func(path string) int {
		rec := httptest.NewRecorder()
		s.echo.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		return rec.Code
	}
	for _, p := range []string{"/_secrets", "/_secrets/set", "/_secrets/delete"} {
		// 認証が無いので 401。ルートが無ければ 404 になる。
		assert.Equal(t, http.StatusUnauthorized, post("/api/plugin/declaring"+p), p)
		assert.Equal(t, http.StatusNotFound, post("/api/plugin/plain"+p), p)
	}

	// 宣言した名前は admin/server-plugins にも出る (値も状態も出さない)。
	infos := serverPluginInfos([]plugin.Definition{declaring, plain}, nil)
	assert.Equal(t, []string{"apiKey"}, infos[0].Secrets)
	assert.Equal(t, []string{}, infos[1].Secrets)
}

// pluginRoles が未配線でも、管理者の判定を「通す」側に倒さない。
func TestRegisterPluginSecretRoutes_UnwiredRolesDenyAll(t *testing.T) {
	assert.False(t, denyAllRoles{}.IsAdministrator("u"))
	assert.False(t, denyAllRoles{}.IsModerator("u"))

	s, api := newPluginTestServer(config.RoleServer)
	svc, err := pluginsecret.New(nil, bytes.Repeat([]byte{1}, pluginsecret.KeySize))
	require.NoError(t, err)
	s.pluginSecrets = svc
	token := "native-token"
	s.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(string(middleware.UserContextKey), &model.User{ID: "u", Token: &token})
			c.Set(string(middleware.TokenContextKey), token)
			return next(c)
		}
	})
	def := plugin.Definition{Name: "p", APIVersion: plugin.APIVersion, Secrets: []plugin.SecretSpec{{Name: "apiKey"}}}
	require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, noopStorage))
	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/plugin/p/_secrets", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// **管理者でも、ブラウザのログイン (users.token) 以外の token では通さない。**
//
// group 側の RejectAppToken は「アプリの token か」を scope で見る。こちらは
// token そのものが users.token と一致するかを見るので、scope の判定が漏れても
// 落ちる (本物の認証では両者が同時に効くので、ここで片方だけを試す)。
func TestRegisterPluginSecretRoutes_RequiresNativeToken(t *testing.T) {
	s, api := newPluginTestServer(config.RoleServer)
	s.pluginRoles = &stubRoles{admin: true, mod: true}
	svc, err := pluginsecret.New(failingSecretRepo{}, bytes.Repeat([]byte{1}, pluginsecret.KeySize))
	require.NoError(t, err)
	s.pluginSecrets = svc
	native := "native-token"
	presented := ""
	s.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(string(middleware.UserContextKey), &model.User{ID: "admin", Token: &native})
			c.Set(string(middleware.TokenContextKey), presented)
			return next(c)
		}
	})
	def := plugin.Definition{Name: "p", APIVersion: plugin.APIVersion, Secrets: []plugin.SecretSpec{{Name: "apiKey"}}}
	require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, noopStorage))
	post := func() int {
		rec := httptest.NewRecorder()
		s.echo.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/plugin/p/_secrets/delete", strings.NewReader(`{"name":"apiKey"}`)))
		return rec.Code
	}

	presented = "access-token-of-some-app"
	assert.Equal(t, http.StatusForbidden, post(), "native でない token で通った")

	// native token なら handler まで届く (repository が落ちるので 500)。
	presented = native
	assert.Equal(t, http.StatusInternalServerError, post())
}

type failingSecretRepo struct{}

func (failingSecretRepo) FindByName(context.Context, string, string) (*pluginsecret.Row, error) {
	return nil, errors.New("down")
}

func (failingSecretRepo) ListByPlugin(context.Context, string) ([]pluginsecret.Row, error) {
	return nil, errors.New("down")
}
func (failingSecretRepo) Upsert(context.Context, pluginsecret.Row) error {
	return errors.New("down")
}
func (failingSecretRepo) Delete(context.Context, string, string) error { return errors.New("down") }

// **本物の認証を通して、管理者のブラウザの token だけが書き換えられること**
// (#3470 の完了条件)。値はどの応答にも出ず、DB には暗号文で入り、プラグインは
// 自分の値だけをサーバー側で読める。
func TestPluginSecrets_EndToEnd(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB

	const (
		value      = "sk-live-E2E-TOPSECRET-4321"
		rootID     = "psec-root"
		rootToken  = "psecroot00000001"
		modID      = "psec-mod"
		modToken   = "psecmod000000001"
		userID     = "psec-user"
		userToken  = "psecuser00000001"
		appTokenID = "psec-app"
		appToken   = "psec-app-token-0000000001"
		modRoleID  = "psec-modrole"
	)
	cleanup := func() {
		require.NoError(t, db.Exec(`DELETE FROM "plugin_secret" WHERE "pluginName" IN ('psec-a', 'psec-b')`).Error)
		require.NoError(t, db.Where("id = ?", appTokenID).Delete(&model.AccessToken{}).Error)
		require.NoError(t, db.Where(`"roleId" = ?`, modRoleID).Delete(&model.RoleAssignment{}).Error)
		require.NoError(t, db.Where("id = ?", modRoleID).Delete(&model.Role{}).Error)
		require.NoError(t, db.Where("id IN ?", []string{rootID, modID, userID}).Delete(&model.User{}).Error)
	}
	cleanup()
	t.Cleanup(cleanup)

	userRepo := repository.NewUserRepository(db)
	rt, mt, ut := rootToken, modToken, userToken
	require.NoError(t, userRepo.Create(&model.User{ID: rootID, Username: "psec_root", UsernameLower: "psec_root", Token: &rt, IsRoot: true}))
	require.NoError(t, userRepo.Create(&model.User{ID: modID, Username: "psec_mod", UsernameLower: "psec_mod", Token: &mt}))
	require.NoError(t, userRepo.Create(&model.User{ID: userID, Username: "psec_user", UsernameLower: "psec_user", Token: &ut}))
	now := time.Now()
	require.NoError(t, db.Create(&model.Role{ID: modRoleID, Name: "psec-mod", UpdatedAt: now, LastUsedAt: now,
		Target: model.RoleTargetManual, IsModerator: true}).Error)
	require.NoError(t, db.Create(&model.RoleAssignment{ID: "psec-assign", UserID: modID, RoleID: modRoleID}).Error)
	// 管理者本人の、全権限を持つアプリの token。それでも書き換えられないこと。
	require.NoError(t, repository.NewAccessTokenRepository(db).Create(&model.AccessToken{
		ID: appTokenID, Token: appToken, Hash: "hash-" + appTokenID, UserID: rootID,
		Permission: model.StringArray{"read:account", "write:account", "read:admin:meta", "write:admin:meta"},
	}))

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
		PluginSecretKey: bytes.Repeat([]byte{42}, config.PluginSecretKeySize),
	}

	// プラグインが読めたかを返すだけのルート。**値は返さない。**
	probe := func(name string) plugin.Definition {
		return plugin.Definition{
			Name: name, APIVersion: plugin.APIVersion,
			Secrets: []plugin.SecretSpec{{Name: "apiKey", Description: "テスト"}},
			Routes: func(ctx plugin.Context, r plugin.Router) error {
				r.POST("/probe", func(req plugin.Request) (any, error) {
					v, err := ctx.Secrets().Get(req.Context(), "apiKey")
					return map[string]any{"matches": v == value, "notSet": errors.Is(err, plugin.ErrSecretNotSet)}, nil
				})
				return nil
			},
		}
	}

	restoreProcessGlobals(t)
	srv, err := newServer(cfg, db, redisClients, []plugin.Definition{probe("psec-a"), probe("psec-b")}, noopStorage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	post := func(path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		if token != "" {
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		// どの応答にも値を出さない。
		assert.NotContains(t, rec.Body.String(), "TOPSECRET", "%s の応答に値が出ている", path)
		return rec
	}
	setBody := `{"name":"apiKey","value":"` + value + `"}`

	// 管理者以外・ブラウザ以外の token は、一覧も保存も削除もできない。
	for _, tc := range []struct {
		who   string
		token string
		want  int
	}{
		{"未認証", "", http.StatusUnauthorized},
		{"一般の利用者", userToken, http.StatusForbidden},
		{"モデレーター", modToken, http.StatusForbidden},
		{"管理者のアプリの token", appToken, http.StatusForbidden},
	} {
		for _, p := range []string{"/_secrets", "/_secrets/set", "/_secrets/delete"} {
			rec := post("/api/plugin/psec-a"+p, tc.token, setBody)
			assert.Equalf(t, tc.want, rec.Code, "%s: %s (%s)", tc.who, p, rec.Body.String())
		}
	}
	var n int64
	require.NoError(t, db.Model(&model.PluginSecret{}).Where(`"pluginName" IN ('psec-a', 'psec-b')`).Count(&n).Error)
	require.Zero(t, n, "拒否されたはずの要求で値が保存されている")

	// 管理者のブラウザの token なら保存できる。
	rec := post("/api/plugin/psec-a/_secrets/set", rootToken, setBody)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = post("/api/plugin/psec-a/_secrets", rootToken, `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"hint":"4321"`)
	assert.Contains(t, rec.Body.String(), `"configured":true`)

	// DB には暗号文で入っている。
	var row model.PluginSecret
	require.NoError(t, db.Where(`"pluginName" = 'psec-a' AND "name" = 'apiKey'`).Take(&row).Error)
	assert.False(t, bytes.Contains(row.SecretCiphertext, []byte(value)), "DB に平文が入っている")

	// プラグインはサーバー側で自分の値だけを読める。
	rec = post("/api/plugin/psec-a/probe", userToken, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"matches":true,"notSet":false}`, rec.Body.String())
	rec = post("/api/plugin/psec-b/probe", userToken, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"matches":false,"notSet":true}`, rec.Body.String(), "別のプラグインの値が見えている")

	// 管理画面のプラグイン一覧には宣言した名前だけが出る。
	rec = post("/api/admin/server-plugins", modToken, `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"secrets":["apiKey"]`)

	rec = post("/api/plugin/psec-a/_secrets/delete", rootToken, `{"name":"apiKey"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = post("/api/plugin/psec-a/probe", userToken, `{}`)
	assert.JSONEq(t, `{"matches":false,"notSet":true}`, rec.Body.String())
}

// **queue だけのプロセスでもプラグインは値を読める。** bot はジョブで動くので、
// 入力口 (HTTP) を張らないロールでも鍵の付いた口を渡す必要がある。
func TestNew_QueueOnlyPluginsCanReadSecrets(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "1")
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB
	key := bytes.Repeat([]byte{43}, config.PluginSecretKeySize)
	clean := func() {
		require.NoError(t, db.Exec(`DELETE FROM "plugin_secret" WHERE "pluginName" = 'psec-queue'`).Error)
	}
	clean()
	t.Cleanup(clean)
	seed, err := pluginsecret.New(pgrepo.New(repository.NewPluginSecretRepository(db)), key)
	require.NoError(t, err)
	require.NoError(t, seed.Set(context.Background(), "psec-queue", "apiKey", "queue-value"))

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
		JobQueueDriver: "mkq", MediaProxySecret: []byte("test-secret"),
		Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
		PluginSecretKey: key,
	}

	var got string
	var getErr error
	def := plugin.Definition{
		Name: "psec-queue", APIVersion: plugin.APIVersion,
		Secrets: []plugin.SecretSpec{{Name: "apiKey"}},
		Jobs: func(ctx plugin.Context, jobs plugin.Jobs) error {
			got, getErr = ctx.Secrets().Get(context.Background(), "apiKey")
			jobs.Handle("noop", func(context.Context, json.RawMessage) error { return nil })
			return nil
		},
	}
	restoreProcessGlobals(t)
	srv, err := newServer(cfg, db, redisClients, []plugin.Definition{def}, noopStorage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	require.Equal(t, config.RoleQueue, srv.role)
	require.NoError(t, getErr)
	assert.Equal(t, "queue-value", got)
}

// **鍵が無いまま、秘密の値を宣言したプラグインを有効にしたら起動時に warn を出す。**
// 鍵があるとき・宣言が無いとき・無効なプラグインでは出さない。
func TestSetupPlugins_WarnsWhenSecretsDeclaredWithoutKey(t *testing.T) {
	run := func(withKey bool, defs []plugin.Definition, settings map[string]map[string]any) string {
		var buf bytes.Buffer
		restore := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(restore)
		s, api := newPluginTestServer(config.RoleServer)
		s.config.Plugins = settings
		if withKey {
			svc, err := pluginsecret.New(nil, bytes.Repeat([]byte{1}, pluginsecret.KeySize))
			require.NoError(t, err)
			s.pluginSecrets = svc
		}
		require.NoError(t, s.setupPlugins(api, defs, noopStorage))
		return buf.String()
	}
	bot := plugin.Definition{Name: "bot", APIVersion: plugin.APIVersion, Secrets: []plugin.SecretSpec{{Name: "apiKey"}},
		Routes: func(plugin.Context, plugin.Router) error { return nil }}
	plain := pluginDef("plain", func(plugin.Context, plugin.Router) error { return nil }, nil)

	got := run(false, []plugin.Definition{bot, plain}, nil)
	assert.Contains(t, got, "pluginSecretKey")
	assert.Contains(t, got, "plugins=bot")
	assert.NotContains(t, got, "plain")

	assert.NotContains(t, run(true, []plugin.Definition{bot}, nil), "pluginSecretKey", "鍵があるのに warn")
	assert.NotContains(t, run(false, []plugin.Definition{plain}, nil), "pluginSecretKey", "宣言が無いのに warn")
	assert.NotContains(t, run(false, []plugin.Definition{bot},
		map[string]map[string]any{"bot": {"enabled": false}}), "pluginSecretKey", "無効なプラグインで warn")
}
