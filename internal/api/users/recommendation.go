package users

import (
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// GetFrequentlyRepliedUsers handles POST /api/users/get-frequently-replied-users.
//
// 指定 userId が最近返信している相手上位 limit 件を weight = count / peak
// (最大件数で正規化) 付きで返す。Misskey 本家と同じレスポンス shape。
//
// reply 集計は visibility push-down で viewer が CanSeeNote で見られる reply
// のみに絞る (#1486)。匿名 viewer は public/home のみ集計する。
func (h *Handler) GetFrequentlyRepliedUsers(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
		Limit  *int   `json:"limit"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	req.Limit = &limit
	if _, err := h.userService.ShowByID(req.UserID); err != nil {
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_USER", "No such user.", "e6965129-7b2a-40a4-bae2-cd84cd434822"))
	}
	viewer := middleware.GetUser(c)
	// upstream は reply 集計の 2 つの query に generateVisibilityQuery を掛けるので、
	// 匿名 visitor かつ ugcVisibilityForVisitor='none' なら何も集計されず [] になる。
	// NO_SUCH_USER は query より前に投げるので、この順に並べる。
	if h.visitorHidesAllNotes(viewer) {
		return c.JSON(http.StatusOK, []any{})
	}
	var viewerID string
	if viewer != nil {
		viewerID = viewer.ID
	}
	rows, err := h.noteRepo.CountReplyTargets(req.UserID, viewerID, limit)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	var peak int64
	for _, r := range rows {
		if r.Count > peak {
			peak = r.Count
		}
	}
	// 返信先の利用者と profile は 2 回の問い合わせでまとめて引く (#3330)。本家も
	// packMany でまとめて組む。以前は返信先ごとに ShowByID を呼んでいた。
	ids := make([]string, len(rows))
	weightByID := make(map[string]float64, len(rows))
	for i, r := range rows {
		ids[i] = r.UserID
		// peak>0 は上で rows が非空なら必ず真だが、念のためガード。
		if peak > 0 {
			weightByID[r.UserID] = float64(r.Count) / float64(peak)
		}
	}
	bundles, err := h.userService.ShowManyByIDs(ids)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	users := make([]*model.User, 0, len(bundles))
	profiles := make(map[string]*model.UserProfile, len(bundles))
	for _, b := range bundles {
		users = append(users, b.User)
		if b.Profile != nil {
			profiles[b.User.ID] = b.Profile
		}
	}
	// 本家 get-frequently-replied-users は packMany(users, me, {schema:
	// 'UserDetailed'})。モデレーター向けの項目・関係 (#1973)・カウントのゲート
	// (#1985)・ピン留め・移行先を DetailedMany でまとめて組み、返信先に閲覧者
	// 本人が居ればその行は本家の pack と同じく MeDetailed にする (#3330)。
	packed := h.packDetailedAll(c.Request().Context(), viewer, users, profiles)
	out := make([]map[string]any, len(users))
	for i, u := range users {
		out[i] = map[string]any{"user": packed[i], "weight": weightByID[u.ID]}
	}
	return c.JSON(http.StatusOK, out)
}

// birthdayRange は月日指定の単発 (Month/Day) もしくは範囲 (Begin/End) を
// JSON で受け取るための共通形。oneOf は Bind では表現できないため、
// すべて optional にして後段で検証する。
type birthdayRange struct {
	Month *int `json:"month,omitempty"`
	Day   *int `json:"day,omitempty"`
	Begin *struct {
		Month int `json:"month"`
		Day   int `json:"day"`
	} `json:"begin,omitempty"`
	End *struct {
		Month int `json:"month"`
		Day   int `json:"day"`
	} `json:"end,omitempty"`
}

func isValidMMDD(m, d int) bool {
	return m >= 1 && m <= 12 && d >= 1 && d <= 31
}

// GetFollowingUsersByBirthday handles POST /api/users/get-following-users-by-birthday.
//
// 認証ユーザーの followee のうち誕生日 (月日) が指定範囲に入る者を返す。
// 単発指定 ({month, day}) と範囲指定 ({begin, end}) の 2 形式に対応し、
// 範囲指定で begin > end の場合は年跨ぎ (例: 12/25..1/5) として扱う。
func (h *Handler) GetFollowingUsersByBirthday(c echo.Context) error {
	viewer := middleware.GetUser(c)
	var req struct {
		Limit    *int           `json:"limit"`
		Offset   int            `json:"offset"`
		Birthday *birthdayRange `json:"birthday"`
	}
	if err := c.Bind(&req); err != nil || req.Birthday == nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	req.Limit = &limit
	var begin, end int
	switch {
	case req.Birthday.Begin != nil && req.Birthday.End != nil:
		if !isValidMMDD(req.Birthday.Begin.Month, req.Birthday.Begin.Day) ||
			!isValidMMDD(req.Birthday.End.Month, req.Birthday.End.Day) {
			return apierr.JSONInvalidParam(c)
		}
		begin = req.Birthday.Begin.Month*100 + req.Birthday.Begin.Day
		end = req.Birthday.End.Month*100 + req.Birthday.End.Day
	case req.Birthday.Month != nil && req.Birthday.Day != nil:
		if !isValidMMDD(*req.Birthday.Month, *req.Birthday.Day) {
			return apierr.JSONInvalidParam(c)
		}
		begin = *req.Birthday.Month*100 + *req.Birthday.Day
		end = begin
	default:
		return apierr.JSONInvalidParam(c)
	}
	if h.followingRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	rows, err := h.followingRepo.ListFollowingByBirthday(viewer.ID, begin, end, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// 本家は「今日以降の最寄りの誕生日の日付」を "YYYY-MM-DD" で返す。
	now := time.Now()
	// 相手は 1 回でまとめて引く (行ごとに ShowByID を呼ぶと N+1 になる、#3330)。
	// 本家も packMany に ID を渡して一括で引く。UserLite なので profile は引かない。
	// 引けない行は従来どおり飛ばし、並びは rows の順を保つ。
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.FolloweeID)
	}
	found, err := h.userService.FindManyByIDs(ids)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	byID := make(map[string]*model.User, len(found))
	for _, u := range found {
		if u != nil {
			byID[u.ID] = u
		}
	}
	out := make([]map[string]any, 0, len(rows))
	users := make([]*model.User, 0, len(rows))
	for _, r := range rows {
		u, ok := byID[r.FolloweeID]
		if !ok {
			continue
		}
		birthday := nextBirthdayDate(r.Birthday, now)
		out = append(out, map[string]any{
			"id":       r.FolloweeID,
			"birthday": birthday,
		})
		users = append(users, u)
	}
	// 本家は packMany(users, me, {schema: 'UserLite'}) なので、instance と絵文字も
	// まとめて埋める (#3330)。
	for i, lite := range h.packLites(users) {
		out[i]["user"] = lite
	}
	return c.JSON(http.StatusOK, out)
}

// nextBirthdayDate returns the bday in "YYYY-MM-DD" form adjusted so that it is
// not earlier than "today". If bday's (month, day) is before today, the year is
// bumped by one — same semantics as Misskey 本家。
func nextBirthdayDate(bday string, now time.Time) string {
	if len(bday) != 10 {
		return bday
	}
	mm := int(bday[5]-'0')*10 + int(bday[6]-'0')
	dd := int(bday[8]-'0')*10 + int(bday[9]-'0')
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	bdayDate := time.Date(now.Year(), time.Month(mm), dd, 0, 0, 0, 0, now.Location())
	if bdayDate.Before(today) {
		bdayDate = bdayDate.AddDate(1, 0, 0)
	}
	return bdayDate.Format("2006-01-02")
}

// UserRecommendation handles POST /api/users/recommendation.
//
// 認証ユーザーがまだフォローしていないローカルのアクティブユーザーを
// followersCount 降順で返す (onboarding 向け)。Misskey 本家互換で 7 日以内に
// 更新があったユーザーに絞る。
func (h *Handler) UserRecommendation(c echo.Context) error {
	viewer := middleware.GetUser(c)
	var req struct {
		Limit  *int `json:"limit"`
		Offset int  `json:"offset"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	req.Limit = &limit
	users, err := h.userService.ListRecommendations(viewer.ID, time.Now().AddDate(0, 0, -7), limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// 本家 users/recommendation は packMany(users, me, {schema: 'UserDetailed'})。
	// モデレーター向けの項目も含めて DetailedMany で組み、profile と関係は IN で
	// まとめて引く (#3330)。自分自身は候補から外れているので MeDetailed にはならない。
	ctx := c.Request().Context()
	return c.JSON(http.StatusOK, h.packDetailedMany(ctx, viewer, users, h.userService.GetProfilesByUserIDs(userIDs(users))))
}

// userIDs returns the IDs of users in order.
func userIDs(users []*model.User) []string {
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	return ids
}

// UsersBulk handles POST /api/users — bulk user lookup.
//
// Misskey 本家は userIds を最大 100 件に制限している。未知 ID は無視される
// (他の ID の結果は返す)。
func (h *Handler) UsersBulk(c echo.Context) error {
	var req struct {
		UserIDs []string `json:"userIds"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if len(req.UserIDs) == 0 {
		return c.JSON(http.StatusOK, []any{})
	}
	if len(req.UserIDs) > 100 {
		req.UserIDs = req.UserIDs[:100]
	}
	out := make([]entity.UserLite, 0, len(req.UserIDs))
	for _, uid := range req.UserIDs {
		if bundle, err := h.userService.ShowByID(uid); err == nil {
			out = append(out, entity.PackUserLite(bundle.User))
		}
	}
	return c.JSON(http.StatusOK, out)
}
