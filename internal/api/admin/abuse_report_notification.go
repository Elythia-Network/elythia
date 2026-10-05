package admin

import (
	"net/http"
	"sort"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// packedRecipient mirrors the upstream AbuseReportNotificationRecipient schema:
// the raw recipient fields plus the resolved user (UserLite) / systemWebhook
// (SystemWebhook). Both resolved objects are optional (omitted when unset),
// matching upstream pack (omit on null userId/systemWebhookId).
type packedRecipient struct {
	*model.AbuseReportNotificationRecipient
	// updatedAt は embedded model の time.Time をそのまま JSON 化すると Go 既定の
	// RFC3339Nano (秒ちょうどは .000 を落とし、非 UTC は +09:00 表記) になり、
	// upstream の Date.toISOString() (常に 3 桁ミリ秒 + Z) と乖離する。entity 層と
	// 同じ canonical layout で文字列化し、embedded 側の updatedAt を shadow する。
	UpdatedAt string           `json:"updatedAt"`
	User      *entity.UserLite `json:"user,omitempty"`
	// systemWebhook は upstream SystemWebhookEntityService.pack shape で返す。
	// model 直埋め込みだと updatedAt/latestSentAt が RFC3339Nano になるため
	// packSystemWebhook 経由で .000Z に揃える (#1948-10)。
	SystemWebhook map[string]any `json:"systemWebhook,omitempty"`
}

// packRecipient resolves the recipient's user / systemWebhook for the response
// (upstream AbuseReportNotificationRecipientEntityService.pack).
func (h *Handler) packRecipient(r *model.AbuseReportNotificationRecipient) packedRecipient {
	p := packedRecipient{AbuseReportNotificationRecipient: r}
	p.UpdatedAt = entity.ISOMillis(r.UpdatedAt)
	if r.UserID != nil && *r.UserID != "" && h.userRepo != nil {
		if u, err := h.userRepo.FindByID(*r.UserID); err == nil && u != nil {
			lite := entity.PackUserLite(u)
			p.User = &lite
		}
	}
	if r.SystemWebhookID != nil && *r.SystemWebhookID != "" && h.systemWebhookRepo != nil {
		if w, err := h.systemWebhookRepo.FindByID(*r.SystemWebhookID); err == nil && w != nil {
			p.SystemWebhook = packSystemWebhook(w)
		}
	}
	return p
}

// validateEmailAddress checks that the target user (method=email recipient) has a
// verified email, matching upstream create/update.ts: profile 不在は
// CORRELATION_CHECK_EMAIL、email 未設定/未 verify は EMAIL_ADDRESS_NOT_SET。
// userId 空は検証対象外として skip する。userRepo は NewHandler の必須引数で
// production / test とも常に wired のため nil check は防御的不変条件であり、
// nil 時の skip 経路は通常到達しない。
func (h *Handler) validateEmailAddress(userID *string) (int, map[string]any) {
	if h.userRepo == nil || userID == nil || *userID == "" {
		return 0, nil
	}
	profile, err := h.userRepo.FindProfileByUserID(*userID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。この関数は
		// (status, body) を返すので 500 も同じ形で返す。
		return http.StatusInternalServerError, apierr.InternalError()
	}
	if err != nil || profile == nil {
		return http.StatusBadRequest, apierr.Error("CORRELATION_CHECK_EMAIL", "If \"method\" is email, \"userId\" must be set.", "348bb8ae-575a-6fe9-4327-5811999def8f")
	}
	if profile.Email == nil || *profile.Email == "" || !profile.EmailVerified {
		return http.StatusBadRequest, apierr.Error("EMAIL_ADDRESS_NOT_SET", "Email address is not set.", "7cc1d85e-2f58-fc31-b644-3de8d0d3421f")
	}
	return 0, nil
}

// AbuseReportNotificationRecipientCreate handles POST /api/admin/abuse-report/notification-recipient/create.
func (h *Handler) AbuseReportNotificationRecipientCreate(c echo.Context) error {
	var req struct {
		Name            string  `json:"name"`
		Method          string  `json:"method"`
		IsActive        *bool   `json:"isActive"`
		UserID          *string `json:"userId"`
		SystemWebhookID *string `json:"systemWebhookId"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	// upstream Misskey TS の paramDef:
	//   required: ['isActive', 'name', 'method']
	//   method.enum: ['email', 'webhook']
	// + method='email' で userId 必須、method='webhook' で systemWebhookId 必須
	// の相関 check (packages/backend/src/.../notification-recipient/create.ts、#929)。
	// nil-repo branch より先に validate して、不正リクエストを早期 reject する。
	if req.Name == "" || req.Method == "" || req.IsActive == nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("name / method / isActive are required."))
	}
	if req.Method != "email" && req.Method != "webhook" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("method must be 'email' or 'webhook'."))
	}
	// 相関 check の error code / id は upstream create.ts の meta.errors 定義と
	// 一致 (CORRELATION_CHECK_EMAIL=348bb8ae-..., CORRELATION_CHECK_WEBHOOK=b0c15051-...)。
	if req.Method == "email" && (req.UserID == nil || *req.UserID == "") {
		return c.JSON(http.StatusBadRequest, apierr.Error("CORRELATION_CHECK_EMAIL", "If \"method\" is email, \"userId\" must be set.", "348bb8ae-575a-6fe9-4327-5811999def8f"))
	}
	// method=email では対象 user の email が設定/verify 済みか検証する。
	if req.Method == "email" {
		if code, body := h.validateEmailAddress(req.UserID); code != 0 {
			return c.JSON(code, body)
		}
	}
	if req.Method == "webhook" && (req.SystemWebhookID == nil || *req.SystemWebhookID == "") {
		return c.JSON(http.StatusBadRequest, apierr.Error("CORRELATION_CHECK_WEBHOOK", "If \"method\" is webhook, \"systemWebhookId\" must be set.", "b0c15051-de2d-29ef-260c-9585cddd701a"))
	}
	if h.recipientRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// upstream create.ts は method に対応しない側を null で保存する
	// (userId = email時のみ / systemWebhookId = webhook時のみ)。これにより
	// list の method filter と packer の resolve が method と整合する。
	r := &model.AbuseReportNotificationRecipient{
		ID:        h.idGen.Generate(time.Now()),
		Name:      req.Name,
		Method:    req.Method,
		IsActive:  *req.IsActive,
		UpdatedAt: time.Now(),
	}
	if req.Method == "email" {
		r.UserID = req.UserID
	} else {
		r.SystemWebhookID = req.SystemWebhookID
	}
	if err := h.recipientRepo.Create(r); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.logModeration(c, moderationlog.LogCreateAbuseReportNotificationRecipient, map[string]any{
		"recipientId": r.ID,
		"recipient":   r,
	})
	return c.JSON(http.StatusOK, h.packRecipient(r))
}

// AbuseReportNotificationRecipientDelete handles POST /api/admin/abuse-report/notification-recipient/delete.
func (h *Handler) AbuseReportNotificationRecipientDelete(c echo.Context) error {
	if h.recipientRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if req.ID == "" {
		return c.NoContent(http.StatusNoContent)
	}
	snapshot, _ := h.recipientRepo.FindByID(req.ID)
	if err := h.recipientRepo.Delete(req.ID); err == nil && snapshot != nil {
		h.logModeration(c, moderationlog.LogDeleteAbuseReportNotificationRecipient, map[string]any{
			"recipientId": req.ID,
			"recipient":   snapshot,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// AbuseReportNotificationRecipientList handles POST /api/admin/abuse-report/notification-recipient/list.
func (h *Handler) AbuseReportNotificationRecipientList(c echo.Context) error {
	if h.recipientRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream list.ts は method[] (enum email/webhook) で絞り込む。
	var req struct {
		Method []string `json:"method"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// upstream paramDef は method.items を enum: ['email','webhook'] で制約し、
	// 範囲外は 400 で reject する。create / update と同じく enum 検証を行い、
	// 不正値で「該当 0 件 (200+空)」と誤認させない。
	methodSet := map[string]bool{}
	for _, m := range req.Method {
		if m != "email" && m != "webhook" {
			return c.JSON(http.StatusBadRequest, apierr.InvalidParam("method must be 'email' or 'webhook'."))
		}
		methodSet[m] = true
	}
	rows, err := h.recipientRepo.List()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	out := make([]packedRecipient, 0, len(rows))
	for _, r := range rows {
		if len(methodSet) > 0 && !recipientMatchesMethod(r, methodSet) {
			continue
		}
		out = append(out, h.packRecipient(r))
	}
	// upstream packMany は id 昇順で返す (repo は id DESC なので reverse する)。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return c.JSON(http.StatusOK, out)
}

// recipientMatchesMethod mirrors upstream fetchRecipients: 'email' は
// method=email かつ userId 非 null、'webhook' は method=webhook かつ userId が
// null の行のみ一致させる (counterpart 不整合な legacy 行を除外)。
func recipientMatchesMethod(r *model.AbuseReportNotificationRecipient, methodSet map[string]bool) bool {
	if methodSet["email"] && r.Method == "email" && r.UserID != nil {
		return true
	}
	if methodSet["webhook"] && r.Method == "webhook" && r.UserID == nil {
		return true
	}
	return false
}

// AbuseReportNotificationRecipientShow handles POST /api/admin/abuse-report/notification-recipient/show.
func (h *Handler) AbuseReportNotificationRecipientShow(c echo.Context) error {
	if h.recipientRepo == nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	r, err := h.recipientRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	return c.JSON(http.StatusOK, h.packRecipient(r))
}

// AbuseReportNotificationRecipientUpdate handles POST /api/admin/abuse-report/notification-recipient/update.
//
// upstream paramDef は method を `enum: ['email', 'webhook']` で制約し、
// method='email' なら userId 必須、'webhook' なら systemWebhookId 必須の
// correlation check を持つ (#1108 sweep)。旧 mk-go は Update で method を
// 素通し + correlation check を一切しておらず、admin が無効な method に
// silent 書き換えできた。本 fix で Create と同等の enum / correlation
// validation を追加。
func (h *Handler) AbuseReportNotificationRecipientUpdate(c echo.Context) error {
	if h.recipientRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		ID              string  `json:"id"`
		Name            *string `json:"name"`
		Method          *string `json:"method"`
		IsActive        *bool   `json:"isActive"`
		UserID          *string `json:"userId"`
		SystemWebhookID *string `json:"systemWebhookId"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	// 不存在 ID は早期に 404 で報告する。旧版は before の取得失敗 (= 不存在)
	// を捨てて後続の Update(GORM) に頼っていたが、enum / correlation check が
	// 入った後 (#1108) は「不存在 ID + 不完全 method payload」のケースで
	// correlation_check の 400 が先に発火して NotFound が永遠に返らない
	// edge case が生まれていた。エラー優先順位を「不存在 → 入力不正」の順
	// に揃えて、admin UI / CLI が ID typo を確実に 404 として受け取れる
	// ようにする (sweep follow-up)。
	before, err := h.recipientRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	fields := map[string]any{}
	if req.Name != nil {
		fields["name"] = *req.Name
	}
	if req.Method != nil {
		// upstream enum: ['email', 'webhook']。silent fallback せず 400 reject
		// (PR #1102 RolesUpdate.target と同方針)。
		if *req.Method != "email" && *req.Method != "webhook" {
			return c.JSON(http.StatusBadRequest, apierr.InvalidParam("method must be 'email' or 'webhook'."))
		}
		// correlation check (upstream update.ts と同じ regex 関連 error code)。
		// 後段の UserID / SystemWebhookID で上書きされる可能性があるので、
		// 「最終的に保存される method に対する counterpart 値」をここで
		// 検査する必要がある。method 単体 update でも payload に id しか
		// 来ない場合は既存 row の counterpart を見て判定する。
		userID := req.UserID
		sysWHID := req.SystemWebhookID
		if userID == nil && before != nil {
			userID = before.UserID
		}
		if sysWHID == nil && before != nil {
			sysWHID = before.SystemWebhookID
		}
		if *req.Method == "email" && (userID == nil || *userID == "") {
			return c.JSON(http.StatusBadRequest, apierr.Error("CORRELATION_CHECK_EMAIL", "If \"method\" is email, \"userId\" must be set.", "348bb8ae-575a-6fe9-4327-5811999def8f"))
		}
		// 最終的に method=email になるなら対象 user の email verify を検証する。
		if *req.Method == "email" {
			if code, body := h.validateEmailAddress(userID); code != 0 {
				return c.JSON(code, body)
			}
		}
		if *req.Method == "webhook" && (sysWHID == nil || *sysWHID == "") {
			return c.JSON(http.StatusBadRequest, apierr.Error("CORRELATION_CHECK_WEBHOOK", "If \"method\" is webhook, \"systemWebhookId\" must be set.", "b0c15051-de2d-29ef-260c-9585cddd701a"))
		}
		fields["method"] = *req.Method
		// method 切替時は対応しない側を null に揃える (upstream は full overwrite で
		// userId/systemWebhookId を method に応じて null 化する)。kept 側は
		// req 値優先、無ければ既存値 (userID/sysWHID = req or before)。
		if *req.Method == "email" {
			// userID は上の correlation check で非 nil/非空が保証されている。
			fields["userId"] = *userID
			fields["systemWebhookId"] = nil
		} else {
			fields["systemWebhookId"] = *sysWHID
			fields["userId"] = nil
		}
	} else {
		// method 未変更の partial update: 来た field だけ反映する。
		// 既存 method=email の recipient で userId だけを差し替える場合も、
		// upstream update.ts (method required で常に email 検証が再実行される) と
		// 揃えて対象 user の email verify を検証する。これが無いと method を省いた
		// partial update 経由で未 verify user を email recipient に向けられてしまう。
		if before != nil && before.Method == "email" && req.UserID != nil {
			if code, body := h.validateEmailAddress(req.UserID); code != 0 {
				return c.JSON(code, body)
			}
		}
		// method に合わない側は書かず、本家と同じく常に NULL にする (#3264)。
		// 本家の update は method が必須で、合わない側を NULL で上書きする。
		// 書けてしまうと、外部キーが CASCADE なので、無関係な利用者や System
		// Webhook を消しただけで通知先ごと消える。
		switch before.Method {
		case "email":
			if req.UserID != nil {
				fields["userId"] = *req.UserID
			}
			fields["systemWebhookId"] = nil
		case "webhook":
			if req.SystemWebhookID != nil {
				fields["systemWebhookId"] = *req.SystemWebhookID
			}
			fields["userId"] = nil
		}
	}
	if req.IsActive != nil {
		fields["isActive"] = *req.IsActive
	}
	// upstream は更新のたびに updatedAt を bump する (golden は updatedAt 必須)。
	fields["updatedAt"] = time.Now()
	// GORM Updates(map) は対象なしでも nil を返すため、ここでのエラーは DB 障害
	// 等の真の失敗。NotFound は続く FindByID で検出する。
	if err := h.recipientRepo.Update(req.ID, fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	r, err := h.recipientRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	if before != nil {
		h.logModeration(c, moderationlog.LogUpdateAbuseReportNotificationRecipient, map[string]any{
			"recipientId": req.ID,
			"before":      before,
			"after":       r,
		})
	}
	return c.JSON(http.StatusOK, h.packRecipient(r))
}
