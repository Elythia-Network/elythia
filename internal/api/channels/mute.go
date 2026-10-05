package channels

import (
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// SetMutingRepo attaches a ChannelMutingRepository for mute endpoints.
func (h *Handler) SetMutingRepo(r ChannelMutingRepository) {
	h.mutingRepo = r
}

// RelationReloadPublisher notifies streaming connections that the viewer's
// mute/block snapshot changed (#2400)。channel mute は MuteBlockSnapshot の
// MutingChannels に載るので、変更したら接続中の snapshot を取り直させる。
type RelationReloadPublisher interface {
	PublishMuteBlockReload(userID string)
}

// SetRelationReloadPublisher wires the streaming reload publisher (#2400)。
// 未配線なら通知しない (= 従来どおり再接続まで stale)。
func (h *Handler) SetRelationReloadPublisher(p RelationReloadPublisher) {
	h.relationReload = p
}

func (h *Handler) publishMuteReload(userID string) {
	if h.relationReload == nil || userID == "" {
		return
	}
	h.relationReload.PublishMuteBlockReload(userID)
}

// MuteCreate handles POST /api/channels/mute/create.
func (h *Handler) MuteCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		ChannelID string `json:"channelId"`
		// ExpiresAt is an epoch-ms timestamp that must lie in the future
		// (nil = indefinite mute), mirroring upstream channels/mute/create
		// paramDef (#1603)。
		ExpiresAt *int64 `json:"expiresAt"`
	}
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.mutingRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if _, err := h.svc.Show(req.ChannelID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "7174361e-d58f-31d6-2e7c-6fb830786a3f"))
	}
	// 本家 create.ts は alreadyMuting → expiresAt-past の順でチェックする。
	// 本家 create.ts は alreadyMuting → expiresAt-past の順でチェックする。既ミュート
	// 済みは ALREADY_MUTING_CHANNEL を返す (#1540)。Exists は期限切れを除外するため
	// (#1603)、ここで true=能動的 mute。
	already, _ := h.mutingRepo.Exists(user.ID, req.ChannelID)
	if already {
		return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_MUTING_CHANNEL", "You are already muting that channel.", "5a251978-769a-da44-3e89-3931e43bb592"))
	}
	// 本家 create.ts: `ps.expiresAt && ps.expiresAt <= Date.now()` で過去なら
	// EXPIRES_AT_IS_PAST。0 は JS で falsy のため無期限 (nil) として扱う。
	var expires *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != 0 {
		if *req.ExpiresAt <= time.Now().UnixMilli() {
			return c.JSON(http.StatusBadRequest, apierr.Error("EXPIRES_AT_IS_PAST", `Cannot set past date to "expiresAt".`, "42b32236-df2c-a45f-fdbf-def67268f749"))
		}
		t := time.UnixMilli(*req.ExpiresAt)
		expires = &t
	}
	mut := &model.ChannelMuting{
		ID:        h.idGen.Generate(time.Now()),
		UserID:    user.ID,
		ChannelID: req.ChannelID,
		ExpiresAt: expires,
	}
	if err := h.mutingRepo.Create(mut); err != nil {
		return apierr.JSONInternalError(c)
	}
	h.publishMuteReload(user.ID)
	return c.NoContent(http.StatusNoContent)
}

// MuteDelete handles POST /api/channels/mute/delete.
func (h *Handler) MuteDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		ChannelID string `json:"channelId"`
	}
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.mutingRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// 本家 delete.ts は channel 不在 → NO_SUCH_CHANNEL、未ミュート → NOT_MUTING_CHANNEL
	// の順でチェックしてから削除する (#1540)。
	if _, err := h.svc.Show(req.ChannelID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "e7998769-6e94-d9c2-6b8f-94a527314aba"))
	}
	if muting, _ := h.mutingRepo.Exists(user.ID, req.ChannelID); !muting {
		return c.JSON(http.StatusBadRequest, apierr.Error("NOT_MUTING_CHANNEL", "You are not muting that channel.", "14d55962-6ea8-d990-1333-d6bef78dc2ab"))
	}
	if err := h.mutingRepo.Delete(user.ID, req.ChannelID); err != nil {
		return apierr.JSONInternalError(c)
	}
	h.publishMuteReload(user.ID)
	return c.NoContent(http.StatusNoContent)
}

// MuteList handles POST /api/channels/mute/list.
func (h *Handler) MuteList(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.mutingRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	mutings, err := h.mutingRepo.ListByUser(user.ID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	rows := make([]*model.Channel, 0, len(mutings))
	for _, m := range mutings {
		ch, err := h.svc.Show(m.ChannelID)
		if err != nil {
			continue
		}
		rows = append(rows, ch)
	}
	// MyFavorites と同じく channelsToList 経由で viewer-aware に
	// 揃える (Devin #530 BUG-1)。
	return c.JSON(http.StatusOK, h.channelsToList(rows, user))
}
