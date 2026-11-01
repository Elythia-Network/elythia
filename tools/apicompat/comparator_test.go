package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCollectTSEndpoints(t *testing.T) {
	tmp := t.TempDir()
	// Build a small fake Misskey endpoints tree:
	//   <tmp>/meta.ts                   (endpoint)
	//   <tmp>/admin/foo.ts              (endpoint)
	//   <tmp>/admin/bar/baz.ts          (endpoint)
	//   <tmp>/endpoint.ts               (endpoint — meta tag in TS)
	//   <tmp>/endpoints.ts              (endpoint — meta tag in TS)
	//   <tmp>/legacy.d.ts               (excluded: type declaration)
	//   <tmp>/admin/README.md           (excluded: non-ts)
	//   <tmp>/admin/foo.test.ts         (excluded: Vitest unit test)
	//   <tmp>/notes/create.test.ts      (excluded: Vitest unit test)
	mustWrite(t, filepath.Join(tmp, "meta.ts"), "")
	mustWrite(t, filepath.Join(tmp, "admin", "foo.ts"), "")
	mustWrite(t, filepath.Join(tmp, "admin", "bar", "baz.ts"), "")
	// `endpoint.ts` / `endpoints.ts` are real Misskey API endpoints (meta tag),
	// not plumbing — must NOT be skipped.
	mustWrite(t, filepath.Join(tmp, "endpoint.ts"), "")
	mustWrite(t, filepath.Join(tmp, "endpoints.ts"), "")
	mustWrite(t, filepath.Join(tmp, "legacy.d.ts"), "")
	mustWrite(t, filepath.Join(tmp, "admin", "README.md"), "")
	// Vitest unit test files are NOT endpoints — must be skipped.
	mustWrite(t, filepath.Join(tmp, "admin", "foo.test.ts"), "")
	mustWrite(t, filepath.Join(tmp, "notes", "create.test.ts"), "")

	got, err := collectTSEndpoints(tmp)
	if err != nil {
		t.Fatalf("collectTSEndpoints: %v", err)
	}
	want := []string{
		"/api/admin/bar/baz",
		"/api/admin/foo",
		"/api/endpoint",
		"/api/endpoints",
		"/api/meta",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collectTSEndpoints mismatch\n got %v\nwant %v", got, want)
	}
}

func TestCollectTSEndpointsMissingDir(t *testing.T) {
	if _, err := collectTSEndpoints(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error for missing dir, got nil")
	}
}

func TestCompare(t *testing.T) {
	ts := []string{
		"/api/meta",
		"/api/notes/create",
		"/api/users/show",
	}
	mk := &DumpedRoutes{
		MisskeyVersion: "2026.5.4",
		MkGoVersion:    "0.9.1",
		Routes: []DumpedRoute{
			{Method: "POST", Path: "/api/meta"},
			{Method: "GET", Path: "/api/meta"}, // GET variant on POST = both
			{Method: "POST", Path: "/api/notes/create"},
			// users/show is missing from mk-go
			{Method: "POST", Path: "/api/test/reset-db"}, // mk-go only (other)
			{Method: "GET", Path: "/api/url"},            // mk-go only (other, no POST)
			{Method: "POST", Path: "/api/chat/messages"}, // mk-go only (chat_ext)
			{Method: "POST", Path: "/api/signup"},        // both (TS router-direct)
			{Method: "POST", Path: "/api/signin-flow"},   // both (TS router-direct)
			{Method: "GET", Path: "/healthz"},            // excluded (not /api)
			{Method: "GET", Path: "/.well-known/nodeinfo"},
			// Echo の catch-all noise — フィルタされて matrix に現れないこと。
			{Method: "POST", Path: "/api/*"},
			{Method: "GET", Path: "/api/*"},
			{Method: "echo_route_not_found", Path: "/api/*"},
		},
	}

	direct := []tsDirectRoute{
		{Method: "POST", Path: "/api/signup"},
		{Method: "POST", Path: "/api/signup-pending"},
		{Method: "POST", Path: "/api/signin-flow"},
		{Method: "POST", Path: "/api/signin-with-passkey"},
		// 非 POST の直登録。mk-go 側に GET 登録が無いので TSOnly に出る。
		{Method: "GET", Path: "/api/v1/instance/peers"},
	}

	got := compare(ts, direct, mk)

	wantBoth := []BothRoute{
		{Method: "POST", Path: "/api/meta"},
		{Method: "POST", Path: "/api/notes/create"},
		{Method: "POST", Path: "/api/signin-flow"},
		{Method: "POST", Path: "/api/signup"},
	}
	if !reflect.DeepEqual(got.Both, wantBoth) {
		t.Errorf("Both mismatch\n got %v\nwant %v", got.Both, wantBoth)
	}
	wantTSOnly := []TSOnlyRoute{
		// router-direct ts paths が mk-go に未登録だった場合に TSOnly に
		// 出ることを担保する。
		{Method: "POST", Path: "/api/signin-with-passkey"},
		{Method: "POST", Path: "/api/signup-pending"},
		{Method: "POST", Path: "/api/users/show"},
		// 非 POST 直登録は method 込みで未実装判定される。
		{Method: "GET", Path: "/api/v1/instance/peers"},
	}
	if !reflect.DeepEqual(got.TSOnly, wantTSOnly) {
		t.Errorf("TSOnly mismatch\n got %v\nwant %v", got.TSOnly, wantTSOnly)
	}
	wantMkOnly := []MkOnlyRoute{
		{Method: "POST", Path: "/api/chat/messages", Category: CatChatExt},
		{Method: "GET", Path: "/api/meta", Category: CatGETVariant},
		{Method: "POST", Path: "/api/test/reset-db", Category: CatOther},
		{Method: "GET", Path: "/api/url", Category: CatOther},
	}
	if !reflect.DeepEqual(got.MkOnly, wantMkOnly) {
		t.Errorf("MkOnly mismatch\n got %v\nwant %v", got.MkOnly, wantMkOnly)
	}
}

