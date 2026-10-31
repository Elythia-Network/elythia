package admin

import (
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// AdCreate handles POST /api/admin/ad/create.
func (h *Handler) AdCreate(c echo.Context) error {
	if h.adRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		URL         string    `json:"url"`
		ImageURL    string    `json:"imageUrl"`
		Place       string    `json:"place"`
		Memo        string    `json:"memo"`
		Priority    string    `json:"priority"`
		Ratio       jsnum.Int `json:"ratio"`
		DayOfWeek   jsnum.Int `json:"dayOfWeek"`
		ExpiresAt   jsnum.Int `json:"expiresAt"`
		StartsAt    jsnum.Int `json:"startsAt"`
		IsSensitive bool      `json:"isSensitive"`
	}
	if err := c.Bind(&req); err != nil || req.URL == "" || req.ImageURL == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	if req.Place == "" {
		req.Place = "square"
	}
	if req.Priority == "" {
		req.Priority = "middle"
	}
	if req.Ratio <= 0 {
		req.Ratio = 1
	}
	ad := &model.Ad{
		ID:          h.idGen.Generate(time.Now()),
		URL:         req.URL,
		ImageURL:    req.ImageURL,
		Place:       req.Place,
		Memo:        req.Memo,
		Priority:    req.Priority,
		Ratio:       int(req.Ratio),
		DayOfWeek:   int(req.DayOfWeek),
		IsSensitive: req.IsSensitive,
		StartsAt:    millisOrNow(int64(req.StartsAt)),
		ExpiresAt:   millisOrDefault(int64(req.ExpiresAt), time.Now().Add(30*24*time.Hour)),
	}
	if err := h.adRepo.Create(ad); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.invalidateMetaResponseCache()
	h.logModeration(c, moderationlog.LogCreateAd, map[string]any{
		"adId": ad.ID,
		"ad":   ad,
	})
	return c.JSON(http.StatusOK, packAd(ad))
}

// packAd serializes an Ad with expiresAt/startsAt as UTC `.000Z` strings,
// matching upstream admin/ad/create.ts / list.ts which return
// `ad.expiresAt.toISOString()` (#1772)。Go の time.Time 既定 marshal は server
// ローカル TZ offset + nanosecond で upstream の Z+ミリ秒固定と書式が異なる。
// field 集合は packedAdSchema と一致。
func packAd(ad *model.Ad) map[string]any {
	const isoMs = "2006-01-02T15:04:05.000Z"
	return map[string]any{
		"id":          ad.ID,
		"expiresAt":   ad.ExpiresAt.UTC().Format(isoMs),
		"startsAt":    ad.StartsAt.UTC().Format(isoMs),
		"place":       ad.Place,
		"priority":    ad.Priority,
		"ratio":       ad.Ratio,
		"url":         ad.URL,
		"imageUrl":    ad.ImageURL,
		"memo":        ad.Memo,
		"dayOfWeek":   ad.DayOfWeek,
		"isSensitive": ad.IsSensitive,
	}
}

// packAds maps packAd over a slice (AdList).
func packAds(ads []*model.Ad) []map[string]any {
	out := make([]map[string]any, 0, len(ads))
	for _, a := range ads {
		out = append(out, packAd(a))
	}
	return out
}

