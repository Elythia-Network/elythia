package userlists

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"

	"github.com/shiroha-a/mk/internal/core/role"
)

// Handler handles users/lists API endpoints.
type Handler struct {
	repo               repository.UserListRepository
	idGen              id.Generator
	rolePolicyProvider role.PolicyProvider
	userRepo           repository.UserRepository
	blockingRepo       repository.BlockingRepository
	favoriteRepo       UserListFavoriteReader
	proxyFollow        ProxyFollowEnqueuer
	userListEventPub   UserListEventPublisher
}

// UserListFavoriteReader abstracts the favorite count / exists lookup for
// users/lists/show forPublic の likedCount/isLiked (#1550)。実装は
// repository.UserListFavoriteRepository。
type UserListFavoriteReader interface {
	CountByList(listID string) (int64, error)
	Exists(userID, listID string) (bool, error)
}

// NewHandler creates a new userlists Handler.
func NewHandler(repo repository.UserListRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, idGen: idGen}
}

// SetRolePolicyProvider wires a role policy source so Create / Push enforce
// the userListLimit / userEachUserListsLimit role policies (#1029).
func (h *Handler) SetRolePolicyProvider(p role.PolicyProvider) {
	h.rolePolicyProvider = p
}

// SetUserRepo wires a UserRepository so Push can validate the target user's
// existence (NO_SUCH_USER) before AddMember, matching upstream
// users/lists/push GetterService.getUser semantics (#1550).
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// SetBlockingRepo wires a BlockingRepository so Push can reject adding a user
// who has blocked the caller (YOU_HAVE_BEEN_BLOCKED), matching upstream
// users/lists/push blocking check (#1550). Fail-closed: a repo error is
// treated as "blocked" to avoid leaking the membership against the blocker's
// intent.
func (h *Handler) SetBlockingRepo(r repository.BlockingRepository) {
	h.blockingRepo = r
}

// SetFavoriteRepo wires a favorite reader so Show can return likedCount/isLiked
// for forPublic public lists (#1550). nil 時は likedCount/isLiked を付与しない。
func (h *Handler) SetFavoriteRepo(r UserListFavoriteReader) {
	h.favoriteRepo = r
}

// ProxyFollowEnqueuer makes the proxy account follow remote users added to a
// list so their posts federate to this instance (#1704)。実装は
// core/userlist.ProxyFollower。
type ProxyFollowEnqueuer interface {
	EnqueueProxyFollow(remoteUserIDs []string)
}

// SetProxyFollow wires the proxy-follow enqueuer used when a remote user is
// pushed to a list (#1704). nil 時は proxy follow を skip する。
func (h *Handler) SetProxyFollow(p ProxyFollowEnqueuer) {
	h.proxyFollow = p
}

// UserListEventPublisher publishes userAdded/userRemoved membership events to a
// list's stream so the userList WS channel updates members live (#1549)。実装は
// stream.UserListStreamPublisher。
type UserListEventPublisher interface {
	PublishUserListEvent(listID, eventType string, body any)
}

// SetUserListEventPublisher wires the membership event publisher used by
// push/pull (#1549). nil 時は membership event を発火しない。
func (h *Handler) SetUserListEventPublisher(p UserListEventPublisher) {
	h.userListEventPub = p
}

