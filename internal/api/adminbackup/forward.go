package adminbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// RestorePathPrefix is where the restore endpoints live. They are forwarded
// to the backup service as they are, also while the server is in
// maintenance (#3463 (6c)).
const RestorePathPrefix = "/api/admin/backup/restore/"

// RestoreOps are the forwarded operations.
var RestoreOps = []string{"prepare", "confirm", "abort", "status"}

// idServiceUnreachable is returned when the backup service cannot be reached.
const idServiceUnreachable = "1d3f5b9a-27c4-4e60-9f0e-6a8b2c4d7e19"

// forwardTimeout bounds one forwarded request. 準備の検査 (保存先の meta.json、
// DB の権限) を含むので、取る・確かめるの受け付けより長くする。
const forwardTimeout = time.Minute

// forwardResponseLimit caps the response read from the backup service.
const forwardResponseLimit = 1 << 20

// Forwarder relays the restore requests to the backup service.
//
// **本体では照合しない。** 再認証は daemon が行う (本体で照合すると、同じ TOTP の
// コードを本体と daemon で二重に消費して落ちる)。本体が見るのは、本文の上限と形、
// daemon の設定の有無だけ。利用者の Authorization ヘッダーの token は本文の i へ
// 移す (daemon への Authorization は serviceToken が使う)。
type Forwarder struct {
	base, token string
	client      *http.Client
}

// NewForwarder returns a Forwarder for the control API at baseURL. An empty
// baseURL or token answers BACKUP_SERVICE_NOT_CONFIGURED.
func NewForwarder(baseURL, token string, client *http.Client) *Forwarder {
	if client == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// 同じ compose の中の宛先。proxy を通すと Bearer の token が proxy に渡る。
		t.Proxy = nil
		client = &http.Client{Timeout: forwardTimeout, Transport: t}
	}
	return &Forwarder{base: strings.TrimRight(baseURL, "/"), token: token, client: client}
}

// Configured reports whether a backup service is set.
func (f *Forwarder) Configured() bool { return f != nil && f.base != "" && f.token != "" }

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve relays one request for op ("prepare", ...). clientIP is the
// administrator's address as the server resolved it.
func (f *Forwarder) Serve(w http.ResponseWriter, r *http.Request, op, clientIP string) {
	known := false
	for _, o := range RestoreOps {
		known = known || o == op
	}
	if !known || r.Method != http.MethodPost {
		writeJSONBody(w, http.StatusNotFound, apierr.NotFound())
		return
	}
	if !f.Configured() {
		writeJSONBody(w, http.StatusBadRequest, apierr.Error("BACKUP_SERVICE_NOT_CONFIGURED",
			"The backup service is not configured.", idServiceNotConfigured))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, reauthBodyLimit+1))
	if err != nil || len(body) > reauthBodyLimit {
		writeJSONBody(w, http.StatusBadRequest, apierr.InvalidParam())
		return
	}
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(body)) > 0 {
		if !middleware.IsJSONContentType(r.Header.Get(echo.HeaderContentType)) {
			writeJSONBody(w, http.StatusBadRequest, apierr.InvalidParam())
			return
		}
		if err := json.Unmarshal(body, &fields); err != nil {
			writeJSONBody(w, http.StatusBadRequest, apierr.InvalidParam())
			return
		}
	}
	if _, ok := fields["i"]; !ok {
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
			b, _ := json.Marshal(tok)
			fields["i"] = b
		}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		writeJSONBody(w, http.StatusBadRequest, apierr.InvalidParam())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), forwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base+"/restore/"+op, bytes.NewReader(out))
	if err != nil {
		f.unreachable(w, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	if clientIP != "" {
		req.Header.Set(clientIPHeader, clientIP)
	}
	// パスキーの検証は Origin を見る。
	if o := r.Header.Get("Origin"); o != "" {
		req.Header.Set("Origin", o)
	}
	res, err := f.client.Do(req)
	if err != nil {
		f.unreachable(w, err)
		return
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, forwardResponseLimit+1))
	if err != nil || len(data) > forwardResponseLimit {
		f.unreachable(w, err)
		return
	}
	if res.StatusCode == http.StatusUnauthorized {
		// daemon が serviceToken を断った。本体と daemon の設定の食い違いで、利用者の
		// 認証の誤りではない (401 のまま返すと、画面がサインアウトの扱いをする)。
		slog.Error("admin/backup: the backup service refused the service token; check backup.server.serviceToken")
		f.unreachable(w, nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if v := res.Header.Get("Retry-After"); v != "" {
		w.Header().Set("Retry-After", v)
	}
	if len(data) == 0 {
		w.WriteHeader(res.StatusCode)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(data)
}

func (f *Forwarder) unreachable(w http.ResponseWriter, err error) {
	if err != nil {
		slog.Warn("admin/backup: cannot reach the backup service", "err", err)
	}
	writeJSONBody(w, http.StatusBadGateway, apierr.ErrorWithKind("BACKUP_SERVICE_UNREACHABLE",
		"The backup service cannot be reached.", idServiceUnreachable, apierr.KindServer))
}

// clientIPHeader carries the administrator's address to the service.
const clientIPHeader = "X-Elythia-Client-IP"

// RestorePrepare handles POST /api/admin/backup/restore/prepare.
func (h *Handler) RestorePrepare(c echo.Context) error { return h.restore(c, "prepare") }

// RestoreConfirm handles POST /api/admin/backup/restore/confirm.
func (h *Handler) RestoreConfirm(c echo.Context) error { return h.restore(c, "confirm") }

// RestoreAbort handles POST /api/admin/backup/restore/abort.
func (h *Handler) RestoreAbort(c echo.Context) error { return h.restore(c, "abort") }

// RestoreStatus handles POST /api/admin/backup/restore/status.
func (h *Handler) RestoreStatus(c echo.Context) error { return h.restore(c, "status") }

func (h *Handler) restore(c echo.Context, op string) error {
	h.forwarder.Serve(c.Response(), c.Request(), op, c.RealIP())
	return nil
}

// SetForwarder sets the forwarder of the restore endpoints.
func (h *Handler) SetForwarder(f *Forwarder) { h.forwarder = f }
