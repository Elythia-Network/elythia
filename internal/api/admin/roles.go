package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/optional"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/effectivepolicy"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"gorm.io/datatypes"
)

// --- Role endpoints ---

// RolesCreate handles POST /api/admin/roles/create.
//
// upstream Misskey TS の paramDef は 14 field を required (一部 nullable) で
// 受け取る (name / description / color / iconUrl / target / condFormula /
// isPublic / isModerator / isAdministrator / isExplorable / asBadge /
// canEditMembersByModerator / displayOrder / policies)。mk-go も drop-in
// 互換のため同 field を accept する (#889)。
//
// model.Role の全 column を実際に persist する (PR #1102 で配線完了)。
// 旧版は policies / condFormula / color / iconUrl / target / canEdit
// MembersByModerator を request で受け取りつつ /dev/null に流していた
// ため、admin UI から policy / 色 / アイコン等を設定しても DB に書かれず
// 「ロール設定が反映されない」現象になっていた。
func (h *Handler) RolesCreate(c echo.Context) error {
	var req struct {
		Name                            *string         `json:"name"`
		Description                     *string         `json:"description"`
		Color                           *string         `json:"color"`
		IconURL                         *string         `json:"iconUrl"`
		Target                          *string         `json:"target"`
		CondFormula                     *map[string]any `json:"condFormula"`
		IsPublic                        *bool           `json:"isPublic"`
		IsModerator                     *bool           `json:"isModerator"`
		IsAdministrator                 *bool           `json:"isAdministrator"`
		IsExplorable                    *bool           `json:"isExplorable"`
		AsBadge                         *bool           `json:"asBadge"`
		CanEditMembersByModerator       *bool           `json:"canEditMembersByModerator"`
		PreserveAssignmentOnMoveAccount *bool           `json:"preserveAssignmentOnMoveAccount"`
		DisplayOrder                    *jsnum.Int      `json:"displayOrder"`
		Policies                        *map[string]any `json:"policies"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream paramDef の required field を非 nil で要求する (= partial
	// payload を 400 で reject、TS 互換、#889)。
	// color / iconUrl は upstream paramDef で nullable: true。JSON で null
	// 送出時に Go の `*string` が nil になり「field 不在」と区別できない
	// ため required check から除外する (= TS は `null` 可で accept、mk-go
	// では実 require は省略するが TS frontend の payload は通る)。
	// nameの空文字は、本家と同じく受ける (#3859)。
	if req.Name == nil ||
		req.Description == nil ||
		req.Target == nil ||
		req.CondFormula == nil ||
		req.IsPublic == nil ||
		req.IsModerator == nil ||
		req.IsAdministrator == nil ||
		req.AsBadge == nil ||
		req.CanEditMembersByModerator == nil ||
		req.DisplayOrder == nil ||
		req.Policies == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Required parameters missing.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	opts := role.CreateOptions{
		Color:                     req.Color,
		IconURL:                   req.IconURL,
		IsPublic:                  *req.IsPublic,
		IsModerator:               *req.IsModerator,
		IsAdministrator:           *req.IsAdministrator,
		AsBadge:                   *req.AsBadge,
		IsExplorable:              req.IsExplorable != nil && *req.IsExplorable,
		DisplayOrder:              int(*req.DisplayOrder),
		CanEditMembersByModerator: *req.CanEditMembersByModerator,
	}
	if req.PreserveAssignmentOnMoveAccount != nil {
		opts.PreserveAssignmentOnMoveAccount = *req.PreserveAssignmentOnMoveAccount
	}
	// Target は upstream paramDef で `enum: ['manual', 'conditional']`。
	// nest.js framework が unknown を 400 で reject するので、mk-go も
	// silent fallback せず 400 を返す (新規 role なので Update 経路ほど
	// 破壊的ではないが、shape を upstream に揃える)。
	switch *req.Target {
	case string(model.RoleTargetManual):
		opts.Target = model.RoleTargetManual
	case string(model.RoleTargetConditional):
		opts.Target = model.RoleTargetConditional
	default:
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "target must be 'manual' or 'conditional'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// CondFormula / Policies は JSON object → datatypes.JSON (= []byte) に Marshal。
	// upstream は object 全体をそのまま column に書くだけで内部構造は consumer 任せ。
	if cf, err := json.Marshal(*req.CondFormula); err == nil {
		opts.CondFormula = cf
	} else {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "condFormula must be a JSON object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if key := invalidRolePolicyKey(*req.Policies); key != "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
			"policy "+key+" has a value of the wrong type.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if pol, err := json.Marshal(*req.Policies); err == nil {
		opts.Policies = pol
	} else {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "policies must be a JSON object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	r, err := h.roleService.Create(*req.Name, *req.Description, opts)
	if err != nil {
		// **クライアント起因の検証失敗を 5xx に潰さない (#3037)。** 管理者に
		// "Internal error." としか出ないと何が悪いか分からないうえ、監視にも
		// 5xx として乗る (#2792 の逆向き)。update 側と同じ応答にすること。
		if errors.Is(err, role.ErrSelfGrantablePrivilege) {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
				selfGrantableRoleMessage, "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	h.logModeration(c, moderationlog.LogCreateRole, map[string]any{
		"roleId": r.ID,
		"role":   r,
	})
	return c.JSON(http.StatusOK, h.packRole(r))
}

// selfGrantableRoleMessage is the 400 body shared by roles/create and
// roles/update.
//
// **2 箇所で同じ文言にする。** 片方だけ直すと、管理者が create と update で
// 違う説明を読むことになる。
//
// **判定の実態に合わせること。** #3037 の 2 周目で `isLocal` /
// `createdLessThan` / 短い `createdMoreThan` / 恒真式 / 未知の型が拒否側に
// 加わったのに、この文面は 1 周目の一覧のままだった。#3045 でも同じことが
// 起きている (年齢の閾値が消えて `createdMoreThan` が長さによらず拒否側に
// なった)。管理者は**自分が使っていない条件の一覧**を読まされ、何が悪いのか
// 分からない。2 度あったので `TestSelfGrantableRoleMessageExplainsEveryLeafCondition`
// で止める。
const selfGrantableRoleMessage = "登録するだけで満たせる条件 (isLocal / createdLessThan)、待てば満たせる条件 (createdMoreThan と not(createdLessThan))、本人が切り替えられる条件 (isBot / isCat / isLocked / isExplorable / フォロワー数 / フォロー数 / 投稿数)、全員に一致する式 (空の and、空の or の否定、中身の無い not の否定)、判定できない未知の条件を使った条件つきロールには、管理者・モデレーターや権限を配る policy を持たせられません。用意できない条件 (isRemote / isSuspended / roleAssignedTo) も、not で包むと新規アカウントがそのまま満たすので同じく使えません。条件で配りたいときは手動ロールを作って roleAssignedTo で参照してください。既存のロールは名前や色だけの更新でもこの検査を通るので、その場合は権限を下ろすか条件を差し替えるか手動ロールに変えてください。"

// invalidRolePolicyKey returns the first policy entry whose value has the wrong
// type, or "" when every entry is acceptable.
//
// **型を見ないと gate が消える (#3037)。** policy の consumer は
// `if limit, ok := role.PolicyNumber(v); ok { ...gate... }` の形なので、
// 数値の policy に文字列が入ると **上限違反で弾かれるのではなく上限そのものが
// 消える** (#2611 と同じ壊れ方)。管理画面は型どおりの値しか送らないが、
// この endpoint を直に 1 回叩けばその状態を作れた。
//
// 期待する形は `{ "<key>": {"useDefault":…, "priority":…, "value":…} }`。
// `useDefault` が真の entry は値を読まないので見ない。**形そのものが違う
// entry は読み飛ばす** — `parseRolePolicies` も同じ扱いで、ここで弾くと
// upstream が entry の形を拡張したときに mk-go だけが設定を拒否する。
func invalidRolePolicyKey(policies map[string]any) string {
	for key, raw := range policies {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if useDefault, ok := entry["useDefault"].(bool); ok && useDefault {
			continue
		}
		value, ok := entry["value"]
		if !ok {
			continue
		}
		if !effectivepolicy.ValidatePolicyValue(key, value) {
			return key
		}
	}
	return ""
}

// invalidDefaultPolicyKey is invalidRolePolicyKey for the flat
// `{ "<key>": <value> }` shape that `roles/update-default-policies` takes.
func invalidDefaultPolicyKey(policies map[string]any) string {
	for key, value := range policies {
		if !effectivepolicy.ValidatePolicyValue(key, value) {
			return key
		}
	}
	return ""
}

// packRole renders a role in the upstream-compatible shape (usersCount /
// createdAt / default-filled policies 等)。raw model.Role を返すとこれらが欠落
// する。usersCount は active assignment 数 (per-role count、role は少数なので
// N+1 でも許容、upstream pack も per-role count する)。
func (h *Handler) packRole(r *model.Role) map[string]any {
	return entity.PackRole(r, h.roleService.CountAssignedUsers(r.ID), h.idGen, role.DefaultPolicies())
}

// RolesShow handles POST /api/admin/roles/show.
func (h *Handler) RolesShow(c echo.Context) error {
	var req struct {
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	r, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "07dc7d34-c0d8-49b7-96c6-db3ce64ee0b3"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.packRole(r))
}

// RolesList handles POST /api/admin/roles/list.
func (h *Handler) RolesList(c echo.Context) error {
	// upstream admin/roles/list.ts は lastUsedAt DESC で返す (#2061)。
	roles, err := h.roleService.ListByLastUsed()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	out := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		out = append(out, h.packRole(r))
	}
	return c.JSON(http.StatusOK, out)
}

// RolesUpdate handles POST /api/admin/roles/update.
//
// upstream Misskey TS の paramDef は 15 field を accept (roleId 以外はすべて
// optional partial update)。旧 mk-go は 5 field しか accept しておらず、
// admin UI で **policies / color / iconUrl / target / condFormula / asBadge /
// isExplorable / displayOrder / canEditMembersByModerator / preserveAssignment
// OnMoveAccount** を変えても DB に書かれず「ロール設定が反映されない」現象
// になっていた (PR #1102)。特に policies field の欠落は「canPublicNote 等の
// ポリシーが UI 上で trueに見えても実際は default 値のまま」という drop-in
// regression 重大度高めの bug。本 fix で upstream 15 field 全部を pipe する。
func (h *Handler) RolesUpdate(c echo.Context) error {
	var req struct {
		RoleID                          string                    `json:"roleId"`
		Name                            *string                   `json:"name"`
		Description                     *string                   `json:"description"`
		Color                           optional.Nullable[string] `json:"color"`
		IconURL                         optional.Nullable[string] `json:"iconUrl"`
		Target                          *string                   `json:"target"`
		CondFormula                     *map[string]any           `json:"condFormula"`
		IsModerator                     *bool                     `json:"isModerator"`
		IsAdministrator                 *bool                     `json:"isAdministrator"`
		IsPublic                        *bool                     `json:"isPublic"`
		IsExplorable                    *bool                     `json:"isExplorable"`
		AsBadge                         *bool                     `json:"asBadge"`
		CanEditMembersByModerator       *bool                     `json:"canEditMembersByModerator"`
		PreserveAssignmentOnMoveAccount *bool                     `json:"preserveAssignmentOnMoveAccount"`
		DisplayOrder                    *jsnum.Int                `json:"displayOrder"`
		Policies                        *map[string]any           `json:"policies"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	before, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "cd23ef55-09ad-428a-ac61-95a45e124b32"))
		}
		return apierr.JSONInternalError(c)
	}
	fields := map[string]any{}
	if req.Name != nil {
		fields["name"] = *req.Name
	}
	if req.Description != nil {
		fields["description"] = *req.Description
	}
	// 本家と同じく、null は列を null で消す (#3369)。欠けは変えない。以前は null と
	// 欠けを見分けずに変えなかったので、同梱の frontend が色やアイコンを空にして
	// null を送っても消えなかった。空文字も null で消す扱いは以前から残す (#1102。
	// 本家は空文字のまま保存する)。
	if req.Color.Present {
		if req.Color.Value == nil || *req.Color.Value == "" {
			fields["color"] = nil
		} else {
			fields["color"] = *req.Color.Value
		}
	}
	if req.IconURL.Present {
		if req.IconURL.Value == nil || *req.IconURL.Value == "" {
			fields["iconUrl"] = nil
		} else {
			fields["iconUrl"] = *req.IconURL.Value
		}
	}
	if req.Target != nil {
		// upstream paramDef は `enum: ['manual', 'conditional']` で nest.js
		// framework が unknown 値を 400 で reject する。mk-go も silent
		// fallback (旧版は unknown → manual) せずに 400 を返す方が、frontend
		// の typo 等で **既存 conditional role が意図せず manual に書き換わる
		// silent corruption** を防げる。
		switch *req.Target {
		case string(model.RoleTargetManual):
			fields["target"] = model.RoleTargetManual
		case string(model.RoleTargetConditional):
			fields["target"] = model.RoleTargetConditional
		default:
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "target must be 'manual' or 'conditional'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
	}
	if req.CondFormula != nil {
		cf, err := json.Marshal(*req.CondFormula)
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "condFormula must be a JSON object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		// []byte のまま渡すと、simple protocol の接続では jsonb の列に bytea として
		// 送られて失敗する (#3369)。update-default-policies と同じく datatypes.JSON にする。
		fields["condFormula"] = datatypes.JSON(cf)
	}
	if req.IsModerator != nil {
		fields["isModerator"] = *req.IsModerator
	}
	if req.IsAdministrator != nil {
		fields["isAdministrator"] = *req.IsAdministrator
	}
	if req.IsPublic != nil {
		fields["isPublic"] = *req.IsPublic
	}
	if req.IsExplorable != nil {
		fields["isExplorable"] = *req.IsExplorable
	}
	if req.AsBadge != nil {
		fields["asBadge"] = *req.AsBadge
	}
	if req.CanEditMembersByModerator != nil {
		fields["canEditMembersByModerator"] = *req.CanEditMembersByModerator
	}
	if req.PreserveAssignmentOnMoveAccount != nil {
		fields["preserveAssignmentOnMoveAccount"] = *req.PreserveAssignmentOnMoveAccount
	}
	if req.DisplayOrder != nil {
		fields["displayOrder"] = int(*req.DisplayOrder)
	}
	if req.Policies != nil {
		if key := invalidRolePolicyKey(*req.Policies); key != "" {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
				"policy "+key+" has a value of the wrong type.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		pol, err := json.Marshal(*req.Policies)
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "policies must be a JSON object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		fields["policies"] = datatypes.JSON(pol)
	}
	if len(fields) == 0 {
		// 全フィールドが nil = 何も変更しないリクエスト。before == after の
		// 無意味な log を書かずに早期 return する。
		return c.NoContent(http.StatusNoContent)
	}
	after, err := h.roleService.UpdateFields(req.RoleID, fields)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "cd23ef55-09ad-428a-ac61-95a45e124b32"))
		}
		// create と同じ理由 (#3037)。**更新後の姿で判定する**ので、
		// 「条件つきに変える」「管理者を立てる」「条件を差し替える」の
		// どれ 1 つでもここへ来る。
		if errors.Is(err, role.ErrSelfGrantablePrivilege) {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
				selfGrantableRoleMessage, "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		return apierr.JSONInternalError(c)
	}
	h.logModeration(c, moderationlog.LogUpdateRole, map[string]any{
		"roleId": req.RoleID,
		"before": before,
		"after":  after,
	})
	return c.NoContent(http.StatusNoContent)
}

