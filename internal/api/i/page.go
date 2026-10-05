package i

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// PageLikes handles POST /api/i/page-likes.
//
// 自分が like した page 一覧を返却する。upstream `PageLikeEntityService.pack`
// と shape を揃え、各 row を `{ id, page: <full Page entity> }` で返す
// (#1136)。frontend pages.vue の `liked` タブは MkPagePreview に `like.page` を
// 渡しているので、`page.user.username` を含む full entity が必要。
//
// 経路:
//  1. page_like list (cursor 無し、limit/offset のみ)
//  2. page を FindManyByIDs で 1 batch fetch
//  3. ユニークな page owner を FindManyByIDs で 1 batch fetch
//  4. page or owner が解決できない like 行は drop (= upstream packMany と同 fail-soft)
func (h *Handler) PageLikes(c echo.Context) error {
	if h.pageLikeRepo == nil || h.pageRepo == nil || h.userService == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	u := middleware.GetUser(c)
	limit, offset, sinceID, untilID, cursorOK := paginationFromRequest(c)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	likes, err := h.pageLikeRepo.ListByUser(u.ID, sinceID, untilID, limit, offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if len(likes) == 0 {
		return c.JSON(http.StatusOK, []any{})
	}

	pageIDs := make([]string, 0, len(likes))
	for _, l := range likes {
		pageIDs = append(pageIDs, l.PageID)
	}
	pages, err := h.pageRepo.FindManyByIDs(pageIDs)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	pagesByID := make(map[string]*model.Page, len(pages))
	for _, p := range pages {
		pagesByID[p.ID] = p
	}

	// page owner を unique 化してから batch fetch (同一 user の page を複数 like
	// しているケースの round-trip 削減)。
	seenOwner := make(map[string]struct{}, len(pages))
	ownerIDs := make([]string, 0, len(pages))
	for _, p := range pages {
		if _, ok := seenOwner[p.UserID]; ok {
			continue
		}
		seenOwner[p.UserID] = struct{}{}
		ownerIDs = append(ownerIDs, p.UserID)
	}
	owners, err := h.userService.FindManyByIDs(ownerIDs)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	ownersByID := make(map[string]*model.User, len(owners))
	for _, o := range owners {
		ownersByID[o.ID] = o
	}

	// i/page-likes は自分が like した page 一覧なので、埋め込み page の isLiked は
	// 常に true。upstream は packMany(likes, me) で me を渡し isLiked を出す
	// (PageEntityService.ts isLiked: meId ? exists(...) : undefined)。mk-go も
	// IsLiked を立てて shape を揃える (#1830)。
	liked := true
	out := make([]map[string]any, 0, len(likes))
	for _, l := range likes {
		p, ok := pagesByID[l.PageID]
		if !ok {
			// page 削除後の dangling like (FK constraint で起きないはずだが
			// transient な timing race 用 fail-soft)。
			continue
		}
		owner, ok := ownersByID[p.UserID]
		if !ok {
			// owner 解決失敗時は drop。pack して null user を出すと frontend
			// MkPagePreview の page.user.username で再 throw する。
			continue
		}
		out = append(out, map[string]any{
			"id": l.ID,
			"page": entity.PackPageWithContext(p, entity.PackPageContext{
				IDGen:   h.idGen,
				Owner:   owner,
				IsLiked: &liked,
			}),
		})
	}
	return c.JSON(http.StatusOK, out)
}
