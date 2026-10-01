package users

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/meself"
	"github.com/shiroha-a/mk/internal/api/notehide"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/core/notesfilter"
	"github.com/shiroha-a/mk/internal/core/reaction"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"gorm.io/gorm"
)

// SetAbuseRepo attaches the abuse report repository.
func (h *Handler) SetAbuseRepo(r repository.AbuseReportRepository) {
	h.abuseRepo = r
}

// Relation handles POST /api/users/relation.
// upstream Misskey TS `users/relation.ts` (= getRelation in
// server/api/common/getRelation.ts) と同 semantics で viewer と target の
// follow / follow-request / block / mute / renote-mute の関係性を返す。
//
// 認証は router.go の `middleware.RequireAuth()` で前段強制されているため
// production では viewer == nil は到達不可。下の `viewer == nil` branch は
// defensive guard + 単体テスト用 (= handler を直接叩く test path)。
//
// repo が wired されていない (= legacy handler test) では該当 field を false
// に保つので production runtime には影響しない (router で必ず wired される)。
func (h *Handler) Relation(c echo.Context) error {
	viewer := middleware.GetUser(c)
	// upstream relation.ts:113-127 は userId を oneOf [string, array] で受け、
	// 配列なら relation 配列を、単体なら relation オブジェクトを返す (#1547)。
	var req struct {
		UserID json.RawMessage `json:"userId"`
	}
	invalidParam := func() error {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if err := c.Bind(&req); err != nil || len(req.UserID) == 0 {
		return invalidParam()
	}
	var single string
	if err := json.Unmarshal(req.UserID, &single); err == nil {
		if single == "" {
			return invalidParam()
		}
		// upstream relation.ts:135-137 は単一 userId (string) でも
		// `.then(it => [it])` で単一要素配列を返す (res schema は object|array の
		// oneOf だが実コードは常に配列)。drop-in 互換のため配列に揃える (#1766)。
		return c.JSON(http.StatusOK, []map[string]any{h.computeRelation(viewer, single)})
	}
	var arr []string
	if err := json.Unmarshal(req.UserID, &arr); err == nil {
		out := make([]map[string]any, 0, len(arr))
		for _, id := range arr {
			out = append(out, h.computeRelation(viewer, id))
		}
		return c.JSON(http.StatusOK, out)
	}
	return invalidParam()
}

// computeRelation builds the relation object for a single target user, matching
// upstream getRelation (#1547)。viewer==nil なら全 false。
func (h *Handler) computeRelation(viewer *model.User, targetID string) map[string]any {
	out := map[string]any{
		"id":                             targetID,
		"isFollowing":                    false,
		"isFollowed":                     false,
		"hasPendingFollowRequestFromYou": false,
		"hasPendingFollowRequestToYou":   false,
		"isBlocking":                     false,
		"isBlocked":                      false,
		"isMuted":                        false,
		"isRenoteMuted":                  false,
	}

	if viewer == nil {
		return out
	}

	// FindByPair は (rec, err) で返す契約 (gorm.ErrRecordNotFound は relation
	// 無し)。row 不在は default の false に倒すが、transient な DB error
	// (timeout / connection 切断等) は運用 debug のため slog.Warn で残す
	// (frontend には false で fallback、次の API call で正される程度の影響)。
	warnRelErr := func(label string, err error) {
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			slog.Warn("users/relation lookup failed", "rel", label, "viewer", viewer.ID, "target", targetID, "err", err)
		}
	}

	if h.followingRepo != nil {
		if rec, err := h.followingRepo.FindByPair(viewer.ID, targetID); rec != nil {
			out["isFollowing"] = true
		} else {
			warnRelErr("isFollowing", err)
		}
		if rec, err := h.followingRepo.FindByPair(targetID, viewer.ID); rec != nil {
			out["isFollowed"] = true
		} else {
			warnRelErr("isFollowed", err)
		}
	}
	if h.followRequestRepo != nil {
		if rec, err := h.followRequestRepo.FindByPair(viewer.ID, targetID); rec != nil {
			out["hasPendingFollowRequestFromYou"] = true
		} else {
			warnRelErr("hasPendingFollowRequestFromYou", err)
		}
		if rec, err := h.followRequestRepo.FindByPair(targetID, viewer.ID); rec != nil {
			out["hasPendingFollowRequestToYou"] = true
		} else {
			warnRelErr("hasPendingFollowRequestToYou", err)
		}
	}
	if h.blockingRepo != nil {
		if rec, err := h.blockingRepo.FindByPair(viewer.ID, targetID); rec != nil {
			out["isBlocking"] = true
		} else {
			warnRelErr("isBlocking", err)
		}
		if rec, err := h.blockingRepo.FindByPair(targetID, viewer.ID); rec != nil {
			out["isBlocked"] = true
		} else {
			warnRelErr("isBlocked", err)
		}
	}
	if h.mutingRepo != nil {
		if rec, err := h.mutingRepo.FindByPair(viewer.ID, targetID); rec != nil {
			out["isMuted"] = true
		} else {
			warnRelErr("isMuted", err)
		}
	}
	if h.renoteMutingRepo != nil {
		if rec, err := h.renoteMutingRepo.FindByPair(viewer.ID, targetID); rec != nil {
			out["isRenoteMuted"] = true
		} else {
			warnRelErr("isRenoteMuted", err)
		}
	}

	return out
}

