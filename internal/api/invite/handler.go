// Package invite implements the user-scope /api/invite/* endpoints used by
// users who hold the canInvite role policy to issue / list / delete / inspect
// invite codes. Admin-scope invite endpoints live under
// internal/api/admin/invite.go and have separate semantics.
package invite

import (
	cryptorand "crypto/rand"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/safemath"
	"github.com/shiroha-a/mk/internal/server/middleware"

	"github.com/shiroha-a/mk/internal/core/role"
)

// ModeratorChecker reports whether a user holds a moderator (or administrator)
// role. Delete uses it to reproduce upstream's moderator bypass (#2812).
//
// 未配線なら「モデレーターではない」に倒れる。**緩む側ではなく厳しい側**なので、
// `recordCriticalWiring` (= 起動を止める配線検査) の対象にはしない。代わりに
// `TestInviteModeratorCheckerIsWired` が router のソースを読んで固定する — 配線を
// 消してもこのパッケージのテストは checker を自分で注入するので緑のまま通る。
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// Handler exposes /api/invite/* handler methods.
type Handler struct {
	repo               repository.RegistrationTicketRepository
	idGen              id.Generator
	rolePolicyProvider role.PolicyProvider
	userRepo           repository.UserRepository
	moderatorChecker   ModeratorChecker
}

// NewHandler returns a Handler wired with the given repository + id generator.
func NewHandler(repo repository.RegistrationTicketRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, idGen: idGen}
}

// SetRolePolicyProvider wires the policy provider used by Create / Limit.
// Nil keeps the gate disabled (= test fixture / wire 未配線).
func (h *Handler) SetRolePolicyProvider(p role.PolicyProvider) {
	h.rolePolicyProvider = p
}

// SetUserRepo wires the user repository used by List to pack the createdBy /
// usedBy UserLite relations (#1776)。未配線なら usedBy は null のまま
// (createdBy は middleware user から常に解決できる)。
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// SetModeratorChecker wires the moderator lookup used by Delete (#2812).
//
// 未配線なら bypass が効かないだけ (= 作成者の未使用の招待しか消せない)。
// **#2812 以前と同じではない** — あちらは作成者なら使用済みでも消せた。
func (h *Handler) SetModeratorChecker(c ModeratorChecker) {
	h.moderatorChecker = c
}

