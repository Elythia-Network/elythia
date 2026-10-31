// Package blocking provides /api/blocking/* endpoints.
package blocking

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// ModeratorChecker reports whether a user holds moderator privileges. Used to
// let moderator viewers see follower/following counts that would otherwise be
// gated by visibility (mirrors upstream UserEntityService iAmModerator, #1985)。
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// Handler handles blocking-related API endpoints.
type Handler struct {
	svc      *coreblocking.Service
	userRepo repository.UserRepository
	idGen    id.Generator
	relation userrelation.Repos
	// moderator は blocking/list の埋め込み blockee の count gate で moderator
	// viewer を判定する (#1985)。未配線なら non-moderator 扱い。
	moderator ModeratorChecker
	// packer は create / delete の応答を本家の UserDetailedNotMe と同じ形
	// (instance・絵文字・ピン留め・移行先・モデレーター向けの項目) に組む (#3330)。
	packer UserPacker
	// extras は一覧の利用者のピン留め・移行先をまとめて埋める (#3330)。
	extras userpack.DetailExtrasMany
	// entityPacker は利用者や投稿を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3792)。組み立てで渡す。
	entityPacker *entity.Packer
}

// UserPacker packs a user as UserDetailedNotMe seen by viewer.
// *userpack.Packer satisfies it.
type UserPacker interface {
	DetailedNotMe(ctx context.Context, target, viewer *model.User) (entity.UserDetailed, bool)
}

// NewHandler creates a new blocking Handler.
// userRepo / idGen are required by blocking/list to embed the
// `blockee` user object that the upstream Misskey frontend renders.
// entityPacker is the Packer this handler packs entities with (#3792); extras
// fills pinnedNotes / pinnedPage / movedTo / alsoKnownAs for the embedded users
// of blocking/list (#3330); packer builds the create / delete response as the
// upstream UserDetailedNotMe (#3330).
func NewHandler(svc *coreblocking.Service, userRepo repository.UserRepository, idGen id.Generator, entityPacker *entity.Packer, extras userpack.DetailExtrasMany, packer UserPacker) *Handler {
	return &Handler{svc: svc, userRepo: userRepo, idGen: idGen, entityPacker: entityPacker, extras: extras, packer: packer}
}

// SetRelationRepos wires the repositories used to compute the viewer->blockee
// relation block (isBlocking etc.) returned by create / delete (#1802). When
// unset the relation flags are simply omitted (= legacy 204-ish behavior for
// test fixtures).
func (h *Handler) SetRelationRepos(r userrelation.Repos) {
	h.relation = r
}

// SetModeratorChecker wires the moderator check used by blocking/list's count
// gate so a moderator viewer keeps seeing follower/following counts (#1985)。
func (h *Handler) SetModeratorChecker(m ModeratorChecker) {
	h.moderator = m
}

// PairRequest is the request body for blocking/create and blocking/delete.
type PairRequest struct {
	UserID string `json:"userId"`
}

// Create handles POST /api/blocking/create.
//
// upstream Misskey TS と同じく成功時は 200 + 対象 blockee の UserDetailed
// (= isBlocking=true 反映) を返す。frontend (vue) が即時 redraw するため
// 必要 (#870)。userRepo が wire されない test stub では legacy 互換で
// 204 No Content にフォールバック。
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req PairRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}

	if _, err := h.svc.Block(user.ID, req.UserID); err != nil {
		switch {
		case errors.Is(err, coreblocking.ErrSelfBlock):
			return c.JSON(http.StatusBadRequest, apierr.Error("BLOCKEE_IS_YOURSELF", "Blockee is yourself.", "88b19138-f28d-42c0-8499-6a31bbd0fdc6"))
		case errors.Is(err, coreblocking.ErrBlockeeNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "7cc4f851-e2f1-4621-9633-ec9e1d00c01e"))
		case errors.Is(err, coreblocking.ErrAlreadyBlocking):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_BLOCKING", "You are already blocking that user.", "787fed64-acb9-464a-82eb-afbd745b9614"))
		}
		return apierr.JSONInternalError(c)
	}
	return h.respondPackedUser(c, user, req.UserID)
}

