package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
)

const testToken = "service-token"

func call(t *testing.T, h http.Handler, method, path, token, body string) (int, map[string]any, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Header().Get("Content-Type") == "application/json" {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec.Code, out, rec.Header()
}

func TestHandlerRequiresToken(t *testing.T) {
	_, err := Handler(New(Options{}), "")
	require.ErrorIs(t, err, ErrNoServiceToken)
}

func TestHandlerRejectsWrongToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil, nil)
		r.start(t)
		h, err := Handler(r.d, testToken)
		require.NoError(t, err)
		for _, tc := range []struct{ method, path, token string }{
			{"GET", "/status", ""},
			{"GET", "/status", "wrong"},
			{"GET", "/status", testToken + "x"},
			{"POST", "/take", "service-toke"},
			{"POST", "/verify", "wrong"},
			{"GET", "/nope", ""},
		} {
			code, body, hdr := call(t, h, tc.method, tc.path, tc.token, `{"id":"20000101T000000Z"}`)
			assert.Equal(t, http.StatusUnauthorized, code, "%+v", tc)
			assert.Equal(t, "unauthorized", body["error"])
			assert.Contains(t, hdr.Get("WWW-Authenticate"), "Bearer")
		}
		// "Bearer " の付かない生の token も通さない。
		req := httptest.NewRequest("POST", "/take", nil)
		req.Header.Set("Authorization", testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		synctest.Wait()
		assert.Empty(t, r.taker.callTimes(), "no rejected request started a job")
		r.stop()
	})
}

func TestHandlerTakeAndStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil, func(r *rig) { r.taker.block = make(chan struct{}) })
		r.start(t)
		h, err := Handler(r.d, testToken)
		require.NoError(t, err)

		code, body, hdr := call(t, h, "POST", "/take", testToken, "")
		require.Equal(t, http.StatusAccepted, code)
		assert.Equal(t, "application/json", hdr.Get("Content-Type"))
		assert.Equal(t, "no-store", hdr.Get("Cache-Control"))
		assert.Equal(t, map[string]any{"kind": "take", "trigger": "api", "startedAt": "2000-01-01T00:00:00Z"}, body["job"])
		synctest.Wait()

		code, body, _ = call(t, h, "POST", "/take", testToken, "")
		assert.Equal(t, http.StatusConflict, code)
		assert.Equal(t, "busy", body["error"])
		assert.Equal(t, "take", body["running"].(map[string]any)["kind"])

		code, body, _ = call(t, h, "GET", "/status", testToken, "")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "take", body["running"].(map[string]any)["kind"])
		assert.Nil(t, body["schedule"])
		assert.Nil(t, body["nextRunAt"])
		assert.Nil(t, body["lastTake"])

		close(r.taker.block)
		synctest.Wait()
		code, body, _ = call(t, h, "GET", "/status", testToken, "")
		require.Equal(t, http.StatusOK, code)
		assert.Nil(t, body["running"])
		last := body["lastTake"].(map[string]any)
		assert.Equal(t, true, last["ok"])
		assert.Equal(t, "20000101T000000Z", last["generationId"])
		assert.Equal(t, "20000101T000000Z", body["latestUsable"].(map[string]any)["id"])

		code, _, _ = call(t, h, "GET", "/take", testToken, "")
		assert.Equal(t, http.StatusMethodNotAllowed, code)
		r.stop()

		code, body, _ = call(t, h, "POST", "/take", testToken, "")
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.Equal(t, "not_running", body["error"])
	})
}

// 本文は 4KiB までしか読まない。正しい ID でも、上限を超える JSON は 400 にする。
func TestHandlerVerifyBodyLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var id string
		r := newRig(t, nil, func(r *rig) { id = r.st.addGeneration(epoch.Add(-time.Hour), true, "") })
		r.start(t)
		h, err := Handler(r.d, testToken)
		require.NoError(t, err)
		padded := func(size int) string {
			head, tail := `{"pad":"`, `","id":"`+id+`"}`
			return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
		}
		const limit = 4 << 10
		code, out, _ := call(t, h, "POST", "/verify", testToken, padded(limit+1))
		assert.Equal(t, http.StatusBadRequest, code)
		assert.Equal(t, "invalid_id", out["error"])
		assert.Empty(t, r.ver.verified())
		// 上限ちょうどまでは読む。
		code, _, _ = call(t, h, "POST", "/verify", testToken, padded(limit))
		assert.Equal(t, http.StatusAccepted, code)
		synctest.Wait()
		r.stop()
	})
}