// Create implements POST /api/invite/create. Upstream Misskey TS
// `invite/create.ts` semantics: enforce inviteLimit / inviteLimitCycle as a
// rolling time-window count, set expiresAt from inviteExpirationTime (0 / unset
// = null = 無期限), then persist the ticket.
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	now := time.Now()

	policies := h.policies(user.ID)
	if invLimit, ok := role.PolicyNumber(policies["inviteLimit"]); ok && invLimit > 0 {
		// **`time.Duration(f) * time.Minute` と書かない。** Duration への変換で
		// 小数が切り捨てられてから掛かるので 0.5 分が 0 になる (#2613)。
		cycle := time.Duration(0)
		if v, ok2 := role.PolicyMinutes(policies["inviteLimitCycle"]); ok2 {
			cycle = v
		}
		cutoff := now.Add(time.Duration(safemath.NegateInt64(int64(cycle))))
		sinceID := h.idGen.Generate(cutoff)
		count, err := h.repo.CountByCreatorSince(user.ID, sinceID)
		if err != nil {
			return apierr.JSONInternalError(c)
		}
		if float64(count) >= invLimit {
			return apierr.JSONExceededLimitOfCreateInviteCode(c)
		}
	}

	// inviteExpirationTime (= 分単位) が truthy なら expiresAt を計算する。
	// 0 / 未設定なら null (= 無期限) を維持し、upstream の falsy skip と一致する。
	var expiresAt *time.Time
	if d, ok := role.PolicyMinutes(policies["inviteExpirationTime"]); ok && d > 0 {
		exp := now.Add(d)
		expiresAt = &exp
	}

	ticket := &model.RegistrationTicket{
		ID:          h.idGen.Generate(now),
		Code:        generateInviteCode(),
		CreatedByID: &user.ID,
		ExpiresAt:   expiresAt,
	}
	if err := h.repo.Create(ticket); err != nil {
		return apierr.JSONInternalError(c)
	}

	resp := map[string]any{
		"id":   ticket.ID,
		"code": ticket.Code,
		// upstream InviteCodeEntityService.ts:50 は expiresAt ? toISOString() : null (#1948-10)。
		"expiresAt": entity.ISOMillisPtr(ticket.ExpiresAt),
		"createdBy": entity.PackUserLite(user),
		"usedBy":    nil,
		"usedAt":    nil,
		"used":      false,
	}
	if t, err := h.idGen.ParseTime(ticket.ID); err == nil {
		resp["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return c.JSON(http.StatusOK, resp)
}

// List implements POST /api/invite/list.
func (h *Handler) List(c echo.Context) error {
	user := middleware.GetUser(c)
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
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}

	limit, limitOK := pagination.ResolveLimit(req.Limit, 30, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	tickets, err := h.repo.ListByCreator(user.ID, sinceID, untilID, limit)
	if err != nil {
		// upstream は handler 内で DB error を listing 404 相当ではなく empty
		// array で返している。caller (frontend invite 管理画面) は array length
		// で空チェックするので 500 を返すと UI が壊れる、空 array で graceful
		// degrade する方が drop-in 互換性が高い。
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream invite/list は createdBy / usedBy を UserLite で埋める
	// (InviteCodeEntityService.pack)。list は createdById=me で絞っているので
	// createdBy は常に呼び出し元自身。usedBy は `usedById` を持つ ticket の利用者を batch 解決する
	// (#1776)。frontend MkInviteCode.vue が createdBy/usedBy の avatar/username を
	// 描画するため、ここを null 固定にすると招待管理画面の詳細表示が空になる。
	createdByLite := entity.PackUserLite(user)
	usedByMap := map[string]entity.UserLite{}
	if h.userRepo != nil {
		seen := make(map[string]struct{}, len(tickets))
		ids := make([]string, 0, len(tickets))
		for _, t := range tickets {
			if t.UsedByID == nil {
				continue
			}
			if _, ok := seen[*t.UsedByID]; ok {
				continue
			}
			seen[*t.UsedByID] = struct{}{}
			ids = append(ids, *t.UsedByID)
		}
		if len(ids) > 0 {
			if users, err := h.userRepo.FindManyByIDs(ids); err == nil {
				for _, u := range users {
					usedByMap[u.ID] = entity.PackUserLite(u)
				}
			}
		}
	}

	out := make([]map[string]any, 0, len(tickets))
	for _, t := range tickets {
		var usedBy any
		if t.UsedByID != nil {
			if lite, ok := usedByMap[*t.UsedByID]; ok {
				usedBy = lite
			}
		}
		entry := map[string]any{
			"id":   t.ID,
			"code": t.Code,
			// upstream InviteCodeEntityService.ts:50,54 は expiresAt/usedAt ? toISOString() : null (#1948-10)。
			"expiresAt": entity.ISOMillisPtr(t.ExpiresAt),
			"createdBy": createdByLite,
			"usedBy":    usedBy,
			"usedAt":    entity.ISOMillisPtr(t.UsedAt),
			// **`usedById` ではなく `usedAt` で見る (#2812)。** upstream は
			// `used: !!target.usedAt` (InviteCodeEntityService.ts:55)、mk-go の
			// `admin/invite/list` も `UsedAt != nil` で、ここだけが違っていた。
			//
			// 差が出るのは**確認メール待ちの ticket** — `MarkPending` は `usedAt`
			// だけ立てて `usedById` は nil のまま残す。`used: false` と答えると、
			// 画面は削除ボタンを出すのに `invite/delete` は使用済みとして 400 を
			// 返す (#2812 で使用済みの保護を入れたので、この食い違いが利用者から
			// 見えるようになる)。
			"used": t.UsedAt != nil,
		}
		if ts, err := h.idGen.ParseTime(t.ID); err == nil {
			entry["createdAt"] = ts.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		out = append(out, entry)
	}
	return c.JSON(http.StatusOK, out)
}

// Delete implements POST /api/invite/delete.
//
// 消せるのは**自分が作った未使用の招待**だけ。モデレーターはどちらの判定も
// bypass する (#2812)。upstream `invite/delete.ts` の
// `createdById !== me.id && !isModerator` / `usedAt && !isModerator` と同じ。
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		InviteID string `json:"inviteId"`
	}
	if err := c.Bind(&req); err != nil || req.InviteID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "inviteId is required.", apierr.UUIDInvalidParam))
	}
	ticket, err := h.repo.FindByID(req.InviteID)
	if err != nil {
		// **DB 障害を「削除成功」に化けさせない (#2812)。** 取り消し系で 204 を
		// 返すと、モデレーターは消えたと思って戻り、ticket は生きている。
		// 種別を見ずに潰さないのは #2799 と同じ理由。
		if !repository.IsNotFound(err) {
			return apierr.JSONInternalError(c)
		}
		// 既に削除済 / 存在しない場合は 204 を返す (= idempotent)。**upstream は
		// NO_SUCH_INVITE_CODE を投げるので、ここは意図的な乖離** (#2812 で記述を
		// 是正。それ以前のコメントは「upstream と同様に」と誤って書いていた)。
		return c.NoContent(http.StatusNoContent)
	}
	// モデレーターは所有者でなくても消せる (#2812)。upstream の
	// `ticket.createdById !== me.id && !isModerator` と同じ形。**mk-go はこの
	// bypass を持っておらず、管理画面の招待一覧から他人の招待を消せなかった。**
	// #2805 で承認由来の ticket が `createdById` NULL になったので、消せない行の
	// 割合が増えていた。
	isModerator := h.moderatorChecker != nil && h.moderatorChecker.IsModerator(user.ID)
	if !isModerator {
		if ticket.CreatedByID == nil || *ticket.CreatedByID != user.ID {
			// invite/delete は汎用 ACCESS_DENIED ではなく endpoint 固有 id を使う
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "5eb8d909-2540-4970-90b8-dd6f86088121"))
		}
		// 使用済みの招待は所有者でも消せない (upstream の `ticket.usedAt &&
		// !isModerator`)。**消せると、誰の招待から入ったアカウントなのかを
		// 招待した本人が消せてしまう。** 順序も upstream に合わせる — 所有者
		// 判定を先にしないと、第三者に「その招待は使用済み」と教えることになる。
		if ticket.UsedAt != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("CAN_NOT_DELETE_INVITE_CODE", "You can't delete this invite code.", "ff17af39-000c-4d4e-abdf-848fa30fc1ce"))
		}
	}
	if err := h.repo.Delete(req.InviteID); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// Limit implements POST /api/invite/limit. Returns the remaining slots within
