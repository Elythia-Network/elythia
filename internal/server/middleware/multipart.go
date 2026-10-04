package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// multipartMaxMemory is the in-memory budget for parsing a multipart body,
// the same value net/http's FormFile / FormValue (and so echo's c.FormFile
// and auth's PostFormValue) use, so the form parsed here is the one they
// reuse instead of parsing the body a second time.
const multipartMaxMemory = 32 << 20

// RequireMultipartFile answers, for a POST to one of routes (route patterns
// as c.Path() reports them), a request that carries no multipart file with
// upstream's bare 400: status 400, no body, no Content-Type.
//
// Upstream registers its requireFile endpoints (drive/files/create) on a
// separate path that starts with `await request.file()`
// (ApiCallService.ts handleMultipartRequest) and replies `reply.code(400);
// reply.send()` when that throws (not multipart/form-data, an unreadable
// multipart body) or yields nothing (no file part). That runs before the
// token is read, so the answer is the same with a missing, invalid or valid
// token. Install it as a global middleware ahead of the auth middleware.
//
// A body Fastify's JSON parser rejects (empty, not valid JSON, prototype
// poisoning) is passed on so JSONBodyParse answers it with Fastify's
// FST_ERR_CTP_* envelope: Fastify parses the body before the handler runs.
// A body-limit error from reading the body (BodyLimitByPath's
// *echo.HTTPError) is returned as is.
//
// The response carries upstream's `Cache-Control: private, max-age=0,
// must-revalidate` (ApiServerService's onRequest hook), which the /api
// group would otherwise add after this middleware has answered.
func RequireMultipartFile(routes ...string) echo.MiddlewareFunc {
	want := make(map[string]bool, len(routes))
	for _, r := range routes {
		want[r] = true
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			if req.Method != http.MethodPost || !want[c.Path()] {
				return next(c)
			}
			ct := req.Header.Get(echo.HeaderContentType)
			switch {
			case isMultipartFormData(ct):
				err := req.ParseMultipartForm(multipartMaxMemory)
				var he *echo.HTTPError
				if errors.As(err, &he) {
					return he
				}
				if err == nil && hasMultipartFile(req) {
					return next(c)
				}
			case IsJSONContentType(ct):
				body, err := io.ReadAll(req.Body)
				if err != nil {
					var he *echo.HTTPError
					if errors.As(err, &he) {
						return he
					}
					// 読み切れなかった body は JSONBodyParse と同じく
					// FST_ERR_CTP_INVALID_JSON_BODY で返す (読んだ分は戻せない)。
					return c.JSON(http.StatusBadRequest, errInvalidJSONBody)
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				if jsonParserRejects(body) {
					return next(c)
				}
			}
			c.Response().Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
			return c.NoContent(http.StatusBadRequest)
		}
	}
}

// isMultipartFormData reports whether @fastify/multipart takes the request
// (its content type parser is registered for multipart/form-data only).
func isMultipartFormData(ct string) bool {
	essence, _, err := mime.ParseMediaType(ct)
	if err != nil {
		essence, _, _ = strings.Cut(ct, ";")
		essence = strings.ToLower(strings.TrimSpace(essence))
	}
	return essence == echo.MIMEMultipartForm
}

// hasMultipartFile reports whether the parsed multipart form holds a file
// part under any field name (upstream's request.file() returns the first
// file part whatever its field is called). req.ParseMultipartForm must have
// succeeded.
func hasMultipartFile(req *http.Request) bool {
	for _, fhs := range req.MultipartForm.File {
		if len(fhs) > 0 {
			return true
		}
	}
	return false
}

// jsonParserRejects reports whether Fastify's application/json parser
// rejects body (the cases JSONBodyParse answers with FST_ERR_CTP_*).
func jsonParserRejects(body []byte) bool {
	body = bytes.TrimPrefix(body, utf8BOM)
	return !json.Valid(body) || isPoisonedJSON(body)
}
