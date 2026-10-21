package paramshadow

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// Kinds of outcome. The first four are disagreements between the validator
// and the handler.
const (
	// KindRejectedHandlerOK: the validator rejects, the handler answers 2xx.
	KindRejectedHandlerOK = "rejected_handler_ok"
	// KindAcceptedHandlerInvalidParam: the validator accepts, the handler
	// answers INVALID_PARAM.
	KindAcceptedHandlerInvalidParam = "accepted_handler_invalid_param"
	// KindParamMismatch: both reject, and the handler's info.param is not the
	// validator's schemaPath (or the handler has no info.param).
	KindParamMismatch = "param_mismatch"
	// KindRejectedHandlerOtherError: the validator rejects, the handler
	// answers an error other than INVALID_PARAM (NO_SUCH_NOTE for an id
	// that is not misskey:id, for example). Switching to the validator
	// changes the error code of these requests.
	KindRejectedHandlerOtherError = "rejected_handler_other_error"
	// KindPanic: the validator panicked. The request went on to the handler.
	KindPanic = "validator_panic"
	// KindCompileError: the endpoint's paramDef did not compile.
	KindCompileError = "compile_error"
	// KindAgreed: both accept, or both reject with the same param.
	KindAgreed = "agreed"
	// KindSkipped: the request was not validated; Event.Skip says why.
	KindSkipped = "skipped"
)

// Event is the outcome of one request to an upstream endpoint. It holds no
// value of the request.
type Event struct {
	// Endpoint is the upstream endpoint name ("notes/create").
	Endpoint string
	Kind     string
	// SchemaPath is what upstream answers as info.param: the schemaPath of
	// the validator's first error, or the param name when the GET cast step
	// fails. Empty when the validator accepts.
	SchemaPath string
	// HandlerParam is the info.param of the handler's INVALID_PARAM, when it
	// has the shape of a schemaPath or a param name; otherParam otherwise.
	HandlerParam string
	// HandlerStatus is the status the handler answered with.
	HandlerStatus int
	// HandlerCode is the error.code of the handler's answer, when it has the
	// shape of one; otherParam otherwise.
	HandlerCode string
	// Skip is the reason of KindSkipped (Skip* constants).
	Skip string
}

// Sink receives the outcome of every request the middleware looks at.
type Sink interface {
	Record(Event)
}

// otherParam replaces a handler string that does not look like a schemaPath,
// a param name or an error code, so that nothing taken from a request can
// reach the record.
const otherParam = "(other)"

