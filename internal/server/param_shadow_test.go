package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
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

// TestParamShadowMiddlewareDisabled checks that disableParamShadow stops
// only the counting: the enforced endpoints still reject with upstream's 400
// (#3742), and the other upstream endpoints are neither validated nor
// counted.
func TestParamShadowMiddlewareDisabled(t *testing.T) {
	logs := captureDefaultLog(t)
	s := &Server{config: &config.Config{DisableParamShadow: true, ParamShadowLogSeconds: 3600}}
	mw := s.paramShadowMiddleware()
	require.NotNil(t, mw)

	e := echo.New()
	api := apiRoutes{e.Group("/api")}
	api.Use(mw)
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	api.POST("/clips/show", ok)
	api.POST("/i/pin", ok)
	serve := func(target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	rec := serve("/api/clips/show", `{"clipId":"a-b"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"param":"#/properties/clipId/format"`)
	assert.Equal(t, http.StatusNoContent, serve("/api/i/pin", `{"noteId":"a-b"}`).Code)

	require.NoError(t, s.lifecycle.stopAll(context.Background()))
	assert.NotContains(t, logs.String(), "paramshadow divergence")
	assert.NotContains(t, logs.String(), "i/pin")
}

// TestParamShadowRunsAfterTheRouteMiddleware pins the wiring of
// apiRoutes: the group middleware enables the validation, and it runs after
// the route's own middleware (a request the route denies is not counted) for
// POST, Match and GET, and only for upstream endpoints. The shutdown hook
// writes the counts.
func TestParamShadowRunsAfterTheRouteMiddleware(t *testing.T) {
	logs := captureDefaultLog(t)
	s := &Server{config: &config.Config{ParamShadowLogSeconds: 3600}}
	mw := s.paramShadowMiddleware()
	require.NotNil(t, mw)
	require.Equal(t, []string{"paramShadowRecorder"}, s.lifecycle.names())

	e := echo.New()
	api := apiRoutes{e.Group("/api")}
	api.Use(mw)
	deny := func(echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { return c.NoContent(http.StatusUnauthorized) }
	}
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	api.POST("/i/pin", ok)
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
	assert.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/i/pin", `{"noteId":"a-b"}`))
	assert.Equal(t, http.StatusUnauthorized, serve(http.MethodPost, "/api/notes/create", `{"text":""}`))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodGet, "/api/federation/instances?limit=0", ""))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/federation/instances", `{"limit":101}`))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodGet, "/api/federation/instances?x=1", ""))
	// 数えるだけのallowGetのendpointは、federation/instancesのほかに無い(#3847)。
	// GETの登録は、本家に揃えたcharts/notesが検証で拒否することで確かめる。
	assert.Equal(t, http.StatusNoContent, serve(http.MethodGet, "/api/charts/notes?span=day", ""))
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodGet, "/api/charts/notes?span=week", ""))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/elythia-only", `{"a":1}`))
	// requireObjectBody は検証の内側で答える(検証器も#/typeで拒否するので一致)。
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/i/pin", `null`))

	require.NoError(t, s.lifecycle.stopAll(context.Background()))
	out := logs.String()
	for _, want := range []string{
		`"endpoint":"i/pin","kind":"rejected_handler_ok","schemaPath":"#/properties/noteId/format"`,
		`"endpoint":"federation/instances","kind":"rejected_handler_ok","schemaPath":"#/properties/limit/minimum"`,
		`"endpoint":"federation/instances","kind":"rejected_handler_ok","schemaPath":"#/properties/limit/maximum"`,
		`"msg":"paramshadow summary"`,
		`"agreed":2,"diverged":3,"skipped":0`,
	} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, `"endpoint":"notes/create"`, "a request the route denied was validated")
	assert.NotContains(t, out, "elythia-only")
}

func TestWithParamShadowWithoutTheGroupMiddleware(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/api/notes/show", nil), httptest.NewRecorder())
	called := false
	require.NoError(t, withParamShadow(func(echo.Context) error { called = true; return nil })(c))
	assert.True(t, called)
}

// newParamShadowServers builds two real servers (PostgreSQL) on one Redis,
// one without the param shadow validation and one with it. The second is
// built after the log capture starts, so its counts go to the capture. opts
// change the config of both.
func newParamShadowServers(t *testing.T, opts ...func(*config.Config)) (off, on *Server, logs *syncBuffer) {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
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
	build := func(disable bool) *Server {
		cfg := &config.Config{
			URL: "http://example.test", Host: "example.test", Hostname: "example.test",
			Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
			Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
			RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
			DisableParamShadow: disable, ParamShadowLogSeconds: 3600,
		}
		for _, opt := range opts {
			opt(cfg)
		}
		srv, err := New(cfg, serverIntegrationDB, redisClients)
		require.NoError(t, err)
		return srv
	}
	off = build(true)
	t.Cleanup(func() { require.NoError(t, off.Shutdown(context.Background())) })
	logs = captureDefaultLog(t)
	on = build(false)
	return off, on, logs
}

