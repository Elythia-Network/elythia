// Package stats serves POST /api/stats — the instance-wide counters shown on
// the about page and consumed by external instance listings.
package stats

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// ChartReader is the subset of *chart.Chart this endpoint needs.
//
// **具体型ではなく interface で受ける。** stats が要るのは「直近 1 時間の
// local / remote の累計」だけで chart engine 全体ではない。テストから任意の
// 値を入れられるようにもなる。
type ChartReader interface {
	GetChart(ctx context.Context, span chart.Span, amount int, cursor *time.Time, group string) (chart.Result, error)
}

// Handler serves the stats endpoint.
type Handler struct {
	db         *gorm.DB
	notesChart ChartReader
	usersChart ChartReader
}

// NewHandler creates a stats Handler.
//
// db / chart はいずれも nil を許す。未配線の項目は 0 で返す — upstream の
// レスポンス shape は全フィールド必須なので、欠けさせるより 0 のほうが安全。
func NewHandler(db *gorm.DB, notesChart, usersChart ChartReader) *Handler {
	return &Handler{db: db, notesChart: notesChart, usersChart: usersChart}
}

// totals reads the newest local/remote cumulative totals from one chart.
//
// **remote を足した合計と local 単独の両方を返す。** upstream stats.ts は
// `notesCount` に全体、`originalNotesCount` に local だけを入れる。
//
// chart が引けないときは 0 を返す。ここで 500 にすると about ページが
// 開かなくなるが、実際に困るのは数字が出ないことだけ。
func totals(ctx context.Context, ch ChartReader) (all, local int64) {
	if ch == nil {
		return 0, 0
	}
	res, err := ch.GetChart(ctx, chart.SpanHour, 1, nil, "")
	if err != nil {
		return 0, 0
	}
	if v, ok := res["local.total"]; ok && len(v) > 0 {
		local = v[0]
	}
	if v, ok := res["remote.total"]; ok && len(v) > 0 {
		all = local + v[0]
	}
	return all, local
}

// Stats returns instance-wide counters.
// POST /api/stats
func (h *Handler) Stats(c echo.Context) error {
	ctx := c.Request().Context()

	notesCount, originalNotesCount := totals(ctx, h.notesChart)
	usersCount, originalUsersCount := totals(ctx, h.usersChart)

	var instancesCount, reactionsCount int64
	if h.db != nil {
		h.db.Model(&model.Instance{}).Count(&instancesCount)
		// upstream stats.ts は reactionsCount を noteReactionsRepository.count()
		// で返す。mk-go は 0 固定だったので note_reaction の総数を集計する (#1777)。
		h.db.Model(&model.NoteReaction{}).Count(&reactionsCount)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"notesCount":         notesCount,
		"originalNotesCount": originalNotesCount,
		"usersCount":         usersCount,
		"originalUsersCount": originalUsersCount,
		"instances":          instancesCount,
		"driveUsageLocal":    0,
		"driveUsageRemote":   0,
		"reactionsCount":     reactionsCount,
	})
}