var (
	// handlerの文字列はソースのリテラルだけのはずだが、記録に値が混ざらない
	// ことをここでも守る。schemaPathか項目名か、errorのcodeの形だけを通す。
	paramShape = regexp.MustCompile(`^(#(/[A-Za-z0-9_]{1,64}){0,12}|[A-Za-z_][A-Za-z0-9_]{0,63})$`)
	codeShape  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// maxCapturedBody bounds the bytes of a 400 answer kept to read its
// error.code and info.param. An INVALID_PARAM answer is a few hundred bytes.
const maxCapturedBody = 8 << 10

// DefaultMaxConcurrent is the number of validations that may run at once
// by default: a quarter of GOMAXPROCS at the time Middleware is called, and
// at least 1.
//
// 上限ちょうどの最も重いbody(未知のキーに空のobjectを並べたもの)は、1件で
// 数msのCPUと数MBの確保を使う。枠をCPUの数の4分の1にして、検証が使える
// CPUをその分までに抑える。
func DefaultMaxConcurrent() int { return max(1, runtime.GOMAXPROCS(0)/4) }

// Option changes a limit of Middleware.
type Option func(*shadow)

// WithMaxBody sets the largest JSON body (and query) validated, in bytes
// (DefaultMaxBody by default). A longer one is counted as SkipTooLarge.
func WithMaxBody(n int) Option { return func(s *shadow) { s.maxBody = n } }

// WithMaxValues sets the largest number of JSON tokens or query params
// validated (DefaultMaxValues by default). More is counted as
// SkipTooManyValues.
func WithMaxValues(n int) Option { return func(s *shadow) { s.maxValues = n } }

// WithBudget sets the validation time per second (in cores) and how much of
// it may accumulate (DefaultBudgetCores and DefaultBudgetBurst by default).
// While the accumulated time is at or over burst, requests are counted as
// SkipBudget without validating.
func WithBudget(cores float64, burst time.Duration) Option {
	return func(s *shadow) { s.budget = newBudget(cores, burst) }
}

// WithEnforced makes the middleware answer for the endpoints named (by
// upstream name): a request the validator rejects gets upstream's 400
// INVALID_PARAM (paramschema.Error.Response) instead of reaching the handler
// (#3742). Their requests are validated whatever the budget and the
// concurrency limit, with the smaller limits DefaultEnforcedMaxBody and
// DefaultEnforcedMaxValues instead, and are not recorded except KindPanic
// and KindCompileError. A request that cannot be validated (a Skip* reason,
// or the GET cast step failing) goes on to the handler, whose own checks
// stay.
func WithEnforced(names ...string) Option {
	return func(s *shadow) {
		s.enforced = make(map[string]bool, len(names))
		for _, n := range names {
			s.enforced[n] = true
		}
	}
}

// WithoutShadow stops validating and recording the requests to the endpoints
// not given to WithEnforced; they go straight to the handler
// (disableParamShadow). A paramDef that does not compile is still recorded as
// KindCompileError.
func WithoutShadow() Option { return func(s *shadow) { s.shadowOff = true } }

// withEnforcedMaxBody sets the size limit of the enforced endpoints (tests).
func withEnforcedMaxBody(n int) Option { return func(s *shadow) { s.enforcedMaxBody = n } }

// withEnforcedMaxValues sets the token limit of the enforced endpoints
// (tests).
func withEnforcedMaxValues(n int) Option { return func(s *shadow) { s.enforcedMaxValues = n } }

// withClock replaces the clock of the budget (tests).
func withClock(now func() time.Time) Option { return func(s *shadow) { s.now = now } }

// WithMaxConcurrent sets how many validations may run at once
// (DefaultMaxConcurrent by default). A request that finds no free slot is
// counted as SkipBusy and goes on without waiting.
func WithMaxConcurrent(n int) Option {
	return func(s *shadow) { s.slots = make(chan struct{}, max(n, 1)) }
}

type shadow struct {
	eps  map[string]paramschema.Endpoint
	sink Sink
	// enforcedは、検証の結果で拒否するendpoint(本家の名前)。
	enforced map[string]bool
	// enforcedMaxBodyとenforcedMaxValuesは、拒否するendpointの大きさと
	// トークンの上限。予算と枠を通さないので、1件の費用をこちらで抑える。
	enforcedMaxBody   int
	enforcedMaxValues int
	// shadowOffは、enforced以外を検証も記録もせずに通す(disableParamShadow)。
	shadowOff bool
	maxBody   int
	// maxValues はトークンの数の上限。bodyの大きさだけでは、小さな値を並べた
	// bodyのdecodeの確保を抑えられない(#3677のレビュー2周目M-1)。
	maxValues int
	// slots は同時に走る検証(bodyの写しとdecodeと検証)の数を絞る。空きが無い
	// ときは待たずに数えるだけにする。検証のためにリクエストを待たせない。
	slots chan struct{}
	// budget は時間あたりに検証へ使う時間の予算。枠だけでは、1件が軽いときに
	// 1つの枠が1コアを使い続けられる(#3677のレビュー2周目M-1)。
	budget *budget
	now    func() time.Time
}

// Middleware returns the route middleware that validates the params of the
// requests to the endpoints of eps (by upstream name; the route is /api/<name>)
// and reports every outcome to sink. It changes neither the response nor
// what the handler reads, except for the endpoints given to WithEnforced,
// whose rejected requests get upstream's 400. Install it as a route
// middleware after the route's credential and permission checks, where
// upstream runs ajv.
//
// The cost is bounded: a body (or query) over the size or token limit is not
// validated, and at most the concurrency limit of validations run at once
// within the budget. The enforced endpoints skip the budget and the
// concurrency limit and have the smaller limits DefaultEnforcedMaxBody and
// DefaultEnforcedMaxValues instead.
func Middleware(eps map[string]paramschema.Endpoint, sink Sink, opts ...Option) echo.MiddlewareFunc {
	s := &shadow{
		eps: eps, sink: sink, maxBody: DefaultMaxBody, maxValues: DefaultMaxValues,
		enforcedMaxBody: DefaultEnforcedMaxBody, enforcedMaxValues: DefaultEnforcedMaxValues,
		budget: newBudget(DefaultBudgetCores(), DefaultBudgetBurst), now: time.Now,
	}
	WithMaxConcurrent(DefaultMaxConcurrent())(s)
	for _, o := range opts {
		o(s)
	}
	return s.middleware
}

func (s *shadow) middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		name, ok := strings.CutPrefix(c.Path(), "/api/")
		if !ok {
			return next(c)
		}
		ep, ok := s.eps[name]
		if !ok {
			return next(c)
		}
		if ep.CompileErr != nil {
			s.sink.Record(Event{Endpoint: name, Kind: KindCompileError})
			return next(c)
		}
		if s.enforced[name] {
			return s.enforce(c, name, ep, next)
		}
		if s.shadowOff {
			return next(c)
		}
		in := s.validate(c, ep, false)
		switch {
		case in.skip != "":
			s.sink.Record(Event{Endpoint: name, Kind: KindSkipped, Skip: in.skip})
			return next(c)
		case in.panicked:
			s.sink.Record(Event{Endpoint: name, Kind: KindPanic})
			return next(c)
		}

		res := c.Response()
		w := &captureWriter{ResponseWriter: res.Writer}
		res.Writer = w
		// handlerがpanicしても、外側(Recover)の書く応答は元のwriterへ行く。
		defer func() { res.Writer = w.ResponseWriter }()
		err := next(c)

		ev := classify(in.verdict, w.outcome(err))
		ev.Endpoint = name
		s.sink.Record(ev)
		return err
	}
}

