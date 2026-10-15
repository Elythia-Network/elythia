package emojis

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 同じ一覧のあいだは、符号化した本文を作り直さない (#3593)。
func TestEmojis_ReusesEncodedBodyForTheSameList(t *testing.T) {
	inner := testutil.NewMockEmojiRepository()
	require.NoError(t, inner.Create(&model.Emoji{ID: "a", Name: "a", PublicURL: "https://e.example/a.png", Aliases: []string{}}))
	repo := repository.NewCachedEmojiRepository(inner)
	h := NewHandler(repo)
	call := func() {
		rec := httptest.NewRecorder()
		require.NoError(t, h.Emojis(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/emojis", nil), rec)))
		require.Equal(t, http.StatusOK, rec.Code)
	}

	call()
	first := h.encoded.Load()
	require.NotNil(t, first)
	call()
	assert.Same(t, first, h.encoded.Load(), "same list must reuse the encoded body")

	repo.Invalidate()
	call()
	assert.NotSame(t, first, h.encoded.Load(), "a new list must be encoded again")
}

func TestSameList(t *testing.T) {
	a := []*model.Emoji{{ID: "1"}, {ID: "2"}}
	b := []*model.Emoji{a[0], a[1]}
	assert.True(t, sameList(a, a))
	assert.False(t, sameList(a, b), "equal contents in another array is a different list")
	assert.False(t, sameList(a, a[:1]))
	assert.True(t, sameList(nil, []*model.Emoji{}))
}
