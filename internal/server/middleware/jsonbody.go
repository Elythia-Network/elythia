package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
)

// fastifyError is the error envelope Fastify's default error handler
// serializes for content-type-parser failures. Upstream Misskey leaves the
// /api scope on the default JSON parser and default error handler, so a
// malformed JSON body is answered with this shape instead of the Misskey
// API error envelope. Field order matches Fastify's generated serializer
// (statusCode, code, error, message).
type fastifyError struct {
	StatusCode int    `json:"statusCode"`
	Code       string `json:"code"`
	Error      string `json:"error"`
	Message    string `json:"message"`
}

// Fastify 5 の FST_ERR_CTP_* 定義 (lib/errors.js) と同文。
var (
	errEmptyJSONBody = fastifyError{
		StatusCode: http.StatusBadRequest,
		Code:       "FST_ERR_CTP_EMPTY_JSON_BODY",
		Error:      "Bad Request",
		Message:    "Body cannot be empty when content-type is set to 'application/json'",
	}
	errInvalidJSONBody = fastifyError{
		StatusCode: http.StatusBadRequest,
		Code:       "FST_ERR_CTP_INVALID_JSON_BODY",
		Error:      "Bad Request",
		Message:    "Body is not valid JSON but content-type is set to 'application/json'",
	}
)

// secure-json-parse の fast-path 正規表現の移植。raw body に怪しい
// パターンが無ければ構造walkを省略できる (大半のリクエストは1回の
// 正規表現スキャンだけで通過する)。エスケープされた unicode 形式
// (_ 等) もオリジナルと同様に検出する。
var (
	suspectProtoRx       = regexp.MustCompile(`"(?:_|\\u005[Ff])(?:_|\\u005[Ff])(?:p|\\u0070)(?:r|\\u0072)(?:o|\\u006[Ff])(?:t|\\u0074)(?:o|\\u006[Ff])(?:_|\\u005[Ff])(?:_|\\u005[Ff])"\s*:`)
	suspectConstructorRx = regexp.MustCompile(`"(?:c|\\u0063)(?:o|\\u006[Ff])(?:n|\\u006[Ee])(?:s|\\u0073)(?:t|\\u0074)(?:r|\\u0072)(?:u|\\u0075)(?:c|\\u0063)(?:t|\\u0074)(?:o|\\u006[Ff])(?:r|\\u0072)"\s*:`)
)

// utf8BOM is the UTF-8 byte order mark Fastify's JSON parser strips before
// JSON.parse (secure-json-parse normalizes charCode 0xFEFF at index 0).
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// JSONBodyParse mirrors Fastify's application/json content-type parser for
// the /api scope. Upstream parses the body during the request lifecycle
// before the endpoint handler (and before rate limiting / authentication,
// which Misskey performs inside the handler), so a malformed or empty JSON
// body short-circuits with a Fastify error envelope regardless of endpoint,
// auth state or rate limit budget. Registering this middleware first on the
// /api group reproduces that ordering (#1609).
//
// Requests whose content type is not application/json (multipart uploads,
// no content type at all) pass through untouched, as do GET/HEAD requests:
// Fastify never parses bodies for those methods. After this middleware,
// c.Bind can no longer fail on malformed JSON for application/json POSTs;
// remaining Bind failures are type mismatches, which handlers keep
// answering with the Misskey INVALID_PARAM envelope (= upstream's schema
// validation error), preserving the distinction between the two layers.
func JSONBodyParse() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			// FastifyはGET/HEAD/TRACEのbodyを一切パースしない (bodyless
			// method 扱い)。content-typeがJSONでもそのまま素通しする。
			if req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodTrace {
				return next(c)
			}
			if !IsJSONContentType(req.Header.Get(echo.HeaderContentType)) {
				return next(c)
			}

			body, err := io.ReadAll(req.Body)
			if err != nil {
				// BodyLimitByPath (#2075) が wrap した limitedReader は body 超過時に
				// 413 HTTPError を返す。ContentLength 経路は middleware 段で 413 に
				// なるが、chunked 超過はここで read error になるため echo.HTTPError を
				// 伝播して upstream fastify と同じ 413 を返す (その他 read error は 400)。
				var he *echo.HTTPError
				if errors.As(err, &he) {
					return he
				}
				return c.JSON(http.StatusBadRequest, errInvalidJSONBody)
			}
			if len(body) == 0 {
				return c.JSON(http.StatusBadRequest, errEmptyJSONBody)
			}
			// secure-json-parse は先頭の U+FEFF (UTF-8 BOM) を除去してから
			// JSON.parse するため、BOM付き有効JSONは受理される。Goの
			// encoding/json はBOMを受け付けないので、ここで剥がしたbodyを
			// 下流の c.Bind にも渡す。
			body = bytes.TrimPrefix(body, utf8BOM)
			if !json.Valid(body) || isPoisonedJSON(body) {
				return c.JSON(http.StatusBadRequest, errInvalidJSONBody)
			}

			req.Body = io.NopCloser(bytes.NewReader(body))
			return next(c)
		}
	}
}

// isPoisonedJSON reports whether the (already syntactically valid) JSON
// body would be rejected by secure-json-parse with the Fastify defaults
// onProtoPoisoning='error' / onConstructorPoisoning='error': any object
// holding an own "__proto__" key, or a "constructor" key whose value is an
// object with an own "prototype" key.
func isPoisonedJSON(body []byte) bool {
	// fast path: raw text に怪しいキーが見えなければ走査しない
	if !suspectProtoRx.Match(body) && !suspectConstructorRx.Match(body) {
		return false
	}
	// UseNumberで数値をjson.Numberのまま保持する。float64変換だと 1e999 の
	// ような範囲外数値がDecodeエラーになり、JSON.parse (Infinity扱いで成功)
	// との差で poisoned 誤判定するため。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return true
	}
	return hasPoisonedKey(v)
}

// IsJSONContentType reports whether Fastify's content-type parser would
// route the request to the application/json parser. Fastify only validates
// the type/subtype token (lib/content-type.js) and tolerates malformed
// parameter lists, while Go's mime.ParseMediaType rejects them — e.g. a
// bare "charset" without value or duplicate parameters. On parse error we
// therefore fall back to comparing the raw essence before the first ';'.
func IsJSONContentType(ct string) bool {
	essence, _, err := mime.ParseMediaType(ct)
	if err == nil {
		return essence == echo.MIMEApplicationJSON
	}
	// パラメータ構文が壊れていてもfastifyはessenceでparserを選ぶため、
	// ';' 手前を自前で取り出して判定する (mime: duplicate parameter name
	// 等ではessenceすら返らない)。
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct)) == echo.MIMEApplicationJSON
}

// hasPoisonedKey walks the decoded JSON tree the same way secure-json-parse's
// filter() does, descending into both objects and arrays.
func hasPoisonedKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["__proto__"]; ok {
			return true
		}
		if cv, ok := x["constructor"]; ok {
			if cm, ok := cv.(map[string]any); ok {
				if _, ok := cm["prototype"]; ok {
					return true
				}
			}
		}
		for _, vv := range x {
			if hasPoisonedKey(vv) {
				return true
			}
		}
	case []any:
		for _, vv := range x {
			if hasPoisonedKey(vv) {
				return true
			}
		}
	}
	return false
}
