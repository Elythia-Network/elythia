package paramshadow

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// TestBudgetBoundary pins the leaky bucket: it refuses once the debt reaches
// burst, drains at cores seconds per second, never goes below zero, and
// ignores a clock that goes back.
func TestBudgetBoundary(t *testing.T) {
	b := newBudget(0.5, 10*time.Millisecond)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	if !b.admit(at(0)) {
		t.Fatal("an empty budget refused")
	}
	b.spend(at(0), 9*time.Millisecond)
	if !b.admit(at(0)) {
		t.Fatal("refused under burst (9ms of 10ms)")
	}
	b.spend(at(0), time.Millisecond)
	if b.admit(at(0)) {
		t.Fatal("admitted at burst (10ms of 10ms)")
	}
	// 0.5コアなので、1ms経つと0.5ms減る。
	if b.admit(at(0)) || !b.admit(at(time.Millisecond)) {
		t.Fatal("did not come back after 1ms (9.5ms of 10ms)")
	}
	if b.debt != 9500*time.Microsecond {
		t.Fatalf("debt = %v, want 9.5ms", b.debt)
	}
	// 時計が戻っても増えず、lastも戻らない。
	if !b.admit(at(-time.Hour)) || b.debt != 9500*time.Microsecond || !b.last.Equal(at(time.Millisecond)) {
		t.Fatalf("a clock going back changed the budget: debt %v last %v", b.debt, b.last)
	}
	// 長く空いても0より下がらない。下がると、その後に上限を超えて溜められる。
	b.spend(at(time.Hour), 10*time.Millisecond)
	if b.admit(at(time.Hour)) {
		t.Fatalf("the debt went below zero while idle (debt %v)", b.debt)
	}
	b.spend(at(time.Hour), -time.Second)
	if b.debt != 10*time.Millisecond {
		t.Fatalf("a negative duration changed the debt: %v", b.debt)
	}
}

func TestDefaultBudgetCores(t *testing.T) {
	prev := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	for procs, want := range map[int]float64{1: 0.1, 2: 0.125, 8: 0.5, 16: 1, 32: 2} {
		runtime.GOMAXPROCS(procs)
		if got := DefaultBudgetCores(); got != want {
			t.Errorf("GOMAXPROCS=%d: DefaultBudgetCores() = %v, want %v", procs, got, want)
		}
	}
	if DefaultBudgetBurst != 100*time.Millisecond {
		t.Errorf("DefaultBudgetBurst = %v; the docs say 100ms", DefaultBudgetBurst)
	}
}

// steppingClock advances by step at every reading, so that every validation
// takes exactly step (it reads the clock once at the start and once at the
// end), whatever path it takes.
type steppingClock struct {
	t    time.Time
	step time.Duration
}

func (c *steppingClock) now() time.Time {
	c.t = c.t.Add(c.step)
	return c.t
}

func serveBudget(t *testing.T, e *echo.Echo, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/t/post", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	e.ServeHTTP(httptest.NewRecorder(), req)
}

// manualClock is a clock the test moves by hand.
type manualClock struct{ t time.Time }

func (c *manualClock) now() time.Time { return c.t }

// TestMiddlewareBudget pins the budget in the middleware: requests are
// validated until the time they took reaches burst, then counted as budget
// without validating, and validated again once the budget has drained.
func TestMiddlewareBudget(t *testing.T) {
	clock := &manualClock{t: t0}
	orig := validate
	t.Cleanup(func() { validate = orig })
	// 検証は1件で4msかかったことにする。
	validate = func(s *paramschema.Schema, data any) *paramschema.Error {
		clock.t = clock.t.Add(4 * time.Millisecond)
		return orig(s, data)
	}
	e := echo.New()
	sink := &fakeSink{}
	e.POST("/api/t/post", ok, Middleware(testEndpoints(t), sink,
		WithBudget(0.25, 10*time.Millisecond), withClock(clock.now)))
	kinds := func(n int) string {
		for range n {
			serveBudget(t, e, `{"noteId":"abc"}`)
		}
		var out []string
		for _, ev := range sink.events {
			out = append(out, ev.Skip)
		}
		sink.events = nil
		return strings.Join(out, ",")
	}
	// 0.25コアなので、4msの検証の間に1ms減る。積み上がりは4ms → 7ms → 10ms。
	// 10msに達した後は、時計が進まない限り飛ばし続ける。
	if got := kinds(5); got != ",,,budget,budget" {
		t.Fatalf("events = %q", got)
	}
	// 時計が進まなければ、飛ばしている間は減らない。
	if got := kinds(1); got != "budget" {
		t.Fatalf("without time passing: %q", got)
	}
	clock.t = clock.t.Add(4 * time.Millisecond) // 1ms減って9ms
	if got := kinds(1); got != "" {
		t.Fatalf("after the budget drained: %q", got)
	}
}

// TestMiddlewareBudgetCountsEverySlotPath pins that the time of the paths
// that end inside the slot without validating (too_large here) is charged
// too: bodies over the size limit sent in a row run out the budget.
func TestMiddlewareBudgetCountsEverySlotPath(t *testing.T) {
	clock := &steppingClock{t: t0, step: 2 * time.Millisecond}
	e := echo.New()
	sink := &fakeSink{}
	e.POST("/api/t/post", ok, Middleware(testEndpoints(t), sink,
		WithBudget(0.01, 5*time.Millisecond), WithMaxBody(16), withClock(clock.now)))
	for range 10 {
		serveBudget(t, e, `{"noteId":"abcdefghijklmnop"}`)
	}
	last := sink.events[len(sink.events)-1]
	if sink.events[0].Skip != SkipTooLarge || last.Skip != SkipBudget {
		t.Errorf("events = %+v", sink.events)
	}
}

// TestMiddlewareDefaultBudget pins that Middleware sets up the default
// budget.
func TestMiddlewareDefaultBudget(t *testing.T) {
	var s *shadow
	_ = Middleware(map[string]paramschema.Endpoint{}, &fakeSink{}, func(got *shadow) { s = got })
	if s.budget == nil || s.budget.cores != DefaultBudgetCores() || s.budget.burst != DefaultBudgetBurst || s.now == nil {
		t.Errorf("default budget = %+v", s.budget)
	}
}
