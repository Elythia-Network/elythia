package notes

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/poll"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// PollVoteRequest is the request body for notes/polls/vote.
type PollVoteRequest struct {
	NoteID string `json:"noteId"`
	Choice *int   `json:"choice"`
}

// PollsVote handles POST /api/notes/polls/vote.
func (h *Handler) PollsVote(c echo.Context) error {
	user := middleware.GetUser(c)
	var req PollVoteRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" || req.Choice == nil {
		return apierr.JSONInvalidParam(c)
	}

	if err := h.pollService.Vote(user, req.NoteID, *req.Choice); err != nil {
		switch {
		case errors.Is(err, poll.ErrNoteNotFound), errors.Is(err, poll.ErrNoteNotVisible):
			// 見えないノートは、存在しないノートと同じ応答にする。本家 vote.ts も
			// isVisibleForMe が偽のとき noSuchNote を返しており、ACCESS_DENIED を
			// 返すとノートの存在が分かってしまう。
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "ecafbd2e-c283-4d6d-aecb-1a0a33b75396"))
		case errors.Is(err, poll.ErrNoPoll):
			// upstream vote.ts は poll を持たない note への投票に対し NO_SUCH_NOTE
			// でなく専用の NO_POLL を返す (#1538)。HTTP status は upstream noPoll が
			// httpStatusCode 未指定 = kind 'client' default = 400 のため 400 に
			// 揃える (#1765。以前は NO_SUCH_* 慣習で 404 を返していた)。
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_POLL", "The note has no poll.", "5f979967-52d9-4314-a911-1c673727f92f"))
		case errors.Is(err, poll.ErrYouHaveBeenBlocked):
			return c.JSON(http.StatusBadRequest, apierr.Error("YOU_HAVE_BEEN_BLOCKED", "You cannot vote this note because you have been blocked by this user.", "85a5377e-b1e9-4617-b0b9-5bea73331e49"))
		case errors.Is(err, poll.ErrInvalidChoice):
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_CHOICE", "Invalid choice.", "e0cc9a04-f2e8-41e4-a5f1-4127293260cc"))
		case errors.Is(err, poll.ErrPollExpired):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_EXPIRED", "The poll is already expired.", "1022a357-b085-4054-9083-8f8de358337e"))
		case errors.Is(err, poll.ErrAlreadyVoted):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_VOTED", "You have already voted.", "0963fc77-efac-419b-9424-b391608dc6d8"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}
