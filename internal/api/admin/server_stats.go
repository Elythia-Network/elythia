package admin

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/core/serverstats"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// GetIndexStats handles POST /api/admin/get-index-stats.
//
// Returns per-index row counts from pg_stat_user_indexes so the admin UI can
// spot hot or unused indexes.
func (h *Handler) GetIndexStats(c echo.Context) error {
	if h.adminDB == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	type row struct {
		Relname      string `json:"tablename" gorm:"column:relname"`
		Indexrelname string `json:"indexname" gorm:"column:indexrelname"`
		IdxScan      int64  `json:"idx_scan" gorm:"column:idx_scan"`
		IdxTupRead   int64  `json:"idx_tup_read" gorm:"column:idx_tup_read"`
	}
	var rows []row
	if err := h.adminDB.Raw(`
		SELECT relname, indexrelname, idx_scan, idx_tup_read
		FROM pg_stat_user_indexes
		ORDER BY relname, indexrelname
	`).Scan(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	return c.JSON(http.StatusOK, rows)
}

// GetTableStats handles POST /api/admin/get-table-stats.
//
// Returns per-table size and row estimate via pg_stat_user_tables joined with
// pg_relation_size for quick capacity planning.
func (h *Handler) GetTableStats(c echo.Context) error {
	if h.adminDB == nil {
		return c.JSON(http.StatusOK, map[string]any{})
	}
	type row struct {
		Relname  string `gorm:"column:relname"`
		Count    int64  `gorm:"column:row_count"`
		SizeBase int64  `gorm:"column:size_base"`
		SizeIdx  int64  `gorm:"column:size_idx"`
	}
	var rows []row
	if err := h.adminDB.Raw(`
		SELECT c.relname,
		       COALESCE(s.n_live_tup, 0) AS row_count,
		       pg_relation_size(c.oid) AS size_base,
		       COALESCE(pg_indexes_size(c.oid), 0) AS size_idx
		FROM pg_class c
		LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
		WHERE c.relkind = 'r'
		  AND c.relnamespace IN (SELECT oid FROM pg_namespace WHERE nspname = 'public')
		ORDER BY c.relname
	`).Scan(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// Misskey 本家は { tableName: { count, size } } の map 形式で返すのでそれに合わせる。
	result := make(map[string]any, len(rows))
	for _, r := range rows {
		result[r.Relname] = map[string]any{
			"count": r.Count,
			"size":  r.SizeBase + r.SizeIdx,
		}
	}
	return c.JSON(http.StatusOK, result)
}

// GetUserIPs handles POST /api/admin/get-user-ips.
func (h *Handler) GetUserIPs(c echo.Context) error {
	if h.userIPRepo == nil {
		noStoreIPLookup(c)
		return c.JSON(http.StatusOK, []any{})
	}
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// **NUL を含む id は引く前に落とす** (#3025)。`ListByUser` は一覧系なので
	// repository 側の guard の射程外で、そのまま渡すと PostgreSQL がクエリごと
	// 落とす。下の握り潰しで 200 になるが、**監査には NUL 入りの値が渡り、
	// そちらの INSERT だけが 22021 で失敗する**。
	if !colfit.Storable(req.UserID) {
		noStoreIPLookup(c)
		return c.JSON(http.StatusOK, []any{})
	}
	ips, err := h.userIPRepo.ListByUser(req.UserID, 30)
	if err != nil {
		// **引けていないので監査にも残さない。** ここで `resultCount: 0` を
		// 書くと、本当に 0 件だった照会と見分けが付かなくなる。
		slog.Error("admin/get-user-ips: list failed", "error", err)
		noStoreIPLookup(c)
		return c.JSON(http.StatusOK, []any{})
	}
	result := make([]map[string]any, 0, len(ips))
	for _, ip := range ips {
		result = append(result, map[string]any{
			"ip":        ip.IP,
			"createdAt": ip.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	// **upstream の口も監査に残す** (#3106)。返すのは mk-go 独自の `admin/ip/*` と
	// 同じ「利用者 ↔ IP の対応」なので、ここを記録しないと**監査を迂回して IP を
	// 引ける経路**が 1 本残る。`sinceDays` は 0 — あちらは窓を取らず最新 30 件を返す。
	if me := middleware.GetUser(c); me != nil && h.ipLookupAudit != nil {
		h.ipLookupAudit.Record(iplookuplog.Entry{
			UserID: me.ID, Kind: model.IPLookupKindUserIPs,
			TargetUserID: req.UserID, ResultCount: len(result),
		})
	}
	noStoreIPLookup(c)
	return c.JSON(http.StatusOK, result)
}

// ServerInfo handles POST /api/admin/server-info.
//
// upstream admin/server-info.ts は enableServerMachineStats gate を持たず、
// moderator には常に live な machine 情報を返す (gate は公開 server-info.ts 側
// のみ)。psql / redis のバージョンは system info に含まれないため DB / Redis
// から best-effort で埋める (upstream は SHOW server_version / redis INFO を引く)。
func (h *Handler) ServerInfo(c echo.Context) error {
	stats := serverstats.Collect()
	stats.Psql = h.postgresVersion()
	stats.Redis = h.redisVersion(c.Request().Context())
	return c.JSON(http.StatusOK, stats)
}

// postgresVersion returns the PostgreSQL `server_version` string, or "" when the
// DB is unwired or the query fails (best-effort, upstream `SHOW server_version`).
func (h *Handler) postgresVersion() string {
	if h.adminDB == nil {
		return ""
	}
	var version string
	if err := h.adminDB.Raw("SHOW server_version").Scan(&version).Error; err != nil {
		return ""
	}
	return version
}

// redisVersion returns the Redis `redis_version` from INFO, or "" when the Redis
// info provider is unwired or the call fails (best-effort).
func (h *Handler) redisVersion(ctx context.Context) string {
	if h.queueRedis == nil {
		return ""
	}
	raw, err := h.queueRedis.QueueRedisInfo(ctx)
	if err != nil {
		return ""
	}
	return parseRedisInfo(raw)["redis_version"]
}