func TestRenderMarkdownHeader(t *testing.T) {
	r := Report{
		Both:   []BothRoute{{Method: "POST", Path: "/api/meta"}},
		TSOnly: []TSOnlyRoute{{Method: "POST", Path: "/api/foo"}},
		MkOnly: []MkOnlyRoute{
			{Method: "POST", Path: "/api/test/reset-db", Category: CatOther},
			{Method: "GET", Path: "/api/charts/users", Category: CatGETVariant},
			{Method: "POST", Path: "/api/chat/messages", Category: CatChatExt},
		},
	}
	md := renderMarkdown(r, "2026.5.4", "0.9.1")

	for _, want := range []string{
		"# API Compatibility Matrix",
		"Misskey (TS) version: `2026.5.4`",
		"mk-go version: `0.9.1`",
		"TS endpoints (POST `/api/*`): **2**",
		"mk-go implemented (TS の subset): **1**",
		"mk-go coverage of TS: **50.0%**",
		"/api/foo",
		"/api/meta",
		// category sections
		"### GET variant 追加 (1)",
		"### cherrypick 系 chat 拡張 (1)",
		"### その他 mk-go 独自 / alias (1)",
		"/api/test/reset-db",
		"/api/charts/users",
		"/api/chat/messages",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("renderMarkdown output missing %q\nfull:\n%s", want, md)
		}
	}
}

func TestRenderMarkdownEmpty(t *testing.T) {
	md := renderMarkdown(Report{}, "x", "y")
	if !strings.Contains(md, "mk-go coverage of TS: **n/a**") {
		t.Errorf("expected n/a coverage marker, got:\n%s", md)
	}
}

