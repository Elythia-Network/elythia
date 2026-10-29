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
	c.Response().Header().Set("Cache-Control", "no-store")
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
	h.seedAPNote(c, note)
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
	available, err := h.requireAPUser(c, author.User, "quote-authorization")
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	if !available {
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
	// interactingObject がこの instance の local note なら、可視性を確認する。
	// 引用者への承認後に引用者が凍結・削除されても、承認者が発行した stamp は失効しない。
	// URI lookup は remote URI なら通常 not-found。
	var quoted *model.Note
	var quotedErr error
	localQuoted := false
	if localNoteID := h.renderer.URLs().LocalNoteIDFromURI(a.QuotingURI); localNoteID != "" {
		localQuoted = true
		quoted, quotedErr = h.resolveAPNote(c, localNoteID)
	} else {
		quoted, quotedErr = h.noteRepo.FindByURI(a.QuotingURI)
	}
	if localQuoted && repository.IsNotFound(quotedErr) {
		return c.NoContent(http.StatusNotFound)
	}
	if quotedErr != nil && !repository.IsNotFound(quotedErr) {
		slog.Error("ap: quote authorization: find interacting note", "err", quotedErr)
		return c.NoContent(http.StatusInternalServerError)
	}
	if quotedErr == nil {
		h.seedAPNote(c, quoted)
	}
	if quotedErr == nil && quoted.UserHost == nil {
		if quoted.LocalOnly {
			return c.NoContent(http.StatusNotFound)
		}
		quotedAuthor, err := h.userService.ShowByID(quoted.UserID)
		if err != nil {
			if errors.Is(err, coreuser.ErrUserNotFound) {
				return c.NoContent(http.StatusNotFound)
			}
			slog.Error("ap: quote authorization: find interacting author", "err", err)
			return c.NoContent(http.StatusInternalServerError)
		}
		available, err = h.requireAPUser(c, quotedAuthor.User, "quote-authorization")
		if err != nil {
			return c.NoContent(http.StatusInternalServerError)
		}
		if !available {
			return c.NoContent(http.StatusNotFound)
		}
	}
	return h.writeAPJSON(c, h.renderer.RenderQuoteAuthorization(note, a))
}
