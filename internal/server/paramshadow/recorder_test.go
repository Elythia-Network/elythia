package paramshadow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
)

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		delete(m, "time")
		delete(m, "level")
		out = append(out, m)
	}
	return out
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestRecorder(interval time.Duration) (*Recorder, *logBuffer, *fakeClock) {
	buf := &logBuffer{}
	clock := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	r := NewRecorder(slog.New(slog.NewJSONHandler(buf, nil)), interval)
	r.now = clock.now
	r.reset(clock.t)
	return r, buf, clock
}

func TestRecorderSummarizes(t *testing.T) {
	r, buf, clock := newTestRecorder(10 * time.Minute)
	mismatch := Event{Endpoint: "notes/create", Kind: KindParamMismatch, SchemaPath: "#/properties/text/minLength",
		HandlerParam: "#/properties/text/type", HandlerStatus: 400, HandlerCode: "INVALID_PARAM"}
	r.Record(mismatch)
	r.Record(mismatch)
	r.Record(Event{Endpoint: "clips/update", Kind: KindRejectedHandlerOK, SchemaPath: "#/properties/isPublic/type", HandlerStatus: 200})
	r.Record(Event{Endpoint: "x", Kind: KindAgreed, HandlerStatus: 200})
	r.Record(Event{Endpoint: "x", Kind: KindSkipped, Skip: SkipNotJSON})
	r.Record(Event{Endpoint: "x", Kind: KindSkipped, Skip: SkipNotJSON})
	r.Record(Event{Endpoint: "y", Kind: KindSkipped, Skip: SkipLoneSurrogate})
	if got := buf.lines(t); len(got) != 0 {
		t.Fatalf("wrote before the interval: %v", got)
	}

	clock.t = clock.t.Add(90 * time.Second)
	r.Flush()
	want := []map[string]any{
		{"msg": "paramshadow divergence", "endpoint": "clips/update", "kind": KindRejectedHandlerOK,
			"schemaPath": "#/properties/isPublic/type", "handlerParam": "", "handlerStatus": 200.0, "handlerCode": "", "count": 1.0},
		{"msg": "paramshadow divergence", "endpoint": "notes/create", "kind": KindParamMismatch,
			"schemaPath": "#/properties/text/minLength", "handlerParam": "#/properties/text/type", "handlerStatus": 400.0,
			"handlerCode": "INVALID_PARAM", "count": 2.0},
		{"msg": "paramshadow summary", "since": "2026-10-18T12:00:00Z", "seconds": 90.0, "agreed": 1.0, "diverged": 3.0,
			"skipped": 3.0, "skippedBy": map[string]any{SkipLoneSurrogate: 1.0, SkipNotJSON: 2.0}},
	}
	if got := buf.lines(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("lines =\n%v\nwant\n%v", got, want)
	}

	// 次の要約は、前の要約からの分だけを数える。何も無ければ書かない。
	buf.buf.Reset()
	r.Flush()
	if got := buf.lines(t); len(got) != 0 {
		t.Errorf("empty period wrote %v", got)
	}
}

func TestRecorderWritesOnceAfterTheInterval(t *testing.T) {
	r, buf, clock := newTestRecorder(time.Minute)
	r.Record(Event{Kind: KindAgreed})
	clock.t = clock.t.Add(59 * time.Second)
	r.Record(Event{Kind: KindAgreed})
	if got := buf.lines(t); len(got) != 0 {
		t.Fatalf("wrote before the interval: %v", got)
	}
	clock.t = clock.t.Add(time.Second)
	r.Record(Event{Kind: KindAgreed})
	r.Record(Event{Kind: KindAgreed})
	got := buf.lines(t)
	if len(got) != 1 || got[0]["agreed"] != 3.0 {
		t.Fatalf("lines = %v", got)
	}
	// 次の間隔は、書いた時点から数える。
	clock.t = clock.t.Add(59 * time.Second)
	r.Record(Event{Kind: KindAgreed})
	if got := buf.lines(t); len(got) != 1 {
		t.Fatalf("wrote again before the next interval: %v", got)
	}
	clock.t = clock.t.Add(time.Second)
	r.Record(Event{Kind: KindAgreed})
	if got := buf.lines(t); len(got) != 2 || got[1]["agreed"] != 3.0 {
		t.Fatalf("lines = %v", got)
	}
}

func TestRecorderDefaultInterval(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if r := NewRecorder(slog.Default(), d); r.interval != DefaultInterval {
			t.Errorf("NewRecorder(%v).interval = %v", d, r.interval)
		}
	}
}

func TestRecorderBoundsTheKeys(t *testing.T) {
	r, buf, _ := newTestRecorder(time.Hour)
	for i := range maxKeys + 5 {
		r.Record(Event{Endpoint: "e", Kind: KindParamMismatch, SchemaPath: fmt.Sprintf("#/properties/p%d/type", i)})
	}
	r.mu.Lock()
	n := len(r.divergences)
	r.mu.Unlock()
	if n != maxKeys+1 {
		t.Fatalf("%d keys", n)
	}
	r.Flush()
	lines := buf.lines(t)
	var overflow map[string]any
	for _, l := range lines {
		if l["handlerParam"] == overflowKey {
			overflow = l
		}
	}
	if overflow == nil || overflow["count"] != 5.0 || overflow["schemaPath"] != "" || overflow["handlerCode"] != overflowKey {
		t.Errorf("overflow line = %v", overflow)
	}
	if last := lines[len(lines)-1]; last["diverged"] != float64(maxKeys+5) {
		t.Errorf("summary = %v", last)
	}
}

