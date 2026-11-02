package reversi

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// Elythia: 対局の操作の REST の入口 (#3942)。本家は準備完了・石を置く・時間切れの
// 申告を websocket の reversiGame チャンネルでしか受け付けない。プラグインが管理する
// アカウントは streaming に繋げないので、同じ操作を REST からも受ける。できることと
// 断る条件はチャンネルと同じにする (どちらも core/reversi.Service を呼ぶ)。

// actionContext is the context the actions pass to the service.
//
// **リクエストの取り消しを伝えない。** Service は手を DB に書いた後で手番のタイマーを
// Redis に置き、連合の相手へ配る。取り消された ctx では書いた後の処理だけが失敗し、
// タイマーの無い手番ができて、相手が時間切れをすぐに申告できてしまう (接続を切るだけで
// 起こせる。レビューで実測)。チャンネルは context.Background() で呼んでいる。
func actionContext(c echo.Context) context.Context {
	return context.WithoutCancel(c.Request().Context())
}

// actionError is one error an action endpoint can return.
type actionError struct {
	err     error
	code    string
	message string
	id      string
}

// actionErrors maps the service errors of one endpoint to its error bodies.
// 本家の慣習に合わせて、endpoint ごとに別の id を持つ。
type actionErrors []actionError

func (e actionErrors) respond(c echo.Context, err error) error {
	for _, a := range e {
		if errors.Is(err, a.err) {
			return c.JSON(http.StatusBadRequest, apierr.Error(a.code, a.message, a.id))
		}
	}
	// DB の障害など。内部の文面は返さない。
	return c.JSON(http.StatusInternalServerError, apierr.InternalError())
}

var readyErrors = actionErrors{
	{corereversi.ErrGameNotFound, "NO_SUCH_GAME", "No such game.", "08beb6e2-6cde-48c6-87db-a5ce07923cde"},
	{corereversi.ErrNotPlayer, "ACCESS_DENIED", "Access denied.", "90727173-553f-42bd-8c91-c1bd8683527d"},
	{corereversi.ErrAlreadyStarted, "ALREADY_STARTED", "Game has already started.", "c3edf082-e015-4235-9446-470f6dc95ee7"},
	{corereversi.ErrAlreadyEnded, "ALREADY_ENDED", "Game has already ended.", "3f128b3c-340c-4b49-b117-91981b51dbaa"},
}

var putStoneErrors = actionErrors{
	{corereversi.ErrGameNotFound, "NO_SUCH_GAME", "No such game.", "602749a2-db0c-4978-bffa-b535be6b5c8e"},
	{corereversi.ErrNotPlayer, "ACCESS_DENIED", "Access denied.", "db6179ef-2feb-47b1-afad-d75423d718d2"},
	{corereversi.ErrNotStarted, "NOT_STARTED", "Game has not started yet.", "b2b320f2-cc77-4089-8fbe-5e40951001af"},
	{corereversi.ErrAlreadyEnded, "ALREADY_ENDED", "Game has already ended.", "8bdf089c-34bb-4e22-91e9-13e82c62b5cd"},
	{corereversi.ErrNotYourTurn, "NOT_YOUR_TURN", "It is not your turn.", "1ff5c3c3-d4f5-4fc0-99a3-5f2d2a8b69aa"},
	{corereversi.ErrInvalidMove, "INVALID_MOVE", "You cannot put a stone there.", "eb0041ee-40e2-4913-8d75-6e7ac73db7a3"},
}

var claimTimeIsUpErrors = actionErrors{
	{corereversi.ErrGameNotFound, "NO_SUCH_GAME", "No such game.", "971a87d5-5a0c-4517-9a90-cbd0f63309a2"},
}

// Ready handles POST /api/reversi/ready (Elythia, #3942): the `ready`
// message of the reversiGame channel. 両者が準備完了になると対局が始まる。
func (h *Handler) Ready(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		GameID string `json:"gameId"`
		Ready  *bool  `json:"ready"`
	}
	if err := c.Bind(&req); err != nil || req.GameID == "" || req.Ready == nil {
		return apierr.JSONInvalidParam(c)
	}
	if h.svc == nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err := h.svc.UpdateReady(actionContext(c), req.GameID, user.ID, *req.Ready); err != nil {
		return readyErrors.respond(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// PutStone handles POST /api/reversi/put-stone (Elythia, #3942): the
// `putStone` message of the reversiGame channel. id はチャンネルと同じく、
// stream の log イベントにそのまま載る (操作の突き合わせ用)。
func (h *Handler) PutStone(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		GameID string `json:"gameId"`
		// 本家の ajv と同じく 10.0 や 1e1 も 10 として受ける (jsnum)
		Pos *jsnum.Int `json:"pos"`
		ID  string     `json:"id"`
	}
	if err := c.Bind(&req); err != nil || req.GameID == "" || req.Pos == nil {
		return apierr.JSONInvalidParam(c)
	}
	if h.svc == nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err := h.svc.PutStone(actionContext(c), req.GameID, user.ID, *req.Pos.IntPtr(), req.ID); err != nil {
		return putStoneErrors.respond(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// ClaimTimeIsUp handles POST /api/reversi/claim-time-is-up (Elythia, #3942):
// the `claimTimeIsUp` message of the reversiGame channel.
//
// チャンネルと同じく対局者でなくても呼べる。手番の時間が残っていれば何もしない
// (結果は返さない。終局したかは reversi/show-game で読む)。
func (h *Handler) ClaimTimeIsUp(c echo.Context) error {
	var req struct {
		GameID string `json:"gameId"`
	}
	if err := c.Bind(&req); err != nil || req.GameID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.svc == nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err := h.svc.CheckTimeout(actionContext(c), req.GameID); err != nil {
		return claimTimeIsUpErrors.respond(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
