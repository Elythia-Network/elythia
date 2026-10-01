package bubblegame

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/bubbleversus"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// UserFinder resolves users for the versus endpoints.
type UserFinder interface {
	FindByID(id string) (*model.User, error)
}

// VersusHandler handles bubble-game/versus/* (#3230). mk-go 独自の endpoint
// (upstream にバブルゲームの対戦は無い)。
type VersusHandler struct {
	svc     *bubbleversus.Service
	users   UserFinder
	records repository.BubbleVersusRepository
	blocks  bubbleversus.BlockChecker
}

// NewVersusHandler constructs a VersusHandler.
func NewVersusHandler(svc *bubbleversus.Service, users UserFinder) *VersusHandler {
	return &VersusHandler{svc: svc, users: users}
}

// 対戦 ID は id.Generator の出力 (英数字。ulid は大文字を使う)。Redis のキーに
// 載るだけだが、形の違うものは引く前に「無い」とする。
var matchIDPattern = regexp.MustCompile(`^[0-9A-Za-z]{1,32}$`)

var (
	errNoSuchMatch    = apierr.Error("NO_SUCH_MATCH", "No such match.", "ef38d161-0c28-4048-8c58-3331f01571a6")
	errNoSuchUser     = apierr.Error("NO_SUCH_USER", "No such user.", "d395492a-6c7b-4e8b-9523-ad2135901ca3")
	errYourself       = apierr.Error("TARGET_IS_YOURSELF", "You cannot invite yourself.", "f891159f-dc42-4621-bac9-ff42a684be5c")
	errRemoteUser     = apierr.Error("TARGET_IS_REMOTE", "Versus is only available between local users.", "b8f0b979-e60b-4b61-a749-5207266aacd6")
	errBlocked        = apierr.Error("BLOCKED", "You cannot invite this user.", "6d2ab43a-2072-4f42-8c9b-337e802f4e14")
	errInvalidMode    = apierr.Error("INVALID_GAME_MODE", "gameMode is invalid.", "f91c03aa-75cc-4444-bc58-d02033ac1ad4")
	errInvalidState   = apierr.Error("INVALID_STATE", "The match is not in a state that allows this.", "ff76d73c-001c-4170-8acb-5a0f930545f8")
	errInvalidReport  = apierr.Error("INVALID_REPORT", "The report is invalid.", "7f5163d3-acd7-4806-9826-ce78392376e5")
	errNotYet         = apierr.Error("NOT_YET", "It is too early for this.", "359c939e-93a2-4373-b4e7-4cb083b616d0")
	errVersusInvalidP = apierr.Error("INVALID_PARAM", "Invalid param.", "3b2e9f34-c3c5-47c7-9c44-5515a98aa11a")
)

// fail maps a service error to a response. 内部のエラーは文面を返さない。
func (h *VersusHandler) fail(c echo.Context, err error) error {
	switch {
	case errors.Is(err, bubbleversus.ErrNoSuchMatch), errors.Is(err, bubbleversus.ErrNotParticipant):
		// 参加していない対局は、あることも知らせない。
		return c.JSON(http.StatusNotFound, errNoSuchMatch)
	case errors.Is(err, bubbleversus.ErrYourself):
		return c.JSON(http.StatusBadRequest, errYourself)
	case errors.Is(err, bubbleversus.ErrRemoteUser):
		return c.JSON(http.StatusBadRequest, errRemoteUser)
	case errors.Is(err, bubbleversus.ErrBlocked):
		return c.JSON(http.StatusBadRequest, errBlocked)
	case errors.Is(err, bubbleversus.ErrInvalidGameMode):
		return c.JSON(http.StatusBadRequest, errInvalidMode)
	case errors.Is(err, bubbleversus.ErrInvalidState):
		return c.JSON(http.StatusBadRequest, errInvalidState)
	case errors.Is(err, bubbleversus.ErrInvalidReport):
		return c.JSON(http.StatusBadRequest, errInvalidReport)
	case errors.Is(err, bubbleversus.ErrNotYet):
		return c.JSON(http.StatusBadRequest, errNotYet)
	}
	slog.Error("bubble-game/versus: request failed", "path", c.Path(), "err", err)
	return apierr.JSONInternalError(c)
}

// bindMatchID reads {matchId}. 形が違えば 404 を書いて false を返す。
func bindMatchID(c echo.Context, req any, matchID *string) (bool, error) {
	if err := c.Bind(req); err != nil || *matchID == "" {
		return false, c.JSON(http.StatusBadRequest, errVersusInvalidP)
	}
	if !matchIDPattern.MatchString(*matchID) {
		return false, c.JSON(http.StatusNotFound, errNoSuchMatch)
	}
	return true, nil
}

// packMatch renders a match for its participants. 記録 (logs) は大きいので
// 載せず、結果は得点などの要約だけにする。
func (h *VersusHandler) packMatch(m *bubbleversus.Match, known map[string]*model.User) (map[string]any, error) {
	out := map[string]any{
		"id":        m.ID,
		"gameMode":  m.GameMode,
		"status":    m.Status,
		"createdAt": msToISO(m.CreatedAt),
		"startAt":   nil,
		"endedAt":   nil,
		"winnerId":  m.WinnerID,
		"reason":    nilIfEmpty(m.Reason),
		// シードは受けた後にだけ決まる。
		"seed": nilIfEmpty(m.Seed),
	}
	if m.StartAt != 0 {
		out["startAt"] = msToISO(m.StartAt)
	}
	if m.EndedAt != 0 {
		out["endedAt"] = msToISO(m.EndedAt)
	}
	for i, p := range m.Players {
		n := "user1"
		if i == 1 {
			n = "user2"
		}
		u, ok := known[p.UserID]
		if !ok {
			var err error
			u, err = h.users.FindByID(p.UserID)
			if err != nil && !repository.IsNotFound(err) {
				return nil, err
			}
			known[p.UserID] = u
		}
		out[n+"Id"] = p.UserID
		if u != nil {
			out[n] = entity.PackUserLite(u)
		} else {
			out[n] = nil
		}
		out[n+"Ready"] = p.Ready
		var result any
		if p.Result != nil {
			result = map[string]any{"score": p.Result.Score, "frame": p.Result.Frame, "reason": p.Result.Reason}
		}
		out[n+"Result"] = result
	}
	return out, nil
}

