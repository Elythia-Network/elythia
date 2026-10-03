package notes

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"

	"github.com/shiroha-a/mk/internal/core/role"
)

// SetDraftRepo attaches a NoteDraftRepository for draft operations.
func (h *Handler) SetDraftRepo(r repository.NoteDraftRepository) {
	h.draftRepo = r
}

// DraftsList handles POST /api/notes/drafts/list.
func (h *Handler) DraftsList(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.draftRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream drafts/list.ts は limit(default30/max100) + sinceId/untilId +
	// sinceDate/untilDate + scheduled(boolean) を受ける (#1538)。NoteDraft.id は
	// aidx で時刻を内包するので、sinceDate/untilDate は id cursor に正規化して
	// keyset pagination に乗せる (#1948-19)。
	var req struct {
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		Scheduled *bool  `json:"scheduled"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 30, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	drafts, err := h.draftRepo.ListByUser(user.ID, sinceID, untilID, req.Scheduled, limit)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// 全 draft の fileIds をまとめて 1 query で解決し N+1 を回避する。
	allFileIDs := []string{}
	for _, d := range drafts {
		allFileIDs = append(allFileIDs, []string(d.FileIDs)...)
	}
	fileByID := h.resolveDraftFileMap(allFileIDs)
	out := make([]map[string]any, len(drafts))
	for i, d := range drafts {
		// drafts は viewer 自身のものなので user を埋める (repo は Preload しない)。
		d.User = user
		out[i] = h.packDraft(c.Request().Context(), user, d, fileByID)
	}
	return c.JSON(http.StatusOK, out)
}

// DraftsCreate handles POST /api/notes/drafts/create.
// upstream `notes/drafts/create.ts` 互換で scheduledAt / isActuallyScheduled
// を受け入れ、scheduledNoteLimit policy gate + 遅延 queue enqueue を行う
// (#1040)。
func (h *Handler) DraftsCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.draftRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		Text                *string    `json:"text"`
		CW                  *string    `json:"cw"`
		Visibility          string     `json:"visibility"`
		VisibleUserIDs      []string   `json:"visibleUserIds"`
		LocalOnly           bool       `json:"localOnly"`
		ReactionAcceptance  *string    `json:"reactionAcceptance"`
		FileIDs             []string   `json:"fileIds"`
		ReplyID             *string    `json:"replyId"`
		RenoteID            *string    `json:"renoteId"`
		ChannelID           *string    `json:"channelId"`
		Hashtag             *string    `json:"hashtag"`
		Poll                *draftPoll `json:"poll"`
		ScheduledAt         *int64     `json:"scheduledAt"`
		IsActuallyScheduled bool       `json:"isActuallyScheduled"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	// visibility / reactionAcceptance enum 検証 (notes/create と共有、#1934)。
	// draft 経由でも upstream drafts/create.ts:161,168 の enum 制約に揃え、不正値が
	// scheduled-note 処理経由で投稿されるのを防ぐ。
	if err := validateNoteEnums(req.Visibility, req.ReactionAcceptance); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), apierr.UUIDInvalidParam))
	}
	if req.Visibility == "" {
		req.Visibility = "public"
	}
	// scheduledAt の validation (#1040)。upstream は ms epoch (`integer`)
	// で受け取って Date 化、time.Now() より将来であることを要求する。
	// isActuallyScheduled=false なら scheduledAt は単なる「下書きとして保持
	// する希望時刻」扱いで、queue enqueue / future check は行わない。
	now := time.Now()
	var scheduledAt *time.Time
	if req.ScheduledAt != nil {
		t := time.UnixMilli(*req.ScheduledAt)
		scheduledAt = &t
	}
	if req.IsActuallyScheduled {
		if scheduledAt == nil {
			// Misskey は SCHEDULED_AT_* に create / update で別 id を割り当てるため endpoint 固有 id を inline で返す
			return c.JSON(http.StatusBadRequest, apierr.Error("SCHEDULED_AT_REQUIRED", "scheduledAt is required when isActuallyScheduled is true.", "15e28a55-e74c-4d65-89b7-8880cdaaa87d"))
		}
		if !scheduledAt.After(now) {
			return c.JSON(http.StatusBadRequest, apierr.Error("SCHEDULED_AT_MUST_BE_IN_FUTURE", "scheduledAt must be in the future.", "e4bed6c9-017e-4934-aed0-01c22cc60ec1"))
		}
	}
	// noteDraftLimit role policy gate (#1029)。policyProvider は #1026 で
	// 配線済 (timeline gate と共用)。CountByUser で current 件数を取得。
	if h.policyProvider != nil {
		policies := h.policyProvider.GetUserPolicies(user.ID)
		if limit, ok := role.PolicyNumber(policies["noteDraftLimit"]); ok && limit >= 0 {
			count, err := h.draftRepo.CountByUser(user.ID)
			if err != nil {
				return apierr.JSONInternalError(c)
			}
			if float64(count) >= limit {
				return apierr.JSONTooManyNoteDrafts(c)
			}
		}
		// scheduledNoteLimit gate (#1040)。isActuallyScheduled draft の数を
		// 集計して上限と比較する。upstream `NoteDraftService.create` と同 logic。
		if req.IsActuallyScheduled {
			if limit, ok := role.PolicyNumber(policies["scheduledNoteLimit"]); ok && limit >= 0 {
				count, err := h.draftRepo.CountScheduledByUser(user.ID)
				if err != nil {
					return apierr.JSONInternalError(c)
				}
				if float64(count) >= limit {
					return c.JSON(http.StatusBadRequest, apierr.Error("TOO_MANY_SCHEDULED_NOTES", "You cannot create scheduled notes any more.", "22ae69eb-09e3-4541-a850-773cfa45e693"))
				}
			}
		}
	}
	// fileIds は全て呼出ユーザー所有でなければ NO_SUCH_FILE (upstream NoteDraftService)。
	if !h.allDraftFilesOwned(req.FileIDs, user.ID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "Some files are not found.", "b6992544-63e7-67f0-fa7f-32444b1b5306"))
	}
	// 既に期限切れの poll は作成不可 (CANNOT_CREATE_ALREADY_EXPIRED_POLL)。
	if pollAlreadyExpired(req.Poll, now) {
		return c.JSON(http.StatusBadRequest, apierr.Error("CANNOT_CREATE_ALREADY_EXPIRED_POLL", "Cannot create an already expired poll.", "04da457d-b083-4055-9082-955525eda5a5"))
	}
	// reply/renote target の存在・可視性・pure-renote 検証 (#2017)。create endpoint
	// 固有の code/id (drafts/create.ts meta.errors) で 400 を返す。error 文字列を
	// この handler 内に直接置くことで entitycompat の error-id gate が正しい endpoint
	// に紐付けられる (package-level var だと textual に別 handler へ誤割当される)。
	if status, body := h.validateDraftReplyRenote(user, req.ReplyID, req.RenoteID, req.ChannelID, req.Visibility, draftReplyRenoteErrs{
		noSuchRenote:   apierr.Error("NO_SUCH_RENOTE_TARGET", "No such renote target.", "b5c90186-4ab0-49c8-9bba-a1f76c282ba4"),
		noSuchReply:    apierr.Error("NO_SUCH_REPLY_TARGET", "No such reply target.", "749ee0f6-d3da-459a-bf02-282e2da4292c"),
		pureRenote:     apierr.Error("CANNOT_RENOTE_TO_A_PURE_RENOTE", "You can not Renote a pure Renote.", "fd4cc33e-2a37-48dd-99cc-9b806eb2031a"),
		replySpecified: apierr.Error("CANNOT_REPLY_TO_SPECIFIED_VISIBILITY_NOTE_WITH_EXTENDED_VISIBILITY", "You cannot reply to a specified visibility note with extended visibility.", "ed940410-535c-4d5e-bfa3-af798671e93c"),
	}); status != 0 {
		return c.JSON(status, body)
	}
	draft := &model.NoteDraft{
		ID:                  h.idGen.Generate(now),
		UserID:              user.ID,
		Text:                req.Text,
		CW:                  req.CW,
		Visibility:          req.Visibility,
		VisibleUserIDs:      model.StringArray(req.VisibleUserIDs),
		LocalOnly:           req.LocalOnly,
		ReactionAcceptance:  req.ReactionAcceptance,
		FileIDs:             model.StringArray(req.FileIDs),
		ReplyID:             req.ReplyID,
		RenoteID:            req.RenoteID,
		ChannelID:           req.ChannelID,
		Hashtag:             req.Hashtag,
		ScheduledAt:         scheduledAt,
		IsActuallyScheduled: req.IsActuallyScheduled,
	}
	applyDraftPoll(draft, req.Poll)
	if err := h.draftRepo.Create(draft); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// scheduled の場合は遅延 queue に投入 (= upstream の `schedule(draft)`
	// と等価、delay = scheduledAt - now)。enqueue 失敗時は draft を残しつつ
	// 500 を返し、frontend に再試行を促す (draft 残しは upstream と同じく
	// 削除しない方が観測可能で安全)。
	if req.IsActuallyScheduled && h.scheduledNoteEnqueuer != nil {
		delay := scheduledAt.Sub(now)
		if err := h.scheduledNoteEnqueuer.EnqueuePostScheduledNote(
			queue.PostScheduledNotePayload{NoteDraftID: draft.ID},
			driver.WithProcessIn(delay),
		); err != nil {
			return apierr.JSONInternalError(c)
		}
	}
	draft.User = user
	// upstream res は { createdDraft: NoteDraft } で包む。
	return c.JSON(http.StatusOK, map[string]any{"createdDraft": h.packDraft(c.Request().Context(), user, draft, nil)})
}

