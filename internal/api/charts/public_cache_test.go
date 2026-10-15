package charts

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// 取得に失敗して返す空の一覧は、匿名の GET でも public にしない (本家は
// エラーになりキャッシュされない、#3593)。
func TestRetention_FallbackIsNotPublic(t *testing.T) {
	get := func(repo *stubRetentionRepo) string {
		h := NewHandler(Charts{})
		h.SetRetentionRepo(repo)
		e := echo.New()
		e.GET("/api/retention", h.Retention, middleware.APICacheControl())
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/retention", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		return rec.Header().Get(echo.HeaderCacheControl)
	}
	assert.Equal(t, "public, max-age=3600", get(&stubRetentionRepo{rows: []*model.RetentionAggregation{}}))
	assert.Empty(t, get(&stubRetentionRepo{err: errors.New("boom")}))
}
