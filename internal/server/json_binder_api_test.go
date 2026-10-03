package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/admin"
	"github.com/shiroha-a/mk/internal/api/apierr"
	apiauth "github.com/shiroha-a/mk/internal/api/auth"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
)

// TestDeserialize_DecoderSemanticsKept pins the parts of the old
// json.NewDecoder(...).Decode behavior the v2-based decoder must keep: only
// the first value is read, and a repeated exact key keeps the last value.
func TestDeserialize_DecoderSemanticsKept(t *testing.T) {
	var got exactTarget
	_, err := deserializeAt(t, "/x", `{"name":"a"} trailing`, &got)
	require.NoError(t, err)
	assert.Equal(t, "a", got.Name)

	got = exactTarget{}
	_, err = deserializeAt(t, "/api/x", `{"name":"a","name":"b"}`, &got)
	require.NoError(t, err)
	assert.Equal(t, "b", got.Name)
}

// TestDeserialize_ReadErrorPassesThrough pins that a body read error (e.g.
// BodyLimitByPath's 413 HTTPError on an oversized chunked body) is returned
// as is.
func TestDeserialize_ReadErrorPassesThrough(t *testing.T) {
	e := echo.New()
	readErr := echo.NewHTTPError(http.StatusRequestEntityTooLarge)
	req := httptest.NewRequest(http.MethodPost, "/", iotest.ErrReader(readErr))
	c := e.NewContext(req, httptest.NewRecorder())
	var got exactTarget
	assert.Same(t, readErr, fastJSONSerializer{}.Deserialize(c, &got))
}

// bindAt runs apiBinder.Bind for a request with the given method, route
// pattern, content type and body.
func bindAt(t *testing.T, method, path, contentType, body string, dst any) (echo.Context, error) {
	t.Helper()
	e := echo.New()
	e.JSONSerializer = fastJSONSerializer{}
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	c := e.NewContext(req, httptest.NewRecorder())
	c.SetPath(path)
	return c, (&apiBinder{}).Bind(dst, c)
}

// TestAPIBinder_NonObjectBody pins upstream's ajv `type: 'object'` failure
// for API requests whose body never becomes an object: no body at all
// (Fastify leaves request.body undefined) and text/plain (request.body is the
// raw string).
func TestAPIBinder_NonObjectBody(t *testing.T) {
	cases := []struct {
		name, method, ct, body string
	}{
		{"POST without body", http.MethodPost, "", ""},
		{"DELETE without body", http.MethodDelete, "", ""},
		{"empty text/plain", http.MethodPost, "text/plain", ""},
		{"text/plain body", http.MethodPost, "text/plain; charset=utf-8", `{"a":1}`},
		{"malformed text/plain params", http.MethodPost, "Text/Plain; charset", `x`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got exactTarget
			c, err := bindAt(t, tc.method, "/api/notes/create", tc.ct, tc.body, &got)
			assert.Same(t, errBodyNotObject, err)
			assert.Equal(t, true, c.Get(apierr.BodyNotObjectContextKey))
		})
	}
	t.Run("map target", func(t *testing.T) {
		var got map[string]any
		_, err := bindAt(t, http.MethodPost, "/api/admin/update-meta", "", "", &got)
		assert.Same(t, errBodyNotObject, err)
	})
}

// TestAPIBinder_OutOfScope pins the requests the binder leaves to
// echo.DefaultBinder unchanged.
// TestNonEndpointAPIRoutes_MatchUpstream pins the routes excluded from the
// object-body check to the POST / ALL routes upstream's ApiServerService.ts
// registers on fastify outside the endpoint machinery: /signup,
// /signin-flow, /signin-with-passkey, /signup-pending,
// /miauth/:session/check, /clear-browser-cache (/v1/instance/peers and /*
// are GET only). reset-db is an ordinary endpoint upstream
// (endpoints/reset-db.ts, paramDef `type: 'object'`), so a `null` body is
// 400 there too and its callers send `{}` (#3330).
func TestNonEndpointAPIRoutes_MatchUpstream(t *testing.T) {
	want := []string{
		"/api/signup",
		"/api/signin-flow",
		"/api/signin-with-passkey",
		"/api/signup-pending",
		"/api/miauth/:session/check",
		"/api/clear-browser-cache",
	}
	got := make([]string, 0, len(nonEndpointAPIRoutes))
	for p := range nonEndpointAPIRoutes {
		got = append(got, p)
	}
	assert.ElementsMatch(t, want, got)

	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
	c.SetPath("/api/reset-db")
	assert.True(t, isAPIEndpointRoute(c), "reset-db は本家でも endpoint")
}