func TestHandlerVerify(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var id string
		r := newRig(t, nil, func(r *rig) { id = r.st.addGeneration(epoch.Add(-time.Hour), true, "") })
		r.start(t)
		h, err := Handler(r.d, testToken)
		require.NoError(t, err)

		for _, body := range []string{"", "{", `{"id":""}`, `{"id":"../x"}`, `{"id":"latest"}`, `{"id":"` + strings.Repeat("1", 5000) + `"}`} {
			code, out, _ := call(t, h, "POST", "/verify", testToken, body)
			assert.Equal(t, http.StatusBadRequest, code, body)
			assert.Equal(t, "invalid_id", out["error"])
		}
		code, out, _ := call(t, h, "POST", "/verify", testToken, `{"id":"20000101T000000Z"}`)
		assert.Equal(t, http.StatusNotFound, code, "no meta.json for that id")
		assert.Equal(t, "no_such_generation", out["error"])

		code, out, _ = call(t, h, "POST", "/verify", testToken, `{"id":"`+id+`"}`)
		require.Equal(t, http.StatusAccepted, code)
		assert.Equal(t, id, out["job"].(map[string]any)["generationId"])
		synctest.Wait()
		assert.Equal(t, []string{id}, r.ver.verified())
		_, st, _ := call(t, h, "GET", "/status", testToken, "")
		assert.Equal(t, true, st["lastVerify"].(map[string]any)["ok"])
		assert.Equal(t, true, st["lastVerify"].(map[string]any)["verify"].(map[string]any)["ok"])

		r.st.mu.Lock()
		r.st.statErr = errBoom
		r.st.mu.Unlock()
		code, out, _ = call(t, h, "POST", "/verify", testToken, `{"id":"`+id+`"}`)
		assert.Equal(t, http.StatusBadGateway, code)
		assert.Equal(t, "storage_error", out["error"])
		r.stop()
	})
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestHandlerClientIPHeaderTrustedOnlyWithToken(t *testing.T) {
	var logs syncBuffer
	d := New(Options{Logger: slog.New(slog.NewTextHandler(&logs, nil)), Storage: newMemStorage()})
	h, err := Handler(d, testToken)
	require.NoError(t, err)
	send := func(token, ip string) {
		req := httptest.NewRequest("GET", "/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set(ClientIPHeader, ip)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	send("wrong", "198.51.100.7")
	assert.NotContains(t, logs.String(), "198.51.100.7", "the header of an unauthenticated request is ignored")
	send(testToken, "203.0.113.9")
	assert.Contains(t, logs.String(), "client_ip=203.0.113.9")
	send(testToken, "evil\nclient_ip=1.2.3.4")
	assert.NotContains(t, logs.String(), "1.2.3.4", "a value that is not an IP is dropped")
	send(testToken, "2001:db8::1")
	assert.Contains(t, logs.String(), "client_ip=2001:db8::1")
}

func TestServe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	go func() { done <- Serve(ctx, ln, h, discardLogger()) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusTeapot, resp.StatusCode)
	cancel()
	require.NoError(t, <-done)

	// listener が閉じていれば、その誤りを返す。
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ln2.Close()
	require.Error(t, Serve(context.Background(), ln2, h, discardLogger()))
}

func TestStatusJSONShape(t *testing.T) {
	// #3462 の本体が読む形。名前を変えるときは本体側も直す。
	now := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	st := Status{Now: now, Schedule: &ScheduleInfo{Interval: "24h0m0s", At: "04:00", Timezone: "UTC", Keep: 7, Verify: true, DelayAfter: "36h0m0s"},
		Running:  &Job{Kind: JobTake, Trigger: TriggerSchedule, StartedAt: now},
		LastTake: &JobResult{Job: Job{Kind: JobTake, Trigger: TriggerAPI, GenerationID: "20261009T190000Z", StartedAt: now}, FinishedAt: now, OK: false, Stage: "verify", Error: "verification failed", Verify: &backup.VerifyResult{ID: "20261009T190000Z"}, Deleted: []string{"20261001T190000Z"}},
	}
	b, err := json.Marshal(st)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"now":"2026-10-10T04:00:00Z",
		"schedule":{"interval":"24h0m0s","at":"04:00","timezone":"UTC","keep":7,"verify":true,"delayAfter":"36h0m0s"},
		"running":{"kind":"take","trigger":"schedule","startedAt":"2026-10-10T04:00:00Z"},
		"pending":false,
		"nextRunAt":null,
		"lastTake":{"kind":"take","trigger":"api","generationId":"20261009T190000Z","startedAt":"2026-10-10T04:00:00Z","finishedAt":"2026-10-10T04:00:00Z","ok":false,"stage":"verify","error":"verification failed",
			"verify":{"id":"20261009T190000Z","verifiedAt":"0001-01-01T00:00:00Z","ok":false,"stages":null,"elythiaVersion":""},"deleted":["20261001T190000Z"]},
		"lastVerify":null,
		"latestUsable":null,
		"overdue":false
	}`, string(b))
}
