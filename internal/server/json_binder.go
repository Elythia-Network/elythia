package server

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// errBodyNotObject is returned by c.Bind when an API endpoint received a body
// that is not a JSON object. Handlers answer it with INVALID_PARAM, and
// apierr.JSONInvalidParam adds upstream's {param: "#/type", reason: "must be
// object"} info because the binder also calls apierr.MarkBodyNotObject.
var errBodyNotObject = echo.NewHTTPError(http.StatusBadRequest, "must be object")

// nonEndpointAPIRoutes are the /api routes upstream serves outside the
// endpoint machinery (ApiServerService.ts registers them directly on fastify),
// so no ajv `type: 'object'` validation applies to their bodies.
var nonEndpointAPIRoutes = map[string]bool{
	"/api/signup":                true,
	"/api/signup-pending":        true,
	"/api/signin-flow":           true,
	"/api/signin-with-passkey":   true,
	"/api/miauth/:session/check": true,
	"/api/clear-browser-cache":   true,
}

// multipartUploadRoutes are the routes of upstream's requireFile endpoints
// (`meta.requireFile: true` in endpoints/*.ts; drive/files/create is the
// only one). ApiServerService.ts routes them to handleMultipartRequest,
// which middleware.RequireMultipartFile mirrors. mk-go's own multipart
// endpoint (drive/files/create-chunked/append) is not one of them.
var multipartUploadRoutes = []string{"/api/drive/files/create"}

// isAPIEndpointRoute reports whether c was routed to an /api endpoint whose
// upstream counterpart validates params with an ajv `type: 'object'` schema.
func isAPIEndpointRoute(c echo.Context) bool {
	p := c.Path()
	return strings.HasPrefix(p, "/api/") && !nonEndpointAPIRoutes[p]
}

// expectsObject reports whether binding into i (of type t) reads a JSON
// object: a struct or map that does not decode itself.
func expectsObject(t reflect.Type) bool {
	t = derefType(t)
	if t == nil || decodesItself(t) {
		return false
	}
	return t.Kind() == reflect.Struct || t.Kind() == reflect.Map
}

// rejectNonObjectBody returns errBodyNotObject (and marks c) when an API
// endpoint's JSON body is a valid non-object top-level value (null, array,
// string, number, boolean) and t expects an object. Upstream hands such a
// body straight to ajv, whose first error is `#/type` "must be object"
// (endpoint-base.ts). Malformed bodies are left to the decoder.
func rejectNonObjectBody(c echo.Context, t reflect.Type, data []byte) error {
	if !isAPIEndpointRoute(c) || !expectsObject(t) {
		return nil
	}
	if isNonObjectJSON(data) {
		apierr.MarkBodyNotObject(c)
		return errBodyNotObject
	}
	return nil
}

// apiBinder is echo's DefaultBinder plus upstream's treatment of a missing
// body on API endpoints.
type apiBinder struct {
	echo.DefaultBinder
}

// Bind binds like echo.DefaultBinder, then, for a non-GET/HEAD request to an
// API endpoint when i expects an object, rejects with errBodyNotObject a
// request without any body (no content type, or text/plain) and a text/plain
// body. Fastify sets request.body to undefined or to the raw string there
// and ApiCallService passes it to ajv as the params, which fails `type:
// 'object'`. An empty application/json body never gets here: JSONBodyParse
// answers it with FST_ERR_CTP_EMPTY_JSON_BODY first, as Fastify does.
//
// For a GET or HEAD request to an API endpoint it binds only the path and
// query params, never the body, as upstream does. A GET first rejects, with
// errQueryCast, a query value that upstream cannot cast to its param's type
// (see checkQueryCasts).
func (b *apiBinder) Bind(i any, c echo.Context) error {
	req := c.Request()
	if (req.Method == http.MethodGet || req.Method == http.MethodHead) && isAPIEndpointRoute(c) {
		if req.Method == http.MethodGet {
			if err := checkQueryCasts(c, reflect.TypeOf(i)); err != nil {
				return err
			}
		}
		// **GET の本文は読まない。** 本家 (Fastify) は GET の本文を parse せず、
		// ApiCallService は request.query だけを引数にする。echo の既定は query の
		// 後に本文も bind して上書きするので、`GET /api/emoji?name=foo` に本文
		// `{"name":"bar"}` を付けると bar の応答が返る。cacheSec の endpoint では
		// それに public が付き、URL だけを鍵にする CDN が別の内容を配ってしまう
		// (#3593)。
		if err := b.BindPathParams(c, i); err != nil {
			return err
		}
		return b.bindExactQueryParams(c, i)
	}
	err := b.DefaultBinder.Bind(i, c)
	if req.Method == http.MethodGet || req.Method == http.MethodHead || !isAPIEndpointRoute(c) || !expectsObject(reflect.TypeOf(i)) {
		return err
	}
	ct := req.Header.Get(echo.HeaderContentType)
	switch {
	case err == nil && req.ContentLength == 0 && (strings.TrimSpace(ct) == "" || isTextPlain(ct)):
	case errors.Is(err, echo.ErrUnsupportedMediaType) && isTextPlain(ct):
		// text/plain の body は Fastify の既定の parser が文字列のまま
		// request.body に入れるので、空でなくても ajv の #/type で落ちる。
	default:
		return err
	}
	apierr.MarkBodyNotObject(c)
	return errBodyNotObject
}

