// Package renotemute provides /api/renote-mute/* endpoints.
package renotemute

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/api/userrelation"
	coremuting "github.com/shiroha-a/mk/internal/core/muting"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// ModeratorChecker reports whether a user holds moderator privileges. Used to
// let moderator viewers see follower/following counts that would otherwise be
// gated by visibility (mirrors upstream UserEntityService iAmModerator, #1985)。
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// Handler handles renote-mute endpoints.
type Handler struct {
	svc      *coremuting.RenoteService
	userRepo repository.UserRepository
	idGen    id.Generator
	// relation は renote-mute/list の埋め込み mutee に viewer-relation flag
	// (isRenoteMuted 等) を付与する repo 束 (#1912)。未配線なら flag は omit。
	relation userrelation.Repos
	// moderator は renote-mute/list の埋め込み mutee の count gate で moderator
	// viewer を判定する (#1985)。未配線なら non-moderator 扱い。
	moderator ModeratorChecker
	// extras は一覧の利用者のピン留め・移行先をまとめて埋める (#3330)。
	extras userpack.DetailExtrasMany
}

// SetDetailExtras wires the batch filler of pinnedNotes / pinnedPage / movedTo /
// alsoKnownAs for the embedded users of renote-mute/list (#3330).
func (h *Handler) SetDetailExtras(x userpack.DetailExtrasMany) {
	h.extras = x
}

// HasDetailExtras reports whether the detail extras filler was wired.
//
// 未配線だと renote-mute/list の利用者の pinnedNotes などが空、movedTo / alsoKnownAs が
// null のまま返る。起動時検査に使う。
func (h *Handler) HasDetailExtras() bool { return h.extras != nil }

// SetRelationRepos wires the repositories used to populate viewer-relation flags
// on the embedded mutee in renote-mute/list (#1912)。
func (h *Handler) SetRelationRepos(r userrelation.Repos) {
	h.relation = r
}

// SetModeratorChecker wires the moderator check used by renote-mute/list's count
// gate so a moderator viewer keeps seeing follower/following counts (#1985)。
func (h *Handler) SetModeratorChecker(m ModeratorChecker) {
	h.moderator = m
}

// NewHandler creates a new Handler.
// userRepo / idGen are required by renote-mute/list to embed the
// `mutee` user object that the upstream Misskey frontend renders.
func NewHandler(svc *coremuting.RenoteService, userRepo repository.UserRepository, idGen id.Generator) *Handler {
	return &Handler{svc: svc, userRepo: userRepo, idGen: idGen}
}

// PairRequest is the body for create/delete.
type PairRequest struct {
	UserID string `json:"userId"`
}

// Create handles POST /api/renote-mute/create.
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req PairRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if _, err := h.svc.Mute(user.ID, req.UserID); err != nil {
		switch {
		case errors.Is(err, coremuting.ErrSelfMute):
			return c.JSON(http.StatusBadRequest, apierr.Error("MUTEE_IS_YOURSELF", "You cannot mute yourself.", "37285718-52f7-4aef-b7de-c38b8e8a8420"))
		case errors.Is(err, coremuting.ErrMuteeNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "5e0a5dff-1e94-4202-87ae-4d9c89eb2271"))
		case errors.Is(err, coremuting.ErrAlreadyMuting):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_MUTING", "You are already muting that user.", "ccfecbe4-1f1c-4fc2-8a3d-c3ffee61cb7b"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// Delete handles POST /api/renote-mute/delete.
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req PairRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Unmute(user.ID, req.UserID); err != nil {
		switch {
		case errors.Is(err, coremuting.ErrSelfMute):
			return c.JSON(http.StatusBadRequest, apierr.Error("MUTEE_IS_YOURSELF", "You cannot unmute yourself.", "619b1314-0850-4597-a242-e245f3da42af"))
		case errors.Is(err, coremuting.ErrNotMuting):
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_MUTING", "You are not muting that user.", "2e4ef874-8bf0-4b4b-b069-4598f6d05817"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// ListRequest is the body for renote-mute/list.
type ListRequest struct {
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// List handles POST /api/renote-mute/list.
//
// frontend Paginator (cursor mode) は untilId / sinceId を forward する。
// 本 endpoint は #493 で cursor 対応に切り替えた。
func (h *Handler) List(c echo.Context) error {
	user := middleware.GetUser(c)
	var req ListRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 30, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.svc.List(user.ID, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	muteeMap := h.fetchMuteeMap(c.Request().Context(), user, rows)
	const tsFormat = "2006-01-02T15:04:05.000Z"
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		createdAt := ""
		if h.idGen != nil {
			if t, err := h.idGen.ParseTime(m.ID); err == nil {
				createdAt = t.UTC().Format(tsFormat)
			}
		}
		entry := map[string]any{
			"id":        m.ID,
			"createdAt": createdAt,
			"muteeId":   m.MuteeID,
		}
		if mutee, ok := muteeMap[m.MuteeID]; ok {
			entry["mutee"] = mutee
		}
		out = append(out, entry)
	}
	return c.JSON(http.StatusOK, out)
}

// fetchMuteeMap batches user + profile lookups for the muteeIds in rows so
// the response build loop performs zero per-row DB queries.
func (h *Handler) fetchMuteeMap(ctx context.Context, viewer *model.User, rows []*model.RenoteMuting) map[string]entity.UserDetailed {
	if h.userRepo == nil || len(rows) == 0 || viewer == nil {
		return nil
	}
	viewerID := viewer.ID
	ids := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, m := range rows {
		if _, ok := seen[m.MuteeID]; ok {
			continue
		}
		seen[m.MuteeID] = struct{}{}
		ids = append(ids, m.MuteeID)
	}
	users, err := h.userRepo.FindManyByIDs(ids)
	if err != nil {
		return nil
	}
	profiles, profErr := h.userRepo.FindProfilesByUserIDs(ids)
	if profErr != nil {
		slog.Warn("renote-mute/list: failed to fetch profiles", "error", profErr)
	}
	profileByUser := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		profileByUser[p.UserID] = p
	}
	iAmModerator := h.moderator != nil && viewerID != "" && h.moderator.IsModerator(viewerID)
	packed := make([]entity.UserDetailed, len(users))
	for i, u := range users {
		d := entity.PackUserDetailed(u, profileByUser[u.ID], h.idGen)
		// viewer-relation flag を付与 (#1912)。renote-mute 一覧なので
		// isRenoteMuted=true が出る。
		viewerIsFollowing := h.relation.Apply(&d, viewerID, u, profileByUser[u.ID])
		// followers-only count を非フォロワーに leak させない (upstream packMany(_, me) の
		// count gate、#1985)。mutee は viewer 自身ではないため isMe は常に false。
		entity.GateCountVisibility(&d, u.ID == viewerID, iAmModerator, viewerIsFollowing)
		packed[i] = d
	}
	// 本家 RenoteMutingEntityService は相手を packMany (UserDetailedNotMe) で組むので、ピン留めと
	// 移行先もまとめて埋める (#3330)。
	if h.extras != nil && viewer != nil {
		targets := make([]userpack.DetailTarget, 0, len(packed))
		for i, u := range users {
			targets = append(targets, userpack.DetailTarget{User: u, Profile: profileByUser[u.ID], Detailed: &packed[i]})
		}
		h.extras.FillDetailedExtrasMany(ctx, viewer, targets)
	}
	out := make(map[string]entity.UserDetailed, len(users))
	for i, u := range users {
		out[u.ID] = packed[i]
	}
	return out
}