// Delete handles POST /api/blocking/delete.
//
// 同じく upstream TS は 200 + UserDetailed (isBlocking=false 反映) を返す。
// userRepo 未 wire 時は 204 No Content フォールバック (#870)。
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req PairRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Unblock(user.ID, req.UserID); err != nil {
		switch {
		case errors.Is(err, coreblocking.ErrSelfBlock):
			return c.JSON(http.StatusBadRequest, apierr.Error("BLOCKEE_IS_YOURSELF", "Blockee is yourself.", "06f6fac6-524b-473c-a354-e97a40ae6eac"))
		case errors.Is(err, coreblocking.ErrBlockeeNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "8621d8bf-c358-4303-a066-5ea78610eb3f"))
		case errors.Is(err, coreblocking.ErrNotBlocking):
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_BLOCKING", "You are not blocking that user.", "291b2efa-60c6-45c0-9f6a-045c8f9b02cd"))
		}
		return apierr.JSONInternalError(c)
	}
	return h.respondPackedUser(c, user, req.UserID)
}

// respondPackedUser fetches the target user + profile via userRepo and writes
// a 200 + UserDetailed response carrying the viewer->target relation block
// (isBlocking etc.), matching upstream pack(blockee, blocker, UserDetailedNotMe)
// (#1802). userRepo 未 wire (= 既存 test stub) なら legacy 204 を返す互換 path に
// 落ちる (production では router で必ず wire されるので影響なし、#870)。lookup
// 失敗時も best-effort 204 にフォールバックして block / unblock の副作用 (= state
// 反映) を尊重する。relation block は h.relation 経由で viewer (= blocker) との関係を
// 解決するため、block 後は isBlocking=true / unblock 後は isBlocking=false が乗る。
func (h *Handler) respondPackedUser(c echo.Context, viewer *model.User, userID string) error {
	if h.userRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	target, err := h.userRepo.FindByID(userID)
	if err != nil || target == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// 本家は pack(blockee, blocker, {schema: 'UserDetailedNotMe'}) を返す。
	//
	// **profile を読めないときは従来の組み方に落とさない。** profile 無しで組むと
	// followersVisibility が既定の public に倒れ、伏せるべきカウントが出る。本家は
	// findOneByOrFail が例外を投げて 500 になる (ブロック / 解除そのものは済んで
	// いる) ので、同じく INTERNAL_ERROR を返す。
	if viewer != nil && h.packer != nil {
		detailed, ok := h.packer.DetailedNotMe(c.Request().Context(), target, viewer)
		if !ok {
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusOK, detailed)
	}
	profile, _ := h.userRepo.FindProfileByUserID(userID)
	detailed := h.entityPacker.PackUserDetailed(target, profile, h.idGen)
	if viewer != nil {
		viewerIsFollowing := h.relation.Apply(&detailed, viewer.ID, target, profile)
		// followers-only count を非フォロワーに leak させない (upstream blocking/create・delete も
		// pack(blockee, blocker, UserDetailedNotMe) で count gate を通す、#1985)。block 自己不可
		// なので isMe は常に false。
		iAmModerator := h.moderator != nil && h.moderator.IsModerator(viewer.ID)
		entity.GateCountVisibility(&detailed, false, iAmModerator, viewerIsFollowing)
	}
	return c.JSON(http.StatusOK, detailed)
}

// ListRequest is the request body for blocking/list.
//
// 本家の paramDef に offset は無く、送られても無視して先頭から返すので読まない。
// limit / sinceDate / untilDate は、本家(ajv の integer)と同じく値が整数の
// 小数(10.0)を受ける (#3760)。
type ListRequest struct {
	Limit     *jsnum.Int `json:"limit"`
	SinceID   string     `json:"sinceId"`
	UntilID   string     `json:"untilId"`
	SinceDate *jsnum.Int `json:"sinceDate"`
	UntilDate *jsnum.Int `json:"untilDate"`
}

