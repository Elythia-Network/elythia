package maintmode

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
)

// HeaderMarker marks a response as coming from the maintenance handler. The
// Service Worker passes a 503 carrying it through instead of replacing it
// with the offline page.
const HeaderMarker = "X-Elythia-Maintenance"

// StatusPath answers the maintenance status as JSON. The normal router
// answers it too (with maintenance: false), so the maintenance page can tell
// when to reload.
const StatusPath = "/maintenance/status"

// DefaultRetryAfter is the Retry-After the handler sends when none is set.
const DefaultRetryAfter = 60 * time.Second

// StaticRoute serves files from disk while in maintenance. With a trailing
// slash Prefix matches a directory tree looked up in Dirs in order; without
// one it matches exactly and serves the file Dirs[0].
type StaticRoute struct {
	Prefix string
	Dirs   []string
}

// HandlerOptions configures NewHandler.
type HandlerOptions struct {
	// State returns the latest maintenance the process knows about. It may
	// return nil (e.g. while the process is reopening the database after the
	// maintenance was cleared); the handler then reports the resuming phase.
	State func() *State
	// RetryAfter is sent on every 503. Zero means DefaultRetryAfter.
	RetryAfter time.Duration
	// Static lists the files served from disk as usual.
	Static []StaticRoute
	// Role is added to the /healthz body when non-empty (queue-only nodes).
	Role string
	// QueueOnly limits the handler to what a queue-only node serves normally:
	// /healthz answers, everything else is a plain 503 (no page, no static
	// files, no API error body).
	QueueOnly bool
}

// Status is the body of StatusPath.
//
// 誰でも読めるので、種類 (リストアかどうか) と操作した人は出さない
// (設計 §7-5 の決定)。
type Status struct {
	Maintenance bool       `json:"maintenance"`
	Phase       string     `json:"phase,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

// NewHandler returns the handler a process serves while it is in
// maintenance. It never touches the database.
//
// 経路ごとの応答は設計 §2.2 のとおり。API は本家のクライアントが読める JSON の
// エラーにする (HTML だと res.json() が SyntaxError になる)。SPA の shell は
// 返さない (meta を読めない shell が空の meta をクライアントに上書きさせるため)。
func NewHandler(opts HandlerOptions) http.Handler {
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = DefaultRetryAfter
	}
	if opts.State == nil {
		opts.State = func() *State { return nil }
	}
	return &handler{opts: opts, retryAfter: strconv.Itoa(int(opts.RetryAfter / time.Second))}
}

type handler struct {
	opts       HandlerOptions
	retryAfter string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if h.opts.QueueOnly {
		// 配送ノードは通常時も /healthz (と /metrics) しか出さない。メンテナンス中に
		// 画面や静的なファイルを配り始めないよう揃える (#3463 のレビュー L4)。
		if p == "/healthz" {
			h.healthz(w)
			return
		}
		h.unavailable(w, true)
		return
	}
	switch {
	case p == "/healthz":
		h.healthz(w)
		return
	case p == StatusPath:
		h.status(w)
		return
	case p == "/api" || strings.HasPrefix(p, "/api/"):
		h.api(w, r)
		return
	case r.Method == http.MethodPost && (p == "/inbox" || isUserInbox(p)):
		// 連合の受信。本家の送り手は 5xx を間隔を空けて再送する (設計 §1.9)。
		h.unavailable(w, false)
		return
	case p == "/streaming":
		// 本家の WebSocket でない GET と同じ 503。upgrade はしない。
		h.unavailable(w, false)
		return
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && h.serveStatic(w, r) {
		return
	}
	if wantsHTML(r) {
		h.page(w, r)
		return
	}
	h.unavailable(w, true)
}

func isUserInbox(p string) bool {
	rest, ok := strings.CutPrefix(p, "/users/")
	if !ok {
		return false
	}
	id, ok := strings.CutSuffix(rest, "/inbox")
	return ok && id != "" && !strings.Contains(id, "/")
}

// wantsHTML reports whether r is a page navigation.
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Sec-Fetch-Dest") == "document" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (h *handler) setUnavailableHeaders(w http.ResponseWriter) {
	w.Header().Set("Retry-After", h.retryAfter)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(HeaderMarker, "1")
}

// unavailable writes a 503, with a short text body when body is true.
func (h *handler) unavailable(w http.ResponseWriter, body bool) {
	h.setUnavailableHeaders(w)
	if !body {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("The server is under maintenance.\n"))
}

func (h *handler) api(w http.ResponseWriter, r *http.Request) {
	// 通常時のグローバル CORS (AllowOrigins: *) と揃える。別 origin の
	// クライアントが、エラーの本文を読めるようにするため。
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.setUnavailableHeaders(w)
	writeJSON(w, http.StatusServiceUnavailable, apierr.Maintenance())
}

func (h *handler) healthz(w http.ResponseWriter) {
	// 200 のまま返す。503 にすると compose の depends_on: service_healthy と
	// 監視がメンテナンスを障害として扱う (設計 §2.2)。
	body := map[string]any{"status": "ok", "maintenance": true}
	if h.opts.Role != "" {
		body["role"] = h.opts.Role
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

func (h *handler) status(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, StatusOf(h.opts.State(), true))
}

// StatusOf builds the public status body for st. holding is whether the
// answering process is in maintenance.
func StatusOf(st *State, holding bool) Status {
	if !holding {
		return Status{Maintenance: false}
	}
	out := Status{Maintenance: true, Phase: PhaseResuming}
	if st == nil {
		return out
	}
	out.Phase = st.Phase
	out.Reason = st.Reason
	if !st.StartedAt.IsZero() {
		t := st.StartedAt.UTC()
		out.StartedAt = &t
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// serveStatic serves r from the configured static routes and reports
// whether it did.
func (h *handler) serveStatic(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	for _, route := range h.opts.Static {
		if len(route.Dirs) == 0 {
			continue
		}
		if !strings.HasSuffix(route.Prefix, "/") {
			if p == route.Prefix {
				return serveFile(w, r, route.Dirs[0])
			}
			continue
		}
		rest, ok := strings.CutPrefix(p, route.Prefix)
		if !ok {
			continue
		}
		// path.Clean("/" + rest) は根より上へ出る ".." を落とす。
		clean := strings.TrimPrefix(path.Clean("/"+rest), "/")
		if clean == "" {
			continue
		}
		for _, dir := range route.Dirs {
			if serveFileIn(w, r, dir, filepath.FromSlash(clean)) {
				return true
			}
		}
	}
	return false
}

// serveFileIn serves name inside dir and reports whether it existed.
//
// os.Root で開くので、dir の中から外を指す symlink もたどらない。".." を
// 落とすだけでは、symlink を経由して dir の外のファイルを返しうる。
func serveFileIn(w http.ResponseWriter, r *http.Request, dir, name string) bool {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	return serveOpened(w, r, f)
}

// serveFile serves a regular file named by the configuration (not by the
// request) and reports whether it existed.
func serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := os.Open(name)
	if err != nil {
		return false
	}
	return serveOpened(w, r, f)
}

// serveOpened serves f when it is a regular file, and closes it.
// ディレクトリの一覧は出さない (http.FileServer と違い、ディレクトリは無視する)。
func serveOpened(w http.ResponseWriter, r *http.Request, f *os.File) bool {
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
	return true
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	nonce := newNonce()
	h.setUnavailableHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(strings.ReplaceAll(pageHTML, "{{nonce}}", nonce)))
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}
