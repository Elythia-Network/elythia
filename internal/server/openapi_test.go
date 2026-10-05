package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOpenAPITestServer(t *testing.T) (*Server, *httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	e := echo.New()
	noop := func(c echo.Context) error { return nil }
	e.POST("/api/notes/create", noop)
	e.POST("/api/notes/show", noop)
	e.GET("/api/notes/show", noop)
	e.POST("/api/i", noop)
	// /api 配下以外と catchall は載らないこと。
	e.GET("/healthz", noop)
	e.Any("/api/*", noop)

	s := &Server{echo: e, config: &config.Config{URL: "https://example.test"}}
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api.json", nil), rec)
	return s, rec, c
}

func TestOpenAPISpec(t *testing.T) {
	s, rec, c := newOpenAPITestServer(t)
	require.NoError(t, s.OpenAPISpec(c))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=600", rec.Header().Get("Cache-Control"))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	assert.Equal(t, "3.1.0", doc["openapi"])
	info := doc["info"].(map[string]any)
	assert.Equal(t, "Misskey API", info["title"])
	assert.NotEmpty(t, info["version"])

	servers := doc["servers"].([]any)
	require.Len(t, servers, 1)
	assert.Equal(t, "https://example.test/api", servers[0].(map[string]any)["url"])

	paths := doc["paths"].(map[string]any)
	assert.Contains(t, paths, "notes/create")
	assert.Contains(t, paths, "i")
	// /api 配下でないものと catchall は載らない。
	assert.NotContains(t, paths, "healthz")
	for k := range paths {
		assert.NotContains(t, k, "*")
	}
}

// 同じ path に複数メソッドが登録されていれば両方載る。
func TestOpenAPISpec_MultipleMethods(t *testing.T) {
	s, rec, c := newOpenAPITestServer(t)
	require.NoError(t, s.OpenAPISpec(c))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	show := doc["paths"].(map[string]any)["notes/show"].(map[string]any)
	assert.Contains(t, show, "post")
	assert.Contains(t, show, "get")
}

// endpoint は先頭セグメントでタグ付けされる。
func TestOpenAPISpec_Tags(t *testing.T) {
	s, rec, c := newOpenAPITestServer(t)
	require.NoError(t, s.OpenAPISpec(c))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	op := doc["paths"].(map[string]any)["notes/create"].(map[string]any)["post"].(map[string]any)
	assert.Equal(t, []any{"notes"}, op["tags"])
	assert.Equal(t, "notes/create", op["operationId"])
}

func TestTopLevelTag(t *testing.T) {
	assert.Equal(t, "notes", topLevelTag("notes/create"))
	assert.Equal(t, "i", topLevelTag("i"))
	assert.Equal(t, "admin", topLevelTag("admin/roles/create"))
}
