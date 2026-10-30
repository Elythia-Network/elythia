package paramshadow

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// enforcedResult is what one request to an enforcing middleware produced.
type enforcedResult struct {
	events []Event
	rec    *httptest.ResponseRecorder
	called bool
	// read is the body the handler read.
	read string
}

// serveEnforced routes one request through a middleware that enforces
// t/post, t/get and t/broken (plus opts) to a handler that answers 200.
func serveEnforced(t *testing.T, opts []Option, method, target, contentType, body string) enforcedResult {
	t.Helper()
	e := echo.New()
	sink := &fakeSink{}
	var res enforcedResult
	h := func(c echo.Context) error {
		res.called = true
		b, _ := io.ReadAll(c.Request().Body)
		res.read = string(b)
		return ok(c)
	}
	mw := Middleware(testEndpoints(t), sink, append([]Option{WithEnforced("t/post", "t/get", "t/broken")}, opts...)...)
	for _, p := range []string{"/api/t/post", "/api/t/get", "/api/t/broken"} {
		e.Match([]string{http.MethodGet, http.MethodPost, http.MethodPut}, p, h, mw)
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	res.rec = httptest.NewRecorder()
	e.ServeHTTP(res.rec, req)
	res.events = sink.events
	return res
}

func wantInvalidParam(t *testing.T, res enforcedResult, param, reason string) {
	t.Helper()
	if res.called {
		t.Error("the handler was called for a rejected request")
	}
	if res.rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", res.rec.Code)
	}
	want, err := json.Marshal(apierr.InvalidParamClient(param, reason))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.rec.Body.String()); got != string(want) {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
	if len(res.events) != 0 {
		t.Errorf("events %+v, want none", res.events)
	}
}

func wantHandler(t *testing.T, res enforcedResult, body string) {
	t.Helper()
	if !res.called || res.rec.Code != http.StatusOK {
		t.Errorf("handler called %v, status %d; want the handler's 200", res.called, res.rec.Code)
	}
	if res.read != body {
		t.Errorf("the handler read %q, want %q", res.read, body)
	}
}

func TestEnforcedRejectsWithUpstreamsAnswer(t *testing.T) {
	for _, tt := range []struct {
		name, body, param, reason string
	}{
		{"format", `{"noteId":"a-b"}`, "#/properties/noteId/format", `must match format "misskey:id"`},
		{"required", `{}`, "#/required", "must have required property 'noteId'"},
		{"null for a non-nullable property", `{"noteId":"abc","flag":null}`, "#/properties/flag/type", "must be boolean"},
		{"maximum", `{"noteId":"abc","limit":101}`, "#/properties/limit/maximum", "must be <= 100"},
		{"not an object", `[]`, "#/type", "must be object"},
		// 対になっていないサロゲートと不正なUTF-8は、U+FFFDに置き換えた値で判定する
		// (2026-10-31の監査)。飛ばすと1バイト混ぜるだけで拒否の検証を迂回できた。
		{"lone surrogate", `{"noteId":"\ud800"}`, "#/properties/noteId/format", `must match format "misskey:id"`},
		{"invalid utf-8", "{\"noteId\":\"abc\",\"limit\":101,\"x\":\"\xff\"}", "#/properties/limit/maximum", "must be <= 100"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wantInvalidParam(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, tt.body), tt.param, tt.reason)
		})
	}
}

// TestEnforcedAcceptedGoesToTheHandlerUnchanged checks that an accepted
// request reaches the handler with its own body: the default the validator
// inserts (limit: 10) stays in the validator's copy.
func TestEnforcedAcceptedGoesToTheHandlerUnchanged(t *testing.T) {
	for _, body := range []string{`{"noteId":"abc"}`, `{"noteId":"abc","limit":10.0}`} {
		res := serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, body)
		wantHandler(t, res, body)
		if len(res.events) != 0 {
			t.Errorf("%s: events %+v, want none", body, res.events)
		}
	}
}

// TestEnforcedLeavesWhatItCannotValidateToTheHandler covers the inputs the
// validator cannot judge like upstream: they go to the handler, whose own
// checks stay, rather than being rejected without upstream's verdict.
func TestEnforcedLeavesWhatItCannotValidateToTheHandler(t *testing.T) {
	large := `{"noteId":"a-b","pad":"` + strings.Repeat("x", 100) + `"}`
	for _, tt := range []struct {
		name                        string
		opts                        []Option
		method, target, ctype, body string
	}{
		{"too large", []Option{WithMaxBody(64)}, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, large},
		{"too many values", []Option{WithMaxValues(4)}, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"a-b","x":[1,2,3]}`},
		{"not json", nil, http.MethodPost, "/api/t/post", echo.MIMETextPlain, `{"noteId":"a-b"}`},
		{"method", nil, http.MethodPut, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"a-b"}`},
		{"GET cast failure", nil, http.MethodGet, "/api/t/get?noteId=abc&limit=x", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := serveEnforced(t, tt.opts, tt.method, tt.target, tt.ctype, tt.body)
			wantHandler(t, res, tt.body)
			if len(res.events) != 0 {
				t.Errorf("events %+v, want none", res.events)
			}
		})
	}
	// GETの検証そのものは効く(変換を通った値が本家の検証で落ちる)。
	res := serveEnforced(t, nil, http.MethodGet, "/api/t/get?noteId=abc&limit=0", "", "")
	wantInvalidParam(t, res, "#/properties/limit/minimum", "must be >= 1")
}

