package meta

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 設定を読めなかったときの空の shape は、匿名の GET でも public にしない (#3593)。
func TestServerInfo_FallbackIsNotPublic(t *testing.T) {
	get := func(repo *testutil.MockMetaRepository) string {
		h := NewHandler(&config.Config{}, repo)
		e := echo.New()
		e.GET("/api/server-info", h.ServerInfo, middleware.APICacheControl())
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/server-info", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		return rec.Header().Get(echo.HeaderCacheControl)
	}
	ok := testutil.NewMockMetaRepository()
	ok.Meta = &model.Meta{}
	assert.Equal(t, "public, max-age=60", get(ok), "disabled stats is a real answer")
	assert.Empty(t, get(testutil.NewMockMetaRepository()), "Meta が nil = Fetch の失敗")
}