// TestParamShadowChangesNoResponse sends representative requests, including
// ones the validator and the handler judge differently, to a server with the
// validation and one without, and compares the answers. It also checks that
// the server with the validation counted them, so the comparison is not
// vacuous.
func TestParamShadowChangesNoResponse(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB
	const userID, token = "pshadow-user", "pshadowtoken0001"
	cleanup := func() {
		require.NoError(t, db.Exec(`DELETE FROM note WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Where("id = ?", userID).Delete(&model.User{}).Error)
	}
	cleanup()
	t.Cleanup(cleanup)
	tok := token
	require.NoError(t, repository.NewUserRepository(db).Create(&model.User{
		ID: userID, Username: "pshadow", UsernameLower: "pshadow", Token: &tok,
	}))

	off, on, logs := newParamShadowServers(t)

	type request struct {
		method, path, body string
		auth               bool
		// mutates: the request changes the DB, so the two answers can differ
		// in ids and dates; only the status and the error code are compared.
		mutates bool
	}
	requests := []request{
		{http.MethodPost, "/api/notes/create", `{"text":"hello"}`, true, true},
		{http.MethodPost, "/api/notes/create", `{"text":"a","fileIds":[]}`, true, true},
		{http.MethodPost, "/api/notes/create", `{"text":"a","visibility":""}`, true, true},
		{http.MethodPost, "/api/notes/create", `{"text":""}`, true, true},
		{http.MethodPost, "/api/notes/create", `{"text":"hello"}`, false, true},
		// 数えるだけの例は、#3851 で本家に揃える i/* を避け、i/notifications・
		// notes/create・reversi/match に置く。
		{http.MethodPost, "/api/i/notifications", `{"limit":10}`, true, false},
		{http.MethodPost, "/api/i/notifications", `{"limit":0}`, true, false},
		{http.MethodPost, "/api/i/notifications", `{"limit":10.0}`, true, false},
		{http.MethodPost, "/api/i/notifications", `{"limit":1e400}`, true, false},
		{http.MethodPost, "/api/notes/create", `{"text":"a","poll":{"choices":["a","b"],"expiredAfter":1e4}}`, true, true},
		{http.MethodPost, "/api/reversi/match", `{"userId":"abc-def"}`, true, false},
		{http.MethodPost, "/api/i/update", `{"isCat":null}`, true, true},
		{http.MethodPost, "/api/i/update", `{"name":"影の検証"}`, true, true},
		{http.MethodPost, "/api/i", `{}`, true, false},
		{http.MethodPost, "/api/i/notifications", `{"limit":"yes"}`, true, false},
		{http.MethodPost, "/api/reversi/match", `null`, true, false},
		{http.MethodPost, "/api/reversi/match", "{\"userId\":\"\\ud800\"}", true, false},
		{http.MethodGet, "/api/charts/notes?span=day&limit=x", "", false, false},
		{http.MethodGet, "/api/charts/notes?span=day&limit=2", "", false, false},
		{http.MethodGet, "/api/federation/instances", "", false, false},
	}
	send := func(srv *Server, r request) *httptest.ResponseRecorder {
		var req *http.Request
		if r.method == http.MethodGet {
			req = httptest.NewRequest(r.method, r.path, nil)
		} else {
			req = httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		}
		if r.auth {
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	errorCode := func(b []byte) string {
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &env)
		return env.Error.Code
	}
	for _, r := range requests {
		a, b := send(off, r), send(on, r)
		name := r.method + " " + r.path + " " + r.body
		assert.Equal(t, a.Code, b.Code, name)
		for _, h := range []string{"Date", "X-Request-Id"} {
			a.Header().Del(h)
			b.Header().Del(h)
		}
		assert.Equal(t, a.Header(), b.Header(), name)
		if r.mutates {
			assert.Equal(t, errorCode(a.Body.Bytes()), errorCode(b.Body.Bytes()), name)
			continue
		}
		assert.Equal(t, a.Body.String(), b.Body.String(), name)
	}

	require.NoError(t, on.Shutdown(context.Background()))
	out := logs.String()
	for _, want := range []string{
		`"endpoint":"notes/create","kind":"rejected_handler_ok","schemaPath":"#/properties/fileIds/minItems"`,
		`"endpoint":"notes/create","kind":"rejected_handler_ok","schemaPath":"#/properties/visibility/enum"`,
		`"endpoint":"reversi/match","kind":"rejected_handler_other_error","schemaPath":"#/properties/userId/format"`,
		`"endpoint":"i/update","kind":"rejected_handler_ok","schemaPath":"#/properties/isCat/type"`,
		`"endpoint":"notes/create","kind":"accepted_handler_invalid_param","schemaPath":""`,
		`"endpoint":"i/notifications","kind":"param_mismatch","schemaPath":"#/properties/limit/minimum"`,
		`"msg":"paramshadow summary"`,
		`"lone_surrogate":1`,
	} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, token)
	assert.NotContains(t, out, "影の検証")
}

// apiRegistrationAllowed are the /api registrations that bypass apiRoutes'
// POST / Match / GET (and so withParamShadow and requireObjectBody) on
// purpose, with the path by which each is reached.
var apiRegistrationAllowed = map[string]string{
	// 登録の無い/api/*に200と空のobjectを返すcatchall。goldenのendpointではない。
	`api.Any("/*", apiCatchall)`: "router.go",
	// プラグインのroute(/api/plugin/<名前>/...)。Elythia独自で、goldenに無い。
	`api.Group(pluginRoutePrefix+def.Name, middleware.RejectAppToken())`: "plugin_wiring.go",
}

// apiBypassingRegistration matches a registration on the /api group that
// does not go through apiRoutes' POST / Match / GET, and one on the echo
// instance under /api/.
var apiBypassingRegistration = regexp.MustCompile(
	`\bapi\.(?:Group\.|Group\(|Add\(|Any\(|PUT\(|PATCH\(|DELETE\(|HEAD\(|OPTIONS\(|CONNECT\(|TRACE\(|RouteNotFound\()[^\n]*` +
		`|\becho\.(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Add|Any|Match)\([^\n]*"/api/[^\n]*`)

// TestAPIRegistrationsGoThroughAPIRoutes is the gate of the #3677 review
// (L-5): an /api route registered without apiRoutes gets neither
// withParamShadow nor requireObjectBody, and nothing else fails.
//
// 名前で見る走査なので、普通に書いたときに踏む形(api.Group.POST、api.Add、
// api.PUTなど)だけを落とす。次の形は見つけられない(docs/gates.mdの既知の限界)。
//   - *echo.Group型の引数で受け取ったgroupへの登録(registerX(api.Group))
//   - s.echo.Group("/api")で作った別のgroupへの登録
//   - g := api.Groupのような別名を通した登録
//   - "/api/"を定数で書いたs.echo.POST
//   - e := s.echoのようにechoを別名にしたe.POST("/api/x", h)
//   - pathをraw stringで書いたs.echo.POST(`/api/x`, h)
//
// この列挙は網羅ではない。
//
// goldenのendpointをこれらの形へ移すと、TestAPICompatDoc_MatchesRouterと
// TestParamShadowChangesNoResponseが落とす。
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
	} {
		assert.False(t, apiBypassingRegistration.MatchString(line), line)
	}
}

// blockingSummaryHandler blocks the goroutine that writes the paramshadow
// summary until release is closed.
type blockingSummaryHandler struct {
	slog.Handler
	writing chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (h blockingSummaryHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "paramshadow summary" {
		h.once.Do(func() { close(h.writing) })
		<-h.release
	}
	return h.Handler.Handle(ctx, r)
}

// TestParamShadowSummaryIsWrittenOffTheRequest pins that the periodic
// summary is written by the generation's background group, not by the
// request that found the interval passed (#3677 review L-2): the request
// completes while the summary is still being written.
func TestParamShadowSummaryIsWrittenOffTheRequest(t *testing.T) {
	h := blockingSummaryHandler{
		Handler: slog.NewJSONHandler(&syncBuffer{}, nil),
		writing: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{},
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := &Server{config: &config.Config{ParamShadowLogSeconds: 1}, background: bgwork.New()}
	mw := s.paramShadowMiddleware()
	e := echo.New()
	api := apiRoutes{e.Group("/api")}
	api.Use(mw)
	api.POST("/i/pin", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
	send := func() {
		req := httptest.NewRequest(http.MethodPost, "/api/i/pin", strings.NewReader(`{"noteId":"abc"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		e.ServeHTTP(httptest.NewRecorder(), req)
	}
	send()
	time.Sleep(1100 * time.Millisecond)
	done := make(chan struct{})
	go func() { send(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(h.release)
		t.Fatal("the request waited for the summary to be written")
	}
	select {
	case <-h.writing:
	case <-time.After(5 * time.Second):
		t.Fatal("the summary was not written")
	}
	close(h.release)
	s.background.Stop(context.Background())
}

// TestParamShadowSummaryAfterTheGenerationStopped pins that once the
// background group is stopping, the summary is written by the request
// itself without the group counting it as dropped work (#3677 review 2 L-1).
func TestParamShadowSummaryAfterTheGenerationStopped(t *testing.T) {
	logs := captureDefaultLog(t)
	s := &Server{config: &config.Config{ParamShadowLogSeconds: 1}, background: bgwork.New()}
	mw := s.paramShadowMiddleware()
	e := echo.New()
	api := apiRoutes{e.Group("/api")}
	api.Use(mw)
	api.POST("/i/pin", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
	send := func() {
		req := httptest.NewRequest(http.MethodPost, "/api/i/pin", strings.NewReader(`{"noteId":"abc"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		e.ServeHTTP(httptest.NewRecorder(), req)
	}
	send()
	s.background.Stop(context.Background())
	time.Sleep(1100 * time.Millisecond)
	send()
	assert.Zero(t, s.background.Dropped(), "the summary was counted as dropped work")
	out := logs.String()
	assert.NotContains(t, out, "background work dropped")
	assert.Contains(t, out, `"msg":"paramshadow summary"`)
	assert.Contains(t, out, `"agreed":2`)
}
