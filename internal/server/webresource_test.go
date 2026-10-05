package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWebResourceTestHandler(t *testing.T, meta *model.Meta) (*webResourceHandler, echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	repo := testutil.NewMockMetaRepository()
	repo.Meta = meta
	h := newWebResourceHandler(&config.Config{URL: "https://example.test"}, repo)

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	return h, c, rec
}

func TestRobotsTxt(t *testing.T) {
	h, c, rec := newWebResourceTestHandler(t, &model.Meta{ID: "x"})
	require.NoError(t, h.RobotsTxt(c))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get(echo.HeaderContentType))
	body := rec.Body.String()
	assert.True(t, strings.HasPrefix(body, "User-agent: *\n"))
	assert.Contains(t, body, "Disallow: /admin")
	assert.Contains(t, body, "Disallow: /api")
	assert.Contains(t, body, "Allow: /")
	// 既定 (ugcVisibilityForVisitor != none) では /@ と /notes は塞がない。
	assert.NotContains(t, body, "Disallow: /@")
	assert.NotContains(t, body, "Disallow: /notes")
}

// 未ログイン閲覧を塞いでいるインスタンスでは /@ と /notes もクロールさせない。
func TestRobotsTxt_UgcVisibilityNone(t *testing.T) {
	h, c, rec := newWebResourceTestHandler(t, &model.Meta{ID: "x", UgcVisibilityForVisitor: "none"})
	require.NoError(t, h.RobotsTxt(c))

	body := rec.Body.String()
	assert.Contains(t, body, "Disallow: /@")
	assert.Contains(t, body, "Disallow: /notes")
}

// robotsDisallowedPaths を破壊的に書き換えないこと (append の共有 backing array
// を踏むと 2 回目以降のリクエストに /@ が漏れる)。
func TestRobotsTxt_DoesNotMutateSharedSlice(t *testing.T) {
	h, c, _ := newWebResourceTestHandler(t, &model.Meta{ID: "x", UgcVisibilityForVisitor: "none"})
	require.NoError(t, h.RobotsTxt(c))

	h2, c2, rec2 := newWebResourceTestHandler(t, &model.Meta{ID: "x"})
	require.NoError(t, h2.RobotsTxt(c2))
	assert.NotContains(t, rec2.Body.String(), "Disallow: /@", "前回の呼び出しが共有 slice を汚していないこと")
}

func TestOpenSearchXML(t *testing.T) {
	name := "Test Instance"
	h, c, rec := newWebResourceTestHandler(t, &model.Meta{ID: "x", Name: &name})
	require.NoError(t, h.OpenSearchXML(c))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/opensearchdescription+xml", rec.Header().Get(echo.HeaderContentType))
	body := rec.Body.String()
	assert.Contains(t, body, `xmlns="http://a9.com/-/spec/opensearch/1.1/"`)
	assert.Contains(t, body, "<ShortName>Test Instance</ShortName>")
	assert.Contains(t, body, "<Description>Test Instance Search</Description>")
	assert.Contains(t, body, "https://example.test/favicon.ico")
	// {searchTerms} は OpenSearch のプレースホルダなので escape されないこと。
	assert.Contains(t, body, "https://example.test/search?q={searchTerms}")
}

// インスタンス名が未設定なら upstream 同様 "Misskey" にフォールバックする。
func TestOpenSearchXML_DefaultName(t *testing.T) {
	h, c, rec := newWebResourceTestHandler(t, &model.Meta{ID: "x"})
	require.NoError(t, h.OpenSearchXML(c))
	assert.Contains(t, rec.Body.String(), "<ShortName>Misskey</ShortName>")
}
