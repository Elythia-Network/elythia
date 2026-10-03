package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- /admin/unset-user-avatar ---

func TestUnsetUserAvatar(t *testing.T) {
	t.Run("missing userId returns 400 INVALID_PARAM", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		assert.Equal(t, http.StatusBadRequest, doPost(h.UnsetUserAvatar, `{}`, adminUser).Code)
	})
	t.Run("clears avatarId / avatarUrl / avatarBlurhash", func(t *testing.T) {
		h, repo, _, _ := newTestHandler(t)
		avatarID := "av1"
		avatarURL := "https://x.example/a.webp"
		avatarBlurhash := "blur"
		repo.Users["u1"] = &model.User{
			ID: "u1", AvatarID: &avatarID, AvatarURL: &avatarURL, AvatarBlurhash: &avatarBlurhash,
		}
		assert.Equal(t, http.StatusNoContent, doPost(h.UnsetUserAvatar, `{"userId":"u1"}`, adminUser).Code)
		got := repo.Users["u1"]
		assert.Nil(t, got.AvatarID, "avatarId must be cleared")
		assert.Nil(t, got.AvatarURL, "avatarUrl must be cleared")
		assert.Nil(t, got.AvatarBlurhash, "avatarBlurhash must be cleared")
	})
}

// --- /admin/unset-user-banner ---

func TestUnsetUserBanner(t *testing.T) {
	t.Run("missing userId returns 400 INVALID_PARAM", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		assert.Equal(t, http.StatusBadRequest, doPost(h.UnsetUserBanner, `{}`, adminUser).Code)
	})
	t.Run("clears bannerId / bannerUrl / bannerBlurhash", func(t *testing.T) {
		h, repo, _, _ := newTestHandler(t)
		bannerID := "b1"
		bannerURL := "https://x.example/b.webp"
		bannerBlurhash := "blur"
		repo.Users["u1"] = &model.User{
			ID: "u1", BannerID: &bannerID, BannerURL: &bannerURL, BannerBlurhash: &bannerBlurhash,
		}
		assert.Equal(t, http.StatusNoContent, doPost(h.UnsetUserBanner, `{"userId":"u1"}`, adminUser).Code)
		got := repo.Users["u1"]
		assert.Nil(t, got.BannerID, "bannerId must be cleared")
		assert.Nil(t, got.BannerURL, "bannerUrl must be cleared")
		assert.Nil(t, got.BannerBlurhash, "bannerBlurhash must be cleared")
	})
}

// --- /admin/update-user-note ---

func TestUpdateUserNote(t *testing.T) {
	t.Run("missing userId returns 400 INVALID_PARAM", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateUserNote, `{}`, adminUser).Code)
	})
	t.Run("writes moderationNote to user_profile", func(t *testing.T) {
		h, repo, _, _ := newTestHandler(t)
		repo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
		body := `{"userId":"u1","text":"naughty user"}`
		assert.Equal(t, http.StatusNoContent, doPost(h.UpdateUserNote, body, adminUser).Code)
		got := repo.Profiles["u1"]
		require.NotNil(t, got.ModerationNote)
		assert.Equal(t, "naughty user", *got.ModerationNote)
	})
}

// --- /admin/delete-all-files-of-a-user ---

// TestDeleteAllFilesOfUser は param 検証 / repo 未配線 fallback パスを
// 押さえる薄いテスト。handler_stubs_test.go から移動 (#581 review)。
func TestDeleteAllFilesOfUser(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// userId 欠落は 400
	assert.Equal(t, http.StatusBadRequest, doPost(h.DeleteAllFilesOfUser, `{}`, adminUser).Code)
	// repo 未注入は 204
	assert.Equal(t, http.StatusNoContent,
		doPost(h.DeleteAllFilesOfUser, `{"userId":"u1"}`, adminUser).Code)
}

// TestDeleteAllFilesOfUser_DeletesOnlyTargetUserFiles は本来 accounts_test.go
// にあったが、handler が moderation.go にあるため discoverability 改善で
// こちらに移動した (#581 review INFO-1)。
func TestDeleteAllFilesOfUser_DeletesOnlyTargetUserFiles(t *testing.T) {
	u1 := "u1"
	u2 := "u2"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "f1", UserID: &u1},
		&model.DriveFile{ID: "f2", UserID: &u1},
		&model.DriveFile{ID: "f3", UserID: &u2},
	)

	rec := doPost(h.DeleteAllFilesOfUser, `{"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Files, "f1")
	assert.NotContains(t, repo.Files, "f2")
	assert.Contains(t, repo.Files, "f3", "other user's files must be kept")
}

// --- /admin/update-abuse-user-report ---

// 旧 handler_stubs_test.go から移動。本 PR で refactor 範囲外の handler
// (UpdateAbuseUserReport) のテストを誤って巻き込み削除してしまったので
// 復元する (#581 review BUG-1)。本 endpoint は abuse report の resolved
// フラグを立てる moderation 操作なので moderation_test.go が居場所として
// 妥当。
func TestUpdateAbuseUserReport(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// reportId 欠落は 400 (upstream paramDef required)。
	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateAbuseUserReport, `{}`, adminUser).Code)
}

// moderationNote のみを更新し、resolved は触らない (upstream AbuseReportService.update)。
func TestUpdateAbuseUserReport_UpdatesModerationNoteOnly(t *testing.T) {
	h, repo := setupAbuseReportHandler(t,
		&model.AbuseUserReport{ID: "r1", TargetUserID: "u1", ReporterID: "u2"},
	)
	rec := doPost(h.UpdateAbuseUserReport, `{"reportId":"r1","moderationNote":"checked"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "checked", repo.Reports["r1"].ModerationNote)
	assert.False(t, repo.Reports["r1"].Resolved, "resolved は触らない")
}

