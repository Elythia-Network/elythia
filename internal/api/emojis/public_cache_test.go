package emojis_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/api/emojis"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 一覧を読めなかったときの空の一覧は、匿名の GET でも public にしない (#3593)。
func TestEmojis_FallbackIsNotPublic(t *testing.T) {
	get := func(repo repository.EmojiRepository) *httptest.ResponseRecorder {
		h := emojis.NewHandler(repo)
		e := echo.New()
		e.GET("/api/emojis", h.Emojis, middleware.APICacheControl())
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/emojis", nil))
		return rec
	}
	ok := get(testutil.NewMockEmojiRepository())
	assert.Equal(t, "public, max-age=3600", ok.Header().Get(echo.HeaderCacheControl))
	failed := get(&failingEmojiRepo{MockEmojiRepository: testutil.NewMockEmojiRepository()})
	assert.Equal(t, http.StatusOK, failed.Code)
	assert.JSONEq(t, `{"emojis":[]}`, failed.Body.String())
	assert.Empty(t, failed.Header().Get(echo.HeaderCacheControl))
}