// the current inviteLimitCycle window. Upstream `invite/limit.ts` returns
// `remaining: null` when policies.inviteLimit is falsy (= 無制限), otherwise
// `max(0, inviteLimit - count)`.
func (h *Handler) Limit(c echo.Context) error {
	user := middleware.GetUser(c)
	policies := h.policies(user.ID)
	invLimit, hasLimit := role.PolicyNumber(policies["inviteLimit"])
	if !hasLimit || invLimit <= 0 {
		return c.JSON(http.StatusOK, map[string]any{"remaining": nil})
	}
	cycle := time.Duration(0)
	if v, ok := role.PolicyMinutes(policies["inviteLimitCycle"]); ok {
		cycle = v
	}
	now := time.Now()
	cutoff := now.Add(time.Duration(safemath.NegateInt64(int64(cycle))))
	sinceID := h.idGen.Generate(cutoff)
	count, err := h.repo.CountByCreatorSince(user.ID, sinceID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	limit := safemath.Float64ToInt64(invLimit)
	remaining := safemath.AddInt64(limit, safemath.NegateInt64(count))
	if remaining < 0 {
		remaining = 0
	}
	return c.JSON(http.StatusOK, map[string]any{"remaining": remaining})
}

// policies returns the merged role policy map for userID, or an empty map
// when no provider is wired so callers can safely type-assert keys.
func (h *Handler) policies(userID string) map[string]any {
	if h.rolePolicyProvider == nil {
		return map[string]any{}
	}
	if p := h.rolePolicyProvider.GetUserPolicies(userID); p != nil {
		return p
	}
	return map[string]any{}
}

// generateInviteCode creates an 8-character random invite code over
// `[a-z0-9]`. mk-go 既存実装 (旧 router.go 内 helper) と同 charset / 長さで、
// 移植後も既発行コードの長さ / 文字種分布を維持する。upstream Misskey TS
// は別 charset (`23456789ABCDEFGHJKLMNPQRSTUVWXYZ` + timestamp suffix) を
// 使うので drop-in 互換の観点では別 issue で揃える余地あり。
func generateInviteCode() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	if _, err := io.ReadFull(cryptorand.Reader, b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	for i := range b {
		b[i] = chars[b[i]%byte(len(chars))]
	}
	return string(b)
}

// HasRolePolicyProvider reports whether the role policy provider was wired.
//
// 未配線だと policy が空 map になり、招待の上限が無制限になる。起動時検査に
// 使う (#2683)。
func (h *Handler) HasRolePolicyProvider() bool { return h.rolePolicyProvider != nil }
