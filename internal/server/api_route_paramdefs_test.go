package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/plugin"
)

// undeclaredAPIRoutes are the /api routes (the path after /api/) that have no
// paramDef, upstream's or Elythia's, on purpose, with where each is
// registered. Upstream's direct routes are not listed: they are read from
// directRoutes of upstream_paramdefs.json (upstreamDirectRoutes).
var undeclaredAPIRoutes = map[string]string{
	// router.goのapi.Any("/*", apiCatchall)。登録の無い/api/*に200と空のobjectを
	// 返すcatchallで、endpointではない。
	"*": "router.go: api.Any(\"/*\", apiCatchall)",
	// 本家の旧いpathの互換。本家のendpointの一覧にもdirectRoutesにも無い。
	"signin": "routes_auth.go: api.POST(\"/signin\", signinHandler.Signin)",
	// Elythia独自の分割アップロードの本文はmultipartで、拒否の検証はJSONでない
	// 本文を検証せずにhandlerへ渡す(SkipNotJSON)。宣言しても何も拒否しないので
	// 宣言しない(#3877)。
	"drive/files/create-chunked/append": "routes_drive.go: api.POST(\"/drive/files/create-chunked/append\", driveHandler.FilesCreateChunkedAppend, ...)",
}

// getRoutesWithoutAllowGet are the declared endpoints that Elythia also
// routes for GET although their paramDef has no meta.allowGet, with where
// the GET route is registered. The validator skips such a GET (SkipMethod),
// so only the handler checks it.
var getRoutesWithoutAllowGet = map[string]string{
	// 本家のnotes/reactionsはallowGetを持たないが、Elythiaは以前からGETも受ける
	// (docs/divergence/approximations.md)。
	"notes/reactions": "routes_notes.go: api.GET(\"/notes/reactions\", notesHandler.Reactions)",
}

// testModeOnlyRoutes are the declared endpoints that the server registers in
// test mode only, with where. The gate builds the server like production, so
// they have no route there.
var testModeOnlyRoutes = map[string]string{
	"reset-db": "router.go: api.POST(\"/reset-db\", testHandler.ResetDB) inside if s.config.TestMode",
}

// routeGatePlugin is the plugin the gate builds the server with, so that the
// plugin routes (/api/plugin/<name>/..., registered by setupPlugins without
// apiRoutes) are there to be told apart.
const routeGatePlugin = "routegate"

// upstreamDirectRoutes returns directRoutes of upstream_paramdefs.json: the
// /api routes upstream registers on fastify outside the endpoint machinery
// (ApiServerService.ts), which have no paramDef.
func upstreamDirectRoutes(t *testing.T) map[string]bool {
	t.Helper()
	var g struct {
		DirectRoutes []string `json:"directRoutes"`
	}
	require.NoError(t, json.Unmarshal(paramschema.UpstreamGolden(), &g))
	out := make(map[string]bool, len(g.DirectRoutes))
	for _, name := range g.DirectRoutes {
		out[name] = true
	}
	// 書式が変わって1件も読めないと、下のゲートは「本家が直に登録するroute」を
	// 宣言の無いrouteとして落とすので空振りはしないが、読めていることを名指しで
	// 確かめる。
	for _, name := range []string{"signup", "signin-flow", "miauth/:session/check", "v1/instance/peers"} {
		require.True(t, out[name], "directRoutes of upstream_paramdefs.json has no %s", name)
	}
	return out
}

// builtAPIRoutes builds the server like production (not in test mode, with
// one plugin that has a route) and returns its /api routes: the path after
// /api/ to the set of methods. echo's RouteNotFound routes, which echo adds
// for the groups with middleware, are left out. It also returns the
// namespaces of the plugins it was built with ("plugin/<name>/").
func builtAPIRoutes(t *testing.T) (routes map[string]map[string]bool, pluginNamespaces []string) {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	clients := &cache.RedisClients{Default: rc, Pubsub: rc, JobQueue: rc, Timelines: rc, Reactions: rc}
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	ro := config.RedisOptions{Host: mr.Host(), Port: port}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx",
		Redis: ro, RedisForPubsub: ro, RedisForJobQueue: ro, RedisForTimelines: ro, RedisForReactions: ro,
	}
	defs := append(plugin.Registered(), plugin.Definition{
		Name: routeGatePlugin, APIVersion: plugin.APIVersion,
		Routes: func(_ plugin.Context, r plugin.Router) error {
			r.POST("/x", func(plugin.Request) (any, error) { return nil, nil })
			return nil
		},
	})
	for _, def := range defs {
		pluginNamespaces = append(pluginNamespaces, strings.TrimPrefix(pluginRoutePrefix, "/")+def.Name+"/")
	}
	srv, err := newServer(cfg, serverIntegrationDB, clients, defs, noopStorage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	routes = map[string]map[string]bool{}
	for _, r := range srv.echo.Routes() {
		name, ok := strings.CutPrefix(r.Path, apiGroupPrefix+"/")
		if !ok || r.Method == echo.RouteNotFound {
			continue
		}
		if routes[name] == nil {
			routes[name] = map[string]bool{}
		}
		routes[name][r.Method] = true
	}
	return routes, pluginNamespaces
}

