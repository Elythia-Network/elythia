package i

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// Apps handles POST /api/i/apps.
//
// 本家 Misskey の i/apps と同 shape (access_token + app の JOIN) を返す。
// 旧実装は誤って空配列を返しており frontend "Apps" 一覧が常に空だったため
// #587 で本実装化した。Misskey TS 本家 ref:
// packages/backend/src/server/api/endpoints/i/apps.ts。
//
// sort 値は +/-createdAt / +/-lastUsedAt。未指定時は token.id ASC (本家 default)。
// 各 token の name / permission / description は app へフォールバックする
// (例: token.name が NULL なら app.name を使う、本家と同じ ?? チェーン)。
func (h *Handler) Apps(c echo.Context) error {
	if h.accessTokenRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	u := middleware.GetUser(c)
	var req struct {
		Sort string `json:"sort"`
	}
	_ = c.Bind(&req)
	tokens, err := h.accessTokenRepo.ListByUserIDPreloadApp(u.ID, req.Sort)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	const tsFormat = "2006-01-02T15:04:05.000Z"
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		entry := map[string]any{
			"id":         t.ID,
			"name":       coalesceString(t.Name, appName(t.App)),
			"lastUsedAt": formatNullableTime(t.LastUsedAt, tsFormat),
			"permission": []string(tokenPermission(t)),
			"iconUrl":    t.IconURL,
			"description": coalesceString(
				t.Description,
				appDescription(t.App),
			),
		}
		if ct, err := h.idGen.ParseTime(t.ID); err == nil {
			entry["createdAt"] = ct.UTC().Format(tsFormat)
		}
		out = append(out, entry)
	}
	return c.JSON(http.StatusOK, out)
}

// coalesceString は本家の `a ?? b` チェーン相当。a が nil/"" なら b を返す。
func coalesceString(a, b *string) *string {
	if a != nil && *a != "" {
		return a
	}
	return b
}

func appName(a *model.App) *string {
	if a == nil {
		return nil
	}
	return &a.Name
}

func appDescription(a *model.App) *string {
	if a == nil {
		return nil
	}
	return &a.Description
}

// tokenPermission は本家と同じ分岐: app があれば app.permission、無ければ
// token.permission (本家 i/apps.ts:98)。
func tokenPermission(t *model.AccessToken) []string {
	if t.App != nil {
		return []string(t.App.Permission)
	}
	return []string(t.Permission)
}

// formatNullableTime は *time.Time が nil の場合 nil を返し、そうでなければ
// 指定 layout で UTC 文字列に整形する。
func formatNullableTime(t *time.Time, layout string) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(layout)
}

// AuthorizedApps handles POST /api/i/authorized-apps.
//
// upstream authorized-apps.ts は `appId IS NOT NULL` の access_token を
// limit/offset/sort 付きで引き、各 token を appEntityService.pack(token.appId,
// me, {detail:true}) で **App entity** に変換して返す。旧 mk-go 実装は
// access_token 自身の shape ({id=token.id, description, iconUrl, lastUsedAt
// など}) を返しており、本家 res schema ({id=app.id, name, callbackUrl,
// permission, isAuthorized}) と非互換だった (#1555)。
//
// isAuthorized は upstream で accessTokensRepository.countBy({appId, userId})>0。
// ここで列挙している token 自体が該当 (appId, userId) ペアの存在を保証するので
// 常に true。requireCredential なので me は必ず存在する。
func (h *Handler) AuthorizedApps(c echo.Context) error {
	if h.accessTokenRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	u := middleware.GetUser(c)
	var req struct {
		Limit  *int   `json:"limit"`
		Offset int    `json:"offset"`
		Sort   string `json:"sort"`
	}
	_ = c.Bind(&req)
	// limit: default 10, clamp 1..100 (authorized-apps.ts paramDef)。
	limit := 10
	if req.Limit != nil {
		limit = *req.Limit
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	// sort enum ['desc','asc'] default 'desc' (= id DESC)。'asc' のみ昇順。
	sortAsc := req.Sort == "asc"
	tokens, err := h.accessTokenRepo.ListAuthorizedApps(u.ID, limit, offset, sortAsc)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		// appId NOT NULL filter 済だが、app 行が削除済なら Preload が nil を返す。
		// upstream は findOneByOrFail で reject するが、mk-go は graceful に skip。
		if t.App == nil {
			continue
		}
		out = append(out, map[string]any{
			"id":           t.App.ID,
			"name":         t.App.Name,
			"callbackUrl":  t.App.CallbackURL,
			"permission":   []string(t.App.Permission),
			"isAuthorized": true,
		})
	}
	return c.JSON(http.StatusOK, out)
}

