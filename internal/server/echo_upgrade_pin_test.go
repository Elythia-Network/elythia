package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinnedExchange is one request to the whole server and the parts of its
// answer that must not change when echo is upgraded (#4236).
type pinnedExchange struct {
	name   string
	method string
	path   string
	header map[string]string
	body   string
	// chunked sends the body without a Content-Length, so that a body limit
	// is hit while the body is read rather than up front.
	chunked bool

	status int
	// wantBody is the exact body, compared as JSON when it starts with "{".
	// anyBody skips the body.
	wantBody string
	anyBody  bool
	// wantHeader pins these headers to exactly these values. An empty value
	// means the header must be absent.
	wantHeader map[string]string
}

// pinnedServer builds the whole server the pinned exchanges are sent to.
func pinnedServer(t *testing.T) *Server {
	t.Helper()
	return newServerWithConfig(t, func(c *config.Config) { c.MaxFileSize = 4 << 20 })
}

// assertPinned sends each exchange to srv and checks the pinned parts of
// the answer.
func assertPinned(t *testing.T, srv *Server, exchanges []pinnedExchange) {
	t.Helper()
	for _, x := range exchanges {
		t.Run(x.name, func(t *testing.T) {
			req := httptest.NewRequest(x.method, x.path, strings.NewReader(x.body))
			if x.chunked {
				req.ContentLength = -1
			}
			for k, v := range x.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			srv.echo.ServeHTTP(rec, req)

			assert.Equal(t, x.status, rec.Code, rec.Body.String())
			switch {
			case x.anyBody:
			case strings.HasPrefix(x.wantBody, "{"):
				assert.JSONEq(t, x.wantBody, rec.Body.String())
			default:
				assert.Equal(t, x.wantBody, rec.Body.String())
			}
			for k, v := range x.wantHeader {
				if v == "" {
					assert.Empty(t, rec.Header().Values(k), k)
					continue
				}
				assert.Equal(t, v, strings.Join(rec.Header().Values(k), ", "), k)
			}
		})
	}
}

const (
	pinnedJSON             = "application/json; charset=utf-8"
	pinnedTooLarge         = `{"message":"Request Entity Too Large"}`
	pinnedNotFound         = `{"message":"Not Found"}`
	pinnedMethodNotAllowed = `{"message":"Method Not Allowed"}`
)

// TestEchoPin_BodyLimitAnswers413 pins the 413 of a body over the limit,
// whether the limit is hit up front (Content-Length) or while the body is
// read (chunked).
//
// 読み込み中に超えたときの 413 は、本文を読む側が echo のエラーを潰さずに
// 返して出る (apierr.HTTPStatusError)。echo の v5 ではこのエラーが
// *echo.HTTPError でなくなるので、判定を直さずに上げると 400 に化ける。
// JSON の本文と multipart の既存のテストは、413 のエラーを自分で作って流し
// 込んでいて、サーバー全体での答えを見ていない。
func TestEchoPin_BodyLimitAnswers413(t *testing.T) {
	srv := pinnedServer(t)
	note := `{"text":"` + strings.Repeat("a", 2<<20) + `"}`
	upload := "--XX\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.bin\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n" + strings.Repeat("b", 5<<20) + "\r\n--XX--\r\n"
	activity := `{"type":"` + strings.Repeat("a", 100<<10) + `"}`
	jsonCT := map[string]string{"Content-Type": "application/json"}
	multipartCT := map[string]string{"Content-Type": "multipart/form-data; boundary=XX"}
	want := map[string]string{"Content-Type": pinnedJSON}
	assertPinned(t, srv, []pinnedExchange{
		{name: "api json content-length", method: http.MethodPost, path: "/api/notes/create", header: jsonCT, body: note,
			status: http.StatusRequestEntityTooLarge, wantBody: pinnedTooLarge, wantHeader: want},
		{name: "api json chunked", method: http.MethodPost, path: "/api/notes/create", header: jsonCT, body: note, chunked: true,
			status: http.StatusRequestEntityTooLarge, wantBody: pinnedTooLarge, wantHeader: want},
		{name: "upload content-length", method: http.MethodPost, path: "/api/drive/files/create", header: multipartCT, body: upload,
			status: http.StatusRequestEntityTooLarge, wantBody: pinnedTooLarge, wantHeader: want},
		{name: "upload chunked", method: http.MethodPost, path: "/api/drive/files/create", header: multipartCT, body: upload, chunked: true,
			status: http.StatusRequestEntityTooLarge, wantBody: pinnedTooLarge, wantHeader: want},
		// テストのサーバーは連合が none なので、inbox に chunked で送ると本文を読む前に
		// 403 になる。読み込み中の 413 は TestInbox_BodyLimit が本物の BodyLimit で見ている。
		{name: "inbox content-length", method: http.MethodPost, path: "/inbox",
			header: map[string]string{"Content-Type": "application/activity+json"}, body: activity,
			status: http.StatusRequestEntityTooLarge, wantBody: pinnedTooLarge, wantHeader: want},
	})
}

