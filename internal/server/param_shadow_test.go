package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureDefaultLog points slog.Default at a buffer until the test ends.
func captureDefaultLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestParamShadowRunsAfterTheRouteMiddleware pins the wiring of
// apiRoutes: the group middleware enables the validation, and it runs after
// the route's own middleware (a request the route denies gets the route's
// answer, not the validator's 400) for POST, Match and GET, and only for the
// endpoints with a paramDef. It adds no lifecycle part.
func TestParamShadowRunsAfterTheRouteMiddleware(t *testing.T) {
	s := &Server{config: &config.Config{}}
	mw := s.paramShadowMiddleware()
	require.NotNil(t, mw)
	require.Empty(t, s.lifecycle.names())

	e := echo.New()
	api := apiRoutes{e.Group("/api")}
	api.Use(mw)
	deny := func(echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { return c.NoContent(http.StatusUnauthorized) }
	}
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	api.POST("/admin/update-meta", ok)
	api.POST("/notes/create", ok, deny)
	api.Match([]string{http.MethodGet, http.MethodPost}, "/federation/instances", ok)
	api.GET("/charts/notes", ok)
	api.POST("/elythia-only", ok)

	serve := func(method, target, body string) int {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/admin/update-meta", `{"themeColor":"#fff"}`))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/admin/update-meta", `{"themeColor":"#ffffff"}`))
	// routeのmiddlewareが断るリクエストは、宣言から外れる本文でも検証に届かない。
	assert.Equal(t, http.StatusUnauthorized, serve(http.MethodPost, "/api/notes/create", `{"visibility":"x"}`))
	// Match(GETとPOST)の登録はfederation/instancesが、GETの登録はcharts/notesが、
	// どちらでも検証して拒否することで確かめる。
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodGet, "/api/federation/instances?limit=0", ""))
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/federation/instances", `{"limit":101}`))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodGet, "/api/federation/instances?x=1", ""))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodGet, "/api/charts/notes?span=day", ""))
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodGet, "/api/charts/notes?span=week", ""))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/elythia-only", `{"a":1}`))
	// requireObjectBody は検証の内側で答える(検証器も#/typeで拒否するので一致)。
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/admin/update-meta", `null`))
}

func TestWithParamShadowWithoutTheGroupMiddleware(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/api/notes/show", nil), httptest.NewRecorder())
	called := false
	require.NoError(t, withParamShadow(func(echo.Context) error { called = true; return nil })(c))
	assert.True(t, called)
}

// newParamServer builds a real server (PostgreSQL) on a miniredis.
func newParamServer(t *testing.T) *Server {
	t.Helper()
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	redisPort, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	return newParamServerOn(t, redisClient, config.RedisOptions{Host: mr.Host(), Port: redisPort}, nil)
}

// newRealRedis starts a Redis container for t and returns a client on it and
// its options.
//
// **ジョブを積む endpoint を通すテストは、miniredis でなく実 Redis を使う (#3369)。**
// miniredis には mkq の Lua が使う cmsgpack が無く、ジョブの投入が必ず失敗して
// 500 になる (本番の挙動と違う)。testcontainers で立てるので Docker が要る。
// **手元で Docker が無いと、使うテスト (TestEnforcedEndpointsMatchTheCorpus) は
// skip する** (testutil.SkipIfNoDocker)。CI (環境変数 CI がある) では skip せず落ちる。
func newRealRedis(t *testing.T) (*redis.Client, config.RedisOptions) {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	tr, err := testutil.SetupRedis(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { tr.Teardown(context.Background()) })
	return tr.Client, config.RedisOptions{Host: tr.Host(), Port: tr.Port()}
}

