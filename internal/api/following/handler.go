// Package following implements the following/* API endpoints.
package following

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// Handler handles following-related API endpoints.
type Handler struct {
	followingService *corefollowing.Service
	userService      *coreuser.Service
	// idGen は /api/following/list の Following.createdAt 生成で使う。nil
	// なら createdAt は空文字列で返す (= 互換性は維持しつつ degrade)。
	idGen id.Generator
	// packer は following/list の埋め込み followee (UserDetailedNotMe) と
	// following/requests/* の follower / followee (UserLite) を本家 packMany と
	// 同じ形に組む (#1912、#3330)。未配線なら関係・ピン留め・instance などを欠く。
	packer userpack.ListPacker
}

// SetListPacker wires the packer of the users embedded in following/list and
// following/requests/{list,sent} (#1912, #3330).
func (h *Handler) SetListPacker(p userpack.ListPacker) {
	h.packer = p
}

// HasListPacker reports whether the list packer was wired.
//
// 未配線だと following/list の followee に関係・ピン留め・移行先・instance・絵文字が
// 載らず、following/requests/* の利用者に instance・絵文字が載らない。起動時検査に使う。
func (h *Handler) HasListPacker() bool { return h.packer != nil }

// NewHandler creates a new following Handler.
func NewHandler(followingService *corefollowing.Service, userService *coreuser.Service) *Handler {
	return &Handler{followingService: followingService, userService: userService}
}

// SetIDGen wires an id.Generator so /api/following/list can derive Following.createdAt
// from aidx-encoded row IDs. router.go から noteCreateService 等と同じく注入する。
func (h *Handler) SetIDGen(g id.Generator) {
	h.idGen = g
}

// CreateRequest is the request body for following/create.
type CreateRequest struct {
	UserID      string `json:"userId"`
	WithReplies *bool  `json:"withReplies"`
}

// Create handles POST /api/following/create.
func (h *Handler) Create(c echo.Context) error {
	me := middleware.GetUser(c)

	var req CreateRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}

	// followeeを先に取得し、エラー時はNO_SUCH_USERを返す
	bundle, err := h.userService.ShowByID(req.UserID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "fcd2eef9-a9b2-4c4f-8624-038099e90aa5"))
	}

	// withReplies 引数を Service に伝搬する (#1056)。upstream Misskey TS の
	// /api/following/create は follow 作成時に withReplies 初期値を受け取り、
	// Following row (locked followee の場合は FollowRequest row) に保存する。
	opts := corefollowing.FollowOptions{}
	if req.WithReplies != nil {
		opts.WithReplies = *req.WithReplies
	}
	if _, err := h.followingService.Follow(me.ID, req.UserID, opts); err != nil {
		switch {
		case errors.Is(err, corefollowing.ErrSelfFollow):
			return c.JSON(http.StatusBadRequest, apierr.Error("FOLLOWEE_IS_YOURSELF", "Followee is yourself.", "26fbe7bb-a331-4857-af17-205b426669a9"))
		case errors.Is(err, corefollowing.ErrAlreadyFollowing), errors.Is(err, corefollowing.ErrAlreadyRequested):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_FOLLOWING", "You are already following that user.", "35387507-38c7-4cb9-9197-300b93783fa0"))
		case errors.Is(err, corefollowing.ErrBlocking):
			// follower が followee を block している方向は BLOCKED ではなく
			// BLOCKING (upstream create.ts の blocking エラー、#1562)。
			return c.JSON(http.StatusBadRequest, apierr.Error("BLOCKING", "You are blocking that user.", "4e2206ec-aa4f-4960-b865-6c23ac38e2d9"))
		case errors.Is(err, corefollowing.ErrBlocked):
			// upstream Misskey TS の following/create は BLOCKED を 400 で
			// 返す (kind: 'client')。旧 mk-go は 403 で返していたが drop-in
			// 互換のため 400 に揃える (#872)。
			return c.JSON(http.StatusBadRequest, apierr.Error("BLOCKED", "You are blocked by that user.", "c4ab57cc-4e41-45e9-bfd9-584f61e35ce0"))
		default:
			return apierr.JSONInternalError(c)
		}
	}

	return c.JSON(http.StatusOK, entity.PackUserLite(bundle.User))
}

// DeleteRequest is the request body for following/delete.
type DeleteRequest struct {
	UserID string `json:"userId"`
}

// Delete handles POST /api/following/delete.
func (h *Handler) Delete(c echo.Context) error {
	me := middleware.GetUser(c)

	var req DeleteRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}

	bundle, err := h.userService.ShowByID(req.UserID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "5b12c78d-2b28-4dca-99d2-f56139b42ff8"))
	}

	if err := h.followingService.Unfollow(me.ID, req.UserID); err != nil {
		switch {
		case errors.Is(err, corefollowing.ErrSelfFollow):
			return c.JSON(http.StatusBadRequest, apierr.Error("FOLLOWEE_IS_YOURSELF", "Followee is yourself.", "d9e400b9-36b0-4808-b1d8-79e707f1296c"))
		case errors.Is(err, corefollowing.ErrNotFollowing):
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_FOLLOWING", "You are not following that user.", "5dbf82f5-c92b-40b1-87d1-6c8c0741fd09"))
		default:
			return apierr.JSONInternalError(c)
		}
	}

	return c.JSON(http.StatusOK, entity.PackUserLite(bundle.User))
}