func TestAPIBinder_OutOfScope(t *testing.T) {
	cases := []struct {
		name, method, path, ct, body string
		dst                          any
		wantErr                      bool
	}{
		{"GET reads the query", http.MethodGet, "/api/meta", "", "", &exactTarget{}, false},
		{"HEAD", http.MethodHead, "/api/meta", "", "", &exactTarget{}, false},
		{"non-API route", http.MethodPost, "/oauth/token", "", "", &exactTarget{}, false},
		{"non-endpoint API route", http.MethodPost, "/api/signin-flow", "", "", &exactTarget{}, false},
		{"slice target", http.MethodPost, "/api/x", "", "", &[]exactInner{}, false},
		{"empty form body", http.MethodPost, "/api/x", echo.MIMEApplicationForm, "", &exactTarget{}, false},
		{"empty multipart body", http.MethodPost, "/api/x", echo.MIMEMultipartForm, "", &exactTarget{}, false},
		{"JSON object body", http.MethodPost, "/api/x", echo.MIMEApplicationJSON, `{"name":"a"}`, &exactTarget{}, false},
		{"other media type keeps 415", http.MethodPost, "/api/x", "application/xml-patch", `x`, &exactTarget{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := bindAt(t, tc.method, tc.path, tc.ct, tc.body, tc.dst)
			if tc.wantErr {
				assert.ErrorIs(t, err, echo.ErrUnsupportedMediaType)
			} else {
				assert.NoError(t, err)
			}
			assert.Nil(t, c.Get(apierr.BodyNotObjectContextKey))
		})
	}
}

// newBodyTestEcho wires the request-body pipeline server.New installs (the
// serializer, the binder and the /api group's JSONBodyParse) in front of two
// real handlers that need no dependencies before their parameter checks.
func newBodyTestEcho() *echo.Echo {
	e := echo.New()
	e.JSONSerializer = fastJSONSerializer{}
	e.Binder = &apiBinder{}
	e.Use(echomw.Recover())
	api := e.Group("/api")
	api.Use(middleware.JSONBodyParse())
	api.POST("/admin/forward-abuse-user-report", (&admin.Handler{}).ForwardAbuseUserReport)
	api.POST("/auth/session/show", (&apiauth.Handler{}).SessionShow)
	return e
}

func serveBody(t *testing.T, e *echo.Echo, path, contentType, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec.Code, out
}

// TestAPIBody_EndToEnd runs real handlers through the server's body pipeline
// and pins upstream's answer for each body shape (#3330).
func TestAPIBody_EndToEnd(t *testing.T) {
	e := newBodyTestEcho()
	const forward = "/api/admin/forward-abuse-user-report"
	mustBeObject := map[string]any{"param": "#/type", "reason": "must be object"}

	cases := []struct {
		name      string
		path      string
		ct        string
		body      string
		wantCode  int
		wantError string
		wantInfo  map[string]any
	}{
		// 大文字小文字違いのキーは無いのと同じ (ajv の required 違反)。
		{"case variant key is missing", forward, echo.MIMEApplicationJSON, `{"reportid":"r1"}`, http.StatusBadRequest, "INVALID_PARAM",
			map[string]any{"param": "#/required", "reason": "must have required property 'reportId'"}},
		// 後ろの REPORTID: null は reportId を上書きしない。abuseRepo が無いので
		// 引数検査を抜けると NO_SUCH_ABUSE_REPORT になる。
		{"case variant does not override", forward, echo.MIMEApplicationJSON, `{"reportId":"r1","REPORTID":null}`, http.StatusNotFound, "NO_SUCH_ABUSE_REPORT", nil},
		{"null body", forward, echo.MIMEApplicationJSON, `null`, http.StatusBadRequest, "INVALID_PARAM", mustBeObject},
		{"array body", forward, echo.MIMEApplicationJSON, `[{"reportId":"r1"}]`, http.StatusBadRequest, "INVALID_PARAM", mustBeObject},
		{"string body", forward, echo.MIMEApplicationJSON, `"r1"`, http.StatusBadRequest, "INVALID_PARAM", mustBeObject},
		{"number body", forward, echo.MIMEApplicationJSON, `1`, http.StatusBadRequest, "INVALID_PARAM", mustBeObject},
		{"no body", forward, "", "", http.StatusBadRequest, "INVALID_PARAM", mustBeObject},
		{"text/plain body", forward, "text/plain", `{"reportId":"r1"}`, http.StatusBadRequest, "INVALID_PARAM", mustBeObject},
		// 空の application/json は Fastify の content-type parser が先に弾く。
		{"empty JSON body", forward, echo.MIMEApplicationJSON, "", http.StatusBadRequest, "", nil},
		// auth/session/show: 大文字の TOKEN では token を欠いた扱いになり、
		// repo に触る前に INVALID_PARAM で止まる。
		{"session/show case variant", "/api/auth/session/show", echo.MIMEApplicationJSON, `{"TOKEN":"t"}`, http.StatusBadRequest, "INVALID_PARAM", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := serveBody(t, e, tc.path, tc.ct, tc.body)
			assert.Equal(t, tc.wantCode, code, out)
			if tc.wantError == "" {
				assert.Equal(t, "FST_ERR_CTP_EMPTY_JSON_BODY", out["code"])
				return
			}
			errObj, ok := out["error"].(map[string]any)
			require.True(t, ok, out)
			assert.Equal(t, tc.wantError, errObj["code"])
			if tc.wantInfo != nil {
				assert.Equal(t, tc.wantInfo, errObj["info"])
				assert.Equal(t, apierr.UUIDInvalidParam, errObj["id"])
			}
		})
	}
}