func TestDedupSorted(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{"a"}, []string{"a"}},
		{[]string{"a", "a"}, []string{"a"}},
		{[]string{"a", "a", "b"}, []string{"a", "b"}},
		{[]string{"a", "b", "b", "c", "c", "c"}, []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		got := dedupSorted(tt.in)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("dedupSorted(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestStripQueryString(t *testing.T) {
	tests := map[string]string{
		"/api/foo?a=1":     "/api/foo",
		"/api/foo":         "/api/foo",
		"/api/foo?a=1&b=2": "/api/foo",
		"":                 "",
		"?":                "",
	}
	for in, want := range tests {
		if got := stripQueryString(in); got != want {
			t.Errorf("stripQueryString(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// collectTSDirectRoutes は ApiServerService.ts の fastify 直登録 route を
// method 込みで抽出する。1 行形式と複数行 generic 形式の両方、および
// catch-all `/*` の除外を固定する。
func TestCollectTSDirectRoutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ApiServerService.ts")
	src := `
		// endpoint ごとの登録。path が literal でないので拾わない。型引数から
		// 後ろの /signup まで伸びて一致してもいけない。
		fastify.all<{
			Params: { endpoint: string; },
		}>('/' + endpoint.name, { bodyLimit: 1024 * 1024 }, async (request, reply) => {
			return reply;
		});

		fastify.post<{
			Body: { username: string; 'hcaptcha-response'?: string; };
		}>('/signup', (request, reply) => this.signupApiService.signup(request, reply));

		fastify.post<{ Body: { code: string; } }>('/signup-pending', (request, reply) => x);

		fastify.get('/v1/instance/peers', async (request, reply) => {
			return [];
		});

		fastify.post<{ Params: { session: string; } }>('/miauth/:session/check', async (request, reply) => {
			return {};
		});

		fastify.all('/clear-browser-cache', (request, reply) => {
			reply.code(204);
		});

		fastify.get('/*', (request, reply) => {
			reply.code(404);
		});
	`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := collectTSDirectRoutes(path)
	if err != nil {
		t.Fatalf("collectTSDirectRoutes: %v", err)
	}
	want := []tsDirectRoute{
		{Method: "GET", Path: "/api/clear-browser-cache"},
		{Method: "POST", Path: "/api/clear-browser-cache"},
		{Method: "POST", Path: "/api/miauth/:session/check"},
		{Method: "POST", Path: "/api/signup"},
		{Method: "POST", Path: "/api/signup-pending"},
		{Method: "GET", Path: "/api/v1/instance/peers"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("direct routes mismatch\n got %v\nwant %v", got, want)
	}
}

// 書式が変わって 1 件も取れなくなったら、静かに空集合を返さずエラーにする
// (直登録 endpoint が丸ごと "mk-go only" に誤分類されるのを防ぐ)。
func TestCollectTSDirectRoutes_EmptyIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ApiServerService.ts")
	if err := os.WriteFile(path, []byte("// no routes here\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := collectTSDirectRoutes(path); err == nil {
		t.Fatal("expected error when no direct routes are found")
	}
}

func TestCollectTSDirectRoutes_MissingFile(t *testing.T) {
	if _, err := collectTSDirectRoutes(filepath.Join(t.TempDir(), "nope.ts")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TS が直登録している非 POST endpoint を mk-go が実装している場合、
// both に **その method で** 入り、mk-only にも TSOnly にも現れないこと。
//
// 旧実装は mk-only の走査で `pr.method == "POST" && ts[pr.path]` しか
// skip しておらず、`GET /api/v1/instance/peers` を実装しても
// 「mk-go 独自 endpoint」として二重計上していた。加えて both は path だけを
// 持ち renderer が method を "POST" 固定で出力していたため、matrix 上は
// `POST /api/v1/instance/peers` という実在しない行になっていた (#2245)。
func TestCompareTSDirectNonPOSTImplemented(t *testing.T) {
	ts := []string{"/api/meta"}
	mk := &DumpedRoutes{
		MisskeyVersion: "2026.7.0",
		MkGoVersion:    "0.9.2",
		Routes: []DumpedRoute{
			{Method: "POST", Path: "/api/meta"},
			{Method: "GET", Path: "/api/v1/instance/peers"},
		},
	}
	direct := []tsDirectRoute{
		{Method: "GET", Path: "/api/v1/instance/peers"},
	}

	got := compare(ts, direct, mk)

	wantBoth := []BothRoute{
		{Method: "POST", Path: "/api/meta"},
		{Method: "GET", Path: "/api/v1/instance/peers"},
	}
	if !reflect.DeepEqual(got.Both, wantBoth) {
		t.Errorf("Both mismatch\n got %v\nwant %v", got.Both, wantBoth)
	}
	if len(got.TSOnly) != 0 {
		t.Errorf("TSOnly should be empty, got %v", got.TSOnly)
	}
	if len(got.MkOnly) != 0 {
		t.Errorf("MkOnly should be empty, got %v", got.MkOnly)
	}

	md := renderMarkdown(got, "2026.7.0", "0.9.2")
	if !strings.Contains(md, "| GET | `/api/v1/instance/peers` |") {
		t.Error("both table should render the GET method for the direct route")
	}
	if strings.Contains(md, "| POST | `/api/v1/instance/peers` |") {
		t.Error("both table must not render the direct GET route as POST")
	}
}

// Any で登録した route (`/api/clear-browser-cache`) は全メソッドの route を持つ。
// 表に入るのは GET と POST だけで、本家の `fastify.all` と両方に存在する行に
// なり、HEAD や PUT が「mk-go only」に並ばないこと (#3926)。
func TestCompareAnyRouteCountsOnlyGETAndPOST(t *testing.T) {
	var routes []DumpedRoute
	for _, m := range []string{"CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PROPFIND", "PUT", "REPORT", "TRACE"} {
		routes = append(routes, DumpedRoute{Method: m, Path: "/api/clear-browser-cache"})
	}
	mk := &DumpedRoutes{MisskeyVersion: "2026.10.1", MkGoVersion: "2.1.0", Routes: routes}
	direct := []tsDirectRoute{
		{Method: "GET", Path: "/api/clear-browser-cache"},
		{Method: "POST", Path: "/api/clear-browser-cache"},
	}

	got := compare(nil, direct, mk)

	wantBoth := []BothRoute{
		{Method: "GET", Path: "/api/clear-browser-cache"},
		{Method: "POST", Path: "/api/clear-browser-cache"},
	}
	if !reflect.DeepEqual(got.Both, wantBoth) {
		t.Errorf("Both mismatch\n got %v\nwant %v", got.Both, wantBoth)
	}
	if len(got.TSOnly) != 0 {
		t.Errorf("TSOnly should be empty, got %v", got.TSOnly)
	}
	if len(got.MkOnly) != 0 {
		t.Errorf("MkOnly should be empty, got %v", got.MkOnly)
	}
}