// ReportAbuse handles POST /api/users/report-abuse.
func (h *Handler) ReportAbuse(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		UserID  string `json:"userId"`
		Comment string `json:"comment"`
	}
	// comment は upstream paramDef で minLength:1 / maxLength:2048 (report-abuse.ts:46)。
	// ajv の maxLength は文字数 (code unit) 基準なので byte 長でなく rune 数で判定する
	// (drive/handler.go の comment 検証と同方針)。
	if err := c.Bind(&req); err != nil || req.UserID == "" || req.Comment == "" || utf8.RuneCountInString(req.Comment) > 2048 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId and comment (1-2048 chars) are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.abuseRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// target 存在 / 自己通報 / 管理者通報の検証 (upstream report-abuse.ts:58-71)。
	var target *model.User
	if h.userRepo != nil {
		t, err := h.userRepo.FindByID(req.UserID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		if err != nil || t == nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "1acefcb5-0959-43fd-9685-b48305736cb5"))
		}
		target = t
	}
	if me != nil && req.UserID == me.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("CANNOT_REPORT_YOURSELF", "Cannot report yourself.", "1e13149e-b1e8-43cf-902e-c01dbfcb202f"))
	}
	if h.moderatorChecker != nil && h.moderatorChecker.IsAdministrator(req.UserID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("CANNOT_REPORT_THE_ADMIN", "Cannot report the admin.", "35e166f5-05fb-4f87-a2d5-adb42676d48f"))
	}
	report := &model.AbuseUserReport{
		ID:           h.idGen.Generate(time.Now()),
		TargetUserID: req.UserID,
		ReporterID:   me.ID,
		Comment:      req.Comment,
	}
	// targetUserHost を保存 (upstream report-abuse.ts:75)。reporterHost は
	// reporter が local viewer なので常に null。
	if target != nil {
		report.TargetUserHost = target.Host
	}
	if err := h.abuseRepo.Create(report); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// 通知 (in-app / admin stream / abuseReport system webhook) は連合経由の
	// Flag と同じ処理に任せる (#3256)。best-effort。
	if h.abuseCreated != nil {
		h.abuseCreated.NotifyCreated(c.Request().Context(), report, me, target)
	}
	return c.NoContent(http.StatusNoContent)
}

