package server

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/server/middleware"
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
func (b *apiBinder) Bind(i any, c echo.Context) error {
	err := b.DefaultBinder.Bind(i, c)
	req := c.Request()
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
		}
		if !notObject {
			return next(c)
		}
		apierr.MarkBodyNotObject(c)
		return apierr.JSONInvalidParam(c)
	}
}

// apiRoutes is the /api group whose POST and Match registrations get
// requireObjectBody as their last route middleware.
type apiRoutes struct {
	*echo.Group
}

// POST registers a POST route like echo.Group.POST, then requireObjectBody.
func (g apiRoutes) POST(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.Group.POST(path, h, append(m, requireObjectBody)...)
}

// Match registers routes like echo.Group.Match, then requireObjectBody (which
// skips GET / HEAD itself).
func (g apiRoutes) Match(methods []string, path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) []*echo.Route {
	return g.Group.Match(methods, path, h, append(m, requireObjectBody)...)
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
