package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/elythia-network/elythia/internal/api/openapi"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOpenAPITestServer(t *testing.T) (*Server, *echo.Echo) {
	t.Helper()
	e := echo.New()
	noop := func(c echo.Context) error { return nil }
	e.POST("/api/notes/create", noop)
	e.POST("/api/notes/show", noop)
	e.POST("/api/i", noop)
	// GETを持つ本家互換のもの(本家はallowGet)。
	e.GET("/api/charts/notes", noop)
	e.POST("/api/charts/notes", noop)
	// Elythia独自のもの。
	e.POST("/api/bubble-game/versus/create", noop)
	// 本家がendpointの外で登録するもの、/api配下以外、catchallは載らない。
	e.POST("/api/signup", noop)
	// 本家の旧いpathの互換も載らない。
	e.POST("/api/signin", noop)
	e.POST("/api/miauth/:session/check", noop)
	e.GET("/healthz", noop)
	e.Any("/api/*", noop)

	s := &Server{echo: e, config: &config.Config{URL: "https://example.test/"}}
	return s, e
}

func getAPIJSON(t *testing.T, s *Server, e *echo.Echo) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api.json", nil), rec)
	require.NoError(t, s.OpenAPISpec(c))
	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	return rec, doc
}

func TestOpenAPISpec(t *testing.T) {
	s, e := newOpenAPITestServer(t)
	rec, doc := getAPIJSON(t, s, e)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=600", rec.Header().Get("Cache-Control"))
	assert.Equal(t, echo.MIMEApplicationJSON, rec.Header().Get(echo.HeaderContentType))
	// Accept-Encodingが無ければ圧縮しないが、Varyは付ける。
	assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding))
	assert.Equal(t, []string{echo.HeaderAcceptEncoding}, rec.Header().Values(echo.HeaderVary))
	assert.Equal(t, strconv.Itoa(rec.Body.Len()), rec.Header().Get(echo.HeaderContentLength))

	assert.Equal(t, "3.1.0", doc["openapi"])
	info := doc["info"].(map[string]any)
	assert.Equal(t, "Elythia API", info["title"])
	assert.Equal(t, config.MkGoVersion+" (Misskey "+config.MisskeyVersion+" 互換)", info["version"])
	assert.Equal(t, config.MkGoRepositoryURL, doc["externalDocs"].(map[string]any)["url"])

	// serversはconfig.urlから作る。末尾の/は重ねない。
	servers := doc["servers"].([]any)
	require.Len(t, servers, 1)
	assert.Equal(t, "https://example.test/api", servers[0].(map[string]any)["url"])

	paths := doc["paths"].(map[string]any)
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"/notes/create", "/notes/show", "/i", "/charts/notes", "/bubble-game/versus/create"}, keys)
	assert.ElementsMatch(t, []string{"get", "post"}, mapKeys(paths["/charts/notes"].(map[string]any)))
	assert.ElementsMatch(t, []string{"post"}, mapKeys(paths["/notes/show"].(map[string]any)))

	// 本家互換のものには引数と応答の型が付き、独自のものは別のタグに入る。
	create := paths["/notes/create"].(map[string]any)["post"].(map[string]any)
	assert.Contains(t, create, "requestBody")
	assert.Contains(t, create["responses"], "200")
	assert.Equal(t, []any{"notes"}, create["tags"])
	own := paths["/bubble-game/versus/create"].(map[string]any)["post"].(map[string]any)
	assert.Equal(t, []any{openapi.ElythiaTag}, own["tags"])
	assert.NotContains(t, own, "requestBody")
	assert.Contains(t, doc["components"].(map[string]any)["schemas"], "Note")
}

func TestOpenAPISpec_BuiltOnce(t *testing.T) {
	s, e := newOpenAPITestServer(t)
	_, first := getAPIJSON(t, s, e)
	// 組み立ての後にルートは増えないので、文書は最初に作ったものを返し続ける。
	e.POST("/api/notes/delete", func(c echo.Context) error { return nil })
	_, second := getAPIJSON(t, s, e)
	assert.Equal(t, first, second)
	assert.NotContains(t, second["paths"], "/notes/delete")
}

func TestOpenAPISpec_BuildError(t *testing.T) {
	s, e := newOpenAPITestServer(t)
	// 作るのに失敗した結果も覚えて、500を返す。
	s.apiSpec.once.Do(func() { s.apiSpec.err = assert.AnError })
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api.json", nil), rec)
	require.NoError(t, s.OpenAPISpec(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "INTERNAL_ERROR")
	assert.Empty(t, rec.Header().Get("Cache-Control"))
}

// /api.jsonはgzip済みの本文を1度だけ作って使い回し、全体のgzip middlewareは
// 二重に包まない。Accept-Encodingのq=0も尊重する。
func TestOpenAPISpec_PrecompressedBehindGlobalGzip(t *testing.T) {
	s, e := newOpenAPITestServer(t)
	e.Use(echomw.GzipWithConfig(gzipConfig()))
	e.GET("/api.json", s.OpenAPISpec)
	_, want := getAPIJSON(t, s, e)

	get := func(accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api.json", nil)
		if accept != "" {
			req.Header.Set(echo.HeaderAcceptEncoding, accept)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, accept)
		assert.Equal(t, []string{echo.HeaderAcceptEncoding}, rec.Header().Values(echo.HeaderVary), accept)
		assert.Equal(t, strconv.Itoa(rec.Body.Len()), rec.Header().Get(echo.HeaderContentLength), accept)
		return rec
	}
	for _, accept := range []string{"gzip, deflate, br", "*", "br;q=1, gzip;q=0.5"} {
		rec := get(accept)
		assert.Equal(t, "gzip", rec.Header().Get(echo.HeaderContentEncoding), accept)
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		require.NoError(t, err, accept)
		plain, err := io.ReadAll(zr)
		require.NoError(t, err, accept)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(plain, &doc), "1回gunzipすればJSONになる: %s", accept)
		assert.Equal(t, want, doc, accept)
	}
	for _, accept := range []string{"", "gzip;q=0", "br", "identity", "*;q=0"} {
		rec := get(accept)
		assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding), accept)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc), accept)
		assert.Equal(t, want, doc, accept)
	}

	// 圧縮した本文も1度だけ作り、同じものを返し続ける。
	first := s.apiSpec.spec.gzip
	get("gzip")
	assert.Same(t, &first[0], &s.apiSpec.spec.gzip[0])
}

// 組み立てがpanicしても、以後のリクエストをnilの参照でpanicさせず、500を返し続ける。
func TestAPISpecCache_BuildPanics(t *testing.T) {
	var a apiSpecCache
	calls := 0
	for range 2 {
		spec, err := a.get(func() (*encodedSpec, error) {
			calls++
			panic("boom")
		})
		assert.Nil(t, spec)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "boom")
	}
	assert.Equal(t, 1, calls)

	// 本文もエラーも返さない組み立ても、エラーとして覚える。
	var b apiSpecCache
	spec, err := b.get(func() (*encodedSpec, error) { return nil, nil })
	assert.Nil(t, spec)
	require.Error(t, err)

	// panicした後のハンドラは500を返す。
	s, e := newOpenAPITestServer(t)
	_, _ = s.apiSpec.get(func() (*encodedSpec, error) { panic("boom") })
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api.json", nil), rec)
	require.NoError(t, s.OpenAPISpec(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