// bindExactQueryParams binds the `query` tagged fields of a struct i from the
// query params whose key equals the tag exactly. echo's binder also takes a
// key that matches ignoring case (`?Limit=` for `limit`), which upstream's
// fast-querystring + ajv never do: an unknown key is ignored there. Binding it
// let a caller hand the handler a value the validator never saw (the
// validator reads the query with exact keys; 2026-10-31 の監査)。A non-struct
// target (a map) keeps echo's behaviour.
func (b *apiBinder) bindExactQueryParams(c echo.Context, i any) error {
	t := derefType(reflect.TypeOf(i))
	if t == nil || t.Kind() != reflect.Struct {
		return b.BindQueryParams(c, i)
	}
	names := map[string]bool{}
	collectQueryNames(t, names)
	filtered := url.Values{}
	for k, vs := range c.QueryParams() {
		if names[k] {
			filtered[k] = vs
		}
	}
	// echo は c.QueryParams() を request の URL から読むので、完全一致の分だけを
	// 持つ URL の使い捨ての context で bind する。
	req := c.Request().Clone(c.Request().Context())
	u := *req.URL
	u.RawQuery = filtered.Encode()
	req.URL = &u
	return b.BindQueryParams(c.Echo().NewContext(req, c.Response()), i)
}

// collectQueryNames gathers the `query` tag names of t's fields, descending
// into untagged struct fields like echo's binder and firstCastFailure do.
func collectQueryNames(t reflect.Type, names map[string]bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		name := f.Tag.Get("query")
		if name == "" {
			if ft := derefType(f.Type); ft != nil && ft.Kind() == reflect.Struct && !decodesItself(ft) {
				collectQueryNames(ft, names)
			}
			continue
		}
		names[name] = true
	}
}

// errQueryCast is returned by c.Bind when a GET query value cannot be cast
// to its field's type. The binder also calls apierr.MarkCastFailure, so the
// handler's apierr.JSONInvalidParam answers with upstream's cast error.
var errQueryCast = echo.NewHTTPError(http.StatusBadRequest, "cannot cast query param")

// checkQueryCasts mirrors the "Cast non JSON input" step of upstream's
// ApiCallService.call for GET: every boolean / number / integer param whose
// query value is a single string goes through JSON.parse, and a value that
// does not parse is answered with INVALID_PARAM 0b5f1631 {param, reason:
// "cannot cast to <type>"} before ajv runs. The params are the `query`
// tagged fields of t (the same fields echo binds); a bool field is a
// boolean param, an integer field an integer param and a float field a
// number param. Fields are checked in declaration order and the first
// failure is reported (upstream walks paramDef.properties in its order).
func checkQueryCasts(c echo.Context, t reflect.Type) error {
	t = derefType(t)
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	query := c.QueryParams()
	if len(query) == 0 {
		return nil
	}
	if param, typ, ok := firstCastFailure(t, query); ok {
		apierr.MarkCastFailure(c, param, typ)
		return errQueryCast
	}
	return nil
}