// List handles POST /api/blocking/list.
//
// frontend Paginator (cursor mode) は untilId / sinceId を forward する。
// 本 endpoint は #493 で cursor 対応に切り替えた。
func (h *Handler) List(c echo.Context) error {
	user := middleware.GetUser(c)
	var req ListRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 30, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursorIDFirst(req.SinceID, req.UntilID, req.SinceDate.Int64Ptr(), req.UntilDate.Int64Ptr())
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.svc.List(user.ID, sinceID, untilID, limit)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// upstream frontend が item.blockee で MkUserCardMini を描画するので
	// batch fetch で N+1 を回避しつつ user object を embed する。
	blockeeMap := h.fetchBlockeeMap(c.Request().Context(), user, rows)
	const tsFormat = "2006-01-02T15:04:05.000Z"
	out := make([]map[string]any, 0, len(rows))
	for _, b := range rows {
		createdAt := ""
		if h.idGen != nil {
			if t, err := h.idGen.ParseTime(b.ID); err == nil {
				createdAt = t.UTC().Format(tsFormat)
			}
		}
		entry := map[string]any{
			"id":        b.ID,
			"createdAt": createdAt,
			"blockeeId": b.BlockeeID,
		}
		if blockee, ok := blockeeMap[b.BlockeeID]; ok {
			entry["blockee"] = blockee
		}
		out = append(out, entry)
	}
	return c.JSON(http.StatusOK, out)
}

// fetchBlockeeMap batches user + profile lookups for the blockee IDs in
// rows so the response build loop performs zero per-row DB queries.
// viewerID は embed する blockee に viewer->blockee の relation block
// (isBlocking=true 等) を付与するために使う。mute/list / renote-mute/list と
// 揃える (upstream BlockingEntityService.packMany が UserDetailedNotMe + me で
// pack するため、#1957-a)。
func (h *Handler) fetchBlockeeMap(ctx context.Context, viewer *model.User, rows []*model.Blocking) map[string]entity.UserDetailed {
	if h.userRepo == nil || len(rows) == 0 || viewer == nil {
		return nil
	}
	viewerID := viewer.ID
	ids := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, b := range rows {
		if _, ok := seen[b.BlockeeID]; ok {
			continue
		}
		seen[b.BlockeeID] = struct{}{}
		ids = append(ids, b.BlockeeID)
	}
	users, err := h.userRepo.FindManyByIDs(ids)
	if err != nil {
		return nil
	}
	profiles, profErr := h.userRepo.FindProfilesByUserIDs(ids)
	if profErr != nil {
		// 部分結果を返す graceful degradation だが、log は残しておく。
		slog.Warn("blocking/list: failed to fetch profiles", "error", profErr)
	}
	profileByUser := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		profileByUser[p.UserID] = p
	}
	iAmModerator := h.moderator != nil && viewerID != "" && h.moderator.IsModerator(viewerID)
	packed := make([]entity.UserDetailed, len(users))
	for i, u := range users {
		d := h.entityPacker.PackUserDetailed(u, profileByUser[u.ID], h.idGen)
		// viewer->blockee の relation block を付与 (isBlocking=true 等)。Apply は
		// viewerID 空 / self では no-op。relation 未配線 (test stub) でも安全。
		viewerIsFollowing := h.relation.Apply(&d, viewerID, u, profileByUser[u.ID])
		// followers-only count を非フォロワーに leak させない (upstream packMany(_, me) の
		// count gate、#1985)。blockee は viewer 自身ではないため isMe は常に false。
		entity.GateCountVisibility(&d, u.ID == viewerID, iAmModerator, viewerIsFollowing)
		packed[i] = d
	}
	// 本家 BlockingEntityService は相手を packMany (UserDetailedNotMe) で組むので、ピン留めと
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
