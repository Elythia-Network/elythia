package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/federation"
)

type castInner struct {
	Count uint `query:"count"`
}

type castTarget struct {
	Limit   *int     `query:"limit"`
	Offset  int64    `query:"offset"`
	Blocked *bool    `query:"blocked"`
	Ratio   *float64 `query:"ratio"`
	Host    string   `query:"host"`
	IDs     []int    `query:"ids"`
	Inner   castInner
}

// bindQuery runs apiBinder.Bind for a request with the given method, route
// pattern and raw query string.
func bindQuery(t *testing.T, method, path, rawQuery string, dst any) (echo.Context, error) {
	t.Helper()
	e := echo.New()
	e.JSONSerializer = fastJSONSerializer{}
	req := httptest.NewRequest(method, "/x?"+rawQuery, nil)
	c := e.NewContext(req, httptest.NewRecorder())
	c.SetPath(path)
	return c, (&apiBinder{}).Bind(dst, c)
}

// TestAPIBinder_QueryCast pins upstream's "Cast non JSON input" step for
// GET (ApiCallService.ts call()): a boolean / number / integer param whose
// single query value JSON.parse rejects is answered with 0b5f1631 before
// echo binds anything, and everything else is left to echo's binder.
func TestAPIBinder_QueryCast(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		path      string
		query     string
		wantParam string // "" = no cast failure
		wantType  string
		wantErr   bool // echo's own bind error (no cast mark)
	}{
		{name: "integer not JSON", query: "limit=abc", wantParam: "limit", wantType: "integer"},
		{name: "empty value", query: "limit=", wantParam: "limit", wantType: "integer"},
		{name: "int64", query: "offset=1x", wantParam: "offset", wantType: "integer"},
		{name: "boolean in Python case", query: "blocked=True", wantParam: "blocked", wantType: "boolean"},
		{name: "number", query: "ratio=one", wantParam: "ratio", wantType: "number"},
		{name: "untagged struct is descended", query: "count=x", wantParam: "count", wantType: "integer"},
		{name: "first field in declaration order", query: "blocked=x&limit=y", wantParam: "limit", wantType: "integer"},
		{name: "valid values", query: "limit=10&offset=-1&blocked=false&ratio=1.5&count=2&host=h"},
		{name: "string param is never cast", query: "host=not-json"},
		// 同じキーが 2 回来ると本家では配列になり、変換されない。
		{name: "repeated key is not cast", query: "limit=x&limit=1", wantErr: true},
		// 配列の param は本家でも変換しない。echo は要素を int に読めずに失敗する。
		{name: "array param is not cast", query: "ids=x", wantErr: true},
		// 本家のキーは完全一致。echo は大文字小文字を無視して拾うので echo が失敗する。
		{name: "case variant key is not cast", query: "LIMIT=abc", wantErr: true},
		// POST は query を変換しない (body が無いので #/type で落ちる)。
		{name: "POST does not read the query", method: http.MethodPost, query: "limit=abc", wantErr: true},
		{name: "non-API route", path: "/oauth/token", query: "limit=abc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method, path := tc.method, tc.path
			if method == "" {
				method = http.MethodGet
			}
			if path == "" {
				path = "/api/federation/instances"
			}
			var dst castTarget
			c, err := bindQuery(t, method, path, tc.query, &dst)
			if tc.wantParam == "" {
				if tc.wantErr {
					assert.Error(t, err)
					assert.NotSame(t, errQueryCast, err)
				} else {
					assert.NoError(t, err)
				}
				assertNoCastMark(t, c)
				return
			}
			assert.Same(t, errQueryCast, err)
			rec := httptest.NewRecorder()
			c.Response().Writer = rec
			require.NoError(t, apierr.JSONInvalidParam(c))
			var out map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			errObj := out["error"].(map[string]any)
			assert.Equal(t, apierr.UUIDInvalidParamCast, errObj["id"])
			assert.Equal(t, map[string]any{"param": tc.wantParam, "reason": "cannot cast to " + tc.wantType}, errObj["info"])
		})
	}
	t.Run("non-struct target", func(t *testing.T) {
		var dst map[string]string
		_, err := bindQuery(t, http.MethodGet, "/api/x", "limit=abc", &dst)
		assert.NoError(t, err)
	})
}

