package federation

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// hostPageRequest is the common request body for the three per-host listing
// endpoints (federation/followers, federation/following, federation/users).
type hostPageRequest struct {
	Host      string `json:"host"`
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
	// sinceID / untilID は parseHostPage が sinceDate/untilDate を aidx prefix に
	// 正規化して埋める cursor 値 (#1732、upstream makePaginationQuery 互換)。
	sinceID string
	untilID string
}

// Followers handles POST /api/federation/followers.
//
// 指定リモートホストに属するユーザーが、このインスタンスのローカルユーザーを
// フォローしている関係の一覧を返す。Misskey 本家互換。
func (h *Handler) Followers(c echo.Context) error {
	req, ok := parseHostPage(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	if h.followingRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	viewer := h.followListViewer(c)
	rows, err := h.followingRepo.ListFollowersByHostCursor(req.Host, req.sinceID, req.untilID, (*req.Limit), viewer)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.packFollowings(viewer.UserID, rows))
}

// Following handles POST /api/federation/following.
//
// このインスタンスのローカルユーザーが、指定リモートホストに属するユーザーを
// フォローしている関係の一覧を返す。
func (h *Handler) Following(c echo.Context) error {
	req, ok := parseHostPage(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	if h.followingRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	viewer := h.followListViewer(c)
	rows, err := h.followingRepo.ListFollowingByHostCursor(req.Host, req.sinceID, req.untilID, (*req.Limit), viewer)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.packFollowings(viewer.UserID, rows))
}

// Users handles POST /api/federation/users.
//
// 指定リモートホストに属するユーザーの一覧を返す。
func (h *Handler) Users(c echo.Context) error {
	req, ok := parseHostPage(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	if h.userRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	users, err := h.userRepo.ListUsers(model.UserListFilter{
		Origin:   "remote",
		Hostname: req.Host,
		Limit:    (*req.Limit),
		Offset:   req.Offset,
		SinceID:  req.sinceID,
		UntilID:  req.untilID,
	})
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// upstream users.ts は packMany(users, me, {schema:'UserDetailedNotMe'}) を
	// 返す。旧実装は id/username/host/name/avatarUrl の最小 map のみで
	// createdAt/notesCount/followersCount/description/fields/roles 等が欠落して
	// いた (#1544)。UserDetailed で pack して shape を揃える。user_profile は
	// 1 batch で解決して N+1 を回避。
	viewerID := viewerIDOf(c)
	iAmModerator := h.moderator != nil && viewerID != "" && h.moderator.IsModerator(viewerID)
	profByID := h.userProfiles(users)
	out := make([]entity.UserDetailed, 0, len(users))
	for _, u := range users {
		d := entity.PackUserDetailed(u, profByID[u.ID], h.idGen)
		// 認証 caller には viewer->user の relation block を付与 (匿名/self は no-op、#1957-a)。
		viewerIsFollowing := h.relation.Apply(&d, viewerID, u, profByID[u.ID])
		// followers-only count を非フォロワー (federation/* は requireCredential:false なので
		// 匿名含む) に leak させない (upstream packMany(users, me) の count gate、#1988)。
		entity.GateCountVisibility(&d, u.ID == viewerID, iAmModerator, viewerIsFollowing)
		out = append(out, d)
	}
	return c.JSON(http.StatusOK, out)
}

// followListViewer describes the caller for the owner visibility filter of
// federation/followers and federation/following. Mirrors upstream, which
// applies generateFollowingRelationVisibilityQuery unless the caller is a
// moderator.
//
// moderator checker が未配線なら moderator 扱いしない (絞る側に倒す)。
func (h *Handler) followListViewer(c echo.Context) model.FollowListViewer {
	viewerID := viewerIDOf(c)
	return model.FollowListViewer{
		UserID:    viewerID,
		Moderator: h.moderator != nil && viewerID != "" && h.moderator.IsModerator(viewerID),
	}
}

// viewerIDOf returns the authenticated viewer's ID, or "" for anonymous callers.
// federation/* は requireCredential:false なので nil viewer を許容する。
func viewerIDOf(c echo.Context) string {
	if u := middleware.GetUser(c); u != nil {
		return u.ID
	}
	return ""
}

// userProfiles batch-loads the user_profile rows for the given users keyed by
// userId (N+1 回避)。userRepo 未配線 / 取得失敗時は nil。
func (h *Handler) userProfiles(users []*model.User) map[string]*model.UserProfile {
	if h.userRepo == nil || len(users) == 0 {
		return nil
	}
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	profiles, err := h.userRepo.FindProfilesByUserIDs(ids)
	if err != nil {
		return nil
	}
	byID := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		if p != nil {
			byID[p.UserID] = p
		}
	}
	return byID
}

