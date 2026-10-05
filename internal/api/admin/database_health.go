package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/dbhealth"
)

// DatabaseHealthReader reads PostgreSQL's statistics (#3095).
type DatabaseHealthReader interface {
	Report(ctx context.Context) (dbhealth.Report, error)
}

// SetDatabaseHealth wires the database health reader.
func (h *Handler) SetDatabaseHealth(r DatabaseHealthReader) {
	h.dbHealth = r
}

// DatabaseHealth handles POST /api/admin/database-health.
//
// **mk-go 独自 endpoint** (#3095)。使われていないインデックス、テーブルごとの
// dead tuple と VACUUM / ANALYZE の最終実行を返す。判断材料を出すだけで、
// 何も消さない。upstream に対応物は無い (`get-index-stats` は pg_indexes の
// 一覧を返すだけ)。
func (h *Handler) DatabaseHealth(c echo.Context) error {
	if h.dbHealth == nil {
		return apierr.JSONInternalError(c)
	}
	r, err := h.dbHealth.Report(c.Request().Context())
	if err != nil {
		// 画面を閉じただけで起きる cancel は障害ではないので、ログを汚さない。
		if !errors.Is(err, context.Canceled) {
			slog.Error("admin: database health failed", "err", err)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, r)
}
