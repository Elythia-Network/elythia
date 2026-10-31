package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/pagination"
	coreabuse "github.com/elythia-network/elythia/internal/core/abuse"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// --- Abuse Report endpoints ---

// AbuseReports handles POST /api/admin/abuse-user-reports.
//
// upstream paramDef (= packages/backend/src/server/api/endpoints/admin/
// abuse-user-reports.ts) で受ける主要 field を frontend (= MkPagination
// + pages/admin/abuses.vue) と互換にする:
//
//   - sinceId / untilId — cursor pagination (#1114 root cause)
//   - state             — 'all' | 'unresolved' | 'resolved'。他の値は全件
//   - reporterOrigin    — 'combined' | 'local' | 'remote'
//   - targetUserOrigin  — 'combined' | 'local' | 'remote'
//   - limit             — default 10 / max 100
//
// 旧 mk-go は `resolved` boolean / `limit` / `offset` しか受けず、frontend が
// 送る untilId を無視して同じ first page を返し続けて MkPagination が末尾
// 検知できず無限ロードになっていた (= 過去 #487 / #488 / #491 / #493 / #520
// / #712 と完全に同 pattern)。
//
// 後方互換: 旧 `resolved` boolean field は引き続き受け付ける (= 旧 client /
// 既存 e2e 互換)。`state` と `resolved` の両方が来た場合は `state` を優先
// する (= upstream paramDef での enum 優先 pattern と同じ)。`state` が 'all'・
// 空文字・null のときだけ `resolved` を見る。
func (h *Handler) AbuseReports(c echo.Context) error {
	var req struct {
		Resolved         *bool      `json:"resolved"`
		State            string     `json:"state"`
		ReporterOrigin   string     `json:"reporterOrigin"`
		TargetUserOrigin string     `json:"targetUserOrigin"`
		SinceID          string     `json:"sinceId"`
		UntilID          string     `json:"untilId"`
		SinceDate        *jsnum.Int `json:"sinceDate"`
		UntilDate        *jsnum.Int `json:"untilDate"`
		Limit            *jsnum.Int `json:"limit"`
		// ReportID は mk-go 独自の additive パラメータ (#2868)。通報の通知から
		// 該当の 1 件へ飛ぶために使う。
		ReportID string `json:"reportId"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.abuseRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// state enum 優先、無ければ legacy boolean fallback。'all' / 空文字列は
	// 「フィルタなし (= resolved 列で絞らない)」を意味する。本家のparamDefは
	// stateをenumにしておらず、知らない値は絞り込まずに返す。本家は独自の
	// resolvedを知らないので、知らないstateはresolvedがあっても全件にする
	// (#3859)。
	resolved := req.Resolved
	switch req.State {
	case "unresolved":
		v := false
		resolved = &v
	case "resolved":
		v := true
		resolved = &v
	case "all", "":
		// 何もしない (= legacy boolean fallback または「全件」)
	default:
		resolved = nil
	}
	// origin enum 検証: 空文字列 / 'combined' / 'local' / 'remote' 以外は reject
	// (= silent fallback で全件返すと「local だけ見たかった」が裏切られて
	// 静かに drop-in regression になるため)。
	if !isValidOrigin(req.ReporterOrigin) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "reporterOrigin must be 'combined', 'local', or 'remote'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if !isValidOrigin(req.TargetUserOrigin) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "targetUserOrigin must be 'combined', 'local', or 'remote'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// reportId 指定時はその 1 件だけを返す (#2868)。
	//
	// **state / origin / cursor の絞りを無視する。** 通知のリンクから開いた
	// ときに「他のモデレーターが先に解決済みにしたので一覧に出ない」が起きると、
	// リンクが役に立たない。1 件を名指しで引く以上、絞り込みは意味を持たない。
	if req.ReportID != "" {
		r, err := h.abuseRepo.FindByID(req.ReportID)
		if err != nil {
			// **DB 障害を not-found に丸めない** (#2792)。
			if !repository.IsNotFound(err) {
				return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
			}
			return c.JSON(http.StatusOK, []packedAbuseReport{})
		}
		if r == nil {
			return c.JSON(http.StatusOK, []packedAbuseReport{})
		}
		ctx := c.Request().Context()
		single := []*model.AbuseUserReport{r}
		return c.JSON(http.StatusOK, []packedAbuseReport{h.packAbuseReport(ctx, r, h.abuseUsers(ctx, single))})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursorIDFirst(req.SinceID, req.UntilID, req.SinceDate.Int64Ptr(), req.UntilDate.Int64Ptr())
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	reports, err := h.abuseRepo.List(resolved, req.ReporterOrigin, req.TargetUserOrigin, sinceID, untilID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// model.AbuseUserReport は createdAt 列を持たず aidx ID 先頭 8 文字に
	// timestamp を埋め込んでいる。frontend (MkAbuseReport.vue) は
	// `<MkTime :time="report.createdAt"/>` を直接読むため、aidx から派生
	// した createdAt 文字列を response に注入する。ShowModerationLogs と
	// 同パターン (#1116)。embedded struct で既存 field を JSON inline、
	// CreatedAt だけ上乗せする。
	// reporter / targetUser / assignee を 1 batch で UserDetailed に pack する (N+1 回避)。
	ctx := c.Request().Context()
	users := h.abuseUsers(ctx, reports)
	out := make([]packedAbuseReport, 0, len(reports))
	for _, r := range reports {
		out = append(out, h.packAbuseReport(ctx, r, users))
	}
	return c.JSON(http.StatusOK, out)
}

// packAbuseReport converts an abuse report into the wire shape of
// admin/abuse-user-reports. users は abuseUsers で batch に pack した利用者。
// System Webhook の本文は形が違うので coreabuse.WebhookPayload で作る (#3260)。
func (h *Handler) packAbuseReport(ctx context.Context, r *model.AbuseUserReport, users map[string]*entity.UserDetailed) packedAbuseReport {
	p := packedAbuseReport{
		ID:             r.ID,
		Comment:        r.Comment,
		Resolved:       r.Resolved,
		ReporterID:     r.ReporterID,
		TargetUserID:   r.TargetUserID,
		AssigneeID:     r.AssigneeID,
		Forwarded:      r.Forwarded,
		ResolvedAs:     r.ResolvedAs,
		ModerationNote: r.ModerationNote,
		// 生 model.User を inline すると usernameLower / inbox 等の内部
		// field が漏れ、UserDetailedNotMe 固有 field も欠ける。UserDetailed
		// で pack する。
		Reporter:   abuseUser(r.Reporter, users),
		TargetUser: abuseUser(r.TargetUser, users),
		Assignee:   abuseUser(r.Assignee, users),
	}
	if s, err := aidxCreatedAtString(h.idGen, r.ID); err == nil {
		p.CreatedAt = s
	} else if !errors.Is(err, ErrIDGenMissing) {
		// idGen は wired されているのに parse 失敗した場合のみログに残す。
		// 非 aidx 形式の legacy ID 等で createdAt が出せない時に frontend
		// 側で「日時の解析が失敗しました。」が出る原因を後追いできるように
		// する (ShowModerationLogs と同 pattern)。
		slog.DebugContext(ctx, "abuseReport: createdAt derive failed",
			"reportId", r.ID, "err", err)
	}
	return p
}

// abuseUsers packs every reporter / targetUser / assignee referenced by the
// reports in one batch, keyed by user ID.
//
// 本家 AbuseUserReportEntityService.packMany は 3 者をまとめて
// packMany(users, null, {schema: 'UserDetailedNotMe'}) で組む。閲覧者が null
// なので、呼んだモデレーターにもフォロワー限定のカウントや moderationNote は
// 出ず、ピン留めも空になる (#3330)。
func (h *Handler) abuseUsers(ctx context.Context, reports []*model.AbuseUserReport) map[string]*entity.UserDetailed {
	seen := map[string]struct{}{}
	users := make([]*model.User, 0, len(reports)*3)
	add := func(u *model.User) {
		if u == nil {
			return
		}
		if _, ok := seen[u.ID]; ok {
			return
		}
		seen[u.ID] = struct{}{}
		users = append(users, u)
	}
	for _, r := range reports {
		add(r.Reporter)
		add(r.TargetUser)
		add(r.Assignee)
	}
	return h.packUsersByID(ctx, nil, users)
}

// packUsersByID batch-loads the profiles of users and packs them with
// packUsersMany, keyed by user ID. profile の取得失敗は致命にせず、profile 無しで組む。
func (h *Handler) packUsersByID(ctx context.Context, viewer *model.User, users []*model.User) map[string]*entity.UserDetailed {
	out := make(map[string]*entity.UserDetailed, len(users))
	if len(users) == 0 {
		return out
	}
	profByID := map[string]*model.UserProfile{}
	if h.userRepo != nil {
		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		if profiles, err := h.userRepo.FindProfilesByUserIDs(ids); err == nil {
			for _, p := range profiles {
				profByID[p.UserID] = p
			}
		}
	}
	packed := h.packUsersMany(ctx, viewer, users, profByID)
	for i, u := range users {
		out[u.ID] = &packed[i]
	}
	return out
}

// abuseUser returns the packed user for u, or nil (-> JSON null) when u is
// nil so the assignee key stays present.
func abuseUser(u *model.User, packed map[string]*entity.UserDetailed) *entity.UserDetailed {
	if u == nil {
		return nil
	}
	return packed[u.ID]
}

// packedAbuseReport mirrors the upstream abuse-user-reports res schema
// (abuse-user-reports.ts:26-89) field-for-field. 旧実装は model.AbuseUserReport
// を embed して生 model.User の relation や targetUserHost/reporterHost 等の
// 内部 field を漏らしていた。明示 struct にして TS res と 1:1 に揃える。
// assignee/assigneeId/resolvedAs は nullable+optional:false なので key 常在
// (null when unset)。createdAt は aidx 由来で legacy 非 aidx ID では omit。
type packedAbuseReport struct {
	ID             string               `json:"id"`
	CreatedAt      string               `json:"createdAt,omitempty"`
	Comment        string               `json:"comment"`
	Resolved       bool                 `json:"resolved"`
	ReporterID     string               `json:"reporterId"`
	TargetUserID   string               `json:"targetUserId"`
	AssigneeID     *string              `json:"assigneeId"`
	Reporter       *entity.UserDetailed `json:"reporter"`
	TargetUser     *entity.UserDetailed `json:"targetUser"`
	Assignee       *entity.UserDetailed `json:"assignee"`
	Forwarded      bool                 `json:"forwarded"`
	ResolvedAs     *string              `json:"resolvedAs"`
	ModerationNote string               `json:"moderationNote"`
}

// isValidOrigin returns true if s is one of the valid origin filter values
// accepted by admin endpoints ('combined' / 'local' / 'remote') or empty.
// 'combined' と 空文字列は「フィルタなし」として等価扱い。
func isValidOrigin(s string) bool {
	switch s {
	case "", "combined", "local", "remote":
		return true
	default:
		return false
	}
}

// ResolveAbuseReport handles POST /api/admin/resolve-abuse-user-report.
//
// upstream paramDef:
//
//	reportId:    string (required)
//	resolvedAs:  enum ['accept', 'reject', null], nullable
//
// 旧 mk-go は `resolvedAs` を `"accept"` 固定にしていたため、admin が
// reject (= 「報告は不適切」判定) を選んでも accept として記録され、
// abuse handling の判定結果が反映されない drop-in regression があった
// (PR #1108 sweep)。本 fix で upstream paramDef 通り `resolvedAs` を
// accept し enum 検証する。
func (h *Handler) ResolveAbuseReport(c echo.Context) error {
	var req struct {
		ReportID   string  `json:"reportId"`
		ResolvedAs *string `json:"resolvedAs"`
	}
	if err := c.Bind(&req); err != nil || req.ReportID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "reportId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.abuseRepo == nil {
		return c.JSON(http.StatusNotFound, apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "ac3794dd-2ce4-d878-e546-73c60c06b398", apierr.KindServer))
	}
	// upstream enum: ['accept', 'reject', null]。null は「未送出」+ JSON null
	// 両方を「resolve したが判定保留」として扱う (= column も null)。
	// 不正値は silent fallback せず 400 で reject (PR #1102 の RolesUpdate.target
	// fix と同方針: 既存 row の resolvedAs が意図せず書き換わる silent
	// corruption を防ぐ)。
	// upstream は resolve 時に assigneeId=moderator.id を記録する。
	fields := map[string]any{"resolved": true}
	if me := middleware.GetUser(c); me != nil {
		fields["assigneeId"] = me.ID
	}
	switch {
	case req.ResolvedAs == nil:
		fields["resolvedAs"] = nil
	case *req.ResolvedAs == "accept":
		fields["resolvedAs"] = "accept"
	case *req.ResolvedAs == "reject":
		fields["resolvedAs"] = "reject"
	case *req.ResolvedAs == "":
		// 明示的空文字も null 扱い (frontend が `null` の代わりに `""` を送る
		// 互換ケース、cf. PR #1102 color clear 経路と同じ workaround)。
		fields["resolvedAs"] = nil
	default:
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "resolvedAs must be 'accept', 'reject', or null.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 存在の確認を UpdateFields に任せない。GORM の Updates は該当行が無くても
	// エラーを返さないので、存在しない ID でも 204 になっていた (#3259)。本家も
	// 先に findOneBy で引いて noSuchAbuseReport を返す。
	report, err := h.abuseRepo.FindByID(req.ReportID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil || report == nil {
		return c.JSON(http.StatusNotFound, apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "ac3794dd-2ce4-d878-e546-73c60c06b398", apierr.KindServer))
	}
	// 更新前の行を、通報の列だけで控える (#3267)。モックは同じポインタを
	// 書き換えるので、UpdateFields の後に report を読むと更新後の値になる。
	// 本家のログも更新前の行を載せる。
	before := abuseReportLogRow(report)
	if err := h.abuseRepo.UpdateFields(req.ReportID, fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// 本家 AbuseReportService.resolve と同じく resolveAbuseReport を残す (#3259)。
	h.logModeration(c, moderationlog.LogResolveAbuseReport, map[string]any{
		"reportId":   req.ReportID,
		"report":     before,
		"resolvedAs": fields["resolvedAs"],
	})
	// upstream AbuseReportService.resolve は notifySystemWebhook('abuseReportResolved')
	// を呼ぶ。best-effort: DB 更新は済んでいるので発火失敗は握り潰す (#1723)。
	h.notifyAbuseReportResolved(c.Request().Context(), req.ReportID)
	return c.NoContent(http.StatusNoContent)
}

// notifyAbuseReportResolved fires the abuseReportResolved system webhook for a
// resolved report. 本家 AbuseReportNotificationService.notifySystemWebhook 相当:
// inactive な notification recipient (method=webhook) が指す systemWebhookId を
// excludes に渡し、残りの active system webhook へ本家と同じ形の本文 (#3260) を
// 配送する。dispatcher / abuseRepo 未配線時は no-op (#1723)。
func (h *Handler) notifyAbuseReportResolved(ctx context.Context, reportID string) {
	if h.systemWebhookDispatcher == nil || h.abuseRepo == nil {
		return
	}
	report, err := h.abuseRepo.FindByID(reportID)
	if err != nil {
		slog.WarnContext(ctx, "abuseReportResolved: load report failed", "reportId", reportID, "err", err)
		return
	}
	// 本文は管理画面の API の形 (packAbuseReport) ではなく、本家の Webhook の形
	// (#3260)。FindByID が利用者 3 人を Preload している。
	body := coreabuse.WebhookPayload(h.entityPacker, report, report.Reporter, report.TargetUser, report.Assignee, h.abuseWebhookLookups(), h.idGen)
	h.systemWebhookDispatcher.DispatchSystemExcluding(
		corewebhook.SystemEventAbuseReportResolved, body, h.inactiveAbuseWebhookIDs())
}

// abuseWebhookLookups returns the lookups for the users in the abuse report
// webhooks. 未配線の repository は nil のまま渡り、その部分を省く。
func (h *Handler) abuseWebhookLookups() coreabuse.UserLookups {
	return coreabuse.UserLookups{Instances: h.instanceRepo, Emojis: h.emojiRepo}
}

// inactiveAbuseWebhookIDs returns the systemWebhookId values of inactive
// abuse-report-notification recipients (method=webhook). 本家 notifySystemWebhook
// の withoutWebhookIds 相当 (= 無効化された recipient 宛は配送しない)。recipientRepo
// 未配線 / 取得失敗時は nil (= excludes 無し)。
func (h *Handler) inactiveAbuseWebhookIDs() []string {
	if h.recipientRepo == nil {
		return nil
	}
	recipients, err := h.recipientRepo.List()
	if err != nil {
		return nil
	}
	var excludes []string
	for _, r := range recipients {
		if r.Method == "webhook" && !r.IsActive && r.SystemWebhookID != nil {
			excludes = append(excludes, *r.SystemWebhookID)
		}
	}
	return excludes
}