// TestEchoPin_ErrorAnswers pins the Content-Type of JSON answers and the
// error answers that echo writes rather than a handler: an unknown path, a
// method the path does not take, and the errors handlers return with
// echo.NewHTTPError.
//
// Content-Type は Response().Before で charset を付けて決めている。本家の e2e は
// 完全一致で見るので、echo の v5 で c.Response() の形が変わっても同じ値で
// なければならない。
func TestEchoPin_ErrorAnswers(t *testing.T) {
	srv := pinnedServer(t)
	jsonCT := map[string]string{"Content-Type": "application/json"}
	assertPinned(t, srv, []pinnedExchange{
		{name: "api answer", method: http.MethodPost, path: "/api/meta", header: jsonCT, body: "{}",
			status: http.StatusOK, anyBody: true, wantHeader: map[string]string{"Content-Type": pinnedJSON}},
		{name: "api credential required", method: http.MethodPost, path: "/api/i", header: jsonCT, body: "{}",
			status:     http.StatusUnauthorized,
			wantBody:   `{"error":{"code":"CREDENTIAL_REQUIRED","id":"1384574d-a912-4b81-8601-c7b1c4085df1","kind":"client","message":"Authentication is required."}}`,
			wantHeader: map[string]string{"Content-Type": pinnedJSON, "WWW-Authenticate": `Bearer realm="Misskey"`}},
		{name: "api invalid token", method: http.MethodPost, path: "/api/notes/create",
			header: map[string]string{"Content-Type": "application/json", "Authorization": "Bearer bogus"}, body: `{"text":"x"}`,
			status:   http.StatusUnauthorized,
			wantBody: `{"error":{"code":"AUTHENTICATION_FAILED","id":"b0a7f5f8-dc2f-4171-b91f-de88ad238e14","kind":"client","message":"Authentication failed. Please ensure your token is correct."}}`,
			wantHeader: map[string]string{
				"Content-Type":     pinnedJSON,
				"WWW-Authenticate": `Bearer realm="Misskey", error="invalid_token", error_description="Authentication failed. Please ensure your token is correct."`,
			}},
		// ブラウザは必ず gzip を受け付ける。gzip は本文を溜めてから書くので、
		// WWW-Authenticate を決める middleware がその外側で本文を見ないと、ヘッダが
		// 付かなくなる (#4239)。
		{name: "api invalid token with gzip", method: http.MethodPost, path: "/api/notes/create",
			header: map[string]string{"Content-Type": "application/json", "Authorization": "Bearer bogus", "Accept-Encoding": "gzip"}, body: `{"text":"x"}`,
			status:   http.StatusUnauthorized,
			wantBody: `{"error":{"code":"AUTHENTICATION_FAILED","id":"b0a7f5f8-dc2f-4171-b91f-de88ad238e14","kind":"client","message":"Authentication failed. Please ensure your token is correct."}}`,
			wantHeader: map[string]string{
				"Content-Type":     pinnedJSON,
				"Content-Encoding": "",
				"WWW-Authenticate": `Bearer realm="Misskey", error="invalid_token", error_description="Authentication failed. Please ensure your token is correct."`,
			}},
		{name: "api credential required with gzip", method: http.MethodPost, path: "/api/i",
			header: map[string]string{"Content-Type": "application/json", "Accept-Encoding": "gzip"}, body: "{}",
			status:     http.StatusUnauthorized,
			wantBody:   `{"error":{"code":"CREDENTIAL_REQUIRED","id":"1384574d-a912-4b81-8601-c7b1c4085df1","kind":"client","message":"Authentication is required."}}`,
			wantHeader: map[string]string{"Content-Type": pinnedJSON, "WWW-Authenticate": `Bearer realm="Misskey"`}},
		{name: "api unknown endpoint by GET", method: http.MethodGet, path: "/api/no-such",
			status:     http.StatusNotFound,
			wantBody:   `{"error":{"code":"UNKNOWN_API_ENDPOINT","id":"2ca3b769-540a-4f08-9dd5-b5a825b6d0f1","kind":"client","message":"Unknown API endpoint."}}`,
			wantHeader: map[string]string{"Content-Type": pinnedJSON}},
		{name: "api unknown endpoint by POST", method: http.MethodPost, path: "/api/no-such", header: jsonCT, body: "{}",
			status: http.StatusOK, wantBody: `{}`, wantHeader: map[string]string{"Content-Type": pinnedJSON}},
		{name: "method not allowed", method: http.MethodPost, path: "/no-such-page",
			status: http.StatusMethodNotAllowed, wantBody: pinnedMethodNotAllowed,
			wantHeader: map[string]string{"Content-Type": pinnedJSON, "Allow": "OPTIONS, GET"}},
		{name: "feed of an unknown user", method: http.MethodGet, path: "/@nobody.rss",
			status: http.StatusNotFound, wantBody: pinnedNotFound, wantHeader: map[string]string{"Content-Type": pinnedJSON}},
		{name: "unknown twemoji badge", method: http.MethodGet, path: "/twemoji-badge/zzzz.png",
			status: http.StatusNotFound, wantBody: pinnedNotFound, wantHeader: map[string]string{"Content-Type": pinnedJSON}},
	})
}

