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

// ランキングを読めなかったとき (500) と、NUL を含む gameMode の空の一覧は、匿名の
// GET でも public にしない (本家はどちらもエラーになりキャッシュされない、#3593・#3369)。
func TestRanking_FallbackIsNotPublic(t *testing.T) {
	get := func(repo repository.BubbleGameRepository, query string, wantStatus int) string {
		idGen, _ := id.NewGenerator("aidx")
		h := NewHandler(repo, idGen, nil)
		e := echo.New()
		e.GET("/api/bubble-game/ranking", h.Ranking, middleware.APICacheControl())
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bubble-game/ranking?"+query, nil))
		assert.Equal(t, wantStatus, rec.Code)
		return rec.Header().Get(echo.HeaderCacheControl)
	}
	assert.Equal(t, "public, max-age=60", get(&mockBubbleGameRepo{}, "gameMode=normal", http.StatusOK))
	assert.Empty(t, get(&failRankingRepo{&mockBubbleGameRepo{}}, "gameMode=normal", http.StatusInternalServerError))
	assert.Empty(t, get(&mockBubbleGameRepo{}, "gameMode=a%00b", http.StatusOK))
}
