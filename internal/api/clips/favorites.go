package clips

import (
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// ClipFavoriteRepository is the interface for clip favorite operations.
type ClipFavoriteRepository interface {
	Create(fav *model.ClipFavorite) error
	Delete(userID, clipID string) error
	ListByUser(userID string) ([]*model.ClipFavorite, error)
	Exists(userID, clipID string) (bool, error)
	// CountByClip returns the favorite count used by the clip response's
	// favoritedCount field (#1562).
	CountByClip(clipID string) (int64, error)
}

// SetFavoriteRepo attaches a ClipFavoriteRepository for favorite endpoints.
func (h *Handler) SetFavoriteRepo(r ClipFavoriteRepository) {
	h.favoriteRepo = r
}

// Favorite handles POST /api/clips/favorite.
func (h *Handler) Favorite(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		ClipID string `json:"clipId"`
	}
	if err := c.Bind(&req); err != nil || req.ClipID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.favoriteRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// クリップの存在確認 (private clip の他人は Show が ErrClipNotFound を
	// 返すので upstream favorite.ts と同じく NO_SUCH_CLIP に落ちる)
	if _, err := h.svc.Show(user.ID, req.ClipID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CLIP", "No such clip.", "4c2aaeae-80d8-4250-9606-26cb1fdb77a5"))
	}
	already, err := h.favoriteRepo.Exists(user.ID, req.ClipID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if already {
		// upstream favorite.ts は重複 favorite を silent 成功させず
		// ALREADY_FAVORITED を返す (#1562)。
		return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_FAVORITED", "The clip has already been favorited.", "92658936-c625-4273-8326-2d790129256e"))
	}
	fav := &model.ClipFavorite{
		ID:     h.idGen.Generate(time.Now()),
		UserID: user.ID,
		ClipID: req.ClipID,
	}
	if err := h.favoriteRepo.Create(fav); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// Unfavorite handles POST /api/clips/unfavorite.
func (h *Handler) Unfavorite(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		ClipID string `json:"clipId"`
	}
	if err := c.Bind(&req); err != nil || req.ClipID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.favoriteRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// upstream unfavorite.ts は clip の生存在 (visibility 検査なし) と
	// favorite row の存在を順に検査する (#1562)。visibility を見ないのは、
	// favorite 後に非公開化された clip でも解除できる必要があるため。
	exists, err := h.svc.Exists(req.ClipID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if !exists {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CLIP", "No such clip.", "2603966e-b865-426c-94a7-af4a01241dc1"))
	}
	favorited, err := h.favoriteRepo.Exists(user.ID, req.ClipID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if !favorited {
		return c.JSON(http.StatusBadRequest, apierr.Error("NOT_FAVORITED", "You have not favorited the clip.", "90c3a9e8-b321-4dae-bf57-2bf79bbcc187"))
	}
	if err := h.favoriteRepo.Delete(user.ID, req.ClipID); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// MyFavorites handles POST /api/clips/my-favorites.
func (h *Handler) MyFavorites(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.favoriteRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	favs, err := h.favoriteRepo.ListByUser(user.ID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(favs))
	for _, f := range favs {
		// upstream clips/my-favorites は favorite に紐づく clip を visibility 無検査で
		// 全件返す (公開時に favorite した clip を所有者が後で非公開化しても自分の
		// favorites には残る、#1830)。Show は他人所有の非公開 clip を弾くので使わず、
		// 可視性 gate 無しの Get を使う。clip 削除済み (orphan favorite) のみ drop。
		cl, err := h.svc.Get(f.ClipID)
		if err != nil {
			continue
		}
		out = append(out, h.clipToMap(cl, user))
	}
	return c.JSON(http.StatusOK, out)
}
