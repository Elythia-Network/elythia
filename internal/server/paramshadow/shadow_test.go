package paramshadow

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/paramschema"
)

type fakeSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *fakeSink) Record(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

const testParamDef = `{"type":"object","properties":{
	"noteId":{"type":"string","format":"misskey:id"},
	"limit":{"type":"integer","minimum":1,"maximum":100,"default":10},
	"flag":{"type":"boolean"}},"required":["noteId"]}`

func testEndpoints(t *testing.T) map[string]paramschema.Endpoint {
	t.Helper()
	s := mustCompile(t, testParamDef)
	return map[string]paramschema.Endpoint{
		"t/post":   {Schema: s},
		"t/get":    {Schema: s, AllowGet: true},
		"t/file":   {Schema: s, RequireFile: true},
		"t/broken": {CompileErr: errors.New("broken")},
	}
}

// serve routes one request through the middleware to h and returns the
// recorded events and the response.
func serve(t *testing.T, method, target, contentType, body string, h echo.HandlerFunc) ([]Event, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	sink := &fakeSink{}
	mw := Middleware(testEndpoints(t), sink)
	for _, p := range []string{"/api/t/post", "/api/t/get", "/api/t/file", "/api/t/broken", "/api/t/other", "/other/t/post"} {
		e.Match([]string{http.MethodGet, http.MethodPost, http.MethodPut}, p, h, mw)
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return sink.events, rec
}

func ok(c echo.Context) error { return c.JSON(http.StatusOK, map[string]any{"ok": true}) }

func invalidParam(param string) echo.HandlerFunc {
	return func(c echo.Context) error { return apierr.JSONInvalidParamClient(c, param, "x") }
}

func one(t *testing.T, events []Event) Event {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one", events)
	}
	return events[0]
}

func TestMiddlewareClassifies(t *testing.T) {
	const jsonCT = echo.MIMEApplicationJSON
	tests := []struct {
		name, method, target, body string
		handler                    echo.HandlerFunc
		want                       Event
	}{
		{"both accept", http.MethodPost, "/api/t/post", `{"noteId":"abc"}`, ok,
			Event{Kind: KindAgreed, HandlerStatus: 200}},
		{"validator rejects, handler ok", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`, ok,
			Event{Kind: KindRejectedHandlerOK, SchemaPath: "#/properties/noteId/format", HandlerStatus: 200}},
		{"validator accepts, handler invalid param", http.MethodPost, "/api/t/post", `{"noteId":"abc"}`, invalidParam("#/properties/limit/maximum"),
			Event{Kind: KindAcceptedHandlerInvalidParam, HandlerStatus: 400, HandlerCode: "INVALID_PARAM", HandlerParam: "#/properties/limit/maximum"}},
		{"both reject with the same param", http.MethodPost, "/api/t/post", `{"noteId":"abc","limit":0}`, invalidParam("#/properties/limit/minimum"),
			Event{Kind: KindAgreed, SchemaPath: "#/properties/limit/minimum", HandlerStatus: 400, HandlerCode: "INVALID_PARAM", HandlerParam: "#/properties/limit/minimum"}},
		{"both reject with different params", http.MethodPost, "/api/t/post", `{"noteId":"abc","limit":0}`, invalidParam("#/properties/limit/maximum"),
			Event{Kind: KindParamMismatch, SchemaPath: "#/properties/limit/minimum", HandlerStatus: 400, HandlerCode: "INVALID_PARAM", HandlerParam: "#/properties/limit/maximum"}},
		{"both reject, handler without info", http.MethodPost, "/api/t/post", `{}`, func(c echo.Context) error { return apierr.JSONInvalidParam(c) },
			Event{Kind: KindParamMismatch, SchemaPath: "#/required", HandlerStatus: 400, HandlerCode: "INVALID_PARAM"}},
		{"validator rejects, handler other error", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`, apierr.JSONNoSuchNote,
			Event{Kind: KindRejectedHandlerOtherError, SchemaPath: "#/properties/noteId/format", HandlerStatus: 400, HandlerCode: "NO_SUCH_NOTE"}},
		{"validator rejects, handler 404", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`, func(c echo.Context) error { return apierr.JSONNotFound(c) },
			Event{Kind: KindRejectedHandlerOtherError, SchemaPath: "#/properties/noteId/format", HandlerStatus: 404}},
		{"handler returns an HTTPError", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`,
			func(echo.Context) error { return echo.NewHTTPError(http.StatusBadRequest, "INVALID_PARAM") },
			Event{Kind: KindRejectedHandlerOtherError, SchemaPath: "#/properties/noteId/format", HandlerStatus: 400}},
		{"handler returns a wrapped HTTPError", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`,
			func(echo.Context) error { return fmt.Errorf("wrapped: %w", echo.NewHTTPError(http.StatusBadRequest)) },
			Event{Kind: KindRejectedHandlerOtherError, SchemaPath: "#/properties/noteId/format", HandlerStatus: 500}},
		{"handler returns an HTTPError with an HTTPError inside", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`,
			func(echo.Context) error {
				return echo.NewHTTPError(http.StatusBadRequest).SetInternal(echo.NewHTTPError(http.StatusNotFound))
			},
			Event{Kind: KindRejectedHandlerOtherError, SchemaPath: "#/properties/noteId/format", HandlerStatus: 404}},
		{"handler returns an HTTPError with a plain error inside", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`,
			func(echo.Context) error {
				return echo.NewHTTPError(http.StatusForbidden).SetInternal(errors.New("inner"))
			},
			Event{Kind: KindRejectedHandlerOtherError, SchemaPath: "#/properties/noteId/format", HandlerStatus: 403}},
		{"handler returns an error", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`,
			func(echo.Context) error { return errors.New("boom") },
			Event{Kind: KindRejectedHandlerOtherError, SchemaPath: "#/properties/noteId/format", HandlerStatus: 500}},
		{"handler writes nothing", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`,
			func(echo.Context) error { return nil },
			Event{Kind: KindRejectedHandlerOK, SchemaPath: "#/properties/noteId/format", HandlerStatus: 200}},
		{"handler 204", http.MethodPost, "/api/t/post", `{"noteId":"a-b"}`,
			func(c echo.Context) error { return c.NoContent(http.StatusNoContent) },
			Event{Kind: KindRejectedHandlerOK, SchemaPath: "#/properties/noteId/format", HandlerStatus: 204}},
		{"handler 400 without a JSON body", http.MethodPost, "/api/t/post", `{"noteId":"abc"}`,
			func(c echo.Context) error { return c.String(http.StatusBadRequest, "no") },
			Event{Kind: KindAgreed, HandlerStatus: 400}},
		{"handler 400 without a code", http.MethodPost, "/api/t/post", `{"noteId":"abc"}`,
			func(c echo.Context) error {
				return c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]any{}})
			},
			Event{Kind: KindAgreed, HandlerStatus: 400}},
		{"GET after the cast", http.MethodGet, "/api/t/get?noteId=abc&limit=200", "", ok,
			Event{Kind: KindRejectedHandlerOK, SchemaPath: "#/properties/limit/maximum", HandlerStatus: 200}},
		{"GET cast failure answered alike", http.MethodGet, "/api/t/get?noteId=abc&flag=yes", "", invalidParam("flag"),
			Event{Kind: KindAgreed, SchemaPath: "flag", HandlerStatus: 400, HandlerCode: "INVALID_PARAM", HandlerParam: "flag"}},
		{"GET cast failure, handler ok", http.MethodGet, "/api/t/get?noteId=abc&flag=yes", "", ok,
			Event{Kind: KindRejectedHandlerOK, SchemaPath: "flag", HandlerStatus: 200}},
		{"GET without allowGet", http.MethodGet, "/api/t/post?noteId=abc", "", ok,
			Event{Kind: KindSkipped, Skip: SkipMethod}},
		{"PUT", http.MethodPut, "/api/t/post", `{"noteId":"abc"}`, ok,
			Event{Kind: KindSkipped, Skip: SkipMethod}},
		{"multipart endpoint", http.MethodPost, "/api/t/file", `{"noteId":"abc"}`, ok,
			Event{Kind: KindSkipped, Skip: SkipMultipart}},
		{"compile error", http.MethodPost, "/api/t/broken", `{"noteId":"abc"}`, ok,
			Event{Kind: KindCompileError}},
		{"lone surrogate", http.MethodPost, "/api/t/post", string(esc(`{"noteId":"~ud800"}`)), ok,
			Event{Kind: KindSkipped, Skip: SkipLoneSurrogate}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := ""
			if tt.body != "" {
				ct = jsonCT
			}
			got := one(t, func() []Event { ev, _ := serve(t, tt.method, tt.target, ct, tt.body, tt.handler); return ev }())
			want := tt.want
			want.Endpoint = strings.TrimPrefix(strings.SplitN(tt.target, "?", 2)[0], "/api/")
			if got != want {
				t.Errorf("event = %+v\nwant    %+v", got, want)
			}
		})
	}
}

func TestMiddlewareSkips(t *testing.T) {
	for _, tt := range []struct{ name, ct, body, want string }{
		{"not JSON", "text/plain", `{"noteId":"abc"}`, SkipNotJSON},
		{"no content type", "", `{"noteId":"abc"}`, SkipNotJSON},
		{"empty JSON body", echo.MIMEApplicationJSON, "", SkipBody},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ev := one(t, func() []Event { e, _ := serve(t, http.MethodPost, "/api/t/post", tt.ct, tt.body, ok); return e }())
			if ev.Kind != KindSkipped || ev.Skip != tt.want {
				t.Errorf("event = %+v, want skip %s", ev, tt.want)
			}
		})
	}
	for _, target := range []string{"/api/t/other", "/other/t/post"} {
		if ev, _ := serve(t, http.MethodPost, target, echo.MIMEApplicationJSON, `{}`, ok); len(ev) != 0 {
			t.Errorf("%s recorded %+v", target, ev)
		}
	}
}

func TestMiddlewareReadErrorReachesTheHandler(t *testing.T) {
	boom := errors.New("boom")
	e := echo.New()
	sink := &fakeSink{}
	var handlerErr error
	e.POST("/api/t/post", func(c echo.Context) error {
		_, handlerErr = io.ReadAll(c.Request().Body)
		return ok(c)
	}, Middleware(testEndpoints(t), sink))
	req := httptest.NewRequest(http.MethodPost, "/api/t/post", nil)
	req.Body = io.NopCloser(io.MultiReader(strings.NewReader(`{"a"`), errReader{boom}))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	e.ServeHTTP(httptest.NewRecorder(), req)
	if !errors.Is(handlerErr, boom) {
		t.Errorf("handler read err = %v", handlerErr)
	}
	if ev := one(t, sink.events); ev.Skip != SkipBody {
		t.Errorf("event = %+v", ev)
	}
}

// TestMiddlewareChangesNothing compares, request by request, what the handler
// read and what the client got with and without the middleware.
func TestMiddlewareChangesNothing(t *testing.T) {
	handler := func(c echo.Context) error {
		b, _ := io.ReadAll(c.Request().Body)
		c.Response().Header().Set("X-Seen", fmt.Sprintf("%q|%s", b, c.QueryParams().Encode()))
		if strings.Contains(string(b), "bad") {
			return apierr.JSONInvalidParamClient(c, "#/properties/limit/type", "must be integer")
		}
		if strings.Contains(string(b), "big") {
			return c.String(http.StatusBadRequest, strings.Repeat("x", 3*maxCapturedBody))
		}
		return c.JSON(http.StatusOK, map[string]any{"body": string(b)})
	}
	for _, r := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/t/post", `{"noteId":"abc","limit":10.0}`},
		{http.MethodPost, "/api/t/post", `{"noteId":"abc","limit":"bad"}`},
		{http.MethodPost, "/api/t/post", `{"noteId":"abc","limit":"big"}`},
		{http.MethodPost, "/api/t/post", `{"noteId":"a-b","limit":1e400,"x":null}`},
		{http.MethodPost, "/api/t/post", string(esc(`{"noteId":"~ud800"}`))},
		{http.MethodGet, "/api/t/get?noteId=abc&limit=1&limit=2", ""},
	} {
		run := func(withShadow bool) *httptest.ResponseRecorder {
			e := echo.New()
			var mws []echo.MiddlewareFunc
			if withShadow {
				mws = append(mws, Middleware(testEndpoints(t), &fakeSink{}))
			}
			e.Match([]string{http.MethodGet, http.MethodPost}, "/api/t/post", handler, mws...)
			e.Match([]string{http.MethodGet, http.MethodPost}, "/api/t/get", handler, mws...)
			req := httptest.NewRequest(r.method, r.target, strings.NewReader(r.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			return rec
		}
		with, without := run(true), run(false)
		if with.Code != without.Code || with.Body.String() != without.Body.String() ||
			fmt.Sprint(with.Header()) != fmt.Sprint(without.Header()) {
			t.Errorf("%s %s %s: with %d %v %q, without %d %v %q", r.method, r.target, r.body,
				with.Code, with.Header(), with.Body, without.Code, without.Header(), without.Body)
		}
	}
}

func TestMiddlewareCountsAPanic(t *testing.T) {
	orig := validate
	t.Cleanup(func() { validate = orig })
	validate = func(*paramschema.Schema, any) *paramschema.Error { panic("validator bug") }
	var called bool
	ev, rec := serve(t, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"abc"}`,
		func(c echo.Context) error { called = true; return ok(c) })
	if e := one(t, ev); e.Kind != KindPanic || !called || rec.Code != http.StatusOK {
		t.Errorf("event = %+v, handler called %v, status %d", e, called, rec.Code)
	}
}

func TestMiddlewareRestoresTheWriterWhenTheHandlerPanics(t *testing.T) {
	e := echo.New()
	var inner http.ResponseWriter
	shadow := Middleware(testEndpoints(t), &fakeSink{})
	recoverMW := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			outer := c.Response().Writer
			defer func() {
				if recover() != nil {
					inner = c.Response().Writer
					if inner != outer {
						t.Error("the writer was not restored")
					}
					err = c.NoContent(http.StatusTeapot)
				}
			}()
			return next(c)
		}
	}
	e.POST("/api/t/post", func(echo.Context) error { panic("handler bug") }, recoverMW, shadow)
	req := httptest.NewRequest(http.MethodPost, "/api/t/post", strings.NewReader(`{"noteId":"abc"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if inner == nil || rec.Code != http.StatusTeapot {
		t.Errorf("status %d", rec.Code)
	}
}

func TestHandlerStringsAreSanitized(t *testing.T) {
	const secret = "secret value 9zz"
	for _, tt := range []struct {
		body      string
		wantParam string
		wantCode  string
	}{
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"#/properties/` + secret + `"}}}`, otherParam, "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":42}}}`, otherParam, "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"a@remote.example"}}}`, otherParam, "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"a.b"}}}`, otherParam, "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"a:b"}}}`, otherParam, "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"a-b"}}}`, otherParam, "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"#/properties/a.b/type"}}}`, otherParam, "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"#/properties/a/type"}}}`, "#/properties/a/type", "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"limit"}}}`, "limit", "INVALID_PARAM"},
		{`{"error":{"code":"INVALID_PARAM","info":{"param":"#"}}}`, "#", "INVALID_PARAM"},
		{`{"error":{"code":"` + secret + `"}}`, "", otherParam},
	} {
		ev, _ := serve(t, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"abc"}`,
			func(c echo.Context) error { return c.JSONBlob(http.StatusBadRequest, []byte(tt.body)) })
		got := one(t, ev)
		if got.HandlerParam != tt.wantParam || got.HandlerCode != tt.wantCode {
			t.Errorf("%s: param %q code %q", tt.body, got.HandlerParam, got.HandlerCode)
		}
		if strings.Contains(fmt.Sprintf("%+v", got), "secret") {
			t.Errorf("event carries the value: %+v", got)
		}
	}
}

func TestEventsCarryNoRequestValue(t *testing.T) {
	const marker = "zzmarker"
	body := `{"noteId":"` + marker + `-x","i":"` + marker + `token","` + marker + `key":"` + marker + `"}`
	e := echo.New()
	sink := &fakeSink{}
	e.POST("/api/t/post", func(c echo.Context) error {
		return apierr.JSONInvalidParamClient(c, "#/properties/"+marker+" v", marker)
	}, Middleware(testEndpoints(t), sink))
	req := httptest.NewRequest(http.MethodPost, "/api/t/post?q="+marker, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("Authorization", "Bearer "+marker)
	req.RemoteAddr = "192.0.2.77:1234"
	e.ServeHTTP(httptest.NewRecorder(), req)
	got := fmt.Sprintf("%+v", one(t, sink.events))
	if strings.Contains(got, marker) || strings.Contains(got, "192.0.2.77") {
		t.Errorf("event carries a request value: %s", got)
	}
}

type flushHijackWriter struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (w *flushHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, nil
}

func TestCaptureWriter(t *testing.T) {
	under := &flushHijackWriter{ResponseRecorder: httptest.NewRecorder()}
	w := &captureWriter{ResponseWriter: under}
	w.WriteHeader(http.StatusBadRequest)
	w.WriteHeader(http.StatusOK) // 2度目は記録しない(net/httpも無視する)
	if _, err := w.Write([]byte(strings.Repeat("a", maxCapturedBody+10))); err != nil {
		t.Fatal(err)
	}
	if w.status != http.StatusBadRequest || len(w.body) != maxCapturedBody || under.Body.Len() != maxCapturedBody+10 {
		t.Errorf("status %d, captured %d, written %d", w.status, len(w.body), under.Body.Len())
	}
	w.Flush()
	if !under.Flushed {
		t.Error("Flush did not reach the writer")
	}
	if _, _, err := w.Hijack(); err != nil || !under.hijacked {
		t.Errorf("Hijack: %v", err)
	}
	if w.Unwrap() != under {
		t.Error("Unwrap")
	}

	plain := &captureWriter{ResponseWriter: struct{ http.ResponseWriter }{httptest.NewRecorder()}}
	plain.Flush()
	if _, _, err := plain.Hijack(); err == nil {
		t.Error("Hijack on a writer without it succeeded")
	}
	if _, _ = plain.Write([]byte("x")); plain.status != http.StatusOK || plain.body != nil {
		t.Errorf("status %d body %q", plain.status, plain.body)
	}
}

// serveWith routes one request through a middleware built with opts.
func serveWith(t *testing.T, opts []Option, method, target, body string, h echo.HandlerFunc) []Event {
	t.Helper()
	e := echo.New()
	sink := &fakeSink{}
	e.Match([]string{http.MethodGet, http.MethodPost}, "/api/t/get", h, Middleware(testEndpoints(t), sink, opts...))
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	e.ServeHTTP(httptest.NewRecorder(), req)
	return sink.events
}

// TestMiddlewareBodyLimit pins the boundary of the size limit: a body or
// query of exactly the limit is validated, one byte more is counted as
// too_large, and the handler reads the whole body either way.
func TestMiddlewareBodyLimit(t *testing.T) {
	base := `{"noteId":"abc","pad":""}`
	for _, tt := range []struct {
		pad  int
		skip string
	}{{0, ""}, {1, SkipTooLarge}} {
		const limit = 100
		body := `{"noteId":"abc","pad":"` + strings.Repeat("x", limit-len(base)+tt.pad) + `"}`
		var read string
		ev := one(t, serveWith(t, []Option{WithMaxBody(limit)}, http.MethodPost, "/api/t/get", body, func(c echo.Context) error {
			b, _ := io.ReadAll(c.Request().Body)
			read = string(b)
			return ok(c)
		}))
		if ev.Skip != tt.skip || (tt.skip == "" && ev.Kind != KindAgreed) {
			t.Errorf("%d bytes: event %+v", len(body), ev)
		}
		if read != body {
			t.Errorf("%d bytes: the handler read %d bytes", len(body), len(read))
		}
	}
	for _, tt := range []struct {
		q    string
		skip string
	}{{"noteId=abcdefghij", ""}, {"noteId=abcdefghijk", SkipTooLarge}} {
		ev := one(t, serveWith(t, []Option{WithMaxBody(len("noteId=abcdefghij"))}, http.MethodGet, "/api/t/get?"+tt.q, "", ok))
		if ev.Skip != tt.skip {
			t.Errorf("query %s: event %+v", tt.q, ev)
		}
	}
	// 既定の上限は1MiBのAPIの上限より十分小さい。
	big := `{"noteId":"abc","x":"` + strings.Repeat("x", DefaultMaxBody) + `"}`
	if ev := one(t, serveWith(t, nil, http.MethodPost, "/api/t/get", big, ok)); ev.Skip != SkipTooLarge {
		t.Errorf("default limit: event %+v", ev)
	}
}

// TestMiddlewareConcurrencyLimit holds the only slot with a validation that
// blocks, and checks that a second request is counted as busy without
// waiting, and that the slot is free again afterwards.
func TestMiddlewareConcurrencyLimit(t *testing.T) {
	orig := validate
	t.Cleanup(func() { validate = orig })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	validate = func(s *paramschema.Schema, data any) *paramschema.Error {
		block := false
		once.Do(func() { block = true })
		if block {
			close(entered)
			<-release
		}
		return orig(s, data)
	}

	e := echo.New()
	sink := &fakeSink{}
	e.POST("/api/t/post", ok, Middleware(testEndpoints(t), sink, WithMaxConcurrent(1)))
	send := func() {
		req := httptest.NewRequest(http.MethodPost, "/api/t/post", strings.NewReader(`{"noteId":"abc"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		e.ServeHTTP(httptest.NewRecorder(), req)
	}
	done := make(chan struct{})
	go func() { send(); close(done) }()
	<-entered
	send() // 枠が埋まっているので、待たずに busy として数える
	close(release)
	<-done
	send() // 枠が空いたので検証する

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var kinds []string
	for _, ev := range sink.events {
		kinds = append(kinds, ev.Kind+"/"+ev.Skip)
	}
	want := []string{KindSkipped + "/" + SkipBusy, KindAgreed + "/", KindAgreed + "/"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("events = %v, want %v", kinds, want)
	}
}