// firstCastFailure walks t's fields like echo's query binder does
// (untagged struct fields are descended into) and returns the first
// `query` param whose value JSON.parse would reject.
func firstCastFailure(t reflect.Type, query map[string][]string) (param, typ string, ok bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		name := f.Tag.Get("query")
		if name == "" {
			if ft := derefType(f.Type); ft.Kind() == reflect.Struct && !decodesItself(ft) {
				if param, typ, ok := firstCastFailure(ft, query); ok {
					return param, typ, true
				}
			}
			continue
		}
		typ := castType(f.Type)
		if typ == "" {
			continue
		}
		// 本家は値が文字列のときだけ変換する。同じキーが 2 回来ると Fastify の
		// querystring は配列にするので変換されず、ajv の型の検査に回る。キーは
		// 完全一致で引く (本家の paramDef のキーは大文字小文字を区別する)。
		vs := query[name]
		if len(vs) != 1 {
			continue
		}
		if !json.Valid([]byte(vs[0])) {
			return name, typ, true
		}
	}
	return "", "", false
}

// castType returns the paramDef type upstream casts for a field of type t
// ("boolean", "integer" or "number"), or "" when no cast applies.
func castType(t reflect.Type) string {
	switch derefType(t).Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	}
	return ""
}

// isTextPlain reports whether Fastify's built-in text/plain parser handles
// a request of this content type (it then sets request.body to the string).
func isTextPlain(ct string) bool {
	essence, _, err := mime.ParseMediaType(ct)
	if err != nil {
		essence, _, _ = strings.Cut(ct, ";")
		essence = strings.ToLower(strings.TrimSpace(essence))
	}
	return essence == "text/plain"
}

// requireObjectBody rejects, on a non-GET/HEAD request to an API endpoint,
// a body upstream would hand to ajv as a non-object: a JSON value that is
// not an object, no body at all (no content type or text/plain), or a
// text/plain body. It answers like the binder does (INVALID_PARAM with
// info {param: "#/type", reason: "must be object"}).
//
// The endpoints whose upstream paramDef accepts a non-object body
// (anyBodyEndpoints) are let through, as upstream does.
//
// apiRoutes appends it as the last route middleware, so it runs after the
// route's credential / permission checks and right before the handler —
// the same place upstream's ajv validation has in ApiCallService.call. It
// covers handlers that never call c.Bind (endpoints without params).
func requireObjectBody(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		if req.Method == http.MethodGet || req.Method == http.MethodHead || !isAPIEndpointRoute(c) {
			return next(c)
		}
		ct := req.Header.Get(echo.HeaderContentType)
		// 本家の paramDef が {} の endpoint も、parser の無い Content-Type は fastify が
		// 415 にする (ajv より前)。それ以外の本文は何でも通す。
		if anyBodyEndpoints()[strings.TrimPrefix(c.Path(), "/api/")] {
			if isUnsupportedAPIContentType(ct) {
				return unsupportedMediaType(c)
			}
			return next(c)
		}
		notObject := false
		switch {
		case middleware.IsJSONContentType(ct):
			// JSONBodyParse が body を検証済みの bytes に差し替えている。読んで
			// 先頭の値の種類だけ見て、同じ bytes を戻す。
			data, err := io.ReadAll(req.Body)
			if err != nil {
				return err
			}
			req.Body = io.NopCloser(bytes.NewReader(data))
			notObject = isNonObjectJSON(data)
		case isTextPlain(ct):
			notObject = true
		case strings.TrimSpace(ct) == "":
			// 長さの分からない body (chunked など、ContentLength が -1) は
			// 読まないと空かどうか分からない。空なら body が無いのと同じ。
			if req.ContentLength < 0 && req.Body != nil {
				data, err := io.ReadAll(req.Body)
				if err != nil {
					return err
				}
				req.Body = io.NopCloser(bytes.NewReader(data))
				notObject = len(data) == 0
				break
			}
			notObject = req.ContentLength == 0
		case isMultipartForm(ct):
			// 本家は @fastify/multipart を全体に登録しているので 415 にはならず、
			// requireFile でない endpoint では request.body が object にならずに
			// ajv の #/type で落ちる。requireFile の endpoint (と Elythia の
			// create-chunked/append) は handler が multipart を読むので通す。
			if multipartBodyRoutes[c.Path()] {
				return next(c)
			}
			notObject = true
		default:
			// 本家は application/json・text/plain・multipart 以外の parser を持たず、
			// fastify が 415 を返す。以前は他の Content-Type を素通しして echo の
			// binder が束縛していたので、XML や form-urlencoded で送ると宣言の
			// 検証 (JSON の body だけを見る) を迂回して handler に届いた
			// (2026-10-31 の監査)。
			return unsupportedMediaType(c)
		}
		if !notObject {
			return next(c)
		}
		apierr.MarkBodyNotObject(c)
		return apierr.JSONInvalidParam(c)
	}
}

