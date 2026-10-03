package admin

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// AvatarDecorationsCreate handles POST /api/admin/avatar-decorations/create.
func (h *Handler) AvatarDecorationsCreate(c echo.Context) error {
	if h.avatarDecoRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		URL         string   `json:"url"`
		RoleIDs     []string `json:"roleIdsThatCanBeUsedThisDecoration"`
		// Category は upstream Misskey #17034 (= 2026.5.0) で追加。nullable。
		Category *string `json:"category"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" || req.URL == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	d := &model.AvatarDecoration{
		ID:          h.idGen.Generate(time.Now()),
		Name:        req.Name,
		Description: req.Description,
		URL:         req.URL,
		RoleIDs:     req.RoleIDs,
		Category:    req.Category,
	}
	if err := h.avatarDecoRepo.Create(d); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// entity 側の catalog cache (30s TTL) を落とす。これが無いと作成直後に
	// 装着した decoration が packer の lookup miss で silent drop され、
	// DB には入っているのに API 応答は avatarDecorations: [] になる (#2258)。
	h.invalidateAvatarDecorationCache()
	h.logModeration(c, moderationlog.LogCreateAvatarDecoration, map[string]any{
		"avatarDecorationId": d.ID,
		"avatarDecoration":   d,
	})
	return c.JSON(http.StatusOK, h.packAvatarDecoration(d))
}

// packAvatarDecoration shapes an AvatarDecoration into the upstream response.
// model.AvatarDecoration は createdAt 列を持たず aidx ID に時刻を埋め込むため、
// raw model を返すと createdAt が欠落する。ID から導出して付与する。
func (h *Handler) packAvatarDecoration(d *model.AvatarDecoration) map[string]any {
	// roleIds は upstream schema 上 nullable:false (必ず [])。model.StringArray が
	// nil のとき []string(nil) は JSON null になるので空 slice に正規化する。
	roleIDs := []string(d.RoleIDs)
	if roleIDs == nil {
		roleIDs = []string{}
	}
	m := map[string]any{
		"id":                                 d.ID,
		"updatedAt":                          d.UpdatedAt,
		"name":                               d.Name,
		"description":                        d.Description,
		"url":                                d.URL,
		"roleIdsThatCanBeUsedThisDecoration": roleIDs,
		"category":                           d.Category,
	}
	if s, err := aidxCreatedAtString(h.idGen, d.ID); err == nil {
		m["createdAt"] = s
	} else {
		m["createdAt"] = nil
	}
	return m
}

// AvatarDecorationsDelete handles POST /api/admin/avatar-decorations/delete.
func (h *Handler) AvatarDecorationsDelete(c echo.Context) error {
	if h.avatarDecoRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if req.ID == "" {
		return c.NoContent(http.StatusNoContent)
	}
	snapshot, _ := h.avatarDecoRepo.FindByID(req.ID)
	if err := h.avatarDecoRepo.Delete(req.ID); err == nil {
		h.invalidateAvatarDecorationCache()
		if snapshot != nil {
			h.logModeration(c, moderationlog.LogDeleteAvatarDecoration, map[string]any{
				"avatarDecorationId": req.ID,
				"avatarDecoration":   snapshot,
			})
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// AvatarDecorationsList handles POST /api/admin/avatar-decorations/list.
func (h *Handler) AvatarDecorationsList(c echo.Context) error {
	// upstream list.ts paramDef は limit/sinceId/untilId/sinceDate/untilDate/userId
	// を受け取るが、実装は avatarDecorationService.getAll(true) で全件返す。mk-go も
	// これに揃えて全件返す。params は受理するが結果には影響しない (#1543)。
	var req struct {
		Limit     int     `json:"limit"`
		SinceID   string  `json:"sinceId"`
		UntilID   string  `json:"untilId"`
		SinceDate *int64  `json:"sinceDate"`
		UntilDate *int64  `json:"untilDate"`
		UserID    *string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	// **結果に影響しなくてもカーソルは検証する (#3025)。** upstream の paramDef は
	// `misskey:id` (`^[a-zA-Z0-9]+$`) なので ajv が 400 で弾く。ここだけ 200 を
	// 返すと「カーソルは 400」という規則の例外になり、`untilId` を bind する
	// handler が正規化を通っているかを機械的に見られなくなる。
	if _, _, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate); !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	if h.avatarDecoRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	rows, err := h.avatarDecoRepo.List()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	out := make([]map[string]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, h.packAvatarDecoration(d))
	}
	return c.JSON(http.StatusOK, out)
}

// AvatarDecorationsUpdate handles POST /api/admin/avatar-decorations/update.
func (h *Handler) AvatarDecorationsUpdate(c echo.Context) error {
	if h.avatarDecoRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		ID          string    `json:"id"`
		Name        *string   `json:"name"`
		Description *string   `json:"description"`
		URL         *string   `json:"url"`
		RoleIDs     *[]string `json:"roleIdsThatCanBeUsedThisDecoration"`
		// Category は upstream Misskey #17034 (= 2026.5.0)。nullable。
		// `*string` で「未指定 / null = 変更なし、value = set」とする (=
		// upstream 側も `ps.category` を直接渡す semantics に揃える)。
		Category *string `json:"category"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	before, err := h.avatarDecoRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	fields := map[string]any{"updatedAt": time.Now()}
	if req.Name != nil {
		fields["name"] = *req.Name
	}
	if req.Description != nil {
		fields["description"] = *req.Description
	}
	if req.URL != nil {
		fields["url"] = *req.URL
	}
	if req.RoleIDs != nil {
		// model.StringArray でラップしないと GORM Updates(map) が空 string[] を
		// NULL 化して NOT NULL 制約違反になる (#931、#896 / #900 / #901 と同 class)。
		fields["roleIdsThatCanBeUsedThisDecoration"] = model.StringArray(*req.RoleIDs)
	}
	if req.Category != nil {
		// nil = 未指定 (no update)、value = set。upstream の category 扱いと
		// 同 semantics。clear には別途 "category": "" (empty string) を送る運用。
		fields["category"] = *req.Category
	}
	if err := h.avatarDecoRepo.UpdateFields(req.ID, fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.invalidateAvatarDecorationCache()
	if after, err := h.avatarDecoRepo.FindByID(req.ID); err == nil && after != nil {
		h.logModeration(c, moderationlog.LogUpdateAvatarDecoration, map[string]any{
			"avatarDecorationId": req.ID,
			"before":             before,
			"after":              after,
		})
	}
	return c.NoContent(http.StatusNoContent)
}