func TestOptions(t *testing.T) {
	s := &shadow{}
	WithMaxConcurrent(0)(s)
	if cap(s.slots) != 1 {
		t.Errorf("WithMaxConcurrent(0) gives %d slots", cap(s.slots))
	}
	if DefaultMaxConcurrent() < 1 {
		t.Errorf("DefaultMaxConcurrent() = %d", DefaultMaxConcurrent())
	}
}

func TestMiddlewareValueLimit(t *testing.T) {
	// {"noteId":"abc","x":[...]} のトークンは 7 + 要素数。
	body := func(n int) string {
		return `{"noteId":"abc","x":[` + strings.TrimSuffix(strings.Repeat("0,", n), ",") + `]}`
	}
	for _, tt := range []struct {
		n    int
		skip string
	}{{3, ""}, {4, SkipTooManyValues}} {
		ev := one(t, serveWith(t, []Option{WithMaxValues(10)}, http.MethodPost, "/api/t/get", body(tt.n), ok))
		if ev.Skip != tt.skip {
			t.Errorf("%d elements: event %+v", tt.n, ev)
		}
	}
	for _, tt := range []struct{ q, skip string }{{"noteId=abc&x=1", ""}, {"noteId=abc&x=1&y=2", SkipTooManyValues}} {
		ev := one(t, serveWith(t, []Option{WithMaxValues(2)}, http.MethodGet, "/api/t/get?"+tt.q, "", ok))
		if ev.Skip != tt.skip {
			t.Errorf("query %s: event %+v", tt.q, ev)
		}
	}
}