// RolesDelete handles POST /api/admin/roles/delete.
func (h *Handler) RolesDelete(c echo.Context) error {
	var req struct {
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 削除直後だと role 情報を取れないので事前に snapshot を取って log info に含める
	snapshot, _ := h.roleService.Show(req.RoleID)
	if err := h.roleService.Delete(req.RoleID); err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "de0d6ecd-8e0a-4253-88ff-74bc89ae3d45"))
		}
		return apierr.JSONInternalError(c)
	}
	h.logModeration(c, moderationlog.LogDeleteRole, map[string]any{
		"roleId": req.RoleID,
		"role":   snapshot,
	})
	return c.NoContent(http.StatusNoContent)
}

// RolesAssign handles POST /api/admin/roles/assign.
func (h *Handler) RolesAssign(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
		RoleID string `json:"roleId"`
		// upstream assign.ts paramDef は expiresAt: { type:'integer', nullable }
		// = epoch ms。frontend も epoch ms (number) を送るため *int64 で受ける。
		// 以前は *string + RFC3339 parse だったが、JSON number を *string に
		// bind すると unmarshal error → 400 となり期限付き role 付与が常に
		// 失敗していた (#1542)。本家と同じく、整数を小数の形で書いた値
		// (1.0e12など)も受ける (#3859)。
		ExpiresAt *jsnum.Int `json:"expiresAt"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId and roleId are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// canEditMembersByModerator gate (upstream assign.ts:69-76)。role lookup →
	// gate の順で、moderator (非 administrator) は canEditMembersByModerator が
	// 立つ role しか付け外しできない。NO_SUCH_ROLE / ACCESS_DENIED の UUID は
	// assign 固有のもの。
	if handled, err := h.requireCanEditRoleMembers(c, req.RoleID, "6503c040-6af4-4ed9-bf07-f2dd16678eab", "25b5bc31-dc79-4ebd-9bd2-c84978fd052c"); handled {
		return err
	}
	// upstream assign.ts:77-81: 対象 user 不在なら NO_SUCH_USER (#1542)。
	if h.userRepo != nil {
		if _, err := h.userRepo.FindByID(req.UserID); err != nil {
			if !repository.IsNotFound(err) {
				// **DB 障害を not-found に丸めない** (#2792)。
				return apierr.JSONInternalError(c)
			}
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "558ea170-f653-4700-94d0-5a818371d0df"))
		}
	}
	var expiresAt *time.Time
	// 本家はexpiresAtを真偽値として見る(ps.expiresAt && ...)ので、0は期限の
	// 無い付与になる (#3859)。
	if ms := req.ExpiresAt.Int64Ptr(); ms != nil && *ms != 0 {
		t := time.UnixMilli(*ms)
		// upstream assign.ts:83-85: 過去 (現在以下) の expiresAt は no-op で
		// return する (過去日時付与の無効化)。
		if !t.After(time.Now()) {
			return c.NoContent(http.StatusNoContent)
		}
		expiresAt = &t
	}
	if err := h.roleService.Assign(req.UserID, req.RoleID, expiresAt); err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "6503c040-6af4-4ed9-bf07-f2dd16678eab"))
		}
		if err == role.ErrAlreadyAssigned {
			return c.JSON(http.StatusConflict, apierr.Error("ALREADY_ASSIGNED", "Role already assigned.", "67d8689c-25c6-435f-8eed-6ea68e5e53e9"))
		}
		return apierr.JSONInternalError(c)
	}
	h.logRoleAssignment(c, moderationlog.LogAssignRole, req.UserID, req.RoleID)
	return c.NoContent(http.StatusNoContent)
}

// requireCanEditRoleMembers enforces the upstream canEditMembersByModerator
// gate shared by admin/roles/assign and /unassign. It looks up the role first
// (responding NO_SUCH_ROLE with the per-endpoint error id when missing) and then
// denies moderators that may not edit the role's members, while administrators
// (and root) are always allowed.
//
// The returned handled flag reports whether a response was already written: when
// true the caller must return the accompanying error immediately; when false the
// caller may proceed. echo の c.JSON は成功時 nil を返すため bool で「応答を書い
// たか」を伝える (err だけでは deny を short-circuit できない)。
//
// 認可判定 (viewer-based) なので service 層ではなく handler 層に置く。viewer が
// 取得できない場合は administrator とみなさず ACCESS_DENIED に倒す (fail-closed)。
// NO_SUCH_ROLE / ACCESS_DENIED の UUID は assign と unassign で異なるため呼び
// 出し側から渡す。
func (h *Handler) requireCanEditRoleMembers(c echo.Context, roleID, noSuchRoleID, accessDeniedID string) (handled bool, err error) {
	r, err := h.roleService.Show(roleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return true, c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", noSuchRoleID))
		}
		return true, apierr.JSONInternalError(c)
	}
	isAdmin := false
	if me := middleware.GetUser(c); me != nil {
		isAdmin = h.roleService.IsAdministrator(me.ID)
	}
	// **管理者権限を配るロールは管理者しか付け外しできない (#3037)。**
	//
	// `canEditMembersByModerator` は「モデレーターがメンバーを編集してよいか」
	// しか見ない。**管理者ロールにそれを立てるのは管理画面のチェックボックス
	// 1 つ**で、立った瞬間からモデレーターは自分自身にそのロールを付けられる
	// = 管理者へ昇格できる。ロールの作成・更新は管理者専用 (`RequireAdmin`)
	// なのに、付け外しだけがこの穴で抜けていた。
	//
	// **unassign も塞ぐ。** 管理者ロールを外せるなら、モデレーターが管理者を
	// 降格させて (あるいは全管理者を降ろして) 実質の最上位になれる。
	//
	// **upstream も同じ状態だが、そちらに合わせない。** `assign.ts:69-76` は
	// `canEditMembersByModerator` だけを見る。ここは mk-go を厳しい側に倒して
	// `docs/divergence.md` に記録する (プロジェクトの既定方針)。
	if r.IsAdministrator && !isAdmin {
		return true, c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Only administrators can edit members of an administrator role.", accessDeniedID))
	}
	// **間接的に特権を配るロールも管理者しか付け外しできない
	// (#3037 レビュー 2 周目)。**
	//
	// 上の判定は「そのロール自身が管理者ロールか」しか見ない。条件つきロールが
	// `roleAssignedTo` でこのロールを参照していると、**権限を持たない素の
	// ロールを配るだけで管理者になれる**。`checkConditionalPrivilege` の側も
	// 「利用者本人が条件を満たせるか」しか見ないので、2 つの判定の隙間を通る。
	if !isAdmin {
		indirect, err := h.roleService.RoleGrantsPrivilegeIndirectly(roleID)
		if err != nil {
			// **判定できないなら通さない (#2792)。** ここは「特権を配らない
			// ことを確かめてから触る」判定なので、fail-open にすると
			// ロールを列挙できない窓で昇格が成立する。
			slog.Error("admin: cannot determine whether the role grants privileges indirectly", "roleId", roleID, "err", err)
			return true, apierr.JSONInternalError(c)
		}
		if indirect {
			return true, c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED",
				"Only administrators can edit members of a role that grants privileges through a conditional role.", accessDeniedID))
		}
	}
	if r.CanEditMembersByModerator {
		return false, nil
	}
	if isAdmin {
		return false, nil
	}
	return true, c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Only administrators can edit members of the role.", accessDeniedID))
}

// RolesUnassign handles POST /api/admin/roles/unassign.
func (h *Handler) RolesUnassign(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId and roleId are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// canEditMembersByModerator gate (upstream unassign.ts:71-78)。role 不在は
	// assign 同様 NO_SUCH_ROLE を先に返す (TS と同順)。NO_SUCH_ROLE /
	// ACCESS_DENIED の UUID は unassign 固有のもの。
	if handled, err := h.requireCanEditRoleMembers(c, req.RoleID, "6e519036-a70d-4c76-b679-bc8fb18194e2", "24636eee-e8c1-493e-94b2-e16ad401e262"); handled {
		return err
	}
	// upstream unassign.ts:80-84: 対象 user 不在なら NO_SUCH_USER (#1542)。
	if h.userRepo != nil {
		if _, err := h.userRepo.FindByID(req.UserID); err != nil {
			if !repository.IsNotFound(err) {
				// **DB 障害を not-found に丸めない** (#2792)。
				return apierr.JSONInternalError(c)
			}
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "2b730f78-1179-461b-88ad-d24c9af1a5ce"))
		}
	}
	if err := h.roleService.Unassign(req.UserID, req.RoleID); err != nil {
		if err == role.ErrNotAssigned {
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_ASSIGNED", "Role not assigned.", "b9060ac7-5c94-4da4-9f55-2047c953df44"))
		}
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	h.logRoleAssignment(c, moderationlog.LogUnassignRole, req.UserID, req.RoleID)
	return c.NoContent(http.StatusNoContent)
}

// RolesUsers handles POST /api/admin/roles/users.
func (h *Handler) RolesUsers(c echo.Context) error {
	var req struct {
		RoleID    string     `json:"roleId"`
		Limit     *jsnum.Int `json:"limit"`
		SinceID   string     `json:"sinceId"`
		UntilID   string     `json:"untilId"`
		SinceDate *jsnum.Int `json:"sinceDate"`
		UntilDate *jsnum.Int `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate.Int64Ptr(), req.UntilDate.Int64Ptr())
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}

	assignments, err := h.roleService.ListByRole(req.RoleID, untilID, sinceID, limit)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "224eff5e-2488-4b18-b3e7-f50d94421648"))
		}
		return apierr.JSONInternalError(c)
	}

	// Profile を per-row 引いていた N+1 を 1 batch query に集約 (#503 と同じ動機)。
	ids := make([]string, 0, len(assignments))
	for _, a := range assignments {
		if a.User != nil {
			ids = append(ids, a.User.ID)
		}
	}
	// Profile fetch 失敗は handler 全体の致命扱いではないので、空 map で
	// fallback する (各 user は profile=nil で pack される)。ShowUsers と同方針。
	profiles, _ := h.userRepo.FindProfilesByUserIDs(ids)
	profileByUser := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		profileByUser[p.UserID] = p
	}

	// Misskey TS は admin/roles/users の返却を { id, createdAt, user } の配列で
	// 包むため互換のため同じ envelope に揃える (UI の Roles 詳細ページが
	// assignment.user を直接参照している)。createdAt は assignment.id (ULID) から
	// 復元 (User と同じく ID 由来 timestamp)。
	kept := make([]*model.RoleAssignment, 0, len(assignments))
	users := make([]*model.User, 0, len(assignments))
	for _, a := range assignments {
		if a.User == nil {
			// role_assignment は残っているのに user 行が消えているデータ不整合。
			// 結果件数だけ silent に減らすと debug が困難なので警告ログを出して
			// admin tooling 側で清掃の signal にする (#598 review item 1)。
			slog.Warn("admin/roles/users: dangling role assignment",
				"assignmentId", a.ID, "userId", a.UserID, "roleId", a.RoleID)
			continue
		}
		kept = append(kept, a)
		users = append(users, a.User)
	}
	// upstream users.ts:95 は packMany(users, me, {schema:'UserDetailed'})。
	// packAdminUser を使うと email / signins / roleAssigns 等の admin 専用
	// field が read:admin:roles scope に漏れる (show-user の admin guard 迂回)。
	// ShowUsers と同様 UserDetailed に揃えて過剰露出を防ぐ (#1822)。ピン留め・
	// 移行先・instance・絵文字・関係もまとめて埋め、自分自身は MeDetailed (#3330)。
	ctx := c.Request().Context()
	viewer := middleware.GetUser(c)
	packed := h.packUsersMany(ctx, viewer, users, profileByUser)
	result := make([]map[string]any, 0, len(kept))
	for i, a := range kept {
		createdAt := ""
		if t, err := h.idGen.ParseTime(a.ID); err == nil {
			createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		// upstream users.ts:101 は expiresAt: assign.expiresAt?.toISOString() ?? null
		// を含める (#1542)。nil は JSON null。
		var expiresAt any
		if a.ExpiresAt != nil {
			expiresAt = a.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		result = append(result, map[string]any{
			"id":        a.ID,
			"createdAt": createdAt,
			"user":      h.selfPacker.Pack(ctx, packed[i], a.User, profileByUser[a.User.ID], viewer),
			"expiresAt": expiresAt,
		})
	}
	return c.JSON(http.StatusOK, result)
}

// RolesUpdateDefaultPolicies handles POST /api/admin/roles/update-default-policies.
func (h *Handler) RolesUpdateDefaultPolicies(c echo.Context) error {
	var req struct {
		Policies map[string]any `json:"policies"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// policiesの欠けとnullを拒否する (#3859)。nilのmapをそのまま書くと、
	// jsonbの列にJSONのnullが入り、既定のpolicyが全部消えていた。
	if req.Policies == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "policies is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// **値の型を見る (#3037)。** ここは既定 policy そのものなので、数値の
	// policy に文字列が入ると**全利用者でその上限が消える**。
	if key := invalidDefaultPolicyKey(req.Policies); key != "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
			"policy "+key+" has a value of the wrong type.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream update-default-policies.ts は更新前後の policies を取得して
	// moderationLogService.log('updateServerSettings', {before, after}) を記録する
	// (#1542)。before snapshot を Update 前に取得する。
	beforeMeta, _ := h.metaRepo.Fetch()
	// Meta の policies フィールドを更新。policies は jsonb 列なので decoded map を
	// そのまま渡すと UPDATE が型不一致で失敗する (#1823)。coerceMetaJSONBFields で
	// datatypes.JSON ([]byte) に変換してから渡す (RolesCreate/Update の Marshal と
	// 同じ動機、UpdateMeta 経路と共通化)。
	policyFields := map[string]any{"policies": req.Policies}
	coerceMetaJSONBFields(policyFields)
	if err := h.metaRepo.Update(policyFields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// moderation log (before/after policies)。TS は policies のみを記録する。
	if afterMeta, err := h.metaRepo.Fetch(); err == nil {
		var beforePolicies any
		if beforeMeta != nil {
			beforePolicies = beforeMeta.Policies
		}
		h.logModeration(c, moderationlog.LogUpdateServerSettings, map[string]any{
			"before": beforePolicies,
			"after":  afterMeta.Policies,
		})
	}
	return c.NoContent(http.StatusNoContent)
}