// isUnsupportedAPIContentType reports whether upstream's /api has no body parser
// for the content type: anything but application/json, text/plain, multipart
// and no content type at all.
func isUnsupportedAPIContentType(ct string) bool {
	return !middleware.IsJSONContentType(ct) && !isTextPlain(ct) && !isMultipartForm(ct) && strings.TrimSpace(ct) != ""
}

// unsupportedMediaType answers like fastify's FST_ERR_CTP_INVALID_MEDIA_TYPE.
func unsupportedMediaType(c echo.Context) error {
	return c.JSON(http.StatusUnsupportedMediaType, map[string]any{
		"statusCode": http.StatusUnsupportedMediaType,
		"code":       "FST_ERR_CTP_INVALID_MEDIA_TYPE",
		"error":      "Unsupported Media Type",
		"message":    "Unsupported Media Type",
	})
}

// multipartBodyRoutes are the /api routes whose handler reads a multipart body:
// upstream's requireFile endpoints (multipartUploadRoutes) and Elythia's
// drive/files/create-chunked/append (docs/param-shadow.md の宣言しない例外)。
var multipartBodyRoutes = func() map[string]bool {
	m := map[string]bool{"/api/drive/files/create-chunked/append": true}
	for _, p := range multipartUploadRoutes {
		m[p] = true
	}
	return m
}()

// isMultipartForm reports whether the content type is multipart/form-data.
func isMultipartForm(ct string) bool {
	essence, _, err := mime.ParseMediaType(ct)
	if err != nil {
		essence, _, _ = strings.Cut(ct, ";")
		essence = strings.ToLower(strings.TrimSpace(essence))
	}
	return essence == echo.MIMEMultipartForm
}

// anyBodyEndpoints returns the upstream endpoints whose paramDef accepts a
// body that is not an object (its paramDef is `{}`: reversi/invitations and
// admin/captcha/current). Upstream hands any body to ajv, which passes it, so
// requireObjectBody lets these through.
//
// 本家の paramDef に type が無いと、ajv は null・配列・文字列も通す。名前を手で
// 書くと本家の版上げで取り残されるので、golden の schema から求める (#3843)。
var anyBodyEndpoints = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	eps, err := paramschema.Upstream()
	if err != nil {
		return out
	}
	for name, ep := range eps {
		if ep.Schema != nil && ep.CompileErr == nil && ep.Schema.Validate(nil) == nil && ep.Schema.Validate("x") == nil {
			out[name] = true
		}
	}
	return out
})

// apiRoutes is the /api group whose POST and Match registrations get
// withParamShadow and requireObjectBody as their last route middleware, and
// whose GET registrations get withParamShadow.
type apiRoutes struct {
	*echo.Group
}

// POST registers a POST route like echo.Group.POST, then withParamShadow and
// requireObjectBody.
func (g apiRoutes) POST(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.Group.POST(path, h, append(m, withParamShadow, requireObjectBody)...)
}

// Match registers routes like echo.Group.Match, then withParamShadow and
// requireObjectBody (which skips GET / HEAD itself).
func (g apiRoutes) Match(methods []string, path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) []*echo.Route {
	return g.Group.Match(methods, path, h, append(m, withParamShadow, requireObjectBody)...)
}

// GET registers a GET route like echo.Group.GET, then withParamShadow.
func (g apiRoutes) GET(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.Group.GET(path, h, append(m, withParamShadow)...)
}

// isNonObjectJSON reports whether data (valid JSON) is a top-level value
// other than an object.
func isNonObjectJSON(data []byte) bool {
	switch b := firstJSONByte(data); {
	case b == 'n', b == '[', b == '"', b == 't', b == 'f', b == '-', b >= '0' && b <= '9':
		return true
	}
	return false
}

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// firstJSONByte returns the first non-whitespace byte of data, or 0.
func firstJSONByte(data []byte) byte {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return b
	}
	return 0
}

func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// decodesItself reports whether encoding/json hands values of type t to a
// custom unmarshaler instead of decoding them itself.
func decodesItself(t reflect.Type) bool {
	pt := reflect.PointerTo(t)
	return pt.Implements(jsonUnmarshalerType) || pt.Implements(textUnmarshalerType)
}