// DraftsUpdate handles POST /api/notes/drafts/update.
// scheduledAt / isActuallyScheduled の変更を upstream `notes/drafts/update`
// と同 logic で取り扱い、変更があれば旧 delayed task を clear → 必要なら
// 新 scheduledAt で再 enqueue する (#1045 Phase 2-C)。
func (h *Handler) DraftsUpdate(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.draftRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		DraftID             string     `json:"draftId"`
		Text                *string    `json:"text"`
		CW                  *string    `json:"cw"`
		Visibility          string     `json:"visibility"`
		VisibleUserIDs      *[]string  `json:"visibleUserIds"`
		LocalOnly           *bool      `json:"localOnly"`
		ReactionAcceptance  *string    `json:"reactionAcceptance"`
		FileIDs             []string   `json:"fileIds"`
		ReplyID             *string    `json:"replyId"`
		RenoteID            *string    `json:"renoteId"`
		ChannelID           *string    `json:"channelId"`
		Hashtag             *string    `json:"hashtag"`
		Poll                *draftPoll `json:"poll"`
		ScheduledAt         *int64     `json:"scheduledAt"`
		IsActuallyScheduled *bool      `json:"isActuallyScheduled"`
	}
	if err := c.Bind(&req); err != nil || req.DraftID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("draftId is required."))
	}
	// visibility / reactionAcceptance enum 検証 (drafts/create と共有、#1934)。"" /
	// nil は「変更しない」セマンティクスなので許容し、非空の不正値のみ弾く。paramDef
	// 相当なので draft 不在 (404) より先に 400 を返す。
	if err := validateNoteEnums(req.Visibility, req.ReactionAcceptance); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), apierr.UUIDInvalidParam))
	}
	draft, err := h.draftRepo.FindByIDAndUser(req.DraftID, user.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.NoSuchNoteDraft())
	}
	if req.Text != nil {
		draft.Text = req.Text
	}
	if req.CW != nil {
		draft.CW = req.CW
	}
	if req.Visibility != "" {
		draft.Visibility = req.Visibility
	}
	if req.VisibleUserIDs != nil {
		draft.VisibleUserIDs = model.StringArray(*req.VisibleUserIDs)
	}
	if req.LocalOnly != nil {
		draft.LocalOnly = *req.LocalOnly
	}
	if req.ReactionAcceptance != nil {
		draft.ReactionAcceptance = req.ReactionAcceptance
	}
	if req.FileIDs != nil {
		// fileIds 指定時は全て所有 file でなければ NO_SUCH_FILE。
		if !h.allDraftFilesOwned(req.FileIDs, user.ID) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "Some files are not found.", "b6992544-63e7-67f0-fa7f-32444b1b5306"))
		}
		draft.FileIDs = model.StringArray(req.FileIDs)
	}
	if req.ReplyID != nil {
		draft.ReplyID = req.ReplyID
	}
	if req.RenoteID != nil {
		draft.RenoteID = req.RenoteID
	}
	if req.ChannelID != nil {
		draft.ChannelID = req.ChannelID
	}
	if req.Hashtag != nil {
		draft.Hashtag = req.Hashtag
	}
	// reply/renote はリクエストで明示指定された場合のみ検証する。upstream update は
	// data.replyId/renoteId が undefined のとき検証 block を skip するので、既存 draft の
	// 値を再検証して編集不能にしない (#2017)。code/id は update endpoint 固有
	// (drafts/update.ts meta.errors)。error 文字列を handler 内に直接置く理由は
	// DraftsCreate 側のコメント参照。
	if status, body := h.validateDraftReplyRenote(user, req.ReplyID, req.RenoteID, req.ChannelID, req.Visibility, draftReplyRenoteErrs{
		noSuchRenote:   apierr.Error("NO_SUCH_RENOTE", "No such renote.", "64929870-2540-4d11-af41-3b484d78c956"),
		noSuchReply:    apierr.Error("NO_SUCH_REPLY", "No such reply.", "c4721841-22fc-4bb7-ad3d-897ef1d375b5"),
		pureRenote:     apierr.Error("CANNOT_RENOTE", "Cannot renote.", "76cc5583-5a14-4ad3-8717-0298507e32db"),
		replySpecified: apierr.Error("CANNOT_REPLY_TO_SPECIFIED_NOTE_WITH_EXTENDED_VISIBILITY", "You cannot reply to a specified visibility note with extended visibility.", "ed940410-535c-4d5e-bfa3-af798671e93c"),
	}); status != 0 {
		return c.JSON(status, body)
	}
	// scheduled 関連 field の変更検出 (#1045 Phase 2-C)。リクエストに
	// scheduledAt / isActuallyScheduled が含まれている場合のみ削除 + 再
	// enqueue する。それ以外は既存 draft 状態を維持。
	scheduleChanged := false
	now := time.Now()
	if req.Poll != nil {
		if pollAlreadyExpired(req.Poll, now) {
			return c.JSON(http.StatusBadRequest, apierr.Error("CANNOT_CREATE_ALREADY_EXPIRED_POLL", "Cannot create an already expired poll.", "04da457d-b083-4055-9082-955525eda5a5"))
		}
		applyDraftPoll(draft, req.Poll)
	}
	if req.ScheduledAt != nil {
		t := time.UnixMilli(*req.ScheduledAt)
		draft.ScheduledAt = &t
		scheduleChanged = true
	}
	if req.IsActuallyScheduled != nil {
		draft.IsActuallyScheduled = *req.IsActuallyScheduled
		scheduleChanged = true
	}
	if scheduleChanged && draft.IsActuallyScheduled {
		// 新しく schedule する / 既に scheduled だった draft の scheduledAt が
		// 変わった場合は validation が必要。
		if draft.ScheduledAt == nil {
			// notes/drafts/update は create とは別の SCHEDULED_AT_* id を持つ
			return c.JSON(http.StatusBadRequest, apierr.Error("SCHEDULED_AT_REQUIRED", "scheduledAt is required when isActuallyScheduled is true.", "fe9737d5-cc41-498c-af9d-149207307530"))
		}
		if !draft.ScheduledAt.After(now) {
			return c.JSON(http.StatusBadRequest, apierr.Error("SCHEDULED_AT_MUST_BE_IN_FUTURE", "scheduledAt must be in the future.", "ed1a6673-d0d1-4364-aaae-9bf3f139cbc5"))
		}
	}
	_ = h.draftRepo.Update(draft)
	// schedule 変更時は旧 delayed task を clear して、新 scheduledAt が
	// 立っていれば新 task を enqueue する。clear は best-effort で fail
	// しても draft 更新自体は成功で返す (= log のみ、frontend は draft
	// 更新の永続化を確認できる)。
	if scheduleChanged && h.scheduledNoteEnqueuer != nil {
		// **捨てずに log へ残す。** 500 にはしない (draft の更新は成功して
		// いるので、そこを失敗にすると「保存できたのに失敗と表示される」)。
		// ただし黙って捨てると、古い job が残って**取り消したはずの時刻で
		// 公開される**事故の手掛かりが 1 つも残らない。
		if err := h.scheduledNoteEnqueuer.ClearScheduledNote(draft.ID); err != nil {
			slog.Warn("notes/drafts/update: 旧 delayed task を消せなかった",
				"draftId", draft.ID, "err", err)
		}
		if draft.IsActuallyScheduled && draft.ScheduledAt != nil {
			delay := draft.ScheduledAt.Sub(now)
			if err := h.scheduledNoteEnqueuer.EnqueuePostScheduledNote(
				queue.PostScheduledNotePayload{NoteDraftID: draft.ID},
				driver.WithProcessIn(delay),
			); err != nil {
				return apierr.JSONInternalError(c)
			}
		}
	}
	draft.User = user
	// upstream res は { updatedDraft: NoteDraft } で包む。
	return c.JSON(http.StatusOK, map[string]any{"updatedDraft": h.packDraft(c.Request().Context(), user, draft, nil)})
}