// assertNoCastMark checks that c answers JSONInvalidParam with the plain
// ajv id, i.e. the binder did not mark a cast failure.
func assertNoCastMark(t *testing.T, c echo.Context) {
	t.Helper()
	rec := httptest.NewRecorder()
	c.Response().Writer = rec
	require.NoError(t, apierr.JSONInvalidParam(c))
	assert.Contains(t, rec.Body.String(), apierr.UUIDInvalidParam)
}

// TestRouter_UploadWithoutFile pins, through the real router, upstream
// handleMultipartRequest's bare 400 for drive/files/create without a
// multipart file, answered before the token is looked at: an unknown token
// does not turn it into 401 AUTHENTICATION_FAILED, and a missing one not
// into CREDENTIAL_REQUIRED.
func TestRouter_UploadWithoutFile(t *testing.T) {
	srv := newServerWithTrustProxy(t, nil)
	cases := []struct {
		name, ct, body, auth string
	}{
		{"no body, no token", "", "", ""},
		{"JSON body with unknown token", echo.MIMEApplicationJSON, `{"i":"no-such-token"}`, ""},
		{"no body, unknown bearer token", "", "", "Bearer no-such-token"},
		{"text/plain", "text/plain", "x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/drive/files/create", strings.NewReader(tc.body))
			if tc.ct != "" {
				req.Header.Set(echo.HeaderContentType, tc.ct)
			}
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			srv.echo.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Empty(t, rec.Body.String())
			assert.Empty(t, rec.Header().Get(echo.HeaderContentType))
			assert.Equal(t, "private, max-age=0, must-revalidate", rec.Header().Get("Cache-Control"))
		})
	}
	// Fastify の JSON parser が先に弾く body は従来どおり FST_ERR_CTP_*。
	t.Run("malformed JSON keeps the Fastify envelope", func(t *testing.T) {
		code, out := serveBody(t, srv.echo, "/api/drive/files/create", echo.MIMEApplicationJSON, `{`)
		assert.Equal(t, http.StatusBadRequest, code)
		assert.Equal(t, "FST_ERR_CTP_INVALID_JSON_BODY", out["code"])
	})
	// file があれば認証へ進む (token が無いので CREDENTIAL_REQUIRED)。
	t.Run("with a file the credential check runs", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, err := w.CreateFormFile("file", "a.txt")
		require.NoError(t, err)
		_, err = fw.Write([]byte("x"))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		code, out := serveBody(t, srv.echo, "/api/drive/files/create", w.FormDataContentType(), buf.String())
		assert.Equal(t, http.StatusUnauthorized, code, out)
		assert.Equal(t, "CREDENTIAL_REQUIRED", out["error"].(map[string]any)["code"])
	})
}

// TestAPIQueryCast_EndToEnd sends GET requests through a real handler
// (federation/instances, allowGet upstream) with the server's binder and
// pins the whole error object upstream returns for an uncastable value.
func TestAPIQueryCast_EndToEnd(t *testing.T) {
	e := echo.New()
	e.JSONSerializer = fastJSONSerializer{}
	e.Binder = &apiBinder{}
	e.Use(echomw.Recover())
	e.GET("/api/federation/instances", (&federation.Handler{}).Instances)

	cases := []struct {
		query, param, typ string
	}{
		{"limit=abc", "limit", "integer"},
		{"blocked=yes", "blocked", "boolean"},
		{"sinceDate=today", "sinceDate", "integer"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/federation/instances?"+tc.query, nil))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			var out map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			assert.Equal(t, map[string]any{"error": map[string]any{
				"message": "Invalid param.",
				"code":    "INVALID_PARAM",
				"id":      "0b5f1631-7c1a-41a6-b399-cce335f34d85",
				"kind":    "client",
				"info":    map[string]any{"param": tc.param, "reason": "cannot cast to " + tc.typ},
			}}, out)
		})
	}
}
