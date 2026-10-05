package ap

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// QuoteAuthorizationStore looks up quote approvals (#3234).
type QuoteAuthorizationStore interface {
	FindByIDAndNoteID(id, noteID string) (*model.NoteQuoteAuthorization, error)
}

// SetQuoteAuthorizationStore wires the store serving QuoteAuthorization.
func (h *Handler) SetQuoteAuthorizationStore(s QuoteAuthorizationStore) {
	h.quoteAuthorizations = s
}

// QuoteAuthorization handles GET /notes/:id/quote-authorizations/:authId, the
// FEP-044f approval stamp for a quote of a local note (#3234).
//
// **公開範囲では絞らない。** followers 限定の投稿への引用でも、第三者 (引用を
// 表示するサーバー) はこれを取得して確かめる。出すのは URI だけで、投稿の中身は
// 埋め込まない (FEP の MUST NOT) ので、見る権限の無い相手に中身は渡らない。
func (h *Handler) QuoteAuthorization(c echo.Context) error {
	c.Response().Header().Set("Vary", "Accept")
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	if h.noteRepo == nil || h.quoteAuthorizations == nil {
		return c.NoContent(http.StatusNotFound)
	}
	note, err := h.noteRepo.FindByID(c.Param("id"))
	if err != nil {
		if repository.IsNotFound(err) {
			return c.NoContent(http.StatusNotFound)
		}
		slog.Error("ap: quote authorization: find note", "err", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	// 承認はローカルの投稿にしか出さない。
	if note.UserHost != nil || note.LocalOnly {
		return c.NoContent(http.StatusNotFound)
	}
	// 凍結・削除された作者の承認は配らない (投稿そのものも配っていない)。
	author, err := h.userService.ShowByID(note.UserID)
	if err != nil {
		if errors.Is(err, coreuser.ErrUserNotFound) {
			return c.NoContent(http.StatusNotFound)
		}
		slog.Error("ap: quote authorization: find author", "err", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if author.User.IsSuspended || author.User.IsDeleted {
		return c.NoContent(http.StatusNotFound)
	}
	a, err := h.quoteAuthorizations.FindByIDAndNoteID(c.Param("authId"), note.ID)
	if err != nil {
		if repository.IsNotFound(err) {
			return c.NoContent(http.StatusNotFound)
		}
		slog.Error("ap: quote authorization: find approval", "err", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	// 取り消し (#3234 の段階 4) を早めに届けるため、長くはキャッシュさせない。
	c.Response().Header().Set("Cache-Control", "public, max-age=180")
	return writeActivityJSON(c, h.renderer.RenderQuoteAuthorization(note, a))
}