func TestRecorderIsSafeForConcurrentUse(t *testing.T) {
	buf := &logBuffer{}
	r := NewRecorder(slog.New(slog.NewJSONHandler(buf, nil)), time.Nanosecond)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				r.Record(Event{Endpoint: "e", Kind: KindRejectedHandlerOK, SchemaPath: "#/type"})
				r.Record(Event{Kind: KindAgreed})
			}
		})
	}
	wg.Wait()
	r.Flush()
	var diverged, agreed float64
	for _, l := range buf.lines(t) {
		if l["msg"] == "paramshadow summary" {
			diverged += l["diverged"].(float64)
			agreed += l["agreed"].(float64)
		}
	}
	if diverged != 1600 || agreed != 1600 {
		t.Errorf("summed %v diverged, %v agreed", diverged, agreed)
	}
}

// TestLogsCarryNoRequestValue sends values that a log line must never hold
// (param values, unknown keys, the token in the body, the Authorization
// header, the query, the client IP, the handler's reason) through the
// middleware and the recorder, and searches the written log for them.
func TestLogsCarryNoRequestValue(t *testing.T) {
	const marker = "qq7marker"
	r, buf, _ := newTestRecorder(time.Hour)
	e := echo.New()
	handlers := []echo.HandlerFunc{
		ok,
		func(c echo.Context) error {
			return apierr.JSONInvalidParamClient(c, "#/properties/limit/minimum", "must be >= 1 "+marker)
		},
		func(c echo.Context) error {
			return apierr.JSONInvalidParamClient(c, "#/properties/"+marker+"/x y", marker)
		},
		func(c echo.Context) error {
			return c.JSON(http.StatusBadRequest, apierr.Error(marker, marker, marker))
		},
	}
	for i, h := range handlers {
		e.POST(fmt.Sprintf("/api/t/post%d", i), h)
	}
	eps := testEndpoints(t)
	for i := range handlers {
		eps[fmt.Sprintf("t/post%d", i)] = eps["t/post"]
	}
	mw := Middleware(eps, r)
	e.Use(mw)
	bodies := []string{
		`{"noteId":"` + marker + `","i":"` + marker + `tok","` + marker + `":"` + marker + `"}`,
		`{"noteId":"` + marker + `-x","limit":0,"i":"` + marker + `"}`,
		`{"limit":"` + marker + `"}`,
	}
	for i := range handlers {
		for _, body := range bodies {
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/t/post%d?i=%s", i, marker), strings.NewReader(body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			req.Header.Set("Authorization", "Bearer "+marker)
			req.Header.Set("User-Agent", marker)
			req.RemoteAddr = "198.51.100.23:4321"
			e.ServeHTTP(httptest.NewRecorder(), req)
		}
	}
	r.Flush()
	out := buf.buf.String()
	if !strings.Contains(out, "paramshadow divergence") || !strings.Contains(out, "paramshadow summary") {
		t.Fatalf("nothing was logged: %s", out)
	}
	for _, v := range []string{marker, "198.51.100.23", "Bearer", "must be"} {
		if strings.Contains(out, v) {
			t.Errorf("the log holds %q:\n%s", v, out)
		}
	}
}

// TestRecorderWritesTheSummaryOffTheRequest checks that the periodic summary
// goes to the async runner, and that Record writes it itself when the runner
// does not start it.
func TestRecorderWritesTheSummaryOffTheRequest(t *testing.T) {
	r, buf, clock := newTestRecorder(time.Minute)
	var pending []func()
	started := true
	r.SetAsync(func(f func()) bool {
		if started {
			pending = append(pending, f)
		}
		return started
	})
	r.Record(Event{Kind: KindAgreed})
	clock.t = clock.t.Add(time.Minute)
	r.Record(Event{Kind: KindAgreed})
	if len(pending) != 1 || len(buf.lines(t)) != 0 {
		t.Fatalf("pending %d, lines %v", len(pending), buf.lines(t))
	}
	pending[0]()
	if got := buf.lines(t); len(got) != 1 || got[0]["agreed"] != 2.0 {
		t.Fatalf("lines = %v", got)
	}

	started = false
	r.Record(Event{Kind: KindAgreed})
	clock.t = clock.t.Add(time.Minute)
	r.Record(Event{Kind: KindAgreed})
	if got := buf.lines(t); len(got) != 2 || got[1]["agreed"] != 2.0 || len(pending) != 1 {
		t.Fatalf("lines = %v, pending %d", got, len(pending))
	}
}

// TestRecorderCloseFlushes checks that Close, the stop of the part
// paramShadowRecorder (#3717), writes what was counted since the previous
// summary, as Flush does.
func TestRecorderCloseFlushes(t *testing.T) {
	r, buf, clock := newTestRecorder(10 * time.Minute)
	r.Record(Event{Endpoint: "x", Kind: KindAgreed, HandlerStatus: 200})
	if got := buf.lines(t); len(got) != 0 {
		t.Fatalf("wrote before the interval: %v", got)
	}
	clock.t = clock.t.Add(30 * time.Second)
	r.Close(context.Background())
	want := []map[string]any{
		{"msg": "paramshadow summary", "since": "2026-10-18T12:00:00Z", "seconds": 30.0, "agreed": 1.0, "diverged": 0.0,
			"skipped": 0.0},
	}
	if got := buf.lines(t); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("lines =\n%v\nwant\n%v", got, want)
	}
}
