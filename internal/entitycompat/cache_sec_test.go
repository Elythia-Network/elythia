package entitycompat

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/server/middleware"
)

// embeddedCacheSecJSON is the committed golden map of every upstream
// `allowGet: true` endpoint -> its `cacheSec` (0 when it has none), generated
// by `go run ./tools/permspec`.
//
//go:embed testdata/golden_cache_sec.json
var embeddedCacheSecJSON []byte

var (
	// apiGetRouteRe matches `api.GET("/path"`.
	apiGetRouteRe = regexp.MustCompile(`\bapi\.GET\("(/[^"]*)"`)
	// apiMatchRouteRe matches `api.Match(<methods>, "/path"`, where methods is
	// an identifier or a []string literal.
	apiMatchRouteRe = regexp.MustCompile(`\bapi\.Match\(\s*([A-Za-z_][A-Za-z0-9_]*|\[\]string\{[^}]*\})\s*,\s*"(/[^"]*)"`)
	// apiCacheControlUseRe matches the /api group mounting APICacheControl.
	apiCacheControlUseRe = regexp.MustCompile(`\bapi\.Use\(\s*middleware\.APICacheControl\(\)\s*\)`)
	// methodsVarRe matches `name := []string{...}`.
	methodsVarRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*\[\]string\{([^}]*)\}`)
)

// parseGetRoutes returns the /api endpoints router.go registers for GET,
// either with api.GET or with api.Match whose method list holds GET.
func parseGetRoutes(t *testing.T, path string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read router: %v", err)
	}
	s := stripGoComments(string(src))
	methodVars := map[string]string{}
	for _, m := range methodsVarRe.FindAllStringSubmatch(s, -1) {
		methodVars[m[1]] = m[2]
	}
	hasGet := func(list string) bool {
		return strings.Contains(list, "http.MethodGet") || strings.Contains(list, `"GET"`)
	}
	out := map[string]bool{}
	for _, m := range apiGetRouteRe.FindAllStringSubmatch(s, -1) {
		out[strings.TrimPrefix(m[1], "/")] = true
	}
	for _, m := range apiMatchRouteRe.FindAllStringSubmatch(s, -1) {
		list := m[1]
		if !strings.HasPrefix(list, "[]string") {
			list = methodVars[list]
		}
		if hasGet(list) {
			out[strings.TrimPrefix(m[2], "/")] = true
		}
	}
	return out
}

// TestCacheSecDrift keeps the GET surface and the public Cache-Control of the
// API in line with upstream's endpoint meta:
//
//   - every `allowGet: true` endpoint has a GET route (upstream's frontend
//     calls some of them with GET, e.g. `misskeyApiGet('emojis')`)
//   - middleware.APICacheSec holds exactly the upstream `cacheSec` values
//
// #3593 の時点で、GET の route が無いもの 2 本 (users/featured-notes、
// retention) と、cacheSec の Cache-Control が付かないもの 8 本があった。
// handler ごとに書く形では付け忘れを検出できない。
func TestCacheSecDrift(t *testing.T) {
	root := repoRoot(t)

	var golden map[string]int
	if err := json.Unmarshal(embeddedCacheSecJSON, &golden); err != nil {
		t.Fatalf("parse golden_cache_sec.json: %v", err)
	}
	// 拾えなくなったら落とす。名指しで要求して、抽出が空振りしたまま緑に
	// なるのを防ぐ。
	if len(golden) < 20 || golden["emojis"] != 3600 {
		t.Fatalf("cacheSec golden looks broken (%d entries, emojis=%d); regenerate with `make shapecheck-gen`", len(golden), golden["emojis"])
	}

	routerPath := filepath.Join(root, "internal/server/router.go")
	// 表があっても、/api の group に middleware を付けていなければ何も起きない。
	src, err := os.ReadFile(routerPath)
	if err != nil {
		t.Fatalf("read router: %v", err)
	}
	if !apiCacheControlUseRe.MatchString(stripGoComments(string(src))) {
		t.Fatal("router.go must mount middleware.APICacheControl() on the /api group (api.Use)")
	}

	gets := parseGetRoutes(t, routerPath)
	if len(gets) < 20 || !gets["emojis"] || !gets["charts/notes"] {
		t.Fatalf("parsed %d GET routes from router.go without emojis / charts/notes; the parser likely broke", len(gets))
	}

	var problems []string
	for ep, sec := range golden {
		if !gets[ep] {
			problems = append(problems, ep+": upstream allowGet, but router.go has no GET route")
		}
		got, ok := middleware.APICacheSec[ep]
		switch {
		case sec > 0 && !ok:
			problems = append(problems, ep+": upstream cacheSec missing from middleware.APICacheSec")
		case ok && got != sec:
			problems = append(problems, ep+": middleware.APICacheSec differs from upstream cacheSec")
		}
	}
	for ep := range middleware.APICacheSec {
		if golden[ep] == 0 {
			problems = append(problems, ep+": in middleware.APICacheSec but upstream has no allowGet + cacheSec")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatal("allowGet / cacheSec drift against upstream:\n  " + strings.Join(problems, "\n  "))
	}
}