// draftPoll mirrors the upstream notes/drafts poll param object.
type draftPoll struct {
	Choices      []string `json:"choices"`
	Multiple     bool     `json:"multiple"`
	ExpiresAt    *int64   `json:"expiresAt"`
	ExpiredAfter *int64   `json:"expiredAfter"`
}

// draftReplyRenoteErrs holds the 4 reply/renote error responses that differ
// between drafts/create.ts and drafts/update.ts. renote-visibility / reply-invisible
// / reply-pure-renote are identical between the two endpoints (#2017)。
type draftReplyRenoteErrs struct {
	noSuchRenote   map[string]any
	noSuchReply    map[string]any
	pureRenote     map[string]any // renote target が pure renote
	replySpecified map[string]any
}

// validateDraftReplyRenote validates the draft's reply/renote targets, mirroring
// upstream NoteDraftService.validate (renote → reply order)。replyID/renoteID は
// **リクエストで明示指定された値**を渡すこと (upstream は data.replyId != null の
// ときだけ検証し、既存 draft の値は再検証しない、#2017)。errs は endpoint 固有の
// code/id。戻り値 status==0 は検証成功。noteRepo/viewer 未配線時は skip。
func (h *Handler) validateDraftReplyRenote(viewer *model.User, replyID, renoteID, channelID *string, visibility string, errs draftReplyRenoteErrs) (int, map[string]any) {
	if h.noteRepo == nil || viewer == nil {
		return 0, nil
	}
	if renoteID != nil {
		t, err := h.noteRepo.FindByIDWithUser(*renoteID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。この関数は
			// (status, body) を返すので 500 も同じ形で返す。
			return http.StatusInternalServerError, apierr.InternalError()
		}
		if err != nil || t == nil {
			return http.StatusBadRequest, errs.noSuchRenote
		}
		if corenote.IsPureRenote(t) {
			return http.StatusBadRequest, errs.pureRenote
		}
		// renote 対象の作者に block されていたら拒否 (upstream blockingsRepository.exists、#2039)。
		if h.draftBlockedBy(t.UserID, viewer.ID) {
			return http.StatusBadRequest, apierr.Error("YOU_HAVE_BEEN_BLOCKED", "You have been blocked by this user.", "b390d7e1-8a5e-46ed-b625-06271cafd3d3")
		}
		// renote 対象が他人の followers note / specified note は (renote 経由の公開
		// 拡散になるため) 拒否する。upstream は isVisibleForMe ではなくこの 2 条件のみ。
		if (t.Visibility == model.NoteVisibilityFollowers && t.UserID != viewer.ID) || t.Visibility == model.NoteVisibilitySpecified {
			return http.StatusBadRequest, apierr.Error("CANNOT_RENOTE_DUE_TO_VISIBILITY", "You can not Renote due to target visibility.", "be9529e9-fe72-4de0-ae43-0b363c4938af")
		}
		// channel note を別 channel / channel 外へ renote する場合、対象 channel が
		// 外部 renote を許可していなければ拒否 (upstream の renote.channelId check、#2039)。
		if status, body := h.validateRenoteChannel(t.ChannelID, channelID); status != 0 {
			return status, body
		}
	}
	if replyID != nil {
		t, err := h.noteRepo.FindByIDWithUser(*replyID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。この関数は
			// (status, body) を返すので 500 も同じ形で返す。
			return http.StatusInternalServerError, apierr.InternalError()
		}
		if err != nil || t == nil {
			return http.StatusBadRequest, errs.noSuchReply
		}
		// 可視性チェックを pure-renote より先に行う (mk-go の existence-oracle hardening
		// doctrine、note_create_service の Devin #270 と同方針)。upstream NoteDraftService は
		// pure-renote を先に見るが、不可視 note の「pure-renote か否か」を error code で
		// 漏らさないよう mk-go では invisible を優先する。error code 自体は upstream 互換。
		// queryService 未配線時は fail-closed で invisible 扱い。
		if h.queryService == nil || !h.queryService.CanSee(viewer, t) {
			return http.StatusBadRequest, apierr.Error("CANNOT_REPLY_TO_AN_INVISIBLE_NOTE", "You cannot reply to an invisible Note.", "b98980fa-3780-406c-a935-b6d0eeee10d1")
		}
		if corenote.IsPureRenote(t) {
			return http.StatusBadRequest, apierr.Error("CANNOT_REPLY_TO_A_PURE_RENOTE", "You can not reply to a pure Renote.", "3ac74a84-8fd5-4bb0-870f-01804f82ce15")
		}
		if t.Visibility == model.NoteVisibilitySpecified && visibility != string(model.NoteVisibilitySpecified) {
			return http.StatusBadRequest, errs.replySpecified
		}
		// reply 対象の作者に block されていたら拒否 (#2039)。
		if h.draftBlockedBy(t.UserID, viewer.ID) {
			return http.StatusBadRequest, apierr.Error("YOU_HAVE_BEEN_BLOCKED", "You have been blocked by this user.", "b390d7e1-8a5e-46ed-b625-06271cafd3d3")
		}
	}
	// draft 自身の channelId が指定されていれば存在 (非 archived) 確認 (#2039)。
	if channelID != nil && *channelID != "" && h.channelRepo != nil {
		ch, err := h.channelRepo.FindByID(*channelID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。この関数は
			// (status, body) を返すので 500 も同じ形で返す。
			return http.StatusInternalServerError, apierr.InternalError()
		}
		if err != nil || ch == nil || ch.IsArchived {
			return http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "b1653923-5453-4edc-b786-7c4f39bb0bbb")
		}
	}
	return 0, nil
}

