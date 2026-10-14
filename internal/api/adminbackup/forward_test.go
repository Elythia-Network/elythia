package adminbackup

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

type seenRequest struct {
	path, auth, ip, origin, contentType string
	body                                map[string]any
}

func fakeService(t *testing.T, status int, respBody string, header map[string]string) (*httptest.Server, *[]seenRequest) {
	t.Helper()
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		seen = append(seen, seenRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), ip: r.Header.Get(clientIPHeader),
			origin: r.Header.Get("Origin"), contentType: r.Header.Get("Content-Type"), body: m})
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func forward(f *Forwarder, op, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, RestorePathPrefix+op, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.Serve(rec, req, op, "192.0.2.5")
	return rec
}

func TestForwarder_Relays(t *testing.T) {
	srv, seen := fakeService(t, http.StatusOK, `{"opId":"op1"}`, map[string]string{"Retry-After": "7"})
	f := NewForwarder(srv.URL+"/", "svc-token", nil)
	rec := forward(f, "prepare", `{"id":"g","password":"pw","token":"123456"}`,
		map[string]string{"Authorization": "Bearer usertoken0000001", "Origin": "https://elythia.example"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"opId":"op1"}`, rec.Body.String())
	assert.Equal(t, "7", rec.Header().Get("Retry-After"))
	require.Len(t, *seen, 1)
	s := (*seen)[0]
	assert.Equal(t, "/restore/prepare", s.path)
	assert.Equal(t, "Bearer svc-token", s.auth, "the service token, not the user's")
	assert.Equal(t, "192.0.2.5", s.ip)
	assert.Equal(t, "https://elythia.example", s.origin)
	assert.Equal(t, "usertoken0000001", s.body["i"], "the user's token moves into the body")
	assert.Equal(t, "123456", s.body["token"])

	// 本体の認証と同じく、ヘッダーの token を本文の i より先に使う。本文の i を使うと、
	// 本体が確かめた管理者と別の人で daemon が照合する。
	forward(f, "confirm", `{"i":"bodytoken0000001","opId":"op1"}`, map[string]string{"Authorization": "Bearer headertoken00001"})
	assert.Equal(t, "headertoken00001", (*seen)[1].body["i"])
	// ヘッダーが無ければ本文の i のまま。
	forward(f, "confirm", `{"i":"bodytoken0000001","opId":"op1"}`, nil)
	assert.Equal(t, "bodytoken0000001", (*seen)[2].body["i"])
}

// TestForwarder_CandidatesSendsOnlyTheSession checks that the listing of
// the kept databases, re-authenticated by the server, does not carry the
// password and the code to the backup service.
func TestForwarder_CandidatesSendsOnlyTheSession(t *testing.T) {
	srv, seen := fakeService(t, http.StatusOK, `{"database":"app","databases":[]}`, nil)
	f := NewForwarder(srv.URL, "svc", nil)
	rec := forward(f, "candidates", `{"password":"pw","token":"123456","credential":{"id":"x"}}`,
		map[string]string{"Authorization": "Bearer usertoken0000001"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"database":"app","databases":[]}`, rec.Body.String())
	require.Len(t, *seen, 1)
	assert.Equal(t, "/restore/candidates", (*seen)[0].path)
	assert.Equal(t, map[string]any{"i": "usertoken0000001"}, (*seen)[0].body)

	forward(f, "candidates", `{"password":"pw"}`, nil)
	assert.Equal(t, map[string]any{}, (*seen)[1].body, "no session, nothing else either")

	// 準備は種類と名前をそのまま渡す。
	forward(f, "prepare", `{"kind":"rollback","database":"app_before_restore_20261001000000","password":"pw","token":"123456"}`, nil)
	assert.Equal(t, "rollback", (*seen)[2].body["kind"])
	assert.Equal(t, "app_before_restore_20261001000000", (*seen)[2].body["database"])
	assert.Equal(t, "pw", (*seen)[2].body["password"])
}

