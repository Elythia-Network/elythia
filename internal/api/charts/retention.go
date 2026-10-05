package charts

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/repository"
)

// SetRetentionRepo wires the repository backing POST /api/retention.
//
// **未配線でも endpoint は生きる** (空配列を返す)。retention の集計ジョブを
// 動かしていないインスタンスでも frontend のダッシュボードが開けるようにする。
func (h *Handler) SetRetentionRepo(r repository.RetentionAggregationRepository) {
	h.retentionRepo = r
}

// Retention returns the recent retention cohorts.
//
// POST /api/retention
//
// **読めなくても 200 で空配列を返す。** upstream `retention.ts` は
// `retentionAggregationsRepository.find()` の結果をそのまま返すだけで、
// frontend 側は配列前提で `.map()` する。ここで 500 にすると管理画面の
// チャートページ全体が開かなくなるので、空で描かせるほうが害が小さい。
func (h *Handler) Retention(c echo.Context) error {
	if h.retentionRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	records, err := h.retentionRepo.ListRecent(retentionLimit)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	out := make([]map[string]any, 0, len(records))
	for _, r := range records {
		out = append(out, map[string]any{
			"createdAt": r.CreatedAt,
			"users":     r.UsersCount,
			"data":      r.Data,
		})
	}
	return c.JSON(http.StatusOK, out)
}

// retentionLimit mirrors upstream `retention.ts` (`take: 30`).
const retentionLimit = 30