// List handles POST /api/users/lists/list.
//
// upstream list.ts は requireCredential:false で、userId(optional) 指定時は対象
// ユーザーの public list を返す (host!=null は REMOTE_USER_NOT_ALLOWED、不在は
// NO_SUCH_USER)。userId 未指定 && 未認証は INVALID_PARAM (#1550)。packed shape
// ({id, createdAt, name, userIds, isPublic})、userIds は ListMembersByListIDs で
// batch fetch (#876)。
func (h *Handler) List(c echo.Context) error {
	viewer := middleware.GetUser(c)
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil { // userId は optional (upstream paramDef required:[])
		return apierr.JSONInvalidParam(c)
	}

	var lists []*model.UserList
	if req.UserID != "" {
		// 対象ユーザーの public list を返す (upstream: findBy {userId, isPublic:true})。
		if h.userRepo != nil {
			u, uerr := h.userRepo.FindByID(req.UserID)
			if uerr != nil && !repository.IsNotFound(uerr) {
				// **DB 障害を not-found に丸めない** (#2792)。
				return apierr.JSONInternalError(c)
			}
			if uerr != nil || u == nil {
				return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "a8af4a82-0980-4cc4-a6af-8b0ffd54465e"))
			}
			if u.Host != nil && *u.Host != "" {
				return c.JSON(http.StatusBadRequest, apierr.Error("REMOTE_USER_NOT_ALLOWED", "Not allowed to load the remote user's list", "53858f1b-3315-4a01-81b7-db9b48d4b79a"))
			}
		}
		all, err := h.repo.ListByUser(req.UserID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		for _, l := range all {
			if l.IsPublic {
				lists = append(lists, l)
			}
		}
	} else {
		// userId 未指定は認証必須 (upstream: me===null なら INVALID_PARAM)。
		if viewer == nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid param.", "ab36de0e-29e9-48cb-9732-d82f1281620d"))
		}
		var err error
		lists, err = h.repo.ListByUser(viewer.ID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
	}

	listIDs := make([]string, 0, len(lists))
	for _, l := range lists {
		listIDs = append(listIDs, l.ID)
	}
	membersByList, err := h.repo.ListMembersByListIDs(listIDs)
	if err != nil {
		// repo error は best-effort で空 map にフォールバックして shape を保つ
		// (= memberIDs helper と同 pattern、entity.PackUserList が [] で
		// serialize する)。観測のため slog.Warn は残す。
		slog.Warn("users/lists/list: failed to batch fetch members", "error", err)
		membersByList = map[string][]string{}
	}
	out := make([]entity.UserList, 0, len(lists))
	for _, l := range lists {
		out = append(out, entity.PackUserList(l, membersByList[l.ID], h.idGen))
	}
	return c.JSON(http.StatusOK, out)
}

// Create handles POST /api/users/lists/create.
//
// 新規作成時は member 0 件なので userIds は []。upstream と同じ packed
// shape を返す (#871)。
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// userListLimit role policy gate (#1029)。
	if h.rolePolicyProvider != nil {
		if limit, ok := role.PolicyNumber(h.rolePolicyProvider.GetUserPolicies(user.ID)["userListLimit"]); ok && limit >= 0 {
			count, err := h.repo.CountByUser(user.ID)
			if err != nil {
				return apierr.JSONInternalError(c)
			}
			if float64(count) >= limit {
				return apierr.JSONTooManyUserLists(c)
			}
		}
	}
	list := &model.UserList{
		ID:     h.idGen.Generate(time.Now()),
		UserID: user.ID,
		Name:   req.Name,
	}
	if err := h.repo.Create(list); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, entity.PackUserList(list, nil, h.idGen))
}

// Show handles POST /api/users/lists/show.
//
// userIds を含む packed shape を返す (#871)。
func (h *Handler) Show(c echo.Context) error {
	var req struct {
		ListID    string `json:"listId"`
		ForPublic bool   `json:"forPublic"`
	}
	if err := c.Bind(&req); err != nil || req.ListID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "listId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	list, err := h.repo.FindByID(req.ListID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "7bc05c21-1d7a-41ae-88f1-66820f4dc686"))
	}
	// upstream show.ts は forPublic で selection を切り替える (#1948-15):
	//   !forPublic && me != null  → {id, userId: me.id}  (= 自分の list のみ。private 可)
	//   それ以外 (forPublic / 匿名) → {id, isPublic: true} (= public list のみ)
	// 以前の mk-go は「private は所有者のみ / public は誰でも」と緩く、(a) 認証
	// 非所有者が他人の public list を forPublic 無しで読め、(b) forPublic=true で
	// 自分の private list が 404 にならない、という乖離があった。
	viewer := middleware.GetUser(c)
	var visible bool
	if !req.ForPublic && viewer != nil {
		visible = list.UserID == viewer.ID
	} else {
		visible = list.IsPublic
	}
	if !visible {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "7bc05c21-1d7a-41ae-88f1-66820f4dc686"))
	}
	packed := entity.PackUserList(list, h.memberIDs(list.ID), h.idGen)
	// upstream show.ts: forPublic && public のとき likedCount/isLiked を付与する。
	// それ以外は通常の UserList shape のまま返す。
	if !(req.ForPublic && list.IsPublic && h.favoriteRepo != nil) {
		return c.JSON(http.StatusOK, packed)
	}
	// 追加フィールドを足すため UserList struct を map に展開する。
	b, _ := json.Marshal(packed)
	resp := map[string]any{}
	_ = json.Unmarshal(b, &resp)
	likedCount, _ := h.favoriteRepo.CountByList(req.ListID)
	resp["likedCount"] = likedCount
	isLiked := false
	if viewer != nil {
		isLiked, _ = h.favoriteRepo.Exists(viewer.ID, req.ListID)
	}
	resp["isLiked"] = isLiked
	return c.JSON(http.StatusOK, resp)
}

