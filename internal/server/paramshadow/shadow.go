package paramshadow

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// Option changes a limit of Middleware.
type Option func(*shadow)

// WithMaxBody sets the largest JSON body (and query) validated, in bytes
// (DefaultMaxBody by default). A longer one goes to the handler unvalidated.
func WithMaxBody(n int) Option { return func(s *shadow) { s.maxBody = n } }

// WithMaxValues sets the largest number of JSON tokens or query params
// validated (DefaultMaxValues by default). A request with more goes to the
// handler unvalidated.
func WithMaxValues(n int) Option { return func(s *shadow) { s.maxValues = n } }

type shadow struct {
	eps map[string]paramschema.Endpoint
	// maxBodyとmaxValuesは、検証するbodyの大きさとトークンの上限。検証は
	// 予算や同時に走る数の枠を通さないので、1件の費用をこちらで抑える。
	maxBody   int
	maxValues int
}

// Middleware returns the route middleware that validates the params of the
// requests to the endpoints of eps (by upstream name; the route is /api/<name>)
// and answers a request the validator rejects with upstream's 400
// INVALID_PARAM (paramschema.Error.Response) instead of reaching the handler
// (#3742). Install it as a route middleware after the route's credential and
// permission checks, where upstream runs ajv.
//
// A request that cannot be validated (a Skip* reason, the GET cast step
// failing, a paramDef that does not compile, or the validator panicking) goes
// on to the handler, whose own checks stay. The cost of one request is
// bounded by the size and token limits (DefaultMaxBody, DefaultMaxValues).
func Middleware(eps map[string]paramschema.Endpoint, opts ...Option) echo.MiddlewareFunc {
	s := &shadow{eps: eps, maxBody: DefaultMaxBody, maxValues: DefaultMaxValues}
	for _, o := range opts {
		o(s)
	}
	return s.middleware
}

// middleware validates a request to an endpoint of eps and answers
// upstream's 400 when the validator rejects it.
//
// **検証できない入力は拒否せず、handlerの検査に任せる。** 16KiBを超えるbody
// やトークンの多いbodyを本家の判定なしに拒否すると、本家が通すリクエストを
// 落としうる。handlerの手書きの検査は消さずに残してあるので、そこで今までと
// 同じ判定を受ける。
func (s *shadow) middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		name, ok := strings.CutPrefix(c.Path(), "/api/")
		if !ok {
			return next(c)
		}
		ep, ok := s.eps[name]
		if !ok || ep.CompileErr != nil {
			// compileできない宣言は、起動のときに警告を出してある。
			return next(c)
		}
		in := s.validate(c, ep)
		if in.panicked || !in.rejected || in.cast {
			// 検証しなかった入力(in.skip)は判定を持たず、rejectedはfalseになる。
			// GETの変換の失敗は、本家ではajvより先の別のエラーになる。binderが
			// 同じ形で答えるので、handlerへ渡す。
			return next(c)
		}
		e := paramschema.Error{SchemaPath: in.param, Message: in.message}
		return c.JSON(http.StatusBadRequest, e.Response())
	}
}

// judged is the validator's verdict on one request, or why there is none.
type judged struct {
	verdict
	skip     string
	panicked bool
}

// validate builds what upstream would validate, without changing what the
// handler reads, and validates it.
func (s *shadow) validate(c echo.Context, ep paramschema.Endpoint) judged {
	req := c.Request()
	if ep.RequireFile {
		return judged{skip: SkipMultipart}
	}
	isGet := req.Method == http.MethodGet
	switch {
	case isGet && !ep.AllowGet, !isGet && req.Method != http.MethodPost:
		return judged{skip: SkipMethod}
	case isGet && len(req.URL.RawQuery) > s.maxBody:
		return judged{skip: SkipTooLarge}
	case isGet && queryTooManyValues(req.URL.RawQuery, s.maxValues):
		return judged{skip: SkipTooManyValues}
	case !isGet && !middleware.IsJSONContentType(req.Header.Get(echo.HeaderContentType)):
		return judged{skip: SkipNotJSON}
	}
	var in input
	if isGet {
		in = queryInput(req.URL.RawQuery, ep.Schema)
	} else {
		body, tooLarge, err := readBody(req, s.maxBody)
		switch {
		case tooLarge:
			return judged{skip: SkipTooLarge}
		case err != nil || len(body) == 0:
			return judged{skip: SkipBody}
		case tooManyValues(body, s.maxValues):
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
				slog.Warn("paramshadow: the validator panicked; passing the request on to the handler", "stack", string(debug.Stack()))
			})
		}
	}()
	if e := validate(s, in.data); e != nil {
		return verdict{rejected: true, param: e.SchemaPath, message: e.Message}, false
	}
	return verdict{}, false
}