// TestRestoreCandidates_LogsTheListing checks that a listing of the kept
// databases the backup service answered is recorded like listBackups, and
// that the response passes through as it is.
func TestRestoreCandidates_LogsTheListing(t *testing.T) {
	call := func(t *testing.T, status int, body string) (*httptest.ResponseRecorder, *modlog) {
		t.Helper()
		srv, _ := fakeService(t, status, body, map[string]string{"Content-Type": "application/json; charset=utf-8", "Retry-After": "3"})
		lg := &modlog{}
		h := NewHandler(nil, nil, lg)
		h.SetForwarder(NewForwarder(srv.URL, "svc", nil))
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, RestorePathPrefix+"candidates", strings.NewReader(`{"password":"pw"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		tok := "usertoken0000001"
		c.Set(string(middleware.UserContextKey), &model.User{ID: "admin1", Token: &tok})
		require.NoError(t, h.RestoreCandidates(c))
		return rec, lg
	}
	body := `{"database":"app","databases":[{"name":"app_before_restore_20261001000000"},{"name":"app_rolled_back_20261002000000"}]}`
	rec, lg := call(t, http.StatusOK, body)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, body, rec.Body.String())
	assert.Equal(t, "3", rec.Header().Get("Retry-After"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, []moderationlog.LogType{moderationlog.LogListKeptDatabases}, lg.entries)
	assert.Equal(t, map[string]any{"databaseCount": 2}, lg.infos[0])

	// 断られた一覧は記録しない。応答はそのまま返す。
	rec, lg = call(t, http.StatusConflict, `{"error":{"code":"BACKUP_SERVICE_BUSY"}}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "BACKUP_SERVICE_BUSY")
	assert.Empty(t, lg.entries)
	// 本文が読めない 200 も記録しない。
	_, lg = call(t, http.StatusOK, `not json`)
	assert.Empty(t, lg.entries)
}

func TestForwarder_RelaysEmptyAndErrorResponses(t *testing.T) {
	srv, _ := fakeService(t, http.StatusNoContent, "", nil)
	rec := forward(NewForwarder(srv.URL, "svc", nil), "confirm", `{}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())

	srv, _ = fakeService(t, http.StatusForbidden, `{"error":{"code":"REAUTHENTICATION_FAILED"}}`, nil)
	rec = forward(NewForwarder(srv.URL, "svc", nil), "confirm", `{}`, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "REAUTHENTICATION_FAILED")

	// daemon が serviceToken を断ったのは設定の食い違い。401 のままだと画面がサインアウトする。
	srv, _ = fakeService(t, http.StatusUnauthorized, `{"error":"unauthorized"}`, map[string]string{"WWW-Authenticate": ServiceAuthChallenge})
	rec = forward(NewForwarder(srv.URL, "wrong", nil), "confirm", `{}`, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "BACKUP_SERVICE_UNREACHABLE")
	// 利用者の token が無いという daemon の 401 は、設定の食い違いではないのでそのまま返す。
	srv, _ = fakeService(t, http.StatusUnauthorized, `{"error":{"code":"CREDENTIAL_REQUIRED"}}`, nil)
	rec = forward(NewForwarder(srv.URL, "svc", nil), "prepare", `{}`, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "CREDENTIAL_REQUIRED")

	srv, _ = fakeService(t, http.StatusOK, strings.Repeat("x", forwardResponseLimit+1), nil)
	rec = forward(NewForwarder(srv.URL, "svc", nil), "confirm", `{}`, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// 本体で認証しない status は、読めない理由 (未設定・daemon に届かない・daemon が
// Redis を読めない・serviceToken の食い違い) を分けずに同じ 503 で返すこと。
// 分けると、匿名で「バックアップのサービスが止まっている時間帯」を観測できた。
// 画面はどれも読み直すだけなので、困らない。
func TestForwarder_StatusFailuresLookTheSame(t *testing.T) {
	want := func(t *testing.T, rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, what)
		assert.JSONEq(t, `{"error":{"code":"RESTORE_UNAVAILABLE","id":"`+IDRestoreUnavailable+`","kind":"server","message":"The restore status cannot be read now. Please try again later."}}`, rec.Body.String(), what)
	}
	want(t, forward(NewForwarder("", "", nil), "status", `{}`, nil), "not configured")
	want(t, forward(NewForwarder("http://127.0.0.1:1", "svc", nil), "status", `{}`, nil), "unreachable")
	srv, _ := fakeService(t, http.StatusServiceUnavailable, `{"error":{"code":"RESTORE_UNAVAILABLE","message":"cannot read Redis"}}`, nil)
	want(t, forward(NewForwarder(srv.URL, "svc", nil), "status", `{}`, nil), "the service cannot read Redis")
	srv, _ = fakeService(t, http.StatusUnauthorized, `{"error":"unauthorized"}`, map[string]string{"WWW-Authenticate": ServiceAuthChallenge})
	want(t, forward(NewForwarder(srv.URL, "wrong", nil), "status", `{}`, nil), "service token refused")

	// 届いた daemon の判断 (操作が無い、ticket が合わない) は、そのまま返す
	// (画面はこれで読み直しをやめる)。
	srv, _ = fakeService(t, http.StatusNotFound, `{"error":{"code":"NO_SUCH_RESTORE"}}`, nil)
	rec := forward(NewForwarder(srv.URL, "svc", nil), "status", `{}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_RESTORE")
	// 認証する操作は、従来どおり理由を返す。
	rec = forward(NewForwarder("", "", nil), "prepare", `{}`, nil)
	assert.Contains(t, rec.Body.String(), "BACKUP_SERVICE_NOT_CONFIGURED")
}

func TestForwarder_Refusals(t *testing.T) {
	srv, seen := fakeService(t, http.StatusOK, `{}`, nil)
	f := NewForwarder(srv.URL, "svc", nil)
	rec := forward(NewForwarder("", "", nil), "prepare", `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "BACKUP_SERVICE_NOT_CONFIGURED")
	rec = forward(NewForwarder(srv.URL, "", nil), "prepare", `{}`, nil)
	assert.Contains(t, rec.Body.String(), "BACKUP_SERVICE_NOT_CONFIGURED", "no token, nothing is sent")

	rec = forward(f, "drop", `{}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	req := httptest.NewRequest(http.MethodGet, RestorePathPrefix+"status", nil)
	rec = httptest.NewRecorder()
	f.Serve(rec, req, "status", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = forward(f, "status", `{"broken`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = forward(f, "status", strings.Repeat(" ", reauthBodyLimit+1), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	req = httptest.NewRequest(http.MethodPost, RestorePathPrefix+"status", bytes.NewReader([]byte(`{"a":1}`)))
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	f.Serve(rec, req, "status", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, *seen, "nothing invalid reaches the service")

	unreachable := NewForwarder("http://127.0.0.1:1", "svc", nil)
	rec = forward(unreachable, "confirm", `{}`, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	rec = forward(NewForwarder("http://[::1", "svc", nil), "confirm", `{}`, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}
