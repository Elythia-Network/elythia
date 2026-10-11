package adminbackup

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// 本文に i があれば、それを使う。
	forward(f, "status", `{"i":"bodytoken0000001","opId":"op1"}`, map[string]string{"Authorization": "Bearer other"})
	assert.Equal(t, "bodytoken0000001", (*seen)[1].body["i"])
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
	srv, _ = fakeService(t, http.StatusUnauthorized, `{"error":"unauthorized"}`, nil)
	rec = forward(NewForwarder(srv.URL, "wrong", nil), "status", `{}`, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "BACKUP_SERVICE_UNREACHABLE")

	srv, _ = fakeService(t, http.StatusOK, strings.Repeat("x", forwardResponseLimit+1), nil)
	rec = forward(NewForwarder(srv.URL, "svc", nil), "status", `{}`, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestForwarder_Refusals(t *testing.T) {
	srv, seen := fakeService(t, http.StatusOK, `{}`, nil)
	f := NewForwarder(srv.URL, "svc", nil)
	rec := forward(NewForwarder("", "", nil), "status", `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "BACKUP_SERVICE_NOT_CONFIGURED")
	rec = forward(NewForwarder(srv.URL, "", nil), "status", `{}`, nil)
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
	rec = forward(unreachable, "status", `{}`, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	rec = forward(NewForwarder("http://[::1", "svc", nil), "status", `{}`, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}
