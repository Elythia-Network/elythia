package users

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/server/middleware"
)

// 数えられなかった 0 は、匿名の GET でも public にしない (#3593)。
func TestOnlineCount_FallbackIsNotPublic(t *testing.T) {
	get := func(err error) string {
		h, repo := newTestHandler(t)
		h.SetUserRepo(&countingOnlineRepo{MockUserRepository: repo, n: 3, err: err})
		e := echo.New()
		e.GET("/api/get-online-users-count", h.OnlineCount, middleware.APICacheControl())
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/get-online-users-count", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		return rec.Header().Get(echo.HeaderCacheControl)
	}
	assert.Equal(t, "public, max-age=60", get(nil))
	assert.Empty(t, get(errors.New("boom")))
}