// enforce validates a request to an enforced endpoint and answers upstream's
// 400 when the validator rejects it.
//
// **検証できない入力は拒否せず、handlerの検査に任せる。** 64KiBを超えるbody
// やトークンの多いbodyを本家の判定なしに拒否すると、本家が通すリクエストを
// 落としうる。handlerの手書きの検査は消さずに残してあるので、そこで今までと
// 同じ判定を受ける。
func (s *shadow) enforce(c echo.Context, name string, ep paramschema.Endpoint, next echo.HandlerFunc) error {
	in := s.validate(c, ep, true)
	switch {
	case in.panicked:
		s.sink.Record(Event{Endpoint: name, Kind: KindPanic})
		return next(c)
	case !in.rejected, in.cast:
		// 検証しなかった入力(in.skip)は判定を持たず、rejectedはfalseになる。
		// GETの変換の失敗は、本家ではajvより先の別のエラーになる。binderが
		// 同じ形で答えるので、handlerへ渡す。
		return next(c)
	}
	e := paramschema.Error{SchemaPath: in.param, Message: in.message}
	return c.JSON(http.StatusBadRequest, e.Response())
}

// judged is the validator's verdict on one request, or why there is none.
type judged struct {
	verdict
	skip     string
	panicked bool
}

// validate builds what upstream would validate, without changing what the
// handler reads, and validates it, holding a slot while it does. An enforced
// request is validated without the budget and the slots: skipping it would
// let the request through without upstream's check.
func (s *shadow) validate(c echo.Context, ep paramschema.Endpoint, enforced bool) judged {
	req := c.Request()
	if ep.RequireFile {
		return judged{skip: SkipMultipart}
	}
	isGet := req.Method == http.MethodGet
	switch {
	case isGet && !ep.AllowGet, !isGet && req.Method != http.MethodPost:
		return judged{skip: SkipMethod}
	case isGet && len(req.URL.RawQuery) > s.bodyLimit(enforced):
		return judged{skip: SkipTooLarge}
	case isGet && queryTooManyValues(req.URL.RawQuery, s.valueLimit(enforced)):
		return judged{skip: SkipTooManyValues}
	case !isGet && !middleware.IsJSONContentType(req.Header.Get(echo.HeaderContentType)):
		return judged{skip: SkipNotJSON}
	}
	if !enforced {
		if !s.budget.admit(s.now()) {
			return judged{skip: SkipBudget}
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			return judged{skip: SkipBusy}
		}
	}
	// 予算に積むのは、枠の中の全部(bodyの読み取り、トークンの走査、decode、
	// 検証)にかかった時間。走査も64KiBで約0.1msかかり、上限を超えるbodyを
	// 送り続けられたときに数えないと、その分が予算の外になる。
	// 拒否するendpointの分は積まない。積むと、匿名で呼べるendpoint(clips/show
	// など)を送り続けるだけで予算を使い切らせ、他のendpointの計測を止められる
	// (#3742のレビュー1周目M-1)。費用は大きさとトークンの上限で抑える
	// (bodyLimit、valueLimit)。
	if !enforced {
		start := s.now()
		defer func() {
			end := s.now()
			s.budget.spend(end, end.Sub(start))
		}()
	}
	var in input
	if isGet {
		in = queryInput(req.URL.RawQuery, ep.Schema)
	} else {
		body, tooLarge, err := readBody(req, s.bodyLimit(enforced))
		switch {
		case tooLarge:
			return judged{skip: SkipTooLarge}
		case err != nil || len(body) == 0:
			return judged{skip: SkipBody}
		case tooManyValues(body, s.valueLimit(enforced)):
			return judged{skip: SkipTooManyValues}
		}
		in = jsonInput(body)
	}
	if in.skip != "" {
		return judged{skip: in.skip}
	}
	v, panicked := judge(ep.Schema, in)
	return judged{verdict: v, panicked: panicked}
}

// bodyLimit is the size limit of a request: maxBody, or for an enforced
// endpoint the smaller enforcedMaxBody.
func (s *shadow) bodyLimit(enforced bool) int {
	if enforced {
		return min(s.maxBody, s.enforcedMaxBody)
	}
	return s.maxBody
}

