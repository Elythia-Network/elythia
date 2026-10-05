package stats

import (
	"context"
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

	"github.com/elythia-network/elythia/internal/core/chart"
)

// stubChart returns a fixed Result and records the query it was given.
type stubChart struct {
	res  chart.Result
	err  error
	span chart.Span
	n    int
}

func (s *stubChart) GetChart(_ context.Context, span chart.Span, amount int, _ *time.Time, _ string) (chart.Result, error) {
	s.span, s.n = span, amount
	return s.res, s.err
}

func callStats(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Stats(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

func TestStats(t *testing.T) {
	t.Run("sums local and remote totals", func(t *testing.T) {
		notes := &stubChart{res: chart.Result{"local.total": {7}, "remote.total": {5}}}
		users := &stubChart{res: chart.Result{"local.total": {3}, "remote.total": {1}}}

		got := callStats(t, NewHandler(nil, notes, users))

		assert.Equal(t, float64(12), got["notesCount"])
		assert.Equal(t, float64(7), got["originalNotesCount"])
		assert.Equal(t, float64(4), got["usersCount"])
		assert.Equal(t, float64(3), got["originalUsersCount"])

		// upstream stats.ts と同じ窓 (直近 1 時間ぶん 1 点)。
		assert.Equal(t, chart.SpanHour, notes.span)
		assert.Equal(t, 1, notes.n)
	})

	t.Run("missing remote leaves the total at zero", func(t *testing.T) {
		// **移設前の挙動をそのまま保つ。** `remote.total` が無いと合計は 0 の
		// ままで、local だけが original に入る。直感には反するが upstream の
		// 分岐と同じ形で、#2791 の移設で変えるところではない。
		notes := &stubChart{res: chart.Result{"local.total": {7}}}

		got := callStats(t, NewHandler(nil, notes, nil))

		assert.Equal(t, float64(0), got["notesCount"])
		assert.Equal(t, float64(7), got["originalNotesCount"])
	})

	t.Run("chart error reports zeros", func(t *testing.T) {
		// about ページを開けなくするより数字が出ないほうが害が小さい。
		notes := &stubChart{err: errors.New("boom")}

		got := callStats(t, NewHandler(nil, notes, nil))

		assert.Equal(t, float64(0), got["notesCount"])
		assert.Equal(t, float64(0), got["originalNotesCount"])
	})

	t.Run("unwired charts and db still return the full shape", func(t *testing.T) {
		// upstream のレスポンスは全フィールド必須。欠けさせると
		// クライアント側が undefined を踏む。
		got := callStats(t, NewHandler(nil, nil, nil))

		for _, k := range []string{
			"notesCount", "originalNotesCount", "usersCount", "originalUsersCount",
			"instances", "driveUsageLocal", "driveUsageRemote", "reactionsCount",
		} {
			assert.Contains(t, got, k)
		}
		assert.Equal(t, float64(0), got["instances"])
		assert.Equal(t, float64(0), got["reactionsCount"])
	})

	t.Run("empty series is treated as absent", func(t *testing.T) {
		notes := &stubChart{res: chart.Result{"local.total": {}, "remote.total": {}}}

		got := callStats(t, NewHandler(nil, notes, nil))

		assert.Equal(t, float64(0), got["originalNotesCount"])
	})
}
