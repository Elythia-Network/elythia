package i

import (
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// PinRequest is the request body for i/pin and i/unpin.
type PinRequest struct {
	NoteID string `json:"noteId"`
}

// Pin handles POST /api/i/pin.
func (h *Handler) Pin(c echo.Context) error {
	me := middleware.GetUser(c)

	var req PinRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}

	if err := h.userService.PinNote(me.ID, req.NoteID); err != nil {
		switch {
		case errors.Is(err, user.ErrNoteNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "56734f8b-3928-431e-bf80-6ff87df40cb3"))
		case errors.Is(err, user.ErrAlreadyPinned):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_PINNED", "That note has already been pinned.", "8b18c2b7-68fe-4edb-9892-c0cbaeb6c913"))
		case errors.Is(err, user.ErrPinLimitExceeded):
			return c.JSON(http.StatusBadRequest, apierr.Error("PIN_LIMIT_EXCEEDED", "You can not pin notes any more.", "72dab508-c64d-498f-8740-a8eec1ba385a"))
		default:
			return apierr.JSONInternalError(c)
		}
	}

	// featured collection への pin を followers/relay へ連合配信 (#2024)。
	if h.pinDeliveryHook != nil {
		h.pinDeliveryHook.OnLocalPinChanged(me.ID, req.NoteID, true)
	}

	return h.me(c, false)
}

// Unpin handles POST /api/i/unpin.
func (h *Handler) Unpin(c echo.Context) error {
	me := middleware.GetUser(c)

	var req PinRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}

	if err := h.userService.UnpinNote(me.ID, req.NoteID); err != nil {
		if errors.Is(err, user.ErrPinNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "454170ce-9d63-4a43-9da1-ea10afe81e21"))
		}
		return apierr.JSONInternalError(c)
	}

	// featured collection からの unpin を followers/relay へ連合配信 (#2024)。
	if h.pinDeliveryHook != nil {
		h.pinDeliveryHook.OnLocalPinChanged(me.ID, req.NoteID, false)
	}

	return h.me(c, false)
}
