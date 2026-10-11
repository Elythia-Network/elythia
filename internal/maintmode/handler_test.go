package maintmode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/pgactivity"
)

func serve(h http.Handler, method, path string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertUnavailable(t *testing.T, rec *httptest.ResponseRecorder, retryAfter string) {
	t.Helper()
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, retryAfter, rec.Header().Get("Retry-After"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "1", rec.Header().Get(HeaderMarker))
}

// TestHandler_Responses pins the answer of every route family while in
// maintenance (design §2.2).
func TestHandler_Responses(t *testing.T) {
	started := time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)
	st := &State{Phase: PhaseHeld, Kind: KindRestore, Reason: "<b>DB</b>", StartedAt: started, RequestedBy: "admin"}
	h := NewHandler(HandlerOptions{State: func() *State { return st }, RetryAfter: 2 * time.Minute})

	t.Run("api answers JSON the upstream client can read", func(t *testing.T) {
		for _, p := range []string{"/api/meta", "/api/notes/create", "/api"} {
			rec := serve(h, http.MethodPost, p, nil)
			assertUnavailable(t, rec, "120")
			assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
			assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), p)
			assert.Equal(t, apierr.Maintenance(), map[string]any{"error": body["error"]}, p)
		}
		pre := serve(h, http.MethodOptions, "/api/meta", nil)
		assert.Equal(t, http.StatusNoContent, pre.Code)
		assert.Contains(t, pre.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	})
	t.Run("inbox is 503 without a body", func(t *testing.T) {
		for _, p := range []string{"/inbox", "/users/9abc/inbox"} {
			rec := serve(h, http.MethodPost, p, nil)
			assertUnavailable(t, rec, "120")
			assert.Empty(t, rec.Body.String(), p)
		}
	})
	t.Run("streaming is 503 and is not upgraded", func(t *testing.T) {
		rec := serve(h, http.MethodGet, "/streaming", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"})
		assertUnavailable(t, rec, "120")
		assert.Empty(t, rec.Body.String())
	})
	t.Run("healthz stays 200", func(t *testing.T) {
		rec := serve(h, http.MethodGet, "/healthz", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"status":"ok","maintenance":true}`, rec.Body.String())
	})
	t.Run("status is public and hides the kind and the operator", func(t *testing.T) {
		rec := serve(h, http.MethodGet, StatusPath, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"maintenance":true,"phase":"held","startedAt":"2026-10-10T03:00:00Z","reason":"<b>DB</b>"}`,
			rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "restore")
		assert.NotContains(t, rec.Body.String(), "admin")
	})
	t.Run("navigation gets the maintenance page, never the SPA shell", func(t *testing.T) {
		for _, hdr := range []map[string]string{
			{"Accept": "text/html,application/xhtml+xml"},
			{"Sec-Fetch-Dest": "document"},
		} {
			rec := serve(h, http.MethodGet, "/@alice", hdr)
			assertUnavailable(t, rec, "120")
			assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			body := rec.Body.String()
			assert.Contains(t, body, "メンテナンス中")
			assert.Contains(t, body, "Under maintenance")
			assert.Contains(t, body, StatusPath)
			// 理由はサーバー側で埋め込まない (textContent で入れる)。
			assert.NotContains(t, body, "<b>DB</b>")
			// SPA の shell (meta を埋めた HTML) ではない。
			assert.NotContains(t, body, "data-generated-at")
			csp := rec.Header().Get("Content-Security-Policy")
			require.Contains(t, csp, "script-src 'nonce-")
			nonce := csp[strings.Index(csp, "'nonce-")+len("'nonce-"):]
			nonce = nonce[:strings.Index(nonce, "'")]
			assert.Contains(t, body, `<script nonce="`+nonce+`">`)
			assert.Contains(t, body, `<style nonce="`+nonce+`">`)
		}
		head := serve(h, http.MethodHead, "/", map[string]string{"Accept": "text/html"})
		assertUnavailable(t, head, "120")
		assert.Empty(t, head.Body.String())
	})
	t.Run("everything else is a plain 503", func(t *testing.T) {
		for _, req := range [][2]string{
			{http.MethodGet, "/manifest.json"},
			{http.MethodGet, "/.well-known/nodeinfo"},
			{http.MethodGet, "/users/9abc"},
			{http.MethodPost, "/users/9abc/outbox"},
			{http.MethodPost, "/users/a/b/inbox"},
			{http.MethodGet, "/nodeinfo/2.1"},
			{http.MethodPost, "/@alice"},
		} {
			rec := serve(h, req[0], req[1], map[string]string{"Accept": "application/activity+json"})
			assertUnavailable(t, rec, "120")
			assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"), req[1])
		}
	})
}

// TestHandler_QueueOnlyServesHealthzOnly checks that a queue-only node keeps
// to what it serves normally (#3463 review L4).
func TestHandler_QueueOnlyServesHealthzOnly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.js"), []byte("x"), 0o644))
	h := NewHandler(HandlerOptions{Role: "queue", QueueOnly: true, Static: []StaticRoute{{Prefix: "/vite/", Dirs: []string{dir}}}})
	rec := serve(h, http.MethodGet, "/healthz", nil)
	assert.JSONEq(t, `{"status":"ok","maintenance":true,"role":"queue"}`, rec.Body.String())
	for _, req := range []struct {
		method, path string
		hdr          map[string]string
	}{
		{http.MethodGet, "/vite/a.js", nil},
		{http.MethodGet, "/", map[string]string{"Accept": "text/html"}},
		{http.MethodPost, "/api/meta", nil},
		{http.MethodGet, StatusPath, nil},
	} {
		rec := serve(h, req.method, req.path, req.hdr)
		assertUnavailable(t, rec, "60")
		assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"), req.path)
	}
}