// TestAPIRoutesHaveParamDefs is the gate of #3890: every /api route of the
// built server has a paramDef, upstream's (upstream_paramdefs.json) or
// Elythia's own (elythia_paramdefs.json), so that its params are validated
// and rejected with upstream's 400; and every paramDef names a POST route, so
// that a declaration does not outlive its route. The exceptions are the
// lists above, upstream's direct routes and the routes under the namespace of
// each plugin the server is built with. That the routes actually run the
// validation (withParamShadow) is checked by TestEnforcedEndpointsMatchTheCorpus,
// which sends the rejected corpus inputs of every enforced endpoint.
//
// プラグインの名前空間(plugin/<名前>/)は、組み立てに渡したプラグインの名前で
// 絞って丸ごと外す。本体がその名前空間に張る受け口(peerの受信、秘密の値の
// 入力口。plugin_wiring.go)も、プラグインのgroupに登録しapiRoutesを通らない
// ので、同じ例外に入る。名前で絞らずplugin/の下を全部外すと、本体のルートを
// plugin/の下に登録しただけでゲートを抜ける。
//
// ルートは、本番と同じく組み立てたサーバー(echo.Routes())から数える。ソースを
// 静的に読むと、api.Matchで登録するcharts/*のような形を取りこぼす。
func TestAPIRoutesHaveParamDefs(t *testing.T) {
	if serverIntegrationDB == nil {
		// CIではTestMainが落とすので、ここには来ない(requireDBEnv)。
		t.Skip("PostgreSQL unavailable")
	}
	eps, err := paramschema.Effective()
	require.NoError(t, err)
	direct := upstreamDirectRoutes(t)
	routes, pluginNamespaces := builtAPIRoutes(t)

	// 抽出の下限。実在するrouteを名指しで要求する。charts/notesはapi.Matchで
	// POSTとGETを登録する。
	require.Greater(t, len(routes), 500, "the /api routes of the built server were not read")
	for name, methods := range map[string][]string{
		"notes/create":      {http.MethodPost},
		"charts/notes":      {http.MethodPost, http.MethodGet},
		"admin/backup/list": {http.MethodPost},
		"signup":            {http.MethodPost},
		"v1/instance/peers": {http.MethodGet},
	} {
		for _, m := range methods {
			require.True(t, routes[name][m], "the built server has no %s /api/%s", m, name)
		}
	}

	posted := map[string]bool{}
	usedUndeclared := map[string]bool{}
	usedGet := map[string]bool{}
	pluginRoutes := 0
	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		methods := routes[name]
		if slices.ContainsFunc(pluginNamespaces, func(ns string) bool { return strings.HasPrefix(name, ns) }) {
			pluginRoutes++
			continue
		}
		if _, ok := undeclaredAPIRoutes[name]; ok {
			usedUndeclared[name] = true
			_, declared := eps[name]
			assert.False(t, declared, "/api/%s is declared now; drop it from undeclaredAPIRoutes", name)
			continue
		}
		if direct[name] {
			continue
		}
		ep, ok := eps[name]
		if !ok {
			t.Errorf("/api/%s has no paramDef: declare it in elythia_paramdefs.json (docs/param-shadow.md), or add it to undeclaredAPIRoutes with where it is registered", name)
			continue
		}
		for m := range methods {
			switch m {
			case http.MethodPost:
				posted[name] = true
			case http.MethodGet:
				if ep.AllowGet {
					continue
				}
				if _, ok := getRoutesWithoutAllowGet[name]; ok {
					usedGet[name] = true
					continue
				}
				t.Errorf("GET /api/%s is routed, but its paramDef has no meta.allowGet, so the validator skips it; add it to getRoutesWithoutAllowGet with the reason", name)
			default:
				t.Errorf("%s /api/%s is routed; the paramDef validates POST and, with meta.allowGet, GET only", m, name)
			}
		}
	}

	// 逆向き: 宣言があるのにPOSTのrouteが無いもの。
	for name := range eps {
		if posted[name] {
			continue
		}
		if _, ok := testModeOnlyRoutes[name]; ok {
			continue
		}
		t.Errorf("%s has a paramDef, but the built server has no POST /api/%s", name, name)
	}

	// 例外の一覧に、使われていない項目を残さない。
	for name, where := range undeclaredAPIRoutes {
		assert.True(t, usedUndeclared[name], "undeclaredAPIRoutes has %s (%s), but the built server does not route it", name, where)
	}
	for name, where := range getRoutesWithoutAllowGet {
		assert.True(t, usedGet[name], "getRoutesWithoutAllowGet has %s (%s), but the built server does not route GET for it, or its paramDef has meta.allowGet", name, where)
	}
	for name, where := range testModeOnlyRoutes {
		_, declared := eps[name]
		assert.True(t, declared, "testModeOnlyRoutes has %s (%s), which has no paramDef", name, where)
		assert.Empty(t, routes[name], "testModeOnlyRoutes has %s (%s), but the server routes it outside test mode", name, where)
	}
	assert.Positive(t, pluginRoutes, "the built server has no route of plugin %s under /api%s", routeGatePlugin, pluginRoutePrefix)
}