// parseHostPage parses + validates the common per-host page request. Returns
// (req, true) on success; on failure the caller must write the 400 response
// itself to avoid double-writing the body.
func parseHostPage(c echo.Context) (hostPageRequest, bool) {
	var req hostPageRequest
	if err := c.Bind(&req); err != nil || req.Host == "" {
		return req, false
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return req, false
	}
	req.Limit = &limit
	// sinceDate / untilDate を aidx prefix に正規化して cursor 値に落とす
	// (#1732、upstream makePaginationQuery 互換)。
	// **列に入らないカーソルもここで ok=false にする (#3025)。**
	cursorSince, cursorUntil, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return req, false
	}
	req.sinceID, req.untilID = cursorSince, cursorUntil
	return req, true
}

// packFollowings converts Following rows into the upstream
// FollowingEntityService.pack shape (id / createdAt / followeeId / followerId
// + populated followee).
//
// 本家 federation/{followers,following}.ts は packMany(..., {populateFollowee:
// true}) を呼ぶため followee (UserDetailedNotMe) を必ず embed する。followee の
// User+UserProfile は ID をまとめて 1 度引いて N+1 を避ける。userRepo が nil の
// 場合 (= test 等で未配線) は followee を埋めずに id 系フィールドだけ返す。
func (h *Handler) packFollowings(viewerID string, rows []*model.Following) []entity.Following {
	lookup := h.followeeLookup(rows)
	iAmModerator := h.moderator != nil && viewerID != "" && h.moderator.IsModerator(viewerID)
	out := make([]entity.Following, 0, len(rows))
	for _, f := range rows {
		// populateFollowee=true / populateFollower=false (本家と同じ)。
		pf := entity.PackFollowing(f, true, false, lookup, h.idGen)
		// embed followee に viewer 視点の relation block を付与 (#1957-a)。
		if pf.Followee != nil {
			if u, p := lookup(f.FolloweeID); u != nil {
				viewerIsFollowing := h.relation.Apply(pf.Followee, viewerID, u, p)
				// followers-only count を非フォロワー (匿名含む) に leak させない
				// (upstream FollowingEntityService が followee を pack(_, me) で gate、#1988)。
				entity.GateCountVisibility(pf.Followee, u.ID == viewerID, iAmModerator, viewerIsFollowing)
			}
		}
		out = append(out, pf)
	}
	return out
}

// followeeLookup batch-loads the followee User + UserProfile for the given
// rows and returns a closure resolving userID → (User, UserProfile). When
// userRepo is nil or a user is missing the closure returns (nil, nil) so the
// corresponding followee stays nil.
func (h *Handler) followeeLookup(rows []*model.Following) entity.UserProfileLookup {
	if h.userRepo == nil || len(rows) == 0 {
		return func(string) (*model.User, *model.UserProfile) { return nil, nil }
	}
	ids := make([]string, 0, len(rows))
	for _, f := range rows {
		ids = append(ids, f.FolloweeID)
	}
	users, err := h.userRepo.FindManyByIDs(ids)
	if err != nil {
		return func(string) (*model.User, *model.UserProfile) { return nil, nil }
	}
	userIdx := make(map[string]*model.User, len(users))
	for _, u := range users {
		if u != nil {
			userIdx[u.ID] = u
		}
	}
	profIdx := make(map[string]*model.UserProfile)
	if profiles, err := h.userRepo.FindProfilesByUserIDs(ids); err == nil {
		for _, p := range profiles {
			if p != nil {
				profIdx[p.UserID] = p
			}
		}
	}
	return func(uid string) (*model.User, *model.UserProfile) {
		u, ok := userIdx[uid]
		if !ok {
			return nil, nil
		}
		return u, profIdx[uid]
	}
}