// memberIDs returns the userId list of members of the given list.
// repo error は best-effort で nil を返し、entity.PackUserList 側で空配列
// として serialize される (= 部分結果でも shape は保つ、production では
// router で必ず repo wire 済)。transient error は運用で観測したいので
// slog.Warn で記録する (= blocking handler の fetchBlockeeMap と同 pattern)。
//
// 本 helper は `Show` (`/api/users/lists/show`) からのみ呼ばれる単発 1 query
// 経路。list endpoint (= `/api/users/lists/list`) は line 57 で
// `ListMembersByListIDs` 経由の 1 batch query を使うので N+1 にならない。
func (h *Handler) memberIDs(listID string) []string {
	members, err := h.repo.ListMembers(listID)
	if err != nil {
		slog.Warn("users/lists: failed to fetch members", "listId", listID, "error", err)
		return nil
	}
	if len(members) == 0 {
		return nil
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	return ids
}

// Push handles POST /api/users/lists/push (add member).
func (h *Handler) Push(c echo.Context) error {
	var req struct {
		ListID string `json:"listId"`
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.ListID == "" || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "listId and userId are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	list, err := h.repo.FindByID(req.ListID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "2214501d-ac96-4049-b717-91e42272a711"))
	}
	// 認証ユーザーが list 所有者でない場合は存在ごと隠す (NO_SUCH_LIST)。これが
	// 無いと任意の認証ユーザーが他人の list に勝手に member を push できる。
	viewer := middleware.GetUser(c)
	if viewer == nil || list.UserID != viewer.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "2214501d-ac96-4049-b717-91e42272a711"))
	}
	// upstream users/lists/push は AddMember 前に対象 user の存在を
	// getterService.getUser で検証し、不在なら NO_SUCH_USER を返す (#1550)。
	// userRepo 未配線の test 経路では skip する (production は router が必ず wire)。
	var target *model.User
	if h.userRepo != nil {
		var ferr error
		target, ferr = h.userRepo.FindByID(req.UserID)
		if ferr != nil && !repository.IsNotFound(ferr) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if ferr != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "a89abd3d-f0bc-4cce-beb1-2f446f4f1e6a"))
		}
	}
	// upstream は「対象 user が自分を block しているか」(blockerId=target,
	// blockeeId=me) を確認し、block されていれば YOU_HAVE_BEEN_BLOCKED を返す
	// (#1550)。自分自身を push するケースは block 判定を skip する (= TS の
	// user.id !== me.id ガードと一致)。repo error は fail-closed で blocked
	// 扱いにし、blocker の意図に反した membership 作成を防ぐ。
	if h.blockingRepo != nil && req.UserID != viewer.ID {
		blocked, err := h.blockingRepo.Exists(req.UserID, viewer.ID)
		if err != nil || blocked {
			return c.JSON(http.StatusBadRequest, apierr.Error("YOU_HAVE_BEEN_BLOCKED", "You cannot push this user because you have been blocked by this user.", "990232c5-3f9d-4d83-9f3f-ef27b6332a4b"))
		}
	}
	// userEachUserListsLimit role policy gate (#1029)。list owner の policy
	// で評価する (= owner == viewer は直上で確定済)。
	if h.rolePolicyProvider != nil {
		if limit, ok := role.PolicyNumber(h.rolePolicyProvider.GetUserPolicies(list.UserID)["userEachUserListsLimit"]); ok && limit >= 0 {
			count, err := h.repo.CountMembers(list.ID)
			if err != nil {
				return apierr.JSONInternalError(c)
			}
			if count >= int64(limit) {
				return apierr.JSONTooManyUsers(c)
			}
		}
	}
	m := &model.UserListMembership{
		ID:         h.idGen.Generate(time.Now()),
		UserListID: req.ListID,
		UserID:     req.UserID,
		// upstream addMember は userListUserId に list owner を入れる (#1550)。
		UserListUserID: &list.UserID,
	}
	if err := h.repo.AddMember(m); err != nil {
		// 既 member への push は TS 互換の ALREADY_ADDED (HTTP 400) で
		// 返す (#396)。UUID は misskey/.../users/lists/push.ts と一致。
		if errors.Is(err, repository.ErrUserListDuplicateMember) {
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_ADDED", "That user has already been added to that list.", "1de7c884-1595-49e9-857e-61f12f4d4fc5"))
		}
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// remote user を list に追加したら proxy account がフォローして連合投稿を
	// 受信できるようにする (#1704、upstream addMember の isRemoteUser 分岐)。
	if h.proxyFollow != nil && target != nil && !target.IsLocal() {
		h.proxyFollow.EnqueueProxyFollow([]string{req.UserID})
	}
	// userList channel に userAdded を流して member list を live 更新する
	// (#1549、upstream UserListService.addMember の publishUserListStream)。
	// body は packed UserLite (= upstream userEntityService.pack(target))。
	if h.userListEventPub != nil && target != nil {
		h.userListEventPub.PublishUserListEvent(list.ID, "userAdded", entity.PackUserLite(target))
	}
	return c.NoContent(http.StatusNoContent)
}

