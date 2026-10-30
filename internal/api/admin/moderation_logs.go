package admin

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
)

// ShowModerationLogs handles POST /api/admin/show-moderation-logs.
//
// model.ModerationLog は createdAt 列を持たず aidx ID 先頭 8 文字に
// timestamp を埋め込んでいる。frontend (modlog.ModLog.vue) は
// `<MkTime :time="log.createdAt"/>` を直接読むため、handler 側で aidx
// から派生した createdAt 文字列を response に注入する。invite/users 系
// と同じく "2006-01-02T15:04:05.000Z" 形式 (Misskey の標準)。
func (h *Handler) ShowModerationLogs(c echo.Context) error {
	var req struct {
		Limit     *jsnum.Int `json:"limit"`
		SinceID   string     `json:"sinceId"`
		UntilID   string     `json:"untilId"`
		SinceDate *jsnum.Int `json:"sinceDate"`
		UntilDate *jsnum.Int `json:"untilDate"`
		Type      string     `json:"type"`
		UserID    string     `json:"userId"`
		Search    string     `json:"search"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate/untilDate を aidx prefix に正規化し、type/userId/search で絞る
	// (upstream show-moderation-logs.ts, #1539)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate.Int64Ptr(), req.UntilDate.Int64Ptr())
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	logs, err := h.modLogService.List(model.ModerationLogFilter{
		Limit:   limit,
		SinceID: sinceID,
		UntilID: untilID,
		Type:    req.Type,
		UserID:  req.UserID,
		Search:  req.Search,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream は user を UserDetailedNotMe (optional:false) で pack する。生 model.User
	// を埋めると usernameLower/inbox 等の内部列が漏れ avatarUrl 等 packed field も欠ける。
	// actor (moderator) は重複が多いので distinct user をまとめて pack する (#1539)。
	// 本家 ModerationLogEntityService.packMany は packMany(users, null) なので、
	// 閲覧者は null として扱う (ピン留めは空、カウントは匿名のゲート、#3330)。
	ctx := c.Request().Context()
	seen := map[string]struct{}{}
	actors := make([]*model.User, 0, len(logs))
	for _, l := range logs {
		if l.User == nil {
			continue
		}
		if _, ok := seen[l.User.ID]; ok {
			continue
		}
		seen[l.User.ID] = struct{}{}
		actors = append(actors, l.User)
	}
	packedActors := h.packUsersByID(ctx, nil, actors)
	out := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		m := map[string]any{
			"id":     l.ID,
			"userId": l.UserID,
			"type":   l.Type,
			"info":   redactModerationLogInfo(l.Type, l.Info),
		}
		if s, err := aidxCreatedAtString(h.idGen, l.ID); err == nil {
			m["createdAt"] = s
		} else if !errors.Is(err, ErrIDGenMissing) {
			// idGen は wired されているのに parse 失敗した場合のみログに残す。
			// 非 aidx 形式の legacy ID 等で createdAt が出せない時に frontend
			// 側で「Invalid Date」が出る原因を後追いできるようにする。
			slog.DebugContext(ctx, "modlog: createdAt derive failed",
				"logId", l.ID, "err", err)
		}
		if l.User != nil {
			m["user"] = packedActors[l.User.ID]
		}
		out = append(out, m)
	}
	return c.JSON(http.StatusOK, out)
}