// TestNew_InstallsRequestBodyPipeline pins that New wires the binder and the
// serializer the tests above exercise on their own echo instance; without
// it, removing the assignment in newServer would leave every test green.
func TestNew_InstallsRequestBodyPipeline(t *testing.T) {
	srv := newServerWithTrustProxy(t, nil)
	assert.IsType(t, &apiBinder{}, srv.echo.Binder)
	assert.IsType(t, fastJSONSerializer{}, srv.echo.JSONSerializer)
}

// TestRouter_NonObjectBodyOnEndpointWithoutParams pins, through the real
// router, that an endpoint whose handler never binds (endpoints, stats)
// still answers a non-object body with upstream's #/type INVALID_PARAM, and
// that a credential-required endpoint keeps answering CREDENTIAL_REQUIRED
// first, as upstream checks credentials before ajv (#3330).
func TestRouter_NonObjectBodyOnEndpointWithoutParams(t *testing.T) {
	srv := newServerWithTrustProxy(t, nil)
	mustBeObject := map[string]any{"param": "#/type", "reason": "must be object"}
	for _, path := range []string{"/api/endpoints", "/api/stats"} {
		for _, body := range []string{`null`, `[]`, `1`} {
			t.Run(path+" "+body, func(t *testing.T) {
				code, out := serveBody(t, srv.echo, path, echo.MIMEApplicationJSON, body)
				assert.Equal(t, http.StatusBadRequest, code, out)
				errObj, _ := out["error"].(map[string]any)
				assert.Equal(t, "INVALID_PARAM", errObj["code"])
				assert.Equal(t, mustBeObject, errObj["info"])
			})
		}
		t.Run(path+" object body", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			srv.echo.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
	// handler の中でも認証を確かめる endpoint (emoji-application/*、promo/read)
	// も、route の RequireAuth が先に応答する。
	for _, path := range []string{"/api/i", "/api/emoji-application/create", "/api/emoji-application/list-mine", "/api/emoji-application/cancel", "/api/promo/read"} {
		t.Run(path+" credential check comes first", func(t *testing.T) {
			code, out := serveBody(t, srv.echo, path, echo.MIMEApplicationJSON, `null`)
			assert.Equal(t, http.StatusUnauthorized, code, out)
			errObj, _ := out["error"].(map[string]any)
			assert.Equal(t, "CREDENTIAL_REQUIRED", errObj["code"])
		})
	}
	// i/revoke-token は本家でも requireCredential を持たず、認証を exec の中で
	// 確かめる (ajv の後)。object でない body は認証より先に 400 になる。
	t.Run("revoke-token validates before its in-handler credential check", func(t *testing.T) {
		code, out := serveBody(t, srv.echo, "/api/i/revoke-token", echo.MIMEApplicationJSON, `null`)
		assert.Equal(t, http.StatusBadRequest, code, out)
		errObj, _ := out["error"].(map[string]any)
		assert.Equal(t, mustBeObject, errObj["info"])
	})
}

// TestRequireObjectBody pins the body shapes the route middleware rejects
// and the requests it leaves to the handler.
func TestRequireObjectBody(t *testing.T) {
	cases := []struct {
		name, method, path, ct, body string
		reject                       bool
	}{
		{"JSON null", http.MethodPost, "/api/x", echo.MIMEApplicationJSON, `null`, true},
		{"JSON array", http.MethodPost, "/api/x", echo.MIMEApplicationJSON, ` [1]`, true},
		{"JSON string", http.MethodPost, "/api/x", "application/json; charset=utf-8", `"s"`, true},
		{"JSON false", http.MethodPost, "/api/x", echo.MIMEApplicationJSON, `false`, true},
		{"no body", http.MethodPost, "/api/x", "", ``, true},
		{"text/plain", http.MethodPost, "/api/x", "text/plain", `{"a":1}`, true},
		// 名前が chunked で始まる行は ContentLength を -1 (長さ不明) にする。
		{"chunked empty body", http.MethodPost, "/api/x", "", ``, true},
		{"chunked body without content type", http.MethodPost, "/api/x", "", `{"a":1}`, false},
		{"JSON object", http.MethodPost, "/api/x", echo.MIMEApplicationJSON, `{"a":1}`, false},
		{"multipart", http.MethodPost, "/api/x", echo.MIMEMultipartForm + "; boundary=b", "--b--", false},
		{"GET", http.MethodGet, "/api/x", "", ``, false},
		{"HEAD", http.MethodHead, "/api/x", "", ``, false},
		{"non-endpoint route", http.MethodPost, "/api/signup", echo.MIMEApplicationJSON, `null`, false},
		{"non-API route", http.MethodPost, "/inbox", echo.MIMEApplicationJSON, `null`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
			if strings.HasPrefix(tc.name, "chunked") {
				req.ContentLength = -1
			}
			if tc.ct != "" {
				req.Header.Set(echo.HeaderContentType, tc.ct)
			}
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetPath(tc.path)
			var seen string
			called := false
			err := requireObjectBody(func(c echo.Context) error {
				called = true
				b, _ := io.ReadAll(c.Request().Body)
				seen = string(b)
				return c.NoContent(http.StatusNoContent)
			})(c)
			require.NoError(t, err)
			assert.Equal(t, !tc.reject, called)
			if tc.reject {
				testutil.AssertInvalidParam(t, rec)
				return
			}
			// 読んだ body を handler にそのまま渡していること。
			assert.Equal(t, tc.body, seen)
		})
	}
}

// TestRequireObjectBody_ReadErrorPassesThrough pins that a body read error
// (e.g. the 413 of an oversized chunked body) is returned as is.
func TestRequireObjectBody_ReadErrorPassesThrough(t *testing.T) {
	e := echo.New()
	readErr := echo.NewHTTPError(http.StatusRequestEntityTooLarge)
	req := httptest.NewRequest(http.MethodPost, "/", iotest.ErrReader(readErr))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(req, httptest.NewRecorder())
	c.SetPath("/api/x")
	err := requireObjectBody(func(echo.Context) error { return nil })(c)
	assert.Same(t, readErr, err)
}

// TestAPIRoutes_AttachCheckAfterRouteMiddleware pins that apiRoutes.POST and
// Match run requireObjectBody after the route's own middleware (so a
// credential check still answers first) and before the handler.
func TestAPIRoutes_AttachCheckAfterRouteMiddleware(t *testing.T) {
	e := echo.New()
	api := apiRoutes{e.Group("/api")}
	deny := func(echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { return c.NoContent(http.StatusUnauthorized) }
	}
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	api.POST("/post", ok)
	api.POST("/denied", ok, deny)
	api.Match([]string{http.MethodGet, http.MethodPost}, "/match", ok)

	serve := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/post", `null`))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodPost, "/api/post", `{}`))
	assert.Equal(t, http.StatusUnauthorized, serve(http.MethodPost, "/api/denied", `null`))
	assert.Equal(t, http.StatusBadRequest, serve(http.MethodPost, "/api/match", `null`))
	assert.Equal(t, http.StatusNoContent, serve(http.MethodGet, "/api/match", ``))
}