// RequestActionRequest is the shared request body for following/requests/{accept,reject,cancel}.
type RequestActionRequest struct {
	UserID string `json:"userId"`
}

// AcceptRequest handles POST /api/following/requests/accept.
func (h *Handler) AcceptRequest(c echo.Context) error {
	me := middleware.GetUser(c)

	var req RequestActionRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}

	// upstream accept.ts は getUser を先に呼び、userId が無効なら NO_SUCH_USER
	// (id 66ce1645) を返す。request 不在は別 error NO_FOLLOW_REQUEST (bcde4f8b)
	// で区別する (#1911)。
	if _, err := h.userService.ShowByID(req.UserID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "66ce1645-d66c-46bb-8b79-96739af885bd"))
	}
	if err := h.followingService.AcceptRequest(me.ID, req.UserID); err != nil {
		switch {
		case errors.Is(err, corefollowing.ErrRequestNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_FOLLOW_REQUEST", "No such follow request.", "bcde4f8b-0913-4614-8881-614e522fb041"))
		case errors.Is(err, corefollowing.ErrBlocking):
			// mk-go 独自の多層防御 (upstream の accept.ts には無い、#1562 の
			// BLOCKING と同じ code だが occurrence が別なので id は別)。
			return c.JSON(http.StatusBadRequest, apierr.Error("BLOCKING", "You are blocking that user.", "b155c6b3-83e9-400a-89c1-4a521da65240"))
		case errors.Is(err, corefollowing.ErrBlocked):
			return c.JSON(http.StatusBadRequest, apierr.Error("BLOCKED", "You are blocked by that user.", "25d1906c-9e73-49c0-85d1-f48d2520cd60"))
		default:
			return apierr.JSONInternalError(c)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// RejectRequest handles POST /api/following/requests/reject.
func (h *Handler) RejectRequest(c echo.Context) error {
	me := middleware.GetUser(c)

	var req RequestActionRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}

	// upstream reject.ts は getUser を先に呼び、userId が無効なら NO_SUCH_USER
	// (id abc2ffa6) を返す。reject.ts に NO_FOLLOW_REQUEST 系 error は無い (#1911)。
	if _, err := h.userService.ShowByID(req.UserID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "abc2ffa6-25b2-4380-ba99-321ff3a94555"))
	}
	// request 不在は upstream removeFollowRequest が silent return するため成功
	// 扱い (204 No Content)。404 にしない (#1911)。
	if err := h.followingService.RejectRequest(me.ID, req.UserID); err != nil {
		if errors.Is(err, corefollowing.ErrRequestNotFound) {
			return c.NoContent(http.StatusNoContent)
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// CancelRequest handles POST /api/following/requests/cancel.
//
// upstream requests/cancel.ts は followee を getUser で先に解決して
// NO_SUCH_USER を返し、request 不在は FOLLOW_REQUEST_NOT_FOUND (service 内部
// error とは別の公開 id) に変換し、成功時は packed followee を返す (#1562)。
func (h *Handler) CancelRequest(c echo.Context) error {
	me := middleware.GetUser(c)

	var req RequestActionRequest
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}

	bundle, err := h.userService.ShowByID(req.UserID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "4e68c551-fc4c-4e46-bb41-7d4a37bf9dab"))
	}

	if err := h.followingService.CancelRequest(me.ID, req.UserID); err != nil {
		if errors.Is(err, corefollowing.ErrRequestNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("FOLLOW_REQUEST_NOT_FOUND", "Follow request not found.", "089b125b-d338-482a-9a09-e2622ac9f8d4"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, entity.PackUserLite(bundle.User))
}

// ListRequestsResponseItem represents a single received follow request.
type ListRequestsResponseItem struct {
	ID       string          `json:"id"`
	Follower entity.UserLite `json:"follower"`
	Followee entity.UserLite `json:"followee"`
}

// ListRequest is the request body for /api/following/list (upstream 2026.5.2
// #17385 + #17416)。`sinceDate` / `untilDate` は upstream paramDef にあるが
// mk-go の他 endpoint と同じく現状未対応で、bind は受けるが silent ignore
// (= 上流仕様の subset、別 issue で全 endpoint 横断対応)。
type ListRequest struct {
	Notification bool   `json:"notification"`
	SinceID      string `json:"sinceId"`
	UntilID      string `json:"untilId"`
	SinceDate    *int64 `json:"sinceDate"`
	UntilDate    *int64 `json:"untilDate"`
	Limit        *int   `json:"limit"`
}

// List handles POST /api/following/list (upstream 2026.5.2 #17385 + #17416)。
// 自分が follow している人の一覧を Following[] で返す (populateFollowee=true 相当)。
// notification=true で `notify IS NOT NULL` 絞り込み。kind は read:following。
func (h *Handler) List(c echo.Context) error {
	me := middleware.GetUser(c)
	var req ListRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate (Unix ms) を aidx prefix に正規化して sinceID /
	// untilID と同 SQL cursor で扱う (#1166)。sinceID が指定されている場合
	// upstream 同様 sinceDate は無視される。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}

	rows, err := h.followingService.ListFollowingForList(me.ID, sinceID, untilID, req.Notification, limit)
	if err != nil {
		return apierr.JSONInternalError(c)
	}

	// followee を batch 取得して N+1 を避ける (upstream packMany と同 pattern)。
	followeeIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		followeeIDs = append(followeeIDs, r.FolloweeID)
	}
	bundles, _ := h.userService.ShowManyByIDs(followeeIDs)
	users := make([]*model.User, 0, len(bundles))
	profiles := make(map[string]*model.UserProfile, len(bundles))
	for _, b := range bundles {
		if b != nil && b.User != nil {
			users = append(users, b.User)
			profiles[b.User.ID] = b.Profile
		}
	}
	// 埋め込み followee は本家 FollowingEntityService.packMany と同じく
	// packMany(followees, me) で組む。関係 (#1912。自分の following 一覧なので
	// isFollowing=true)、カウントのゲート、ピン留め・移行先・instance・絵文字を
	// まとめて埋める (#3330)。自分はフォローできないので MeDetailed は混ざらない。
	followees := h.packDetailed(c, me, users, profiles)
	packedByID := make(map[string]*entity.UserDetailed, len(users))
	for i, u := range users {
		packedByID[u.ID] = &followees[i]
	}
	lookup := func(string) (*model.User, *model.UserProfile) { return nil, nil }

	out := make([]entity.Following, 0, len(rows))
	for _, r := range rows {
		packed := entity.PackFollowing(r, false, false, lookup, h.idGen)
		packed.Followee = packedByID[r.FolloweeID]
		out = append(out, packed)
	}
	return c.JSON(http.StatusOK, out)
}

// packDetailed packs users as upstream packMany(users, viewer) through the
// shared list packer. Without one it falls back to the bare UserDetailed
// (packer の既定で非公開のカウントは伏せたまま)。
func (h *Handler) packDetailed(c echo.Context, viewer *model.User, users []*model.User, profiles map[string]*model.UserProfile) []entity.UserDetailed {
	if h.packer != nil {
		return h.packer.DetailedMany(c.Request().Context(), viewer, users, profiles)
	}
	out := make([]entity.UserDetailed, len(users))
	for i, u := range users {
		out[i] = entity.PackUserDetailed(u, profiles[u.ID], h.idGen)
	}
	return out
}

// packRequests packs follow requests as upstream FollowRequestEntityService.
// packMany: follower / followee are UserLite packed in one batch.
//
// 旧実装は行ごとに ShowByID を 2 回呼び、instance と絵文字も埋めていなかった (#3330)。
func (h *Handler) packRequests(rows []*model.FollowRequest) []ListRequestsResponseItem {
	ids := make([]string, 0, len(rows)*2)
	seen := make(map[string]struct{}, len(rows)*2)
	for _, r := range rows {
		for _, uid := range []string{r.FollowerID, r.FolloweeID} {
			if _, ok := seen[uid]; ok {
				continue
			}
			seen[uid] = struct{}{}
			ids = append(ids, uid)
		}
	}
	bundles, _ := h.userService.ShowManyByIDs(ids)
	lites := make([]entity.UserLite, 0, len(bundles))
	users := make([]*model.User, 0, len(bundles))
	for _, b := range bundles {
		if b != nil && b.User != nil {
			lites = append(lites, entity.PackUserLite(b.User))
			users = append(users, b.User)
		}
	}
	if h.packer != nil {
		ptrs := make([]*entity.UserLite, len(users))
		for i := range users {
			ptrs[i] = &lites[i]
		}
		h.packer.FillLites(users, ptrs)
	}
	byID := make(map[string]entity.UserLite, len(users))
	for i, u := range users {
		byID[u.ID] = lites[i]
	}
	out := make([]ListRequestsResponseItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, ListRequestsResponseItem{ID: r.ID, Follower: byID[r.FollowerID], Followee: byID[r.FolloweeID]})
	}
	return out
}

// ListRequests handles POST /api/following/requests/list.
func (h *Handler) ListRequests(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}

	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	requests, err := h.followingService.ListReceivedRequests(me.ID, limit, sinceID, untilID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}

	return c.JSON(http.StatusOK, h.packRequests(requests))
}
