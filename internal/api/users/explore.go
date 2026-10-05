package users

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// SetMetaRepo attaches a MetaRepository for POST /api/pinned-users.
//
// 未配線なら pinned-users は空配列を返す (upstream も `pinnedUsers` が空なら
// 同じ結果になるので、shape は変わらない)。
func (h *Handler) SetMetaRepo(r repository.MetaRepository) { h.metaRepo = r }

// HasMetaRepo reports whether the blocked-host filter can read meta.
//
// 未配線だと `blockedHosts` の除外が黙って no-op になる (ブロックしたはずの
// インスタンスのノートが一覧に出続ける)。起動時の critical wiring 検査で落とす。
func (h *Handler) HasMetaRepo() bool {
	return h != nil && h.metaRepo != nil
}

// SetLocalHost records the instance hostname used to resolve `@user@host`
// entries in `meta.pinnedUsers`.
//
// 空でも動く — その場合 `@user@own.example` 形式の指定が remote 扱いになり
// 引けなくなるだけで、`@user` 形式は従来どおり解決する。
func (h *Handler) SetLocalHost(host string) { h.localHost = host }

// List serves the public user directory.
//
// POST /api/users
//
// **DB から読めなくても 200 で空配列を返す。** upstream の explore ページは
// 配列前提で `.map()` するので、ここで 500 にするとページ全体が開かなくなる。
// 引数の不正は本家と同じく 400 INVALID_PARAM にする (#3330)。
func (h *Handler) List(c echo.Context) error {
	var req struct {
		Limit    int    `json:"limit"`
		Offset   int    `json:"offset"`
		Sort     string `json:"sort"`
		State    string `json:"state"`
		Origin   string `json:"origin"`
		Hostname string `json:"hostname"`
	}
	if err := c.Bind(&req); err != nil {
		// 本家は paramDef の ajv 検査で 400 にする (#3330)。
		return apierr.JSONInvalidParam(c)
	}
	// upstream users.ts:35 の state enum は ['all','alive'] (default 'all')。
	// 範囲外 (moderator/admin 等の role state) は ajv が 400 で reject するので、
	// public /users でも同じく弾く (ListUsers の role filter に到達させない、#1996)。
	if !ValidListState(req.State) {
		return apierr.JSONInvalidParam(c)
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}
	if req.Origin == "" {
		req.Origin = "local"
	}
	viewer := middleware.GetUser(c)
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	if h.userRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream users.ts: base filter isExplorable=TRUE AND isSuspended=FALSE、
	// hostname 絞り込み、認証時は mute/block 除外 (#1957-b)。
	list, err := h.userRepo.ListUsers(model.UserListFilter{
		State: req.State, Origin: req.Origin, Sort: req.Sort,
		Limit: req.Limit, Offset: req.Offset,
		Hostname:             req.Hostname,
		ExplorableOnly:       true,
		ExcludeRelatedTo:     viewerID,
		UpdatedAtSortNonNull: true, // #1975: public /users は updatedAt sort で NULL updatedAt を除外
	})
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// 本家 users.ts は packMany(users, me, {schema: 'UserDetailed'})。モデレーター
	// 向けの項目・関係・カウントのゲート・ピン留め・移行先を DetailedMany で
	// まとめて組み、profile と関係は一覧ぶんを IN でまとめて引く (#3330)。
	ctx := c.Request().Context()
	return c.JSON(http.StatusOK, h.packDetailedAll(ctx, viewer, list, h.profilesByUserIDs(list)))
}

// profilesByUserIDs loads the profiles of users with one IN query. A failed
// lookup yields an empty map, so the users are packed with closed counts
// (GateCountVisibility) instead of failing the whole list.
func (h *Handler) profilesByUserIDs(users []*model.User) map[string]*model.UserProfile {
	out := make(map[string]*model.UserProfile, len(users))
	if len(users) == 0 || h.userRepo == nil {
		return out
	}
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	profiles, err := h.userRepo.FindProfilesByUserIDs(ids)
	if err != nil {
		return out
	}
	for _, p := range profiles {
		out[p.UserID] = p
	}
	return out
}

// PinnedUsers serves the instance's featured accounts.
//
// POST /api/pinned-users
//
// 引けない acct は**黙って飛ばす**。`meta.pinnedUsers` は管理者が手で書く
// 文字列で、typo や退会済みの指定が混ざりうる。1 件のせいで全体が空になる
// ほうが困る。
func (h *Handler) PinnedUsers(c echo.Context) error {
	if h.metaRepo == nil || h.userRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	m, err := h.metaRepo.Fetch()
	if err != nil || m == nil || len(m.PinnedUsers) == 0 {
		return c.JSON(http.StatusOK, []any{})
	}
	users := make([]*model.User, 0, len(m.PinnedUsers))
	for _, acct := range m.PinnedUsers {
		username, host := ParseAcct(acct, h.localHost)
		if username == "" {
			continue
		}
		u, err := h.userRepo.FindByUsernameLower(strings.ToLower(username), host)
		if err != nil {
			continue
		}
		users = append(users, u)
	}
	// 本家 pinned-users は packMany(users, me, {schema: 'UserDetailed'})。
	// モデレーター向けの項目も含めて DetailedMany で組み、自分の行は本家の pack と
	// 同じく MeDetailed にする (#3330)。
	ctx := c.Request().Context()
	return c.JSON(http.StatusOK, h.packDetailedAll(ctx, middleware.GetUser(c), users, h.profilesByUserIDs(users)))
}

// ParseAcct splits an `@user@host` string into its username and host parts.
//
// host は**自インスタンスなら nil** を返す (local user は host 列が NULL)。
// 先頭の `@` は付いていてもいなくてもよい。
//
// **localHost はここで小文字化する。** `url.Parse(config.URL).Host` は host を
// 正規化しないので、呼び出し側に任せると `config.url` に大文字が混ざる構成で
// 経路ごとに local / remote の判定が食い違う (実際 avatar 側だけが小文字化
// していて、pinned-users から `@user@Own.Example` が黙って消えていた、#2791)。
func ParseAcct(acct, localHost string) (username string, host *string) {
	localHost = strings.ToLower(localHost)
	acct = strings.TrimSpace(acct)
	acct = strings.TrimPrefix(acct, "@")
	if acct == "" {
		return "", nil
	}
	at := strings.IndexByte(acct, '@')
	if at < 0 {
		return acct, nil
	}
	name := acct[:at]
	h := strings.ToLower(acct[at+1:])
	if name == "" {
		return "", nil
	}
	if h == "" || h == localHost {
		return name, nil
	}
	return name, &h
}
