// Package promo serves POST /api/promo/read — marking a promoted note as read
// for the caller.
package promo

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// Handler serves the promo endpoints.
type Handler struct {
	noteRepo  repository.NoteRepository
	promoRepo repository.PromoReadRepository
	idGen     id.Generator
}

// NewHandler creates a promo Handler.
func NewHandler(noteRepo repository.NoteRepository, promoRepo repository.PromoReadRepository, idGen id.Generator) *Handler {
	return &Handler{noteRepo: noteRepo, promoRepo: promoRepo, idGen: idGen}
}

// Read marks a promoted note as read for the authenticated user.
// POST /api/promo/read
func (h *Handler) Read(c echo.Context) error {
	user := middleware.GetUser(c)
	if user == nil {
		return c.JSON(http.StatusUnauthorized, apierr.CredentialRequired())
	}
	var req struct {
		NoteID string `json:"noteId"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
	}
	if h.noteRepo == nil || h.promoRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if _, err := h.noteRepo.FindByID(req.NoteID); err != nil {
		if !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		// upstream promo/read.ts の endpoint 固有 id を返す。汎用の
		// UUIDNoSuchNote は **upstream `notes/show` の id** で、error.id で
		// 分岐する drop-in クライアントが誤分類する (#2784)。
		return c.JSON(http.StatusBadRequest,
			apierr.Error("NO_SUCH_NOTE", "No such note.", apierr.UUIDNoSuchNotePromoRead))
	}
	// **エラーを捨てない** (#2784)。upstream は `insert` を await するので
	// 真の DB エラーは 500 になる。重複は `OnConflict{DoNothing}` が握るので、
	// ここに来るのは本物の障害だけ。捨てると「既読にならないのに 204」という
	// 気付けない壊れ方をする。
	if err := h.promoRepo.MarkRead(&model.PromoRead{
		ID:     h.idGen.Generate(time.Now()),
		UserID: user.ID,
		NoteID: req.NoteID,
	}); err != nil {
		slog.Error("promo/read: failed to mark read", "userId", user.ID, "noteId", req.NoteID, "err", err)
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	return c.NoContent(http.StatusNoContent)
}