// newParamServerOn is newParamServer on the Redis redisClient talks to
// (described by redisOptions for the queue driver), with beforeSetup
// functions applied to the server.
func newParamServerOn(t *testing.T, redisClient *redis.Client, redisOptions config.RedisOptions, beforeSetup []func(*Server), opts ...func(*config.Config)) *Server {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	redisClients := &cache.RedisClients{
		Default: redisClient, Pubsub: redisClient, JobQueue: redisClient,
		Timelines: redisClient, Reactions: redisClient,
	}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	srv, err := newServer(cfg, serverIntegrationDB, redisClients, plugin.Registered(), nil, beforeSetup...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return srv
}

// apiRegistrationAllowed are the /api registrations that bypass apiRoutes'
// POST / Match / GET (and so withParamShadow and requireObjectBody) on
// purpose, with the path by which each is reached.
var apiRegistrationAllowed = map[string]string{
	// 登録の無い/api/*に200と空のobjectを返すcatchall。goldenのendpointではない。
	`api.Any("/*", apiCatchall)`: "router.go",
	// 本家がendpointの外でfastify.allに直接登録するroute(#3926)。frontendが
	// 設定画面のキャッシュの削除からGETで呼ぶ。本文を読まないので本文の検査は要らない。
	`api.Any("/clear-browser-cache", meta.ClearBrowserCache)`: "router.go",
	// プラグインのroute(/api/plugin/<名前>/...)。Elythia独自で、goldenに無い。
	`api.Group(pluginRoutePrefix+def.Name, middleware.RejectAppToken())`: "plugin_wiring.go",
	// 上の登録をするsetupPluginsへ、/apiのgroupを渡す。
	`s.setupPlugins(api.Group, registeredPlugins, openPluginStorage)`: "router.go",
}

// apiBypassingRegistration matches a registration on the /api group that
// does not go through apiRoutes' POST / Match / GET, a use of the /api group
// as a value (passed to a function or aliased), and one on the echo instance
// under /api/.
//
// **値として使う`api.Group`も落とす (#3369 I10)。** `addXRoutes(api.Group, h)`や
// `g := api.Group`を通すと、受け取った側の登録はどの名前の走査にも掛からない。
var apiBypassingRegistration = regexp.MustCompile(
	`[\w.]+\(api\.Group\b[^\n]*?\)` +
		`|\bapi\.Group(?:[^.(\w]|$)[^\n]*` +
		`|\bapi\.(?:Group\.|Group\(|Add\(|Any\(|PUT\(|PATCH\(|DELETE\(|HEAD\(|OPTIONS\(|CONNECT\(|TRACE\(|RouteNotFound\()[^\n]*` +
		`|\becho\.(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Add|Any|Match)\([^\n]*"/api/[^\n]*`)

// TestAPIRegistrationsGoThroughAPIRoutes is the gate of the #3677 review
// (L-5): an /api route registered without apiRoutes gets neither
// withParamShadow nor requireObjectBody, and nothing else fails.
//
// 名前で見る走査なので、普通に書いたときに踏む形(api.Group.POST、api.Add、
// api.PUTなど)だけを落とす。次の形は見つけられない(docs/gates.mdの既知の限界)。
//   - s.echo.Group("/api")で作った別のgroupへの登録
//   - "/api/"を定数で書いたs.echo.POST
//   - e := s.echoのようにechoを別名にしたe.POST("/api/x", h)
//   - pathをraw stringで書いたs.echo.POST(`/api/x`, h)
//
// この列挙は網羅ではない。
//
// goldenのendpointをこれらの形へ移すと、TestAPICompatDoc_MatchesRouterと
// TestEnforcedEndpointsMatchTheCorpusが落とす(後者は、paramDefが{}の2件では
// 落ちない。docs/gates.md)。
//
// **許可した登録は、書いてあるファイルでだけ許す (#3697)。** ルートの登録を
// routes_<領域>.goへ分けたので、catchallを別のファイル(登録の関数の中など)へ
// 移すと、プラグインのルートより前に登録される形になりうる。ファイルごとに
// 照合し、許可リストのファイルと違えば落とす。
func TestAPIRegistrationsGoThroughAPIRoutes(t *testing.T) {
	files := packageSources(t)
	used := map[string]bool{}
	posts := 0
	for _, f := range files {
		src := stripComments(f.src)
		posts += strings.Count(src, "api.POST(")
		for _, m := range apiBypassingRegistration.FindAllString(src, -1) {
			call := strings.TrimSpace(m)
			if file, ok := apiRegistrationAllowed[call]; ok {
				assert.Equalf(t, file, f.name, "%s is allowed only in %s", call, file)
				used[call] = true
				continue
			}
			t.Errorf("an /api route bypasses apiRoutes in %s: %s (register it with api.POST / api.Match / api.GET, or add it to apiRegistrationAllowed with the reason)", f.name, call)
		}
	}
	for call, file := range apiRegistrationAllowed {
		assert.Truef(t, used[call], "apiRegistrationAllowed has %s (%s) but the source no longer does", call, file)
	}
	// 何も拾えなかったら落とす。apiRoutesを通る登録が読めていることも見る。
	assert.Greater(t, posts, 400, "the /api POST registrations were not read")
}

func TestAPIBypassingRegistrationPattern(t *testing.T) {
	for _, line := range []string{
		`api.Group.POST("/x", h)`,
		`api.Add(http.MethodPost, "/x", h)`,
		`api.PUT("/x", h)`,
		`s.echo.POST("/api/notes/create", h)`,
		`e.echo.Match(methods, "/api/x", h)`,
		`addXRoutes(api.Group, h)`,
		`registerX(h, api.Group)`,
		`g := api.Group`,
		`return api.Group`,
	} {
		assert.True(t, apiBypassingRegistration.MatchString(line), line)
	}
	for _, line := range []string{
		`api.POST("/x", h)`,
		`api.Match(methods, "/x", h)`,
		`api.GET("/x", h)`,
		`api.Use(mw)`,
		`s.echo.GET("/api.json", h)`,
		`apiGroup.PUT("/x", h)`,
		`api.Groups("/x")`,
	} {
		assert.False(t, apiBypassingRegistration.MatchString(line), line)
	}
}
