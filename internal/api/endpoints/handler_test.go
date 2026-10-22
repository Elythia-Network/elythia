package endpoints

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func call(t *testing.T, h echo.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h(e.NewContext(req, rec)))
	return rec
}

func TestEndpoints(t *testing.T) {
	t.Run("returns the catalog", func(t *testing.T) {
		h := NewHandler(func() []string { return []string{"notes/create", "meta"} })
		rec := call(t, h.Endpoints, `{}`)

		assert.Equal(t, http.StatusOK, rec.Code)
		var got []string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, []string{"notes/create", "meta"}, got)
	})

	t.Run("unwired lister still returns an array", func(t *testing.T) {
		// upstream は必ず配列を返す。null になるとクライアント側の
		// `.includes()` が落ちる。
		rec := call(t, NewHandler(nil).Endpoints, `{}`)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})
}

func TestEndpoint(t *testing.T) {
	h := NewHandler(func() []string { return []string{"notes/create"} })

	t.Run("known endpoint returns a params array", func(t *testing.T) {
		rec := call(t, h.Endpoint, `{"endpoint":"notes/create"}`)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"params":[]}`, rec.Body.String())
	})

	t.Run("unknown endpoint returns null", func(t *testing.T) {
		// upstream `endpoint.ts` は `ep == null` でそのまま null を返す。
		// 404 にすると「存在しない」と「引数が変」が区別できなくなる。
		rec := call(t, h.Endpoint, `{"endpoint":"nope/nope"}`)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "null", strings.TrimSpace(rec.Body.String()))
	})

	t.Run("empty endpoint returns null", func(t *testing.T) {
		// 本家の paramDef は空文字を通し、未知の endpoint として null を返す (#3774)。
		rec := call(t, h.Endpoint, `{"endpoint":""}`)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "null", strings.TrimSpace(rec.Body.String()))
	})

	t.Run("missing endpoint param is a 400", func(t *testing.T) {
		rec := call(t, h.Endpoint, `{}`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	})

	t.Run("malformed body is a 400", func(t *testing.T) {
		rec := call(t, h.Endpoint, `{`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("unwired lister reports everything as unknown", func(t *testing.T) {
		rec := call(t, NewHandler(nil).Endpoint, `{"endpoint":"notes/create"}`)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "null", strings.TrimSpace(rec.Body.String()))
	})
}
