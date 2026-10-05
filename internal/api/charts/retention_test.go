package charts

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// stubRetentionRepo serves a fixed result for ListRecent and panics on the
// rest — those methods belong to the aggregation job, not this endpoint.
type stubRetentionRepo struct {
	repository.RetentionAggregationRepository
	rows []*model.RetentionAggregation
	err  error
	// limit records what the handler asked for.
	limit int
}

func (r *stubRetentionRepo) ListRecent(limit int) ([]*model.RetentionAggregation, error) {
	r.limit = limit
	return r.rows, r.err
}

func callRetention(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Retention(e.NewContext(req, rec)))
	return rec
}

func TestRetention(t *testing.T) {
	t.Run("returns the aggregated cohorts", func(t *testing.T) {
		at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		repo := &stubRetentionRepo{rows: []*model.RetentionAggregation{
			{CreatedAt: at, UsersCount: 3, Data: []byte(`{"1":2}`)},
		}}
		h := NewHandler(Charts{})
		h.SetRetentionRepo(repo)

		rec := callRetention(t, h)
		assert.Equal(t, http.StatusOK, rec.Code)

		var got []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, float64(3), got[0]["users"])
		assert.Contains(t, got[0], "createdAt")
		assert.Contains(t, got[0], "data")

		// upstream `retention.ts` の `take: 30`。
		assert.Equal(t, 30, repo.limit)
	})

	t.Run("read failure still returns an empty array", func(t *testing.T) {
		// **500 にしない。** frontend は配列前提で `.map()` するので、
		// ここで落とすと管理画面のチャートページ全体が開かなくなる。
		h := NewHandler(Charts{})
		h.SetRetentionRepo(&stubRetentionRepo{err: errors.New("boom")})

		rec := callRetention(t, h)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})

	t.Run("unwired repo still returns an empty array", func(t *testing.T) {
		// 集計ジョブを動かしていないインスタンスでもページを開けるようにする。
		rec := callRetention(t, NewHandler(Charts{}))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})

	t.Run("no rows returns an empty array, not null", func(t *testing.T) {
		h := NewHandler(Charts{})
		h.SetRetentionRepo(&stubRetentionRepo{rows: nil})

		rec := callRetention(t, h)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})
}