// Pull handles POST /api/users/lists/pull (remove member).
func (h *Handler) Pull(c echo.Context) error {
	var req struct {
		ListID string `json:"listId"`
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.ListID == "" || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "listId and userId are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 所有者以外は存在ごと隠す。upstream Misskey TS users/lists/pull と同 UUID
	// (= update-membership と共有)。これにより「未存在」と「他人」を error UUID
	// で識別できないようにする副次効果も得られる。
	list, err := h.repo.FindByID(req.ListID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "7f44670e-ab16-43b8-b4c1-ccd2ee89cc02"))
	}
	viewer := middleware.GetUser(c)
	if viewer == nil || list.UserID != viewer.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "7f44670e-ab16-43b8-b4c1-ccd2ee89cc02"))
	}
	// upstream pull.ts は removeMember 前に対象 user の存在を getterService.getUser
	// で検証し、不在なら NO_SUCH_USER を返す (#1550)。userRepo 未配線の test 経路
	// では skip する。
	var target *model.User
	if h.userRepo != nil {
		var uerr error
		target, uerr = h.userRepo.FindByID(req.UserID)
		if uerr != nil && !repository.IsNotFound(uerr) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if uerr != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "588e7f72-c744-4a61-b180-d354e912bda2"))
		}
	}
	if err := h.repo.RemoveMember(req.ListID, req.UserID); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// userList channel に userRemoved を流して member list を live 更新する
	// (#1549、upstream UserListService.removeMember の publishUserListStream)。
	if h.userListEventPub != nil && target != nil {
		h.userListEventPub.PublishUserListEvent(list.ID, "userRemoved", entity.PackUserLite(target))
	}
	return c.NoContent(http.StatusNoContent)
}

// Delete handles POST /api/users/lists/delete.
func (h *Handler) Delete(c echo.Context) error {
	var req struct {
		ListID string `json:"listId"`
	}
	if err := c.Bind(&req); err != nil || req.ListID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "listId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 所有者以外は存在ごと隠す。これが無いと任意の認証ユーザーが他人の list
	// を delete できる (data loss、ulid のため復元不能)。UUID は upstream
	// Misskey TS users/lists/delete と一致させる。
	list, err := h.repo.FindByID(req.ListID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "78436795-db79-42f5-b1e2-55ea2cf19166"))
	}
	viewer := middleware.GetUser(c)
	if viewer == nil || list.UserID != viewer.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "78436795-db79-42f5-b1e2-55ea2cf19166"))
	}
	if err := h.repo.Delete(req.ListID); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.NoContent(http.StatusNoContent)
}

// HasRolePolicyProvider reports whether the role policy provider was wired.
//
// 未配線だと `userListLimit` と `userEachUserListsLimit` が両方とも効かない。
// 起動時検査に使う (#2683)。
func (h *Handler) HasRolePolicyProvider() bool { return h.rolePolicyProvider != nil }