// TestEnforcedIgnoresTheBudget checks that an enforced request is validated
// when the budget is spent: skipping it would let it through unchecked.
func TestEnforcedIgnoresTheBudget(t *testing.T) {
	spent := WithBudget(0.1, 0)
	wantInvalidParam(t, serveEnforced(t, []Option{spent}, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"a-b"}`),
		"#/properties/noteId/format", `must match format "misskey:id"`)

	// 同じ設定で、拒否しないendpointは予算で飛ばされる(対照)。
	ev := one(t, serveWith(t, []Option{spent}, http.MethodPost, "/api/t/get", `{"noteId":"a-b"}`, ok))
	if ev.Skip != SkipBudget {
		t.Errorf("not enforced: event %+v, want budget", ev)
	}
}

// TestEnforcedIgnoresTheSlots holds the only slot with a shadow validation
// that blocks, and checks that an enforced request is still validated.
func TestEnforcedIgnoresTheSlots(t *testing.T) {
	orig := validate
	t.Cleanup(func() { validate = orig })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	validate = func(s *paramschema.Schema, data any) *paramschema.Error {
		blocked := false
		once.Do(func() { blocked = true })
		if blocked {
			close(entered)
			<-release
		}
		return orig(s, data)
	}

	e := echo.New()
	sink := &fakeSink{}
	mw := Middleware(testEndpoints(t), sink, WithEnforced("t/post"), WithMaxConcurrent(1))
	e.POST("/api/t/post", ok, mw)
	e.POST("/api/t/get", ok, mw)
	send := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	done := make(chan struct{})
	go func() { defer close(done); send("/api/t/get", `{"noteId":"abc"}`) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the shadow validation did not start")
	}
	if rec := send("/api/t/post", `{"noteId":"a-b"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("enforced request while the slot is held: status %d, want 400", rec.Code)
	}
	close(release)
	<-done
}

func TestEnforcedPanicGoesToTheHandler(t *testing.T) {
	orig := validate
	t.Cleanup(func() { validate = orig })
	validate = func(*paramschema.Schema, any) *paramschema.Error { panic("validator bug") }
	res := serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"a-b"}`)
	wantHandler(t, res, `{"noteId":"a-b"}`)
	if ev := one(t, res.events); ev.Kind != KindPanic || ev.Endpoint != "t/post" {
		t.Errorf("event %+v, want a panic of t/post", ev)
	}
}

func TestEnforcedCompileErrorGoesToTheHandler(t *testing.T) {
	res := serveEnforced(t, nil, http.MethodPost, "/api/t/broken", echo.MIMEApplicationJSON, `{"noteId":"a-b"}`)
	wantHandler(t, res, `{"noteId":"a-b"}`)
	if ev := one(t, res.events); ev.Kind != KindCompileError {
		t.Errorf("event %+v, want compile_error", ev)
	}
}

// TestWithoutShadow checks that the other upstream endpoints are neither
// validated nor recorded, while the enforced ones still reject.
func TestWithoutShadow(t *testing.T) {
	orig := validate
	t.Cleanup(func() { validate = orig })
	var validated []string
	validate = func(s *paramschema.Schema, data any) *paramschema.Error {
		validated = append(validated, "x")
		return orig(s, data)
	}
	e := echo.New()
	sink := &fakeSink{}
	mw := Middleware(testEndpoints(t), sink, WithEnforced("t/post"), WithoutShadow())
	e.POST("/api/t/post", ok, mw)
	e.POST("/api/t/get", ok, mw)
	send := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"noteId":"a-b"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send("/api/t/get"); code != http.StatusOK || len(validated) != 0 || len(sink.events) != 0 {
		t.Errorf("not enforced: status %d, validated %d times, events %+v", code, len(validated), sink.events)
	}
	if code := send("/api/t/post"); code != http.StatusBadRequest || len(validated) != 1 {
		t.Errorf("enforced: status %d, validated %d times", code, len(validated))
	}
}

// TestEnforcedTokenLimit pins the smaller token limit of the enforced
// endpoints: a body at the limit is validated, one token more goes to the
// handler unvalidated, and the same body to an endpoint that only counts is
// still validated under DefaultMaxValues.
func TestEnforcedTokenLimit(t *testing.T) {
	// {"noteId":"a-b","x":[0,0,...]} のトークンは、括弧4つとキー2つと値1つの
	// 7に、配列の要素の数を足したもの。
	body := func(tokens int) string {
		return `{"noteId":"a-b","x":[` + strings.TrimSuffix(strings.Repeat("0,", tokens-7), ",") + `]}`
	}
	at, over := body(DefaultEnforcedMaxValues), body(DefaultEnforcedMaxValues+1)
	if tooManyValues([]byte(at), DefaultEnforcedMaxValues) || !tooManyValues([]byte(over), DefaultEnforcedMaxValues) {
		t.Fatal("the bodies do not sit on the limit")
	}
	wantInvalidParam(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, at),
		"#/properties/noteId/format", `must match format "misskey:id"`)
	res := serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, over)
	wantHandler(t, res, over)

	// 数えるだけのendpointは、従来の上限(DefaultMaxValues)で検証する(対照)。
	if ev := one(t, serveWith(t, nil, http.MethodPost, "/api/t/get", over, ok)); ev.Kind != KindRejectedHandlerOK {
		t.Errorf("not enforced: event %+v, want it validated", ev)
	}
	// GETのqueryにも同じ上限が掛かる。
	q := "/api/t/get?noteId=a-b" + strings.Repeat("&x=0", DefaultEnforcedMaxValues)
	wantHandler(t, serveEnforced(t, nil, http.MethodGet, q, "", ""), "")
	// 上限は差し替えられる(上限2に対して3項目)。
	small := serveEnforced(t, []Option{withEnforcedMaxValues(2)}, http.MethodGet, "/api/t/get?noteId=a-b&limit=1&x=1", "", "")
	wantHandler(t, small, "")
}

// TestEnforcedDoesNotSpendTheBudget checks that the enforced endpoints'
// validations are not charged to the budget: charging them would let anyone
// who sends to an anonymous enforced endpoint stop the counting elsewhere.
func TestEnforcedDoesNotSpendTheBudget(t *testing.T) {
	clock := &manualClock{t: t0}
	orig := validate
	t.Cleanup(func() { validate = orig })
	validate = func(s *paramschema.Schema, data any) *paramschema.Error {
		clock.t = clock.t.Add(4 * time.Millisecond)
		return orig(s, data)
	}
	e := echo.New()
	sink := &fakeSink{}
	mw := Middleware(testEndpoints(t), sink, WithEnforced("t/post"), WithBudget(0.25, 10*time.Millisecond), withClock(clock.now))
	e.POST("/api/t/post", ok, mw)
	e.POST("/api/t/get", ok, mw)
	send := func(path string) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"noteId":"abc"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		e.ServeHTTP(httptest.NewRecorder(), req)
	}
	// 積まれれば、4msの検証を5回で予算(10ms)を超える。
	for range 5 {
		send("/api/t/post")
	}
	send("/api/t/get")
	if ev := one(t, sink.events); ev.Kind != KindAgreed {
		t.Errorf("event %+v; the enforced validations spent the budget", ev)
	}
}

// TestEnforcedSizeLimit pins the smaller size limit of the enforced
// endpoints: a body of exactly the limit is validated, one byte more goes to
// the handler unvalidated, and the same body to an endpoint that only counts
// is still validated under DefaultMaxBody.
func TestEnforcedSizeLimit(t *testing.T) {
	body := func(size int) string {
		head, tail := `{"noteId":"a-b","pad":"`, `"}`
		return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
	}
	at, over := body(DefaultEnforcedMaxBody), body(DefaultEnforcedMaxBody+1)
	wantInvalidParam(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, at),
		"#/properties/noteId/format", `must match format "misskey:id"`)
	wantHandler(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, over), over)

	// 数えるだけのendpointは、従来の上限(DefaultMaxBody)で検証する(対照)。
	if ev := one(t, serveWith(t, nil, http.MethodPost, "/api/t/get", over, ok)); ev.Kind != KindRejectedHandlerOK {
		t.Errorf("not enforced: event %+v, want it validated", ev)
	}
	// GETのqueryにも同じ上限が掛かる(上限16バイトに対して17バイト)。
	q := "noteId=a-b&x=123" // 16バイト
	wantInvalidParam(t, serveEnforced(t, []Option{withEnforcedMaxBody(len(q))}, http.MethodGet, "/api/t/get?"+q, "", ""),
		"#/properties/noteId/format", `must match format "misskey:id"`)
	wantHandler(t, serveEnforced(t, []Option{withEnforcedMaxBody(len(q))}, http.MethodGet, "/api/t/get?"+q+"4", "", ""), "")
}