// blockingReader blocks its first Read until release is closed.
type blockingReader struct {
	r        io.Reader
	entered  chan struct{}
	release  chan struct{}
	blockOne sync.Once
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.blockOne.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.r.Read(p)
}

// TestMiddlewareReadsTheBodyInsideTheSlot holds the only slot while the body
// is being read, and checks that a second request is counted as busy: the
// read and the decode, the costly part, run inside the slot.
func TestMiddlewareReadsTheBodyInsideTheSlot(t *testing.T) {
	e := echo.New()
	sink := &fakeSink{}
	e.POST("/api/t/post", ok, Middleware(testEndpoints(t), sink, WithMaxConcurrent(1)))
	br := &blockingReader{r: strings.NewReader(`{"noteId":"abc"}`), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/t/post", nil)
		req.Body = io.NopCloser(br)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		e.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-br.entered
	req := httptest.NewRequest(http.MethodPost, "/api/t/post", strings.NewReader(`{"noteId":"abc"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	e.ServeHTTP(httptest.NewRecorder(), req)
	close(br.release)
	<-done
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 2 || sink.events[0].Skip != SkipBusy || sink.events[1].Kind != KindAgreed {
		t.Errorf("events = %+v", sink.events)
	}
}

// TestMiddlewareReleasesTheSlotOnEveryPath sends, with one slot, a request
// that ends on each early return inside the slot, then a normal one, which
// must not be busy.
func TestMiddlewareReleasesTheSlotOnEveryPath(t *testing.T) {
	for _, tt := range []struct{ name, body, skip string }{
		{"too large", `{"noteId":"abc","pad":"` + strings.Repeat("x", 100) + `"}`, SkipTooLarge},
		{"too many values", `{"noteId":"abc","x":[0,0,0,0,0,0,0,0,0,0]}`, SkipTooManyValues},
		{"empty body", "", SkipBody},
		{"lone surrogate", string(esc(`{"noteId":"~ud800"}`)), SkipLoneSurrogate},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			sink := &fakeSink{}
			e.POST("/api/t/post", ok, Middleware(testEndpoints(t), sink, WithMaxConcurrent(1), WithMaxBody(64), WithMaxValues(12)))
			for _, body := range []string{tt.body, `{"noteId":"abc"}`} {
				req := httptest.NewRequest(http.MethodPost, "/api/t/post", strings.NewReader(body))
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				e.ServeHTTP(httptest.NewRecorder(), req)
			}
			if len(sink.events) != 2 || sink.events[0].Skip != tt.skip || sink.events[1].Kind != KindAgreed {
				t.Errorf("events = %+v", sink.events)
			}
		})
	}
}

// TestEchoErrorStatusFollowsEcho pins the rules of echo's
// DefaultHTTPErrorHandler that echoErrorStatus copies: Internal is looked at
// one level deep, by a direct type assertion.
func TestEchoErrorStatusFollowsEcho(t *testing.T) {
	three := echo.NewHTTPError(http.StatusBadRequest).SetInternal(
		echo.NewHTTPError(http.StatusNotFound).SetInternal(echo.NewHTTPError(http.StatusConflict)))
	wrappedInside := echo.NewHTTPError(http.StatusBadRequest).SetInternal(
		fmt.Errorf("wrapped: %w", echo.NewHTTPError(http.StatusNotFound)))
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{"three levels", three, http.StatusNotFound},
		{"wrapped HTTPError inside", wrappedInside, http.StatusBadRequest},
	} {
		// echo自身の答えと比べる。
		e := echo.New()
		rec := httptest.NewRecorder()
		e.DefaultHTTPErrorHandler(tt.err, e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec))
		if rec.Code != tt.want {
			t.Fatalf("%s: echo answers %d, the test expects %d", tt.name, rec.Code, tt.want)
		}
		if got := echoErrorStatus(tt.err); got != tt.want {
			t.Errorf("%s: echoErrorStatus = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestDefaultMaxConcurrentIsAQuarterOfGOMAXPROCS(t *testing.T) {
	prev := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	for procs, want := range map[int]int{1: 1, 3: 1, 4: 1, 7: 1, 8: 2, 16: 4} {
		runtime.GOMAXPROCS(procs)
		if got := DefaultMaxConcurrent(); got != want {
			t.Errorf("GOMAXPROCS=%d: DefaultMaxConcurrent() = %d, want %d", procs, got, want)
		}
	}
}