// draftBlockedBy reports whether targetUserID blocks the viewer (upstream
// blockingsRepository.exists({blockerId: target, blockeeId: me})、#2039)。
// 自分自身 / blockingRepo 未配線時は false。
func (h *Handler) draftBlockedBy(targetUserID, viewerID string) bool {
	if targetUserID == viewerID || h.blockingRepo == nil {
		return false
	}
	blocked, err := h.blockingRepo.Exists(targetUserID, viewerID)
	return err == nil && blocked
}

// validateRenoteChannel enforces the upstream renote.channelId check: renote 対象が
// channel note で、draft が同 channel 以外 (= 外部/別 channel) へ renote する場合、
// 対象 channel が allowRenoteToExternal でなければ拒否する (#2039)。status==0 は OK。
func (h *Handler) validateRenoteChannel(renoteChannelID, draftChannelID *string) (int, map[string]any) {
	if renoteChannelID == nil || *renoteChannelID == "" || h.channelRepo == nil {
		return 0, nil
	}
	if draftChannelID != nil && *draftChannelID == *renoteChannelID {
		return 0, nil // 同一 channel への renote は対象外
	}
	ch, err := h.channelRepo.FindByID(*renoteChannelID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。この関数は
		// (status, body) を返すので 500 も同じ形で返す。
		return http.StatusInternalServerError, apierr.InternalError()
	}
	if err != nil || ch == nil {
		return http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "b1653923-5453-4edc-b786-7c4f39bb0bbb")
	}
	if !ch.AllowRenoteToExternal {
		return http.StatusBadRequest, apierr.Error("CANNOT_RENOTE_TO_EXTERNAL", "Cannot Renote to External.", "ed1952ac-2d26-4957-8b30-2deda76bedf7")
	}
	return 0, nil
}