// 存在しない reportId は NO_SUCH_ABUSE_REPORT (404, upstream id 15f51cf5)。
func TestUpdateAbuseUserReport_NotFound(t *testing.T) {
	h, _ := setupAbuseReportHandler(t)
	rec := doPost(h.UpdateAbuseUserReport, `{"reportId":"ghost","moderationNote":"x"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_ABUSE_REPORT")
	assert.Contains(t, rec.Body.String(), "15f51cf5-46d1-4b1d-a618-b35bcbed0662")
}

// 引けた後の更新の失敗は DB の障害なので 500 にする (#2792)。
func TestUpdateAbuseUserReport_UpdateFailureIs500(t *testing.T) {
	h, inner := setupAbuseReportHandler(t, &model.AbuseUserReport{ID: "r1"})
	h.SetAbuseRepo(&failingAbuseUpdateRepo{MockAbuseReportRepository: inner})

	rec := doPost(h.UpdateAbuseUserReport, `{"reportId":"r1","moderationNote":"x"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// updateAbuseReportNote の log info に before/after が記録される。
func TestUpdateAbuseUserReport_LogPayload(t *testing.T) {
	h, _ := setupAbuseReportHandler(t,
		&model.AbuseUserReport{ID: "r1", TargetUserID: "u1", ReporterID: "u2", ModerationNote: "old"},
	)
	repo := attachModLog(t, h)
	rec := doPost(h.UpdateAbuseUserReport, `{"reportId":"r1","moderationNote":"new"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	var info map[string]any
	require.NoError(t, json.Unmarshal(repo.Snapshot()[0].Info, &info))
	assert.Equal(t, "old", info["before"])
	assert.Equal(t, "new", info["after"])
}

// --- moderation log assertions ---

func TestUnsetUserAvatar_WritesModerationLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	avatarID := "av1"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarID: &avatarID}
	repo := attachModLog(t, h)

	rec := doPost(h.UnsetUserAvatar, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	log := repo.Snapshot()[0]
	assert.Equal(t, "unsetUserAvatar", log.Type)
	// #1539: log info に解除した fileId を含める。
	var info map[string]any
	require.NoError(t, json.Unmarshal(log.Info, &info))
	assert.Equal(t, "av1", info["fileId"])
	assert.Equal(t, "u1", info["userId"])
}

func TestUnsetUserBanner_WritesModerationLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	bannerID := "bn1"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", BannerID: &bannerID}
	repo := attachModLog(t, h)

	rec := doPost(h.UnsetUserBanner, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	log := repo.Snapshot()[0]
	assert.Equal(t, "unsetUserBanner", log.Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(log.Info, &info))
	assert.Equal(t, "bn1", info["fileId"])
}

// #1539: 既に未設定なら no-op で moderation log を書かない。
func TestUnsetUserAvatar_AlreadyUnsetNoLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"} // avatarId nil
	repo := attachModLog(t, h)

	rec := doPost(h.UnsetUserAvatar, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, repo.Snapshot(), "already-unset avatar must not write a log")
}

func TestUpdateAbuseUserReport_WritesModerationLog(t *testing.T) {
	// moderationNote が変化したとき updateAbuseReportNote log を出す。
	h, _ := setupAbuseReportHandler(t,
		&model.AbuseUserReport{ID: "r1", TargetUserID: "u1", ReporterID: "u2"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.UpdateAbuseUserReport, `{"reportId":"r1","moderationNote":"note added"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "updateAbuseReportNote", repo.Snapshot()[0].Type)
}

// moderationNote が変化しないとき log は出さない (upstream の changed guard)。
func TestUpdateAbuseUserReport_NoLogWhenUnchanged(t *testing.T) {
	h, _ := setupAbuseReportHandler(t,
		&model.AbuseUserReport{ID: "r1", TargetUserID: "u1", ReporterID: "u2", ModerationNote: "same"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.UpdateAbuseUserReport, `{"reportId":"r1","moderationNote":"same"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	// 変化なしなので log は書かれない。少し待っても 0 件のまま。
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, repo.Snapshot())
}

func TestUpdateUserNote_WritesModerationLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	beforeNote := "old note"
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", ModerationNote: &beforeNote}
	repo := attachModLog(t, h)

	rec := doPost(h.UpdateUserNote, `{"userId":"u1","text":"new note"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "updateUserNote", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "u1", info["userId"])
	assert.Equal(t, "old note", info["before"])
	assert.Equal(t, "new note", info["after"])
}
