package notes

import (
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// ReactionCreateRequest is the request body for notes/reactions/create.
type ReactionCreateRequest struct {
	NoteID string `json:"noteId"`
	// 欠落 (キー無し) と空文字を区別する。upstream の paramDef は
	// `reaction: {type:'string'}` を required にするだけで minLength が無く、
	// 空文字は valid な入力として ReactionService.normalize が ❤ に倒す。
	Reaction *string `json:"reaction"`
}

// ReactionsCreate handles POST /api/notes/reactions/create.
func (h *Handler) ReactionsCreate(c echo.Context) error {
	user := middleware.GetUser(c)

	var req ReactionCreateRequest
	// #2106 L36: upstream create.ts の paramDef は required:['noteId','reaction']。
	// キーごと欠落しているときだけ endpoint 層で 400 にする。
	if err := c.Bind(&req); err != nil || req.NoteID == "" || req.Reaction == nil {
		return apierr.JSONInvalidParam(c)
	}

	_, err := h.reactionService.Create(user, req.NoteID, *req.Reaction)
	if err != nil {
		switch {
		case errors.Is(err, reaction.ErrNoteNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "033d0620-5bfe-4027-965d-980b0c85a3ea"))
		case errors.Is(err, reaction.ErrNoteNotVisible):
			// #2106 L38 (意図的 divergence): upstream は ReactionService の可視性 IdentifiableError を
			// catch せず ApiCallService が generic INTERNAL_ERROR(500) に包む。mk-go は semantics 上
			// 適切な 403 ACCESS_DENIED を維持する (worse な 500 拡散を避ける)。
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "You can not see this note.", "fe8d7103-0ea8-4ec3-814d-f8b401dc69e9"))
		case errors.Is(err, reaction.ErrAlreadyReacted):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_REACTED", "You are already reacting to that note.", "71efcf98-86d6-4e2b-b2ad-9d032369366b"))
		case errors.Is(err, reaction.ErrCannotReactToPureRenote):
			return c.JSON(http.StatusBadRequest, apierr.Error("CANNOT_REACT_TO_RENOTE", "You cannot react to Renote.", "eaccdc08-ddef-43fe-908f-d108faad57f5"))
		case errors.Is(err, reaction.ErrBlocked):
			// upstream notes/reactions/create.ts は ReactionService の内部 id
			// (e70412a4…) を catch し、endpoint error youHaveBeenBlocked
			// (code=YOU_HAVE_BEEN_BLOCKED, id=20ef5475…) に変換して返す。旧実装は
			// 内部 id と独自 code=BLOCKED をそのまま leak していた (#1538)。
			return c.JSON(http.StatusBadRequest, apierr.Error("YOU_HAVE_BEEN_BLOCKED", "You cannot react this note because you have been blocked by this user.", "20ef5475-9f38-4e4c-bd33-de6d979498ec"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// ReactionDeleteRequest is the request body for notes/reactions/delete.
type ReactionDeleteRequest struct {
	NoteID string `json:"noteId"`
}

// ReactionsDelete handles POST /api/notes/reactions/delete.
func (h *Handler) ReactionsDelete(c echo.Context) error {
	user := middleware.GetUser(c)

	var req ReactionDeleteRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}

	if err := h.reactionService.Delete(user, req.NoteID); err != nil {
		switch {
		case errors.Is(err, reaction.ErrNoteNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "764d9fce-f9f2-4a0e-92b1-6ceac9a7ad37"))
		case errors.Is(err, reaction.ErrReactionNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_REACTED", "You are not reacting to that note.", "92f4426d-4196-4125-aa5b-02943e2ec8fc"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// ReactionsListRequest is the request body for notes/reactions.
type ReactionsListRequest struct {
	NoteID    string `json:"noteId" query:"noteId"`
	Type      string `json:"type" query:"type"`
	Limit     *int   `json:"limit" query:"limit"`
	SinceID   string `json:"sinceId" query:"sinceId"`
	UntilID   string `json:"untilId" query:"untilId"`
	SinceDate *int64 `json:"sinceDate" query:"sinceDate"`
	UntilDate *int64 `json:"untilDate" query:"untilDate"`
}

// Reactions handles POST /api/notes/reactions.
func (h *Handler) Reactions(c echo.Context) error {
	var req ReactionsListRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}

	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	viewer := middleware.GetUser(c)
	rows, err := h.reactionService.List(viewer, req.NoteID, untilID, sinceID, limit, req.Type)
	if err != nil {
		switch {
		case errors.Is(err, reaction.ErrNoteNotFound), errors.Is(err, reaction.ErrNoteNotVisible):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "263fff3d-d0e1-4af4-bea7-8408059b451a"))
		}
		return apierr.JSONInternalError(c)
	}

	// 本家 NoteReactionEntityService.packMany は利用者を packMany (UserLite) で組む
	// ので、リモートの利用者の instance と絵文字をまとめて埋める (#3330)。
	lites := make([]entity.UserLite, len(rows))
	litePtrs := make([]*entity.UserLite, len(rows))
	reactors := make([]*model.User, len(rows))
	for i, r := range rows {
		if r.User != nil {
			lites[i] = entity.PackUserLite(r.User)
			litePtrs[i], reactors[i] = &lites[i], r.User
		}
	}
	entity.FillUserLites(h.instanceLookup(), h.emojiLookup(), reactors, litePtrs)

	out := make([]map[string]any, 0, len(rows))
	for i, r := range rows {
		createdAt := ""
		if t, err := h.idGen.ParseTime(r.ID); err == nil {
			createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		var userField any = map[string]any{"id": r.UserID}
		if r.User != nil {
			userField = lites[i]
		}
		out = append(out, map[string]any{
			"id":        r.ID,
			"createdAt": createdAt,
			"user":      userField,
			// #2106 L7: upstream NoteReactionEntityService.pack は convertLegacyReaction を通す。
			// TS から移行した legacy 値 (like / :smile: host無) を 👍 / :smile@.: に正規化して揃える
			// (mk-native は write-time 正規化済だが drop-in 直後の DB 値はここで揃う)。
			"type": entity.NormalizeReactionWithLegacy(r.Reaction),
		})
	}
	return c.JSON(http.StatusOK, out)
}