// Reactions handles POST /api/users/reactions.
//
// upstream Misskey TS の users/reactions と同 semantics:
//   - target user の publicReactions が false で viewer ≠ target なら
//     403 REACTIONS_NOT_PUBLIC (= reaction history は private)。
//   - target user が remote なら 400 IS_REMOTE_USER (mk-go では現状 local
//     のみ対応、upstream と同じ制限)。
//   - それ以外 (public か self view) なら reactor の reaction list を
//     id / createdAt / type / user / note shape で返す。
//
// note は upstream の packManyWithNote 相当の完全 shape (PackNotes) で返す。
// 最小 shape (id / userId / text) だと createdAt / visibility / user 等が
// 欠落し、misskey_dart の Note.fromJson が非null cast に失敗して落ちる
// (#1227、当初は #821 PR-D で最小 shape start としていた)。
func (h *Handler) Reactions(c echo.Context) error {
	viewer := middleware.GetUser(c)
	var req struct {
		UserID    string `json:"userId"`
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	cursorSince, cursorUntil, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	req.SinceID, req.UntilID = cursorSince, cursorUntil
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}

	// upstream: moderator / admin は全ユーザー (remote / 非 public reactions
	// 含む) の reaction を閲覧できる ("Moderators can see reactions of all
	// users")。viewer が moderator なら remote / publicReactions check を
	// 丸ごと bypass して reaction list を返す。
	iAmModerator := viewer != nil && h.moderatorChecker != nil && h.moderatorChecker.IsModerator(viewer.ID)

	// target user lookup + remote / publicReactions check (non-moderator のみ)。
	// production では userRepo が必ず wire されるが、既存の handler test
	// (TestReactions_Success 等) は userRepo を wire しないので nil guard で
	// fall-through する (= test compat、production 影響なし)。
	if !iAmModerator && h.userRepo != nil {
		target, err := h.userRepo.FindByID(req.UserID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		if err != nil || target == nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "27e494ba-2ac2-48e8-893b-10d4d8c2387b"))
		}
		// remote user は upstream でも display を制限している。
		// upstream TS は `host !== null` で判定するので mk-go も nil 判定に
		// 揃える (host="" の row は実際には DB に書かれないが、揺らぎ抑止)。
		if target.Host != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("IS_REMOTE_USER", "Currently unavailable to display reactions of remote users.", "6b95fa98-8cf9-2350-e284-f0ffdb54a805"))
		}
		// self view 以外は publicReactions check。
		// profile 行が無い (= nil) 場合は DB 列 default の `true` 扱いで
		// fall-through する (upstream TS の getUserPolicies と同 semantics)。
		if viewer == nil || viewer.ID != req.UserID {
			// **DB 障害を「公開」に倒さない。** nil で fall-through すると
			// 接続断のあいだ非公開リアクションが読めてしまう (#2799)。
			profile, perr := h.userService.GetProfileErr(req.UserID)
			if perr != nil && !repository.IsNotFound(perr) {
				return apierr.JSONInternalError(c)
			}
			if profile != nil && !profile.PublicReactions {
				return c.JSON(http.StatusBadRequest, apierr.Error("REACTIONS_NOT_PUBLIC", "Reactions of the user is not public.", "673a7dd2-6924-1093-e0c0-e68456ceae5c"))
			}
		}
	}

	// #2106 L8: block 早期 return は upstream reactions.ts 同様 remote/public check の後に置く。
	// block されていても remote/非公開 target には IS_REMOTE_USER/REACTIONS_NOT_PUBLIC を先に返す。
	// moderator は bypass。
	if !iAmModerator && h.isBlockedByTarget(viewer, req.UserID) {
		return c.JSON(http.StatusOK, []any{})
	}

	// reactor の reaction list を取得 (User / Note を Preload 済み)。
	// userRepo と同じく test stub では noteReactionRepo を wire しないので、
	// nil guard で空配列を返して既存 handler test の挙動を維持する
	// (= 本 PR で stub から本実装に書き換えたが test 互換は保つ、#821 PR-D)。
	if h.noteReactionRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream の generateVisibilityQuery(query, me) 相当を repo の SQL に push
	// down する (moderator も含む全 viewer に適用)。viewer が閲覧できない note
	// (followers/specified) の reaction を LIMIT 前に除外することで、post-filter
	// 時に起きる「ページが limit 未満になりページネーションが途切れる」問題を
	// 避ける。
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	rows, err := h.noteReactionRepo.ListByUserID(req.UserID, viewerID, req.UntilID, req.SinceID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// reaction 先 note の author/reply/renote author を viewer が mute / 自分を
	// block している場合は除外する (upstream reactions.ts:115-121 の isUserRelated、
	// #1547)。ApplyMuteBlockChannel で生き残った note id のみ残す。
	sets, err := notesfilter.LoadMuteBlockSets(viewer, h.mutingRepo, h.blockingRepo, h.channelMutingRepo, h.userRepo)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **ブロック済みインスタンスのノートも落とす** (upstream
	// generateBlockedHostQueryForNote)。`ListByUserID` が push down するのは
	// visibility だけなので、ブロック後も既存のリアクションが出続けていた。
	blockedHosts, berr := notesfilter.LoadBlockedHosts(h.metaRepo)
	if berr != nil {
		return apierr.JSONInternalError(c)
	}
	if len(sets.MutedUserIDs) > 0 || len(sets.BlockerIDs) > 0 || len(sets.MutedInstances) > 0 || len(blockedHosts) > 0 {
		rowNotes := make([]*model.Note, 0, len(rows))
		for _, r := range rows {
			if r.Note != nil {
				rowNotes = append(rowNotes, r.Note)
			}
		}
		filteredNotes, ferr := notesfilter.ApplyMuteBlockChannel(rowNotes, sets, h.noteRepo)
		if ferr != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		filteredNotes = notesfilter.ApplyBlockedHosts(filteredNotes, blockedHosts)
		survived := make(map[string]struct{}, len(filteredNotes))
		for _, n := range filteredNotes {
			survived[n.ID] = struct{}{}
		}
		filtered := rows[:0]
		for _, r := range rows {
			if r.Note == nil {
				continue
			}
			if _, ok := survived[r.Note.ID]; ok {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	// note は upstream の packManyWithNote と同じく完全 shape で返す。最小 shape
	// (id/userId/text) だと createdAt / visibility / user 等が欠落し、misskey_dart
	// の Note.fromJson が非null フィールドの cast に失敗して落ちる (#1227)。
	// PackNotes で batch pack して instance / emoji / buffered reaction も解決する。
	notes := make([]*model.Note, 0, len(rows))
	for _, r := range rows {
		if r.Note != nil {
			notes = append(notes, r.Note)
		}
	}
	packed := entity.PackNotes(c.Request().Context(), notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	notehide.HideEmbeds(viewer, packed)
	noteByID := make(map[string]entity.NoteEntity, len(packed))
	for _, ne := range packed {
		noteByID[ne.ID] = ne
	}

	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		entry := map[string]any{
			"id": r.ID,
			// upstream NoteReactionEntityService は convertLegacyReaction を通す
			// (legacy alias + 局所 custom emoji の @. 除去)。生 DB 文字列を
			// leak しないよう read-time 変換する (#1547)。
			"type": reaction.ConvertLegacy(r.Reaction),
		}
		if t, err := h.idGen.ParseTime(r.ID); err == nil {
			entry["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		if r.User != nil {
			entry["user"] = entity.PackUserLite(r.User)
		}
		if r.Note != nil {
			if ne, ok := noteByID[r.Note.ID]; ok {
				entry["note"] = ne
			}
		}
		out = append(out, entry)
	}
	return c.JSON(http.StatusOK, out)
}

// FeaturedNotes handles POST /api/users/featured-notes.
//
// upstream featured-notes.ts は Redis sorted set (getPerUserNotesRanking) で
// engagement 上位 50 を選抜したあと、Go 側で id DESC に並べ替えて untilId で
// ページングする 2 段構成。mk-go は SQL ranking + visibility push-down に
// 置き換えて同じ 2 段で揃える (#1487 Option B / #1491 review):
//
//   - selection: `ListFeaturedByUser` が `("renoteCount" + "repliesCount") DESC,
//     id DESC` + visibility push-down + channel 除外 + LIMIT 50 で SQL から
//     プールを取る (post-fetch FilterVisible のページ過少充填と followers 判定
//     N+1 を回避)。
//   - display: 同 repo 内で id DESC sort → untilId filter → 先頭 limit 件。
//     id cursor とソート順が一致するので untilId ページングで重複/欠落しない。
//   - hard mute は packing 前に post-fetch で適用 (visibility と独立な viewer 個別
//     filter なので SQL push-down 対象外)。
func (h *Handler) FeaturedNotes(c echo.Context) error {
	var req struct {
		UserID  string `json:"userId"`
		Limit   *int   `json:"limit"`
		UntilID string `json:"untilId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// 他のページングと同じく `id.NormalizeCursor` を通す (#3025)。
	_, untilID, cursorOK := id.NormalizeCursor("", req.UntilID, nil, nil)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	viewer := middleware.GetUser(c)
	var viewerID string
	if viewer != nil {
		viewerID = viewer.ID
	}
	// viewer が target にブロックされていれば空配列 (upstream featured-notes.ts:58-61、#1547)。
	if h.isBlockedByTarget(viewer, req.UserID) {
		return c.JSON(http.StatusOK, []entity.NoteEntity{})
	}
	notes, err := h.featuredNotesByUser(c.Request().Context(), req.UserID, viewerID, untilID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	notes = notesfilter.ApplyHardMute(h.userRepo, viewer, notes)
	// viewer が mute した user / viewer を block している user が note/reply/renote
	// の author なら除外する (upstream featured-notes.ts:93-98 の isUserRelated、#1547)。
	sets, err := notesfilter.LoadMuteBlockSets(viewer, h.mutingRepo, h.blockingRepo, h.channelMutingRepo, h.userRepo)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	notes, err = notesfilter.ApplyMuteBlockChannel(notes, sets, h.noteRepo)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **ブロック済みインスタンスのノートを落とす** (upstream
	// generateBlockedHostQueryForNote)。ランキング / SQL のどちらの経路にも
	// 入っていないので post-fetch で落とす。
	blockedHosts, err := notesfilter.LoadBlockedHosts(h.metaRepo)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	notes = notesfilter.ApplyBlockedHosts(notes, blockedHosts)
	result := entity.PackNotes(c.Request().Context(), notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	h.fieldRes.Apply(result, viewer)
	notehide.HideEmbeds(viewer, result)
	return c.JSON(http.StatusOK, result)
}

// featuredPerUserThreshold は per-user ranking から取得する上位件数 (upstream
// featured-notes.ts の getPerUserNotesRanking(50))。
const featuredPerUserThreshold = 50

// featuredNotesByUser returns userID's featured notes from the per-user
// engagement ranking (#1687)。ranking 未配線 / 空 (fresh instance 等) のときは
// 既存の SQL count-DESC ranking (ListFeaturedByUser) に fallback する。ranking
// 経路は upstream featured-notes.ts と同じく id DESC sort → untilId filter → limit。
func (h *Handler) featuredNotesByUser(ctx context.Context, userID, viewerID, untilID string, limit int) ([]*model.Note, error) {
	if h.featuredRanking == nil {
		return h.noteRepo.ListFeaturedByUser(userID, viewerID, untilID, limit)
	}
	ids, err := h.featuredRanking.GetPerUserNotesRanking(ctx, userID, featuredPerUserThreshold)
	if err != nil || len(ids) == 0 {
		return h.noteRepo.ListFeaturedByUser(userID, viewerID, untilID, limit)
	}
	ids = sortAndPageFeaturedIDs(ids, untilID, limit)
	if len(ids) == 0 {
		return []*model.Note{}, nil
	}
	return h.noteRepo.FindManyByIDsWithUser(ids)
}

// sortAndPageFeaturedIDs sorts note IDs DESC, drops IDs >= untilID, and caps to
// limit (upstream featured-notes.ts の noteIds.sort / filter / slice)。
func sortAndPageFeaturedIDs(ids []string, untilID string, limit int) []string {
	out := make([]string, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	if untilID != "" {
		filtered := out[:0]
		for _, noteID := range out {
			if noteID < untilID {
				filtered = append(filtered, noteID)
			}
		}
		out = filtered
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SearchByUsernameAndHost handles POST /api/users/search-by-username-and-host.
func (h *Handler) SearchByUsernameAndHost(c echo.Context) error {
	var req struct {
		// upstream の paramDef は username / host の anyOf で、どちらか一方の
		// キーがあればよい (値は null 可)。username 省略時は username で
		// 絞らずに host だけで検索する。両方欠けたときだけ 400。
		Username *string `json:"username"`
		Host     *string `json:"host"`
		Limit    *int    `json:"limit"`
		// Detail は upstream search-by-username-and-host.ts:52 と同じく default
		// true。false のとき UserLite を返す (#1547)。
		Detail *bool `json:"detail"`
	}
	// キーの有無を見るため raw body も読む (null 指定と省略は upstream では
	// 区別されないが、「どちらのキーも無い」だけは弾く必要がある)。
	raw := map[string]json.RawMessage{}
	body, readErr := io.ReadAll(c.Request().Body)
	if readErr == nil {
		_ = json.Unmarshal(body, &raw)
		c.Request().Body = io.NopCloser(bytes.NewReader(body))
	}
	_, hasUsername := raw["username"]
	_, hasHost := raw["host"]
	if err := c.Bind(&req); err != nil || (!hasUsername && !hasHost) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "username or host is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	username := ""
	if req.Username != nil {
		username = *req.Username
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// host=nil → local user、host=*string → 当該 host のみで絞り込む
	// (#766 fix、upstream Misskey TS の同 endpoint と同じ semantics)。
	users, err := h.userService.SearchByUsernameAndHost(username, req.Host, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	resolver := entity.NewInstanceResolver(h.instanceLookup(), users...)

	// detail=false → UserLite。default (true) は UserDetailed (upstream は
	// detail default true で UserDetailed pack、#1547)。旧実装は常に UserLite。
	if req.Detail != nil && !*req.Detail {
		result := make([]entity.UserLite, 0, len(users))
		for _, u := range users {
			lite := entity.PackUserLite(u)
			resolver.FillUserLite(&lite)
			h.populateUserEmojis(u, &lite)
			result = append(result, lite)
		}
		return c.JSON(http.StatusOK, result)
	}

	viewer := middleware.GetUser(c)
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	iAmModerator := viewer != nil && h.moderatorChecker != nil && h.moderatorChecker.IsModerator(viewer.ID)
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	profiles := h.userService.GetProfilesByUserIDs(ids)
	ctx := c.Request().Context()
	result := make([]any, 0, len(users))
	for _, u := range users {
		d := entity.PackUserDetailed(u, profiles[u.ID], h.idGen)
		resolver.FillUserLite(&d.UserLite)
		h.populateUserEmojis(u, &d.UserLite)
		// upstream search-by-username-and-host は packMany(users, me, {schema:'UserDetailed'})
		// で embed user に viewer 視点の relation block を付ける (#1980)。匿名/self で no-op。
		viewerIsFollowing := h.viewerRelationRepos().Apply(&d, viewerID, u, profiles[u.ID])
		// upstream pack は count gate (followersVisibility=followers 等) を isFollowing で
		// 解く。これが無いと followers-only count が非フォロワーに leak する (#1980、users/search と対称)。
		isMe := viewer != nil && viewer.ID == u.ID
		entity.GateCountVisibility(&d, isMe, iAmModerator, viewerIsFollowing)
		// upstream の pack は isDetailed && isMe で MeDetailed を返す。
		result = append(result, meself.Pack(ctx, d, u, profiles[u.ID], viewer))
	}
	return c.JSON(http.StatusOK, result)
}

// UpdateMemo handles POST /api/users/update-memo.
// memo が null / 空文字なら削除、それ以外なら upsert する。
func (h *Handler) UpdateMemo(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		UserID string  `json:"userId"`
		Memo   *string `json:"memo"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	// upstream update-memo.ts:22-27 は getUser で target 存在を確認し、無ければ
	// NO_SUCH_USER を返す (#1547)。旧実装は存在確認せず直接 upsert していた。
	if _, err := h.userService.ShowByID(req.UserID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "6fef56f3-e765-4957-88e5-c6f65329b8a5"))
	}

	if h.memoRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}

	if req.Memo == nil || *req.Memo == "" {
		_ = h.memoRepo.Delete(me.ID, req.UserID)
	} else {
		memo := &model.UserMemo{
			ID:           h.idGen.Generate(time.Now()),
			UserID:       me.ID,
			TargetUserID: req.UserID,
			Memo:         *req.Memo,
		}
		if err := h.memoRepo.CreateOrUpdate(memo); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
	}
	return c.NoContent(http.StatusNoContent)
}
