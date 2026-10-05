package federation

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
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
	return c.JSON(http.StatusOK, h.packFollowings(c, rows))
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
	return c.JSON(http.StatusOK, h.packFollowings(c, rows))
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
	return c.JSON(http.StatusOK, h.packUsers(c, users, h.userProfiles(users)))
}

// packUsers packs users as upstream packMany(users, me, {schema:
// 'UserDetailed'}) seen by the caller, in order.
//
// 関係・カウントのゲート (匿名含む、#1957-a / #1988)・モデレーター向けの項目・
// ピン留め・移行先・instance・絵文字は共有の packer に任せる (#3330)。本家の
// pack は isMe なら MeDetailed を返すので、閲覧者自身は meself.Pack で昇格する。
func (h *Handler) packUsers(c echo.Context, users []*model.User, profiles map[string]*model.UserProfile) []any {
	out := make([]any, len(users))
	if len(users) == 0 {
		return out
	}
	viewer := middleware.GetUser(c)
	ctx := c.Request().Context()
	if h.packer == nil {
		// 配線が外れた構成では素の UserDetailed を返す (packer が既定で
		// 非公開のカウントを伏せるので、漏れる側には倒れない)。
		for i, u := range users {
			out[i] = entity.PackUserDetailed(u, profiles[u.ID], h.idGen)
		}
		return out
	}
	packed := h.packer.DetailedMany(ctx, viewer, users, profiles)
	for i, u := range users {
		out[i] = meself.Pack(ctx, packed[i], u, profiles[u.ID], viewer)
	}
	return out
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

// packedFollowing is the upstream FollowingEntityService.pack shape with
// populateFollowee. Followee is any because the caller's own entry is
// MeDetailed (upstream pack returns MeDetailed when isMe).
type packedFollowing struct {
	ID         string `json:"id"`
	CreatedAt  string `json:"createdAt"`
	FolloweeID string `json:"followeeId"`
	FollowerID string `json:"followerId"`
	Followee   any    `json:"followee,omitempty"`
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
// FollowingEntityService.packMany shape (id / createdAt / followeeId /
// followerId + populated followee).
//
// 本家 federation/{followers,following}.ts は packMany(..., {populateFollowee:
// true}) を呼ぶため followee (UserDetailedNotMe) を必ず embed する。followee の
// User+UserProfile は ID をまとめて 1 度引いて N+1 を避け、packMany と同じく
// まとめて pack する (#3330)。userRepo が nil の場合 (= test 等で未配線) は
// followee を埋めずに id 系フィールドだけ返す。
func (h *Handler) packFollowings(c echo.Context, rows []*model.Following) []packedFollowing {
	users, profiles := h.followees(rows)
	packed := h.packUsers(c, users, profiles)
	byID := make(map[string]any, len(users))
	for i, u := range users {
		byID[u.ID] = packed[i]
	}
	out := make([]packedFollowing, 0, len(rows))
	for _, f := range rows {
		// populateFollowee=true / populateFollower=false (本家と同じ)。
		pf := packedFollowing{ID: f.ID, FolloweeID: f.FolloweeID, FollowerID: f.FollowerID}
		if h.idGen != nil {
			if t, err := h.idGen.ParseTime(f.ID); err == nil {
				pf.CreatedAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
			}
		}
		if d, ok := byID[f.FolloweeID]; ok {
			pf.Followee = d
		}
		out = append(out, pf)
	}
	return out
}

// followees batch-loads the distinct followee users of rows and their
// profiles. When userRepo is nil or the lookup fails it returns no users, so
// every followee stays absent.
func (h *Handler) followees(rows []*model.Following) ([]*model.User, map[string]*model.UserProfile) {
	if h.userRepo == nil || len(rows) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(rows))
	ids := make([]string, 0, len(rows))
	for _, f := range rows {
		if _, ok := seen[f.FolloweeID]; ok {
			continue
		}
		seen[f.FolloweeID] = struct{}{}
		ids = append(ids, f.FolloweeID)
	}
	found, err := h.userRepo.FindManyByIDs(ids)
	if err != nil {
		return nil, nil
	}
	users := make([]*model.User, 0, len(found))
	for _, u := range found {
		if u != nil {
			users = append(users, u)
		}
	}
	return users, h.userProfiles(users)
}