// allDraftFilesOwned reports whether every fileID exists and is owned by
// userID (upstream NoteDraftService requires files.length === fileIds.length).
// Unwired driveFileRepo (tests) skips the check.
func (h *Handler) allDraftFilesOwned(fileIDs []string, userID string) bool {
	if len(fileIDs) == 0 || h.driveFileRepo == nil {
		return true
	}
	rows, err := h.driveFileRepo.FindByIDs(fileIDs)
	if err != nil {
		return false
	}
	owned := make(map[string]bool, len(rows))
	for _, f := range rows {
		if f.UserID != nil && *f.UserID == userID {
			owned[f.ID] = true
		}
	}
	for _, fid := range fileIDs {
		if !owned[fid] {
			return false
		}
	}
	return true
}

// pollAlreadyExpired reports whether the poll's expiresAt is in the past.
func pollAlreadyExpired(p *draftPoll, now time.Time) bool {
	return p != nil && p.ExpiresAt != nil && time.UnixMilli(*p.ExpiresAt).Before(now)
}

// applyDraftPoll writes poll fields into the draft. nil clears the poll
// (hasPoll=false); non-nil sets hasPoll=true + the choices/multiple/expiry.
func applyDraftPoll(d *model.NoteDraft, p *draftPoll) {
	if p == nil {
		d.HasPoll = false
		d.PollChoices = model.StringArray{}
		d.PollMultiple = false
		d.PollExpiresAt = nil
		d.PollExpiredAfter = nil
		return
	}
	d.HasPoll = true
	d.PollChoices = model.StringArray(p.Choices)
	d.PollMultiple = p.Multiple
	d.PollExpiredAfter = p.ExpiredAfter
	if p.ExpiresAt != nil {
		t := time.UnixMilli(*p.ExpiresAt)
		d.PollExpiresAt = &t
	} else {
		d.PollExpiresAt = nil
	}
}

