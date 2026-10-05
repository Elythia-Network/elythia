package channels

import (
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// SetFavoriteRepo attaches a ChannelFavoriteRepository for favorite endpoints.
func (h *Handler) SetFavoriteRepo(r ChannelFavoriteRepository) {
	h.favoriteRepo = r
}

// Favorite handles POST /api/channels/favorite.
func (h *Handler) Favorite(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		ChannelID string `json:"channelId"`
	}
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.favoriteRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if _, err := h.svc.Show(req.ChannelID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "4938f5f3-6167-4c04-9149-6607b7542861"))
	}
	already, _ := h.favoriteRepo.Exists(user.ID, req.ChannelID)
	if already {
		return c.NoContent(http.StatusNoContent)
	}
	fav := &model.ChannelFavorite{
		ID:        h.idGen.Generate(time.Now()),
		UserID:    user.ID,
		ChannelID: req.ChannelID,
	}
	if err := h.favoriteRepo.Create(fav); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// Unfavorite handles POST /api/channels/unfavorite.
func (h *Handler) Unfavorite(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		ChannelID string `json:"channelId"`
	}
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.favoriteRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// upstream unfavorite.ts:53-58 は delete 前に findOneBy で channel 存在を確認し、
	// 無ければ NO_SUCH_CHANNEL を返す (Favorite と対称、#1770)。unfavorite 固有の
	// error id は favorite (4938f5f3) と異なり 353c68dd である点に注意。
	if _, err := h.svc.Show(req.ChannelID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "353c68dd-131a-476c-aa99-88a345e83668"))
	}
	if err := h.favoriteRepo.Delete(user.ID, req.ChannelID); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// MyFavorites handles POST /api/channels/my-favorites.
func (h *Handler) MyFavorites(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.favoriteRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	favs, err := h.favoriteRepo.ListByUser(user.ID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	rows := make([]*model.Channel, 0, len(favs))
	for _, f := range favs {
		ch, err := h.svc.Show(f.ChannelID)
		if err != nil {
			continue
		}
		rows = append(rows, ch)
	}
	// my-favorites の結果は viewer 自身が favorite している channel ばかり
	// なので、batch 経由で isFavorited=true / isFollowing 状態が埋まる (#522)。
	return c.JSON(http.StatusOK, h.channelsToList(rows, user))
}