// RevokeToken handles POST /api/i/revoke-token.
// TS互換: tokenId (ID) または token (ハッシュ文字列) のどちらかで指定可。
// 指定 access_token が自分の所有であれば削除する。
func (h *Handler) RevokeToken(c echo.Context) error {
	// upstream c07ce75281 は requireCredential を外し、認証を実装内に移したうえで
	// **endpoint 固有の** CREDENTIAL_REQUIRED を投げる。汎用の middleware に任せると
	// id が 1384574d-... になり upstream (6f1f0d3a-...) と wire が食い違う。
	u := middleware.GetUser(c)
	if u == nil {
		// **凍結アカウントの扱いは upstream と意図的に違う (#2877)。** upstream の
		// isSuspended 判定は ApiCallService の
		// `requireCredential || requireModerator || requireAdmin` ブロックの中にあり、
		// この endpoint はどれも宣言していないので **upstream は suspended を一切見ず、
		// 凍結ユーザーも 204 で失効できる**。
		//
		// mk-go は Authenticate が凍結ユーザーを anonymous に落とす構造なので、ここで
		// 分けないと 401 CREDENTIAL_REQUIRED になり upstream からさらに遠のく。403 の
		// ほうが「凍結ゆえに拒否した」ことが伝わるので、そちらを採る。
		if middleware.IsSuspendedRequest(c) {
			return c.JSON(http.StatusForbidden, apierr.YourAccountSuspended())
		}
		return c.JSON(http.StatusUnauthorized, apierr.Error("CREDENTIAL_REQUIRED", "Credential required.", "6f1f0d3a-3d5b-4b1f-9c3e-2a6d1e5b8c47"))
	}
	// repo 未配線は認証の**後**に見る。前に置くと未認証リクエストにも 204 を返す
	// (起動時の HasAccessTokenRepo 検査があるので production には届かないが、
	// 認証を handler に移した以上、順序で守るべきもの)。
	if h.accessTokenRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		TokenID string `json:"tokenId"`
		Token   string `json:"token"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if req.TokenID == "" && req.Token == "" {
		return apierr.JSONInvalidParam(c)
	}
	var tok *model.AccessToken
	var err error
	if req.TokenID != "" {
		tok, err = h.accessTokenRepo.FindByID(req.TokenID)
	} else {
		// hash 列 (miauth: sha256(token)) と token 列 (raw, app/auth) を 1 query
		// で OR 検索する。middleware と同じ pattern (#910 / #913)。これにより
		// app/auth flow で発行された access token も raw token 文字列で revoke
		// できる。
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(req.Token)))
		tok, err = h.accessTokenRepo.FindByHashOrToken(hash, req.Token)
	}
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を「失効した」(204) に丸めない** (#2792)。呼び出し側は成功と
		// 受け取るのに token は生きている。secure を外してサードパーティアプリが
		// この経路に到達できるようになったので、露出範囲が広がっている。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.NoContent(http.StatusNoContent)
	}
	if tok.UserID != u.ID {
		// upstream revoke-token は delete を {id/token, userId: me.id} 条件で
		// 実行するだけで、他人の token は単に no-op (成功 204) になる (#1546)。
		// 403 を返すと token の存在 / 所有者が leak するため、no-op 204 に揃える。
		return c.NoContent(http.StatusNoContent)
	}
	// upstream c07ce75281: アクセストークン経由 (サードパーティアプリ) では
	// **いま使っているトークン自身しか**失効させられない。native session から
	// 叩く分は従来どおり自分の全トークンを失効できる。
	// 他人のトークンより後に置く — 存在しないトークン ID に 403 を返すと
	// 「そのトークンは在る」ことが漏れるため。
	if sc := middleware.GetAuthScope(c); sc != nil && sc.IsApp && sc.TokenID != tok.ID {
		return c.JSON(http.StatusForbidden, apierr.Error("PERMISSION_DENIED", "Permission denied.", "fc20d118-5705-4462-b6c5-2b5b43092cf3"))
	}
	if err := h.accessTokenRepo.DeleteByID(tok.ID); err != nil {
		return apierr.JSONInternalError(c)
	}
	// auth middleware の token cache に entry が残っていると revoke の効果が
	// cache TTL (= 30 秒) 遅延して上流互換でなくなるため、regenerate-token
	// (#884) と同じく invalidator が wire されている場合は即時削除する。
	if h.authInvalidator != nil && tok.Token != "" {
		h.authInvalidator.InvalidateToken(tok.Token)
	}
	// その token で張られた WebSocket も閉じる (mk-go 独自)。アプリ連携の解除も
	// この endpoint を通る。
	if h.streamRevoker != nil {
		h.streamRevoker.RevokeAccessTokenStreams(u.ID, tok.ID)
	}
	return c.NoContent(http.StatusNoContent)
}