// TestEchoPin_TrailingJSON pins the answer to a JSON body followed by more
// bytes. echo の v5 の既定の JSONSerializer は後ろに続くものを 400 にする
// (Elythia は fastJSONSerializer に差し替えている)。/api では JSONBodyParse が
// それより先に本家と同じ答えを返す。
func TestEchoPin_TrailingJSON(t *testing.T) {
	srv := pinnedServer(t)
	invalidJSON := `{"statusCode":400,"code":"FST_ERR_CTP_INVALID_JSON_BODY","error":"Bad Request","message":"Body is not valid JSON but content-type is set to 'application/json'"}`
	assertPinned(t, srv, []pinnedExchange{
		{name: "garbage", method: http.MethodPost, path: "/api/endpoints",
			header: map[string]string{"Content-Type": "application/json"}, body: `{"a":1}xyz`,
			status: http.StatusBadRequest, wantBody: invalidJSON},
		{name: "second value", method: http.MethodPost, path: "/api/endpoints",
			header: map[string]string{"Content-Type": "application/json"}, body: `{"a":1}{"b":2}`,
			status: http.StatusBadRequest, wantBody: invalidJSON},
		// essence の違う JSON の型は、v4 の binder なら前方一致で JSON として読むが、
		// その前に本家と同じ 415 で断っている。
		{name: "json-seq", method: http.MethodPost, path: "/api/endpoints",
			header: map[string]string{"Content-Type": "application/json-seq"}, body: `{"a":1}xyz`,
			status:   http.StatusUnsupportedMediaType,
			wantBody: `{"code":"FST_ERR_CTP_INVALID_MEDIA_TYPE","error":"Unsupported Media Type","message":"Unsupported Media Type","statusCode":415}`},
	})
}

// TestEchoPin_PanicAnswers500AndIsLogged pins that a handler that panics is
// answered with a 500 and its panic is logged with the stack.
//
// echo の v5 の Recover は、ログを書かずにエラーを返すだけになる。ログは
// recoverMiddleware が slog に書くので、v5 へ上げた後もここで確かめられる。
func TestEchoPin_PanicAnswers500AndIsLogged(t *testing.T) {
	srv := pinnedServer(t)
	srv.echo.GET("/__pin-panic", func(*echo.Context) error { panic("pinned panic") })
	logs := captureWarnLogs(t)

	assertPinned(t, srv, []pinnedExchange{
		{name: "panic", method: http.MethodGet, path: "/__pin-panic",
			status: http.StatusInternalServerError, wantBody: `{"message":"Internal Server Error"}`,
			wantHeader: map[string]string{"Content-Type": pinnedJSON}},
	})
	logged := logs.String()
	require.Contains(t, logged, "panic recovered in a handler")
	assert.Contains(t, logged, "route=/__pin-panic")
	assert.Contains(t, logged, "pinned panic")
	assert.Contains(t, logged, "TestEchoPin_PanicAnswers500AndIsLogged.func", "the stack must be logged")
}

