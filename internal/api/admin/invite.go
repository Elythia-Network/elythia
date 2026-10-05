package admin

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// InviteCreate handles POST /api/admin/invite/create.
func (h *Handler) InviteCreate(c echo.Context) error {
	// 本家 TS は count (1-100, default 1) 分のチケットを配列で返す。個々の Create
	// 失敗時でも既作成分はロールバックしない (本家も Promise.all で非原子的)。
	if h.inviteRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		Count     int     `json:"count"`
		ExpiresAt *string `json:"expiresAt"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Count > 100 {
		req.Count = 100
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_DATE_TIME", "Invalid date-time format", "f1380b15-3760-4c6c-a1db-5c3aaf1cbd49"))
		}
		expiresAt = &parsed
	}
	user := middleware.GetUser(c)
	var createdByID *string
	if user != nil {
		createdByID = &user.ID
	}
	tickets := make([]*model.RegistrationTicket, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		t := &model.RegistrationTicket{
			ID:          h.idGen.Generate(time.Now()),
			Code:        hex.EncodeToString(b),
			ExpiresAt:   expiresAt,
			CreatedByID: createdByID,
		}
		if err := h.inviteRepo.Create(t); err != nil {
			continue
		}
		tickets = append(tickets, t)
	}
	if len(tickets) > 0 {
		h.logModeration(c, moderationlog.LogCreateInvitation, map[string]any{
			"invitations": tickets,
		})
	}
	return c.JSON(http.StatusOK, h.packInviteTickets(tickets))
}

// packInviteTickets transforms RegistrationTicket rows into the
// Misskey-compatible InviteCodeEntityService.pack shape.
//
// createdBy / usedBy field を `nil` で hardcode していた旧実装では frontend
// が「system」「不明（メール認証待ち）」の fallback 表示に倒れていた
// (#1048 / #1049)。upstream `InviteCodeEntityService.packMany` と同じく、
// 全 row 分の createdById / usedById を 1 度の FindManyByIDs で bulk fetch
// (= N+1 回避) し、UserLite DTO で pack する。
func (h *Handler) packInviteTickets(rows []*model.RegistrationTicket) []map[string]any {
	// step 1: createdBy / usedBy で参照される全 user ID を集約 (dedup)。
	idSet := make(map[string]struct{})
	for _, t := range rows {
		if t.CreatedByID != nil {
			idSet[*t.CreatedByID] = struct{}{}
		}
		if t.UsedByID != nil {
			idSet[*t.UsedByID] = struct{}{}
		}
	}
	// step 2: 1 度の bulk fetch で全 user を取得。userRepo 未配線 path (=
	// test fixture) では空 map で fallback、旧挙動と互換に振る舞う。
	userMap := make(map[string]*model.User, len(idSet))
	if len(idSet) > 0 && h.userRepo != nil {
		ids := make([]string, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		users, err := h.userRepo.FindManyByIDs(ids)
		if err != nil {
			// graceful degrade: bulk fetch 失敗時は全 createdBy / usedBy が
			// null で返って frontend は旧 fallback (= "system" / "不明") に倒れる
			// が、admin list 自体は表示できる方が UX 優先。operator が原因を
			// 追える程度に Warn を残す (= silent fallback の観測性確保)。
			slog.Warn("admin/invite: bulk fetch user failed for invite pack",
				"err", err, "count", len(ids))
		} else {
			for _, u := range users {
				userMap[u.ID] = u
			}
		}
	}
	// step 3: pack。userMap に hit すれば UserLite DTO、miss なら null。
	// upstream は relations join で `createdBy` 自体が null の case (=
	// 既に user が削除された ticket) と区別しない。本実装も同 trade-off。
	packUser := func(id *string) any {
		if id == nil {
			return nil
		}
		u, ok := userMap[*id]
		if !ok {
			return nil
		}
		return entity.PackUserLite(u)
	}

	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		var createdAt *string
		if s, err := aidxCreatedAtString(h.idGen, t.ID); err == nil {
			createdAt = &s
		}
		var expiresAt *string
		if t.ExpiresAt != nil {
			s := t.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
			expiresAt = &s
		}
		var usedAt *string
		if t.UsedAt != nil {
			s := t.UsedAt.UTC().Format("2006-01-02T15:04:05.000Z")
			usedAt = &s
		}
		out = append(out, map[string]any{
			"id":          t.ID,
			"code":        t.Code,
			"expiresAt":   expiresAt,
			"createdAt":   createdAt,
			"createdBy":   packUser(t.CreatedByID),
			"usedBy":      packUser(t.UsedByID),
			"usedAt":      usedAt,
			"used":        t.UsedAt != nil,
			"createdById": t.CreatedByID,
			"usedById":    t.UsedByID,
		})
	}
	return out
}

// InviteList handles POST /api/admin/invite/list.
func (h *Handler) InviteList(c echo.Context) error {
	if h.inviteRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	var req struct {
		Limit  *int   `json:"limit"`
		Offset int    `json:"offset"`
		Type   string `json:"type"`
		Sort   string `json:"sort"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 30, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	filter := req.Type
	switch filter {
	case "unused", "used", "expired", "all":
	default:
		filter = "all"
	}
	// upstream admin/invite/list sort enum (#1545)。未知値は repo 側で id DESC に倒れる。
	sort := req.Sort
	switch sort {
	case "+createdAt", "-createdAt", "+usedAt", "-usedAt":
	default:
		sort = ""
	}
	rows, err := h.inviteRepo.ListSorted(filter, sort, limit, req.Offset, time.Now())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	return c.JSON(http.StatusOK, h.packInviteTickets(rows))
}