// valueLimit is the token limit of a request: maxValues, or for an enforced
// endpoint the smaller enforcedMaxValues.
func (s *shadow) valueLimit(enforced bool) int {
	if enforced {
		return min(s.maxValues, s.enforcedMaxValues)
	}
	return s.maxValues
}

// verdict is upstream's answer to the params: accepted, or rejected with
// info.param = param and info.reason = message. cast is set when the GET
// cast step rejected (param is then the param name and message is empty).
type verdict struct {
	rejected bool
	param    string
	message  string
	cast     bool
}

var panicStackOnce sync.Once

// validate is Schema.Validate; the tests replace it to make it panic.
var validate = (*paramschema.Schema).Validate

// judge validates in against s, recovering a panic of the validator.
func judge(s *paramschema.Schema, in input) (v verdict, panicked bool) {
	if in.castFailed != "" {
		return verdict{rejected: true, param: in.castFailed, cast: true}, false
	}
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			// 値を含みうるpanicの値は出さない。場所を知るためのstackだけを、
			// プロセスで1度だけ出す。
			panicStackOnce.Do(func() {
				slog.Warn("paramshadow: the validator panicked; counting it and passing the request on", "stack", string(debug.Stack()))
			})
		}
	}()
	if e := validate(s, in.data); e != nil {
		return verdict{rejected: true, param: e.SchemaPath, message: e.Message}, false
	}
	return verdict{}, false
}

// handlerOutcome is what the handler answered.
type handlerOutcome struct {
	status       int
	invalidParam bool
	param        string
	code         string
}

// classify combines the validator's verdict and the handler's answer.
func classify(v verdict, h handlerOutcome) Event {
	ev := Event{SchemaPath: v.param, HandlerStatus: h.status, HandlerCode: h.code, HandlerParam: h.param}
	switch {
	case !v.rejected && h.invalidParam:
		ev.Kind = KindAcceptedHandlerInvalidParam
	case !v.rejected:
		ev.Kind = KindAgreed
	case h.status >= 200 && h.status < 300:
		ev.Kind = KindRejectedHandlerOK
	case !h.invalidParam:
		ev.Kind = KindRejectedHandlerOtherError
	case h.param != v.param:
		ev.Kind = KindParamMismatch
	default:
		ev.Kind = KindAgreed
	}
	return ev
}

// captureWriter passes everything through to the response and keeps the
// status and, for a 400, the first maxCapturedBody bytes of the body.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status == http.StatusBadRequest && len(w.body) < maxCapturedBody {
		w.body = append(w.body, b[:min(len(b), maxCapturedBody-len(w.body))]...)
	}
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher when the wrapped writer does.
func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker when the wrapped writer does.
func (w *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("paramshadow: the response writer does not support hijacking")
}

// Unwrap lets http.ResponseController reach the wrapped writer.
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// outcome reads what the handler answered. err is what the handler chain
// returned: when nothing was written, echo's error handler answers with its
// status (500 for an error that is not an *echo.HTTPError).
func (w *captureWriter) outcome(err error) handlerOutcome {
	status := w.status
	if status == 0 {
		status = http.StatusOK
		if err != nil {
			status = echoErrorStatus(err)
		}
	}
	h := handlerOutcome{status: status}
	if status != http.StatusBadRequest || len(w.body) == 0 {
		return h
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
			Info struct {
				Param any `json:"param"`
			} `json:"info"`
		} `json:"error"`
	}
	if json.Unmarshal(w.body, &env) != nil || env.Error.Code == "" {
		return h
	}
	h.code = sanitize(env.Error.Code, codeShape)
	h.invalidParam = env.Error.Code == "INVALID_PARAM"
	if p, ok := env.Error.Info.Param.(string); ok {
		h.param = sanitize(p, paramShape)
	} else if env.Error.Info.Param != nil {
		h.param = otherParam
	}
	return h
}

// echoErrorStatus is the status echo's DefaultHTTPErrorHandler answers err
// with: the code of an *echo.HTTPError (or of the *echo.HTTPError it holds as
// Internal), 500 for anything else. Like echo, it does not unwrap err.
func echoErrorStatus(err error) int {
	// echoのDefaultHTTPErrorHandlerと同じく、包まれたerrorは見ない(errors.Asに
	// しない)。fmt.Errorfで包んだHTTPErrorは、echoでは500になる。
	he, ok := err.(*echo.HTTPError)
	if !ok {
		return http.StatusInternalServerError
	}
	if inner, ok := he.Internal.(*echo.HTTPError); ok {
		return inner.Code
	}
	return he.Code
}

func sanitize(s string, shape *regexp.Regexp) string {
	if shape.MatchString(s) {
		return s
	}
	return otherParam
}