// AdDelete handles POST /api/admin/ad/delete.
func (h *Handler) AdDelete(c echo.Context) error {
	if h.adRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if req.ID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// 本家 ad/delete.ts と同じく、無い広告は NO_SUCH_AD にする (#3369)。以前は
	// 何もせずに 204 を返していた。DB の障害は「無い」に丸めない (#2792)。
	snapshot, err := h.adRepo.FindByID(req.ID)
	if err != nil {
		if repository.IsNotFound(err) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_AD", "No such ad.", "ccac9863-3a03-416e-b899-8a64041118b1"))
		}
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err := h.adRepo.Delete(req.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.invalidateMetaResponseCache()
	h.logModeration(c, moderationlog.LogDeleteAd, map[string]any{
		"adId": req.ID,
		"ad":   snapshot,
	})
	return c.NoContent(http.StatusNoContent)
}

// AdList handles POST /api/admin/ad/list.
func (h *Handler) AdList(c echo.Context) error {
	if h.adRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream admin/ad/list: {limit, sinceId, untilId, sinceDate, untilDate,
	// publishing(boolean,nullable)} の cursor pagination + 配信中フィルタ (#1545)。
	//
	// 本家のparamDefにoffsetは無いので読まない(#3854)。以前は旧クライアント向けに
	// offsetの経路を残していたが、本家が無視する値で件数を飛ばし、型の違う値を400に
	// していた。同梱のfrontendはuntilIdで送るので影響しない。
	var req struct {
		Limit      *jsnum.Int `json:"limit"`
		SinceID    string     `json:"sinceId"`
		UntilID    string     `json:"untilId"`
		SinceDate  *jsnum.Int `json:"sinceDate"`
		UntilDate  *jsnum.Int `json:"untilDate"`
		Publishing *bool      `json:"publishing"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate.Int64Ptr(), req.UntilDate.Int64Ptr())
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.adRepo.ListFiltered(req.Publishing, sinceID, untilID, limit, time.Now())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if rows == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	return c.JSON(http.StatusOK, packAds(rows))
}

// AdUpdate handles POST /api/admin/ad/update.
func (h *Handler) AdUpdate(c echo.Context) error {
	// 旧実装が `c.Request().Body` を `Updates` に直接渡しており部分更新が機能して
	// いなかった。partial field map に差し替え、リクエストで明示された項目のみ
	// 書き換える。
	if h.adRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		ID          string     `json:"id"`
		URL         *string    `json:"url"`
		ImageURL    *string    `json:"imageUrl"`
		Place       *string    `json:"place"`
		Memo        *string    `json:"memo"`
		Priority    *string    `json:"priority"`
		Ratio       *jsnum.Int `json:"ratio"`
		DayOfWeek   *jsnum.Int `json:"dayOfWeek"`
		ExpiresAt   *jsnum.Int `json:"expiresAt"`
		StartsAt    *jsnum.Int `json:"startsAt"`
		IsSensitive *bool      `json:"isSensitive"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	before, err := h.adRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		// 本家 ad/update.ts の NO_SUCH_AD (400) に揃える (#3369)。以前は 404 NOT_FOUND。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_AD", "No such ad.", "b7aa1727-1354-47bc-a182-3a9c3973d300"))
	}
	fields := map[string]any{}
	if req.URL != nil {
		fields["url"] = *req.URL
	}
	if req.ImageURL != nil {
		fields["imageUrl"] = *req.ImageURL
	}
	if req.Place != nil {
		fields["place"] = *req.Place
	}
	if req.Memo != nil {
		fields["memo"] = *req.Memo
	}
	if req.Priority != nil {
		fields["priority"] = *req.Priority
	}
	if req.Ratio != nil {
		fields["ratio"] = int(*req.Ratio)
	}
	if req.DayOfWeek != nil {
		fields["dayOfWeek"] = int(*req.DayOfWeek)
	}
	if req.IsSensitive != nil {
		fields["isSensitive"] = *req.IsSensitive
	}
	// 本家の update.ts は`ps.expiresAt ? new Date(ps.expiresAt) : undefined`なので、
	// 0 を「変えない」として無視する。Elythia も同じにする (#3369)。以前は 0 を
	// 1970年 (エポック) として保存し、広告がすぐに期限切れになっていた。
	if req.StartsAt != nil && *req.StartsAt != 0 {
		fields["startsAt"] = time.UnixMilli(int64(*req.StartsAt))
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != 0 {
		fields["expiresAt"] = time.UnixMilli(int64(*req.ExpiresAt))
	}
	if len(fields) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.adRepo.UpdateFields(req.ID, fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.invalidateMetaResponseCache()
	if after, err := h.adRepo.FindByID(req.ID); err == nil && after != nil {
		h.logModeration(c, moderationlog.LogUpdateAd, map[string]any{
			"adId":   req.ID,
			"before": before,
			"after":  after,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// millisOrNow converts a UNIX millisecond timestamp to time.Time; 0 falls back
// to the current wall clock.
func millisOrNow(ms int64) time.Time {
	if ms == 0 {
		return time.Now()
	}
	return time.UnixMilli(ms)
}

// millisOrDefault converts a UNIX millisecond timestamp to time.Time; 0 falls
// back to the provided default.
func millisOrDefault(ms int64, def time.Time) time.Time {
	if ms == 0 {
		return def
	}
	return time.UnixMilli(ms)
}