func TestHandler_DefaultsAndQueueRole(t *testing.T) {
	h := NewHandler(HandlerOptions{Role: "queue"})
	rec := serve(h, http.MethodPost, "/inbox", nil)
	assertUnavailable(t, rec, "60")
	rec = serve(h, http.MethodGet, "/healthz", nil)
	assert.JSONEq(t, `{"status":"ok","maintenance":true,"role":"queue"}`, rec.Body.String())
	// 状態が読めていない (抜けた直後など) ときは再開中と答える。
	rec = serve(h, http.MethodGet, StatusPath, nil)
	assert.JSONEq(t, `{"maintenance":true,"phase":"resuming"}`, rec.Body.String())
}

func TestStatusOf(t *testing.T) {
	assert.Equal(t, Status{Maintenance: false}, StatusOf(&State{Phase: PhaseHeld}, false))
	assert.Equal(t, Status{Maintenance: true, Phase: PhaseEntering}, StatusOf(&State{Phase: PhaseEntering}, true))

	// 誰でも読める応答に、判定役が書いた接続の一覧 (pid・ユーザー・接続元) を載せない。
	st := &State{
		Phase: PhaseEntering, OpID: "secret-op", RequestedBy: "u1",
		Blockers:   []Blocker{{Server: "primary", Session: pgactivity.Session{PID: 1, User: "dbuser", Addr: "10.0.0.5"}}},
		Terminated: []Blocker{{Server: "primary"}}, WaitingFor: []string{"host-1(running)"}, JudgeError: "x",
	}
	b, err := json.Marshal(StatusOf(st, true))
	require.NoError(t, err)
	for _, leak := range []string{"secret-op", "u1", "dbuser", "10.0.0.5", "host-1", "blockers"} {
		assert.NotContains(t, string(b), leak)
	}
}

// TestHandler_StaticFiles checks that built assets keep being served while
// in maintenance, with directory fallback, no directory listing and no way
// out of the configured directories.
func TestHandler_StaticFiles(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	repo := filepath.Join(root, "repo")
	for _, d := range []string{dist, repo, filepath.Join(dist, "sub")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dist, "a.js"), []byte("console.log(1)"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "b.png"), []byte("png"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.txt"), []byte("secret"), 0o644))
	fav := filepath.Join(root, "favicon.ico")
	require.NoError(t, os.WriteFile(fav, []byte("ico"), 0o644))

	// dir の中から外を指す symlink は、たどらない。
	require.NoError(t, os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(dist, "escape.txt")))

	h := NewHandler(HandlerOptions{Static: []StaticRoute{
		{Prefix: "/assets/", Dirs: []string{dist, repo}},
		{Prefix: "/favicon.ico", Dirs: []string{fav}},
		{Prefix: "/empty/"},
		{Prefix: "/gone/", Dirs: []string{filepath.Join(root, "missing")}},
	}})

	rec := serve(h, http.MethodGet, "/assets/a.js", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "console.log(1)", rec.Body.String())
	rec = serve(h, http.MethodGet, "/assets/b.png", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "2 つ目のディレクトリへ落ちていない")
	assert.Equal(t, "png", rec.Body.String())
	rec = serve(h, http.MethodGet, "/favicon.ico", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	for _, p := range []string{"/assets/sub", "/assets/", "/assets/missing.js", "/assets/../secret.txt", "/assets/%2e%2e/secret.txt", "/assets/escape.txt", "/empty/x", "/gone/x"} {
		rec = serve(h, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, p)
		assert.NotContains(t, rec.Body.String(), "secret", p)
	}
	// POST は静的ファイルとして扱わない。
	rec = serve(h, http.MethodPost, "/assets/a.js", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestHandler_ForwardsTheRestorePaths checks that the restore endpoints are
// forwarded while in maintenance (#3463 (6c)) and every other API path still
// gets the 503.
func TestHandler_ForwardsTheRestorePaths(t *testing.T) {
	var forwarded []string
	h := NewHandler(HandlerOptions{
		State: func() *State { return &State{Phase: PhaseHeld, OpID: "op"} },
		Forward: func(w http.ResponseWriter, r *http.Request) {
			forwarded = append(forwarded, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		},
		ForwardPrefix: "/api/admin/backup/restore/",
	})
	rec := serve(h, http.MethodPost, "/api/admin/backup/restore/status", nil)
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, []string{"/api/admin/backup/restore/status"}, forwarded)
	rec = serve(h, http.MethodPost, "/api/admin/backup/list", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "other admin/backup paths are not forwarded")
	assert.Len(t, forwarded, 1)

	// 転送先が無ければ、ほかの API と同じく 503。
	h = NewHandler(HandlerOptions{State: func() *State { return &State{Phase: PhaseHeld, OpID: "op"} }})
	rec = serve(h, http.MethodPost, "/api/admin/backup/restore/status", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