func msToISO(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (h *VersusHandler) respond(c echo.Context, m *bubbleversus.Match, known map[string]*model.User) error {
	packed, err := h.packMatch(m, known)
	if err != nil {
		return h.fail(c, err)
	}
	return c.JSON(http.StatusOK, packed)
}

// Invite handles POST /api/bubble-game/versus/invite.
func (h *VersusHandler) Invite(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		UserID   string `json:"userId"`
		GameMode string `json:"gameMode"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" || req.GameMode == "" {
		return c.JSON(http.StatusBadRequest, errVersusInvalidP)
	}
	target, err := h.users.FindByID(req.UserID)
	if repository.IsNotFound(err) || (err == nil && target == nil) {
		return c.JSON(http.StatusBadRequest, errNoSuchUser)
	}
	if err != nil {
		return h.fail(c, err)
	}
	// 凍結・削除済みの相手には送らない (通知の届かない招待が残るだけ)。
	if target.IsSuspended || target.IsDeleted {
		return c.JSON(http.StatusBadRequest, errNoSuchUser)
	}
	m, err := h.svc.Invite(c.Request().Context(), me, target, req.GameMode)
	if err != nil {
		return h.fail(c, err)
	}
	return h.respond(c, m, map[string]*model.User{me.ID: me, target.ID: target})
}

// Invitations handles POST /api/bubble-game/versus/invitations.
func (h *VersusHandler) Invitations(c echo.Context) error {
	me := middleware.GetUser(c)
	list, err := h.svc.Invitations(c.Request().Context(), me.ID)
	if err != nil {
		return h.fail(c, err)
	}
	known := map[string]*model.User{me.ID: me}
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		packed, err := h.packMatch(m, known)
		if err != nil {
			return h.fail(c, err)
		}
		out = append(out, packed)
	}
	return c.JSON(http.StatusOK, out)
}

type matchReq struct {
	MatchID string `json:"matchId"`
}

// Show handles POST /api/bubble-game/versus/show.
func (h *VersusHandler) Show(c echo.Context) error {
	me := middleware.GetUser(c)
	var req matchReq
	if ok, err := bindMatchID(c, &req, &req.MatchID); !ok {
		return err
	}
	m, err := h.svc.Get(c.Request().Context(), req.MatchID)
	if err == nil && m.Side(me.ID) < 0 {
		err = bubbleversus.ErrNotParticipant
	}
	if err != nil {
		return h.fail(c, err)
	}
	return h.respond(c, m, map[string]*model.User{me.ID: me})
}

// Accept handles POST /api/bubble-game/versus/accept.
func (h *VersusHandler) Accept(c echo.Context) error {
	me := middleware.GetUser(c)
	var req matchReq
	if ok, err := bindMatchID(c, &req, &req.MatchID); !ok {
		return err
	}
	m, err := h.svc.Accept(c.Request().Context(), me.ID, req.MatchID)
	if err != nil {
		return h.fail(c, err)
	}
	return h.respond(c, m, map[string]*model.User{me.ID: me})
}

// Decline handles POST /api/bubble-game/versus/decline.
func (h *VersusHandler) Decline(c echo.Context) error {
	me := middleware.GetUser(c)
	var req matchReq
	if ok, err := bindMatchID(c, &req, &req.MatchID); !ok {
		return err
	}
	if err := h.svc.Decline(c.Request().Context(), me.ID, req.MatchID); err != nil {
		return h.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// Cancel handles POST /api/bubble-game/versus/cancel.
func (h *VersusHandler) Cancel(c echo.Context) error {
	me := middleware.GetUser(c)
	var req matchReq
	if ok, err := bindMatchID(c, &req, &req.MatchID); !ok {
		return err
	}
	if err := h.svc.Cancel(c.Request().Context(), me.ID, req.MatchID); err != nil {
		return h.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// Report handles POST /api/bubble-game/versus/report.
func (h *VersusHandler) Report(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		MatchID string  `json:"matchId"`
		Score   int64   `json:"score"`
		Frame   int64   `json:"frame"`
		Reason  string  `json:"reason"`
		Logs    [][]any `json:"logs"`
		// エンジンの版 (#3232)。古いクライアントは送らないので省略できる。
		GameVersion *int `json:"gameVersion"`
	}
	if ok, err := bindMatchID(c, &req, &req.MatchID); !ok {
		return err
	}
	m, err := h.svc.SubmitReport(c.Request().Context(), me.ID, req.MatchID, bubbleversus.Report{
		Score: req.Score, Frame: req.Frame, Reason: req.Reason, Logs: req.Logs, GameVersion: req.GameVersion,
	})
	if err != nil {
		return h.fail(c, err)
	}
	return h.respond(c, m, map[string]*model.User{me.ID: me})
}
