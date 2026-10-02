package admin

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// UnsetUserAvatar handles POST /api/admin/unset-user-avatar.
//
// upstream unset-user-avatar.ts: avatarId が既に null なら no-op (log も書かない)、
// それ以外は解除して moderation log に fileId: 旧 avatarId を含めて記録する (#1539)。
func (h *Handler) UnsetUserAvatar(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.NoContent(http.StatusNoContent)
	}
	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil || user == nil || user.AvatarID == nil {
		// 既に未設定 / 不在なら no-op (upstream は avatarId==null で early-return)。
		return c.NoContent(http.StatusNoContent)
	}
	// fileId は UpdateUser が user pointer を mutate する前に値で控える
	// (log は goroutine で後から marshal するため、pointer のままだと nil 化する)。
	fileID := *user.AvatarID
	if err := h.userRepo.UpdateUser(req.UserID, map[string]any{"avatarId": nil, "avatarUrl": nil, "avatarBlurhash": nil}); err == nil {
		info := moderationlog.UserInfo(user)
		info["fileId"] = fileID
		h.logModeration(c, moderationlog.LogUnsetUserAvatar, info)
	}
	return c.NoContent(http.StatusNoContent)
}

// UnsetUserBanner handles POST /api/admin/unset-user-banner.
//
// upstream unset-user-banner.ts: bannerId 解除版の UnsetUserAvatar 相当 (#1539)。
func (h *Handler) UnsetUserBanner(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.NoContent(http.StatusNoContent)
	}
	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil || user == nil || user.BannerID == nil {
		return c.NoContent(http.StatusNoContent)
	}
	fileID := *user.BannerID
	if err := h.userRepo.UpdateUser(req.UserID, map[string]any{"bannerId": nil, "bannerUrl": nil, "bannerBlurhash": nil}); err == nil {
		info := moderationlog.UserInfo(user)
		info["fileId"] = fileID
		h.logModeration(c, moderationlog.LogUnsetUserBanner, info)
	}
	return c.NoContent(http.StatusNoContent)
}

// UpdateUserNote handles POST /api/admin/update-user-note. user_profile の
// moderationNote 列を更新する。
func (h *Handler) UpdateUserNote(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
		Text   string `json:"text"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.NoContent(http.StatusNoContent)
	}
	// before を log の info に含めるため UpdateProfile の前に取得する。
	// 取得失敗時は before 不明扱い (空文字) で log を書く方が監査価値が高い。
	var before string
	if profile, err := h.userRepo.FindProfileByUserID(req.UserID); err == nil && profile != nil && profile.ModerationNote != nil {
		before = *profile.ModerationNote
	}
	if err := h.userRepo.UpdateProfile(req.UserID, map[string]any{"moderationNote": req.Text}); err != nil {
		return c.NoContent(http.StatusNoContent)
	}
	target, err := h.userRepo.FindByID(req.UserID)
	if err != nil || target == nil {
		// UpdateProfile が成功した直後に user lookup が失敗するのは
		// 並行削除等のレアケース。debug-level に残して早期 return する。
		slog.DebugContext(c.Request().Context(), "moderation log: target user not found after UpdateUserNote",
			"userId", req.UserID)
		return c.NoContent(http.StatusNoContent)
	}
	info := moderationlog.UserInfo(target)
	info["before"] = before
	info["after"] = req.Text
	h.logModeration(c, moderationlog.LogUpdateUserNote, info)
	return c.NoContent(http.StatusNoContent)
}

// UpdateAbuseUserReport handles POST /api/admin/update-abuse-user-report.
//
// upstream AbuseReportService.update は `moderationNote` 列のみを更新し、
// resolved は触らない (resolve は resolve-abuse-user-report 側の責務)。旧
// mk-go は moderationNote param を無視して resolved=true を立て、log type も
// resolveAbuseReport にしていた (= resolve-abuse-user-report との取り違え)。
// 本 fix で moderationNote を受け取り、変化時のみ updateAbuseReportNote を
// log する (upstream update-abuse-user-report.ts / AbuseReportService.ts:154)。
func (h *Handler) UpdateAbuseUserReport(c echo.Context) error {
	var req struct {
		ReportID       string  `json:"reportId"`
		ModerationNote *string `json:"moderationNote"`
	}
	if err := c.Bind(&req); err != nil || req.ReportID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("reportId is required."))
	}
	if h.abuseRepo == nil {
		return c.JSON(http.StatusNotFound, apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "15f51cf5-46d1-4b1d-a618-b35bcbed0662", apierr.KindServer))
	}
	before, err := h.abuseRepo.FindByID(req.ReportID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil || before == nil {
		return c.JSON(http.StatusNotFound, apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "15f51cf5-46d1-4b1d-a618-b35bcbed0662", apierr.KindServer))
	}
	// upstream は moderationNote が undefined のとき列を触らない (TypeORM が
	// undefined field を skip)。mk-go も moderationNote 未送出時は no-op。
	if req.ModerationNote == nil {
		return c.NoContent(http.StatusNoContent)
	}
	note := *req.ModerationNote
	// 更新前の値を控える (UpdateFields 後に before を参照すると、in-memory repo
	// 実装では同一ポインタが書き換わって比較が壊れる)。
	beforeNote := before.ModerationNote
	logRow := abuseReportLogRow(before)
	if err := h.abuseRepo.UpdateFields(req.ReportID, map[string]any{"moderationNote": note}); err != nil {
		// 行があることは引いて確かめ済みなので、ここで失敗するのは DB の障害 (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// 変化があったときのみ updateAbuseReportNote を記録 (upstream と同じ)。
	if beforeNote != note {
		h.logModeration(c, moderationlog.LogUpdateAbuseReportNote, map[string]any{
			"reportId": req.ReportID,
			"report":   logRow,
			"before":   beforeNote,
			"after":    note,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// DeleteAllFilesOfUser handles POST /api/admin/delete-all-files-of-a-user.
//
// upstream delete-all-files-of-a-user.ts は対象ユーザーの全 drive file を
// findBy({userId}) で回し driveService.deleteFile で物理ストレージごと削除する
// (dbQueue 'deleteDriveFiles' processor は upstream でも未使用の vestigial)。
// storage backend が配線されていれば list→object storage 削除→DB 行削除を
// バッチで回し orphan を残さない。未配線時は DB 行のみ一括削除。
func (h *Handler) DeleteAllFilesOfUser(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("userId is required."))
	}
	if h.driveFileRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if h.storageDeleter == nil {
		// storage 未配線: DB 行のみ一括削除。
		if _, err := h.driveFileRepo.DeleteByUser(req.UserID); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.deleteFilesBatched(c, func(limit int) ([]*model.DriveFile, error) {
		return h.driveFileRepo.ListByUserAll(req.UserID, limit)
	}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	return c.NoContent(http.StatusNoContent)
}