// DraftsDelete handles POST /api/notes/drafts/delete.
func (h *Handler) DraftsDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.draftRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		DraftID string `json:"draftId"`
	}
	if err := c.Bind(&req); err != nil || req.DraftID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("draftId is required."))
	}
	// upstream notes/drafts/delete.ts は存在チェックして無ければ
	// `NO_SUCH_NOTE_DRAFT` を返す。silent 204 だと frontend が削除確認 UI
	// で「該当 draft が無い」フィードバックを出せないので、本家挙動に合わせて
	// 404 を返すよう修正 (#688 review follow-up)。
	// Delete は RowsAffected を返すので、Find → Delete の 2 query 化を避け
	// つつ TOCTOU race も発生しない (DB 1 文で atomic に「あれば削除」)。
	rowsAffected, err := h.draftRepo.Delete(req.DraftID, user.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if rowsAffected == 0 {
		return c.JSON(http.StatusNotFound, apierr.NoSuchNoteDraft())
	}
	// delete に成功したら旧 delayed task も best-effort で clear する
	// (#1045 Phase 2-C)。upstream は `clearSchedule(draftId)` を fire
	// and forget で呼ぶ。clear 失敗は 204 を阻害しない (= 残った task が
	// fire しても processor 側で draft 不在 silent skip で済む)。
	if h.scheduledNoteEnqueuer != nil {
		// 上と同じ。消せなくても 204 は返すが、手掛かりは残す。
		if err := h.scheduledNoteEnqueuer.ClearScheduledNote(req.DraftID); err != nil {
			slog.Warn("notes/drafts/delete: 旧 delayed task を消せなかった",
				"draftId", req.DraftID, "err", err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// DraftsCount handles POST /api/notes/drafts/count.
func (h *Handler) DraftsCount(c echo.Context) error {
	user := middleware.GetUser(c)
	// upstream drafts/count.ts は res type:'number' で裸の数値を返す。旧実装は
	// {"count": N} オブジェクトを返していた (#1538)。
	if h.draftRepo == nil {
		return c.JSON(http.StatusOK, 0)
	}
	count, _ := h.draftRepo.CountByUser(user.ID)
	return c.JSON(http.StatusOK, count)
}

// SetThreadMutingRepo wires the NoteThreadMutingRepository used by the
// thread-muting create/delete endpoints (#1538). The notes/state read path is
// wired separately on the query service.
func (h *Handler) SetThreadMutingRepo(r repository.NoteThreadMutingRepository) {
	h.threadMutingRepo = r
}

// threadIDForMute resolves the thread key a mute row should use: the note's
// threadId when set, else the note id itself (mirrors upstream getNote +
// note.threadId ?? note.id).
func threadIDForMute(n *model.Note) string {
	if n.ThreadID != nil && *n.ThreadID != "" {
		return *n.ThreadID
	}
	return n.ID
}

// ThreadMutingCreate handles POST /api/notes/thread-muting/create.
//
// 旧実装は noteId 検証後 204 を返すだけで note_thread_muting に永続化しておらず
// (notes/state の isMutedThread 読み取り経路は実装済みなのに) スレッドミュートが
// 機能していなかった (#1538)。本家 thread-muting/create.ts 同様 note を解決し
// (可視性 gate 無し)、threadId(無ければ id)で冪等に muting 行を作る。
func (h *Handler) ThreadMutingCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		NoteID string `json:"noteId"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "noteId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.threadMutingRepo == nil || h.noteRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	note, err := h.noteRepo.FindByID(req.NoteID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil || note == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "5ff67ada-ed3b-2e71-8e87-a1a421e177d2"))
	}
	threadID := threadIDForMute(note)
	// upstream 1853898d71 で重複時の error が定義された。#1538 では UNIQUE 制約違反に
	// よる 500 を避けるため既存行を成功扱い (204) にしていたが、upstream は 400
	// ALREADY_MUTING を返すので wire 上のレスポンスが分岐していた。upstream に揃える。
	exists, err := h.threadMutingRepo.Exists(user.ID, threadID)
	if err != nil {
		// **DB 障害を「既にミュート済み」に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if exists {
		return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_MUTING", "You are already muting that thread.", "c146e22d-1141-4b31-b28d-176371014d18"))
	}
	if err := h.threadMutingRepo.Create(&model.NoteThreadMuting{
		ID:       h.idGen.Generate(time.Now()),
		UserID:   user.ID,
		ThreadID: threadID,
	}); err != nil {
		// Exists 側と同じ apierr.InternalError() に揃える。同じ endpoint の同じ
		// code / id なのに kind が枝ごとに違うと、クライアントの分類が枝で変わる。
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// ThreadMutingDelete handles POST /api/notes/thread-muting/delete.
func (h *Handler) ThreadMutingDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		NoteID string `json:"noteId"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "noteId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.threadMutingRepo == nil || h.noteRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	note, err := h.noteRepo.FindByID(req.NoteID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil || note == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "bddd57ac-ceb3-b29d-4334-86ea5fae481a"))
	}
	// Delete は冪等 (該当行が無くてもエラーにしない)。
	if err := h.threadMutingRepo.Delete(user.ID, threadIDForMute(note)); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.NoContent(http.StatusNoContent)
}

// PollsRecommendation handles POST /api/notes/polls/recommendation.
//
// 旧実装は常に [] を返すスタブだった (#1538)。本家 recommendation.ts 同様、
// ローカル public で未期限切れの poll のうち、自分の投稿でない / 未投票 /
// 投稿者を mute していないものを noteId DESC で返す (excludeChannels で
// チャンネル poll を除外可)。可視性は public 限定なので追加 gate は不要。
func (h *Handler) PollsRecommendation(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Limit  *int `json:"limit"`
		Offset int  `json:"offset"`
		// upstream recommendation.ts:35 は excludeChannels を boolean で受け、
		// true のとき全 channel poll を除外する (#1765。以前は []string だった)。
		ExcludeChannels bool `json:"excludeChannels"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if h.pollRepo == nil || h.noteRepo == nil {
		return c.JSON(http.StatusOK, []entity.NoteEntity{})
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	var muted []string
	if h.mutingRepo != nil {
		muted, _ = h.mutingRepo.ListMuteeIDs(user.ID)
	}
	noteIDs, err := h.pollRepo.ListRecommendation(user.ID, muted, req.ExcludeChannels, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if len(noteIDs) == 0 {
		return c.JSON(http.StatusOK, []entity.NoteEntity{})
	}
	notes, err := h.noteRepo.FindManyByIDsWithUser(noteIDs)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// FindManyByIDsWithUser は noteIDs の順序 (noteId DESC) を保持する。
	return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, user))
}

// packDraft renders a NoteDraft into the upstream-compatible shape. 旧実装は
// text/cw/visibility/localOnly/fileIds しか出さず、reactionAcceptance /
// visibleUserIds / replyId / renoteId / channelId / hashtag / poll /
// scheduledAt / isActuallyScheduled / user / files を欠いていた。
//
// reply / renote / channel の解決済みオブジェクトは現状 null で返す (ID は
// 出すので frontend は再取得可能)。files は driveFileRepo から解決する。
// packDraft packs a draft. fileByID, when non-nil, is a pre-resolved DriveFile
// map (List uses it to batch all drafts' files in one query, avoiding N+1).
// When nil, files are resolved per-draft (single create/update/show path).
func (h *Handler) packDraft(ctx context.Context, viewer *model.User, d *model.NoteDraft, fileByID map[string]entity.DriveFileEntity) map[string]any {
	const tsFmt = "2006-01-02T15:04:05.000Z"
	visibleUserIDs := []string(d.VisibleUserIDs)
	if visibleUserIDs == nil {
		visibleUserIDs = []string{}
	}
	fileIDs := []string(d.FileIDs)
	if fileIDs == nil {
		fileIDs = []string{}
	}
	result := map[string]any{
		"id":                  d.ID,
		"userId":              d.UserID,
		"text":                d.Text,
		"cw":                  d.CW,
		"visibility":          d.Visibility,
		"localOnly":           d.LocalOnly,
		"reactionAcceptance":  d.ReactionAcceptance,
		"visibleUserIds":      visibleUserIDs,
		"fileIds":             fileIDs,
		"replyId":             d.ReplyID,
		"renoteId":            d.RenoteID,
		"channelId":           d.ChannelID,
		"hashtag":             d.Hashtag,
		"isActuallyScheduled": d.IsActuallyScheduled,
		"files":               h.draftFiles(fileIDs, fileByID),
	}
	// upstream NoteDraftEntityService.pack (detail) は replyId/renoteId set 時のみ
	// reply/renote を解決し (見つからなければ null)、未設定なら key を省略する
	// (#1948-19)。reply は detail:false、renote は detail:true 相当。
	if d.ReplyID != nil {
		// reply は upstream で detail:false (clippedCount/poll/myReaction/nested embed 省略、#2016)。
		result["reply"] = h.packDraftNote(ctx, viewer, *d.ReplyID, false)
	}
	if d.RenoteID != nil {
		// renote は detail:true。
		result["renote"] = h.packDraftNote(ctx, viewer, *d.RenoteID, true)
	}
	// channel は channelId set かつ channel が見つかったときだけ {id,name,color,
	// isSensitive,allowRenoteToExternal,userId} を出力、それ以外は key 省略 (#1948-19)。
	if d.ChannelID != nil {
		if ch := h.packDraftChannel(*d.ChannelID); ch != nil {
			result["channel"] = ch
		}
	}
	if t, err := h.idGen.ParseTime(d.ID); err == nil {
		result["createdAt"] = t.UTC().Format(tsFmt)
	}
	// upstream schema: scheduledAt は number (ms epoch)。createdAt は ISO 文字列
	// だが scheduledAt は getTime() なので number で返す。
	if d.ScheduledAt != nil {
		result["scheduledAt"] = d.ScheduledAt.UnixMilli()
	} else {
		result["scheduledAt"] = nil
	}
	// poll: hasPoll のときだけ object、それ以外は null (upstream schema)。
	if d.HasPoll {
		choices := []string(d.PollChoices)
		if choices == nil {
			choices = []string{}
		}
		poll := map[string]any{
			"choices":      choices,
			"multiple":     d.PollMultiple,
			"expiredAfter": d.PollExpiredAfter,
		}
		// upstream は expiresAt: pollExpiresAt?.toISOString() で、nil のとき key を
		// 省略する (undefined。null ではない、#1948-19)。
		if d.PollExpiresAt != nil {
			poll["expiresAt"] = d.PollExpiresAt.UTC().Format(tsFmt)
		}
		result["poll"] = poll
	} else {
		result["poll"] = nil
	}
	if d.User != nil {
		result["user"] = entity.PackUserLite(d.User)
	}
	return result
}

// packDraftNote resolves a draft's reply/renote target Note and packs it for the
// viewer, returning nil when the note no longer exists (upstream
// nullIfEntityNotFound). noteRepo 未配線時も nil (#1948-19)。
func (h *Handler) packDraftNote(ctx context.Context, viewer *model.User, noteID string, detail bool) any {
	if h.noteRepo == nil {
		return nil
	}
	n, err := h.noteRepo.FindByIDWithUser(noteID)
	if err != nil || n == nil {
		return nil
	}
	// packMany は timeline 用 hard-mute drop を行い非可視 note を漏らすため使わない。
	// packReferencedNote が hard-mute skip + visibility hide を行う (#1948-19)。
	// reply は detail:false、renote は detail:true で pack する (#2016)。
	return h.packReferencedNote(ctx, n, viewer, detail)
}

// packDraftChannel resolves a draft's channel into the upstream NoteDraft.channel
// summary {id,name,color,isSensitive,allowRenoteToExternal,userId}, or nil when
// the channel is missing / channelRepo unwired so the caller omits the key (#1948-19).
func (h *Handler) packDraftChannel(channelID string) map[string]any {
	if h.channelRepo == nil {
		return nil
	}
	ch, err := h.channelRepo.FindByID(channelID)
	if err != nil || ch == nil {
		return nil
	}
	return map[string]any{
		"id":                    ch.ID,
		"name":                  ch.Name,
		"color":                 ch.Color,
		"isSensitive":           ch.IsSensitive,
		"allowRenoteToExternal": ch.AllowRenoteToExternal,
		"userId":                ch.UserID,
	}
}

// draftFiles resolves a draft's fileIds to packed DriveFiles in order. When
// fileByID is provided (List batch path) it is used directly; otherwise a
// per-draft FindByIDs query runs. Returns a non-nil slice ([] when empty).
func (h *Handler) draftFiles(fileIDs []string, fileByID map[string]entity.DriveFileEntity) []entity.DriveFileEntity {
	out := make([]entity.DriveFileEntity, 0, len(fileIDs))
	if len(fileIDs) == 0 {
		return out
	}
	if fileByID == nil {
		fileByID = h.resolveDraftFileMap(fileIDs)
	}
	for _, fid := range fileIDs {
		if f, ok := fileByID[fid]; ok {
			out = append(out, f)
		}
	}
	return out
}

// resolveDraftFileMap fetches+packs DriveFiles for the given ids into a map.
func (h *Handler) resolveDraftFileMap(fileIDs []string) map[string]entity.DriveFileEntity {
	m := map[string]entity.DriveFileEntity{}
	if len(fileIDs) == 0 || h.driveFileRepo == nil {
		return m
	}
	rows, err := h.driveFileRepo.FindByIDs(fileIDs)
	if err != nil {
		return m
	}
	for _, f := range rows {
		m[f.ID] = entity.PackDriveFile(f, h.idGen)
	}
	return m
}
