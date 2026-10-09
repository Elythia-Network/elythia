package following

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// SilentFollowItem is one element of following/silent/list.
type SilentFollowItem struct {
	// ID is the cursor for sinceId / untilId. It is regenerated each time the
	// same user follows again.
	ID string `json:"id"`
	// CreatedAt is when the latest silent follow from this user happened.
	CreatedAt string          `json:"createdAt"`
	Follower  entity.UserLite `json:"follower"`
}

// ListSilent handles POST /api/following/silent/list (Elythia-only, #3466).
//
// It returns the follows toward the caller that succeeded without a
// notification because of followApprovalAction "silentFollow", newest first.
// The parameters are the same as following/requests/list.
func (h *Handler) ListSilent(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.followingService.ListSilentFollows(me.ID, limit, sinceID, untilID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.packSilent(rows))
}

// packSilent packs silent follows with the followers as UserLite in one
// batch, like packRequests. Rows whose follower no longer exists are dropped.
func (h *Handler) packSilent(rows []*model.SilentFollow) []SilentFollowItem {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.FollowerID)
	}
	bundles, _ := h.userService.ShowManyByIDs(ids)
	users := make([]*model.User, 0, len(bundles))
	for _, b := range bundles {
		if b != nil && b.User != nil {
			users = append(users, b.User)
		}
	}
	lites := make([]entity.UserLite, len(users))
	ptrs := make([]*entity.UserLite, len(users))
	for i, u := range users {
		lites[i] = entity.PackUserLite(u)
		ptrs[i] = &lites[i]
	}
	if h.packer != nil {
		h.packer.FillLites(users, ptrs)
	}
	byID := make(map[string]entity.UserLite, len(users))
	for i, u := range users {
		byID[u.ID] = lites[i]
	}
	out := make([]SilentFollowItem, 0, len(rows))
	for _, r := range rows {
		follower, ok := byID[r.FollowerID]
		if !ok {
			continue
		}
		item := SilentFollowItem{ID: r.ID, Follower: follower}
		if h.idGen != nil {
			if t, err := h.idGen.ParseTime(r.ID); err == nil {
				item.CreatedAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
			}
		}
		out = append(out, item)
	}
	return out
}