// TestEchoPin_AnyRoutesAndUnknownMethods pins the answers of the routes
// registered with Any, and of other routes, to methods outside the usual
// ones.
//
// v4 の Any は決まった 11 個のメソッドを登録する。v5 は全メソッドに合う 1 本の
// ルートになるので、MKCOL のような表に無いメソッドも handler に届くようになる。
func TestEchoPin_AnyRoutesAndUnknownMethods(t *testing.T) {
	srv := pinnedServer(t)
	oauthUnknown := `{"error":{"code":"UNKNOWN_OAUTH_ENDPOINT","id":"aa49e620-26cb-4e28-aad6-8cbcb58db147","kind":"client","message":"Unknown OAuth endpoint."}}`
	assertPinned(t, srv, []pinnedExchange{
		{name: "PROPFIND api catch-all", method: "PROPFIND", path: "/api/no-such",
			status: http.StatusOK, wantBody: `{}`},
		{name: "MKCOL api catch-all", method: "MKCOL", path: "/api/no-such",
			status: http.StatusNotFound, wantBody: pinnedNotFound},
		{name: "PROPFIND clear-browser-cache", method: "PROPFIND", path: "/api/clear-browser-cache",
			status: http.StatusMethodNotAllowed, wantBody: ""},
		{name: "MKCOL clear-browser-cache", method: "MKCOL", path: "/api/clear-browser-cache",
			status: http.StatusNotFound, wantBody: pinnedNotFound},
		{name: "PROPFIND oauth catch-all", method: "PROPFIND", path: "/oauth/x",
			status: http.StatusNotFound, wantBody: oauthUnknown},
		{name: "MKCOL oauth catch-all", method: "MKCOL", path: "/oauth/x",
			status: http.StatusMethodNotAllowed, wantBody: pinnedMethodNotAllowed,
			wantHeader: map[string]string{"Allow": "OPTIONS, CONNECT, DELETE, GET, HEAD, PATCH, POST, PROPFIND, PUT, TRACE, REPORT"}},
		{name: "MKCOL page", method: "MKCOL", path: "/",
			status: http.StatusMethodNotAllowed, wantBody: pinnedMethodNotAllowed,
			wantHeader: map[string]string{"Allow": "OPTIONS, GET"}},
	})
}

// TestEchoPin_CORS pins the CORS answers, preflights with and without
// Origin and Access-Control-Request-Method included.
//
// 今は OPTIONS なら Origin や Access-Control-Request-Method が無くても、全体の
// CORS が 204 で打ち切る。echo の v5 は両方があるときだけを preflight として
// 扱い、それ以外の OPTIONS は後ろのルートへ流す。
func TestEchoPin_CORS(t *testing.T) {
	srv := pinnedServer(t)
	preflight := map[string]string{"Origin": "https://o.example", "Access-Control-Request-Method": "POST"}
	originOnly := map[string]string{"Origin": "https://o.example"}
	allowed := map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET,POST,PUT,DELETE,OPTIONS",
		"Access-Control-Allow-Headers": "Origin,Content-Type,Accept,Authorization",
		"Vary":                         "Origin, Access-Control-Request-Method, Access-Control-Request-Headers",
	}
	none := map[string]string{
		"Access-Control-Allow-Origin":  "",
		"Access-Control-Allow-Methods": "",
		"Vary":                         "Origin",
	}
	assertPinned(t, srv, []pinnedExchange{
		{name: "api preflight", method: http.MethodOptions, path: "/api/notes/create", header: preflight,
			status: http.StatusNoContent, wantBody: "", wantHeader: allowed},
		{name: "api OPTIONS with Origin only", method: http.MethodOptions, path: "/api/notes/create", header: originOnly,
			status: http.StatusNoContent, wantBody: "", wantHeader: allowed},
		{name: "api OPTIONS without CORS headers", method: http.MethodOptions, path: "/api/notes/create",
			status: http.StatusNoContent, wantBody: "", wantHeader: none},
		{name: "oauth token preflight", method: http.MethodOptions, path: "/oauth/token", header: preflight,
			status: http.StatusNoContent, wantBody: "", wantHeader: allowed},
		{name: "oauth token OPTIONS without CORS headers", method: http.MethodOptions, path: "/oauth/token",
			status: http.StatusNoContent, wantBody: "", wantHeader: none},
		{name: "page OPTIONS without CORS headers", method: http.MethodOptions, path: "/",
			status: http.StatusNoContent, wantBody: "", wantHeader: map[string]string{"Allow": "OPTIONS, GET", "Vary": "Origin"}},
		{name: "oauth token without Origin", method: http.MethodPost, path: "/oauth/token",
			header:     map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			status:     http.StatusBadRequest,
			wantBody:   `{"error":"unsupported_grant_type","error_description":"only authorization_code is supported"}`,
			wantHeader: map[string]string{"Access-Control-Allow-Origin": "*"}},
		{name: "api with Origin", method: http.MethodPost, path: "/api/meta",
			header: map[string]string{"Content-Type": "application/json", "Origin": "https://o.example"}, body: "{}",
			status: http.StatusOK, anyBody: true, wantHeader: map[string]string{"Access-Control-Allow-Origin": "*"}},
	})
}
