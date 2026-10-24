package bubblegame

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// ランキングを読めなかったときの空の一覧は、匿名の GET でも public にしない
// (本家はエラーになりキャッシュされない、#3593)。
func TestRanking_FallbackIsNotPublic(t *testing.T) {
	get := func(repo repository.BubbleGameRepository) string {
		idGen, _ := id.NewGenerator("aidx")
		h := NewHandler(repo, idGen, nil)
		e := echo.New()
		e.GET("/api/bubble-game/ranking", h.Ranking, middleware.APICacheControl())
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bubble-game/ranking?gameMode=normal", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		return rec.Header().Get(echo.HeaderCacheControl)
	}
	assert.Equal(t, "public, max-age=60", get(&mockBubbleGameRepo{}))
	assert.Empty(t, get(&failRankingRepo{&mockBubbleGameRepo{}}))
}
